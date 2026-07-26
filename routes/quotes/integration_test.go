package quotes

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// TestQuoteHandlerIntegration 通过真实 TDX MAC 主站验证正式 HTTP 行情响应。
func TestQuoteHandlerIntegration(t *testing.T) {
	if os.Getenv("GOTDX_INTEGRATION") != "1" {
		t.Skip("set GOTDX_INTEGRATION=1 to run live quote service test")
	}

	server := httptest.NewServer(NewHandler(NewService()))
	defer server.Close()
	client := &http.Client{Timeout: 20 * time.Second}
	request, err := http.NewRequest(
		http.MethodPost,
		server.URL,
		bytes.NewBufferString(`{"codes":["600519","300750","000001"]}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")

	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("unexpected status: %d", response.StatusCode)
	}

	var payload struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			Quotes       map[string]StockQuote `json:"quotes"`
			MissingCodes []string              `json:"missing_codes"`
		} `json:"data"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if payload.Code != 0 || payload.Msg != "success" {
		t.Fatalf("unexpected envelope: %#v", payload)
	}
	for _, code := range []string{"600519", "300750", "000001"} {
		quote, ok := payload.Data.Quotes[code]
		if !ok || quote.Price <= 0 || quote.PreviousClose <= 0 || quote.LimitUpPrice <= 0 {
			t.Fatalf("invalid live quote code=%s quote=%#v missing=%v", code, quote, payload.Data.MissingCodes)
		}
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"market"`) {
		t.Fatalf("response must not expose market: %s", raw)
	}
}
