package boards

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const (
	// Path 是板块列表服务的固定地址。
	Path = "/api/market/boards"
	// MembersPath 是板块成分股服务的固定地址。
	MembersPath    = "/api/market/boards/members"
	maxRequestBody = 64 * 1024
)

// Board 是一个板块条目；Type 为通达信原始板块类型码，供调用方映射分类，不要求落库。
type Board struct {
	Code string `json:"code"`
	Name string `json:"name"`
	Type uint16 `json:"type"`
}

// Stock 是一只板块成分股。
type Stock struct {
	Code   string `json:"code"`
	Name   string `json:"name"`
	Market uint16 `json:"market"`
}

// BoardMembers 是一个板块的成分股结果。
type BoardMembers struct {
	Code   string  `json:"code"`
	Stocks []Stock `json:"stocks"`
}

// Fetcher 描述 HTTP Handler 需要的板块能力。
type Fetcher interface {
	FetchBoards() ([]Board, error)
	FetchMembers(symbol string) (BoardMembers, error)
}

type apiResponse struct {
	Code int    `json:"code"`
	Msg  string `json:"msg"`
	Data any    `json:"data"`
}

type membersRequest struct {
	Symbol string `json:"symbol"`
}

type handler struct {
	fetcher Fetcher
}

// Register 在 mux 上注册板块列表与成分股服务。
func Register(mux *http.ServeMux, fetcher Fetcher) {
	if mux == nil {
		return
	}
	route := NewHandler(fetcher)
	mux.Handle(Path, route)
	mux.Handle(MembersPath, route)
}

// NewHandler 创建可独立测试的板块 HTTP Handler。
func NewHandler(fetcher Fetcher) http.Handler {
	return &handler{fetcher: fetcher}
}

// ServeHTTP 按路径分发板块列表与成分股查询。
func (route *handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case Path:
		route.serveBoards(w, r)
	case MembersPath:
		route.serveMembers(w, r)
	default:
		http.NotFound(w, r)
	}
}

func (route *handler) serveBoards(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeError(w, http.StatusMethodNotAllowed, "请求方法不允许")
		return
	}
	if route.fetcher == nil {
		writeError(w, http.StatusServiceUnavailable, "TDX 板块服务不可用")
		return
	}
	items, err := route.fetcher.FetchBoards()
	if err != nil {
		writeError(w, http.StatusBadGateway, fmt.Sprintf("TDX 板块列表请求失败: %v", err))
		return
	}
	if items == nil {
		items = []Board{}
	}
	writeJSON(w, http.StatusOK, apiResponse{Code: 0, Msg: "success", Data: map[string]any{"boards": items}})
}

func (route *handler) serveMembers(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeError(w, http.StatusMethodNotAllowed, "请求方法不允许")
		return
	}
	if route.fetcher == nil {
		writeError(w, http.StatusServiceUnavailable, "TDX 板块服务不可用")
		return
	}
	symbol, err := decodeMembersRequest(w, r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	members, err := route.fetcher.FetchMembers(symbol)
	if err != nil {
		writeError(w, http.StatusBadGateway, fmt.Sprintf("TDX 板块成分股请求失败: %v", err))
		return
	}
	if members.Stocks == nil {
		members.Stocks = []Stock{}
	}
	writeJSON(w, http.StatusOK, apiResponse{Code: 0, Msg: "success", Data: members})
}

// decodeMembersRequest 解析并校验板块代码；板块代码是 4–10 位数字，避免把股票代码误传进来。
func decodeMembersRequest(w http.ResponseWriter, r *http.Request) (string, error) {
	r.Body = http.MaxBytesReader(w, r.Body, maxRequestBody)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	var request membersRequest
	if err := decoder.Decode(&request); err != nil {
		return "", fmt.Errorf("请求体格式错误: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return "", fmt.Errorf("请求体只能包含一个 JSON 对象")
	}
	symbol := strings.TrimSpace(request.Symbol)
	if symbol == "" {
		return "", fmt.Errorf("symbol 不能为空")
	}
	if len(symbol) < 4 || len(symbol) > 10 || !isDigits(symbol) {
		return "", fmt.Errorf("无效板块代码: %s", symbol)
	}
	return symbol, nil
}

func isDigits(value string) bool {
	for _, ch := range value {
		if ch < '0' || ch > '9' {
			return false
		}
	}
	return true
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, apiResponse{Code: status, Msg: message, Data: nil})
}

func writeJSON(w http.ResponseWriter, status int, payload apiResponse) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}
