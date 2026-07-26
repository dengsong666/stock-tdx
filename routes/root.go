package routes

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"

	"github.com/bensema/gotdx/routes/quotes"
	"github.com/bensema/gotdx/routes/unusual"
)

const (
	// HealthPath 是 HTTP 服务存活检查的固定路径。
	HealthPath = "/api/health"
	// WebPath 是 Web Viewer 的固定入口。
	WebPath = "/web"
)

// NewRootHandler 创建包含业务 API、健康检查和 Web Viewer 的根路由。
func NewRootHandler(webHandler http.Handler, client unusual.MACMarketMonitorClient, options ...unusual.StockUnusualSSEOption) http.Handler {
	if webHandler == nil {
		webHandler = http.NotFoundHandler()
	}
	mux := http.NewServeMux()
	unusual.RegisterStockUnusualSSE(mux, client, options...)
	quotes.Register(mux, quotes.NewService())
	mux.HandleFunc(HealthPath, handleHealth)
	mux.HandleFunc(WebPath, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != WebPath {
			http.NotFound(w, r)
			return
		}
		serveWebRoot(w, r, webHandler)
	})
	mux.Handle(WebPath+"/", http.StripPrefix(WebPath, webHandler))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		http.Redirect(w, r, WebPath, http.StatusFound)
	})
	return sameMainDomainCORS(mux)
}

// writeRouteJSONError 输出根路由使用的 JSON 错误响应。
func writeRouteJSONError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		writeRouteJSONError(w, http.StatusMethodNotAllowed, "请求方法不允许")
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
}

func serveWebRoot(w http.ResponseWriter, r *http.Request, webHandler http.Handler) {
	if webHandler == nil {
		http.NotFound(w, r)
		return
	}
	cloned := r.Clone(r.Context())
	cloned.URL.Path = "/"
	cloned.URL.RawPath = ""
	webHandler.ServeHTTP(w, cloned)
}

func sameMainDomainCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if isAllowedOrigin(origin) {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			w.Header().Add("Vary", "Origin")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func isAllowedOrigin(origin string) bool {
	if origin == "" {
		return false
	}
	parsed, err := url.Parse(origin)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return false
	}
	host := strings.ToLower(parsed.Hostname())
	return host == "dengsong.online" || strings.HasSuffix(host, ".dengsong.online")
}
