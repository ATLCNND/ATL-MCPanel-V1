package httpapi

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/ATLCNND/ATL-MCPanel/internal/common/portguard"
)

// ============================================================================
// 实例的公网端口（对所有能看到该实例的人可见）
// ============================================================================

// instancePortView 一个对外暴露的端口。
//
// 字段是**刻意挑过的**：这里给普通用户看，绝不能带 frps token、
// 节点地址、Daemon 端口这类运维信息。对外地址也优先用管理员配置的
// display_domain，没有才回退到 line_host:port。
type instancePortView struct {
	ID         int64  `json:"id"`
	TunnelID   string `json:"tunnel_id"`
	Name       string `json:"name"`
	Protocol   string `json:"protocol"`
	LocalPort  int32  `json:"local_port"`
	RemotePort int32  `json:"remote_port"`
	// LineName 线路名（如「华东一区」），仅展示用
	LineName string `json:"line_name"`
	// PublicAddress 玩家/外部服务实际该连的地址：
	// 有 display_domain 用它，否则用 line_host:remote_port
	PublicAddress string `json:"public_address"`
	Status        string `json:"status"`
	LiveStatus    string `json:"live_status"`
	LiveError     string `json:"live_error"`
	// CanEdit 当前用户能否增删/改这条端口
	// （总管理员 / 该节点的节点用户 / 本实例的 owner 级用户）
	CanEdit bool `json:"can_edit"`
}

// handleListInstancePorts GET /api/instances/{id}/ports
//
// 权限放到 viewer：**普通用户也需要知道自己的实例开了哪些公网端口** ——
// 多端口模组（BlueMap 网页、Geyser 基岩版、Votifier 等）配好后，
// 用户得把地址填进配置或告诉朋友。此前这些信息只有管理员在「穿透管理」里看得到，
// 用户只能去问管理员。
func (s *Server) handleListInstancePorts(w http.ResponseWriter, r *http.Request) {
	instanceID := r.PathValue("id")
	if !s.requireInstanceLevel(w, r, instanceID, LevelViewer) {
		return
	}

	rows, err := s.db.Query(`
		SELECT t.id, t.tunnel_id, t.name, t.protocol, t.local_port, t.remote_port,
		       COALESCE(f.name, ''), COALESCE(f.host, ''), t.display_domain, t.status, t.frps_id,
		       COALESCE(f.display_domain, '')
		FROM tunnels t
		LEFT JOIN frps_servers f ON f.id = t.frps_id
		WHERE t.instance_id = ?
		ORDER BY t.remote_port`, instanceID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()

	canEdit := s.canManageInstancePorts(currentUserID(r), roleOf(r), instanceID)

	list := []instancePortView{}
	for rows.Next() {
		var v instancePortView
		var lineHost, displayDomain, lineDomain string
		var frpsID int64
		if err := rows.Scan(&v.ID, &v.TunnelID, &v.Name, &v.Protocol, &v.LocalPort, &v.RemotePort,
			&v.LineName, &lineHost, &displayDomain, &v.Status, &frpsID, &lineDomain); err != nil {
			continue
		}
		// 隧道级域名 > 线路级域名 > IP:端口（见 publicAddress 的注释：
		// 兜底那档会把节点真实入口暴露给用户，所以管理员应尽量给线路配上域名）
		v.PublicAddress = publicAddress(displayDomain, lineDomain, lineHost, v.RemotePort)
		v.CanEdit = canEdit
		list = append(list, v)
	}

	// 合并 Daemon 上报的实时状态（隧道是不是真的在跑）
	live := s.daemonTunnelStatusByInstance()
	for i := range list {
		if st, ok := live[instanceID+"|"+list[i].TunnelID]; ok {
			list[i].LiveStatus = st.status
			list[i].LiveError = st.err
		}
	}

	// 顺带告诉前端两件事，避免"点了才被拒"：
	//   - remaining：还能再开几个（-1 = 不限）
	//   - lines：本实例能用哪些线路、各还剩多少
	//
	// 两者都按**本实例配额归属者**计算，与 handleAddInstancePort 的扣费口径一致。
	// 不能复用 /api/my/ports —— 那个返回的是**操作者**的配额，
	// 而这里的开销记在**实例归属者**头上（节点用户替别人的实例开端口时两者不同），
	// 拿操作者的列表填表单会出现"下拉框显示还剩 3 个，一提交却说配额不足"。
	type portLine struct {
		FrpsID    int64  `json:"frps_id"`
		FrpsName  string `json:"frps_name"`
		FrpsHost  string `json:"frps_host"`
		Quota     int    `json:"quota"`
		Used      int    `json:"used"`
		Available int    `json:"available"`
		Unlimited bool   `json:"unlimited"`
	}

	lines := []portLine{}
	remaining := -1 // -1 = 不限
	chargingSelf := true

	if roleOf(r) == RoleAdmin {
		// 总管理员：所有线路都可用且不限量
		if qrows, err := s.db.Query(`SELECT id, name, host FROM frps_servers ORDER BY id`); err == nil {
			for qrows.Next() {
				var l portLine
				if err := qrows.Scan(&l.FrpsID, &l.FrpsName, &l.FrpsHost); err == nil {
					l.Unlimited = true
					l.Quota = -1
					lines = append(lines, l)
				}
			}
			qrows.Close()
		}
	} else {
		ownerID := s.instanceQuotaOwner(instanceID)
		if ownerID == 0 {
			// 追溯不到归属者（v17 之前的老实例）→ 退回按操作者算，与扣费口径一致
			ownerID = currentUserID(r)
		}
		chargingSelf = ownerID == currentUserID(r)
		usage := s.portUsage(ownerID)
		remaining = 0
		if qrows, err := s.db.Query(`
			SELECT np.frps_id, COALESCE(f.name, '(已删除线路)'), COALESCE(f.host, ''), np.quota
			FROM node_user_ports np
			LEFT JOIN frps_servers f ON f.id = np.frps_id
			WHERE np.user_id = ?
			ORDER BY np.frps_id`, ownerID); err == nil {
			for qrows.Next() {
				var l portLine
				if err := qrows.Scan(&l.FrpsID, &l.FrpsName, &l.FrpsHost, &l.Quota); err == nil {
					l.Used = usage[l.FrpsID]
					l.Available = maxInt(0, l.Quota-l.Used)
					remaining += l.Available
					lines = append(lines, l)
				}
			}
			qrows.Close()
		}
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"ports":     list,
		"can_edit":  canEdit,
		"remaining": remaining,
		"lines":     lines,
		// 扣的是不是操作者自己的配额。不是的话前端要说明"扣的是实例归属者的"，
		// 否则节点用户会以为在消耗自己的配额。
		"charging_self": chargingSelf,
	})
}

