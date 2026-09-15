package httpapi

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ============================================================================
// 用户资料：头像上传与审核、UID、入站时长、累计在线
// ============================================================================

// maxAvatarBytes 头像大小上限（2 MB）。
// 头像是小图，过大的文件既浪费磁盘也会拖慢每次加载。
const maxAvatarBytes = 2 << 20

// allowAvatarExt 允许的图片类型。只按扩展名与内容嗅探双重校验，
// 不信任客户端提交的 Content-Type。
var allowAvatarExt = map[string]string{
	"image/jpeg": ".jpg",
	"image/png":  ".png",
	"image/webp": ".webp",
	"image/gif":  ".gif",
}

// defaultAvatarDir 由面板数据目录推导头像目录。
func defaultAvatarDir(dataDir string) string {
	if dataDir == "" {
		dataDir = "data"
	}
	return filepath.Join(dataDir, "avatars")
}

// profileView 是 /api/auth/me 的响应。
type profileView struct {
	ID               int64  `json:"id"`
	Username         string `json:"username"`
	Role             string `json:"role"`
	Status           string `json:"status"`
	RegisteredAt     string `json:"registered_at"`
	TotalOnlineSec   int64  `json:"total_online_seconds"`
	AvatarStatus     string `json:"avatar_status"` // none / pending / approved / rejected
	AvatarNote       string `json:"avatar_note"`   // 驳回原因
	AvatarURL        string `json:"avatar_url"`    // 审核通过后才有值
	AvatarReviewedAt string `json:"avatar_reviewed_at"`
}

// avatarStatusOf 根据状态决定对外暴露的 URL。
func avatarURLFor(id int64, status string) string {
	// 审核通过才提供图片；pending / rejected 一律回退到字母头像，
	// 避免未审核内容被其他用户看到。
	if status == "approved" {
		return fmt.Sprintf("/api/avatars/%d", id)
	}
	return ""
}

// handleMe GET /api/auth/me
//
// 返回当前用户资料。前端登录态原本只存在 localStorage（用户名+角色），
// 这里补上 UID、注册时间、累计在线与头像状态。
func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	uid := currentUserID(r)

	var v profileView
	var avatarReviewedAt sql.NullTime
	err := s.db.QueryRow(`
		SELECT id, username, role, status, created_at,
		       total_online_seconds, avatar_status, avatar_note, avatar_reviewed_at
		FROM users WHERE id = ?`, uid).
		Scan(&v.ID, &v.Username, &v.Role, &v.Status, &v.RegisteredAt,
			&v.TotalOnlineSec, &v.AvatarStatus, &v.AvatarNote, &avatarReviewedAt)
	if err != nil {
		writeErr(w, http.StatusNotFound, "用户不存在")
		return
	}
	v.AvatarURL = avatarURLFor(v.ID, v.AvatarStatus)
	if avatarReviewedAt.Valid {
		v.AvatarReviewedAt = avatarReviewedAt.Time.Format(time.RFC3339)
	}
	writeJSON(w, http.StatusOK, v)
}

// touchOnline 累计用户在线时长。
//
// 策略：以相邻两次请求的间隔累加，但**单次间隔上限 5 分钟** ——
// 否则用户关掉页面去睡觉，下次打开会被计入一大段"在线"。
// 同时做写节流：间隔不足 30 秒不写库，避免每个请求都产生一次 UPDATE。
func (s *Server) touchOnline(userID int64) {
	var last sql.NullTime
	if err := s.db.QueryRow(`SELECT last_active_at FROM users WHERE id = ?`, userID).
		Scan(&last); err != nil {
		return
	}
	now := time.Now()
	if last.Valid {
		gap := now.Sub(last.Time)
		if gap < 30*time.Second {
			return // 节流：过近的请求不写库
		}
		add := int64(gap.Seconds())
		if add > 300 {
			add = 300 // 上限 5 分钟，避免把长时间离开算成在线
		}
		_, _ = s.db.Exec(
			`UPDATE users SET total_online_seconds = total_online_seconds + ?, last_active_at = CURRENT_TIMESTAMP WHERE id = ?`,
			add, userID)
		return
	}
	_, _ = s.db.Exec(`UPDATE users SET last_active_at = CURRENT_TIMESTAMP WHERE id = ?`, userID)
}

