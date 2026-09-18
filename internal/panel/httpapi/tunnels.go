package httpapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ATLCNND/ATL-MCPanel/internal/common/portguard"
	pb "github.com/ATLCNND/ATL-MCPanel/internal/proto/mcpanel"
)

// ---- frps 服务端 ----

// normalizeDisplayHost 把管理员填的「对外域名」整理成**纯主机名**。
//
// 管理员可能顺手填成 `https://mc.example.com/`、`mc.example.com:25570`
// 或者带尾随空格 —— 这里统一剥掉协议、路径、查询串与端口。
//
// **端口必须去掉**：线路级域名要给该线路上的**所有**端口复用，
// 端口是在拼装地址时才追加的；留着端口会拼出 `x.com:25570:25571`。
//
// 注意 IPv6 字面量（`fe80::1`）里本就有多个冒号，只有"恰好一个冒号
// 且后面全是数字"才当成端口剥掉，避免把 `fe80::1` 削成 `fe80:`。
func normalizeDisplayHost(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	if i := strings.IndexAny(s, "/?#"); i >= 0 {
		s = s[:i]
	}
	if strings.Count(s, ":") == 1 {
		if i := strings.LastIndex(s, ":"); i >= 0 {
			port := s[i+1:]
			if port != "" && strings.IndexFunc(port, func(r rune) bool { return r < '0' || r > '9' }) < 0 {
				s = s[:i]
			}
		}
	}
	return strings.TrimSpace(s)
}

// publicAddress 组装"玩家/外部服务实际该连的地址"。
//
// 优先级：
//  1. **隧道级** display_domain —— 管理员给某一条隧道单独配的（可含端口），最具体
//  2. **线路级** display_domain + 端口 —— 最常见：一条线路一个域名，所有端口复用
//  3. 线路 host + 端口 —— 兜底
//
// 为什么线路级这一档很重要：没有它，实例页只能给用户显示 `节点IP:端口`。
// 用户把地址填进模组配置或发给朋友，等于把节点的真实入口地址公布了。
func publicAddress(tunnelDomain, lineDomain, lineHost string, remotePort int32) string {
	if d := strings.TrimSpace(tunnelDomain); d != "" {
		return d
	}
	if d := normalizeDisplayHost(lineDomain); d != "" {
		return d + ":" + strconv.Itoa(int(remotePort))
	}
	return strings.TrimSpace(lineHost) + ":" + strconv.Itoa(int(remotePort))
}

// handleListFrps GET /api/frps
func (s *Server) handleListFrps(w http.ResponseWriter, r *http.Request) {
	if !requireAdminIn(w, r) {
		return
	}
	rows, err := s.db.Query(`
		SELECT id, name, host, bind_port, token, port_start, port_end, remark, display_domain
		FROM frps_servers ORDER BY id`)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()

	type item struct {
		ID            int64  `json:"id"`
		Name          string `json:"name"`
		Host          string `json:"host"`
		BindPort      int32  `json:"bind_port"`
		Token         string `json:"token"`
		PortStart     int32  `json:"port_start"`
		PortEnd       int32  `json:"port_end"`
		Remark        string `json:"remark"`
		DisplayDomain string `json:"display_domain"`
		UsedPorts     int32  `json:"used_ports"`
	}
	list := []item{}
	for rows.Next() {
		var it item
		if err := rows.Scan(&it.ID, &it.Name, &it.Host, &it.BindPort, &it.Token,
			&it.PortStart, &it.PortEnd, &it.Remark, &it.DisplayDomain); err != nil {
			continue
		}
		_ = s.db.QueryRow(`SELECT COUNT(*) FROM tunnels WHERE frps_id = ?`, it.ID).Scan(&it.UsedPorts)
		list = append(list, it)
	}
	writeJSON(w, http.StatusOK, list)
}

