package httpapi

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ATLCNND/ATL-MCPanel/internal/panel/nodeinstall"
)

// nodeRequest 节点登记/更新请求。
type nodeRequest struct {
	Name    string `json:"name"`
	IP      string `json:"ip"`
	SSHUser string `json:"ssh_user"`
	SSHAuth string `json:"ssh_auth"`
	SSHPort int    `json:"ssh_port"`
}

// nodeView 节点列表项（不含 SSH 凭据）。
type nodeView struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	IP        string `json:"ip"`
	SSHUser   string `json:"ssh_user"`
	SSHPort   int    `json:"ssh_port"`
	HasAuth   bool   `json:"has_auth"`
	Status    string `json:"status"`
	// Online 界面该用的"在线/离线"：按心跳新鲜度算出来的（见 nodestatus.go）。
	// status 只是数据库里的原始列——Daemon 失联时它**不会**自己变成 offline，
	// 只看它会把"已经掉线 4 分钟的节点"显示成在线。
	Online   bool   `json:"online"`
	CPU      int    `json:"cpu"`
	Mem      int64  `json:"mem"`
	LastSeen string `json:"last_seen"`
	// LastSeenAgeS 心跳距今秒数（-1 = 从未上报）：排查"到底断了多久"时最直观
	LastSeenAgeS int `json:"last_seen_age_s"`
	Instances    int `json:"instances"`
}

// handleListNodes GET /api/nodes
func (s *Server) handleListNodes(w http.ResponseWriter, r *http.Request) {
	if !requireAdminIn(w, r) {
		return
	}
	rows, err := s.db.Query(`SELECT id, name, ip, ssh_user, ssh_port, ssh_auth, status, cpu, mem, last_seen FROM nodes ORDER BY id`)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()

	list := []nodeView{}
	for rows.Next() {
		var v nodeView
		var auth sql.NullString
		// last_seen 直接扫成时间（与 monitor.go / scheduler.go 一致）。
		// 早先这里扫的是字符串，结果只能原样丢给前端 —— 判不了新鲜度，
		// 也就没法回答"这个节点到底断了多久"。
		var lastSeen *time.Time
		if err := rows.Scan(&v.ID, &v.Name, &v.IP, &v.SSHUser, &v.SSHPort, &auth, &v.Status, &v.CPU, &v.Mem, &lastSeen); err != nil {
			continue
		}
		v.HasAuth = auth.String != ""
		v.LastSeenAgeS = -1
		if lastSeen != nil {
			v.LastSeen = lastSeen.Format("2006-01-02 15:04:05")
			v.LastSeenAgeS = int(time.Since(*lastSeen).Seconds())
		}
		v.Online = nodeOnline(v.Status, lastSeen)
		_ = s.db.QueryRow(`SELECT COUNT(*) FROM instances WHERE node_id = ?`, v.ID).Scan(&v.Instances)
		list = append(list, v)
	}
	writeJSON(w, http.StatusOK, list)
}

// handleCreateNode POST /api/nodes
func (s *Server) handleCreateNode(w http.ResponseWriter, r *http.Request) {
	if !requireAdminIn(w, r) {
		return
	}
	var req nodeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "无效请求体")
		return
	}
	if req.Name == "" || req.IP == "" {
		writeErr(w, http.StatusBadRequest, "name 与 ip 必填")
		return
	}
	if req.SSHPort <= 0 {
		req.SSHPort = 22
	}
	if req.SSHUser == "" {
		req.SSHUser = "root"
	}

	var exists int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM nodes WHERE name = ?`, req.Name).Scan(&exists)
	if exists > 0 {
		writeErr(w, http.StatusConflict, "节点名已存在")
		return
	}

	res, err := s.db.Exec(
		`INSERT INTO nodes (name, ip, ssh_user, ssh_auth, ssh_port, grpc_port, status) VALUES (?, ?, ?, ?, ?, ?, 'offline')`,
		req.Name, req.IP, req.SSHUser, req.SSHAuth, req.SSHPort, s.daemonPortFromListen())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	id, _ := res.LastInsertId()
	s.audit(r, "create_node", req.Name, req.IP)
	writeJSON(w, http.StatusCreated, map[string]interface{}{
		"id":      id,
		"message": "节点已登记，可执行一键部署",
	})
}

// handleUpdateNode PUT /api/nodes/{id}
func (s *Server) handleUpdateNode(w http.ResponseWriter, r *http.Request) {
	if !requireAdminIn(w, r) {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "id 无效")
		return
	}
	var req nodeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "无效请求体")
		return
	}

	sets := []string{}
	args := []interface{}{}
	if req.Name != "" {
		sets = append(sets, "name = ?")
		args = append(args, req.Name)
	}
	if req.IP != "" {
		sets = append(sets, "ip = ?")
		args = append(args, req.IP)
	}
	if req.SSHUser != "" {
		sets = append(sets, "ssh_user = ?")
		args = append(args, req.SSHUser)
	}
	if req.SSHAuth != "" {
		sets = append(sets, "ssh_auth = ?")
		args = append(args, req.SSHAuth)
	}
	if req.SSHPort > 0 {
		sets = append(sets, "ssh_port = ?")
		args = append(args, req.SSHPort)
	}
	if len(sets) == 0 {
		writeErr(w, http.StatusBadRequest, "没有需要更新的字段")
		return
	}
	args = append(args, id)
	if _, err := s.db.Exec(`UPDATE nodes SET `+strings.Join(sets, ", ")+` WHERE id = ?`, args...); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	// 节点地址变化后需重建连接
	s.nodes.Invalidate(id)
	s.audit(r, "update_node", strconv.FormatInt(id, 10), "")
	writeJSON(w, http.StatusOK, map[string]string{"message": "节点已更新"})
}

// handleDeleteNode DELETE /api/nodes/{id}
func (s *Server) handleDeleteNode(w http.ResponseWriter, r *http.Request) {
	if !requireAdminIn(w, r) {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "id 无效")
		return
	}
	var cnt int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM instances WHERE node_id = ?`, id).Scan(&cnt)
	if cnt > 0 {
		writeErr(w, http.StatusConflict, fmt.Sprintf("该节点下仍有 %d 个实例，请先迁移或删除", cnt))
		return
	}
	if _, err := s.db.Exec(`DELETE FROM nodes WHERE id = ?`, id); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.nodes.Invalidate(id)
	s.audit(r, "delete_node", strconv.FormatInt(id, 10), "")
	writeJSON(w, http.StatusOK, map[string]string{"message": "节点已删除"})
}

