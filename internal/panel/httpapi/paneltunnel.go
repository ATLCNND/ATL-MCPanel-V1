package httpapi

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/ATLCNND/ATL-MCPanel/internal/frp"
)

// 面板自身穿透使用固定的伪实例 ID 与隧道 ID。
const (
	panelTunnelInstanceID = "__panel__"
	panelTunnelID         = "atlmcpanel-panel"
)

// panelTunnelConfig 面板穿透配置（与 panel_tunnel 表对应）。
type panelTunnelConfig struct {
	Enabled      bool   `json:"enabled"`
	FrpsID       int64  `json:"frps_id"`
	ProxyType    string `json:"proxy_type"` // tcp / http / https
	RemotePort   int    `json:"remote_port"`
	CustomDomain string `json:"custom_domain"`
	Subdomain    string `json:"subdomain"`
	UseTLS       bool   `json:"use_tls"`
	CertFile     string `json:"cert_file"`
	KeyFile      string `json:"key_file"`
}

// loadPanelTunnel 读取面板穿透配置。
func (s *Server) loadPanelTunnel() (panelTunnelConfig, error) {
	var c panelTunnelConfig
	var enabled, useTLS int
	err := s.db.QueryRow(`
		SELECT enabled, frps_id, proxy_type, remote_port, custom_domain, subdomain, use_tls, cert_file, key_file
		FROM panel_tunnel WHERE id = 1`).
		Scan(&enabled, &c.FrpsID, &c.ProxyType, &c.RemotePort, &c.CustomDomain, &c.Subdomain, &useTLS, &c.CertFile, &c.KeyFile)
	if err == sql.ErrNoRows {
		return panelTunnelConfig{ProxyType: "tcp"}, nil
	}
	if err != nil {
		return c, err
	}
	c.Enabled = enabled == 1
	c.UseTLS = useTLS == 1
	if c.ProxyType == "" {
		c.ProxyType = "tcp"
	}
	return c, nil
}

// savePanelTunnel 保存面板穿透配置。
func (s *Server) savePanelTunnel(c panelTunnelConfig) error {
	enabled := 0
	if c.Enabled {
		enabled = 1
	}
	useTLS := 0
	if c.UseTLS {
		useTLS = 1
	}
	_, err := s.db.Exec(`
		INSERT INTO panel_tunnel (id, enabled, frps_id, proxy_type, remote_port, custom_domain, subdomain, use_tls, cert_file, key_file, updated_at)
		VALUES (1, ?, ?, ?, ?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(id) DO UPDATE SET
			enabled=excluded.enabled, frps_id=excluded.frps_id, proxy_type=excluded.proxy_type,
			remote_port=excluded.remote_port, custom_domain=excluded.custom_domain,
			subdomain=excluded.subdomain, use_tls=excluded.use_tls,
			cert_file=excluded.cert_file, key_file=excluded.key_file, updated_at=CURRENT_TIMESTAMP`,
		enabled, c.FrpsID, c.ProxyType, c.RemotePort, c.CustomDomain, c.Subdomain, useTLS, c.CertFile, c.KeyFile)
	return err
}

// parsePort 从 ":8080" / "0.0.0.0:8080" 解析端口。
func parsePort(addr string) int {
	i := strings.LastIndex(addr, ":")
	if i < 0 {
		return 0
	}
	var p int
	_, _ = fmt.Sscanf(addr[i+1:], "%d", &p)
	return p
}

// localPanelPort 面板 HTTP 监听端口。
func (s *Server) localPanelPort() int {
	return parsePort(s.listenAddr)
}

// publicTargetPort 面板穿透的转发目标端口。
// 若配置了 HTTPS 监听，则优先转发到 HTTPS 端口（对外为加密访问）。
func (s *Server) publicTargetPort() int {
	if p := parsePort(s.tlsListen); p > 0 {
		return p
	}
	return s.localPanelPort()
}

