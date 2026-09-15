package httpapi

import (
	"net/http"
	"time"
)

// ListenAndServe 启动 HTTP 服务；当提供证书与私钥时以 HTTPS 提供服务。
//
// 面板以 HTTPS 提供服务后，配合 frp 的 tcp 穿透即可实现「公网端口 → 面板 TLS」
// 的端到端加密，无需域名或 frps 的 vhost 配置。
func ListenAndServe(addr string, handler http.Handler, certFile, keyFile string) error {
	srv := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}
	if certFile != "" && keyFile != "" {
		return srv.ListenAndServeTLS(certFile, keyFile)
	}
	return srv.ListenAndServe()
}