// loadNodeSSH 读取节点 SSH 信息。
func (s *Server) loadNodeSSH(id int64) (*nodeView, string, error) {
	var v nodeView
	var auth sql.NullString
	err := s.db.QueryRow(`SELECT id, name, ip, ssh_user, ssh_port, ssh_auth FROM nodes WHERE id = ?`, id).
		Scan(&v.ID, &v.Name, &v.IP, &v.SSHUser, &v.SSHPort, &auth)
	if err != nil {
		return nil, "", fmt.Errorf("节点不存在")
	}
	return &v, auth.String, nil
}

// handleProbeNode POST /api/nodes/{id}/probe
//
// 探测节点环境（架构、systemd、已安装状态、磁盘空间），便于部署前确认。
func (s *Server) handleProbeNode(w http.ResponseWriter, r *http.Request) {
	if !requireAdminIn(w, r) {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "id 无效")
		return
	}
	nv, auth, err := s.loadNodeSSH(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	if auth == "" {
		writeErr(w, http.StatusBadRequest, "该节点未配置 SSH 凭据")
		return
	}

	cli, err := nodeinstall.Dial(nodeinstall.Options{
		Host: nv.IP, Port: nv.SSHPort, User: nv.SSHUser, Auth: auth,
	})
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	defer cli.Close()

	probe, err := cli.Probe(s.remoteInstallDir())
	if err != nil {
		writeErr(w, http.StatusBadGateway, "探测失败: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"node":  nv.Name,
		"probe": probe,
		"ready": probe.HasSystemd && probe.FreeDiskMB > 200,
	})
}