// handleCreateFrps POST /api/frps
func (s *Server) handleCreateFrps(w http.ResponseWriter, r *http.Request) {
	if !requireAdminIn(w, r) {
		return
	}
	var req struct {
		Name          string `json:"name"`
		Host          string `json:"host"`
		BindPort      int32  `json:"bind_port"`
		Token         string `json:"token"`
		PortStart     int32  `json:"port_start"`
		PortEnd       int32  `json:"port_end"`
		Remark        string `json:"remark"`
		DisplayDomain string `json:"display_domain"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "无效请求体")
		return
	}
	if req.Name == "" || req.Host == "" {
		writeErr(w, http.StatusBadRequest, "名称与地址不能为空")
		return
	}
	if req.BindPort <= 0 {
		req.BindPort = 7000
	}
	if req.PortStart <= 0 {
		req.PortStart = 25565
	}
	if req.PortEnd <= 0 || req.PortEnd < req.PortStart {
		req.PortEnd = req.PortStart + 100
	}
	domain := normalizeDisplayHost(req.DisplayDomain)

	res, err := s.db.Exec(`
		INSERT INTO frps_servers (name, host, bind_port, token, port_start, port_end, remark, display_domain)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		req.Name, req.Host, req.BindPort, req.Token, req.PortStart, req.PortEnd, req.Remark, domain)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	id, _ := res.LastInsertId()
	s.audit(r, "create_frps", req.Name, fmt.Sprintf("%s:%d domain=%s", req.Host, req.BindPort, domain))
	writeJSON(w, http.StatusCreated, map[string]interface{}{"id": id, "message": "frps 服务器已添加"})
}

