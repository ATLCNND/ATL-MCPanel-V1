package httpapi

import (
	"database/sql"
	"net/http"
)

// 实例授权级别（数值越大权限越高）。
const (
	LevelViewer = "viewer" // 只看状态/日志/监控
	LevelCollab = "collab" // 可启停 + 控制台
	LevelOwner  = "owner"  // 全部（含文件、配置、删除、授权）
)

// levelRank 返回级别权重。
func levelRank(level string) int {
	switch level {
	case LevelOwner:
		return 3
	case LevelCollab:
		return 2
	case LevelViewer:
		return 1
	default:
		return 0
	}
}

// levelAtLeast 判断 have 是否达到 need 级别。
func levelAtLeast(have, need string) bool {
	return levelRank(have) >= levelRank(need)
}

// 角色（users.role）。
const (
	RoleAdmin    = "admin"    // 总管理员：全局所有权限
	RoleNodeUser = "nodeuser" // 节点用户：普通用户 + 被指定节点内建实例 + 预分配的穿透端口
	RoleUser     = "user"     // 普通用户：只能操作被授权的实例
)

// legacyNodeAdminRole 改名前的角色值（v17 迁移已把库里的值改成 nodeuser）。
// 仍然接受它，是为了让"迁移已跑但旧令牌还没过期"的会话不至于突然 403 ——
// 角色是写在 JWT 里的，令牌最长 24 小时。
const legacyNodeAdminRole = "nodeadmin"

// isAdmin 当前请求是否为总管理员。
func isAdmin(r *http.Request) bool {
	role, _ := r.Context().Value(ctxKeyRole).(string)
	return role == RoleAdmin
}

// roleOf 取当前请求的角色。
func roleOf(r *http.Request) string {
	role, _ := r.Context().Value(ctxKeyRole).(string)
	return role
}

// isNodeUser 是否为节点用户（可能是，也可能不是总管理员）。
func isNodeUser(r *http.Request) bool {
	role := roleOf(r)
	return role == RoleNodeUser || role == legacyNodeAdminRole
}

// isNodeUserRole 判断某个角色字符串是否算"节点用户"。
func isNodeUserRole(role string) bool {
	return role == RoleNodeUser || role == legacyNodeAdminRole
}

// canManageNode 判断用户能否管理**某个节点**上的实例。
//
// 这是"节点用户"的核心判据：
//   - 总管理员：任何节点
//   - 节点用户：仅总管理员分配给自己的节点（node_users 表）
//   - 普通用户：否
//
// 注意：这里只管"节点级"的增删权（创建 / 删除 / 到期）。
// 对**已有实例**的日常操作仍然走 instanceLevel（owner/collab/viewer），
// 两套授权是正交的 —— 一个节点用户并不自动拥有该节点上别人实例的控制台权限。
func (s *Server) canManageNode(userID int64, role string, nodeID int64) bool {
	if role == RoleAdmin {
		return true
	}
	if !isNodeUserRole(role) || nodeID == 0 {
		return false
	}
	var one int
	err := s.db.QueryRow(
		`SELECT 1 FROM node_users WHERE user_id = ? AND node_id = ?`, userID, nodeID).Scan(&one)
	return err == nil
}

// managedNodeIDs 返回该用户可管理的节点 ID 列表（总管理员返回 nil 表示"全部"）。
func (s *Server) managedNodeIDs(userID int64, role string) (ids []int64, all bool) {
	if role == RoleAdmin {
		return nil, true
	}
	if !isNodeUserRole(role) {
		return nil, false
	}
	rows, err := s.db.Query(`SELECT node_id FROM node_users WHERE user_id = ? ORDER BY node_id`, userID)
	if err != nil {
		return nil, false
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err == nil {
			ids = append(ids, id)
		}
	}
	return ids, false
}

// canManageInstance 判断能否对该实例做"管理层"操作（删除 / 改到期）。
func (s *Server) canManageInstance(userID int64, role, instanceID string) bool {
	if role == RoleAdmin {
		return true
	}
	if !isNodeUserRole(role) {
		return false
	}
	nodeID, ok := s.instanceNodeID(instanceID)
	if !ok {
		return false
	}
	return s.canManageNode(userID, role, nodeID)
}

// canManageInstancePorts 判断能否开通 / 修改 / 关闭该实例的公网端口。
//
// 与 canManageInstance **刻意放宽一级**，两者管的不是一回事：
//   - canManageInstance 管的是"节点级"的权力（删除实例、改到期）——
//     这些会影响节点资源的回收与调度，所以只给总管理员与该节点的节点用户
//   - 而"这台实例要对外开哪些端口"是**实例自己的事**：
//     BlueMap 网页、Geyser 基岩版、Votifier 这些模组本来就得由使用者自己配，
//     每加一个端口都要去求管理员，面板就失去意义了
//
// 因此允许：总管理员 / 该节点的节点用户 / **该实例的 owner 级用户**。
//
// 注意 collab（协作者）**不在内**：开端口等于扩大对外暴露面，
// 不该由"只被授权启停 + 看控制台"的人决定。
func (s *Server) canManageInstancePorts(userID int64, role, instanceID string) bool {
	if s.canManageInstance(userID, role, instanceID) {
		return true
	}
	level, ok := s.instanceLevel(userID, role, instanceID)
	return ok && levelAtLeast(level, LevelOwner)
}

// instanceLevel 计算当前用户对某实例的有效级别。
// 全局 admin 视为 owner；否则查 instance_assignments。
func (s *Server) instanceLevel(userID int64, role, instanceID string) (string, bool) {
	if role == "admin" {
		return LevelOwner, true
	}
	var level string
	err := s.db.QueryRow(
		`SELECT level FROM instance_assignments WHERE instance_id = ? AND user_id = ?`,
		instanceID, userID,
	).Scan(&level)
	if err == sql.ErrNoRows || err != nil {
		return "", false
	}
	return level, true
}

// requireInstanceLevel 校验当前请求对实例的权限，不满足则写错误响应并返回 false。
func (s *Server) requireInstanceLevel(w http.ResponseWriter, r *http.Request, instanceID, need string) bool {
	userID := currentUserID(r)
	role, _ := r.Context().Value(ctxKeyRole).(string)

	level, ok := s.instanceLevel(userID, role, instanceID)
	if !ok {
		writeErr(w, http.StatusForbidden, "无权访问该实例")
		return false
	}
	if !levelAtLeast(level, need) {
		writeErr(w, http.StatusForbidden, "权限不足（需要 "+need+" 级别）")
		return false
	}
	return true
}

// instanceExists 检查实例是否登记在库。
func (s *Server) instanceExists(instanceID string) bool {
	var one int
	err := s.db.QueryRow(`SELECT 1 FROM instances WHERE instance_id = ?`, instanceID).Scan(&one)
	return err == nil
}

// instanceNodeID 查询实例所属节点，返回 (nodeID, ok)。
func (s *Server) instanceNodeID(instanceID string) (int64, bool) {
	var nodeID int64
	err := s.db.QueryRow(`SELECT node_id FROM instances WHERE instance_id = ?`, instanceID).Scan(&nodeID)
	if err != nil {
		return 0, false
	}
	return nodeID, true
}