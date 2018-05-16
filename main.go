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
	"github.com/gorilla/websocket"
	"github.com/larspensjo/config"
	"github.com/tidwall/gjson"
	"io"
	"io/ioutil"
	"log"
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
    -h host     host for listen
    -p port     port for listen
    -t token    notify token for Server酱[http://sc.ftqq.com/3.version]
    -g gap      time gap Millisecond for get data and save file
    -s saveGap  time gap second for save history data with redis
    -x proxy    default no use proxy
    -o outDir   dir to save origin data
    -f configFile     General configuration file
`
var (
	host       string
	port       int
	token      string
	gap        int
	savegap    int
	proxy      string
	outDir     string
	configFile string
	info       string
)

func main() {
	runtime.GOMAXPROCS(runtime.NumCPU())

	//解析命令行参数
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, usage, os.Args[0])
	}
	flag.StringVar(&host, "h", "127.0.0.1", "")
	flag.IntVar(&port, "p", 9999, "")
	flag.StringVar(&token, "t", "", "")
	flag.IntVar(&gap, "g", 1, "")
	flag.IntVar(&savegap, "s", 60, "")
	flag.StringVar(&proxy, "x", "", "")
	flag.StringVar(&outDir, "o", "", "")
	flag.StringVar(&configFile, "f", "", "")
	flag.StringVar(&info, "i", "ok", "")
	flag.Parse()
	//解析配置文件参数
	var redisConfig map[string]string = make(map[string]string)
	if len(configFile) > 0 {
		cfg, err := config.ReadDefault(configFile)
		if err != nil {
			log.Fatalf("Fail to find", configFile, err)
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
			if data, err := cfg.String("app", "outdir"); err == nil {
				outDir = data
			}
			if data, err := cfg.String("app", "info"); err == nil {
				info = data
			}
		}
	}

	//工作对象
	w := Work{
		Host:        host,
		Port:        port,
		Token:       token,
		Gap:         gap,
		SaveGap:     savegap,
		Proxy:       proxy,
		OutDir:      outDir,
		RedisConfig: redisConfig,
		Info:        info,
	}
	w.Platform = make(map[string]*currentPrices)
	w.Platform24 = make(map[string]map[string]currentPrice)
	w.NotifyCount.Num = make(map[string]int)

	log.Println("[app] listen:", w.Host, w.Port)
	log.Println("[app] gap time:", w.Gap, "Millisecond")
	log.Println("[app] savegap time:", w.SaveGap, "Second")
	log.Println("[app] proxy:", w.Proxy)
	log.Println("[app] outDir:", w.OutDir)
	log.Println("[app] info:", w.Info)
	w.initRedis()
	w.runWorkers()
	w.RunHttp()

	w.notify("[currentPrice] 程序结束", "")
}

//redis初始化
func (w *Work) initRedis() {
	w.Redis.Addr = w.RedisConfig["host"] + ":" + w.RedisConfig["port"]
	redisdb, err := strconv.Atoi(w.RedisConfig["db"])
	if err != nil {
		log.Fatalln("[redis] config select db error")
	}
	w.Redis.Db = redisdb
	w.Redis.Password = w.RedisConfig["auth"]

	_, err = w.Redis.Ping()
	if err != nil {
		log.Fatalln("[redis] ping ", err)
	}
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
	Host        string
	Port        int
	Token       string
	Platform    map[string]*currentPrices
	Platform24  map[string]map[string]currentPrice
	NotifyCount Count
	Gap         int
	SaveGap     int
	Proxy       string
	OutDir      string
	RedisConfig map[string]string
	Redis       goredis.Client
	Info        string
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
		w.Platform["huobi"] = new(currentPrices)
		w.Platform["huobi"].Data = make(map[string]currentPrice)
		w.setNotify("huobi", 0)
		for {
			w.runWorkerHuobi()
			// 超过5次错误后休息一分钟
			if w.getNotify("huobi") > 5 {
				w.notify("[huobi] currentPrice fail", "读取现价接口超过五次错误休息一分钟")
				w.setNotify("huobi", 0)
				time.Sleep(60 * time.Second)
			}
			//log.Println("[okex] huobi websocket reconnecting", w.getNotify("huobi"))
		}
		w.notify("[huobi] 协程结束", "")
	}()

	// okex http
	go func() {
		w.Platform["okex"] = new(currentPrices)
		w.Platform["okex"].Data = make(map[string]currentPrice)
		w.setNotify("okex", 0)
		ticker := time.NewTicker(time.Duration(w.Gap) * time.Millisecond)
		for range ticker.C {
			//for {
			w.runWorkerOkex()
			// 超过10次错误后休息两分钟
			if w.getNotify("okex") > 10 {
				w.notify("[okex] currentPrice fail", "读取现价接口超过十次错误休息两分钟")
				w.setNotify("okex", 0)
				time.Sleep(120 * time.Second)
			}
			//log.Println("[okex] http get", w.getNotify("okex"))
		}
		w.notify("[okex] 协程结束", "")
	}()

	// binance http
	go func() {
		w.Platform["binance"] = new(currentPrices)
		w.Platform["binance"].Data = make(map[string]currentPrice)
		w.setNotify("binance", 0)
		ticker := time.NewTicker(time.Duration(w.Gap) * time.Millisecond)
		for range ticker.C {
			//for {
			w.runWorkerBinance()
			// 超过10次错误后休息两分钟
			if w.getNotify("binance") > 10 {
				w.notify("[binance] currentPrice fail", "读取现价接口超过十次错误休息两分钟")
				w.setNotify("binance", 0)
				time.Sleep(120 * time.Second)
			}
			//log.Println("[binance] http get", w.getNotify("binance"))
		}
		w.notify("[binance] 协程结束", "")
	}()

	// gate http
	go func() {
		w.Platform["gate"] = new(currentPrices)
		w.Platform["gate"].Data = make(map[string]currentPrice)
		w.setNotify("gate", 0)
		ticker := time.NewTicker(time.Duration(w.Gap) * time.Millisecond)
		for range ticker.C {
			//for {
			w.runWorkerGate()
			// 超过10次错误后休息两分钟
			if w.getNotify("gate") > 10 {
				w.notify("[gate] currentPrice fail", "读取现价接口超过十次错误休息两分钟")
				w.setNotify("gate", 0)
				time.Sleep(120 * time.Second)
			}
			//log.Println("[gate] http get", w.getNotify("gate"))
		}
		w.notify("[gate] 协程结束", "")
	}()

	// zb http
	go func() {
		w.Platform["zb"] = new(currentPrices)
		w.Platform["zb"].Data = make(map[string]currentPrice)
		w.setNotify("zb", 0)
		ticker := time.NewTicker(time.Duration(w.Gap) * time.Millisecond * 2)
		for range ticker.C {
			//for {
			w.runWorkerZb()
			// 超过10次错误后休息两分钟
			if w.getNotify("zb") > 10 {
				w.notify("[zb] currentPrice fail", "读取现价接口超过十次错误休息两分钟")
				w.setNotify("zb", 0)
				time.Sleep(120 * time.Second)
			}
			//log.Println("[zb] http get", w.getNotify("zb"))
		}
		w.notify("[zb] 协程结束", "")
	}()

	// 存储历史现价
	go func() {
		var storeKey string = "currentZset"
		// 获取24小时历史现价，供涨跌幅计算
		w.save24History(storeKey)

		// 定时处理
		ticker := time.NewTicker(time.Duration(w.SaveGap) * time.Second) //存储时间间隔由配置决定
		for range ticker.C {
			// 存储现价成历史数据
			w.saveHistory(storeKey)
			// 获取24小时历史现价，供涨跌幅计算
			w.save24History(storeKey)

			//log.Println("[platform24] ", now.Format("2006-01-02 15:04:05"))
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
		dec := gob.NewDecoder(bytes.NewBuffer(data1[0]))
		err = dec.Decode(&w.Platform24)
		if err != nil {
			log.Println("[save24History] decode data1:", err)
		}
		w.Redis.Zremrangebyscore(storeKey, float64(now.Unix()-87000), float64(now.Unix()-864000))
	} else {
		data2, err := w.Redis.Zrangebyscore(storeKey, float64(now.Unix()-86400), float64(now.Unix()), 0, 1)
		if err == nil && len(data2) == 2 {
			dec := gob.NewDecoder(bytes.NewBuffer(data2[0]))
			err = dec.Decode(&w.Platform24)
			if err != nil {
				log.Println("[save24History] decode data2:", err)
			}
		} else {
			for k, v := range w.Platform {
				v.Lock()
				w.Platform24[k] = v.Data
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
	if len(w.Proxy) == 0 {
		u = url.URL{Scheme: "ws", Host: "api.huobi.pro", Path: "/ws"}
	} else {
		u = url.URL{Scheme: "ws", Host: w.Proxy, Path: "/huobi/ws"}
	}

	ws, _, err := websocket.DefaultDialer.Dial(u.String(), nil)
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
	}
	log.Println("[huobi] huobi websocket connected")

	//数据
	var i int = 0 //用于计算websocket出错次数
	for {
		//多次出错后重连
		if i > 100 {
			log.Println("[huobi] too many err:", i)
			break
		}

		//阻塞读取数据
		_, originMsg, err := ws.ReadMessage()
		if err != nil {
			log.Println("[huobi] ws read err:", err)
			w.incrNotify("huobi")
			i = i + 40
			continue
		}
		//解压数据
		msg, err := GzipDecode(originMsg)
		if err != nil {
			log.Println("[huobi] gzip decode err:", err)
			w.incrNotify("huobi")
			i = i + 10
			continue
		}

		if strings.Contains(string(msg), "ping") { //心跳
			if err := ws.WriteMessage(websocket.TextMessage, []byte(strings.Replace(string(msg), "ping", "pong", 1))); err != nil {
				log.Println("[huobi] ws pong err:", err)
				w.incrNotify("huobi")
				i = i + 10
				continue
			}
		} else {
			if tmp := gjson.GetBytes(msg, "ch"); tmp.String() == "market.overview" {
				if !gjson.Valid(string(msg)) {
					log.Println("[huobi] invalid json")
					w.incrNotify("huobi")
					i = i + 5
					continue
				}
				result := gjson.GetBytes(msg, "data")
				if result.Exists() {
					//现价数据
					result.ForEach(func(key, value gjson.Result) bool {
						symbol := strings.ToLower(value.Get("symbol").String())
						coin := symbol[0 : len(symbol)-4]
						market := symbol[len(symbol)-4 : len(symbol)]
						now := time.Now().Format("20060102150405")
						if market != "usdt" {
							coin = symbol[0 : len(symbol)-3]
							market = symbol[len(symbol)-3 : len(symbol)]
						}
						price := value.Get("close").Float()

						var pencent float64 = 0
						var upPrice float64 = 0
						var upTime string
						if prevPrice, prevExist := w.Platform24["huobi"][coin+"-"+market]; prevExist {
							if prevPrice.Price != 0 {
								pencent = (price - prevPrice.Price) / prevPrice.Price
							}
							upPrice = prevPrice.Price
							upTime = prevPrice.Time
							w.Redis.Zadd("currentRank", []byte(coin+"|"+market+"|huobi"), pencent)
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
			}
		}

	}
}

// okex现价
func (w *Work) runWorkerOkex() {
	// 现价接口
	/*
		urli := url.URL{}
		urlproxy, _ := urli.Parse("https://127.0.0.1:1088")
		client := &http.Client{
			Transport: &http.Transport{
				Proxy: http.ProxyURL(urlproxy),
			},
		}
	*/

	client := &http.Client{
		Timeout: time.Duration(8) * time.Second,
	}

	var req *http.Request
	var err error
	if len(w.Proxy) == 0 {
		req, err = http.NewRequest("GET", "http://www.okex.com/v2/markets/tickers", nil)
	} else {
		req, err = http.NewRequest("GET", "http://"+w.Proxy+"/okex/v2/markets/tickers", nil)
	}
	//req.Header.Add("auth", "good")

	if err != nil {
		log.Println("[okex] ", err.Error())
		w.incrNotify("okex")
		return
	}

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

			var pencent float64 = 0
			var upPrice float64 = 0
			var upTime string
			if prevPrice, prevExist := w.Platform24["okex"][coin+"-"+market]; prevExist {
				if prevPrice.Price != 0 {
					pencent = (price - prevPrice.Price) / prevPrice.Price
				}
				upPrice = prevPrice.Price
				upTime = prevPrice.Time
				w.Redis.Zadd("currentRank", []byte(coin+"|"+market+"|okex"), pencent)
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
	client := &http.Client{
		Timeout: time.Duration(5) * time.Second,
	}

	var req *http.Request
	var err error
	if len(w.Proxy) == 0 {
		req, err = http.NewRequest("GET", "http://www.binance.com/api/v3/ticker/price", nil)
	} else {
		req, err = http.NewRequest("GET", "http://"+w.Proxy+"/binance/api/v3/ticker/price", nil)
	}

	if err != nil {
		log.Println("[binance] ", err.Error())
		w.incrNotify("binance")
		return
	}

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

			var pencent float64 = 0
			var upPrice float64 = 0
			var upTime string
			if prevPrice, prevExist := w.Platform24["binance"][coin+"-"+market]; prevExist {
				if prevPrice.Price != 0 {
					pencent = (price - prevPrice.Price) / prevPrice.Price
				}
				upPrice = prevPrice.Price
				upTime = prevPrice.Time
				w.Redis.Zadd("currentRank", []byte(coin+"|"+market+"|binance"), pencent)
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
	client := &http.Client{
		Timeout: time.Duration(5) * time.Second,
	}

	var req *http.Request
	var err error
	if len(w.Proxy) == 0 {
		req, err = http.NewRequest("GET", "http://data.gate.io/api2/1/tickers", nil)
	} else {
		req, err = http.NewRequest("GET", "http://"+w.Proxy+"/gate/api2/1/tickers", nil)
	}

	if err != nil {
		log.Println("[gate] ", err.Error())
		w.incrNotify("gate")
		return
	}

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

			var pencent float64 = 0
			var upPrice float64 = 0
			var upTime string
			if prevPrice, prevExist := w.Platform24["gate"][coin+"-"+market]; prevExist {
				if prevPrice.Price != 0 {
					pencent = (price - prevPrice.Price) / prevPrice.Price
				}
				upPrice = prevPrice.Price
				upTime = prevPrice.Time
				w.Redis.Zadd("currentRank", []byte(coin+"|"+market+"|gate"), pencent)
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
	client := &http.Client{
		Timeout: time.Duration(8) * time.Second,
	}

	var req *http.Request
	var err error
	if len(w.Proxy) == 0 {
		req, err = http.NewRequest("GET", "https://trans.zb.com/line/topall", nil)
	} else {
		req, err = http.NewRequest("GET", "http://"+w.Proxy+"/zb/line/topall", nil)
	}

	if err != nil {
		log.Println("[zb] ", err.Error())
		w.incrNotify("zb")
		return
	}

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

			var pencent float64 = 0
			var upPrice float64 = 0
			var upTime string
			if prevPrice, prevExist := w.Platform24["zb"][coin+"-"+market]; prevExist {
				if prevPrice.Price != 0 {
					pencent = (price - prevPrice.Price) / prevPrice.Price
				}
				upPrice = prevPrice.Price
				upTime = prevPrice.Time
				w.Redis.Zadd("currentRank", []byte(coin+"|"+market+"|zb"), pencent)
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

//信息通知函数
func (w *Work) notify(text string, desp string) error {
	if len(w.Token) > 0 {
		url := fmt.Sprintf("https://sc.ftqq.com/%s.send?text=%s&desp=%s", url.QueryEscape(w.Token), url.QueryEscape(text), url.QueryEscape(desp))
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
