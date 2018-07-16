package main

import (
	"bytes"
	"compress/gzip"
	"encoding/gob"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/forthxu/goredis"
	"github.com/forthxu/websocket"
	"github.com/larspensjo/config"
	"github.com/tidwall/gjson"
	"golang.org/x/net/proxy"
	"io"
	"io/ioutil"
	"log"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

var usage = `Usage: %s [options] 
Options are:
    -f configuration file
`

func infoExit(info string) {
	fmt.Print(info)
	os.Exit(0)
}

func main() {
	//根据CPU数量设置多核运行
	runtime.GOMAXPROCS(runtime.NumCPU())

	//获取配置文件
	var configFile string
	flag.Usage = func() {
		infoExit(fmt.Sprintf(usage, os.Args[0]))
	}
	flag.StringVar(&configFile, "f", "config.ini", "configuration file")
	flag.Parse()
	if len(configFile) <= 0 {
		infoExit(fmt.Sprintf(usage, os.Args[0]))
	} else if _, err := os.Stat(configFile); err != nil && os.IsNotExist(err) {
		infoExit(fmt.Sprintf("%s configFile not exist", configFile))
	}

	//解析配置文件参数
	var (
		host        string            = "127.0.0.1"
		port        int               = 9999
		token       string            = ""        // Server酱通知(http://sc.ftqq.com/3.version) token
		gap         int               = 1000      // 取数据的的基础间隔时间，默认1000毫秒
		savegap     int               = 600       // 保存24H数据的间隔时间，用于比较涨跌幅
		proxy       string            = ""        // 代理的地址
		nginxProxy  string            = ""        // nginx代理的地址
		outDir      string            = ""        // 数据输出成文件，默认空不输出成文件
		info        string            = "default" // 程序运行标志
		bind        string            = ""        // 绑定本地出口ip
		redisConfig map[string]string = make(map[string]string)
		debug       bool              = false
	)
	cfg, err := config.ReadDefault(configFile)
	if err != nil {
		infoExit(fmt.Sprintf("%s configFile parse fail: %s", configFile, err.Error()))
	}
	if cfg.HasSection("redis") {
		section, err := cfg.SectionOptions("redis")
		if err == nil {
			for _, v := range section {
				options, err := cfg.String("redis", v)
				if err == nil {
					redisConfig[v] = options
				}
			}
		}
	}
	if cfg.HasSection("app") {
		if data, err := cfg.String("app", "host"); err == nil {
			host = data
		}
		if data, err := cfg.Int("app", "port"); err == nil {
			port = data
		}
		if data, err := cfg.String("app", "token"); err == nil {
			token = data
		}
		if data, err := cfg.Int("app", "gap"); err == nil {
			gap = data
		}
		if data, err := cfg.Int("app", "savegap"); err == nil {
			savegap = data
		}
		if data, err := cfg.String("app", "proxy"); err == nil {
			proxy = data
		}
		if data, err := cfg.String("app", "nginxproxy"); err == nil {
			nginxProxy = data
		}
		if data, err := cfg.String("app", "outdir"); err == nil {
			outDir = data
		}
		if data, err := cfg.String("app", "info"); err == nil {
			info = data
		}
		if data, err := cfg.String("app", "bind"); err == nil {
			bind = data
		}
		if data, err := cfg.Bool("app", "debug"); err == nil {
			debug = data
		}
	}
	//日志配置
	if debug {
		log.SetFlags(log.LstdFlags | log.Lshortfile) //带文件行号的日志
	} else {
		log.SetFlags(log.LstdFlags)
	}

	//初始工作对象
	w := Work{
		Host:        host,
		Port:        port,
		Token:       token,
		Gap:         gap,
		SaveGap:     savegap,
		Proxy:       proxy,
		NginxProxy:  nginxProxy,
		OutDir:      outDir,
		RedisConfig: redisConfig,
		Info:        info,
		Bind:        bind,
	}
	w.Platform = make(map[string]*currentPrices)
	w.Platform24 = make(map[string]*currentPrices)
	w.NotifyCount.Num = make(map[string]int)

	//提示信息
	log.Println("[app] config:", configFile)
	log.Println("[app] listen:", w.Host, w.Port)
	log.Println("[app] gap time:", w.Gap, "Millisecond")
	log.Println("[app] savegap time:", w.SaveGap, "Second")
	log.Println("[app] proxy:", w.Proxy)
	log.Println("[app] nginxProxy:", w.NginxProxy)
	log.Println("[app] bind local ip:", w.Bind)
	log.Println("[app] outDir:", w.OutDir)
	log.Println("[app] info:", w.Info)
	log.Println("[app] debug:", debug)

	//开始工作
	w.initRedis()
	w.runWorkers()
	w.RunHttp()

	w.notify("[currentPrice] 程序结束", "")
}

//redis初始化
func (w *Work) initRedis() {
	//连接数据库
	if _, existHost := w.RedisConfig["host"]; !existHost {
		log.Fatalln("[redis] host error")
	}
	if _, existPort := w.RedisConfig["port"]; !existPort {
		log.Fatalln("[redis] host error")
	}
	w.Redis.Addr = w.RedisConfig["host"] + ":" + w.RedisConfig["port"]

	//授权密码
	if _, existAuth := w.RedisConfig["auth"]; existAuth {
		w.Redis.Password = w.RedisConfig["auth"]
	}

	//选择DB
	if _, existDB := w.RedisConfig["db"]; !existDB {
		w.Redis.Db = 0
	} else {
		redisdb, err := strconv.Atoi(w.RedisConfig["db"])
		if err != nil {
			log.Fatalln("[redis] config select db error")
		}
		w.Redis.Db = redisdb
	}

	//测试连接
	_, err := w.Redis.Ping()
	if err != nil {
		log.Fatalln("[redis] ping ", err)
	}
}

//绑定本地出口ip
func (w *Work) bindIP() (*http.Transport, error) {
	if len(w.Bind) < 0 {
		return nil, errors.New(fmt.Sprintf("[%s] local bind ip not exist", w.Bind))
	}

	localAddr, err := net.ResolveIPAddr("ip", w.Bind)
	if err != nil {
		return nil, errors.New(fmt.Sprintf("[%s] local bind ip error:%s", w.Bind, err.Error()))
	}
	localTCPAddr := net.TCPAddr{
		IP: localAddr.IP,
	}
	d := net.Dialer{
		LocalAddr: &localTCPAddr,
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
	}

	tr := &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		Dial:                d.Dial,
		TLSHandshakeTimeout: 10 * time.Second,
	}
	return tr, nil
}

func (w *Work) getHttpClient() (*http.Client, error) {
	var timeout int = 30
	var client *http.Client
	if strings.HasPrefix(w.Proxy, "http") {
		urlParse := url.URL{}
		urlProxy, err := urlParse.Parse(w.Proxy)
		if err != nil {
			return nil, err
		}

		client = &http.Client{
			Transport: &http.Transport{
				Proxy: http.ProxyURL(urlProxy),
			},
			Timeout: time.Duration(timeout) * time.Second,
		}
	} else if strings.HasPrefix(w.Proxy, "socks5://") {
		var auths_hosts, auths []string
		var hosts string
		var auth *proxy.Auth
		auths_hosts = strings.Split(strings.Replace(w.Proxy, "socks5://", "", -1), "@")
		if len(auths_hosts) == 2 {
			auths = strings.Split(auths_hosts[0], ":")
			if len(auths) == 2 {
				auth = &proxy.Auth{User: auths[0], Password: auths[1]}
			} else {
				auth = &proxy.Auth{User: auths[0], Password: ""}
			}
			hosts = auths_hosts[1]
		} else {
			auth = nil
			hosts = auths_hosts[0]
		}
		dialer, err := proxy.SOCKS5(
			"tcp",
			hosts,
			auth,
			&net.Dialer{
				Timeout:   time.Duration(timeout) * time.Second,
				KeepAlive: time.Duration(timeout) * time.Second,
			},
		)
		if err != nil {
			return nil, err
		}

		client = &http.Client{
			Transport: &http.Transport{
				Proxy:               nil,
				Dial:                dialer.Dial,
				TLSHandshakeTimeout: time.Duration(timeout) * time.Second,
			},
			Timeout: time.Duration(timeout) * time.Second,
		}
	} else {
		client = &http.Client{
			Timeout: time.Duration(timeout) * time.Second,
		}
	}

	if len(w.Bind) > 0 {
		localAddr, err := net.ResolveIPAddr("ip", w.Bind)
		if err != nil {
			return nil, errors.New(fmt.Sprintf("bind local ip[%s] error:%s", w.Bind, err.Error()))
		}
		localTCPAddr := net.TCPAddr{
			IP: localAddr.IP,
		}
		d := net.Dialer{
			LocalAddr: &localTCPAddr,
			Timeout:   time.Duration(timeout) * time.Second,
			KeepAlive: time.Duration(timeout) * time.Second,
		}

		tr := &http.Transport{
			Proxy:               http.ProxyFromEnvironment,
			Dial:                d.Dial,
			TLSHandshakeTimeout: time.Duration(timeout) * time.Second,
		}
		client.Transport = tr
	}

	return client, nil
}

