package routes

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/bensema/gotdx/proto"
	"github.com/bensema/gotdx/routes/kline"
)

type rootMonitorClient struct{}

// MACMarketMonitor 为根路由测试提供不联网的空异动客户端。
func (*rootMonitorClient) MACMarketMonitor(uint8, uint32, uint32) ([]proto.MACMarketMonitorItem, error) {
	return nil, nil
}

func TestRootHandlerRoutes(t *testing.T) {
	web := http.NewServeMux()
	web.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte("viewer"))
	})
	web.HandleFunc("/api/methods", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("methods"))
	})

	handler := NewRootHandler(web, &rootMonitorClient{})

	request := httptest.NewRequest(http.MethodGet, "/", nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusFound || recorder.Header().Get("Location") != WebPath {
		t.Fatalf("unexpected root redirect: code=%d location=%q", recorder.Code, recorder.Header().Get("Location"))
	}

	for path, want := range map[string]string{
		WebPath:                  "viewer",
		WebPath + "/api/methods": "methods",
	} {
		request = httptest.NewRequest(http.MethodGet, path, nil)
		recorder = httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK || recorder.Body.String() != want {
			t.Fatalf("unexpected response for %s: code=%d body=%q", path, recorder.Code, recorder.Body.String())
		}
	}

	request = httptest.NewRequest(http.MethodGet, "/api/methods", nil)
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("legacy path must not be available: %d", recorder.Code)
	}

	request = httptest.NewRequest(http.MethodGet, kline.Path, nil)
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("kline route must be registered: code=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestRootHandlerHealth(t *testing.T) {
	handler := NewRootHandler(http.NotFoundHandler(), &rootMonitorClient{})
	request := httptest.NewRequest(http.MethodGet, HealthPath, nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("unexpected health status: %d", recorder.Code)
	}
	var payload map[string]string
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode health response: %v", err)
	}
	if payload["status"] != "ok" {
		t.Fatalf("unexpected health payload: %#v", payload)
	}
}

func TestRootHandlerCORS(t *testing.T) {
	handler := NewRootHandler(http.NotFoundHandler(), &rootMonitorClient{})
	for _, test := range []struct {
		origin string
		want   string
	}{
		{origin: "https://dengsong.online", want: "https://dengsong.online"},
		{origin: "https://app.dengsong.online", want: "https://app.dengsong.online"},
		{origin: "https://example.com", want: ""},
	} {
		request := httptest.NewRequest(http.MethodGet, HealthPath, nil)
		request.Header.Set("Origin", test.origin)
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if got := recorder.Header().Get("Access-Control-Allow-Origin"); got != test.want {
			t.Fatalf("origin %q: got %q want %q", test.origin, got, test.want)
		}
	}
}