// handleUpdateFrps PUT /api/frps/{id}
//
// 目前主要用来改**对外域名**（本轮新增），顺带允许改名称与备注。
// host / bind_port / token / 端口段不在这里改：它们一动，该线路上已下发的
// 所有 frpc 配置都得重写，属于"删掉重建"更安全的操作。
func (s *Server) handleUpdateFrps(w http.ResponseWriter, r *http.Request) {
	if !requireAdminIn(w, r) {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id == 0 {
		writeErr(w, http.StatusBadRequest, "id 无效")
		return
	}
	var req struct {
		Name          *string `json:"name"`
		Remark        *string `json:"remark"`
		DisplayDomain *string `json:"display_domain"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "无效请求体")
		return
	}

	var curName, curRemark, curDomain string
	if err := s.db.QueryRow(
		`SELECT name, remark, display_domain FROM frps_servers WHERE id = ?`, id).
		Scan(&curName, &curRemark, &curDomain); err != nil {
		writeErr(w, http.StatusNotFound, "线路不存在")
		return
	}
	if req.Name != nil {
		v := strings.TrimSpace(*req.Name)
		if v == "" {
			writeErr(w, http.StatusBadRequest, "名称不能为空")
			return
		}
		curName = v
	}
	if req.Remark != nil {
		curRemark = strings.TrimSpace(*req.Remark)
	}
	if req.DisplayDomain != nil {
		curDomain = normalizeDisplayHost(*req.DisplayDomain)
	}
	if len([]rune(curDomain)) > 200 {
		writeErr(w, http.StatusBadRequest, "对外域名过长")
		return
	}

	if _, err := s.db.Exec(
		`UPDATE frps_servers SET name = ?, remark = ?, display_domain = ? WHERE id = ?`,
		curName, curRemark, curDomain, id); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.audit(r, "update_frps", strconv.FormatInt(id, 10), "domain="+curDomain)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"message": "线路已保存", "display_domain": curDomain,
	})
}

// handleDeleteFrps DELETE /api/frps/{id}
func (s *Server) handleDeleteFrps(w http.ResponseWriter, r *http.Request) {
	if !requireAdminIn(w, r) {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "id 无效")
		return
	}

	// 先移除该 frps 下的所有隧道（含 Daemon 侧）
	rows, err := s.db.Query(`SELECT tunnel_id, instance_id FROM tunnels WHERE frps_id = ?`, id)
	if err == nil {
		type t struct{ tunnelID, instanceID string }
		var list []t
		for rows.Next() {
			var it t
			if err := rows.Scan(&it.tunnelID, &it.instanceID); err == nil {
				list = append(list, it)
			}
		}
		rows.Close()
		for _, it := range list {
			s.daemonRemoveTunnel(it.instanceID, it.tunnelID)
		}
	}

	_, _ = s.db.Exec(`DELETE FROM tunnels WHERE frps_id = ?`, id)
	if _, err := s.db.Exec(`DELETE FROM frps_servers WHERE id = ?`, id); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.audit(r, "delete_frps", strconv.FormatInt(id, 10), "")
	writeJSON(w, http.StatusOK, map[string]string{"message": "frps 服务器及其隧道已删除"})
}

// ---- 隧道 ----

// handleListTunnels GET /api/tunnels
func (s *Server) handleListTunnels(w http.ResponseWriter, r *http.Request) {
	if !requireAdminIn(w, r) {
		return
	}
	rows, err := s.db.Query(`
		SELECT t.id, t.tunnel_id, t.instance_id, i.name, t.frps_id, f.name, f.host,
		       t.name, t.protocol, t.local_port, t.remote_port, t.display_domain, t.status,
		       COALESCE(f.display_domain, '')
		FROM tunnels t
		LEFT JOIN instances i ON i.instance_id = t.instance_id
		LEFT JOIN frps_servers f ON f.id = t.frps_id
		ORDER BY t.id`)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()

	type item struct {
		ID            int64  `json:"id"`
		TunnelID      string `json:"tunnel_id"`
		InstanceID    string `json:"instance_id"`
		InstanceName  string `json:"instance_name"`
		FrpsID        int64  `json:"frps_id"`
		FrpsName      string `json:"frps_name"`
		FrpsHost      string `json:"frps_host"`
		Name          string `json:"name"`
		Protocol      string `json:"protocol"`
		LocalPort     int32  `json:"local_port"`
		RemotePort    int32  `json:"remote_port"`
		DisplayDomain string `json:"display_domain"` // 对外展示域名（可含端口），空则展示 IP:端口
		Status        string `json:"status"`
		LiveStatus    string `json:"live_status"`
		LiveError     string `json:"live_error"`
		PublicAddress string `json:"public_address"`
	}
	list := []item{}
	for rows.Next() {
		var it item
		var iname, fname, fhost, lineDomain sql.NullString
		if err := rows.Scan(&it.ID, &it.TunnelID, &it.InstanceID, &iname, &it.FrpsID, &fname, &fhost,
			&it.Name, &it.Protocol, &it.LocalPort, &it.RemotePort, &it.DisplayDomain, &it.Status,
			&lineDomain); err != nil {
			continue
		}
		it.InstanceName = iname.String
		it.FrpsName = fname.String
		it.FrpsHost = fhost.String
		it.PublicAddress = publicAddress(it.DisplayDomain, lineDomain.String, it.FrpsHost, it.RemotePort)
		list = append(list, it)
	}

	// 合并 Daemon 上报的实时状态
	live := s.daemonTunnelStatusByInstance()
	for i := range list {
		if st, ok := live[list[i].InstanceID+"|"+list[i].TunnelID]; ok {
			list[i].LiveStatus = st.status
			list[i].LiveError = st.err
		}
	}
	writeJSON(w, http.StatusOK, list)
}

// handleCreateTunnel POST /api/tunnels  {instance_id, frps_id, protocol, local_port?, remote_port?, name?}
func (s *Server) handleCreateTunnel(w http.ResponseWriter, r *http.Request) {
	if !requireAdminIn(w, r) {
		return
	}
	var req struct {
		InstanceID string `json:"instance_id"`
		FrpsID     int64  `json:"frps_id"`
		Protocol   string `json:"protocol"`
		LocalPort  int32  `json:"local_port"`
		RemotePort int32  `json:"remote_port"`
		Name       string `json:"name"`
		// DisplayDomain 对外展示的域名（可含端口）。管理员填写后，
		// 实例详情页的「公网域名」会同步显示它，而不是 IP:端口。
		DisplayDomain string `json:"display_domain"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "无效请求体")
		return
	}
	if req.InstanceID == "" || req.FrpsID == 0 {
		writeErr(w, http.StatusBadRequest, "instance_id 与 frps_id 必填")
		return
	}
	if req.Protocol == "" {
		req.Protocol = "tcp"
	}

	// 实例必须存在
	var instName string
	var instPort int32
	if err := s.db.QueryRow(`SELECT name, port FROM instances WHERE instance_id = ?`, req.InstanceID).
		Scan(&instName, &instPort); err != nil {
		writeErr(w, http.StatusNotFound, "实例不存在")
		return
	}
	if req.LocalPort <= 0 {
		req.LocalPort = instPort // 默认转发实例游戏端口
	}
	// 总管理员建隧道同样要过这一关：默认清单里全是"谁都不该挂公网"的端口
	// （SSH / 数据库 / 平台自身服务），放行它没有正当用途，只有手滑与事故。
	if err := portguard.Check(int(req.LocalPort), s.protectedLocalPorts()); err != nil {
		writeErr(w, http.StatusForbidden, err.Error())
		return
	}

	// frps 信息（含线路级对外域名，用于组装展示给用户的公网地址）
	var frpsHost, frpsToken, lineDomain string
	var bindPort, portStart, portEnd int32
	if err := s.db.QueryRow(`SELECT host, bind_port, token, port_start, port_end, display_domain FROM frps_servers WHERE id = ?`, req.FrpsID).
		Scan(&frpsHost, &bindPort, &frpsToken, &portStart, &portEnd, &lineDomain); err != nil {
		writeErr(w, http.StatusNotFound, "frps 服务器不存在")
		return
	}

	// 公网端口：未指定则自动分配
	if req.RemotePort <= 0 {
		// 避开设该节点上全部实例的端口：frps 与实例同机时，公网端口会和实例抢绑定
		p, err := s.allocRemotePortAvoiding(req.FrpsID, portStart, portEnd, s.remotePortsToAvoid(req.FrpsID, req.InstanceID))
		if err != nil {
			writeErr(w, http.StatusConflict, err.Error())
			return
		}
		req.RemotePort = p
	} else {
		// 校验指定端口是否在范围内且未被占用
		if req.RemotePort < portStart || req.RemotePort > portEnd {
			writeErr(w, http.StatusBadRequest, fmt.Sprintf("公网端口需在 %d-%d 范围内", portStart, portEnd))
			return
		}
		var cnt int
		_ = s.db.QueryRow(`SELECT COUNT(*) FROM tunnels WHERE frps_id = ? AND remote_port = ?`, req.FrpsID, req.RemotePort).Scan(&cnt)
		if cnt > 0 {
			writeErr(w, http.StatusConflict, "该公网端口已被其它隧道占用")
			return
		}
	}

	tunnelID := fmt.Sprintf("%s-%s-%d", req.InstanceID, req.Protocol, req.RemotePort)
	name := req.Name
	if name == "" {
		name = fmt.Sprintf("%s-%s-%d", instName, req.Protocol, req.RemotePort)
	}

	res, err := s.db.Exec(`
		INSERT INTO tunnels (tunnel_id, instance_id, frps_id, name, protocol, local_port, remote_port, display_domain, status)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, 'pending')`,
		tunnelID, req.InstanceID, req.FrpsID, name, req.Protocol, req.LocalPort, req.RemotePort, req.DisplayDomain)
	if err != nil {
		writeErr(w, http.StatusConflict, "创建隧道失败（可能已存在相同配置）")
		return
	}
	id, _ := res.LastInsertId()

	// 立即下发到 Daemon
	status, msg := s.daemonApplyTunnel(req.InstanceID, tunnelID, name, req.Protocol, req.LocalPort, req.RemotePort, req.DisplayDomain,
		frpsHost, bindPort, frpsToken)
	_, _ = s.db.Exec(`UPDATE tunnels SET status = ? WHERE id = ?`, status, id)

	s.audit(r, "create_tunnel", req.InstanceID, fmt.Sprintf("公网 %s:%d -> %d", frpsHost, req.RemotePort, req.LocalPort))
	writeJSON(w, http.StatusCreated, map[string]interface{}{
		"id":             id,
		"tunnel_id":      tunnelID,
		"remote_port":    req.RemotePort,
		"status":         status,
		"message":        msg,
		"public_address": publicAddress(req.DisplayDomain, lineDomain, frpsHost, req.RemotePort),
	})
}

