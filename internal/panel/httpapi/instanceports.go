package httpapi

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/ATLCNND/ATL-MCPanel/internal/common/portguard"
	"github.com/ATLCNND/ATL-MCPanel/internal/frp"
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
	//
	// 只问**本实例**：下面的合并只用 instanceID 开头的 key，而原来那个
	// "全表所有实例"的扇出意味着一个 viewer 的请求会让面板向所有租户的
	// 实例逐个发 gRPC —— 既没有收益，又是个跨租户的放大器。
	live := s.daemonTunnelStatusByInstance(r.Context(), []string{instanceID})
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
	// portLineTunnel 第三方线路上一条可绑定的隧道。
	// 必须先于 portLine 声明（同一函数内的类型在被引用时已经可见）。
	type portLineTunnel struct {
		TunnelID   string `json:"tunnel_id"`
		Name       string `json:"name"`
		RemotePort int32  `json:"remote_port"`
		Protocol   string `json:"protocol"`
	}

	type portLine struct {
		FrpsID    int64  `json:"frps_id"`
		FrpsName  string `json:"frps_name"`
		FrpsHost  string `json:"frps_host"`
		Quota     int    `json:"quota"`
		Used      int    `json:"used"`
		Available int    `json:"available"`
		Unlimited bool   `json:"unlimited"`
		// LineType 线路类型（self_hosted / third_party / direct）。
		// 前端据此把"这条线路对本实例是否可用"讲清楚，而不是等提交后报错。
		LineType string `json:"line_type"`
		// Usable 这条线路对**本实例**能不能用；Reason 说明为什么不能用。
		//
		// 公网直出的地址就是某个节点的公网 IP：给别的节点上的实例选它，
		// 玩家会连到另一台机器 —— 表现为"端口开了但连不上"（用户 2026-10-24 指出）。
		Usable bool   `json:"usable"`
		Reason string `json:"reason"`
		// Tunnels 第三方线路上「可绑定」的隧道（已导入、尚未绑实例）。
		//
		// 第三方线路的远端端口是服务商分配的固定值，不能像自建那样从端口段里
		// 新分配一个。所以界面必须先让用户看到能选哪几条、各自的固定端口是多少，
		// 再把 tunnel_id 一起提交（否则后端只能报「请选择要绑定的隧道」）。
		Tunnels []portLineTunnel `json:"tunnels,omitempty"`
	}

	lines := []portLine{}
	remaining := -1 // -1 = 不限
	chargingSelf := true

	if roleOf(r) == RoleAdmin {
		// 总管理员：所有线路都可用且不限量
		if qrows, err := s.db.Query(`SELECT id, name, host, COALESCE(line_type,'self_hosted')
			FROM frps_servers ORDER BY id`); err == nil {
			for qrows.Next() {
				var l portLine
				if err := qrows.Scan(&l.FrpsID, &l.FrpsName, &l.FrpsHost, &l.LineType); err == nil {
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
		// 注意：这条是**非管理员**分支 —— 不返回线路的 host。
		//
		// 与 handleMyPorts（nodeusers.go）同一条政策：创建实例的表单会在"线路"
		// 那一行显示 host，等于把公网服务器的真实地址交给使用者。用户只需要知道
		// 线路**名字**和还剩几个端口，host 是运维信息（管理员看「穿透管理」即可）。
		// 这里直接不查它，而不是查出来再清空 —— 少一份能泄露的数据。
		// 字段本身保留在 JSON 里（空串），否则前端的 frps_host 会变成 undefined。
		if qrows, err := s.db.Query(`
			SELECT np.frps_id, COALESCE(f.name, '(已删除线路)'), np.quota,
			       COALESCE(f.line_type,'self_hosted')
			FROM node_user_ports np
			LEFT JOIN frps_servers f ON f.id = np.frps_id
			WHERE np.user_id = ?
			ORDER BY np.frps_id`, ownerID); err == nil {
			for qrows.Next() {
				var l portLine
				if err := qrows.Scan(&l.FrpsID, &l.FrpsName, &l.Quota, &l.LineType); err == nil {
					l.Used = usage[l.FrpsID]
					l.Available = maxInt(0, l.Quota-l.Used)
					remaining += l.Available
					lines = append(lines, l)
				}
			}
			qrows.Close()
		}
	}

	// 逐条判定「对本实例是否可用」：直连线路要与实例所在节点的公网地址一致，
	// 不一致就明确标不可用并给出原因 —— 而不是让用户提交后才看到一句「不支持」。
	var nodePublicIP, nodeIP string
	if id, ok := s.instanceNodeID(instanceID); ok {
		_ = s.db.QueryRow(`SELECT COALESCE(public_ip,''), COALESCE(ip,'') FROM nodes WHERE id = ?`, id).
			Scan(&nodePublicIP, &nodeIP)
	}
	nodeAddr := nodePublicIP
	if nodeAddr == "" {
		nodeAddr = nodeIP
	}
	for i := range lines {
		lines[i].Usable = true
		if frp.NormalizeLineType(lines[i].LineType) == frp.LineThirdParty {
			if trows, err := s.db.Query(`SELECT tunnel_id, COALESCE(name,''), COALESCE(remote_port,0), COALESCE(protocol,'tcp')
				FROM tunnels WHERE frps_id = ? AND remote_port > 0
				AND (instance_id IS NULL OR instance_id = '') ORDER BY id`, lines[i].FrpsID); err == nil {
				for trows.Next() {
					var pt portLineTunnel
					if err := trows.Scan(&pt.TunnelID, &pt.Name, &pt.RemotePort, &pt.Protocol); err == nil {
						lines[i].Tunnels = append(lines[i].Tunnels, pt)
					}
				}
				trows.Close()
			}
			if len(lines[i].Tunnels) == 0 {
				lines[i].Usable = false
				lines[i].Reason = "这条线路上没有可绑定的隧道了（都已分配，或还没导入配置）"
			}
		}
		if frp.NormalizeLineType(lines[i].LineType) == frp.LineDirect {
			if nodeAddr == "" || lines[i].FrpsHost != nodeAddr {
				lines[i].Usable = false
				lines[i].Reason = "该实例所在节点不支持这条公网直连线路（线路地址 " +
					lines[i].FrpsHost + "，节点对外地址 " + orDash(nodeAddr) + "）"
			}
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
// addPortReq 「给实例开一个公网端口」的请求体。
type addPortReq struct {
	FrpsID    int64  `json:"frps_id"`
	LocalPort int32  `json:"local_port"`
	Protocol  string `json:"protocol"`
	// TunnelID 第三方线路专用：把**已导入的那条隧道**绑到本实例上。
	//
	// 为什么第三方不能像自建那样"从端口段分配一个新端口"：
	// 端口与代理名都是服务商分配的，面板只能挑一条现成的绑上去。
	// 留空则自动挑第一条未绑定的。
	TunnelID string `json:"tunnel_id"`
}

func (s *Server) handleAddInstancePort(w http.ResponseWriter, r *http.Request) {
	instanceID := r.PathValue("id")
	if !s.canManageInstancePorts(currentUserID(r), roleOf(r), instanceID) {
		writeErr(w, http.StatusForbidden, "仅总管理员、该节点的节点用户，或本实例的 owner 可开通公网端口")
		return
	}
	var req addPortReq
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

	// 先看线路类型：第三方单隧道与公网直出**不走**端口段分配，也不扣端口配额。
	//
	//	third_party —— 端口与代理名是服务商分配的，面板只能"选一条已导入的隧道"绑上来；
	//	               从端口段里再分配一个根本不存在。
	//	direct      —— 远端就是节点自己，端口 = 本地端口，同样没有"分配"这回事。
	// 这两类若还按自建那套走，就会出现"配额不足 / 分配不到端口"这种假故障。
	var lineType, authModeOfLine, userOfLine string
	{
		if err := s.db.QueryRow(
			`SELECT COALESCE(line_type,'self_hosted'), COALESCE(auth_mode,'auth'), COALESCE(user,'')
			 FROM frps_servers WHERE id = ?`, req.FrpsID).
			Scan(&lineType, &authModeOfLine, &userOfLine); err != nil {
			writeErr(w, http.StatusNotFound, "线路不存在")
			return
		}
	}
	isThirdParty := frp.NormalizeLineType(lineType) == frp.LineThirdParty
	isDirect := frp.NormalizeLineType(lineType) == frp.LineDirect

	// 配额校验（总管理员不限；第三方/直连不占我们的端口池，跳过）
	//
	// 扣的是**本实例配额归属者**的配额，而不是操作者本人的：
	//   - 归属者自己开端口时，两者是同一个人（最常见的情况）
	//   - 节点用户替别人的实例开端口时，若扣操作者的，
	//     那条隧道的用量永远不会出现在任何人的账上
	//     （用量是按实例归属者推导的，见 portUsage），配额就形同虚设
	if roleOf(r) != RoleAdmin && !isThirdParty && !isDirect {
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

	// ---- 第三方单隧道：把一条**已导入的**隧道绑到本实例上 ----
	if isThirdParty {
		s.bindImportedTunnel(w, r, instanceID, instName, req, frpsHost, bindPort, frpsToken, lineDomain)
		return
	}

	// ---- 公网直出：远端就是本地端口，不分配 ----
	//
	// 但先要确认"这条直连线路确实属于**本实例所在的节点**"：
	// 直连线路的地址就是某个节点的公网 IP，给别的节点上的实例选它，
	// 玩家人会连到另一台机器上 —— 表现为"端口开了但连不上"。
	if isDirect {
		var nodePublicIP, nodeIP string
		var nodeID int64
		if id, ok := s.instanceNodeID(instanceID); ok {
			nodeID = id
			_ = s.db.QueryRow(`SELECT COALESCE(public_ip,''), COALESCE(ip,'') FROM nodes WHERE id = ?`, nodeID).
				Scan(&nodePublicIP, &nodeIP)
		}
		usable := nodePublicIP
		if usable == "" {
			usable = nodeIP
		}
		if usable == "" || frpsHost != usable {
			writeErr(w, http.StatusConflict, fmt.Sprintf(
				"该实例所在节点不支持这条公网直连线路：线路地址是 %s，而节点对外地址是 %s。"+
					"公网直出只能用「实例所在节点自己的」那条线路（先在节点管理里补全该节点的公网 IP）",
				frpsHost, orDash(usable)))
			return
		}
	}
	remote := req.LocalPort
	if !isDirect {
		// 自建：避开设该节点上**全部实例**的端口（frps 与节点同机时公网端口会和实例抢绑定）
		var err error
		remote, err = s.allocRemotePortAvoiding(req.FrpsID, portStart, portEnd, s.remotePortsToAvoid(req.FrpsID, instanceID))
		if err != nil {
			writeErr(w, http.StatusConflict, err.Error())
			return
		}
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
	// 第三方线路的隧道是**导入来的固定资源**（名字、远端端口、transport 参数都在
	// 这一行里）。删掉整行 = 把服务商给的那份配置一起丢了，之后既绑不回原实例、
	// 也分配不到别的实例（用户 2026-10-24 反馈）。
	// 所以这类「关闭端口」只做**解绑**：清掉归属与本地端口，状态回到 imported。
	var lineType string
	_ = s.db.QueryRow(`SELECT COALESCE(f.line_type,'self_hosted') FROM tunnels t
		LEFT JOIN frps_servers f ON f.id = t.frps_id
		WHERE t.tunnel_id = ? AND t.instance_id = ?`, tunnelID, instanceID).Scan(&lineType)
	isThirdParty := frp.NormalizeLineType(lineType) == frp.LineThirdParty

	var res sql.Result
	var err error
	if isThirdParty {
		res, err = s.db.Exec(`UPDATE tunnels SET instance_id = '', status = 'imported'
			WHERE tunnel_id = ? AND instance_id = ?`, tunnelID, instanceID)
	} else {
		res, err = s.db.Exec(`DELETE FROM tunnels WHERE tunnel_id = ? AND instance_id = ?`, tunnelID, instanceID)
	}
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
	msg := "端口已关闭"
	if isThirdParty {
		msg = "已解除绑定（隧道仍在该线路下，可分配给其它实例）"
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": msg})
}

// orDash 空串时返回一个占位符，避免提示里出现「节点对外地址是 」这种断句。
func orDash(s string) string {
	if s == "" {
		return "（未登记）"
	}
	return s
}

// bindImportedTunnel 把第三方线路里**已导入的一条隧道**绑到本实例上。
//
// 为什么不是新建一条：第三方隧道的名字 / 远端端口 / transport 参数都是
// 服务商分配的，新建意味着要重新造一套 —— 造出来的服务端不认（实测回
// 「FRPC 配置文件错误」）。所以这里的语义是**绑定**：更新那条隧道行的
// instance_id 与 local_port，其余字段原样保留。
func (s *Server) bindImportedTunnel(w http.ResponseWriter, r *http.Request, instanceID, instName string,
	req addPortReq, frpsHost string, bindPort int32, frpsToken, lineDomain string) {
	s.bindTunnelToInstance(w, r, instanceID, req.FrpsID, req.TunnelID, req.LocalPort, frpsHost, bindPort, frpsToken, lineDomain)
}

// bindTunnelToInstance 是上面那个的实现，参数是显式的，便于管理端（/api/tunnels）复用。
func (s *Server) bindTunnelToInstance(w http.ResponseWriter, r *http.Request, instanceID string,
	frpsID int64, wantTunnelID string, localPort int32, frpsHost string, bindPort int32, frpsToken, lineDomain string) {

	// 必须**显式**指定要绑定哪条隧道，不再自动挑第一条。
	//
	// 为什么：以前 tunnel_id 为空就自动挑一条未绑定的，而表单里实例选择框默认停在
	// 第一个实例上 —— 结果是「导入配置后随便点一下，线路就自动挂到了某个实例」，
	// 用户既没选过隧道也没选过实例（2026-10-24 用户反馈）。
	// 第三方线路的每条隧道都是一份服务商分配的固定资源，绑错要付出代价。
	tunnelID := strings.TrimSpace(wantTunnelID)
	if tunnelID == "" {
		writeErr(w, http.StatusBadRequest,
			"请选择要绑定的隧道（第三方线路的端口由服务商分配，需要明确指定绑哪一条）")
		return
	}

	var tName, tProto string
	var tLocal, tRemote int32
	if err := s.db.QueryRow(`SELECT COALESCE(name,''), COALESCE(protocol,'tcp'), local_port, remote_port
		FROM tunnels WHERE tunnel_id = ? AND frps_id = ?`, tunnelID, frpsID).
		Scan(&tName, &tProto, &tLocal, &tRemote); err != nil {
		writeErr(w, http.StatusNotFound, "该线路下没有这条隧道")
		return
	}
	if tRemote <= 0 {
		writeErr(w, http.StatusConflict, "这条隧道没有服务商分配的远端端口，无法绑定")
		return
	}

	// 绑定：只改归属与本地端口，其余（名字/端口/transport）原样保留
	if _, err := s.db.Exec(`UPDATE tunnels SET instance_id = ?, local_port = ?, protocol = ?, status = 'pending'
		WHERE tunnel_id = ? AND frps_id = ?`,
		instanceID, localPort, tProto, tunnelID, frpsID); err != nil {
		writeErr(w, http.StatusInternalServerError, "绑定隧道失败："+err.Error())
		return
	}

	status, msg := s.daemonApplyTunnel(instanceID, tunnelID, tName, tProto, localPort, tRemote, "",
		frpsHost, bindPort, frpsToken)
	_, _ = s.db.Exec(`UPDATE tunnels SET status = ? WHERE tunnel_id = ?`, status, tunnelID)
	s.audit(r, "bind_imported_tunnel", instanceID,
		fmt.Sprintf("线路 %s 的隧道 %s（远端 %d）-> %d", frpsHost, tName, tRemote, localPort))
	writeJSON(w, http.StatusCreated, map[string]interface{}{
		"tunnel_id":      tunnelID,
		"remote_port":    tRemote,
		"status":         status,
		"message":        msg,
		"public_address": publicAddress("", lineDomain, frpsHost, tRemote),
	})
}