// handleDeployNode POST /api/nodes/{id}/deploy
//
// 一键部署：签发节点证书 → 上传二进制/配置/证书 → 安装 systemd 单元并启动。
func (s *Server) handleDeployNode(w http.ResponseWriter, r *http.Request) {
	if !requireAdminIn(w, r) {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "id 无效")
		return
	}
	nv, auth, err := s.loadNodeSSH(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	if auth == "" {
		writeErr(w, http.StatusBadRequest, "该节点未配置 SSH 凭据，无法自动部署")
		return
	}

	// 读取要下发的 Daemon 二进制
	binPath, err := s.daemonBinaryPath()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	binData, err := os.ReadFile(binPath)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "读取 Daemon 二进制失败: "+err.Error())
		return
	}

	opts := nodeinstall.Options{
		Host:         nv.IP,
		Port:         nv.SSHPort,
		User:         nv.SSHUser,
		Auth:         auth,
		RemoteDir:    s.remoteInstallDir(),
		NodeID:       nv.Name,
		PanelAddress: s.grpcAddressFor(nv.IP),
		DaemonBinary: binData,
		GRPCListen:   s.daemonGRPCListen,
		ServiceName:  s.daemonServiceName,
		// frpc 就在面板二进制旁边（面板包 bin/ 里带着）——有就一并下发，
		// 让"给实例开公网端口"开箱可用；没有时跳过（穿透是可选能力）。
		FrpcBinary: readSiblingBinary(binPath, "frpc"),
	}

	// 启用 mTLS 时为新节点签发证书
	if s.grpcMTLS && s.ca != nil {
		certPEM, keyPEM, err := s.ca.IssueClientCert(nv.Name)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "签发节点证书失败: "+err.Error())
			return
		}
		opts.CACert = s.ca.CertPEM
		opts.ClientCert = certPEM
		opts.ClientKey = keyPEM
	}

	cli, err := nodeinstall.Dial(opts)
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	defer cli.Close()

	res, err := cli.Deploy(opts)
	if err != nil {
		s.audit(r, "deploy_node_failed", nv.Name, err.Error())
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	// 端口/地址变化后重建连接
	s.nodes.Invalidate(id)
	// 记录该节点的 Daemon gRPC 端口（部署时已按此端口配置）
	if p := s.daemonPortFromListen(); p > 0 {
		_, _ = s.db.Exec(`UPDATE nodes SET grpc_port = ? WHERE id = ?`, p, id)
	}
	s.audit(r, "deploy_node", nv.Name, strings.Join(res.Steps, " → "))
	resp := map[string]interface{}{
		"message": res.Message,
		"steps":   res.Steps,
	}
	// 远程节点 + 面板 gRPC 只监听回环 = 这个节点**永远连不上来**：
	// 部署本身会成功（SSH 通、服务能起），但心跳/注册全失败，
	// 于是「节点监控」显示离线、而实例操控却一切正常 —— 极难归因。
	// 与其让用户去猜，不如在这里直接说清楚。
	if !isLocalNodeHost(nv.IP) && s.grpcListenIsLoopback() {
		resp["warning"] = "面板的 gRPC 只监听回环（" + s.listenAddrOfGRPC + "），" +
			"而 " + nv.IP + " 是另一台机器 —— 它连不上面板，节点会一直显示离线（实例操控不受影响）。" +
			"请把 config.yaml 的 server.grpc_listen 改成 0.0.0.0:" + s.grpcPort() +
			"（并用防火墙只放行节点 IP），或改用同机部署。"
		s.logger.Warn("部署远程节点，但面板 gRPC 只监听回环",
			"node", nv.Name, "ip", nv.IP, "grpc_listen", s.listenAddrOfGRPC)
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleRestartDaemon POST /api/nodes/{id}/daemon/restart
func (s *Server) handleRestartDaemon(w http.ResponseWriter, r *http.Request) {
	if !requireAdminIn(w, r) {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "id 无效")
		return
	}
	nv, auth, err := s.loadNodeSSH(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	cli, err := nodeinstall.Dial(nodeinstall.Options{Host: nv.IP, Port: nv.SSHPort, User: nv.SSHUser, Auth: auth})
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	defer cli.Close()

	out, err := cli.Run("systemctl restart atlmcpanel-daemon && sleep 2 && systemctl is-active atlmcpanel-daemon")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, fmt.Sprintf("重启失败: %s", out))
		return
	}
	s.nodes.Invalidate(id)
	s.audit(r, "restart_daemon", nv.Name, strings.TrimSpace(out))
	writeJSON(w, http.StatusOK, map[string]string{"message": "Daemon 已重启", "status": strings.TrimSpace(out)})
}

// handleDaemonLogs GET /api/nodes/{id}/daemon/logs?lines=100
func (s *Server) handleDaemonLogs(w http.ResponseWriter, r *http.Request) {
	if !requireAdminIn(w, r) {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "id 无效")
		return
	}
	lines := 100
	if v := r.URL.Query().Get("lines"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 2000 {
			lines = n
		}
	}
	nv, auth, err := s.loadNodeSSH(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	cli, err := nodeinstall.Dial(nodeinstall.Options{Host: nv.IP, Port: nv.SSHPort, User: nv.SSHUser, Auth: auth})
	if err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	defer cli.Close()

	out, _ := cli.Run(fmt.Sprintf("journalctl -u atlmcpanel-daemon --no-pager -n %d 2>&1", lines))
	writeJSON(w, http.StatusOK, map[string]string{"node": nv.Name, "logs": out})
}

// ---- 内部辅助 ----

// remoteInstallDir 节点上的安装目录。
func (s *Server) remoteInstallDir() string {
	if s.remoteDir != "" {
		return s.remoteDir
	}
	return "/opt/mcpanel"
}

// daemonPortFromListen 从配置的 Daemon gRPC 监听地址解析端口。
func (s *Server) daemonPortFromListen() int {
	addr := s.daemonGRPCListen
	if addr == "" {
		return 9091
	}
	i := strings.LastIndex(addr, ":")
	if i < 0 {
		return 9091
	}
	p, err := strconv.Atoi(strings.TrimSpace(addr[i+1:]))
	if err != nil || p <= 0 {
		return 9091
	}
	return p
}