// handleDeleteTunnel DELETE /api/tunnels/{id}
func (s *Server) handleDeleteTunnel(w http.ResponseWriter, r *http.Request) {
	if !requireAdminIn(w, r) {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "id 无效")
		return
	}
	var tunnelID, instanceID string
	if err := s.db.QueryRow(`SELECT tunnel_id, instance_id FROM tunnels WHERE id = ?`, id).
		Scan(&tunnelID, &instanceID); err != nil {
		writeErr(w, http.StatusNotFound, "隧道不存在")
		return
	}

	s.daemonRemoveTunnel(instanceID, tunnelID)
	if _, err := s.db.Exec(`DELETE FROM tunnels WHERE id = ?`, id); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.audit(r, "delete_tunnel", instanceID, tunnelID)
	writeJSON(w, http.StatusOK, map[string]string{"message": "隧道已删除"})
}

// handleReapplyTunnel POST /api/tunnels/{id}/reapply  重新下发到 Daemon
func (s *Server) handleReapplyTunnel(w http.ResponseWriter, r *http.Request) {
	if !requireAdminIn(w, r) {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "id 无效")
		return
	}

	var tunnelID, instanceID, name, protocol, displayDomain string
	var localPort, remotePort, frpsID int32
	if err := s.db.QueryRow(`
		SELECT tunnel_id, instance_id, name, protocol, local_port, remote_port, frps_id, display_domain
		FROM tunnels WHERE id = ?`, id).
		Scan(&tunnelID, &instanceID, &name, &protocol, &localPort, &remotePort, &frpsID, &displayDomain); err != nil {
		writeErr(w, http.StatusNotFound, "隧道不存在")
		return
	}
	var frpsHost, frpsToken string
	var bindPort int32
	if err := s.db.QueryRow(`SELECT host, bind_port, token FROM frps_servers WHERE id = ?`, frpsID).
		Scan(&frpsHost, &bindPort, &frpsToken); err != nil {
		writeErr(w, http.StatusNotFound, "frps 服务器不存在")
		return
	}

	status, msg := s.daemonApplyTunnel(instanceID, tunnelID, name, protocol, localPort, remotePort, displayDomain, frpsHost, bindPort, frpsToken)
	_, _ = s.db.Exec(`UPDATE tunnels SET status = ? WHERE id = ?`, status, id)
	writeJSON(w, http.StatusOK, map[string]string{"status": status, "message": msg})
}

