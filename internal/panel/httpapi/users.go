package httpapi

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strconv"

	"github.com/ATLCNND/ATL-MCPanel/internal/panel/auth"
)

// handleRegister 管理员创建用户（注册由管理员控制；首个用户自动成为 admin）。
type registerReq struct {
	Username string `json:"username"`
	Password string `json:"password"`
	Role     string `json:"role"` // admin / user，默认 user
}

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	var req registerReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "无效的请求体")
		return
	}
	if req.Username == "" || len(req.Password) < 6 {
		writeErr(w, http.StatusBadRequest, "用户名不能为空，密码至少 6 位")
		return
	}

	// 检查 users 表是否为空（首次初始化）
	var count int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&count)
	if count == 0 {
		// 第一个用户自动成为 admin
		role := "admin"
		hash, err := auth.HashPassword(req.Password)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "密码加密失败")
			return
		}
		res, err := s.db.Exec(`INSERT INTO users (username, password_hash, role) VALUES (?, ?, ?)`, req.Username, hash, role)
		if err != nil {
			writeErr(w, http.StatusConflict, "用户名已存在")
			return
		}
		newID, _ := res.LastInsertId()
		s.auditAs(newID, r, "create_user", req.Username, "首个用户（自动 admin）")
		writeJSON(w, http.StatusCreated, map[string]string{"message": "首个用户已创建为管理员", "role": "admin"})
		return
	}

	// 非首次：要求当前用户是 admin
	curRole, _ := r.Context().Value(ctxKeyRole).(string)
	if curRole != "admin" {
		writeErr(w, http.StatusForbidden, "只有管理员能创建用户")
		return
	}

	role := req.Role
	if role != "admin" {
		role = "user"
	}
	hash, err := auth.HashPassword(req.Password)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "密码加密失败")
		return
	}
	_, err = s.db.Exec(`INSERT INTO users (username, password_hash, role) VALUES (?, ?, ?)`, req.Username, hash, role)
	if err != nil {
		writeErr(w, http.StatusConflict, "用户名已存在")
		return
	}
	s.audit(r, "create_user", req.Username, "角色="+role)
	writeJSON(w, http.StatusCreated, map[string]string{"message": "用户创建成功"})
}

// handleListUsers 列出所有用户（仅 admin）。
func (s *Server) handleListUsers(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.Query(`SELECT id, username, role, status, created_at FROM users ORDER BY id`)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()
	type user struct {
		ID        int64  `json:"id"`
		Username  string `json:"username"`
		Role      string `json:"role"`
		Status    string `json:"status"`
		CreatedAt string `json:"created_at"`
	}
	list := []user{}
	for rows.Next() {
		var u user
		if err := rows.Scan(&u.ID, &u.Username, &u.Role, &u.Status, &u.CreatedAt); err != nil {
			continue
		}
		list = append(list, u)
	}
	writeJSON(w, http.StatusOK, list)
}

// handleDeleteUser 删除账号（仅管理员）。
// 保护：不能删除自己；不能删除最后一个管理员；会同时清理其实例授权。
// DELETE /api/users/{id}
func (s *Server) handleDeleteUser(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	targetID, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "user id 无效")
		return
	}

	// 不能删除自己
	if targetID == currentUserID(r) {
		writeErr(w, http.StatusForbidden, "不能删除当前登录的账号")
		return
	}

	var username, role string
	if err := s.db.QueryRow(`SELECT username, role FROM users WHERE id = ?`, targetID).Scan(&username, &role); err != nil {
		writeErr(w, http.StatusNotFound, "用户不存在")
		return
	}

	// 不能删除最后一个管理员
	if role == "admin" {
		var adminCount int
		_ = s.db.QueryRow(`SELECT COUNT(*) FROM users WHERE role = 'admin'`).Scan(&adminCount)
		if adminCount <= 1 {
			writeErr(w, http.StatusForbidden, "不能删除最后一个管理员账号")
			return
		}
	}

	// 头像文件要单独删：它只存在磁盘上（data/avatars/<uid><ext>），
	// 删用户行不会连带删掉 —— 否则每删一个有头像的账号就留一个孤儿文件。
	// 自查时实测积了 4 个（含更早测试留下的）。
	// 注意：得在 DELETE 之前把文件名读出来，删完就查不到了。
	var avatarFile string
	_ = s.db.QueryRow(`SELECT avatar_file FROM users WHERE id = ?`, targetID).Scan(&avatarFile)

	// 清理该用户的实例授权。
	// 他名下的**实例本身保留**（那是节点的资源，不该因为删账号就消失），
	// 但注意 instances.created_by 仍指向这个已删除的 id —— 配额归属解析遇到
	// 这种情况会退回"按操作者算"（见 instanceQuotaOwner）。
	_, _ = s.db.Exec(`DELETE FROM instance_assignments WHERE user_id = ?`, targetID)
	_, _ = s.db.Exec(`DELETE FROM users WHERE id = ?`, targetID)

	if avatarFile != "" {
		_ = os.Remove(filepath.Join(s.avatarDir, filepath.Base(avatarFile)))
	}

	s.audit(r, "delete_user", username, "角色="+role)
	writeJSON(w, http.StatusOK, map[string]string{"message": "账号已删除"})
}

// handleChangePassword 修改密码（改自己，或用 admin 改他人）。
func (s *Server) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	var req struct {
		OldPassword string `json:"old_password"`
		NewPassword string `json:"new_password"`
		TargetUser  string `json:"target_user"` // 可选，admin 重置他人密码时填写
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "无效的请求体")
		return
	}
	if len(req.NewPassword) < 6 {
		writeErr(w, http.StatusBadRequest, "新密码至少 6 位")
		return
	}

	userID := currentUserID(r)
	role, _ := r.Context().Value(ctxKeyRole).(string)

	var targetID int64
	var username string

	if req.TargetUser != "" {
		// admin 重置他人密码
		if role != "admin" {
			writeErr(w, http.StatusForbidden, "只有管理员能重置他人密码")
			return
		}
		err := s.db.QueryRow(`SELECT id, username FROM users WHERE username = ?`, req.TargetUser).Scan(&targetID, &username)
		if err != nil {
			writeErr(w, http.StatusNotFound, "目标用户不存在")
			return
		}
	} else {
		// 修改自己的密码，需校验旧密码
		var hash string
		err := s.db.QueryRow(`SELECT password_hash, username FROM users WHERE id = ?`, userID).Scan(&hash, &username)
		if err != nil {
			writeErr(w, http.StatusNotFound, "用户不存在")
			return
		}
		if !auth.CheckPassword(hash, req.OldPassword) {
			writeErr(w, http.StatusUnauthorized, "旧密码错误")
			return
		}
		targetID = userID
	}

	newHash, err := auth.HashPassword(req.NewPassword)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "密码加密失败")
		return
	}
	_, err = s.db.Exec(`UPDATE users SET password_hash = ? WHERE id = ?`, newHash, targetID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "更新密码失败")
		return
	}
	s.audit(r, "change_password", username, "目标用户="+username)
	writeJSON(w, http.StatusOK, map[string]string{"message": "密码修改成功"})
}