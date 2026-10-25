package httpapi

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net"
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
	// AuthType 凭证类型：password / key / 空（按内容自动判断，老行为）。
	AuthType string `json:"auth_type"`
	SSHPort  int    `json:"ssh_port"`
	// PanelGRPCPort 该节点连面板用的 gRPC 端口（0 = 用面板全局配置的端口）。
	// 面板的 9090 被改过端口映射、或多面板共存时用得上。
	PanelGRPCPort int `json:"panel_grpc_port"`
	// DaemonGRPCPort 该节点 Daemon 自己的 gRPC 监听端口（0 = 用面板全局配置）。
	// 同一台机器上跑多个节点，或与别的服务端口冲突时需要改。
	DaemonGRPCPort int `json:"daemon_grpc_port"`
	// PublicIP 节点的**公网地址**（给玩家用的那一个）。
	//
	// 与 IP 分开的原因：IP 是"面板/运维怎么连这台机器"，常是内网或 127.0.0.1；
	// 公网直出的线路地址必须等于这个值，面板才能判断"这条直连线路属于哪个节点"。
	PublicIP string `json:"public_ip"`
}

// normalizeAuthType 把请求里的凭证类型收敛到数据库允许的取值。
//
// 只认 password / key，其余（含空串）= "" 表示"按内容自动判断"（老行为）。
// 不做大小写宽容：这个字段是我们自己的前端填的，写错就是 bug，早暴露比晚暴露好。
func normalizeAuthType(t string) string {
	switch strings.TrimSpace(t) {
	case nodeinstall.AuthPassword:
		return nodeinstall.AuthPassword
	case nodeinstall.AuthKey:
		return nodeinstall.AuthKey
	default:
		return ""
	}
}

// firstNonZero 取第一个非 0 值（用于「没填就用默认」的端口字段）。
func firstNonZero(vals ...int) int {
	for _, v := range vals {
		if v != 0 {
			return v
		}
	}
	return 0
}