func (w *Work) getWebsocketClient() (*websocket.Dialer, error) {
	var timeout int = 30
	var client *websocket.Dialer
	if strings.HasPrefix(w.Proxy, "http://") {
		urlParse := url.URL{}
		urlProxy, err := urlParse.Parse(w.Proxy)
		if err != nil {
			return nil, err
		}

		client = &websocket.Dialer{
			Proxy: http.ProxyURL(urlProxy),
		}
	} else if strings.HasPrefix(w.Proxy, "socks5://") {
		var auths_hosts, auths []string
		var hosts string
		var auth *proxy.Auth
		auths_hosts = strings.Split(strings.Replace(w.Proxy, "socks5://", "", -1), "@")
		if len(auths_hosts) == 2 {
			auths = strings.Split(auths_hosts[0], ":")
			if len(auths) == 2 {
				auth = &proxy.Auth{User: auths[0], Password: auths[1]}
			} else {
				auth = &proxy.Auth{User: auths[0], Password: ""}
			}
			hosts = auths_hosts[1]
		} else {
			auth = nil
			hosts = auths_hosts[0]
		}
		dialer, err := proxy.SOCKS5(
			"tcp",
			hosts,
			auth,
			&net.Dialer{
				Timeout: time.Duration(timeout) * time.Second,
				//KeepAlive: time.Duration(timeout) * time.Second,
			},
		)
		if err != nil {
			return nil, err
		}

		client = &websocket.Dialer{
			NetDial: dialer.Dial,
		}
	} else {
		client = &websocket.Dialer{
			Proxy: http.ProxyFromEnvironment,
		}
	}

	if len(w.Bind) > 0 {
		client.LocalAddr = w.Bind
	}

	return client, nil
}

//http线程返回结果结构函数
func retrunJson(msg string, status bool, data interface{}) []byte {
	b, err := json.Marshal(Result{status, msg, data})
	if err != nil {
		log.Println("[retrunJson] Marshal", err)
	}
	return b
}

//http线程返回结果结构
type Result struct {
	Status bool        `json:"status"`
	Msg    string      `json:"msg"`
	Data   interface{} `json:"data"`
}

type Count struct {
	sync.Mutex
	Num map[string]int
}

//工作线程结构
type Work struct {
	sync.Mutex
	Host        string
	Port        int
	Token       string
	Platform    map[string]*currentPrices
	Platform24  map[string]*currentPrices
	NotifyCount Count
	Gap         int
	SaveGap     int
	Proxy       string
	NginxProxy  string
	OutDir      string
	RedisConfig map[string]string
	Redis       goredis.Client
	Info        string
	Bind        string
}

type currentPrices struct {
	sync.Mutex
	Data map[string]currentPrice
}

//数据格式
type currentPrice struct {
	Symbol  string  `json:"symbol"`
	Coin    string  `json:"coin"`
	Market  string  `json:"market"`
	Price   float64 `json:"price"`
	Time    string  `json:"time"`
	UpPrice float64 `json:"upprice"`
	Upime   string  `json:"uptime"`
	Change  float64 `json:"change"`
}

//http线程
func (w *Work) RunHttp() {
	// info
	http.HandleFunc("/api/debug/", w.Debug)
	// 涨跌幅排行榜
	http.HandleFunc("/api/currentPrices/", w.CurrentPrices)
	// 现价
	http.HandleFunc("/api/currentRank/", w.CurrentRank)

	listen := (w.Host + ":" + strconv.Itoa(w.Port))
	err := http.ListenAndServe(listen, nil)
	if err != nil {
		log.Fatalln("[http] ListenAndServe: ", err)
		return
	}
	log.Println("[http] start ", w.Host, w.Port)
}

//http线程信息接口函数
func (w *Work) Debug(resp http.ResponseWriter, req *http.Request) {
	resp.Write(retrunJson(w.Info, true, nil))
}

//http线程涨跌幅排行榜接口函数
func (w *Work) CurrentRank(resp http.ResponseWriter, req *http.Request) {
	//up涨榜 down跌榜
	req.ParseForm()
	var change string = "up"
	if len(req.Form["change"]) > 0 && len(req.Form["change"][0]) > 0 {
		change = req.Form["change"][0]
	}

	if len(req.Form["test"]) < 1 || len(req.Form["test"][0]) < 1 {
		resp.Write(retrunJson("[currentRank] 暂时关闭旧接口", false, nil))
		return
	}

	//获取涨跌榜对象
	var markets map[int]string = make(map[int]string)
	var data [][]byte
	var err error
	if change == "down" {
		data, err = w.Redis.Zrangebyscore("currentRank", float64(-100), float64(-0.00000000000000000001), 0, 10)
	} else {
		data, err = w.Redis.Zrevrangebyscore("currentRank", float64(100), float64(0.00000000000000000001), 0, 10)
	}

	if err == nil {
		for k, v := range data {
			if k%2 == 0 {
				markets[k] = string(v)
			}
		}
	} else {
		resp.Write(retrunJson("[currentRank] found invail", false, nil))
		return
	}

	//根据涨跌榜对象获取涨跌榜数据
	var resultData map[string]map[string]currentPrice = make(map[string]map[string]currentPrice)
	for _, marketValue := range markets {
		tmp := strings.Split(strings.Trim(strings.ToLower(marketValue), " "), "|")
		if len(tmp) != 3 {
			resp.Write(retrunJson("[currentRank] markets invail", false, nil))
			return
		}
		symbol := tmp[0] + "-" + tmp[1]
		site := tmp[2]

		data, platformExist := w.Platform[site]
		if !platformExist {
			log.Println("[currentRank] site no exist", site)
			continue
		}
		_, siteExist := resultData[site]
		if !siteExist {
			resultData[site] = make(map[string]currentPrice)
		}
		currentPrice, symbolExist := data.Data[symbol]
		if symbolExist {
			resultData[site][symbol] = currentPrice
		} else {
			log.Println("[currentRank] symbol no exist", symbol)
		}
	}

	resp.Write(retrunJson("ok", true, resultData))
}

