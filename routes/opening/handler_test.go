package opening

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
	date   uint32
	codes  []string
	result FetchResult
	err    error
}

func (fetcher *fakeFetcher) Fetch(date uint32, codes []string) (FetchResult, error) {
	fetcher.date = date
	fetcher.codes = append([]string(nil), codes...)
	return fetcher.result, fetcher.err
}

func serveRequest(handler http.Handler, method, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, Path, bytes.NewBufferString(body))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func TestHandlerSuccess(t *testing.T) {
	auctionTime, auctionPrice := "09:25:03", 25.93
	fetcher := &fakeFetcher{result: FetchResult{
		Trades: map[string]OpeningTrade{
			"001202": {AuctionTime: &auctionTime, AuctionPrice: &auctionPrice, FirstTime: "09:30:03", FirstPrice: 26.02, FirstVol: 450},
		},
		MissingCodes: []string{"600519"},
	}}
	recorder := serveRequest(NewHandler(fetcher), http.MethodPost, `{"date":"20210512","codes":[" 001202 ","001202","600519"]}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if fetcher.date != 20210512 || strings.Join(fetcher.codes, ",") != "001202,600519" {
		t.Fatalf("unexpected query date=%d codes=%v", fetcher.date, fetcher.codes)
	}
	var payload struct {
		Code int `json:"code"`
		Data struct {
			Trades       map[string]map[string]any `json:"trades"`
			MissingCodes []string                  `json:"missing_codes"`
		} `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	trade := payload.Data.Trades["001202"]
	if payload.Code != 0 || trade["first_price"] != 26.02 || trade["auction_price"] != 25.93 || trade["first_time"] != "09:30:03" {
		t.Fatalf("unexpected payload %s", recorder.Body.String())
	}
	if strings.Join(payload.Data.MissingCodes, ",") != "600519" {
		t.Fatalf("unexpected missing codes %v", payload.Data.MissingCodes)
	}
}

func TestHandlerEmptyResultUsesEmptyCollections(t *testing.T) {
	recorder := serveRequest(NewHandler(&fakeFetcher{}), http.MethodPost, `{"date":"20210512","codes":["001202"]}`)
	if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"trades":{}`) || !strings.Contains(recorder.Body.String(), `"missing_codes":[]`) {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestHandlerRejectsBadRequests(t *testing.T) {
	tooMany := make([]string, maxCodes+1)
	for index := range tooMany {
		tooMany[index] = `"600519"`
	}
	cases := map[string]string{
		"bad date":      `{"date":"20261332","codes":["600519"]}`,
		"dashed date":   `{"date":"2026-11-25","codes":["600519"]}`,
		"empty codes":   `{"date":"20261125","codes":[]}`,
		"too many":      `{"date":"20261125","codes":[` + strings.Join(tooMany, ",") + `]}`,
		"bad code":      `{"date":"20261125","codes":["12345"]}`,
		"unknown field": `{"date":"20261125","codes":["600519"],"days":3}`,
		"two objects":   `{"date":"20261125","codes":["600519"]}{}`,
	}
	for name, body := range cases {
		recorder := serveRequest(NewHandler(&fakeFetcher{}), http.MethodPost, body)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("%s: status=%d body=%s", name, recorder.Code, recorder.Body.String())
		}
	}
}

func TestHandlerMethodAndUpstreamErrors(t *testing.T) {
	if recorder := serveRequest(NewHandler(&fakeFetcher{}), http.MethodGet, ""); recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET status=%d", recorder.Code)
	}
	if recorder := serveRequest(NewHandler(nil), http.MethodPost, `{"date":"20261125","codes":["600519"]}`); recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("nil fetcher status=%d", recorder.Code)
	}
	failing := &fakeFetcher{err: errors.New("host=a: timeout")}
	recorder := serveRequest(NewHandler(failing), http.MethodPost, `{"date":"20261125","codes":["600519"]}`)
	if recorder.Code != http.StatusBadGateway || !strings.Contains(recorder.Body.String(), "timeout") {
		t.Fatalf("upstream error status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}
