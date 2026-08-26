package auction

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
	codes  []string
	days   int
	result FetchResult
	err    error
}

func (fetcher *fakeFetcher) Fetch(codes []string, days int) (FetchResult, error) {
	fetcher.codes = append([]string(nil), codes...)
	fetcher.days = days
	return fetcher.result, fetcher.err
}

func TestHandlerSuccess(t *testing.T) {
	fetcher := &fakeFetcher{result: FetchResult{
		Auctions: map[string][]AuctionAmount{
			"600127": {{Date: "20260826", AmountBid: 11193, AmountPrev: 80000}},
			"000001": {{Date: "20260826", AmountBid: 404, AmountPrev: 10000}},
		},
		MissingCodes: []string{"000001"},
	}}
	recorder := serveRequest(NewHandler(fetcher), http.MethodPost, `{"codes":[" 600127 ","600127","000001"],"days":3}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if strings.Join(fetcher.codes, ",") != "600127,000001" || fetcher.days != 3 {
		t.Fatalf("unexpected query codes=%v days=%d", fetcher.codes, fetcher.days)
	}
	var payload struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			Auctions     map[string][]AuctionAmount `json:"auctions"`
			MissingCodes []string                   `json:"missing_codes"`
		} `json:"data"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Code != 0 || payload.Msg != "success" || payload.Data.Auctions["600127"][0].AmountBid != 11193 || payload.Data.Auctions["600127"][0].AmountPrev != 80000 {
		t.Fatalf("unexpected payload: %#v", payload)
	}
}

func TestHandlerValidationAndUpstreamError(t *testing.T) {
	tests := []struct {
		name    string
		method  string
		body    string
		fetcher Fetcher
		want    int
	}{
		{name: "method", method: http.MethodGet, body: `{}`, fetcher: &fakeFetcher{}, want: http.StatusMethodNotAllowed},
		{name: "empty_codes", method: http.MethodPost, body: `{"codes":[],"days":3}`, fetcher: &fakeFetcher{}, want: http.StatusBadRequest},
		{name: "invalid_code", method: http.MethodPost, body: `{"codes":["510300"],"days":3}`, fetcher: &fakeFetcher{}, want: http.StatusBadRequest},
		{name: "invalid_days", method: http.MethodPost, body: `{"codes":["600127"],"days":11}`, fetcher: &fakeFetcher{}, want: http.StatusBadRequest},
		{name: "unknown_field", method: http.MethodPost, body: `{"codes":["600127"],"days":3,"extra":1}`, fetcher: &fakeFetcher{}, want: http.StatusBadRequest},
		{name: "upstream", method: http.MethodPost, body: `{"codes":["600127"],"days":3}`, fetcher: &fakeFetcher{err: errors.New("dial failed")}, want: http.StatusBadGateway},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			recorder := serveRequest(NewHandler(test.fetcher), test.method, test.body)
			if recorder.Code != test.want {
				t.Fatalf("status=%d want=%d body=%s", recorder.Code, test.want, recorder.Body.String())
			}
		})
	}
}

func serveRequest(handler http.Handler, method string, body string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, Path, bytes.NewBufferString(body))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}
