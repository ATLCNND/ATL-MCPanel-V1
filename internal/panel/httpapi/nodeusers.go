package httpapi

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ============================================================================
// 节点用户：总管理员指定「谁能管哪些节点」+「在每条线路上有几个穿透端口」
// ============================================================================
//
// 命名：这个角色叫「节点用户」而不是「节点管理员」。
// 前者描述的是"被分配了资源的普通用户"，后者暗示"节点的二把手" ——
// 而他的实际权限只有三件事：在被指定的节点上创建/删除实例、设置到期时间、
// 以及使用预分配的穿透端口。名字必须与权限边界一致，否则迟早被误解。

// handleMyNodes GET /api/my/nodes
//
// 返回"我可以在哪些节点上创建实例"，供创建表单填节点下拉框。
//
// 为什么不复用 /api/nodes：那个接口只有总管理员能调，而且会带出
// SSH 用户名、凭据标识、Daemon 端口等运维信息 —— 节点用户只需要知道
// "我能往哪台机器上放实例"。字段按最小必要原则给。
func (s *Server) handleMyNodes(w http.ResponseWriter, r *http.Request) {
	userID := currentUserID(r)
	role := roleOf(r)

	type nodeView struct {
		ID        int64  `json:"id"`
		Name      string `json:"name"`
		IP        string `json:"ip"`
		Status    string `json:"status"`
		Instances int    `json:"instances"`
	}

	base := `
		SELECT n.id, n.name, n.ip, n.status,
		       (SELECT COUNT(*) FROM instances i WHERE i.node_id = n.id)
		FROM nodes n`

	var (
		rows *sql.Rows
		err  error
	)
	switch {
	case role == RoleAdmin:
		rows, err = s.db.Query(base + ` ORDER BY n.id`)
	case isNodeUserRole(role):
		rows, err = s.db.Query(base+`
			WHERE n.id IN (SELECT node_id FROM node_users WHERE user_id = ?)
			ORDER BY n.id`, userID)
	default:
		// 普通用户不能创建实例，返回空列表即可（前端据此隐藏创建入口）
		writeJSON(w, http.StatusOK, []nodeView{})
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()

	list := []nodeView{}
	for rows.Next() {
		var v nodeView
		if err := rows.Scan(&v.ID, &v.Name, &v.IP, &v.Status, &v.Instances); err != nil {
			continue
		}
		// 节点 IP 只给总管理员。
		//
		// 节点用户只需要知道"我能把实例放到哪台机器上"（靠 name 就够），
		// 而 IP 属于运维信息 —— 界面上的下拉框会把它显示出来，
		// 等于把节点入口地址暴露给普通使用者。
		// 放在这里清空而不是改 SQL：上面两条查询共用一份 base，
		// 在这里一句话就够，也不会让 SQL 分叉。
		if role != RoleAdmin {
			v.IP = ""
		}
		list = append(list, v)
	}
	writeJSON(w, http.StatusOK, list)
}

// ---- 节点范围授权 ----

type nodeUserView struct {
	UserID    int64  `json:"user_id"`
	Username  string `json:"username"`
	NodeID    int64  `json:"node_id"`
	NodeName  string `json:"node_name"`
	GrantedBy string `json:"granted_by_name"`
	CreatedAt string `json:"created_at"`
}

// handleListNodeUsers GET /api/node-users
func (s *Server) handleListNodeUsers(w http.ResponseWriter, r *http.Request) {
	if !requireAdminIn(w, r) {
		return
	}
	rows, err := s.db.Query(`
		SELECT nu.user_id, u.username, nu.node_id, COALESCE(n.name, '(已删除节点)'),
		       COALESCE(g.username, ''), nu.created_at
		FROM node_users nu
		JOIN users u ON u.id = nu.user_id
		LEFT JOIN nodes n ON n.id = nu.node_id
		LEFT JOIN users g ON g.id = nu.granted_by
		ORDER BY u.username, nu.node_id`)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()

	list := []nodeUserView{}
	for rows.Next() {
		var v nodeUserView
		if err := rows.Scan(&v.UserID, &v.Username, &v.NodeID, &v.NodeName, &v.GrantedBy, &v.CreatedAt); err != nil {
			continue
		}
		list = append(list, v)
	}
	writeJSON(w, http.StatusOK, list)
}

// handleGrantNodeUser POST /api/node-users  body: {username, node_id}
//
// 授予时会**把该用户的角色提升为 nodeuser**：这是"角色 + 节点范围"两段式，
// 只写关联表而不改角色的话，权限判定的第一关就过不去（角色还是 user）。
func (s *Server) handleGrantNodeUser(w http.ResponseWriter, r *http.Request) {
	if !requireAdminIn(w, r) {
		return
	}
	var req struct {
		Username string `json:"username"`
		NodeID   int64  `json:"node_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "无效请求体")
		return
	}
	req.Username = strings.TrimSpace(req.Username)
	if req.Username == "" || req.NodeID == 0 {
		writeErr(w, http.StatusBadRequest, "username 与 node_id 均不能为空")
		return
	}

	var uid int64
	var role string
	if err := s.db.QueryRow(`SELECT id, role FROM users WHERE username = ?`, req.Username).
		Scan(&uid, &role); err != nil {
		writeErr(w, http.StatusNotFound, "用户不存在："+req.Username)
		return
	}
	if role == RoleAdmin {
		// 总管理员本来就管所有节点，再挂一条节点授权只会让"他到底管哪些"变得含糊
		writeErr(w, http.StatusBadRequest, "该用户已是总管理员，无需分配节点")
		return
	}
	var exists int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM nodes WHERE id = ?`, req.NodeID).Scan(&exists); err != nil || exists == 0 {
		writeErr(w, http.StatusNotFound, "节点不存在")
		return
	}

	if _, err := s.db.Exec(`
		INSERT INTO node_users (user_id, node_id, granted_by) VALUES (?, ?, ?)
		ON CONFLICT(user_id, node_id) DO NOTHING`,
		uid, req.NodeID, currentUserID(r)); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	// 角色提升：只在还不是节点用户时改，避免把 admin 降级
	if !isNodeUserRole(role) {
		if _, err := s.db.Exec(`UPDATE users SET role = ? WHERE id = ?`, RoleNodeUser, uid); err != nil {
			writeErr(w, http.StatusInternalServerError, "更新角色失败: "+err.Error())
			return
		}
	}

	s.audit(r, "grant_node_user", req.Username, fmt.Sprintf("node_id=%d", req.NodeID))
	writeJSON(w, http.StatusOK, map[string]string{
		"message": fmt.Sprintf("已把 %s 设为节点用户并授权该节点（角色已更新）", req.Username),
	})
}

// handleRevokeNodeUser DELETE /api/node-users?user_id=&node_id=
//
// 收回最后一个节点时会**把角色降回 user**：留着 nodeuser 角色但一个节点都不管，
// 会让这个用户在界面上看到一堆点不动的入口，也容易让人误以为还有权限。
func (s *Server) handleRevokeNodeUser(w http.ResponseWriter, r *http.Request) {
	if !requireAdminIn(w, r) {
		return
	}
	userID, _ := strconv.ParseInt(r.URL.Query().Get("user_id"), 10, 64)
	nodeID, _ := strconv.ParseInt(r.URL.Query().Get("node_id"), 10, 64)
	if userID == 0 || nodeID == 0 {
		writeErr(w, http.StatusBadRequest, "user_id 与 node_id 均不能为空")
		return
	}

	if _, err := s.db.Exec(`DELETE FROM node_users WHERE user_id = ? AND node_id = ?`, userID, nodeID); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	var left int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM node_users WHERE user_id = ?`, userID).Scan(&left)
	msg := "已收回该节点的管理权限"
	if left == 0 {
		if _, err := s.db.Exec(`UPDATE users SET role = ? WHERE id = ? AND role IN (?, ?)`,
			RoleUser, userID, RoleNodeUser, legacyNodeAdminRole); err == nil {
			msg = "已收回该节点的管理权限（该用户已无任何节点，角色降回普通用户）"
		}
	}

	s.audit(r, "revoke_node_user", strconv.FormatInt(userID, 10), fmt.Sprintf("node_id=%d", nodeID))
	writeJSON(w, http.StatusOK, map[string]string{"message": msg})
}

// ---- 穿透端口配额 ----

// portGrantView 一条"某用户在某线路上有几个端口"的授权。
type portGrantView struct {
	UserID    int64  `json:"user_id"`
	Username  string `json:"username"`
	FrpsID    int64  `json:"frps_id"`
	FrpsName  string `json:"frps_name"`
	FrpsHost  string `json:"frps_host"`
	Quota     int    `json:"quota"`
	Used      int    `json:"used"`
	Available int    `json:"available"`
	GrantedBy string `json:"granted_by_name"`
}

// instanceQuotaOwnerExpr 是"这条实例的端口配额算在谁头上"的 SQL 表达式。
//
// 为什么不是直接用 instances.created_by：
// 总管理员可以**替别人建实例**（建完再把 owner 授权给那个用户），
// 这时 created_by 是管理员，而端口实际属于那台实例 ——
// 配额该由实例归属者承担，否则普通用户永远用不上自己的配额。
// 所以优先取 instance_assignments 里的 owner 级用户，取不到才退回 created_by。
//
// ⚠️ portUsage 与各处配额校验**必须引用同一个表达式**：
// 用量和上限若按两套口径算，迟早会出现"明明没超却被拒"或"超了也不拦"。
//
// 外层查询里实例表的别名必须是 i。
const instanceQuotaOwnerExpr = `COALESCE(
		(SELECT a.user_id FROM instance_assignments a
		  WHERE a.instance_id = i.instance_id AND a.level = 'owner'
		  ORDER BY a.user_id LIMIT 1),
		i.created_by)`

// instanceQuotaOwner 返回该实例的端口配额归属者；无法确定时返回 0。
//
// 返回 0 表示"追溯不到归属者"（v17 之前建的老实例 created_by 为 0），
// 调用方应退回按操作者本人处理，行为与改动前一致。
func (s *Server) instanceQuotaOwner(instanceID string) int64 {
	var uid int64
	err := s.db.QueryRow(
		`SELECT `+instanceQuotaOwnerExpr+` FROM instances i WHERE i.instance_id = ?`, instanceID).Scan(&uid)
	if err != nil {
		return 0
	}
	return uid
}

// portUsage 统计某用户在各线路上已用掉的端口数。
//
// 用量不是单独记一本账，而是**从隧道推导**：归属该用户的实例占用了多少条隧道，
// 就算用掉多少端口。这样删除实例、删除隧道都会自动释放配额，
// 不会出现"账本说用了 3 个、实际一条隧道都没有"的对不上。
//
// "归属该用户"的判定见 instanceQuotaOwnerExpr（owner 授权优先，其次 created_by）。
func (s *Server) portUsage(userID int64) map[int64]int {
	out := map[int64]int{}
	rows, err := s.db.Query(`
		SELECT t.frps_id, COUNT(*)
		FROM tunnels t
		JOIN instances i ON i.instance_id = t.instance_id
		WHERE `+instanceQuotaOwnerExpr+` = ?
		GROUP BY t.frps_id`, userID)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var frpsID int64
		var n int
		if err := rows.Scan(&frpsID, &n); err == nil {
			out[frpsID] = n
		}
	}
	return out
}

// handleListPortGrants GET /api/node-users/ports （仅总管理员）
func (s *Server) handleListPortGrants(w http.ResponseWriter, r *http.Request) {
	if !requireAdminIn(w, r) {
		return
	}
	rows, err := s.db.Query(`
		SELECT np.user_id, u.username, np.frps_id, COALESCE(f.name, '(已删除线路)'),
		       COALESCE(f.host, ''), np.quota, COALESCE(g.username, '')
		FROM node_user_ports np
		JOIN users u ON u.id = np.user_id
		LEFT JOIN frps_servers f ON f.id = np.frps_id
		LEFT JOIN users g ON g.id = np.granted_by
		ORDER BY u.username, np.frps_id`)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()

	type row struct {
		uid      int64
		username string
		frpsID   int64
		frpsName string
		frpsHost string
		quota    int
		granted  string
	}
	var raw []row
	users := map[int64]bool{}
	for rows.Next() {
		var x row
		if err := rows.Scan(&x.uid, &x.username, &x.frpsID, &x.frpsName, &x.frpsHost, &x.quota, &x.granted); err != nil {
			continue
		}
		raw = append(raw, x)
		users[x.uid] = true
	}

	usage := map[int64]map[int64]int{}
	for uid := range users {
		usage[uid] = s.portUsage(uid)
	}

	list := make([]portGrantView, 0, len(raw))
	for _, x := range raw {
		used := usage[x.uid][x.frpsID]
		list = append(list, portGrantView{
			UserID: x.uid, Username: x.username,
			FrpsID: x.frpsID, FrpsName: x.frpsName, FrpsHost: x.frpsHost,
			Quota: x.quota, Used: used, Available: maxInt(0, x.quota-used),
			GrantedBy: x.granted,
		})
	}
	writeJSON(w, http.StatusOK, list)
}

// handleSetPortGrant POST /api/node-users/ports  body: {username, frps_id, quota}
//
// 配额可以调小到低于已用量 —— 这时不阻止保存（管理员可能就是要"收紧"），
// 但会返回提示说明已经超用，由管理员决定要不要删隧道。
func (s *Server) handleSetPortGrant(w http.ResponseWriter, r *http.Request) {
	if !requireAdminIn(w, r) {
		return
	}
	var req struct {
		Username string `json:"username"`
		FrpsID   int64  `json:"frps_id"`
		Quota    int    `json:"quota"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "无效请求体")
		return
	}
	req.Username = strings.TrimSpace(req.Username)
	if req.Username == "" || req.FrpsID == 0 {
		writeErr(w, http.StatusBadRequest, "username 与 frps_id 均不能为空")
		return
	}
	if req.Quota < 0 {
		req.Quota = 0
	}
	if req.Quota > 1000 {
		writeErr(w, http.StatusBadRequest, "单个用户的端口配额上限为 1000")
		return
	}

	var uid int64
	var role string
	if err := s.db.QueryRow(`SELECT id, role FROM users WHERE username = ?`, req.Username).
		Scan(&uid, &role); err != nil {
		writeErr(w, http.StatusNotFound, "用户不存在："+req.Username)
		return
	}
	if role == RoleAdmin {
		writeErr(w, http.StatusBadRequest, "总管理员不受端口配额限制")
		return
	}
	var frpsName string
	if err := s.db.QueryRow(`SELECT name FROM frps_servers WHERE id = ?`, req.FrpsID).Scan(&frpsName); err != nil {
		writeErr(w, http.StatusNotFound, "线路（frps 服务器）不存在")
		return
	}
	// 端口配额**不再要求"必须是节点用户"**。
	//
	// 普通用户也可能是实例的 owner：总管理员建好实例后把 owner 授权给他，
	// 他就要给自己那台实例加多端口模组要用的端口 —— 得有配额可扣。
	// 配额是按**线路**（frps_id）发的、与节点无关，所以这里不需要节点范围，
	// 与「节点范围」那套授权是正交的两件事。

	if _, err := s.db.Exec(`
		INSERT INTO node_user_ports (user_id, frps_id, quota, granted_by, updated_at)
		VALUES (?, ?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(user_id, frps_id) DO UPDATE SET
			quota = excluded.quota, granted_by = excluded.granted_by, updated_at = CURRENT_TIMESTAMP`,
		uid, req.FrpsID, req.Quota, currentUserID(r)); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	used := s.portUsage(uid)[req.FrpsID]
	msg := fmt.Sprintf("已设置 %s 在「%s」上的端口配额为 %d 个", req.Username, frpsName, req.Quota)
	if !isNodeUserRole(role) {
		// 普通用户没有建实例的权限，配额只能用在"被授权为 owner 的实例"上，
		// 说清楚免得管理员以为发了他就能自己建实例
		msg += "（该用户是普通用户：配额只对他被授权为 owner 的实例生效）"
	}
	if used > req.Quota {
		msg += fmt.Sprintf("（注意：该用户已用 %d 个，已超出配额，需要删除部分隧道）", used)
	}

	s.audit(r, "set_port_quota", req.Username, fmt.Sprintf("frps=%s quota=%d used=%d", frpsName, req.Quota, used))
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"message": msg, "quota": req.Quota, "used": used,
	})
}

