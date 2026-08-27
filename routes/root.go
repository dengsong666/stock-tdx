package routes

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/bensema/gotdx/routes/auction"
	"github.com/bensema/gotdx/routes/kline"
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
	kline.Register(mux, kline.NewService())
	auction.Register(mux, auction.NewService())
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
	return sameMainDomainCORS(jwtAuth(mux))
}

// jwtAuth 保护 TDX 的业务接口，健康检查保持免登录供容器探活。
func jwtAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if os.Getenv("JWT_SECRET") == "" && os.Getenv("INTERNAL_TASK_TOKEN") == "" {
			next.ServeHTTP(w, r)
			return
		}
		if r.URL.Path == HealthPath {
			next.ServeHTTP(w, r)
			return
		}
		if !validJWT(r.Header.Get("Authorization")) && !validInternalToken(r.Header.Get("Authorization")) {
			writeRouteJSONError(w, http.StatusUnauthorized, "请先登录")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// validInternalToken 允许模型容器使用内部共享令牌访问 TDX。
func validInternalToken(header string) bool {
	expected := os.Getenv("INTERNAL_TASK_TOKEN")
	return expected != "" && hmac.Equal([]byte(strings.TrimPrefix(header, "Bearer ")), []byte(expected)) && strings.HasPrefix(header, "Bearer ")
}

// validJWT 使用共享密钥校验访问 JWT 的签名和过期时间。
func validJWT(header string) bool {
	if len(header) < 7 || header[:7] != "Bearer " {
		return false
	}
	parts := strings.Split(header[7:], ".")
	if len(parts) != 3 {
		return false
	}
	secret := os.Getenv("JWT_SECRET")
	if secret == "" {
		secret = "stock-dev-jwt-secret-change-me"
	}
	unsigned := parts[0] + "." + parts[1]
	expected := hmac.New(sha256.New, []byte(secret))
	_, _ = expected.Write([]byte(unsigned))
	actual, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || !hmac.Equal(expected.Sum(nil), actual) {
		return false
	}
	body, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return false
	}
	var payload struct {
		Purpose string  `json:"purpose"`
		Exp     float64 `json:"exp"`
	}
	if json.Unmarshal(body, &payload) != nil || payload.Purpose != "access" {
		return false
	}
	return payload.Exp > float64(time.Now().Unix())
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
