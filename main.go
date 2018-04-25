package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/gorilla/websocket"
	"github.com/tidwall/gjson"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

var usage = `Usage: %s [options] 
Options are:
    -h host     host for listen
    -p port     port for listen
    -t token    notify token for Server酱[http://sc.ftqq.com/3.version]
    -g gap      time gap for get data
    -x proxy    default no use proxy
    -f path     path to save data
`

var (
	host  string
	port  int
	token string
	gap   int
	proxy string
	path  string
)

func main() {
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, usage, os.Args[0])
	}

	flag.StringVar(&host, "h", "127.0.0.1", "")
	flag.IntVar(&port, "p", 9999, "")
	flag.StringVar(&token, "t", "", "")
	flag.IntVar(&gap, "g", 1, "")
	flag.StringVar(&proxy, "x", "", "")
	flag.StringVar(&path, "f", "", "")
	flag.Parse()

	w := Work{
		Host:  host,
		Port:  port,
		Token: token,
		Gap:   gap,
		Proxy: proxy,
		Path:  path,
	}
	w.Platform = make(map[string]map[string]currentPrice)
	w.NotifyCount = make(map[string]int)

	log.Println("listen:", w.Host, w.Port)
	log.Println("gap time:", w.Gap, "second")
	log.Println("proxy:", w.Proxy)
	w.runWorkers()
	w.RunHttp()
	w.notify("[currentPrice] 程序结束", "")
}

//返回结果处理
func retrunJson(msg string, status bool, data interface{}) []byte {
	b, err := json.Marshal(Result{status, msg, data})
	if err != nil {
		log.Println(err)
	}
	return b
}

type Result struct {
	Status bool        `json:"status"`
	Msg    string      `json:"msg"`
	Data   interface{} `json:"data"`
}

//工作线程
type Work struct {
	Host        string
	Port        int
	Token       string
	Platform    map[string]map[string]currentPrice
	NotifyCount map[string]int
	Gap         int
	Proxy       string
	Path        string
}
type currentPrice struct {
	Symbol string `json:"symbol"`
	Coin   string `json:"coin"`
	Market string `json:"market"`
	Price  string `json:"price"`
	Time   string `json:"time"`
}

