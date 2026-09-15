package httpapi

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ============================================================================
// 公告与帮助文档
// ============================================================================
//
// 定位：这两样都是"管理员对全体用户说话"的渠道 ——
// 公告偏"现在发生的事"（维护、迁移、临时调整），帮助偏"一直成立的事"（怎么用面板）。
// 所以做在同一个页面里：管理员在这里写，用户在这里看。
//
// 权限：**读给所有登录用户，写只给总管理员**。
// 不做"已读/未读"（那需要 announcement_reads 表 + 每条公告的已读记录），
// 先只做展示 —— 加了未读反而容易变成"红点消不掉"的噪音源。

// announcementView 一条公告。
type announcementView struct {
	ID        int64  `json:"id"`
	Title     string `json:"title"`
	Body      string `json:"body"`
	Pinned    bool   `json:"pinned"`
	Published bool   `json:"published"`
	CreatedBy string `json:"created_by_name"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// handleListAnnouncements GET /api/announcements
//
// 普通用户只看得到**已发布**的；总管理员额外看得到草稿（否则他存了草稿就再也找不回来）。
// 排序：置顶优先，再按时间倒序 —— 与索引 idx_announcements_visible 一致。
func (s *Server) handleListAnnouncements(w http.ResponseWriter, r *http.Request) {
	admin := isAdmin(r)
	q := `
		SELECT a.id, a.title, a.body, a.pinned, a.published,
		       COALESCE(u.username, ''), a.created_at, a.updated_at
		FROM announcements a
		LEFT JOIN users u ON u.id = a.created_by`
	if !admin {
		q += ` WHERE a.published = 1`
	}
	q += ` ORDER BY a.pinned DESC, a.created_at DESC, a.id DESC`

	rows, err := s.db.Query(q)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()

	list := []announcementView{}
	for rows.Next() {
		var v announcementView
		var pinned, published int
		if err := rows.Scan(&v.ID, &v.Title, &v.Body, &pinned, &published,
			&v.CreatedBy, &v.CreatedAt, &v.UpdatedAt); err != nil {
			continue
		}
		v.Pinned = pinned == 1
		v.Published = published == 1
		list = append(list, v)
	}
	writeJSON(w, http.StatusOK, list)
}

// announcementReq 新建/修改公告的请求体。
type announcementReq struct {
	Title     string `json:"title"`
	Body      string `json:"body"`
	Pinned    *bool  `json:"pinned"`
	Published *bool  `json:"published"`
}

// handleCreateAnnouncement POST /api/announcements （仅总管理员）
func (s *Server) handleCreateAnnouncement(w http.ResponseWriter, r *http.Request) {
	if !requireAdminIn(w, r) {
		return
	}
	var req announcementReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "无效请求体")
		return
	}
	req.Title = strings.TrimSpace(req.Title)
	if req.Title == "" {
		writeErr(w, http.StatusBadRequest, "标题不能为空")
		return
	}
	if len([]rune(req.Title)) > 120 {
		writeErr(w, http.StatusBadRequest, "标题过长（最多 120 字）")
		return
	}
	if len([]rune(req.Body)) > 8000 {
		writeErr(w, http.StatusBadRequest, "正文过长（最多 8000 字）")
		return
	}
	pinned, published := 0, 1
	if req.Pinned != nil && *req.Pinned {
		pinned = 1
	}
	if req.Published != nil && !*req.Published {
		published = 0
	}

	res, err := s.db.Exec(`
		INSERT INTO announcements (title, body, pinned, published, created_by)
		VALUES (?, ?, ?, ?, ?)`,
		req.Title, req.Body, pinned, published, currentUserID(r))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	id, _ := res.LastInsertId()
	s.audit(r, "create_announcement", strconv.FormatInt(id, 10), req.Title)
	// 草稿要说成草稿：否则管理员以为已经公布了，实际全站都看不到
	msg := "公告已发布"
	if published == 0 {
		msg = "公告已存为草稿（普通用户看不到，勾选「发布」后才会公开）"
	}
	writeJSON(w, http.StatusCreated, map[string]interface{}{
		"id": id, "published": published == 1, "message": msg,
	})
}

// handleUpdateAnnouncement PUT /api/announcements/{id} （仅总管理员）
func (s *Server) handleUpdateAnnouncement(w http.ResponseWriter, r *http.Request) {
	if !requireAdminIn(w, r) {
		return
	}
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if id == 0 {
		writeErr(w, http.StatusBadRequest, "无效的公告 id")
		return
	}
	var req announcementReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "无效请求体")
		return
	}
	req.Title = strings.TrimSpace(req.Title)
	if req.Title == "" {
		writeErr(w, http.StatusBadRequest, "标题不能为空")
		return
	}
	if len([]rune(req.Title)) > 120 || len([]rune(req.Body)) > 8000 {
		writeErr(w, http.StatusBadRequest, "标题或正文过长")
		return
	}
	// 指针为 nil 表示"这次不改这一项"，保留库里的原值
	var curPinned, curPublished int
	if err := s.db.QueryRow(
		`SELECT pinned, published FROM announcements WHERE id = ?`, id).
		Scan(&curPinned, &curPublished); err != nil {
		writeErr(w, http.StatusNotFound, "公告不存在")
		return
	}
	if req.Pinned != nil {
		curPinned = 0
		if *req.Pinned {
			curPinned = 1
		}
	}
	if req.Published != nil {
		curPublished = 0
		if *req.Published {
			curPublished = 1
		}
	}

	if _, err := s.db.Exec(`
		UPDATE announcements
		SET title = ?, body = ?, pinned = ?, published = ?, updated_at = CURRENT_TIMESTAMP
		WHERE id = ?`, req.Title, req.Body, curPinned, curPublished, id); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.audit(r, "update_announcement", strconv.FormatInt(id, 10), req.Title)
	writeJSON(w, http.StatusOK, map[string]string{"message": "公告已保存"})
}

// handleDeleteAnnouncement DELETE /api/announcements/{id} （仅总管理员）
func (s *Server) handleDeleteAnnouncement(w http.ResponseWriter, r *http.Request) {
	if !requireAdminIn(w, r) {
		return
	}
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if id == 0 {
		writeErr(w, http.StatusBadRequest, "无效的公告 id")
		return
	}
	res, err := s.db.Exec(`DELETE FROM announcements WHERE id = ?`, id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		writeErr(w, http.StatusNotFound, "公告不存在")
		return
	}
	s.audit(r, "delete_announcement", strconv.FormatInt(id, 10), "")
	writeJSON(w, http.StatusOK, map[string]string{"message": "公告已删除"})
}

// ============================================================================
// 帮助文档（单行表，所有人可读，仅总管理员可改）
// ============================================================================

// handleGetHelp GET /api/help
func (s *Server) handleGetHelp(w http.ResponseWriter, r *http.Request) {
	var content, updatedAt string
	var updatedBy int64
	err := s.db.QueryRow(`
		SELECT content, updated_by, updated_at FROM help_doc WHERE id = 1`).
		Scan(&content, &updatedBy, &updatedAt)
	if err == sql.ErrNoRows {
		// 迁移里插过一行；真没有也不该 500 —— 当成"还没写过"返回空文档
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"content": "", "updated_at": "", "updated_by_name": "",
		})
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	var byName string
	_ = s.db.QueryRow(`SELECT username FROM users WHERE id = ?`, updatedBy).Scan(&byName)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"content": content, "updated_at": updatedAt, "updated_by_name": byName,
	})
}

// handleSaveHelp PUT /api/help （仅总管理员）
func (s *Server) handleSaveHelp(w http.ResponseWriter, r *http.Request) {
	if !requireAdminIn(w, r) {
		return
	}
	var req struct {
		Content string `json:"content"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "无效请求体")
		return
	}
	// 上限给得比公告宽：帮助文档本来就是长文
	if len([]rune(req.Content)) > 100000 {
		writeErr(w, http.StatusBadRequest, "帮助文档过长（最多 10 万字）")
		return
	}
	if _, err := s.db.Exec(`
		INSERT INTO help_doc (id, content, updated_by, updated_at)
		VALUES (1, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(id) DO UPDATE SET
			content = excluded.content,
			updated_by = excluded.updated_by,
			updated_at = CURRENT_TIMESTAMP`,
		req.Content, currentUserID(r)); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.audit(r, "update_help_doc", "help", strconv.Itoa(len([]rune(req.Content)))+" 字")
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"message": "帮助文档已保存", "updated_at": time.Now().Format("2006-01-02 15:04:05"),
	})
}
