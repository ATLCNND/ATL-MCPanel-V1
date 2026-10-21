package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/ATLCNND/ATL-MCPanel/internal/frp"
)

// handleApplyFrpcImport POST /api/frp/import/apply
//
// 把用户粘贴的第三方 frpc 配置**真正落库**：建一条 third_party 线路 + 若干隧道。
//
// 为什么与 preview 分成两个接口：
//   · preview 是"看一眼"，不改任何东西；apply 是**写操作**，会新增线路与隧道。
//     合成一个接口就必然要一个 confirm 参数，那种"贴进去就建"的交互正是
//     底账里反复强调要避免的（用户根本没机会看清解析出了什么）。
//
// 三道保护：
//   ① 整件事在**一个事务**里：线路建了但隧道失败，不留半截电路由；
//   ② 同名线路已存在时直接拒绝，而不是悄悄叠加第二条（否则会出现两条同名线路，
//      界面上分不清，删的时候更分不清）；
//   ③ 隧道一条都建不成就整体失败（导入一份没有隧道的东西没有意义）。
func (s *Server) handleApplyFrpcImport(w http.ResponseWriter, r *http.Request) {
	if !requireAdminIn(w, r) {
		return
	}
	limited := http.MaxBytesReader(w, r.Body, maxImportConfigBytes)
	var req struct {
		Config string `json:"config"`
		// Name 线路名（可选）：留空就用配置里的 serverAddr
		Name string `json:"name"`
	}
	if err := json.NewDecoder(limited).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "请求体过大或不是合法 JSON（上限 64KB）")
		return
	}

	cfg, err := frp.ImportConfig(req.Config)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(cfg.Proxies) > maxImportProxies {
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("隧道条数超过上限（%d 条）", maxImportProxies))
		return
	}

	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = cfg.ServerAddr
	}
	var exists int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM frps_servers WHERE name = ?`, name).Scan(&exists); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if exists > 0 {
		writeErr(w, http.StatusConflict, "已存在同名线路；换个名字，或先删掉旧的再导入")
		return
	}

	tx, err := s.db.Begin()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer func() { _ = tx.Rollback() }()

	// 第三方线路：没有端口段（0），远端端口由服务商给定，面板不分配
	res, err := tx.Exec(`
		INSERT INTO frps_servers (name, host, bind_port, token, port_start, port_end, remark, display_domain, line_type)
		VALUES (?, ?, ?, ?, 0, 0, ?, '', ?)`,
		name, cfg.ServerAddr, cfg.ServerPort, cfg.Token,
		"由导入生成（"+frp.MaskToken(cfg.Token)+"）", frp.LineThirdParty)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	lineID, _ := res.LastInsertId()

	created := []map[string]interface{}{}
	for i, p := range cfg.Proxies {
		tunnelID := fmt.Sprintf("import-%d-%d", lineID, i+1)
		// 名字：优先用配置里的 proxy 名（那是用户能认出来的）
		tname := strings.TrimSpace(p.Name)
		if tname == "" {
			tname = tunnelID
		}
		sub, domain := p.Subdomain, ""
		if len(p.CustomDomains) > 0 {
			domain = strings.Join(p.CustomDomains, ",")
		}
		// 导入的隧道**没有实例归属**：它描述的是"服务商给了什么"，
		// 具体挂到哪个实例上由管理员之后在「线路分配」里选。
		// 因此这里不写 local_port 的实例语义，只如实记录配置里的值。
		if _, err := tx.Exec(`INSERT INTO tunnels (tunnel_id, instance_id, frps_id, name, protocol, local_port, remote_port, subdomain, custom_domain, status) VALUES (?, '', ?, ?, ?, ?, ?, ?, ?, 'imported')`,
			tunnelID, lineID, tname, p.Type, p.LocalPort, p.RemotePort, sub, domain); err != nil {
			writeErr(w, http.StatusInternalServerError, "写入隧道失败："+err.Error())
			return
		}
		created = append(created, map[string]interface{}{
			"tunnel_id": tunnelID, "name": tname, "type": p.Type,
			"local_port": p.LocalPort, "remote_port": p.RemotePort,
		})
	}
	if len(created) == 0 {
		writeErr(w, http.StatusBadRequest, "这份配置里没有可导入的隧道")
		return
	}
	if err := tx.Commit(); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	s.audit(r, "import_frpc_config", name,
		fmt.Sprintf("line_id=%d tunnels=%d", lineID, len(created)))
	writeJSON(w, http.StatusCreated, map[string]interface{}{
		"line_id":   lineID,
		"line_name": name,
		"tunnels":   created,
		"warnings":  cfg.Warnings,
		"message":   "已建线路与 " + fmt.Sprint(len(created)) + " 条隧道（尚未绑定实例）",
	})
}

