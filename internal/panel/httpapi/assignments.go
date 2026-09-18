package httpapi

import (
	"encoding/json"
	"net/http"
	"strconv"
)

// canManageAssignments 能否查看/管理某实例的协作者授权。
//
// 与 canManageInstanceSettings 同一口径（总管理员 / 该节点的节点用户 /
// 该实例的 owner 级用户），而不是"仅总管理员"。
//
// 为什么放宽：权限级别表里 owner 的定义就是「全部（含文件、配置、删除、**授权**）」，
// 前端的实例页也给 owner 显示了「授权」按钮 —— 后端却只认管理员，
// 于是非管理员的实例主人点进去必然 403（内测报告第 7 条）。
// 这不是放宽安全边界：能改一台实例的 start.sh 与文件的人，本来就已经完全控制它了，
// 把"允许谁一起用"交给他，与"他可以把存档下载走"是同一量级的权力，
// 而前者恰恰是多租户面板必须提供的能力。
//
// 仍然排除 collab/ viewer：他们只被授权"用这台实例"，不该能决定别人能不能用。
func (s *Server) canManageAssignments(w http.ResponseWriter, r *http.Request, instanceID string) bool {
	if requireAdminIn(w, r) {
		return true
	}
	if s.canManageInstanceSettings(currentUserID(r), roleOf(r), instanceID) {
		return true
	}
	writeErr(w, http.StatusForbidden, "需要该实例的拥有者或管理员权限")
	return false
}

// handleListAssignments 列出实例的授权关系。
// GET /api/instances/{id}/assignments
func (s *Server) handleListAssignments(w http.ResponseWriter, r *http.Request) {
	instanceID := r.PathValue("id")
	if !s.canManageAssignments(w, r, instanceID) {
		return
	}
	if !s.instanceExists(instanceID) {
		writeErr(w, http.StatusNotFound, "实例不存在")
		return
	}

	rows, err := s.db.Query(`
		SELECT a.id, u.id, u.username, a.level
		FROM instance_assignments a
		JOIN users u ON u.id = a.user_id
		WHERE a.instance_id = ? AND u.role != 'admin'
		ORDER BY a.id`, instanceID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()

	type assignItem struct {
		ID       int64  `json:"id"`
		UserID   int64  `json:"user_id"`
		Username string `json:"username"`
		Level    string `json:"level"`
	}
	list := []assignItem{}
	for rows.Next() {
		var it assignItem
		if err := rows.Scan(&it.ID, &it.UserID, &it.Username, &it.Level); err != nil {
			continue
		}
		list = append(list, it)
	}
	writeJSON(w, http.StatusOK, list)
}

// handleGrantAssignment 授予/更新用户对实例的权限。
// POST /api/instances/{id}/assignments  {username, level}
func (s *Server) handleGrantAssignment(w http.ResponseWriter, r *http.Request) {
	instanceID := r.PathValue("id")
	if !s.canManageAssignments(w, r, instanceID) {
		return
	}
	if !s.instanceExists(instanceID) {
		writeErr(w, http.StatusNotFound, "实例不存在")
		return
	}

	var req struct {
		Username string `json:"username"`
		Level    string `json:"level"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "无效请求体")
		return
	}
	if levelRank(req.Level) == 0 {
		writeErr(w, http.StatusBadRequest, "level 必须是 owner / collab / viewer")
		return
	}

	// 查目标用户
	var targetID int64
	var targetRole string
	if err := s.db.QueryRow(`SELECT id, role FROM users WHERE username = ?`, req.Username).Scan(&targetID, &targetRole); err != nil {
		writeErr(w, http.StatusNotFound, "用户不存在")
		return
	}
	// 管理员默认拥有全部实例权限，不接受单独授权（避免误配与越权撤销）
	if targetRole == "admin" {
		writeErr(w, http.StatusBadRequest, "管理员默认拥有全部实例权限，无需单独授权")
		return
	}

	// upsert（SQLite: INSERT ... ON CONFLICT）
	_, err := s.db.Exec(`
		INSERT INTO instance_assignments (instance_id, user_id, level) VALUES (?, ?, ?)
		ON CONFLICT(instance_id, user_id) DO UPDATE SET level = excluded.level`,
		instanceID, targetID, req.Level)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	s.audit(r, "grant_assignment", instanceID, "用户="+req.Username+" 级别="+req.Level)
	writeJSON(w, http.StatusOK, map[string]string{"message": "授权成功"})
}

// handleRevokeAssignment 撤销授权（不可撤销管理员）。
// DELETE /api/instances/{id}/assignments?user_id=N
func (s *Server) handleRevokeAssignment(w http.ResponseWriter, r *http.Request) {
	instanceID := r.PathValue("id")
	if !s.canManageAssignments(w, r, instanceID) {
		return
	}

	userIDStr := r.URL.Query().Get("user_id")
	userID, err := strconv.ParseInt(userIDStr, 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "user_id 无效")
		return
	}

	// 保护：不允许撤销管理员的权限
	var targetRole string
	if err := s.db.QueryRow(`SELECT role FROM users WHERE id = ?`, userID).Scan(&targetRole); err != nil {
		writeErr(w, http.StatusNotFound, "用户不存在")
		return
	}
	if targetRole == "admin" {
		writeErr(w, http.StatusForbidden, "不能撤销管理员的权限")
		return
	}

	if _, err := s.db.Exec(`DELETE FROM instance_assignments WHERE instance_id = ? AND user_id = ?`, instanceID, userID); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	s.audit(r, "revoke_assignment", instanceID, "user_id="+userIDStr)
	writeJSON(w, http.StatusOK, map[string]string{"message": "已撤销授权"})
}

// handleMyPermissions 返回当前用户对各实例的权限，供前端做 UI 控制。
// GET /api/my/instances
func (s *Server) handleMyPermissions(w http.ResponseWriter, r *http.Request) {
	userID := currentUserID(r)
	role, _ := r.Context().Value(ctxKeyRole).(string)

	type perm struct {
		InstanceID string `json:"instance_id"`
		Level      string `json:"level"`
	}

	if role == "admin" {
		// admin 对所有实例都是 owner
		rows, err := s.db.Query(`SELECT instance_id FROM instances`)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		defer rows.Close()
		list := []perm{}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err == nil {
				list = append(list, perm{InstanceID: id, Level: LevelOwner})
			}
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"role": role, "permissions": list})
		return
	}

	rows, err := s.db.Query(`SELECT instance_id, level FROM instance_assignments WHERE user_id = ?`, userID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()
	list := []perm{}
	for rows.Next() {
		var p perm
		if err := rows.Scan(&p.InstanceID, &p.Level); err == nil {
			list = append(list, p)
		}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"role": role, "permissions": list})
}
