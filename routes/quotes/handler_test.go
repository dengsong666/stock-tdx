package quotes

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type fakeFetcher struct {
	result FetchResult
	err    error
	codes  []string
}

// Fetch 记录 Handler 传入的代码并返回预设结果。
func (fetcher *fakeFetcher) Fetch(codes []string) (FetchResult, error) {
	fetcher.codes = append([]string(nil), codes...)
	return fetcher.result, fetcher.err
}

// TestQuoteHandlerSuccess 验证成功响应、代码去重和统一响应结构。
func TestQuoteHandlerSuccess(t *testing.T) {
	fetcher := &fakeFetcher{result: FetchResult{
		Quotes: map[string]StockQuote{
			"600519": {
				Code:          "600519",
				Price:         1297.41,
				Change:        0.42,
				PreviousClose: 1292.01,
				LimitUpPrice:  1421.21,
			},
		},
		MissingCodes: []string{"300750"},
	}}
	body := []byte(`{"codes":[" 600519 ","600519","300750"]}`)
	recorder := serveQuoteRequest(NewHandler(fetcher), http.MethodPost, body)

	if recorder.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", recorder.Code, recorder.Body.String())
	}
	if got := strings.Join(fetcher.codes, ","); got != "600519,300750" {
		t.Fatalf("unexpected codes: %s", got)
	}
	if strings.Contains(recorder.Body.String(), `"market"`) {
		t.Fatalf("response must not expose market: %s", recorder.Body.String())
	}

	var payload struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			Quotes       map[string]StockQuote `json:"quotes"`
			MissingCodes []string              `json:"missing_codes"`
		} `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if payload.Code != 0 || payload.Msg != "success" {
		t.Fatalf("unexpected envelope: %#v", payload)
	}
	if payload.Data.Quotes["600519"].LimitUpPrice != 1421.21 {
		t.Fatalf("unexpected quote: %#v", payload.Data.Quotes["600519"])
	}
	if len(payload.Data.MissingCodes) != 1 || payload.Data.MissingCodes[0] != "300750" {
		t.Fatalf("unexpected missing codes: %#v", payload.Data.MissingCodes)
	}
}

// TestQuoteHandlerValidation 验证方法、请求体、数量和股票代码校验。
func TestQuoteHandlerValidation(t *testing.T) {
	tooMany := make([]string, maxQuoteCodes+1)
	for index := range tooMany {
		tooMany[index] = "600519"
	}
	tooManyBody, err := json.Marshal(quoteRequest{Codes: tooMany})
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name   string
		method string
		body   []byte
		status int
	}{
		{name: "method", method: http.MethodGet, body: nil, status: http.StatusMethodNotAllowed},
		{name: "empty", method: http.MethodPost, body: []byte(`{"codes":[]}`), status: http.StatusBadRequest},
		{name: "invalid_code", method: http.MethodPost, body: []byte(`{"codes":["ABC"]}`), status: http.StatusBadRequest},
		{name: "unknown_field", method: http.MethodPost, body: []byte(`{"codes":["600519"],"extra":1}`), status: http.StatusBadRequest},
		{name: "trailing_json", method: http.MethodPost, body: []byte(`{"codes":["600519"]}{}`), status: http.StatusBadRequest},
		{name: "too_many", method: http.MethodPost, body: tooManyBody, status: http.StatusBadRequest},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := serveQuoteRequest(NewHandler(&fakeFetcher{}), test.method, test.body)
			if recorder.Code != test.status {
				t.Fatalf("unexpected status: got=%d want=%d body=%s", recorder.Code, test.status, recorder.Body.String())
			}
			var payload apiResponse
			if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
				t.Fatalf("decode error response: %v", err)
			}
			if payload.Code != test.status || payload.Msg == "" || payload.Data != nil {
				t.Fatalf("unexpected error envelope: %#v", payload)
			}
		})
	}
}

// TestQuoteHandlerUpstreamFailure 验证 TDX 上游错误转换为 502 统一响应。
func TestQuoteHandlerUpstreamFailure(t *testing.T) {
	fetcher := &fakeFetcher{err: errors.New("all hosts failed")}
	recorder := serveQuoteRequest(NewHandler(fetcher), http.MethodPost, []byte(`{"codes":["600519"]}`))
	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("unexpected status: %d body=%s", recorder.Code, recorder.Body.String())
	}
	var payload apiResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Code != http.StatusBadGateway || payload.Data != nil {
		t.Fatalf("unexpected payload: %#v", payload)
	}
}

// serveQuoteRequest 向指定 Handler 发送测试请求并返回响应记录器。
func serveQuoteRequest(handler http.Handler, method string, body []byte) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, Path, bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}