// http线程现价接口函数
func (w *Work) CurrentPrices(resp http.ResponseWriter, req *http.Request) {
	req.ParseForm()

	if len(req.Form["markets[]"]) > 0 && len(req.Form["markets[]"][0]) > 0 { //指定市场对
		markets := req.Form["markets[]"]
		var resultData map[string]map[string]currentPrice = make(map[string]map[string]currentPrice)
		for marketIndex := range markets {
			tmp := strings.Split(strings.ToLower(markets[marketIndex]), "|")
			if len(tmp) != 3 {
				resp.Write(retrunJson("[CurrentPrices] markets invail", false, nil))
				return
			}
			symbol := tmp[0] + "-" + tmp[1]
			site := tmp[2]

			data, platformExist := w.Platform[site]
			if !platformExist {
				continue
			}
			_, siteExist := resultData[site]
			if !siteExist {
				resultData[site] = make(map[string]currentPrice)
			}
			data.Lock()
			currentPrice, symbolExist := data.Data[symbol]
			if symbolExist {
				resultData[site][symbol] = currentPrice
			}
			data.Unlock()
		}
		resp.Write(retrunJson("ok", true, resultData))
		return
	} else if len(req.Form["site"]) > 0 && len(req.Form["site"][0]) > 0 { //指定平台
		site := req.Form["site"][0]
		data, platformExist := w.Platform[site]
		if !platformExist {
			resp.Write(retrunJson("[CurrentPrices] data invail", false, nil))
			return
		}

		var resultData map[string]map[string]currentPrice = make(map[string]map[string]currentPrice)
		if len(req.Form["market"]) > 0 && len(req.Form["market"][0]) > 0 { //同时指定了市场
			market := strings.ToLower(req.Form["market"][0])
			resultData[site] = make(map[string]currentPrice)
			data.Lock()
			for k, v := range data.Data {
				if v.Market == market {
					resultData[site][k] = v
				}
			}
			resp.Write(retrunJson("ok", true, resultData))
			data.Unlock()
			return
		} else { //平台内所有市场对
			data.Lock()
			resultData[site] = data.Data
			resp.Write(retrunJson("ok", true, resultData))
			data.Unlock()
			return
		}

	}

	resp.Write(retrunJson("[CurrentPrices] site invail", false, nil))
}

//工作线程，分协程读取个平台现价、存储涨跌幅、存储平台现价文件，存储历史现价
func (w *Work) runWorkers() {

	// huobi websocket 实时读取推送过来的数据
	go func() {
		w.Lock()
		w.Platform["huobi"] = new(currentPrices)
		w.Unlock()

		w.Platform["huobi"].Lock()
		w.Platform["huobi"].Data = make(map[string]currentPrice)
		w.Platform["huobi"].Unlock()

		w.setNotify("huobi", 0)
		for {
			w.runWorkerHuobi()
			// 超过5次错误后休息一分钟
			if w.getNotify("huobi") > 5 {
				log.Println("[huobi] 读取现价接口超过五次错误休息一分钟 ")
				w.notify("[huobi] currentPrice fail", "读取现价接口超过五次错误休息一分钟")
				w.setNotify("huobi", 0)
				time.Sleep(60 * time.Second)
			}
			log.Println("[huobi] websocket reconnecting ", w.getNotify("huobi"))
		}
		w.notify("[huobi] 协程结束", "")
	}()

	// hadax websocket
	go func() {
		w.Lock()
		w.Platform["hadax"] = new(currentPrices)
		w.Unlock()

		w.Platform["hadax"].Lock()
		w.Platform["hadax"].Data = make(map[string]currentPrice)
		w.Platform["hadax"].Unlock()

		w.setNotify("hadax", 0)
		for {
			w.runWorkerHadax()
			// 超过5次错误后休息一分钟
			if w.getNotify("hadax") > 5 {
				log.Println("[hadax] 读取现价接口超过五次错误休息一分钟 ")
				w.notify("[hadax] currentPrice fail", "读取现价接口超过五次错误休息一分钟")
				w.setNotify("hadax", 0)
				time.Sleep(60 * time.Second)
			}
			log.Println("[hadax] websocket reconnecting ", w.getNotify("hadax"))
		}
		w.notify("[hadax] 协程结束", "")
	}()

	// fcoin websocket
	go func() {
		w.Lock()
		w.Platform["fcoin"] = new(currentPrices)
		w.Unlock()

		w.Platform["fcoin"].Lock()
		w.Platform["fcoin"].Data = make(map[string]currentPrice)
		w.Platform["fcoin"].Unlock()

		w.setNotify("fcoin", 0)
		for {
			w.runWorkerFcoin()
			// 超过5次错误后休息一分钟
			if w.getNotify("fcoin") > 5 {
				log.Println("[fcoin] 读取现价接口超过五次错误休息一分钟 ")
				w.notify("[fcoin] currentPrice fail", "读取现价接口超过五次错误休息一分钟")
				w.setNotify("fcoin", 0)
				time.Sleep(60 * time.Second)
			}
			log.Println("[fcoin] websocket reconnecting ", w.getNotify("fcoin"))
		}
		w.notify("[fcoin] 协程结束", "")
	}()

	// okex http
	go func() {
		w.Lock()
		w.Platform["okex"] = new(currentPrices)
		w.Unlock()

		w.Platform["okex"].Lock()
		w.Platform["okex"].Data = make(map[string]currentPrice)
		w.Platform["okex"].Unlock()

		w.setNotify("okex", 0)
		ticker := time.NewTicker(time.Duration(w.Gap) * time.Millisecond)
		w.runWorkerOkex()
		for range ticker.C {
			w.runWorkerOkex()
			// 超过10次错误后休息两分钟
			if w.getNotify("okex") > 10 {
				log.Println("[okex] 读取现价接口超过十次错误休息两分钟 ")
				w.notify("[okex] currentPrice fail", "读取现价接口超过十次错误休息两分钟")
				w.setNotify("okex", 0)
				time.Sleep(120 * time.Second)
			}
		}
		w.notify("[okex] 协程结束", "")
	}()

	// binance http
	go func() {
		w.Lock()
		w.Platform["binance"] = new(currentPrices)
		w.Unlock()

		w.Platform["binance"].Lock()
		w.Platform["binance"].Data = make(map[string]currentPrice)
		w.Platform["binance"].Unlock()

		w.setNotify("binance", 0)
		ticker := time.NewTicker(time.Duration(w.Gap) * time.Millisecond)
		w.runWorkerBinance()
		for range ticker.C {
			w.runWorkerBinance()
			// 超过10次错误后休息两分钟
			if w.getNotify("binance") > 10 {
				log.Println("[binance] 读取现价接口超过十次错误休息两分钟 ")
				w.notify("[binance] currentPrice fail", "读取现价接口超过十次错误休息两分钟")
				w.setNotify("binance", 0)
				time.Sleep(120 * time.Second)
			}
		}
		w.notify("[binance] 协程结束", "")
	}()

	// gate http
	go func() {
		w.Lock()
		w.Platform["gate"] = new(currentPrices)
		w.Unlock()

		w.Platform["gate"].Lock()
		w.Platform["gate"].Data = make(map[string]currentPrice)
		w.Platform["gate"].Unlock()

		w.setNotify("gate", 0)
		ticker := time.NewTicker(time.Duration(w.Gap) * time.Millisecond)
		w.runWorkerGate()
		for range ticker.C {
			w.runWorkerGate()
			// 超过10次错误后休息两分钟
			if w.getNotify("gate") > 10 {
				log.Println("[gate] 读取现价接口超过十次错误休息两分钟 ")
				w.notify("[gate] currentPrice fail", "读取现价接口超过十次错误休息两分钟")
				w.setNotify("gate", 0)
				time.Sleep(120 * time.Second)
			}
		}
		w.notify("[gate] 协程结束", "")
	}()

	// zb http
	go func() {
		w.Lock()
		w.Platform["zb"] = new(currentPrices)
		w.Unlock()

		w.Platform["zb"].Lock()
		w.Platform["zb"].Data = make(map[string]currentPrice)
		w.Platform["zb"].Unlock()

		w.setNotify("zb", 0)
		ticker := time.NewTicker(time.Duration(w.Gap) * time.Millisecond)
		w.runWorkerZb()
		for range ticker.C {
			//for {
			w.runWorkerZb()
			// 超过10次错误后休息两分钟
			if w.getNotify("zb") > 10 {
				log.Println("[zb] 读取现价接口超过十次错误休息两分钟 ")
				w.notify("[zb] currentPrice fail", "读取现价接口超过十次错误休息两分钟")
				w.setNotify("zb", 0)
				time.Sleep(120 * time.Second)
			}
		}
		w.notify("[zb] 协程结束", "")
	}()

	// huilv http
	go func() {
		w.Lock()
		w.Platform["huilv"] = new(currentPrices)
		w.Unlock()

		w.Platform["huilv"].Lock()
		w.Platform["huilv"].Data = make(map[string]currentPrice)
		w.Platform["huilv"].Unlock()

		w.setNotify("huilv", 0)
		ticker := time.NewTicker(time.Duration(w.Gap) * time.Millisecond * 100)
		w.runWorkerHuilv()
		for range ticker.C {
			w.runWorkerHuilv()
			// 超过10次错误后休息两分钟
			if w.getNotify("huilv") > 10 {
				log.Println("[huilv] 读取现价接口超过十次错误休息两分钟 ")
				w.notify("[huilv] currentPrice fail", "读取现价接口超过十次错误休息两分钟")
				w.setNotify("huilv", 0)
				time.Sleep(120 * time.Second)
			}
		}
		w.notify("[huilv] 协程结束", "")
	}()

	// 存储历史现价用于计算涨跌幅
	go func() {
		var storeKey string = "currentZset"
		// 立即获取24小时历史现价，供涨跌幅计算
		w.save24History(storeKey)

		// 定时处理
		ticker := time.NewTicker(time.Duration(w.SaveGap) * time.Second) //存储时间间隔由配置决定
		for range ticker.C {
			// 存储现价成历史数据
			w.saveHistory(storeKey)
			// 获取24小时历史现价，供涨跌幅计算
			w.save24History(storeKey)
		}
		w.notify("[platform24] 协程结束", "")
	}()
}