// handleDeletePortGrant DELETE /api/node-users/ports?user_id=&frps_id=
func (s *Server) handleDeletePortGrant(w http.ResponseWriter, r *http.Request) {
	if !requireAdminIn(w, r) {
		return
	}
	userID, _ := strconv.ParseInt(r.URL.Query().Get("user_id"), 10, 64)
	frpsID, _ := strconv.ParseInt(r.URL.Query().Get("frps_id"), 10, 64)
	if userID == 0 || frpsID == 0 {
		writeErr(w, http.StatusBadRequest, "user_id 与 frps_id 均不能为空")
		return
	}
	if _, err := s.db.Exec(`DELETE FROM node_user_ports WHERE user_id = ? AND frps_id = ?`, userID, frpsID); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.audit(r, "delete_port_quota", strconv.FormatInt(userID, 10), fmt.Sprintf("frps_id=%d", frpsID))
	writeJSON(w, http.StatusOK, map[string]string{"message": "已移除该线路的端口配额"})
}

// handleMyPorts GET /api/my/ports
//
// 我自己在各线路上的端口配额与已用量 —— 创建实例时据此填"线路 + 端口数量"。
// 任何登录用户都可读：普通用户只会拿到自己的（通常为空）。
func (s *Server) handleMyPorts(w http.ResponseWriter, r *http.Request) {
	userID := currentUserID(r)
	role := roleOf(r)

	type line struct {
		FrpsID    int64  `json:"frps_id"`
		FrpsName  string `json:"frps_name"`
		FrpsHost  string `json:"frps_host"`
		Quota     int    `json:"quota"`
		Used      int    `json:"used"`
		Available int    `json:"available"`
		Unlimited bool   `json:"unlimited"`
	}

	// 总管理员不受配额限制：把每条线路都列为"不限"
	if role == RoleAdmin {
		rows, err := s.db.Query(`SELECT id, name, host FROM frps_servers ORDER BY id`)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		defer rows.Close()
		usage := s.portUsage(userID)
		list := []line{}
		for rows.Next() {
			var l line
			if err := rows.Scan(&l.FrpsID, &l.FrpsName, &l.FrpsHost); err != nil {
				continue
			}
			l.Unlimited = true
			l.Used = usage[l.FrpsID]
			l.Quota = -1
			list = append(list, l)
		}
		writeJSON(w, http.StatusOK, list)
		return
	}

	// 注意：这条是**非管理员**分支 —— 不返回线路的 host。
	//
	// 为什么：创建实例的表单会在"线路"那一行用小字显示 host，
	// 等于把公网服务器的真实地址暴露给使用者。用户只需要知道
	// 线路**名字**和还剩几个端口，host 是运维信息（管理员看「穿透管理」即可）。
	// 这里直接不查它，而不是查出来再清空 —— 少一份能泄露的数据。
	rows, err := s.db.Query(`
		SELECT np.frps_id, COALESCE(f.name, '(已删除线路)'), np.quota
		FROM node_user_ports np
		LEFT JOIN frps_servers f ON f.id = np.frps_id
		WHERE np.user_id = ?
		ORDER BY np.frps_id`, userID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()

	usage := s.portUsage(userID)
	list := []line{}
	for rows.Next() {
		var l line
		if err := rows.Scan(&l.FrpsID, &l.FrpsName, &l.Quota); err != nil {
			continue
		}
		l.Used = usage[l.FrpsID]
		l.Available = maxInt(0, l.Quota-l.Used)
		list = append(list, l)
	}
	writeJSON(w, http.StatusOK, list)
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// ============================================================================
// 实例到期时间
// ============================================================================

// handleSetExpiry POST /api/instances/{id}/expiry
//
// body: { "expires_at": "2026-12-31T23:59", "notice_days": 3, "autostop": true }
// expires_at 为空串表示清除到期时间（永不过期）。
//
// 权限：总管理员，或该节点上的节点用户 —— 与删除同级。
// 到期会**自动停机**，这跟直接操作实例的破坏性相当，因此不用 owner 级别
// （那会让租户自己给自己续期，到期就失去意义了）。
func (s *Server) handleSetExpiry(w http.ResponseWriter, r *http.Request) {
	instanceID := r.PathValue("id")
	if !s.canManageInstance(currentUserID(r), roleOf(r), instanceID) {
		writeErr(w, http.StatusForbidden, "仅总管理员或该节点的节点用户可设置到期时间")
		return
	}
	if !s.instanceExists(instanceID) {
		writeErr(w, http.StatusNotFound, "实例不存在")
		return
	}

	var req struct {
		ExpiresAt  string `json:"expires_at"`
		NoticeDays *int   `json:"notice_days"`
		Autostop   *bool  `json:"autostop"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "无效请求体")
		return
	}

	raw := strings.TrimSpace(req.ExpiresAt)
	var at sql.NullTime
	if raw != "" {
		t, err := parseExpiryTime(raw)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "到期时间格式无法识别："+raw+"（可用 2026-12-31T23:59 或 2026-12-31）")
			return
		}
		at = sql.NullTime{Time: t, Valid: true}
	}

	notice := 3
	if req.NoticeDays != nil {
		notice = *req.NoticeDays
		if notice < 0 {
			notice = 0
		}
		if notice > 60 {
			notice = 60
		}
	}
	autostop := 1
	if req.Autostop != nil {
		if *req.Autostop {
			autostop = 1
		} else {
			autostop = 0
		}
	}

	var expires interface{}
	if at.Valid {
		// 统一按 UTC 入库：读取端（scheduler / 列表）都按绝对时刻比较，
		// 存本地时间会让跨时区或改时区后语义漂移
		expires = at.Time.UTC()
	}
	if _, err := s.db.Exec(`
		UPDATE instances SET expires_at = ?, expiry_notice_days = ?, expiry_autostop = ?
		WHERE instance_id = ?`, expires, notice, autostop, instanceID); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	detail := "已清除到期时间"
	if at.Valid {
		detail = fmt.Sprintf("到期时间 %s（提前 %d 天提醒，%s）",
			at.Time.Format("2006-01-02 15:04"), notice,
			map[bool]string{true: "到期自动停止", false: "到期仅告警"}[autostop == 1])
	}
	s.audit(r, "set_instance_expiry", instanceID, detail)

	// 设置到期时间时顺手清掉"已到期"告警：管理员刚续期，旧告警就该消失
	if _, err := s.db.Exec(
		`UPDATE alerts SET active = 0, resolved_at = CURRENT_TIMESTAMP
		 WHERE kind IN ('instance_expiring','instance_expired') AND target = ? AND active = 1`,
		instanceID); err != nil {
		s.logger.Warn("清除到期告警失败", "instance", instanceID, "error", err)
	}

	writeJSON(w, http.StatusOK, map[string]string{"message": detail})
}

// parseExpiryTime 宽容地解析管理员填的到期时间。
//
// 面板上用的是 <input type="datetime-local">，它给出的是 `2026-12-31T23:59`
// 这种**没有时区**的本地时间；管理员也可能只填日期。两种都要能收，
// 且都按服务器本地时区解释 —— 管理员说的"12 月 31 日到期"就是他那边的 12 月 31 日。
func parseExpiryTime(raw string) (time.Time, error) {
	layouts := []string{
		time.RFC3339,
		"2006-01-02T15:04:05",
		"2006-01-02T15:04",
		"2006-01-02 15:04:05",
		"2006-01-02 15:04",
		"2006-01-02",
		"2006/01/02",
	}
	for _, l := range layouts {
		if t, err := time.ParseInLocation(l, raw, time.Local); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("无法解析")
}
