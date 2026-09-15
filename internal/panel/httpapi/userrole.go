package httpapi

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
)

// handleSetUserRole 改账号类型（仅总管理员）。
// PUT /api/users/{id}  body: {"role": "user" | "nodeuser" | "admin"}
//
// 为什么需要它：角色原先只能在**建号时**指定，建完想改就只能删号重建 ——
// 而删号会连带清掉该用户的实例授权（`instance_assignments`），
// 于是"把 bob 升成节点用户"变成一件要重新配授权的事。
//
// 三个刻意的设计决定：
//
//  1. **角色写在 JWT 里**，改完不会立刻生效：对方手上的旧令牌最长还能用 24 小时
//     （`auth` 的令牌有效期）。所以响应里明确写清"重新登录后立即生效"，
//     而不是假装已经生效 —— 同"设为节点用户后要重新登录"是同一个坑。
//     没做 token 版本号：那是另一套机制，收益（最多提前 24 小时生效）不值这个复杂度。
//
//  2. **不能改自己的角色**：把自己降级会当场把自己锁在门外（而且旧令牌还能用 24 小时，
//     表现会非常费解）。升/降都拒掉，因为改自己角色这件事没有正当场景 ——
//     真有需要就让另一个管理员来改。
//
//  3. **不能降级最后一个管理员**：否则系统里就再也没有人能改权限了。
//     （与 handleDeleteUser 里"不能删最后一个管理员"是同一条底线。）
//
// 另外：从 nodeuser 降回 user 时**保留** `node_users` / `node_user_ports`。
// 权限判定的第一关（role）过不去就够了，将来再升回来配置还在；
// 顺手清掉反而是"降级即销毁配置"这种更难挽回的行为。
func (s *Server) handleSetUserRole(w http.ResponseWriter, r *http.Request) {
	if !requireAdminIn(w, r) {
		return
	}
	targetID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "user id 无效")
		return
	}

	var req struct {
		Role string `json:"role"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "无效请求体")
		return
	}
	newRole := strings.TrimSpace(req.Role)
	if !isValidRole(newRole) {
		writeErr(w, http.StatusBadRequest, "角色只能是 user / nodeuser / admin")
		return
	}

	if targetID == currentUserID(r) {
		writeErr(w, http.StatusForbidden, "不能修改自己的角色（把自己降级会当场失去管理权限）")
		return
	}

	var username, oldRole string
	if err := s.db.QueryRow(`SELECT username, role FROM users WHERE id = ?`, targetID).
		Scan(&username, &oldRole); err != nil {
		writeErr(w, http.StatusNotFound, "用户不存在")
		return
	}
	if oldRole == newRole {
		writeJSON(w, http.StatusOK, map[string]string{
			"message": "角色未变化（当前已是 " + roleLabelZh(newRole) + "）",
			"role":    newRole,
		})
		return
	}

	// 最后一个管理员不可降级（升到 admin 不受限）
	if oldRole == RoleAdmin {
		var adminCount int
		_ = s.db.QueryRow(`SELECT COUNT(*) FROM users WHERE role = ?`, RoleAdmin).Scan(&adminCount)
		if adminCount <= 1 {
			writeErr(w, http.StatusForbidden, "不能降级最后一个管理员账号")
			return
		}
	}

	if _, err := s.db.Exec(`UPDATE users SET role = ? WHERE id = ?`, newRole, targetID); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	s.audit(r, "set_user_role", username, roleLabelZh(oldRole)+" → "+roleLabelZh(newRole))

	// 提醒写进响应里：前端直接展示这句，管理员就不会以为改完立刻生效
	msg := "已把 " + username + " 改为" + roleLabelZh(newRole) +
		"；该用户**重新登录后**生效（旧令牌最长 24 小时内仍按原角色判定）"
	if newRole == RoleUser && oldRole == RoleNodeUser {
		msg += "；其节点授权与端口配额已保留（再升回节点用户时配置还在）"
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": msg, "role": newRole})
}

// isValidRole 角色是否合法。
func isValidRole(role string) bool {
	switch role {
	case RoleAdmin, RoleNodeUser, RoleUser:
		return true
	}
	return false
}

// roleLabelZh 角色的中文名（用于提示与审计，避免界面/日志里蹦英文枚举）。
func roleLabelZh(role string) string {
	switch role {
	case RoleAdmin:
		return "总管理员"
	case RoleNodeUser:
		return "节点用户"
	case RoleUser:
		return "普通用户"
	}
	return role
}