// 存储现价成历史数据
func (w *Work) saveHistory(storeKey string) {
	var now time.Time = time.Now()

	var resultData map[string]map[string]currentPrice = make(map[string]map[string]currentPrice)
	for k, v := range w.Platform {
		v.Lock()
		defer v.Unlock()
		resultData[k] = v.Data
	}

	//存储
	var encodeBuffer bytes.Buffer
	enc := gob.NewEncoder(&encodeBuffer)
	err := enc.Encode(resultData)
	if err != nil {
		log.Println("[saveHistory] encode:", err)
	} else {
		w.Redis.Zadd(storeKey, []byte(encodeBuffer.String()), float64(now.Unix()))
		//w.Redis.Expire(storeKey, int64(87000))
	}
}

// 获取24小时历史现价，供涨跌幅计算
func (w *Work) save24History(storeKey string) {
	var now time.Time = time.Now()

	data1, err := w.Redis.Zrevrangebyscore(storeKey, float64(now.Unix()-86400), float64(now.Unix()-87000), 0, 1)
	if err == nil && len(data1) == 2 {
		var platformData map[string]map[string]currentPrice = make(map[string]map[string]currentPrice)

		dec := gob.NewDecoder(bytes.NewBuffer(data1[0]))
		err = dec.Decode(&platformData)
		if err != nil {
			log.Println("[save24History] decode data1:", err)
			return
		}
		for k, v := range platformData {
			if _, platformExist := w.Platform24[k]; !platformExist {
				w.Platform24[k] = new(currentPrices)
			}
			w.Platform24[k].Lock()
			w.Platform24[k].Data = v
			w.Platform24[k].Unlock()
		}
		//删除过期的数据
		w.Redis.Zremrangebyscore(storeKey, float64(0), float64(now.Unix()-87000))
	} else {
		data2, err := w.Redis.Zrangebyscore(storeKey, float64(now.Unix()-86400), float64(now.Unix()), 0, 1)
		if err == nil && len(data2) == 2 {
			var platformData map[string]map[string]currentPrice = make(map[string]map[string]currentPrice)

			dec := gob.NewDecoder(bytes.NewBuffer(data2[0]))
			err = dec.Decode(&platformData)
			if err != nil {
				log.Println("[save24History] decode data2:", err)
				return
			}

			for k, v := range platformData {
				if _, platformExist := w.Platform24[k]; !platformExist {
					w.Platform24[k] = new(currentPrices)
				}
				w.Platform24[k].Lock()
				w.Platform24[k].Data = v
				w.Platform24[k].Unlock()
			}
		} else {
			for k, v := range w.Platform {
				if _, platformExist := w.Platform24[k]; !platformExist {
					w.Platform24[k] = new(currentPrices)
				}
				v.Lock()
				w.Platform24[k].Data = v.Data
				v.Unlock()
			}
		}
	}
}

// 计数器用来计数通知和任务休息
func (w *Work) incrNotify(site string) {
	w.NotifyCount.Lock()
	defer w.NotifyCount.Unlock()

	_, siteExist := w.NotifyCount.Num[site]
	if siteExist {
		w.NotifyCount.Num[site] = w.NotifyCount.Num[site] + 1
		return
	}
	w.NotifyCount.Num[site] = 1
	return
}
func (w *Work) setNotify(site string, value int) {
	w.NotifyCount.Lock()
	defer w.NotifyCount.Unlock()

	_, siteExist := w.NotifyCount.Num[site]
	if siteExist {
		w.NotifyCount.Num[site] = value
		return
	}

	w.NotifyCount.Num[site] = value
	return
}
func (w *Work) getNotify(site string) int {
	w.NotifyCount.Lock()
	defer w.NotifyCount.Unlock()

	_, siteExist := w.NotifyCount.Num[site]
	if siteExist {
		return w.NotifyCount.Num[site]
	}
	return 0
}

// gzip压缩用于wesocket
func GzipEncode(in []byte) ([]byte, error) {
	var (
		buffer bytes.Buffer
		out    []byte
		err    error
	)
	writer := gzip.NewWriter(&buffer)
	_, err = writer.Write(in)
	if err != nil {
		writer.Close()
		return out, err
	}
	err = writer.Close()
	if err != nil {
		return out, err
	}

	return buffer.Bytes(), nil
}

// gzip解压用于wesocket
func GzipDecode(in []byte) ([]byte, error) {
	reader, err := gzip.NewReader(bytes.NewReader(in))
	if err != nil {
		var out []byte
		return out, err
	}
	defer reader.Close()

	return ioutil.ReadAll(reader)
}

