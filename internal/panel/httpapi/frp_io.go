package httpapi

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/ATLCNND/ATL-MCPanel/internal/frp"
)

// 穿透配置的导入/导出（底账 F15）。
//
// 这两个接口都**只做文本与结构之间的转换**，不落库：
//   · 导出：把面板里已有的一条线路（含隧道）还原成标准 frpc 配置，供复制/备份；
//   · 导入预览：把用户粘贴的第三方配置解析出来给用户**看一眼**，确认无误后再谈落库。
// 落库要等线路类型模型（自建 frps / 第三方单隧道 / 公网直出）落地，见底账 F16 ——
// 在那之前先让用户能「看清解析出了什么」，比让他盲贴一条自动建线更安全。

// maxImportConfigBytes 粘贴内容的体积上限。
//
// 这个输入来自浏览器、会被逐行扫描，不设上限就是一个免费的 CPU/内存放大器
//（和 console 的 ?spam= 参数同一个道理）。64KB 足够容纳任何真实 frpc 配置 ——
// 实测两份服务商样例都在 1KB 以内。
const maxImportConfigBytes = 64 * 1024

// maxImportProxies 单份配置允许的隧道条数上限（同理，防放大）。
const maxImportProxies = 200

// handleExportFrps GET /api/frps/{id}/export?token=0&style=metadatas
//
// 返回标准 frpc TOML 文本。**含 token 时属于敏感操作**，因此：
//   · 默认**不带** token（?token=1 才带）；
//   · 带了就记一条审计（谁在什么时候导出了含凭据的配置）。
func (s *Server) handleExportFrps(w http.ResponseWriter, r *http.Request) {
	if !requireAdminIn(w, r) {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id == 0 {
		writeErr(w, http.StatusBadRequest, "id 无效")
		return
	}

	var name, host string
	var bindPort int32
	var token, authMode, frpsUser string
	if err := s.db.QueryRow(
		`SELECT name, host, bind_port, COALESCE(token,''), COALESCE(auth_mode,'auth'), COALESCE(user,'')
		 FROM frps_servers WHERE id = ?`, id).
		Scan(&name, &host, &bindPort, &token, &authMode, &frpsUser); err != nil {
		writeErr(w, http.StatusNotFound, "frps 线路不存在")
		return
	}

	rows, err := s.db.Query(
		`SELECT tunnel_id, COALESCE(name,''), COALESCE(protocol,'tcp'), local_port, remote_port,
		        COALESCE(bandwidth_limit,''), COALESCE(bandwidth_limit_mode,'')
		 FROM tunnels WHERE frps_id = ? ORDER BY tunnel_id`, id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()

	tunnels := []frp.Tunnel{}
	for rows.Next() {
		var t frp.Tunnel
		if err := rows.Scan(&t.TunnelID, &t.Name, &t.Protocol, &t.LocalPort, &t.RemotePort,
			&t.BandwidthLimit, &t.BandwidthLimitMode); err != nil {
			continue
		}
		tunnels = append(tunnels, t)
	}

	withToken := r.URL.Query().Get("token") == "1"
	style := strings.TrimSpace(r.URL.Query().Get("style"))
	// 线路本身是 metadatas 登录方式的，导出就按那种写法生成 ——
	// 导出一份「登录方式不对」的配置，对方拿去跑照样登不上（这正是本轮修的坑）。
	if authMode == frp.AuthModeMetadatas {
		style = frp.AuthModeMetadatas
	}

	text, err := frp.RenderStandardConfig(
		frp.Server{Host: host, BindPort: int(bindPort), Token: token},
		tunnels,
		frp.ExportOptions{
			IncludeToken:       withToken,
			UseLoliaTokenStyle: style == frp.AuthModeMetadatas,
			Header:             "ATL-MCPanel 导出：" + name,
			User:               frpsUser,
		},
	)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if withToken && token != "" {
		// 导出凭据是敏感动作，必须留痕（谁、哪条线路、带没带 token）
		s.audit(r, "export_frps_config", name, "includes_token=true")
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"text":           text,
		"tunnel_count":   len(tunnels),
		"includes_token": withToken && token != "",
		"line": map[string]interface{}{
			"name": name, "host": host, "bind_port": bindPort,
		},
	})
}

// PreviewProxy 导入预览里的一条隧道（**不含**任何凭据）。
type PreviewProxy struct {
	Name          string   `json:"name"`
	Type          string   `json:"type"`
	LocalIP       string   `json:"local_ip"`
	LocalPort     int      `json:"local_port"`
	RemotePort    int      `json:"remote_port"`
	Subdomain     string   `json:"subdomain,omitempty"`
	CustomDomains []string `json:"custom_domains,omitempty"`
	Bandwidth     string   `json:"bandwidth_limit,omitempty"`
}

// handlePreviewFrpcImport POST /api/frp/import/preview  {"config": "..."}
//
// 解析一份第三方 frpc 配置并**原样回显**解析结果，让用户在落库之前先看清：
// 服务端地址/端口、token 是否存在（**打码回显**，不回传明文）、有哪些隧道、
// 哪些内容被跳过了。全部成功才算 ok=true。
func (s *Server) handlePreviewFrpcImport(w http.ResponseWriter, r *http.Request) {
	if !requireAdminIn(w, r) {
		return
	}
	// 读之前先限长：这类「用户粘贴的文本」必须有上限（见 maxImportConfigBytes 的说明）
	limited := http.MaxBytesReader(w, r.Body, maxImportConfigBytes)
	var req struct {
		Config string `json:"config"`
	}
	if err := json.NewDecoder(limited).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体过大或不是合法 JSON（上限 64KB）")
		return
	}
	if strings.TrimSpace(req.Config) == "" {
		writeErr(w, http.StatusBadRequest, "请粘贴 frpc 配置文本")
		return
	}

	cfg, err := frp.ImportConfig(req.Config)
	if err != nil {
		// 解析失败也返回 200：这是「预览」接口，失败是**预期的业务结果**，
		// 不是服务端错误。前端据此把 error 显示在粘贴框下面即可。
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"ok":    false,
			"error": err.Error(),
		})
		return
	}
	if len(cfg.Proxies) > maxImportProxies {
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"ok":    false,
			"error": "隧道条数超过上限（" + strconv.Itoa(maxImportProxies) + " 条）",
		})
		return
	}

	proxies := make([]PreviewProxy, 0, len(cfg.Proxies))
	for _, p := range cfg.Proxies {
		proxies = append(proxies, PreviewProxy{
			Name: p.Name, Type: p.Type, LocalIP: p.LocalIP,
			LocalPort: p.LocalPort, RemotePort: p.RemotePort,
			Subdomain: p.Subdomain, CustomDomains: p.CustomDomains,
			Bandwidth: p.BandwidthLimit,
		})
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"ok": true,
		"line": map[string]interface{}{
			"server_addr":  cfg.ServerAddr,
			"server_port":  cfg.ServerPort,
			"user":         cfg.User,
			"token_masked": frp.MaskToken(cfg.Token),
			"has_token":    cfg.Token != "",
			"tls_enable":   cfg.TLS.Enable,
		},
		"proxies":  proxies,
		"warnings": cfg.Warnings,
	})
}

