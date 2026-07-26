package kline

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	// Path 是单股票时间区间 K 线服务的固定地址。
	Path           = "/api/stock/kline"
	maxRequestBody = 64 * 1024
)

// Fetcher 描述 HTTP Handler 需要的单股票 K 线查询能力。
type Fetcher interface {
	Fetch(query Query) ([]Bar, error)
}

type klineRequest struct {
	Code   string `json:"code"`
	Period string `json:"period"`
	Start  string `json:"start"`
	End    string `json:"end"`
	Adjust string `json:"adjust"`
}

type klineData struct {
	Code   string `json:"code"`
	Period string `json:"period"`
	Start  string `json:"start"`
	End    string `json:"end"`
	Adjust string `json:"adjust"`
	KLines []Bar  `json:"klines"`
}

type apiResponse struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
	Data any    `json:"data"`
}

type handler struct {
	fetcher Fetcher
	now     func() time.Time
}

// Register 在 mux 上注册单股票时间区间 K 线服务。
func Register(mux *http.ServeMux, fetcher Fetcher) {
	if mux == nil {
		return
	}
	mux.Handle(Path, NewHandler(fetcher))
}

// NewHandler 创建可独立测试的 K 线 HTTP Handler。
func NewHandler(fetcher Fetcher) http.Handler {
	return newHandler(fetcher, time.Now)
}

// newHandler 创建可注入当前时间的 Handler，避免离线测试依赖系统时钟。
func newHandler(fetcher Fetcher, now func() time.Time) http.Handler {
	return &handler{fetcher: fetcher, now: now}
}

// ServeHTTP 校验请求、查询 K 线并输出统一的 code、msg、data 响应。
func (route *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeError(w, http.StatusMethodNotAllowed, "请求方法不允许")
		return
	}
	if route.fetcher == nil {
		writeError(w, http.StatusServiceUnavailable, "TDX K线服务不可用")
		return
	}

	query, err := route.decodeQuery(w, r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	bars, err := route.fetcher.Fetch(query)
	if err != nil {
		if errors.Is(err, ErrTooManyBars) {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		writeError(w, http.StatusBadGateway, fmt.Sprintf("TDX K线请求失败: %v", err))
		return
	}
	if bars == nil {
		bars = []Bar{}
	}
	writeJSON(w, http.StatusOK, apiResponse{
		Code: 0,
		Msg:  "success",
		Data: klineData{
			Code:   query.Code,
			Period: query.Period.Name,
			Start:  query.Start.Format(dateTimeLayout),
			End:    query.End.Format(dateTimeLayout),
			Adjust: query.AdjustName,
			KLines: bars,
		},
	})
}

// decodeQuery 解码并规范化股票代码、周期、时间区间和复权方式。
func (route *handler) decodeQuery(w http.ResponseWriter, r *http.Request) (Query, error) {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	var request klineRequest
	if err := decoder.Decode(&request); err != nil {
		return Query{}, fmt.Errorf("请求体格式错误: %w", err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return Query{}, err
	}

	code := strings.TrimSpace(request.Code)
	market, err := marketForCode(code)
	if err != nil {
		return Query{}, err
	}
	period, err := parsePeriod(request.Period)
	if err != nil {
		return Query{}, err
	}
	if strings.TrimSpace(request.Start) == "" {
		return Query{}, fmt.Errorf("start 必填")
	}
	start, err := parseRangeTime(request.Start, false)
	if err != nil {
		return Query{}, fmt.Errorf("start %w", err)
	}

	end := route.now().In(shanghaiLocation)
	if strings.TrimSpace(request.End) != "" {
		end, err = parseRangeTime(request.End, true)
		if err != nil {
			return Query{}, fmt.Errorf("end %w", err)
		}
	}
	if start.After(end) {
		return Query{}, fmt.Errorf("start 不能晚于 end")
	}
	adjustName, adjust, err := parseAdjust(request.Adjust)
	if err != nil {
		return Query{}, err
	}
	return Query{
		Code:       code,
		Market:     market,
		Period:     period,
		Start:      start,
		End:        end,
		Adjust:     adjust,
		AdjustName: adjustName,
	}, nil
}

// ensureJSONEOF 拒绝在合法 JSON 对象后继续追加其他内容。
func ensureJSONEOF(decoder *json.Decoder) error {
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return fmt.Errorf("请求体只能包含一个 JSON 对象")
		}
		return fmt.Errorf("请求体格式错误: %w", err)
	}
	return nil
}

// writeError 输出 data 为 null 的统一错误响应。
func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, apiResponse{Code: status, Msg: message, Data: nil})
}

// writeJSON 输出统一 JSON 响应。
func writeJSON(w http.ResponseWriter, status int, payload apiResponse) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