// huobi现价
func (w *Work) runWorkerHuobi() {
	//连接websocket
	var u url.URL
	if len(w.NginxProxy) == 0 {
		u = url.URL{Scheme: "ws", Host: "api.huobi.pro", Path: "/ws"}
	} else {
		u = url.URL{Scheme: "ws", Host: w.NginxProxy, Path: "/huobi/ws"}
	}

	DefaultDialer, err := w.getWebsocketClient()
	if err != nil {
		log.Println("[huobi] ", err.Error())
		w.incrNotify("huobi")
		return
	}
	header := http.Header{"User-Agent": []string{"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_12_6) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/67.0.3396.99 Safari/537.36"}}
	ws, _, err := DefaultDialer.Dial(u.String(), header)
	if err != nil {
		log.Println("[huobi] ", err.Error())
		w.incrNotify("huobi")
		return
	}
	defer ws.Close()

	//订阅现价数据
	err = ws.WriteMessage(websocket.TextMessage, []byte("{\"sub\":\"market.overview\"}"))
	if err != nil {
		log.Println("[huobi] ", err)
		w.incrNotify("huobi")
		return
	}
	log.Println("[huobi] websocket connected")

	//数据
	var i int = 0                                            //websocket出错多次退出
	timeout := time.After(time.Second * time.Duration(3600)) //超时退出
ForEnd:
	for {
		select {
		case <-timeout:
			log.Println("[huobi] timeout")
			break ForEnd
		default:
			//多次出错后重连
			if i > 100 {
				log.Println("[huobi] too many err:", i)
				break ForEnd
			}

			//阻塞读取数据
			_, originMsg, err := ws.ReadMessage()
			if err != nil {
				log.Println("[huobi] ws read err:", err)
				w.incrNotify("huobi")
				i = i + 40
				continue ForEnd
			}
			//解压数据
			msg, err := GzipDecode(originMsg)
			if err != nil {
				log.Println("[huobi] gzip decode err:", err)
				w.incrNotify("huobi")
				i = i + 10
				continue ForEnd
			}

			if strings.Contains(string(msg), "ping") { //心跳
				if err := ws.WriteMessage(websocket.TextMessage, []byte(strings.Replace(string(msg), "ping", "pong", 1))); err != nil {
					log.Println("[huobi] ws pong err:", err)
					w.incrNotify("huobi")
					i = i + 10
					continue ForEnd
				}
			} else {
				if tmp := gjson.GetBytes(msg, "ch"); tmp.String() == "market.overview" {
					if !gjson.Valid(string(msg)) {
						log.Println("[huobi] invalid json")
						w.incrNotify("huobi")
						i = i + 5
						continue ForEnd
					}
					result := gjson.GetBytes(msg, "data")
					if result.Exists() {
						//现价数据
						result.ForEach(func(key, value gjson.Result) bool {
							symbol := strings.ToLower(value.Get("symbol").String())
							coin := symbol[0 : len(symbol)-4]
							market := symbol[len(symbol)-4 : len(symbol)]
							now := time.Now().Format("20060102150405")
							if symbol[len(symbol)-2:len(symbol)] == "ht" {
								coin = symbol[0 : len(symbol)-2]
								market = symbol[len(symbol)-2 : len(symbol)]
							} else if market != "usdt" {
								coin = symbol[0 : len(symbol)-3]
								market = symbol[len(symbol)-3 : len(symbol)]
							}
							price := value.Get("close").Float()
							if price <= 0 {
								//return true
							}

							var pencent float64 = 0
							var upPrice float64 = 0
							var upTime string
							if _, platformExist := w.Platform24["huobi"]; platformExist {
								w.Platform24["huobi"].Lock()
								if prevPrice, prevExist := w.Platform24["huobi"].Data[coin+"-"+market]; prevExist {
									if prevPrice.Price != 0 {
										pencent = (price - prevPrice.Price) / prevPrice.Price
									}
									upPrice = prevPrice.Price
									upTime = prevPrice.Time
									w.Redis.Zadd("currentRank", []byte(coin+"|"+market+"|huobi"), pencent)
								}
								w.Platform24["huobi"].Unlock()
							}

							w.Platform["huobi"].Lock()
							w.Platform["huobi"].Data[coin+"-"+market] = currentPrice{
								symbol,
								coin,
								market,
								price,
								now,
								upPrice,
								upTime,
								pencent,
							}
							w.Platform["huobi"].Unlock()

							if _, platformExist := w.Platform["hadax"]; platformExist && (coin+"-"+market == "ht-usdt" || coin+"-"+market == "eth-usdt" || coin+"-"+market == "btc-usdt") {
								w.Platform["hadax"].Lock()
								w.Platform["hadax"].Data[coin+"-"+market] = currentPrice{
									symbol,
									coin,
									market,
									price,
									now,
									upPrice,
									upTime,
									pencent,
								}
								w.Platform["hadax"].Unlock()
							}

							return true // keep iterating
						})
						//错误计数归零
						w.setNotify("huobi", 0)
						//wesocket错误计数归零
						i = 0
						//保存现价数据为文件
						if len(w.OutDir) > 0 {
							w.Platform["huobi"].Lock()
							w.save(string(retrunJson("[huobi] ok", true, w.Platform["huobi"].Data)), "huobi")
							w.Platform["huobi"].Unlock()
						}
					} else {
						log.Println("[huobi] data nil")
						w.incrNotify("huobi")
						i = i + 5
					}
				} else {
					i = i + 1
				}
			}
		}
	}
}

// hadax现价
func (w *Work) runWorkerHadax() {
	//连接websocket
	var u url.URL
	if len(w.NginxProxy) == 0 {
		u = url.URL{Scheme: "wss", Host: "www.huobi.br.com", Path: "/-/s/hdx/ws"}
	} else {
		u = url.URL{Scheme: "ws", Host: w.NginxProxy, Path: "/hadax/ws"}
	}

	DefaultDialer, err := w.getWebsocketClient()
	if err != nil {
		log.Println("[hadax] ", err.Error())
		w.incrNotify("hadax")
		return
	}
	header := http.Header{"User-Agent": []string{"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_12_6) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/67.0.3396.99 Safari/537.36"}}
	ws, _, err := DefaultDialer.Dial(u.String(), header)
	if err != nil {
		log.Println("[hadax] ", err.Error())
		w.incrNotify("hadax")
		return
	}
	defer ws.Close()

	//订阅现价数据
	err = ws.WriteMessage(websocket.TextMessage, []byte("{\"sub\":\"market.overview\"}"))
	if err != nil {
		log.Println("[hadax] ", err)
		w.incrNotify("hadax")
		return
	}
	log.Println("[hadax] websocket connected")

	//数据
	var i int = 0                                            //websocket出错多次退出
	timeout := time.After(time.Second * time.Duration(3600)) //超时退出
ForEnd:
	for {
		select {
		case <-timeout:
			log.Println("[huobi] timeout")
			break ForEnd
		default:
			//多次出错后重连
			if i > 100 {
				log.Println("[hadax] too many err:", i)
				break ForEnd
			}

			//阻塞读取数据
			_, originMsg, err := ws.ReadMessage()
			if err != nil {
				log.Println("[hadax] ws read err:", err)
				w.incrNotify("hadax")
				i = i + 40
				continue ForEnd
			}
			//解压数据
			msg, err := GzipDecode(originMsg)
			if err != nil {
				log.Println("[hadax] gzip decode err:", err)
				w.incrNotify("hadax")
				i = i + 10
				continue ForEnd
			}

			if strings.Contains(string(msg), "ping") { //心跳
				if err := ws.WriteMessage(websocket.TextMessage, []byte(strings.Replace(string(msg), "ping", "pong", 1))); err != nil {
					log.Println("[hadax] ws pong err:", err)
					w.incrNotify("hadax")
					i = i + 10
					continue ForEnd
				}
			} else {
				if tmp := gjson.GetBytes(msg, "ch"); tmp.String() == "market.overview" {
					if !gjson.Valid(string(msg)) {
						log.Println("[hadax] invalid json")
						w.incrNotify("hadax")
						i = i + 5
						continue ForEnd
					}
					result := gjson.GetBytes(msg, "data")
					if result.Exists() {
						//现价数据
						result.ForEach(func(key, value gjson.Result) bool {
							symbol := strings.ToLower(value.Get("symbol").String())
							coin := symbol[0 : len(symbol)-4]
							market := symbol[len(symbol)-4 : len(symbol)]
							now := time.Now().Format("20060102150405")
							if symbol[len(symbol)-2:len(symbol)] == "ht" {
								coin = symbol[0 : len(symbol)-2]
								market = symbol[len(symbol)-2 : len(symbol)]
							} else if market != "usdt" {
								coin = symbol[0 : len(symbol)-3]
								market = symbol[len(symbol)-3 : len(symbol)]
							}
							price := value.Get("close").Float()
							if price <= 0 {
								//return true
							}

							var pencent float64 = 0
							var upPrice float64 = 0
							var upTime string
							if _, platformExist := w.Platform24["hadax"]; platformExist {
								w.Platform24["hadax"].Lock()
								if prevPrice, prevExist := w.Platform24["hadax"].Data[coin+"-"+market]; prevExist {
									if prevPrice.Price != 0 {
										pencent = (price - prevPrice.Price) / prevPrice.Price
									}
									upPrice = prevPrice.Price
									upTime = prevPrice.Time
									w.Redis.Zadd("currentRank", []byte(coin+"|"+market+"|hadax"), pencent)
								}
								w.Platform24["hadax"].Unlock()
							}

							w.Platform["hadax"].Lock()
							w.Platform["hadax"].Data[coin+"-"+market] = currentPrice{
								symbol,
								coin,
								market,
								price,
								now,
								upPrice,
								upTime,
								pencent,
							}
							w.Platform["hadax"].Unlock()
							return true // keep iterating
						})
						//错误计数归零
						w.setNotify("hadax", 0)
						//wesocket错误计数归零
						i = 0
						//保存现价数据为文件
						if len(w.OutDir) > 0 {
							w.Platform["hadax"].Lock()
							w.save(string(retrunJson("[hadax] ok", true, w.Platform["hadax"].Data)), "hadax")
							w.Platform["hadax"].Unlock()
						}
					} else {
						log.Println("[hadax] data nil")
						w.incrNotify("hadax")
						i = i + 5
					}
				} else {
					i = i + 1
				}
			}
		}
	}
}

