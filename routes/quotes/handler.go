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
	FetchIndexes(indexes []IndexRequest) (FetchResult, error)
}

// IndexRequest 是指数行情的市场与代码；指数与股票存在同号代码，必须显式带市场。
type IndexRequest struct {
	Market string
	Code   string
}

type indexRequest struct {
	Market string `json:"market"`
	Code   string `json:"code"`
}

type quoteRequest struct {
	Codes   []string       `json:"codes"`
	Indexes []indexRequest `json:"indexes"`
}

type quoteData struct {
	Quotes         map[string]StockQuote `json:"quotes"`
	IndexQuotes    map[string]StockQuote `json:"index_quotes"`
	MissingCodes   []string              `json:"missing_codes"`
	MissingIndexes []string              `json:"missing_indexes"`
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

	codes, indexes, err := decodeRequest(w, r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	quotes := map[string]StockQuote{}
	missingCodes := []string{}
	if len(codes) > 0 {
		result, err := route.fetcher.Fetch(codes)
		if err != nil {
			writeError(w, http.StatusBadGateway, fmt.Sprintf("TDX 行情请求失败: %v", err))
			return
		}
		if result.Quotes != nil {
			quotes = result.Quotes
		}
		if result.MissingCodes != nil {
			missingCodes = result.MissingCodes
		}
	}

	indexQuotes := map[string]StockQuote{}
	missingIndexes := []string{}
	if len(indexes) > 0 {
		result, err := route.fetcher.FetchIndexes(indexes)
		if err != nil {
			writeError(w, http.StatusBadGateway, fmt.Sprintf("TDX 指数行情请求失败: %v", err))
			return
		}
		if result.Quotes != nil {
			indexQuotes = result.Quotes
		}
		if result.MissingCodes != nil {
			missingIndexes = result.MissingCodes
		}
	}

	writeJSON(w, http.StatusOK, apiResponse{
		Code: 0,
		Msg:  "success",
		Data: quoteData{
			Quotes:         quotes,
			IndexQuotes:    indexQuotes,
			MissingCodes:   missingCodes,
			MissingIndexes: missingIndexes,
		},
	})
}

// decodeRequest 解码、校验并按首次出现顺序去重股票代码与指数。
func decodeRequest(w http.ResponseWriter, r *http.Request) ([]string, []IndexRequest, error) {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	var request quoteRequest
	if err := decoder.Decode(&request); err != nil {
		return nil, nil, fmt.Errorf("请求体格式错误: %w", err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return nil, nil, err
	}
	if len(request.Codes) == 0 && len(request.Indexes) == 0 {
		return nil, nil, fmt.Errorf("codes 与 indexes 不能同时为空")
	}
	if len(request.Codes)+len(request.Indexes) > maxQuoteCodes {
		return nil, nil, fmt.Errorf("单次最多查询 %d 个代码或指数", maxQuoteCodes)
	}

	seen := make(map[string]struct{}, len(request.Codes))
	codes := make([]string, 0, len(request.Codes))
	for _, rawCode := range request.Codes {
		code := strings.TrimSpace(rawCode)
		if _, err := marketForCode(code); err != nil {
			return nil, nil, err
		}
		if _, ok := seen[code]; ok {
			continue
		}
		seen[code] = struct{}{}
		codes = append(codes, code)
	}

	seenIndexes := make(map[string]struct{}, len(request.Indexes))
	indexes := make([]IndexRequest, 0, len(request.Indexes))
	// 响应按裸代码回填，同号代码跨市场无法区分，只能拒绝。
	indexMarkets := make(map[string]string, len(request.Indexes))
	for _, raw := range request.Indexes {
		market := strings.ToLower(strings.TrimSpace(raw.Market))
		code := strings.TrimSpace(raw.Code)
		if err := validateIndexMarket(market); err != nil {
			return nil, nil, err
		}
		if len(code) != 6 || !isASCIIDigits(code) {
			return nil, nil, fmt.Errorf("无效指数代码: %s", code)
		}
		if existing, ok := indexMarkets[code]; ok && existing != market {
			return nil, nil, fmt.Errorf("指数代码 %s 不能同时出现在多个市场", code)
		}
		indexMarkets[code] = market
		key := market + code
		if _, ok := seenIndexes[key]; ok {
			continue
		}
		seenIndexes[key] = struct{}{}
		indexes = append(indexes, IndexRequest{Market: market, Code: code})
	}
	return codes, indexes, nil
}

// validateIndexMarket 校验指数市场只能是沪、深、北三个取值。
func validateIndexMarket(market string) error {
	switch market {
	case "sh", "sz", "bj":
		return nil
	default:
		return fmt.Errorf("无效指数市场: %s（仅支持 sh、sz、bj）", market)
	}
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
