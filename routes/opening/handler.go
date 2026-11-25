// Package opening 提供按日期批量查询股票开盘成交（09:25 竞价成交与 09:30 后第一笔）的 HTTP 服务。
package opening

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	// Path 是批量股票开盘成交服务的固定地址。
	Path = "/api/stock/opening-trades"
	// maxCodes 限制单次代码数：每只要翻约 5 页整天分笔，20 只约 100 次往返。
	maxCodes       = 20
	maxRequestBody = 64 * 1024
)

// Fetcher 描述 Handler 需要的批量开盘成交查询能力。
type Fetcher interface {
	Fetch(date uint32, codes []string) (FetchResult, error)
}

type openingRequest struct {
	Date  string   `json:"date"`
	Codes []string `json:"codes"`
}

type responseData struct {
	Trades       map[string]OpeningTrade `json:"trades"`
	MissingCodes []string                `json:"missing_codes"`
}

type apiResponse struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
	Data any    `json:"data"`
}

type handler struct {
	fetcher Fetcher
}

// Register 在 mux 上注册批量股票开盘成交服务。
func Register(mux *http.ServeMux, fetcher Fetcher) {
	if mux == nil {
		return
	}
	mux.Handle(Path, NewHandler(fetcher))
}

// NewHandler 创建可独立测试的开盘成交 HTTP Handler。
func NewHandler(fetcher Fetcher) http.Handler {
	return &handler{fetcher: fetcher}
}

// ServeHTTP 只接受 POST：校验请求后整批查询，上游失败返回 502。
func (route *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeError(w, http.StatusMethodNotAllowed, "请求方法不允许")
		return
	}
	if route.fetcher == nil {
		writeError(w, http.StatusServiceUnavailable, "TDX 开盘成交服务不可用")
		return
	}

	date, codes, err := decodeRequest(w, r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	result, err := route.fetcher.Fetch(date, codes)
	if err != nil {
		writeError(w, http.StatusBadGateway, fmt.Sprintf("TDX 开盘成交请求失败: %v", err))
		return
	}
	if result.Trades == nil {
		result.Trades = map[string]OpeningTrade{}
	}
	if result.MissingCodes == nil {
		result.MissingCodes = []string{}
	}
	writeJSON(w, http.StatusOK, apiResponse{
		Code: 0,
		Msg:  "success",
		Data: responseData{Trades: result.Trades, MissingCodes: result.MissingCodes},
	})
}

// decodeRequest 解码并校验日期与代码，代码按首次出现顺序去重。
func decodeRequest(w http.ResponseWriter, r *http.Request) (uint32, []string, error) {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	var request openingRequest
	if err := decoder.Decode(&request); err != nil {
		return 0, nil, fmt.Errorf("请求体格式错误: %w", err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return 0, nil, err
	}
	date, err := parseDate(request.Date)
	if err != nil {
		return 0, nil, err
	}
	if len(request.Codes) == 0 {
		return 0, nil, fmt.Errorf("codes 不能为空")
	}
	if len(request.Codes) > maxCodes {
		return 0, nil, fmt.Errorf("单次最多查询 %d 个 code", maxCodes)
	}

	seen := make(map[string]struct{}, len(request.Codes))
	codes := make([]string, 0, len(request.Codes))
	for _, rawCode := range request.Codes {
		code := strings.TrimSpace(rawCode)
		if _, err := marketForCode(code); err != nil {
			return 0, nil, err
		}
		if _, ok := seen[code]; ok {
			continue
		}
		seen[code] = struct{}{}
		codes = append(codes, code)
	}
	return date, codes, nil
}

// parseDate 校验 YYYYMMDD 是真实存在的日期，返回协议要的整数形式。
func parseDate(value string) (uint32, error) {
	text := strings.TrimSpace(value)
	if _, err := time.Parse("20060102", text); err != nil || len(text) != 8 {
		return 0, fmt.Errorf("date 必须是 YYYYMMDD 格式的日期")
	}
	number, err := strconv.ParseUint(text, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("date 必须是 YYYYMMDD 格式的日期")
	}
	return uint32(number), nil
}

// ensureJSONEOF 拒绝请求体里多出来的第二个 JSON 值。
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

// writeError 按统一响应格式写错误。
func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, apiResponse{Code: status, Msg: message, Data: nil})
}

// writeJSON 写 JSON 响应。
func writeJSON(w http.ResponseWriter, status int, payload apiResponse) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