//http线程
func (w *Work) RunHttp() {
	http.HandleFunc("/api/currentPrices/", w.CurrentPrices)
	listen := (w.Host + ":" + strconv.Itoa(w.Port))
	err := http.ListenAndServe(listen, nil)
	if err != nil {
		log.Println("ListenAndServe: ", err)
		return
	}
	log.Println("start ", w.Host, w.Port)
}
func (w *Work) CurrentPrices(resp http.ResponseWriter, req *http.Request) {
	req.ParseForm()

	if len(req.Form["markets[]"]) > 0 && len(req.Form["markets[]"][0]) > 0 {
		markets := req.Form["markets[]"]
		var resultData map[string]map[string]currentPrice = make(map[string]map[string]currentPrice)
		for marketIndex := range markets {
			tmp := strings.Split(strings.ToLower(markets[marketIndex]), "|")
			if len(tmp) != 3 {
				resp.Write(retrunJson("markets invail", false, nil))
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
			currentPrice, symbolExist := data[symbol]
			if symbolExist {
				resultData[site][symbol] = currentPrice
			}
		}
		resp.Write(retrunJson("ok", true, resultData))
		return
	} else if len(req.Form["site"]) > 0 && len(req.Form["site"][0]) > 0 {
		site := req.Form["site"][0]
		data, platformExist := w.Platform[site]
		if !platformExist {
			resp.Write(retrunJson("data invail", false, nil))
			return
		}

		var resultData map[string]map[string]currentPrice = make(map[string]map[string]currentPrice)
		if len(req.Form["market"]) > 0 && len(req.Form["market"][0]) > 0 {
			market := strings.ToLower(req.Form["market"][0])
			resultData[site] = make(map[string]currentPrice)
			for k, v := range data {
				if v.Market == market {
					resultData[site][k] = v
				}
			}
			resp.Write(retrunJson("ok", true, resultData))
			return
		} else {
			resultData[site] = data
			resp.Write(retrunJson("ok", true, resultData))
			return
		}

	}

	resp.Write(retrunJson("site invail", false, nil))
}

//工作线程
func (w *Work) runWorkers() {
	go func() {
		w.Platform["huobi"] = make(map[string]currentPrice)
		w.setNotify("huobi", 0)
		for {
			w.runWorkerHuobi()
			if w.getNotify("huobi") > 5 {
				w.notify("[huobi] currentPrice fail", "")
				w.setNotify("huobi", 0)
				time.Sleep(60 * time.Second)
			}
			//log.Println("[okex] huobi websocket reconnecting", w.getNotify("huobi"))
		}
		w.notify("[huobi] 协程结束", "")
	}()

	go func() {
		w.Platform["okex"] = make(map[string]currentPrice)
		w.setNotify("okex", 0)
		ticker := time.NewTicker(time.Duration(w.Gap) * time.Second)
		for range ticker.C {
			w.runWorkerOkex()
			if w.getNotify("okex") > 10 {
				w.notify("[okex] currentPrice fail", "")
				w.setNotify("okex", 0)
				time.Sleep(180 * time.Second)
			}
			//log.Println("[okex] http get", w.getNotify("okex"))
		}
		w.notify("[okex] 协程结束", "")
	}()

	go func() {
		w.Platform["binance"] = make(map[string]currentPrice)
		w.setNotify("binance", 0)
		ticker := time.NewTicker(time.Duration(w.Gap) * time.Second)
		for range ticker.C {
			w.runWorkerBinance()
			if w.getNotify("binance") > 10 {
				w.notify("[binance] currentPrice fail", "")
				w.setNotify("binance", 0)
				time.Sleep(180 * time.Second)
			}
			//log.Println("[binance] http get", w.getNotify("binance"))
		}
		w.notify("[binance] 协程结束", "")
	}()

	go func() {
		w.Platform["gate"] = make(map[string]currentPrice)
		w.setNotify("gate", 0)
		ticker := time.NewTicker(time.Duration(w.Gap) * time.Second)
		for range ticker.C {
			w.runWorkerGate()
			if w.getNotify("gate") > 10 {
				w.notify("[gate] currentPrice fail", "")
				w.setNotify("gate", 0)
				time.Sleep(180 * time.Second)
			}
			//log.Println("[gate] http get", w.getNotify("gate"))
		}
		w.notify("[gate] 协程结束", "")
	}()

	go func() {
		w.Platform["zb"] = make(map[string]currentPrice)
		w.setNotify("zb", 0)
		ticker := time.NewTicker(time.Duration(w.Gap) * time.Second)
		for range ticker.C {
			w.runWorkerZb()
			if w.getNotify("zb") > 10 {
				w.notify("[zb] currentPrice fail", "")
				w.setNotify("zb", 0)
				time.Sleep(180 * time.Second)
			}
			//log.Println("[zb] http get", w.getNotify("zb"))
		}
		w.notify("[zb] 协程结束", "")
	}()
}

func (w *Work) incrNotify(site string) {
	_, siteExist := w.NotifyCount[site]
	if siteExist {
		w.NotifyCount[site] = w.NotifyCount[site] + 1
		return
	}
	w.NotifyCount[site] = 0
}
func (w *Work) setNotify(site string, value int) {
	w.NotifyCount[site] = value
}
func (w *Work) getNotify(site string) int {
	_, siteExist := w.NotifyCount[site]
	if siteExist {
		return w.NotifyCount[site]
	}
	return 0
}

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

func GzipDecode(in []byte) ([]byte, error) {
	reader, err := gzip.NewReader(bytes.NewReader(in))
	if err != nil {
		var out []byte
		return out, err
	}
	defer reader.Close()

	return ioutil.ReadAll(reader)
}
func (w *Work) runWorkerHuobi() {
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

	//订阅
	err = ws.WriteMessage(websocket.TextMessage, []byte("{\"sub\":\"market.overview\"}"))
	if err != nil {
		log.Println("[huobi] ", err)
		w.incrNotify("huobi")
	}
	log.Println("[huobi] huobi websocket connected")

	//数据
	var i int = 0
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
			i = i + 30
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

		if strings.Contains(string(msg), "ping") {
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
					result.ForEach(func(key, value gjson.Result) bool {
						symbol := strings.ToLower(value.Get("symbol").String())
						coin := symbol[0 : len(symbol)-4]
						market := symbol[len(symbol)-4 : len(symbol)]
						now := time.Now().Format("20060102150405")
						if market != "usdt" {
							coin = symbol[0 : len(symbol)-3]
							market = symbol[len(symbol)-3 : len(symbol)]
						}
						w.Platform["huobi"][coin+"-"+market] = currentPrice{
							symbol,
							coin,
							market,
							value.Get("close").String(),
							now,
						}
						return true // keep iterating
					})
					w.setNotify("huobi", 0)
					i = 0
					w.save(string(retrunJson("ok", true, w.Platform["huobi"])), "huobi")
				} else {
					log.Println("[huobi] data nil")
					w.incrNotify("huobi")
					i = i + 5
				}
			}
		}

	}
}