// handleAddInstancePort POST /api/instances/{id}/ports
//
// body: { frps_id, local_port, protocol? }
//
// 权限：总管理员 / 该节点的节点用户 / 本实例的 owner 级用户（见
// canManageInstancePorts 的注释 —— 开端口是"实例自己的事"，比删除宽松一级）。
func (s *Server) handleAddInstancePort(w http.ResponseWriter, r *http.Request) {
	instanceID := r.PathValue("id")
	if !s.canManageInstancePorts(currentUserID(r), roleOf(r), instanceID) {
		writeErr(w, http.StatusForbidden, "仅总管理员、该节点的节点用户，或本实例的 owner 可开通公网端口")
		return
	}
	var req struct {
		FrpsID    int64  `json:"frps_id"`
		LocalPort int32  `json:"local_port"`
		Protocol  string `json:"protocol"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "无效请求体")
		return
	}
	if req.FrpsID == 0 || req.LocalPort <= 0 {
		writeErr(w, http.StatusBadRequest, "请选择线路并填写本地端口")
		return
	}
	// 本地端口安全检查：不许把节点自己的服务挂到公网。
	//
	// 这一条是**安全边界**，不是输入校验：frpc 以 root 运行、且生成的配置固定
	// `localAddr = "127.0.0.1:<local_port>"`，所以填 22 就等于把节点的 SSH
	// 公开到全网（实测此前确实放行）。
	if err := portguard.Check(int(req.LocalPort), s.protectedLocalPorts()); err != nil {
		writeErr(w, http.StatusForbidden, err.Error())
		return
	}
	if req.Protocol == "" {
		req.Protocol = "tcp"
	}
	if req.Protocol != "tcp" && req.Protocol != "udp" {
		writeErr(w, http.StatusBadRequest, "协议只支持 tcp / udp")
		return
	}

	var instName string
	var instPort int32
	if err := s.db.QueryRow(`SELECT name, port FROM instances WHERE instance_id = ?`, instanceID).
		Scan(&instName, &instPort); err != nil {
		writeErr(w, http.StatusNotFound, "实例不存在")
		return
	}

	// 配额校验（总管理员不限）
	//
	// 扣的是**本实例配额归属者**的配额，而不是操作者本人的：
	//   - 归属者自己开端口时，两者是同一个人（最常见的情况）
	//   - 节点用户替别人的实例开端口时，若扣操作者的，
	//     那条隧道的用量永远不会出现在任何人的账上
	//     （用量是按实例归属者推导的，见 portUsage），配额就形同虚设
	if roleOf(r) != RoleAdmin {
		ownerID := s.instanceQuotaOwner(instanceID)
		if ownerID == 0 {
			// 追溯不到归属者（v17 之前建的老实例），退回扣操作者，行为与改动前一致
			ownerID = currentUserID(r)
		}
		var quota int
		if err := s.db.QueryRow(
			`SELECT quota FROM node_user_ports WHERE user_id = ? AND frps_id = ?`, ownerID, req.FrpsID).
			Scan(&quota); err != nil {
			if ownerID == currentUserID(r) {
				writeErr(w, http.StatusForbidden,
					"你还没有该线路的端口配额，请联系总管理员分配")
			} else {
				writeErr(w, http.StatusForbidden,
					"该实例的归属者还没有该线路的端口配额，请联系总管理员分配")
			}
			return
		}
		used := s.portUsage(ownerID)[req.FrpsID]
		if used >= quota {
			writeErr(w, http.StatusConflict,
				"该实例归属者在「该线路」的端口配额已用尽（配额 "+strconv.Itoa(quota)+
					"）——删掉不再需要的端口或联系总管理员加配额")
			return
		}
	}

	var frpsHost, frpsToken, lineDomain string
	var bindPort, portStart, portEnd int32
	if err := s.db.QueryRow(`SELECT host, bind_port, token, port_start, port_end, display_domain FROM frps_servers WHERE id = ?`, req.FrpsID).
		Scan(&frpsHost, &bindPort, &frpsToken, &portStart, &portEnd, &lineDomain); err != nil {
		writeErr(w, http.StatusNotFound, "线路不存在")
		return
	}

	// 避开设该节点上**全部实例**的端口：frps 与节点同机时，公网端口会和实例抢绑定
	// （见 allocRemotePortAvoiding 的注释）
	remote, err := s.allocRemotePortAvoiding(req.FrpsID, portStart, portEnd, s.remotePortsToAvoid(req.FrpsID, instanceID))
	if err != nil {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}

	tunnelID := instanceID + "-" + req.Protocol + "-" + strconv.Itoa(int(remote))
	name := instName + "-" + req.Protocol + "-" + strconv.Itoa(int(remote))
	if _, err := s.db.Exec(`
		INSERT INTO tunnels (tunnel_id, instance_id, frps_id, name, protocol, local_port, remote_port, status)
		VALUES (?, ?, ?, ?, ?, ?, ?, 'pending')`,
		tunnelID, instanceID, req.FrpsID, name, req.Protocol, req.LocalPort, remote); err != nil {
		writeErr(w, http.StatusConflict, "创建隧道失败（可能已存在相同配置）")
		return
	}
	status, msg := s.daemonApplyTunnel(instanceID, tunnelID, name, req.Protocol, req.LocalPort, remote, "",
		frpsHost, bindPort, frpsToken)
	_, _ = s.db.Exec(`UPDATE tunnels SET status = ? WHERE tunnel_id = ?`, status, tunnelID)

	s.audit(r, "add_instance_port", instanceID,
		"公网 "+frpsHost+":"+strconv.Itoa(int(remote))+" -> "+strconv.Itoa(int(req.LocalPort)))
	writeJSON(w, http.StatusCreated, map[string]interface{}{
		"tunnel_id":      tunnelID,
		"remote_port":    remote,
		"status":         status,
		"message":        msg,
		"public_address": publicAddress("", lineDomain, frpsHost, remote),
	})
}

// handleUpdateInstancePort POST /api/instances/{id}/ports/{tunnel_id}
//
// body: { local_port, protocol? }
//
// 只允许改**本地端口**（也就是"这个公网口子转发到实例内部的哪个端口"）。
// 公网端口本身不给改：它一旦公布出去就有人配好了，改它等于把别人的配置改坏；
// 要换公网端口就删了重开。
func (s *Server) handleUpdateInstancePort(w http.ResponseWriter, r *http.Request) {
	instanceID := r.PathValue("id")
	tunnelID := r.PathValue("tunnel_id")
	if !s.canManageInstancePorts(currentUserID(r), roleOf(r), instanceID) {
		writeErr(w, http.StatusForbidden, "仅总管理员、该节点的节点用户，或本实例的 owner 可修改公网端口")
		return
	}
	var req struct {
		LocalPort int32  `json:"local_port"`
		Protocol  string `json:"protocol"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "无效请求体")
		return
	}
	if req.LocalPort <= 0 || req.LocalPort > 65535 {
		writeErr(w, http.StatusBadRequest, "本地端口需为 1 ~ 65535 之间的整数")
		return
	}
	// 与新建时同一条安全边界：改端口同样不能指向节点自己的服务
	if err := portguard.Check(int(req.LocalPort), s.protectedLocalPorts()); err != nil {
		writeErr(w, http.StatusForbidden, err.Error())
		return
	}

	var frpsID, remotePort int32
	var protocol, displayDomain, name string
	if err := s.db.QueryRow(`
		SELECT frps_id, remote_port, protocol, display_domain, name FROM tunnels
		WHERE tunnel_id = ? AND instance_id = ?`, tunnelID, instanceID).
		Scan(&frpsID, &remotePort, &protocol, &displayDomain, &name); err != nil {
		writeErr(w, http.StatusNotFound, "该实例下没有这条端口")
		return
	}
	if req.Protocol != "" {
		if req.Protocol != "tcp" && req.Protocol != "udp" {
			writeErr(w, http.StatusBadRequest, "协议只支持 tcp / udp")
			return
		}
		protocol = req.Protocol
	}

	var frpsHost, frpsToken string
	var bindPort int32
	if err := s.db.QueryRow(`SELECT host, bind_port, token FROM frps_servers WHERE id = ?`, frpsID).
		Scan(&frpsHost, &bindPort, &frpsToken); err != nil {
		writeErr(w, http.StatusNotFound, "线路不存在")
		return
	}

	if _, err := s.db.Exec(
		`UPDATE tunnels SET local_port = ?, protocol = ? WHERE tunnel_id = ?`,
		req.LocalPort, protocol, tunnelID); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	// 改完立刻重新下发，否则 frpc 里还是旧配置（要等下次重启才生效）
	status, msg := s.daemonApplyTunnel(instanceID, tunnelID, name, protocol, req.LocalPort, remotePort, displayDomain,
		frpsHost, bindPort, frpsToken)
	_, _ = s.db.Exec(`UPDATE tunnels SET status = ? WHERE tunnel_id = ?`, status, tunnelID)

	s.audit(r, "update_instance_port", instanceID,
		tunnelID+" 本地端口 "+strconv.Itoa(int(req.LocalPort)))
	writeJSON(w, http.StatusOK, map[string]string{"message": msg, "status": status})
}

// handleDeleteInstancePort DELETE /api/instances/{id}/ports/{tunnel_id}
func (s *Server) handleDeleteInstancePort(w http.ResponseWriter, r *http.Request) {
	instanceID := r.PathValue("id")
	tunnelID := r.PathValue("tunnel_id")
	if !s.canManageInstancePorts(currentUserID(r), roleOf(r), instanceID) {
		writeErr(w, http.StatusForbidden, "仅总管理员、该节点的节点用户，或本实例的 owner 可删除公网端口")
		return
	}
	res, err := s.db.Exec(`DELETE FROM tunnels WHERE tunnel_id = ? AND instance_id = ?`, tunnelID, instanceID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		writeErr(w, http.StatusNotFound, "该实例下没有这条端口")
		return
	}
	s.daemonRemoveTunnel(instanceID, tunnelID)
	s.audit(r, "delete_instance_port", instanceID, tunnelID)
	writeJSON(w, http.StatusOK, map[string]string{"message": "端口已关闭"})
}
