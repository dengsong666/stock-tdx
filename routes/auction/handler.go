// Package auction 提供批量股票多日竞价金额 HTTP 服务。
package auction

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const (
	// Path 是批量股票多日竞价金额服务的固定地址。
	Path           = "/api/stock/auction-amounts"
	maxCodes       = 20
	maxDays        = 10
	maxRequestBody = 64 * 1024
)

// Fetcher 描述 Handler 需要的批量竞价金额查询能力。
type Fetcher interface {
	Fetch(codes []string, days int) (FetchResult, error)
}

type auctionRequest struct {
	Codes []string `json:"codes"`
	Days  int      `json:"days"`
}

type responseData struct {
	Auctions     map[string][]AuctionAmount `json:"auctions"`
	MissingCodes []string                   `json:"missing_codes"`
}

type apiResponse struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
	Data any    `json:"data"`
}

type handler struct {
	fetcher Fetcher
}

// Register 在 mux 上注册批量股票多日竞价金额服务。
func Register(mux *http.ServeMux, fetcher Fetcher) {
	if mux == nil {
		return
	}
	mux.Handle(Path, NewHandler(fetcher))
}

// NewHandler 创建可独立测试的竞价金额 HTTP Handler。
func NewHandler(fetcher Fetcher) http.Handler {
	return &handler{fetcher: fetcher}
}

func (route *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeError(w, http.StatusMethodNotAllowed, "请求方法不允许")
		return
	}
	if route.fetcher == nil {
		writeError(w, http.StatusServiceUnavailable, "TDX 竞价金额服务不可用")
		return
	}

	codes, days, err := decodeRequest(w, r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	result, err := route.fetcher.Fetch(codes, days)
	if err != nil {
		writeError(w, http.StatusBadGateway, fmt.Sprintf("TDX 竞价金额请求失败: %v", err))
		return
	}
	if result.Auctions == nil {
		result.Auctions = map[string][]AuctionAmount{}
	}
	if result.MissingCodes == nil {
		result.MissingCodes = []string{}
	}
	writeJSON(w, http.StatusOK, apiResponse{
		Code: 0,
		Msg:  "success",
		Data: responseData{Auctions: result.Auctions, MissingCodes: result.MissingCodes},
	})
}

// decodeRequest 解码、校验并按首次出现顺序去重股票代码。
func decodeRequest(w http.ResponseWriter, r *http.Request) ([]string, int, error) {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	var request auctionRequest
	if err := decoder.Decode(&request); err != nil {
		return nil, 0, fmt.Errorf("请求体格式错误: %w", err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return nil, 0, err
	}
	if len(request.Codes) == 0 {
		return nil, 0, fmt.Errorf("codes 不能为空")
	}
	if len(request.Codes) > maxCodes {
		return nil, 0, fmt.Errorf("单次最多查询 %d 个 code", maxCodes)
	}
	if request.Days < 1 || request.Days > maxDays {
		return nil, 0, fmt.Errorf("days 必须在 1 到 %d 之间", maxDays)
	}

	seen := make(map[string]struct{}, len(request.Codes))
	codes := make([]string, 0, len(request.Codes))
	for _, rawCode := range request.Codes {
		code := strings.TrimSpace(rawCode)
		if _, err := marketForCode(code); err != nil {
			return nil, 0, err
		}
		if _, ok := seen[code]; ok {
			continue
		}
		seen[code] = struct{}{}
		codes = append(codes, code)
	}
	return codes, request.Days, nil
}

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

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, apiResponse{Code: status, Msg: message, Data: nil})
}

func writeJSON(w http.ResponseWriter, status int, payload apiResponse) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
