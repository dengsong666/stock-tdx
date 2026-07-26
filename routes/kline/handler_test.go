package kline

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bensema/gotdx/types"
)

type fakeFetcher struct {
	query Query
	bars  []Bar
	err   error
}

// Fetch 记录规范化查询并返回预设结果。
func (fetcher *fakeFetcher) Fetch(query Query) ([]Bar, error) {
	fetcher.query = query
	return fetcher.bars, fetcher.err
}

func TestKLineHandlerSuccessDefaults(t *testing.T) {
	now := time.Date(2026, 7, 26, 14, 30, 45, 0, shanghaiLocation)
	fetcher := &fakeFetcher{bars: []Bar{{Time: "2026-07-25 15:00:00", Close: 10}}}
	handler := newHandler(fetcher, func() time.Time { return now })
	body := []byte(`{"code":" 600519 ","period":"3m","start":"2026-07-01"}`)
	recorder := serveRequest(handler, http.MethodPost, body)

	if recorder.Code != http.StatusOK {
		t.Fatalf("unexpected status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if fetcher.query.Code != "600519" || fetcher.query.Market != 1 {
		t.Fatalf("unexpected code/market: %#v", fetcher.query)
	}
	if fetcher.query.Type != assetStock {
		t.Fatalf("default type must be stock: %#v", fetcher.query)
	}
	if fetcher.query.Period.Category != types.KLINE_TYPE_1MIN || fetcher.query.Period.Times != 3 {
		t.Fatalf("unexpected 3m mapping: %#v", fetcher.query.Period)
	}
	if fetcher.query.Adjust != types.AdjustQFQ || fetcher.query.AdjustName != "qfq" {
		t.Fatalf("default adjust must be qfq: %#v", fetcher.query)
	}
	if got := fetcher.query.End.Format(dateTimeLayout); got != "2026-07-26 14:30:45" {
		t.Fatalf("unexpected default end: %s", got)
	}
	if strings.Contains(recorder.Body.String(), `"market"`) {
		t.Fatalf("response must not expose market: %s", recorder.Body.String())
	}

	var payload struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data []Bar  `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if payload.Code != 0 || payload.Msg != "success" || len(payload.Data) != 1 {
		t.Fatalf("unexpected payload: %#v", payload)
	}
	if strings.Contains(recorder.Body.String(), `"is_complete"`) || strings.Contains(recorder.Body.String(), `"klines"`) {
		t.Fatalf("TDX response must directly return bars without completion flag: %s", recorder.Body.String())
	}
}

func TestKLineHandlerExplicitEndAndAdjust(t *testing.T) {
	fetcher := &fakeFetcher{}
	handler := newHandler(fetcher, time.Now)
	body := []byte(`{"code":"300750","period":"day","start":"2026-07-01","end":"2026-07-20","adjust":"none"}`)
	recorder := serveRequest(handler, http.MethodPost, body)
	if recorder.Code != http.StatusOK {
		t.Fatalf("unexpected status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if got := fetcher.query.End.Format("2006-01-02 15:04:05.999999999"); got != "2026-07-20 23:59:59.999999999" {
		t.Fatalf("date-only end must include whole day: %s", got)
	}
	if fetcher.query.Adjust != types.AdjustNone {
		t.Fatalf("unexpected adjust: %d", fetcher.query.Adjust)
	}
}

func TestKLineHandlerIndexMarketAndAdjust(t *testing.T) {
	fetcher := &fakeFetcher{}
	handler := newHandler(fetcher, time.Now)
	body := []byte(`{"type":"index","market":"bj","code":"899050","period":"day","start":"2026-07-01"}`)
	recorder := serveRequest(handler, http.MethodPost, body)
	if recorder.Code != http.StatusOK {
		t.Fatalf("unexpected status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if fetcher.query.Type != assetIndex || fetcher.query.Market != 2 || fetcher.query.Adjust != types.AdjustNone || fetcher.query.AdjustName != "none" {
		t.Fatalf("unexpected index query: %#v", fetcher.query)
	}
}

func TestKLineHandlerValidation(t *testing.T) {
	handler := newHandler(&fakeFetcher{}, time.Now)
	tests := []struct {
		name   string
		method string
		body   string
	}{
		{name: "method", method: http.MethodGet, body: `{}`},
		{name: "missing_start", method: http.MethodPost, body: `{"code":"600519","period":"day"}`},
		{name: "invalid_stock", method: http.MethodPost, body: `{"code":"510300","period":"day","start":"2026-01-01"}`},
		{name: "index_missing_market", method: http.MethodPost, body: `{"type":"index","code":"000001","period":"day","start":"2026-01-01"}`},
		{name: "index_adjust", method: http.MethodPost, body: `{"type":"index","market":"sh","code":"000001","period":"day","start":"2026-01-01","adjust":"qfq"}`},
		{name: "stock_market", method: http.MethodPost, body: `{"type":"stock","market":"sh","code":"600519","period":"day","start":"2026-01-01"}`},
		{name: "unsupported_120m", method: http.MethodPost, body: `{"code":"600519","period":"120m","start":"2026-01-01"}`},
		{name: "invalid_range", method: http.MethodPost, body: `{"code":"600519","period":"day","start":"2026-02-01","end":"2026-01-01"}`},
		{name: "unknown_field", method: http.MethodPost, body: `{"code":"600519","period":"day","start":"2026-01-01","extra":1}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := serveRequest(handler, test.method, []byte(test.body))
			want := http.StatusBadRequest
			if test.method == http.MethodGet {
				want = http.StatusMethodNotAllowed
			}
			if recorder.Code != want {
				t.Fatalf("status=%d want=%d body=%s", recorder.Code, want, recorder.Body.String())
			}
			assertErrorEnvelope(t, recorder)
		})
	}
}

func TestKLineHandlerUpstreamErrors(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		want int
	}{
		{name: "too_many", err: ErrTooManyBars, want: http.StatusBadRequest},
		{name: "upstream", err: errors.New("dial failed"), want: http.StatusBadGateway},
	} {
		t.Run(test.name, func(t *testing.T) {
			handler := newHandler(&fakeFetcher{err: test.err}, time.Now)
			recorder := serveRequest(handler, http.MethodPost, []byte(`{"code":"600519","period":"day","start":"2026-01-01"}`))
			if recorder.Code != test.want {
				t.Fatalf("status=%d want=%d body=%s", recorder.Code, test.want, recorder.Body.String())
			}
			assertErrorEnvelope(t, recorder)
		})
	}
}

// serveRequest 使用 httptest 调用指定 Handler。
func serveRequest(handler http.Handler, method string, body []byte) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, Path, bytes.NewReader(body))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

// assertErrorEnvelope 校验错误响应固定为 code、msg、data(null)。
func assertErrorEnvelope(t *testing.T, recorder *httptest.ResponseRecorder) {
	t.Helper()
	var payload struct {
		Code int     `json:"code"`
		Msg  string  `json:"msg"`
		Data *string `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode error response: %v", err)
	}
	if payload.Code != recorder.Code || payload.Msg == "" || payload.Data != nil {
		t.Fatalf("unexpected error response: %#v", payload)
	}
}