// nodeView 节点列表项（不含 SSH 凭据）。
type nodeView struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	IP        string `json:"ip"`
	PublicIP  string `json:"public_ip"`
	SSHUser   string `json:"ssh_user"`
	SSHPort   int    `json:"ssh_port"`
	HasAuth   bool   `json:"has_auth"`
	// AuthType 当前凭据是密码还是密钥（空 = 升级前登记的老节点，自动判断）
	AuthType string `json:"auth_type"`
	// 两个可覆盖端口（0 = 用面板全局配置）
	PanelGRPCPort  int `json:"panel_grpc_port"`
	DaemonGRPCPort int `json:"daemon_grpc_port"`
	// EnrollMode 接入方式（迁移 v31）：界面据此决定哪些操作与提示才有意义
	EnrollMode string `json:"enroll_mode"`
	// NodeUID 节点的稳定身份（迁移 v32，Daemon 首次启动生成）。老节点为空。
	NodeUID string `json:"node_uid"`
	Status     string `json:"status"`
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
	rows, err := s.db.Query(`SELECT id, name, ip, COALESCE(public_ip,''), ssh_user, ssh_port, ssh_auth,
		COALESCE(ssh_auth_type,''), COALESCE(panel_grpc_port,0), COALESCE(grpc_port,0),
		COALESCE(enroll_mode,''), COALESCE(node_uid,''), status, cpu, mem, last_seen FROM nodes ORDER BY id`)
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
		if err := rows.Scan(&v.ID, &v.Name, &v.IP, &v.PublicIP, &v.SSHUser, &v.SSHPort, &auth,
			&v.AuthType, &v.PanelGRPCPort, &v.DaemonGRPCPort, &v.EnrollMode, &v.NodeUID,
			&v.Status, &v.CPU, &v.Mem, &lastSeen); err != nil {
			continue
		}
		v.HasAuth = auth.String != ""
		v.LastSeenAgeS = -1
		if lastSeen != nil {
			v.LastSeen = lastSeen.Format("2006-01-02 15:04:05")
			v.LastSeenAgeS = int(time.Since(*lastSeen).Seconds())
		}
		v.Online = nodeOnline(v.Status, lastSeen)
		list = append(list, v)
	}
	// 逐节点统计实例数必须等 rows 关掉之后再做：在遍历 *sql.Rows 的同时发新查询
	// 是"占着一条连接再要一条"，池一小就互相等死（2026-10-01 面板整体卡死就是这个形状）。
	// 正确写法见 filejobs.go 的 pumpJobs：收集 → Close → 再逐条处理。
	// 显式 Close 之后 defer 的那次是个空操作，留着只作保险。
	rows.Close()
	for i := range list {
		_ = s.db.QueryRow(`SELECT COUNT(*) FROM instances WHERE node_id = ?`, list[i].ID).
			Scan(&list[i].Instances)
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

	// 凭据形式在**保存前**就校验：别让用户先保存、再点部署、再读一条模糊报错。
	if err := nodeinstall.ValidateAuth(req.AuthType, req.SSHAuth); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	res, err := s.db.Exec(
		`INSERT INTO nodes (name, ip, public_ip, ssh_user, ssh_auth, ssh_auth_type, ssh_port, grpc_port,
		                   panel_grpc_port, status)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, 'offline')`,
		req.Name, req.IP, strings.TrimSpace(req.PublicIP), req.SSHUser, req.SSHAuth,
		normalizeAuthType(req.AuthType), req.SSHPort,
		firstNonZero(req.DaemonGRPCPort, s.daemonPortFromListen()),
		firstNonZero(req.PanelGRPCPort, 0))
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
		if err := nodeinstall.ValidateAuth(req.AuthType, req.SSHAuth); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
		sets = append(sets, "ssh_auth = ?")
		args = append(args, req.SSHAuth)
	}
	// 类型可以单独改（例如把老的"自动判断"改成显式的 key/password），
	// 但没给凭据就没法校验 —— 只在这两种显式类型之间切换时才允许单独提交。
	if req.AuthType != "" {
		if req.SSHAuth == "" {
			if err := nodeinstall.ValidateAuth(req.AuthType, ""); err != nil {
				writeErr(w, http.StatusBadRequest, "改凭证类型时请同时重新填写该类型的凭据")
				return
			}
		}
		sets = append(sets, "ssh_auth_type = ?")
		args = append(args, normalizeAuthType(req.AuthType))
	}
	if req.SSHPort > 0 {
		sets = append(sets, "ssh_port = ?")
		args = append(args, req.SSHPort)
	}
	// public_ip 允许被清空（改成空串），所以用"字段是否出现在请求里"判断，
	// 而不是"是否非空" —— 否则想把填错的公网 IP 清掉都做不到。
	if req.PublicIP != "" {
		sets = append(sets, "public_ip = ?")
		args = append(args, strings.TrimSpace(req.PublicIP))
	}
	// 端口覆盖：显式给了才改（0 = 交回面板默认）
	if req.PanelGRPCPort != 0 {
		sets = append(sets, "panel_grpc_port = ?")
		args = append(args, req.PanelGRPCPort)
	}
	if req.DaemonGRPCPort != 0 {
		sets = append(sets, "grpc_port = ?")
		args = append(args, req.DaemonGRPCPort)
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

// handleRevokeNodeCredential POST /api/nodes/{id}/revoke
//
// 撤销该节点的凭证交换授权：把它的 enrollment 标为已撤销，并**删除节点记录**。
//
// 与「移除凭据」的区别：那个只清 SSH 凭据、节点照常工作；这个是**彻底切断**——
// 节点下一次心跳会被 PermissionDenied 拒绝（面板侧判据：node_uid 有撤销记录，或节点行已不存在），
// 于是「我删掉节点，它还能不能连回来」的答案是：不能，最迟 10 秒。
//
// 不依赖面板能连到节点（撤销必须在这种场景下也生效），见 grpcapi/nodeRevoked。
func (s *Server) handleRevokeNodeCredential(w http.ResponseWriter, r *http.Request) {
	if !requireAdminIn(w, r) {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "id 无效")
		return
	}
	uid := s.nodeUIDOf(id)
	if uid != "" {
		if _, err := s.db.Exec(`UPDATE node_enrollments SET revoked_at = CURRENT_TIMESTAMP, reason = '管理员撤销'
			WHERE node_uid = ? AND revoked_at IS NULL`, uid); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	if _, err := s.db.Exec(`DELETE FROM nodes WHERE id = ?`, id); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.audit(r, "revoke_node_credential", strconv.FormatInt(id, 10), "uid="+uid)
	writeJSON(w, http.StatusOK, map[string]string{
		"message": "已撤销并删除该节点；它下一次心跳会被拒绝（最迟 10 秒）",
	})
}

// handleClearNodeCredential DELETE /api/nodes/{id}/credential
//
// 把节点上保存的 SSH 凭据**清掉**（密码/私钥与类型一起），节点本身保留。
//
// 为什么需要：SSH 凭据目前是明文存库（底账 S1）。用户想停止用"面板推部署"时，
// 现在只能删掉整个节点再重新登记（那会丢掉实例归属、公网地址、端口覆盖等配置）。
// 清掉凭据之后：节点照常靠 mTLS/心跳工作，只是「探测 / 部署 / 重启 Daemon / 看日志」
// 这些走 SSH 的功能会明确报"未配置 SSH 凭据"，而不是留着一把不该留的钥匙。
func (s *Server) handleClearNodeCredential(w http.ResponseWriter, r *http.Request) {
	if !requireAdminIn(w, r) {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "id 无效")
		return
	}
	res, err := s.db.Exec(`UPDATE nodes SET ssh_auth = '', ssh_auth_type = '' WHERE id = ?`, id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		writeErr(w, http.StatusNotFound, "节点不存在")
		return
	}
	s.audit(r, "clear_node_credential", strconv.FormatInt(id, 10), "SSH 凭据已清除")
	writeJSON(w, http.StatusOK, map[string]string{
		"message": "已移除保存的 SSH 凭据（探测/部署等需要 SSH 的功能将不可用）",
	})
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
	err := s.db.QueryRow(`SELECT id, name, ip, COALESCE(public_ip,''), ssh_user, ssh_port, ssh_auth,
		COALESCE(ssh_auth_type,''), COALESCE(panel_grpc_port,0), COALESCE(grpc_port,0)
		FROM nodes WHERE id = ?`, id).
		Scan(&v.ID, &v.Name, &v.IP, &v.PublicIP, &v.SSHUser, &v.SSHPort, &auth,
			&v.AuthType, &v.PanelGRPCPort, &v.DaemonGRPCPort)
	if err != nil {
		return nil, "", fmt.Errorf("节点不存在")
	}
	return &v, auth.String, nil
}