// applyPanelTunnel 依据配置启动/更新/停止面板 frpc。
func (s *Server) applyPanelTunnel(c panelTunnelConfig) (string, string) {
	if s.panelFrp == nil {
		return "error", "面板穿透未初始化"
	}

	// 停用：移除代理
	if !c.Enabled {
		_ = s.panelFrp.Remove(panelTunnelInstanceID, s.panelFrpDir, panelTunnelID)
		return "stopped", "面板穿透已关闭"
	}

	// 取 frps 信息
	var host, token string
	var bindPort int32
	if err := s.db.QueryRow(`SELECT host, bind_port, token FROM frps_servers WHERE id = ?`, c.FrpsID).
		Scan(&host, &bindPort, &token); err != nil {
		return "error", "frps 服务器不存在"
	}

	localPort := s.publicTargetPort()
	if localPort <= 0 {
		return "error", "无法确定面板监听端口"
	}

	proto := c.ProxyType
	if proto == "" {
		proto = "tcp"
	}
	// tcp 模式需要公网端口；http/https 使用域名
	if proto == "tcp" && c.RemotePort <= 0 {
		return "error", "TCP 模式需要指定公网端口"
	}
	if (proto == "http" || proto == "https") && c.CustomDomain == "" && c.Subdomain == "" {
		return "error", "HTTP/HTTPS 模式需要指定域名"
	}
	if proto == "https" && c.UseTLS && (c.CertFile == "" || c.KeyFile == "") {
		return "error", "启用 TLS 终止需要提供证书与私钥路径"
	}

	err := s.panelFrp.Apply(panelTunnelInstanceID, s.panelFrpDir, frp.Server{
		Host:     host,
		BindPort: int(bindPort),
		Token:    token,
	}, frp.Tunnel{
		TunnelID:      panelTunnelID,
		Name:          "面板访问",
		Protocol:      proto,
		LocalPort:     localPort,
		RemotePort:    c.RemotePort,
		CustomDomains: splitDomains(c.CustomDomain),
		Subdomain:     strings.TrimSpace(c.Subdomain),
		UseTLS:        c.UseTLS,
		CertFile:      strings.TrimSpace(c.CertFile),
		KeyFile:       strings.TrimSpace(c.KeyFile),
	})
	if err != nil {
		return "error", err.Error()
	}
	return "running", "面板穿透已启用"
}

// splitDomains 拆分逗号分隔的域名列表。
func splitDomains(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if v := strings.TrimSpace(p); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// panelPublicAddress 计算面板的公网访问地址（供前端展示）。
func (s *Server) panelPublicAddress(c panelTunnelConfig, frpsHost string) string {
	if !c.Enabled || frpsHost == "" {
		return ""
	}
	tlsScheme := "http://"
	if parsePort(s.tlsListen) > 0 {
		tlsScheme = "https://"
	}
	switch c.ProxyType {
	case "http":
		if c.CustomDomain != "" {
			return "http://" + strings.TrimSpace(strings.Split(c.CustomDomain, ",")[0])
		}
		return "http://" + c.Subdomain
	case "https":
		if c.CustomDomain != "" {
			return "https://" + strings.TrimSpace(strings.Split(c.CustomDomain, ",")[0])
		}
		return "https://" + c.Subdomain
	default:
		return fmt.Sprintf("%s%s:%d", tlsScheme, frpsHost, c.RemotePort)
	}
}

// handleGetPanelTunnel GET /api/panel-tunnel
func (s *Server) handleGetPanelTunnel(w http.ResponseWriter, r *http.Request) {
	if !requireAdminIn(w, r) {
		return
	}
	c, err := s.loadPanelTunnel()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	status, detail := "stopped", ""
	if s.panelFrp != nil {
		for _, t := range s.panelFrp.List(panelTunnelInstanceID) {
			status = t.Status
			detail = t.Error
		}
	}

	frpsHost := ""
	if c.FrpsID > 0 {
		_ = s.db.QueryRow(`SELECT host FROM frps_servers WHERE id = ?`, c.FrpsID).Scan(&frpsHost)
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"config":         c,
		"status":         status,
		"error":          detail,
		"public_address": s.panelPublicAddress(c, frpsHost),
		"local_port":     s.localPanelPort(),
		"tls_port":       parsePort(s.tlsListen),
		"current_url":    s.currentURL(),
	})
}

// handleSetPanelTunnel POST /api/panel-tunnel
func (s *Server) handleSetPanelTunnel(w http.ResponseWriter, r *http.Request) {
	if !requireAdminIn(w, r) {
		return
	}
	var c panelTunnelConfig
	if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
		writeErr(w, http.StatusBadRequest, "无效请求体")
		return
	}
	if c.ProxyType == "" {
		c.ProxyType = "tcp"
	}
	if c.ProxyType != "tcp" && c.ProxyType != "http" && c.ProxyType != "https" {
		writeErr(w, http.StatusBadRequest, "proxy_type 必须是 tcp / http / https")
		return
	}
	if c.Enabled && c.FrpsID == 0 {
		writeErr(w, http.StatusBadRequest, "启用面板穿透需要先选择 frps 服务器")
		return
	}

	if err := s.savePanelTunnel(c); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	status, msg := s.applyPanelTunnel(c)
	s.audit(r, "set_panel_tunnel", fmt.Sprintf("enabled=%v type=%s", c.Enabled, c.ProxyType), msg)
	if status == "error" {
		writeErr(w, http.StatusInternalServerError, msg)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": status, "message": msg})
}

// initPanelTunnel 在 Panel 启动时恢复面板穿透配置。
func (s *Server) initPanelTunnel() {
	c, err := s.loadPanelTunnel()
	if err != nil || !c.Enabled {
		return
	}
	status, msg := s.applyPanelTunnel(c)
	if status == "error" {
		s.logger.Warn("恢复面板穿透失败", "error", msg)
		return
	}
	s.logger.Info("面板穿透已恢复", "type", c.ProxyType, "msg", msg)
}
