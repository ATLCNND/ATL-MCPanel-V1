package httpapi

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// tlsConnState 只用来让 request.TLS 非空（中间件据此判断"这次是不是走的 https"）。
var tlsConnState = tls.ConnectionState{}

// 安全响应头必须真的下发 —— 而且 HSTS 只能走 TLS 时下发。
//
// 为什么要专门写一条测试来守 HSTS 的条件：一旦在纯 HTTP 部署上也发 HSTS，
// 浏览器会记住"这个主机只能走 https"，之后用 http:// 访问会被强制升级，
// 没有证书的部署就被自己锁死了 —— 而且**没有回退办法**（用户改不了浏览器里的记录）。
// 这种"看起来只是多一个响应头"的改动，靠 review 是看不住的。
func TestSecurityHeaders(t *testing.T) {
	srv, ts := newTestServer(t)

	// ① 明文请求：该有的都有，HSTS **不能**有
	resp, err := http.Get(ts.URL + "/api/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	want := map[string]string{
		"X-Content-Type-Options": "nosniff",
		"X-Frame-Options":        "DENY",
		"Referrer-Policy":        "no-referrer",
	}
	for k, v := range want {
		if got := resp.Header.Get(k); got != v {
			t.Errorf("%s 应为 %q，实际 %q", k, v, got)
		}
	}
	if got := resp.Header.Get("Content-Security-Policy"); got == "" {
		t.Error("应下发 Content-Security-Policy")
	} else {
		// script-src 'self' 是真正挡住 XSS 外带的那一条，不能漏
		if !containsAll(got, "script-src 'self'", "frame-ancestors 'none'", "object-src 'none'") {
			t.Errorf("CSP 缺少关键指令，实际 %q", got)
		}
	}
	if got := resp.Header.Get("Strict-Transport-Security"); got != "" {
		t.Errorf("明文请求不该下发 HSTS（会把纯 HTTP 部署锁死），实际 %q", got)
	}

	// ② 走 TLS 的请求：HSTS 必须下发
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "https://example.test/api/health", nil)
	req.TLS = &tlsConnState
	securityHeaders(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).ServeHTTP(rec, req)
	if got := rec.Header().Get("Strict-Transport-Security"); got == "" {
		t.Error("TLS 请求应下发 HSTS")
	}

	// ③ 中间件对每条路由都生效（不只是健康检查）
	for _, path := range []string{"/api/instances", "/ws/console/x"} {
		r := httptest.NewRecorder()
		srv.Handler().ServeHTTP(r, httptest.NewRequest("GET", path, nil))
		if r.Header().Get("X-Frame-Options") != "DENY" {
			t.Errorf("%s 应带上安全响应头", path)
		}
	}
}

func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !strings.Contains(s, sub) {
			return false
		}
	}
	return true
}