// daemonBinaryPath 返回要下发到节点的 Daemon 二进制路径。
func (s *Server) daemonBinaryPath() (string, error) {
	if s.daemonBinary != "" {
		return s.daemonBinary, nil
	}
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("无法定位面板二进制: %w", err)
	}
	p := filepath.Join(filepath.Dir(exe), "dsh-daemon")
	if _, err := os.Stat(p); err != nil {
		return "", fmt.Errorf("未找到同目录的 dsh-daemon（%s），请在配置中指定 server.daemon_binary", p)
	}
	return p, nil
}

// readSiblingBinary 读"与某个二进制同目录"的可选文件（读不到就返回 nil）。
//
// 用途：面板包在 bin/ 里同时带着 dsh-daemon 与 frpc，一键部署要把 frpc 也下发。
// 读不到**不算错误** —— 老包或裁剪过的包没有 frpc，穿透只是不可用而已，
// 不该让整次部署失败。
func readSiblingBinary(refPath, name string) []byte {
	p := filepath.Join(filepath.Dir(refPath), name)
	b, err := os.ReadFile(p)
	if err != nil || len(b) == 0 {
		return nil
	}
	return b
}

// grpcAddressFor 返回**这个节点**该用来连面板 gRPC 的地址。
//
// 为什么要按节点区分（2026-09-30 实测踩到）：面板的 gRPC 默认只监听回环
//（`grpc_listen: 127.0.0.1:9090`，gRPC 是**明文 + 仅 CA 校验节点证书**的管理口，
// 不该直接暴露公网）。如果节点就是面板本机，却把 external_url 推导出来的
// 公网域名交给它，Daemon 会一直 `connect: connection refused`：
//
//   - 面板 → 节点的 gRPC（9091）照常可用，所以**实例操控一切正常**；
//   - 节点 → 面板的心跳/注册全失败，于是「节点监控」显示离线、
//     `node_offline` 告警一直挂着 —— 一个看起来自相矛盾、极难归因的现象。
//
// 同机节点（ip 是回环 / localhost / 本机名）一律给回环地址。
func (s *Server) grpcAddressFor(nodeIP string) string {
	if isLocalNodeHost(nodeIP) {
		return "127.0.0.1:" + s.grpcPort()
	}
	return s.grpcPublicAddress()
}

// isLocalNodeHost 这个节点地址是不是"面板自己这台机器"。
func isLocalNodeHost(host string) bool {
	h := strings.ToLower(strings.TrimSpace(host))
	switch h {
	case "127.0.0.1", "::1", "localhost", "":
		return true
	}
	// 本机名（如 HK8194921.local）：面板与节点同机部署时，一键部署默认填的就是它
	if name, err := os.Hostname(); err == nil && name != "" {
		if h == strings.ToLower(name) {
			return true
		}
		if short := strings.SplitN(strings.ToLower(name), ".", 2)[0]; h == short {
			return true
		}
	}
	return false
}

// grpcPort 面板 gRPC 实际监听的端口（取 grpc_listen 的端口部分）。
func (s *Server) grpcPort() string {
	if i := strings.LastIndex(s.listenAddrOfGRPC, ":"); i >= 0 {
		if p := strings.TrimSpace(s.listenAddrOfGRPC[i+1:]); p != "" {
			return p
		}
	}
	return "9090"
}

// grpcListenIsLoopback 面板的 gRPC 是否**只**监听回环。
//
// 刻意不复用 isLocalNodeHost：这里问的是"监听地址覆盖面"，
// `:9090` 与 `0.0.0.0:9090` 都表示"所有网卡"（远程节点连得上），
// 而空主机名在 isLocalNodeHost 里会被当成"本机" —— 那样会得出反的结论。
func (s *Server) grpcListenIsLoopback() bool {
	host := s.listenAddrOfGRPC
	if i := strings.LastIndex(host, ":"); i >= 0 {
		host = host[:i]
	}
	host = strings.Trim(strings.TrimSpace(host), "[]")
	switch strings.ToLower(host) {
	case "127.0.0.1", "::1", "localhost":
		return true
	}
	return false
}

// grpcPublicAddress 返回节点用于连接面板 gRPC 的地址。
func (s *Server) grpcPublicAddress() string {
	if s.grpcPublicAddr != "" {
		return s.grpcPublicAddr
	}
	// 从 external_url 推导主机，端口沿用 grpc_listen
	host := "127.0.0.1"
	if u := s.externalURL; u != "" {
		trimmed := u
		if i := strings.Index(trimmed, "://"); i >= 0 {
			trimmed = trimmed[i+3:]
		}
		if i := strings.IndexAny(trimmed, "/:"); i >= 0 {
			trimmed = trimmed[:i]
		}
		if trimmed != "" {
			host = trimmed
		}
	}
	return host + ":" + s.grpcPort()
}