// fcoin现价
func (w *Work) runWorkerFcoin() {
	//连接websocket
	var u url.URL
	if len(w.NginxProxy) == 0 {
		u = url.URL{Scheme: "wss", Host: "api.fcoin.com", Path: "/v2/ws"}
	} else {
		u = url.URL{Scheme: "ws", Host: w.NginxProxy, Path: "/fcoin/ws"}
	}

	DefaultDialer, err := w.getWebsocketClient()
	if err != nil {
		log.Println("[fcoin] ", err.Error())
		w.incrNotify("fcoin")
		return
	}
	header := http.Header{"User-Agent": []string{"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_12_6) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/67.0.3396.99 Safari/537.36"}}
	ws, _, err := DefaultDialer.Dial(u.String(), header)
	if err != nil {
		log.Println("[fcoin] ", err.Error())
		w.incrNotify("fcoin")
		return
	}
	defer ws.Close()

	//订阅现价数据
	err = ws.WriteMessage(websocket.TextMessage, []byte("{\"cmd\":\"sub\",\"args\":[\"all-tickers\"],\"id\":\"20271c80-79d1-11e8-8fa7-e1d825d3873b\"}"))
	if err != nil {
		log.Println("[fcoin] ", err)
		w.incrNotify("fcoin")
		return
	}
	log.Println("[fcoin] websocket connected")

	//数据
	var i int = 0                                            //websocket出错多次退出
	timeout := time.After(time.Second * time.Duration(3600)) //超时退出
ForEnd:
	for {
		select {
		case <-timeout:
			log.Println("[fcoin] timeout")
			break ForEnd
		default:
			//多次出错后重连
			if i > 100 {
				log.Println("[fcoin] too many err:", i)
				break ForEnd
			}

			//阻塞读取数据
			_, originMsg, err := ws.ReadMessage()
			if err != nil {
				log.Println("[fcoin] ws read err:", err)
				w.incrNotify("fcoin")
				i = i + 40
				continue ForEnd
			}
			//解压数据
			/*
				msg, err := GzipDecode(originMsg)
				if err != nil {
					log.Println("[fcoin] gzip decode err:", err)
					w.incrNotify("fcoin")
					i = i + 10
					continue ForEnd
				}
			*/
			msg := originMsg

			if strings.Contains(string(msg), "ping") { //心跳
				log.Println("[fcoin] ws ping msg:", string(msg))
				w.incrNotify("fcoin")
				i = i + 10
				continue ForEnd
			} else {
				if tmp := gjson.GetBytes(msg, "topic"); tmp.String() == "all-tickers" {
					if !gjson.Valid(string(msg)) {
						log.Println("[fcoin] invalid json")
						w.incrNotify("fcoin")
						i = i + 5
						continue ForEnd
					}
					result := gjson.GetBytes(msg, "tickers")
					if result.Exists() {
						//现价数据
						result.ForEach(func(key, value gjson.Result) bool {
							symbol := strings.ToLower(value.Get("symbol").String())
							coin := symbol[0 : len(symbol)-4]
							market := symbol[len(symbol)-4 : len(symbol)]
							now := time.Now().Format("20060102150405")
							if symbol[len(symbol)-2:len(symbol)] == "ft" {
								coin = symbol[0 : len(symbol)-2]
								market = symbol[len(symbol)-2 : len(symbol)]
							} else if market != "usdt" {
								coin = symbol[0 : len(symbol)-3]
								market = symbol[len(symbol)-3 : len(symbol)]
							}
							price := value.Get("ticker.0").Float()
							if price <= 0 {
								//return true
							}

							var pencent float64 = 0
							var upPrice float64 = 0
							var upTime string
							if _, platformExist := w.Platform24["fcoin"]; platformExist {
								w.Platform24["fcoin"].Lock()
								if prevPrice, prevExist := w.Platform24["fcoin"].Data[coin+"-"+market]; prevExist {
									if prevPrice.Price != 0 {
										pencent = (price - prevPrice.Price) / prevPrice.Price
									}
									upPrice = prevPrice.Price
									upTime = prevPrice.Time
									w.Redis.Zadd("currentRank", []byte(coin+"|"+market+"|fcoin"), pencent)
								}
								w.Platform24["fcoin"].Unlock()
							}

							w.Platform["fcoin"].Lock()
							w.Platform["fcoin"].Data[coin+"-"+market] = currentPrice{
								symbol,
								coin,
								market,
								price,
								now,
								upPrice,
								upTime,
								pencent,
							}
							w.Platform["fcoin"].Unlock()
							return true // keep iterating
						})
						//错误计数归零
						w.setNotify("fcoin", 0)
						//wesocket错误计数归零
						i = 0
						//保存现价数据为文件
						if len(w.OutDir) > 0 {
							w.Platform["fcoin"].Lock()
							w.save(string(retrunJson("[fcoin] ok", true, w.Platform["fcoin"].Data)), "fcoin")
							w.Platform["fcoin"].Unlock()
						}
					} else {
						log.Println("[fcoin] data nil")
						w.incrNotify("fcoin")
						i = i + 5
					}
				} else {
					i = i + 1
				}
			}
		}
	}
}

// okex现价
func (w *Work) runWorkerOkex() {
	// 现价接口
	var err error

	client, err := w.getHttpClient()
	if err != nil {
		log.Println("[okex] ", err.Error())
		w.incrNotify("okex")
		return
	}

	var req *http.Request
	if len(w.NginxProxy) == 0 {
		req, err = http.NewRequest("GET", "https://www.okex.com/v2/spot/markets/tickers", nil)
	} else {
		req, err = http.NewRequest("GET", "http://"+w.NginxProxy+"/okex/v2/spot/markets/tickers", nil)
	}

	if err != nil {
		log.Println("[okex] ", err.Error())
		w.incrNotify("okex")
		return
	}
	req.Header.Add("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_12_6) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/67.0.3396.99 Safari/537.36")

	resp, err := client.Do(req)
	if err != nil {
		log.Println("[okex] ", err.Error())
		w.incrNotify("okex")
		return
	}
	defer resp.Body.Close()
	body, err := ioutil.ReadAll(resp.Body)

	// 数据处理
	if !gjson.Valid(string(body)) {
		log.Println("[okex] invalid json")
		w.incrNotify("okex")
		return
	}
	result := gjson.GetBytes(body, "data")
	if result.Exists() {
		// 处理现价
		result.ForEach(func(key, value gjson.Result) bool {
			symbol := strings.ToLower(value.Get("symbol").String())
			tmp := strings.Split(symbol, "_")
			if len(tmp) != 2 {
				log.Println("[okex] invalid symbol", symbol)
				w.incrNotify("okex")
				return true
			}
			coin := tmp[0]
			market := tmp[1]
			now := time.Now().Format("20060102150405")
			price := value.Get("close").Float()
			if price <= 0 {
				//return true
			}

			var pencent float64 = 0
			var upPrice float64 = 0
			var upTime string
			if _, platformExist := w.Platform24["okex"]; platformExist {
				w.Platform24["okex"].Lock()
				if prevPrice, prevExist := w.Platform24["okex"].Data[coin+"-"+market]; prevExist {
					if prevPrice.Price != 0 {
						pencent = (price - prevPrice.Price) / prevPrice.Price
					}
					upPrice = prevPrice.Price
					upTime = prevPrice.Time
					w.Redis.Zadd("currentRank", []byte(coin+"|"+market+"|okex"), pencent)
				}
				w.Platform24["okex"].Unlock()
			}

			w.Platform["okex"].Lock()
			w.Platform["okex"].Data[coin+"-"+market] = currentPrice{
				symbol,
				coin,
				market,
				price,
				now,
				upPrice,
				upTime,
				pencent,
			}
			w.Platform["okex"].Unlock()
			return true // keep iterating
		})
		//错误计数归零
		w.setNotify("okex", 0)
		//保存现价数据为文件
		if len(w.OutDir) > 0 {
			w.save(string(body), "okex")
		}
	} else {
		log.Println("[okex] data nil")
		w.incrNotify("okex")
	}
}