// handleUploadAvatar POST /api/my/avatar （multipart/form-data，字段名 avatar）
//
// 上传后进入「待审核」状态，由管理员批准后才对外可见。
func (s *Server) handleUploadAvatar(w http.ResponseWriter, r *http.Request) {
	uid := currentUserID(r)

	r.Body = http.MaxBytesReader(w, r.Body, maxAvatarBytes+1<<20)
	if err := r.ParseMultipartForm(maxAvatarBytes + 1<<20); err != nil {
		writeErr(w, http.StatusBadRequest, "文件过大或表单格式错误（上限 2 MB）")
		return
	}
	file, hdr, err := r.FormFile("avatar")
	if err != nil {
		writeErr(w, http.StatusBadRequest, "缺少 avatar 字段")
		return
	}
	defer file.Close()

	if hdr.Size > maxAvatarBytes {
		writeErr(w, http.StatusBadRequest, "图片超过 2 MB 上限")
		return
	}

	// 读入前 512 字节做内容嗅探，不信任客户端声明的 Content-Type
	head := make([]byte, 512)
	n, _ := io.ReadFull(file, head)
	head = head[:n]
	ext, ok := allowAvatarExt[http.DetectContentType(head)]
	if !ok {
		writeErr(w, http.StatusBadRequest, "仅支持 JPG / PNG / WebP / GIF 图片")
		return
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		writeErr(w, http.StatusInternalServerError, "读取上传内容失败")
		return
	}

	dir := s.avatarDir
	if err := os.MkdirAll(dir, 0o755); err != nil {
		writeErr(w, http.StatusInternalServerError, "创建头像目录失败: "+err.Error())
		return
	}

	// 文件名只由用户 ID 决定（外加扩展名），不采用用户提供的文件名，
	// 从根本上避免路径穿越与同名覆盖。
	name := fmt.Sprintf("%d%s", uid, ext)
	dst := filepath.Join(dir, name)
	tmp := dst + ".tmp"

	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "保存头像失败: "+err.Error())
		return
	}
	if _, err := io.Copy(out, file); err != nil {
		out.Close()
		os.Remove(tmp)
		writeErr(w, http.StatusInternalServerError, "写入头像失败: "+err.Error())
		return
	}
	out.Close()
	// 先写临时文件再改名：避免上传中断留下半个文件被当成有效头像
	if err := os.Rename(tmp, dst); err != nil {
		os.Remove(tmp)
		writeErr(w, http.StatusInternalServerError, "提交头像失败: "+err.Error())
		return
	}

	// 清掉旧的其它扩展名文件，避免残留
	for _, e := range []string{".jpg", ".png", ".webp", ".gif"} {
		if e != ext {
			_ = os.Remove(filepath.Join(dir, fmt.Sprintf("%d%s", uid, e)))
		}
	}

	if _, err := s.db.Exec(`
		UPDATE users SET avatar_file = ?, avatar_status = 'pending',
		       avatar_note = '', avatar_reviewed_by = 0, avatar_reviewed_at = NULL
		WHERE id = ?`, name, uid); err != nil {
		writeErr(w, http.StatusInternalServerError, "更新头像状态失败: "+err.Error())
		return
	}

	s.audit(r, "upload_avatar", strconv.FormatInt(uid, 10), "等待管理员审核")
	writeJSON(w, http.StatusOK, map[string]string{
		"message": "头像已上传，等待管理员审核",
		"status":  "pending",
	})
}

// handleGetAvatar GET /api/avatars/{id}
//
// 只返回**已审核通过**的头像；其它情况一律 404，前端回退到字母头像。
func (s *Server) handleGetAvatar(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	var file, status string
	if err := s.db.QueryRow(`SELECT avatar_file, avatar_status FROM users WHERE id = ?`, id).
		Scan(&file, &status); err != nil || file == "" || status != "approved" {
		http.NotFound(w, r)
		return
	}
	// file 由服务端生成（<uid><ext>），这里再做一次基名校验以防脏数据
	path := filepath.Join(s.avatarDir, filepath.Base(file))
	w.Header().Set("Cache-Control", "public, max-age=300")
	http.ServeFile(w, r, path)
}

// handleDeleteMyAvatar DELETE /api/my/avatar —— 撤回待审核或删除已有头像
func (s *Server) handleDeleteMyAvatar(w http.ResponseWriter, r *http.Request) {
	uid := currentUserID(r)
	var file string
	_ = s.db.QueryRow(`SELECT avatar_file FROM users WHERE id = ?`, uid).Scan(&file)
	if file != "" {
		_ = os.Remove(filepath.Join(s.avatarDir, filepath.Base(file)))
	}
	_, _ = s.db.Exec(`
		UPDATE users SET avatar_file = '', avatar_status = 'none', avatar_note = '',
		       avatar_reviewed_by = 0, avatar_reviewed_at = NULL WHERE id = ?`, uid)
	s.audit(r, "delete_avatar", strconv.FormatInt(uid, 10), "")
	writeJSON(w, http.StatusOK, map[string]string{"message": "头像已移除"})
}