func (w *Work) runWorkerOkex() {
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

	if !gjson.Valid(string(body)) {
		log.Println("[okex] invalid json")
		w.incrNotify("okex")
		return
	}
	result := gjson.GetBytes(body, "data")
	if result.Exists() {
		result.ForEach(func(key, value gjson.Result) bool {
			symbol := strings.ToLower(value.Get("symbol").String())
			tmp := strings.Split(symbol, "_")
			coin := tmp[0]
			market := tmp[1]
			now := time.Now().Format("20060102150405")
			w.Platform["okex"][coin+"-"+market] = currentPrice{
				symbol,
				coin,
				market,
				value.Get("close").String(),
				now,
			}
			return true // keep iterating
		})
		w.setNotify("okex", 0)
		w.save(string(body), "okex")
	} else {
		log.Println("[okex] data nil")
		w.incrNotify("okex")
	}
}

func (w *Work) runWorkerBinance() {
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

	if !gjson.Valid(string(body)) {
		log.Println("[binance] invalid json")
		w.incrNotify("binance")
		return
	}
	result := gjson.Parse(string(body))
	if result.Exists() {
		result.ForEach(func(key, value gjson.Result) bool {
			symbol := strings.ToLower(value.Get("symbol").String())
			coin := symbol[0 : len(symbol)-4]
			market := symbol[len(symbol)-4 : len(symbol)]
			now := time.Now().Format("20060102150405")
			if market != "usdt" {
				coin = symbol[0 : len(symbol)-3]
				market = symbol[len(symbol)-3 : len(symbol)]
			}
			w.Platform["binance"][coin+"-"+market] = currentPrice{
				symbol,
				coin,
				market,
				value.Get("price").String(),
				now,
			}
			return true // keep iterating
		})
		w.setNotify("binance", 0)
		w.save(string(body), "binance")
	} else {
		log.Println("[binance] data nil")
		w.incrNotify("binance")
	}
}

func (w *Work) runWorkerGate() {
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

	if !gjson.Valid(string(body)) {
		log.Println("[gate] invalid json")
		w.incrNotify("gate")
		return
	}
	result := gjson.Parse(string(body))
	if result.Exists() {
		result.ForEach(func(key, value gjson.Result) bool {
			symbol := strings.ToLower(key.String())
			tmp := strings.Split(symbol, "_")
			coin := tmp[0]
			market := tmp[1]
			now := time.Now().Format("20060102150405")
			w.Platform["gate"][coin+"-"+market] = currentPrice{
				symbol,
				coin,
				market,
				value.Get("last").String(),
				now,
			}
			return true // keep iterating
		})
		w.setNotify("gate", 0)
		w.save(string(body), "gate")
	} else {
		log.Println("[gate] data nil")
		w.incrNotify("gate")
	}
}

func (w *Work) runWorkerZb() {
	client := &http.Client{
		Timeout: time.Duration(5) * time.Second,
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

	if !gjson.Valid(strings.Trim(string(body), "()")) {
		log.Println("[zb] invalid json")
		w.incrNotify("zb")
		return
	}
	result := gjson.Get(strings.Trim(string(body), "()"), "datas")
	if result.Exists() {
		result.ForEach(func(key, value gjson.Result) bool {
			symbol := strings.ToLower(value.Get("market").String())
			tmp := strings.Split(symbol, "/")
			coin := tmp[0]
			market := tmp[1]
			now := time.Now().Format("20060102150405")
			w.Platform["zb"][coin+"-"+market] = currentPrice{
				symbol,
				coin,
				market,
				value.Get("lastPrice").String(),
				now,
			}
			return true // keep iterating
		})
		w.setNotify("zb", 0)
		w.save(strings.Trim(string(body), "()"), "zb")
	} else {
		log.Println("[zb] data nil")
		w.incrNotify("zb")
	}
}

func (w *Work) notify(text string, desp string) error {
	if len(w.Token) > 0 {
		url := fmt.Sprintf("https://sc.ftqq.com/%s.send?text=%s&desp=%s", url.QueryEscape(token), url.QueryEscape(text), url.QueryEscape(desp))
		resp, err := http.Get(url)
		defer resp.Body.Close()
		return err
	}
	log.Println("[notify] no token")
	return errors.New("no notify token")
}

func (w *Work) save(text string, file string) {
	if len(w.Path) > 0 {
		var f *os.File
		var err1 error
		filename := w.Path + "/" + file + ".json"

		f, err1 = os.OpenFile(filename, os.O_WRONLY|os.O_TRUNC|os.O_CREATE, 0664)
		defer f.Close()

		_, err1 = io.WriteString(f, text+"\n")
		if err1 != nil {
			log.Println("[save] write fail", err1.Error())
			return
		}
	}
}
