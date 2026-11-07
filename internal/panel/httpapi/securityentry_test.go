package httpapi

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 启用自定义安全入口时，index.html 的注入必须是**能执行**的形式。
//
// 这条钉的是一个"看起来完全正常"的 bug（2026-11-07 真机撞到）：
// 面板自己下发的 CSP 是 `script-src 'self'`，于是注入的
// `<script>window.__ATL_BASE__="/入口";</script>` **被浏览器拒绝执行** ——
// 页面照常渲染、控制台只有一条 CSP 警告，而前端拿不到前缀，
// 所有接口与 WebSocket 都打到无前缀路径，表现为「启用安全入口后全站 404」。
// 0.11.0 的验收只核对了 HTML 里**有没有**那段字符串，没核对它**有没有跑**。
//
// 所以这里断言三件事：
//  1. 注入用 <meta name="atl-base">（不受 CSP 限制）；
//  2. **没有**内联 script（否则又埋回去）；
//  3. CSP 的 base-uri 允许同源（否则注入的 <base href> 也会被挡）。
func TestSecurityEntryInjectionIsExecutable(t *testing.T) {
	srv, _ := newTestServer(t)
	srv.securityEntry = "secrettunnel"
	srv.webDir = t.TempDir()
	if err := os.WriteFile(filepath.Join(srv.webDir, "index.html"),
		[]byte("<!DOCTYPE html><html><head><title>x</title></head><body><div id=root></div></body></html>"), 0o644); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/secrettunnel/", nil)
	srv.handleStatic(rec, req)

	body := rec.Body.String()
	if !strings.Contains(body, `<meta name="atl-base" content="/secrettunnel">`) {
		t.Errorf("index.html 应注入 <meta name=\"atl-base\">，实际：%s", body)
	}
	if strings.Contains(body, "<script>window.__ATL_BASE__") {
		t.Error("不要再注入内联 script：CSP 是 script-src 'self'，它执行不了（会静默导致全站接口 404）")
	}
	if !strings.Contains(body, `<base href="/secrettunnel/">`) {
		t.Errorf("<base href> 丢了，静态资源会按文档 URL 解析出问题：%s", body)
	}

	// CSP：script-src 仍然是 'self'（挡 XSS 外带），但 base-uri 必须放行同源
	rec2 := httptest.NewRecorder()
	securityHeaders(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})).ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/", nil))
	csp := rec2.Header().Get("Content-Security-Policy")
	if !strings.Contains(csp, "base-uri 'self'") {
		t.Errorf("CSP 的 base-uri 应为 'self'（注入的 <base> 才不会被挡），实际 %q", csp)
	}
	if !strings.Contains(csp, "script-src 'self'") {
		t.Errorf("script-src 必须保持 'self'，实际 %q", csp)
	}
	if strings.Contains(csp, "script-src 'self' 'unsafe-inline'") {
		t.Errorf("不要把 'unsafe-inline' 加到 script-src（那是把 XSS 防护拆掉换这一处方便）")
	}
}

// 没有配安全入口时**不应**注入任何东西（普通部署零影响）。
func TestNoSecurityEntryNoInjection(t *testing.T) {
	srv, _ := newTestServer(t)
	srv.webDir = t.TempDir()
	raw := "<!DOCTYPE html><html><head><title>x</title></head><body></body></html>"
	if err := os.WriteFile(filepath.Join(srv.webDir, "index.html"), []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	srv.handleStatic(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if strings.Contains(rec.Body.String(), "atl-base") {
		t.Errorf("没配安全入口时不该注入 atl-base：%s", rec.Body.String())
	}
}