// binance现价
func (w *Work) runWorkerBinance() {
	// 现价接口
	var err error

	client, err := w.getHttpClient()
	if err != nil {
		log.Println("[binance] ", err.Error())
		w.incrNotify("binance")
		return
	}

	var req *http.Request
	if len(w.NginxProxy) == 0 {
		req, err = http.NewRequest("GET", "https://www.binance.com/api/v3/ticker/price", nil)
	} else {
		req, err = http.NewRequest("GET", "http://"+w.NginxProxy+"/binance/api/v3/ticker/price", nil)
	}

	if err != nil {
		log.Println("[binance] ", err.Error())
		w.incrNotify("binance")
		return
	}
	req.Header.Add("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_12_6) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/67.0.3396.99 Safari/537.36")

	resp, err := client.Do(req)
	if err != nil {
		log.Println("[binance] ", err.Error())
		w.incrNotify("binance")
		return
	}
	defer resp.Body.Close()
	body, err := ioutil.ReadAll(resp.Body)

	// 数据处理
	if !gjson.Valid(string(body)) {
		log.Println("[binance] invalid json")
		w.incrNotify("binance")
		return
	}
	result := gjson.Parse(string(body))
	if result.Exists() {
		// 现价数据
		result.ForEach(func(key, value gjson.Result) bool {
			symbol := strings.ToLower(value.Get("symbol").String())
			coin := symbol[0 : len(symbol)-4]
			market := symbol[len(symbol)-4 : len(symbol)]
			now := time.Now().Format("20060102150405")
			if market != "usdt" {
				coin = symbol[0 : len(symbol)-3]
				market = symbol[len(symbol)-3 : len(symbol)]
			}
			price := value.Get("price").Float()
			if price <= 0 {
				//return true
			}

			var pencent float64 = 0
			var upPrice float64 = 0
			var upTime string
			if _, platformExist := w.Platform24["binance"]; platformExist {
				w.Platform24["binance"].Lock()
				if prevPrice, prevExist := w.Platform24["binance"].Data[coin+"-"+market]; prevExist {
					if prevPrice.Price != 0 {
						pencent = (price - prevPrice.Price) / prevPrice.Price
					}
					upPrice = prevPrice.Price
					upTime = prevPrice.Time
					w.Redis.Zadd("currentRank", []byte(coin+"|"+market+"|binance"), pencent)
				}
				w.Platform24["binance"].Unlock()
			}

			w.Platform["binance"].Lock()
			w.Platform["binance"].Data[coin+"-"+market] = currentPrice{
				symbol,
				coin,
				market,
				price,
				now,
				upPrice,
				upTime,
				pencent,
			}
			w.Platform["binance"].Unlock()
			return true // keep iterating
		})
		//错误计数归零
		w.setNotify("binance", 0)
		//保存现价数据为文件
		if len(w.OutDir) > 0 {
			w.save(string(body), "binance")
		}
	} else {
		log.Println("[binance] data nil")
		w.incrNotify("binance")
	}
}

// okex现价
func (w *Work) runWorkerGate() {
	// 现价接口
	var err error

	client, err := w.getHttpClient()
	if err != nil {
		log.Println("[gate] ", err.Error())
		w.incrNotify("gate")
		return
	}

	var req *http.Request
	if len(w.NginxProxy) == 0 {
		req, err = http.NewRequest("GET", "http://data.gate.io/api2/1/tickers", nil)
	} else {
		req, err = http.NewRequest("GET", "http://"+w.NginxProxy+"/gate/api2/1/tickers", nil)
	}

	if err != nil {
		log.Println("[gate] ", err.Error())
		w.incrNotify("gate")
		return
	}
	req.Header.Add("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_12_6) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/67.0.3396.99 Safari/537.36")

	resp, err := client.Do(req)
	if err != nil {
		log.Println("[gate] ", err.Error())
		w.incrNotify("gate")
		return
	}
	defer resp.Body.Close()
	body, err := ioutil.ReadAll(resp.Body)

	// 数据处理
	if !gjson.Valid(string(body)) {
		log.Println("[gate] invalid json")
		w.incrNotify("gate")
		return
	}
	result := gjson.Parse(string(body))
	if result.Exists() {
		//现价数据
		result.ForEach(func(key, value gjson.Result) bool {
			symbol := strings.ToLower(key.String())
			tmp := strings.Split(symbol, "_")
			if len(tmp) != 2 {
				log.Println("[gate] invalid symbol", symbol)
				w.incrNotify("gate")
				return true
			}
			coin := tmp[0]
			market := tmp[1]
			now := time.Now().Format("20060102150405")
			price := value.Get("last").Float()
			if price <= 0 {
				//return true
			}

			var pencent float64 = 0
			var upPrice float64 = 0
			var upTime string
			if _, platformExist := w.Platform24["gate"]; platformExist {
				w.Platform24["gate"].Lock()
				if prevPrice, prevExist := w.Platform24["gate"].Data[coin+"-"+market]; prevExist {
					if prevPrice.Price != 0 {
						pencent = (price - prevPrice.Price) / prevPrice.Price
					}
					upPrice = prevPrice.Price
					upTime = prevPrice.Time
					w.Redis.Zadd("currentRank", []byte(coin+"|"+market+"|gate"), pencent)
				}
				w.Platform24["gate"].Unlock()
			}

			w.Platform["gate"].Lock()
			w.Platform["gate"].Data[coin+"-"+market] = currentPrice{
				symbol,
				coin,
				market,
				price,
				now,
				upPrice,
				upTime,
				pencent,
			}
			w.Platform["gate"].Unlock()
			return true // keep iterating
		})
		//错误计数归零
		w.setNotify("gate", 0)
		//保存现价数据为文件
		if len(w.OutDir) > 0 {
			w.save(string(body), "gate")
		}
	} else {
		log.Println("[gate] data nil")
		w.incrNotify("gate")
	}
}

