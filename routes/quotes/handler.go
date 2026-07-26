package quotes

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const (
	// Path 是 TDX 批量股票行情服务的固定地址。
	Path           = "/api/stock/quotes"
	maxQuoteCodes  = 1000
	maxRequestBody = 128 * 1024
)

// Fetcher 描述 HTTP Handler 需要的批量行情能力。
type Fetcher interface {
	Fetch(codes []string) (FetchResult, error)
}

type quoteRequest struct {
	Codes []string `json:"codes"`
}

type quoteData struct {
	Quotes       map[string]StockQuote `json:"quotes"`
	MissingCodes []string              `json:"missing_codes"`
}

type apiResponse struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
	Data any    `json:"data"`
}

type handler struct {
	fetcher Fetcher
}

// Register 在 mux 上注册正式的 TDX 批量股票行情服务。
func Register(mux *http.ServeMux, fetcher Fetcher) {
	if mux == nil {
		return
	}
	mux.Handle(Path, NewHandler(fetcher))
}

// NewHandler 创建可独立测试的批量股票行情 HTTP Handler。
func NewHandler(fetcher Fetcher) http.Handler {
	return &handler{fetcher: fetcher}
}

// ServeHTTP 校验请求、调用行情服务并输出统一的 code、msg、data 响应。
func (route *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeError(w, http.StatusMethodNotAllowed, "请求方法不允许")
		return
	}
	if route.fetcher == nil {
		writeError(w, http.StatusServiceUnavailable, "TDX 行情服务不可用")
		return
	}

	codes, err := decodeCodes(w, r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	result, err := route.fetcher.Fetch(codes)
	if err != nil {
		writeError(w, http.StatusBadGateway, fmt.Sprintf("TDX 行情请求失败: %v", err))
		return
	}
	if result.Quotes == nil {
		result.Quotes = map[string]StockQuote{}
	}
	if result.MissingCodes == nil {
		result.MissingCodes = []string{}
	}
	writeJSON(w, http.StatusOK, apiResponse{
		Code: 0,
		Msg:  "success",
		Data: quoteData{Quotes: result.Quotes, MissingCodes: result.MissingCodes},
	})
}

// decodeCodes 解码、校验并按首次出现顺序去重股票代码。
func decodeCodes(w http.ResponseWriter, r *http.Request) ([]string, error) {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	var request quoteRequest
	if err := decoder.Decode(&request); err != nil {
		return nil, fmt.Errorf("请求体格式错误: %w", err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return nil, err
	}
	if len(request.Codes) == 0 {
		return nil, fmt.Errorf("codes 不能为空")
	}
	if len(request.Codes) > maxQuoteCodes {
		return nil, fmt.Errorf("单次最多查询 %d 个 code", maxQuoteCodes)
	}

	seen := make(map[string]struct{}, len(request.Codes))
	codes := make([]string, 0, len(request.Codes))
	for _, rawCode := range request.Codes {
		code := strings.TrimSpace(rawCode)
		if _, err := marketForCode(code); err != nil {
			return nil, err
		}
		if _, ok := seen[code]; ok {
			continue
		}
		seen[code] = struct{}{}
		codes = append(codes, code)
	}
	return codes, nil
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