// ---- 内部辅助 ----

// allocRemotePort 在 frps 的端口范围内分配一个空闲公网端口。
func (s *Server) allocRemotePort(frpsID int64, start, end int32) (int32, error) {
	return s.allocRemotePortAvoiding(frpsID, start, end, nil)
}

// allocRemotePortAvoiding 分配空闲公网端口，并尽量避开 avoid 里的端口。
//
// ---------------------------------------------------------------------------
// 为什么需要 avoid（2026-09-17 在"面板+节点+frps 同一台公网机"上实测踩到）
// ---------------------------------------------------------------------------
// 那种部署下 frps 与 Minecraft 实例在**同一台机器**上：frps 会在本机绑定 remote_port，
// 而实例自己也占着游戏端口。两者抢同一个端口号时，实例会直接
//
//	**** FAILED TO BIND TO PORT! ****
//	bind(..) failed: Address already in use
//
// 启动失败并退出 —— 表现为"实例起了一下就自己停了"，而面板上只看到 status 从
// running 变 stopped，不看服务端日志根本猜不到原因。
//
// ⚠️ avoid 必须是**该节点上全部实例的端口**，不能只有"本实例自己的"：
// 只避开自己的话，第二个实例的公网端口会分到第一个实例的游戏端口上（25565），
// 于是第一个实例反而起不来 —— 这个跨实例的坑在 2026-09-17 建第二个实例时踩到了。
//
// avoid 为 nil 表示不避开任何端口。
//
// 注意顺序：**先避开**，只有当整个端口段只剩这些时，才退而求其次用它们 ——
// 因为远程 frps 上"公网端口 == 实例端口"本来是正常且好记的用法，
// 不该为了极端情况把端口段用尽。
func (s *Server) allocRemotePortAvoiding(frpsID int64, start, end int32, avoid map[int32]bool) (int32, error) {
	used := map[int32]bool{}
	rows, err := s.db.Query(`SELECT remote_port FROM tunnels WHERE frps_id = ?`, frpsID)
	if err == nil {
		for rows.Next() {
			var p int32
			if err := rows.Scan(&p); err == nil {
				used[p] = true
			}
		}
		rows.Close()
	}
	// 第一轮：跳过已用与要避开的
	for p := start; p <= end; p++ {
		if !used[p] && !avoid[p] {
			return p, nil
		}
	}
	// 第二轮：端口段里只剩被避开的那些了，那就用它们
	// （同端口在远程 frps 上是正常的；宁可复用也不要开不出端口）
	for p := start; p <= end; p++ {
		if !used[p] {
			return p, nil
		}
	}
	return 0, fmt.Errorf("端口范围 %d-%d 已用尽", start, end)
}