// handleTestNodeAuth POST /api/nodes/test —— 用表单里的地址与凭据**当场试连一次**。
//
// 为什么要这个接口：登记节点是目前唯一"填错了要等到点部署才发现"的地方，
// 而报错还往往是模糊的 auth 失败。给协作者用的时候，先在表单里点一下「测试连接」，
// 通不过就不用保存 —— 省掉来回猜的几轮。
func (s *Server) handleTestNodeAuth(w http.ResponseWriter, r *http.Request) {
	if !requireAdminIn(w, r) {
		return
	}
	var req nodeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "无效请求体")
		return
	}
	req.IP = strings.TrimSpace(req.IP)
	if req.IP == "" {
		writeErr(w, http.StatusBadRequest, "节点地址不能为空")
		return
	}
	if req.SSHUser == "" {
		req.SSHUser = "root"
	}
	if req.SSHPort <= 0 {
		req.SSHPort = 22
	}
	if err := nodeinstall.ValidateAuth(req.AuthType, req.SSHAuth); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	cli, err := nodeinstall.Dial(nodeinstall.Options{
		Host: req.IP, Port: req.SSHPort, User: req.SSHUser,
		Auth: req.SSHAuth, AuthType: normalizeAuthType(req.AuthType),
	})
	if err != nil {
		writeErr(w, http.StatusBadGateway, "连接失败："+err.Error())
		return
	}
	defer cli.Close()

	probe, err := cli.Probe(s.remoteInstallDir())
	if err != nil {
		writeErr(w, http.StatusBadGateway, "已连上，但探测失败："+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"message": "连接成功",
		"probe":   probe,
		"ready":   probe.HasSystemd && probe.FreeDiskMB > 200,
	})
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
		Host: nv.IP, Port: nv.SSHPort, User: nv.SSHUser, Auth: auth, AuthType: nv.AuthType,
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
		PanelAddress: s.grpcAddressForNode(nv),
		DaemonBinary: binData,
		// 按**这个节点**算监听地址：同机绑回环、跨机绑该节点的地址。
		// 直接把配置里的 `:9091` 下发的话，每台节点的管理口都会监听所有网卡
		//（2026-10-02 安全审查发现内测节点的 9091 从公网可达就是这个原因）。
		// 该节点自己的 Daemon 端口（nodes.grpc_port，0/缺省时用面板配置）
		GRPCListen: s.daemonListenForWithPort(nv.IP, nv.DaemonGRPCPort),
		ServiceName:  s.daemonServiceName,
		// frpc 就在面板二进制旁边（面板包 bin/ 里带着）——有就一并下发，
		// 让"给实例开公网端口"开箱可用；没有时跳过（穿透是可选能力）。
		FrpcBinary: readSiblingBinary(binPath, "frpc"),
	}

	// 启用 mTLS 时为新节点签发证书
	if s.grpcMTLS && s.ca != nil {
		// 证书里带上 node_uid（URI SAN）：显示名可改，身份不能跟着改。
		// 老节点没有 node_uid 时该参数为空，签发结果与以前完全一致。
		certPEM, keyPEM, err := s.ca.IssueClientCertWithUID(nv.Name, s.nodeUIDOf(nv.ID), 90*24*time.Hour)
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
	// 找"与面板二进制同目录"的 daemon。名字在 2026-10-12 由 dsh-daemon 改成
	// atl-daemon，这里**先新名、再退回旧名**：手工部署（自己把二进制拷进 bin/）
	// 或只换了面板二进制的场景下，bin/ 里可能只有旧名那一份，而旧名在新装里
	// 只是个软链接 —— 两条路都能走通。
	dir := filepath.Dir(exe)
	for _, name := range []string{"atl-daemon", "dsh-daemon"} {
		p := filepath.Join(dir, name)
		if _, err := os.Stat(p); err == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("未找到同目录的 atl-daemon（%s/atl-daemon），请在配置中指定 server.daemon_binary", dir)
}

// readSiblingBinary 读"与某个二进制同目录"的可选文件（读不到就返回 nil）。
//
// 用途：面板包在 bin/ 里同时带着 atl-daemon 与 frpc，一键部署要把 frpc 也下发。
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

// daemonListenFor 返回**该节点上** Daemon 应该绑定的 gRPC 监听地址。
//
// 为什么要按节点算，而不是把配置里的值原样下发（2026-10-02 安全审查）：
// `server.daemon_grpc_listen` 的默认值是 `":9091"` —— 也就是**所有网卡**。
// 一键部署会把它写进节点配置，于是每台节点的 Daemon 管理口都摆在公网上
//（虽然要求 mTLS，但没有任何理由多开这个面）。实测内测节点上 `:::9091`
// 从公网可达，就是因为这个默认值。
//
// 规则（既修掉默认值，又不覆盖管理员的显式选择）：
//   - 配置里写的是**具体某个非回环地址**（如 `10.0.0.5:9091`）→ 原样使用，
//     管理员明确指定了绑定哪块网卡，那是他的决定。
//   - 否则：同机节点绑 `127.0.0.1:<端口>`（面板也是从回环连它的），
//     跨机节点绑 `<节点IP>:<端口>` —— 只暴露在面板要连的那一个地址上。
//   - 拿不到节点 IP 时退回 `:<端口>`（所有网卡），**并记一条告警**：
//     这仍然比"部署失败"好，但值得让人看见。
func (s *Server) daemonListenFor(nodeIP string) string {
	port := s.daemonPortFromListen()
	cfg := strings.TrimSpace(s.daemonGRPCListen)

	// 配置里显式写了一个非回环的具体地址：尊重它
	if cfg != "" && !strings.HasPrefix(cfg, ":") {
		host := cfg
		if i := strings.LastIndex(host, ":"); i >= 0 {
			host = strings.Trim(host[:i], "[]")
		}
		if host != "" && !isLoopbackHost(host) {
			return cfg
		}
	}

	if isLocalNodeHost(nodeIP) {
		// 注意 isLocalNodeHost 把**空地址**也当作本机，这是有意的：
		// 拿不到节点 IP 时绑回环是安全的一侧（宁可部署后发现连不上，
		// 也不要把管理口摆到所有网卡上）。
		return "127.0.0.1:" + strconv.Itoa(port)
	}
	return net.JoinHostPort(strings.TrimSpace(nodeIP), strconv.Itoa(port))
}

// isLoopbackHost 判断主机名/地址是否只表示回环。
func isLoopbackHost(host string) bool {
	switch strings.ToLower(strings.TrimSpace(host)) {
	case "127.0.0.1", "::1", "localhost":
		return true
	}
	return false
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

// grpcAddressForNode 该节点该连哪个「面板 gRPC 地址」。
//
// 与 grpcAddressFor 的区别：优先用节点自己的 panel_grpc_port 覆盖值。
// 面板的 9090 被端口映射改过、同一台机器上跑多个面板、或不同网络环境的节点
// 需要不同入口时，改单个节点即可，不必动全局配置（那会牵连所有节点）。
func (s *Server) grpcAddressForNode(nv *nodeView) string {
	addr := s.grpcAddressFor(nv.IP)
	if nv.PanelGRPCPort <= 0 {
		return addr
	}
	// 只换端口部分（地址可能是 host:port，也可能是 https://host 这类）
	if i := strings.LastIndex(addr, ":"); i >= 0 && !strings.Contains(addr[i:], "/") {
		return addr[:i+1] + strconv.Itoa(nv.PanelGRPCPort)
	}
	return addr
}

// daemonListenForWithPort 同 daemonListenFor，但端口可被节点覆盖。
func (s *Server) daemonListenForWithPort(nodeIP string, port int) string {
	if port <= 0 {
		return s.daemonListenFor(nodeIP)
	}
	addr := s.daemonListenFor(nodeIP)
	if i := strings.LastIndex(addr, ":"); i >= 0 {
		return addr[:i+1] + strconv.Itoa(port)
	}
	return ":" + strconv.Itoa(port)
}

// nodeUIDOf 读某节点的 node_uid（可能为空：升级前的老节点）。
func (s *Server) nodeUIDOf(id int64) string {
	var uid string
	_ = s.db.QueryRow(`SELECT COALESCE(node_uid,'') FROM nodes WHERE id = ?`, id).Scan(&uid)
	return uid
}