// zb现价
func (w *Work) runWorkerZb() {
	//现价接口
	var err error

	client, err := w.getHttpClient()
	if err != nil {
		log.Println("[zb] ", err.Error())
		w.incrNotify("zb")
		return
	}

	var req *http.Request
	/*
		if len(w.NginxProxy) == 0 {
			req, err = http.NewRequest("GET", "https://trans.zb.com/line/topall", nil)
		} else {
			req, err = http.NewRequest("GET", "http://"+w.NginxProxy+"/zb/line/topall", nil)
		}
	*/
	ips := []string{"119.28.21.201", "119.28.44.120", "119.28.135.26", "119.28.189.170", "119.28.54.197", "119.28.188.99", "119.28.140.253", "119.28.66.42", "119.28.41.146", "119.28.73.146", "119.28.73.143", "119.28.73.252", "119.28.63.239", "119.28.76.186", "119.28.67.93", "119.28.137.161", "119.28.12.198", "119.28.140.110", "119.28.55.145", "119.28.190.36", "119.28.191.249", "119.28.83.173", "119.28.181.183", "119.28.73.138", "119.28.133.171", "119.28.19.188", "119.28.65.128", "119.28.130.226"}
	req, err = http.NewRequest("GET", "http://"+ips[rand.Intn(len(ips)-1)]+":30026/line/topall", nil)

	if err != nil {
		log.Println("[zb] ", err.Error())
		w.incrNotify("zb")
		return
	}
	req.Header.Add("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_12_6) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/67.0.3396.99 Safari/537.36")

	resp, err := client.Do(req)
	if err != nil {
		log.Println("[zb] ", err.Error())
		w.incrNotify("zb")
		return
	}
	defer resp.Body.Close()
	body, err := ioutil.ReadAll(resp.Body)

	//数据处理
	if !gjson.Valid(strings.Trim(string(body), "()")) {
		log.Println("[zb] invalid json")
		w.incrNotify("zb")
		return
	}
	result := gjson.Get(strings.Trim(string(body), "()"), "datas")
	if result.Exists() {
		//现价数据
		result.ForEach(func(key, value gjson.Result) bool {
			symbol := strings.ToLower(value.Get("market").String())
			tmp := strings.Split(symbol, "/")
			if len(tmp) != 2 {
				log.Println("[zb] invalid symbol", symbol)
				w.incrNotify("zb")
				return true
			}
			coin := tmp[0]
			market := tmp[1]
			now := time.Now().Format("20060102150405")
			price := value.Get("lastPrice").Float()
			if price <= 0 {
				//return true
			}

			var pencent float64 = 0
			var upPrice float64 = 0
			var upTime string
			if _, platformExist := w.Platform24["zb"]; platformExist {
				w.Platform24["zb"].Lock()
				if prevPrice, prevExist := w.Platform24["zb"].Data[coin+"-"+market]; prevExist {
					if prevPrice.Price != 0 {
						pencent = (price - prevPrice.Price) / prevPrice.Price
					}
					upPrice = prevPrice.Price
					upTime = prevPrice.Time
					w.Redis.Zadd("currentRank", []byte(coin+"|"+market+"|zb"), pencent)
				}
				w.Platform24["zb"].Unlock()
			}

			w.Platform["zb"].Lock()
			w.Platform["zb"].Data[coin+"-"+market] = currentPrice{
				symbol,
				coin,
				market,
				price,
				now,
				upPrice,
				upTime,
				pencent,
			}
			w.Platform["zb"].Unlock()
			return true // keep iterating
		})
		//错误计数归零
		w.setNotify("zb", 0)
		//保存现价数据为文件
		if len(w.OutDir) > 0 {
			w.save(strings.Trim(string(body), "()"), "zb")
		}
	} else {
		log.Println("[zb] data nil")
		w.incrNotify("zb")
	}
}

// huilv
func (w *Work) runWorkerHuilv() {
	//现价接口
	var err error

	client, err := w.getHttpClient()
	if err != nil {
		log.Println("[huilv] ", err.Error())
		w.incrNotify("huilv")
		return
	}

	var req *http.Request
	req, err = http.NewRequest("GET", "http://currency.tratao.com/api/ver2/exchange/latest", nil)

	if err != nil {
		log.Println("[huilv] ", err.Error())
		w.incrNotify("huilv")
		return
	}
	req.Header.Add("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_12_6) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/67.0.3396.99 Safari/537.36")

	resp, err := client.Do(req)
	if err != nil {
		log.Println("[huilv] ", err.Error())
		w.incrNotify("huilv")
		return
	}
	defer resp.Body.Close()
	body, err := ioutil.ReadAll(resp.Body)

	//数据处理
	if !gjson.Valid(string(body)) {
		log.Println("[huilv] invalid json")
		w.incrNotify("huilv")
		return
	}
	result := gjson.Get(string(body), "list.0.resources")
	if result.Exists() {
		//现价数据
		result.ForEach(func(key, value gjson.Result) bool {
			symbol := strings.ToLower(value.Get("resource.fields.name").String())
			if !strings.Contains(symbol, "/") {
				return true
			}
			tmp := strings.Split(symbol, "/")
			if len(tmp) != 2 {
				log.Println("[huilv] invalid symbol", symbol)
				w.incrNotify("huilv")
				return true
			}
			coin := tmp[0]
			market := tmp[1]
			timestamp := value.Get("resource.fields.ts").Int()
			now := time.Unix(timestamp, 0).Format("20060102150405")
			//now := time.Now().Format("20060102150405")
			price := value.Get("resource.fields.price").Float()
			if price <= 0 {
				//return true
			}

			//正向汇率
			var pencent float64 = 0
			var upPrice float64 = 0
			var upTime string
			if _, platformExist := w.Platform24["huilv"]; platformExist {
				w.Platform24["huilv"].Lock()
				if prevPrice, prevExist := w.Platform24["huilv"].Data[coin+"-"+market]; prevExist {
					if prevPrice.Price != 0 {
						pencent = (price - prevPrice.Price) / prevPrice.Price
					}
					upPrice = prevPrice.Price
					upTime = prevPrice.Time
					w.Redis.Zadd("currentRank", []byte(coin+"|"+market+"|huilv"), pencent)
				}
				w.Platform24["huilv"].Unlock()
			}

			w.Platform["huilv"].Lock()
			w.Platform["huilv"].Data[coin+"-"+market] = currentPrice{
				symbol,
				coin,
				market,
				price,
				now,
				upPrice,
				upTime,
				pencent,
			}
			w.Platform["huilv"].Unlock()

			//反向汇率
			priceX := 1 / price
			var pencentX float64 = 0
			var upPriceX float64 = 0
			var upTimeX string
			if _, platformExistX := w.Platform24["huilv"]; platformExistX {
				w.Platform24["huilv"].Lock()
				if prevPriceX, prevExistX := w.Platform24["huilv"].Data[market+"-"+coin]; prevExistX {
					if prevPriceX.Price != 0 {
						pencentX = (priceX - prevPriceX.Price) / prevPriceX.Price
					}
					upPriceX = prevPriceX.Price
					upTimeX = prevPriceX.Time
					w.Redis.Zadd("currentRank", []byte(market+"|"+coin+"|huilv"), pencentX)
				}
				w.Platform24["huilv"].Unlock()
			}

			w.Platform["huilv"].Lock()
			w.Platform["huilv"].Data[market+"-"+coin] = currentPrice{
				market + "/" + coin,
				market,
				coin,
				priceX,
				now,
				upPriceX,
				upTimeX,
				pencentX,
			}
			w.Platform["huilv"].Unlock()
			return true // keep iterating
		})

		//错误计数归零
		w.setNotify("huilv", 0)
		//保存现价数据为文件
		if len(w.OutDir) > 0 {
			w.save(string(body), "huilv")
		}
	} else {
		log.Println("[huilv] data nil")
		w.incrNotify("huilv")
	}
}

//信息通知函数
func (w *Work) notify(text string, desp string) error {
	if len(w.Token) > 0 {
		url := fmt.Sprintf("https://sc.ftqq.com/%s.send?text=%s&desp=%s", url.QueryEscape(w.Token), url.QueryEscape("["+w.Info+"]"+text), url.QueryEscape(desp))
		resp, err := http.Get(url)
		defer resp.Body.Close()
		return err
	}
	log.Println("[notify] no token")
	return errors.New("no notify token")
}

//数据保存为文件函数
func (w *Work) save(text string, file string) {
	if len(w.OutDir) > 0 {
		var f *os.File
		var err1 error
		filename := w.OutDir + "/" + file + ".json"

		f, err1 = os.OpenFile(filename, os.O_WRONLY|os.O_TRUNC|os.O_CREATE, 0664)
		defer f.Close()

		_, err1 = io.WriteString(f, text+"\n")
		if err1 != nil {
			log.Println("[save] write fail", err1.Error())
			return
		}
	}
}