// ---- 管理员审核 ----

// avatarReviewItem 待审核/已审核列表项
type avatarReviewItem struct {
	UserID     int64  `json:"user_id"`
	Username   string `json:"username"`
	Role       string `json:"role"`
	Status     string `json:"avatar_status"`
	Note       string `json:"avatar_note"`
	PreviewURL string `json:"preview_url"` // 审核时需看到待审图片本身
	ReviewedAt string `json:"avatar_reviewed_at"`
	ReviewedBy string `json:"avatar_reviewed_by_name"`
}

// handleListAvatars GET /api/admin/avatars?status=pending
//
// 管理员审核用列表。**与对外接口不同，这里会给出待审图片的预览地址** ——
// 否则管理员无从判断该不该通过。
func (s *Server) handleListAvatars(w http.ResponseWriter, r *http.Request) {
	if !requireAdminIn(w, r) {
		return
	}
	status := r.URL.Query().Get("status")
	if status == "" {
		status = "pending"
	}

	q := `SELECT u.id, u.username, u.role, u.avatar_status, u.avatar_note, u.avatar_reviewed_at,
	             COALESCE(r.username, '')
	      FROM users u LEFT JOIN users r ON r.id = u.avatar_reviewed_by
	      WHERE u.avatar_status != 'none'`
	args := []interface{}{}
	if status != "all" {
		q += ` AND u.avatar_status = ?`
		args = append(args, status)
	}
	q += ` ORDER BY u.avatar_status = 'pending' DESC, u.id`

	rows, err := s.db.Query(q, args...)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()

	list := []avatarReviewItem{}
	for rows.Next() {
		var it avatarReviewItem
		var reviewedAt sql.NullTime
		if err := rows.Scan(&it.UserID, &it.Username, &it.Role, &it.Status, &it.Note,
			&reviewedAt, &it.ReviewedBy); err != nil {
			continue
		}
		if reviewedAt.Valid {
			it.ReviewedAt = reviewedAt.Time.Format(time.RFC3339)
		}
		// 审核预览：待审图片尚未对外公开，走管理员专用地址
		if it.Status == "pending" || it.Status == "approved" {
			it.PreviewURL = fmt.Sprintf("/api/admin/avatars/%d/raw", it.UserID)
		}
		list = append(list, it)
	}
	writeJSON(w, http.StatusOK, list)
}

// handleGetAvatarRaw GET /api/admin/avatars/{id}/raw —— 管理员预览（含待审核）
func (s *Server) handleGetAvatarRaw(w http.ResponseWriter, r *http.Request) {
	if !requireAdminIn(w, r) {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	var file string
	if err := s.db.QueryRow(`SELECT avatar_file FROM users WHERE id = ?`, id).Scan(&file); err != nil || file == "" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	http.ServeFile(w, r, filepath.Join(s.avatarDir, filepath.Base(file)))
}

// handleReviewAvatar POST /api/admin/avatars/{id}/review
// body: { "approve": true, "note": "驳回原因" }
func (s *Server) handleReviewAvatar(w http.ResponseWriter, r *http.Request) {
	if !requireAdminIn(w, r) {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "id 无效")
		return
	}
	var req struct {
		Approve bool   `json:"approve"`
		Note    string `json:"note"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "无效请求体")
		return
	}

	var status, file string
	if err := s.db.QueryRow(`SELECT avatar_file, avatar_status FROM users WHERE id = ?`, id).
		Scan(&file, &status); err != nil {
		writeErr(w, http.StatusNotFound, "用户不存在")
		return
	}
	if file == "" {
		writeErr(w, http.StatusBadRequest, "该用户没有待审核的头像")
		return
	}
	if status == "approved" && req.Approve {
		writeJSON(w, http.StatusOK, map[string]string{"message": "该头像已通过审核"})
		return
	}

	decision := "rejected"
	msg := "已驳回"
	if req.Approve {
		decision = "approved"
		msg = "已通过"
	}
	note := strings.TrimSpace(req.Note)
	if note == "" && !req.Approve {
		note = "未通过审核"
	}

	if _, err := s.db.Exec(`
		UPDATE users SET avatar_status = ?, avatar_note = ?,
		       avatar_reviewed_by = ?, avatar_reviewed_at = CURRENT_TIMESTAMP
		WHERE id = ?`, decision, note, currentUserID(r), id); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	// 驳回时保留文件（便于用户查看被拒的是哪张），但不再对外提供
	s.audit(r, "review_avatar", strconv.FormatInt(id, 10), msg+" "+note)
	writeJSON(w, http.StatusOK, map[string]string{"message": msg, "status": decision})
}