// sameMachine 判断线路主机与节点是否在同一台机器上。
//
// 只看回环地址与节点 IP：线路里填 127.0.0.1 / localhost 时必然同机；
// 填成节点自己的 IP 也是同机。其余情况（域名、别的 IP）按不同机处理 ——
// 判错的代价只是少避开一个端口号，不会造成故障。
func sameMachine(lineHost, nodeIP string) bool {
	h := strings.ToLower(strings.TrimSpace(lineHost))
	switch h {
	case "", "localhost", "127.0.0.1", "::1", "[::1]":
		// 空主机名不该出现，但真出现了按同机处理更安全（宁可多避开一个端口）
		return true
	}
	return nodeIP != "" && h == strings.ToLower(strings.TrimSpace(nodeIP))
}

// remotePortsToAvoid 返回给某实例分配公网端口时应避开的**全部**端口号。
//
// 只有"线路与实例所在节点同机"时才有值 —— 远端 frps 绑端口不会和实例冲突。
// 注意返回的是该节点上**所有实例**的游戏端口，不只是这个实例自己的：
// 只避开自己的话，第二个实例的公网端口会落到第一个实例的游戏端口上，
// 把第一个实例挤掉（2026-09-17 实测）。
func (s *Server) remotePortsToAvoid(frpsID int64, instanceID string) map[int32]bool {
	var frpsHost, nodeIP string
	var nodeID int64
	err := s.db.QueryRow(`
		SELECT f.host, n.ip, n.id
		  FROM instances i
		  JOIN nodes n ON n.id = i.node_id
		  JOIN frps_servers f ON f.id = ?
		 WHERE i.instance_id = ?`, frpsID, instanceID).Scan(&frpsHost, &nodeIP, &nodeID)
	if err != nil {
		return nil
	}
	if !sameMachine(frpsHost, nodeIP) {
		return nil
	}
	return s.portsInUseOnNode(nodeID)
}

// portsInUseOnNode 返回某节点上所有实例的游戏端口。
func (s *Server) portsInUseOnNode(nodeID int64) map[int32]bool {
	out := map[int32]bool{}
	rows, err := s.db.Query(`SELECT port FROM instances WHERE node_id = ?`, nodeID)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var p int32
		if err := rows.Scan(&p); err == nil && p > 0 {
			out[p] = true
		}
	}
	return out
}

