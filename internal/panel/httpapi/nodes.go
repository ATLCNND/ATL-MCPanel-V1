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
	CPU       int    `json:"cpu"`
	Mem       int64  `json:"mem"`
	LastSeen  string `json:"last_seen"`
	Instances int    `json:"instances"`
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
		var lastSeen sql.NullString
		if err := rows.Scan(&v.ID, &v.Name, &v.IP, &v.SSHUser, &v.SSHPort, &auth, &v.Status, &v.CPU, &v.Mem, &lastSeen); err != nil {
			continue
		}
		v.HasAuth = auth.String != ""
		v.LastSeen = lastSeen.String
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
		PanelAddress: s.grpcPublicAddress(),
		DaemonBinary: binData,
		GRPCListen:   s.daemonGRPCListen,
		ServiceName:  s.daemonServiceName,
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
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"message": res.Message,
		"steps":   res.Steps,
	})
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
	port := "9090"
	if i := strings.LastIndex(s.listenAddrOfGRPC, ":"); i >= 0 {
		if p := strings.TrimSpace(s.listenAddrOfGRPC[i+1:]); p != "" {
			port = p
		}
	}
	return host + ":" + port
}