// daemonApplyTunnel 把隧道下发给 Daemon，返回 (状态, 提示信息)。
func (s *Server) daemonApplyTunnel(instanceID, tunnelID, name, protocol string, localPort, remotePort int32, displayDomain string,
	frpsHost string, frpsPort int32, frpsToken string) (string, string) {

	nodeID, ok := s.instanceNodeID(instanceID)
	if !ok {
		return "error", "实例不存在"
	}
	cli, err := s.nodes.GetClient(nodeID)
	if err != nil {
		return "error", "节点连接失败: " + err.Error()
	}
	resp, err := cli.ApplyTunnel(context.Background(), &pb.TunnelRequest{
		InstanceId:    instanceID,
		TunnelId:      tunnelID,
		Name:          name,
		Protocol:      protocol,
		LocalPort:     localPort,
		RemotePort:    remotePort,
		FrpsHost:      frpsHost,
		FrpsPort:      frpsPort,
		FrpsToken:     frpsToken,
		Enabled:       true,
		DisplayDomain: displayDomain,
	})
	if err != nil {
		return "error", "下发失败: " + err.Error()
	}
	if !resp.Success {
		return "error", resp.Error
	}
	// 状态以 **Daemon 的实时结果**为准，而不是一律写 "running"。
	//
	// 一律写 "running" 是有问题的：第 16 项修完之后，"给已停止的实例开端口/重新下发"
	// 是**只登记、不启动**的 —— 那时写 running 就是假话。而这个值会
	//
	//	① 存进 tunnels.status，
	//	② 原样返回给前端（界面上的提示、以及 live_status 拿不到时的回退显示）
	//
	// 于是"节点不可达"会被显示成"运行中"。多问一次 Daemon 就能拿到真话
	// （下发是低频的管理动作，这一次额外往返可以接受）。
	return s.daemonTunnelLiveStatus(instanceID, tunnelID), resp.Message
}

// daemonTunnelLiveStatus 取某条隧道在 Daemon 侧的实时状态；拿不到就返回 "unknown"。
//
// "unknown" 是**刻意**的一个取值：它表示"问不到"，而不是"没在跑"。
// 前端据此显示"未知（节点不可达）"，不再回退到数据库里那个"最后一次下发结果"。
func (s *Server) daemonTunnelLiveStatus(instanceID, tunnelID string) string {
	nodeID, ok := s.instanceNodeID(instanceID)
	if !ok {
		return "unknown"
	}
	cli, err := s.nodes.GetClient(nodeID)
	if err != nil {
		return "unknown"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	resp, err := cli.ListTunnels(ctx, &pb.ListTunnelsRequest{InstanceId: instanceID})
	if err != nil {
		return "unknown"
	}
	for _, t := range resp.Tunnels {
		if t.TunnelId == tunnelID {
			if t.Status == "" {
				return "unknown"
			}
			return t.Status
		}
	}
	return "unknown"
}

// daemonRemoveTunnel 通知 Daemon 移除隧道（忽略错误）。
func (s *Server) daemonRemoveTunnel(instanceID, tunnelID string) {
	nodeID, ok := s.instanceNodeID(instanceID)
	if !ok {
		return
	}
	cli, err := s.nodes.GetClient(nodeID)
	if err != nil {
		return
	}
	_, _ = cli.RemoveTunnel(context.Background(), &pb.TunnelRequest{
		InstanceId: instanceID,
		TunnelId:   tunnelID,
	})
}

type tunnelLiveStatus struct {
	status string
	err    string
}

// daemonTunnelStatusByInstance 汇总各实例的隧道实时状态（key: instanceID|tunnelID）。
func (s *Server) daemonTunnelStatusByInstance() map[string]tunnelLiveStatus {
	out := map[string]tunnelLiveStatus{}

	rows, err := s.db.Query(`SELECT DISTINCT instance_id FROM tunnels`)
	if err != nil {
		return out
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()

	for _, id := range ids {
		nodeID, ok := s.instanceNodeID(id)
		if !ok {
			continue
		}
		cli, err := s.nodes.GetClient(nodeID)
		if err != nil {
			continue
		}
		resp, err := cli.ListTunnels(context.Background(), &pb.ListTunnelsRequest{InstanceId: id})
		if err != nil {
			continue
		}
		for _, t := range resp.Tunnels {
			out[id+"|"+t.TunnelId] = tunnelLiveStatus{status: t.Status, err: t.Error}
		}
	}
	return out
}
