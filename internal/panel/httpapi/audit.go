package httpapi

import (
	"net"
	"net/http"
	"strconv"
	"strings"
)

// audit 记录一条审计日志（失败不阻断主流程）。
func (s *Server) audit(r *http.Request, action, target, detail string) {
	s.auditAs(currentUserID(r), r, action, target, detail)
}

// auditAs 以指定用户身份记录审计日志（用于登录等尚未建立上下文的场景）。
func (s *Server) auditAs(userID int64, r *http.Request, action, target, detail string) {
	ip := s.clientIP(r)
	_, _ = s.db.Exec(
		`INSERT INTO audit_logs (user_id, action, target, detail, ip) VALUES (?, ?, ?, ?, ?)`,
		userID, action, target, detail, ip,
	)
}

// clientIP 提取客户端 IP。
// 当面板位于 frp / 反向代理之后时（TrustProxy 开启），优先取 X-Forwarded-For 首项；
// 否则使用 TCP 对端地址（避免伪造头绕过限流）。
func (s *Server) clientIP(r *http.Request) string {
	if s.trustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			if first := strings.TrimSpace(strings.Split(xff, ",")[0]); first != "" {
				return first
			}
		}
		if xr := strings.TrimSpace(r.Header.Get("X-Real-IP")); xr != "" {
			return xr
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// handleListAuditLogs 查询审计日志（仅 admin），支持 ?limit= 与 ?username= 过滤。
func (s *Server) handleListAuditLogs(w http.ResponseWriter, r *http.Request) {
	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 1000 {
			limit = n
		}
	}

	// 支持按操作类型 / 用户名筛选。
	// 只给 limit 的话，翻查"谁删除过实例"这类问题只能靠肉眼扫最近 100 条。
	action := strings.TrimSpace(r.URL.Query().Get("action"))
	username := strings.TrimSpace(r.URL.Query().Get("username"))

	q := `
		SELECT a.id, COALESCE(u.username, '(已删除)'), a.action, a.target, a.detail, a.ip, a.created_at
		FROM audit_logs a
		LEFT JOIN users u ON u.id = a.user_id
		WHERE 1 = 1`
	args := []interface{}{}
	if action != "" {
		q += " AND a.action LIKE ?"
		args = append(args, "%"+action+"%")
	}
	if username != "" {
		q += " AND u.username LIKE ?"
		args = append(args, "%"+username+"%")
	}
	q += " ORDER BY a.id DESC LIMIT ?"
	args = append(args, limit)

	rows, err := s.db.Query(q, args...)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()

	type logItem struct {
		ID        int64  `json:"id"`
		Username  string `json:"username"`
		Action    string `json:"action"`
		Target    string `json:"target"`
		Detail    string `json:"detail"`
		IP        string `json:"ip"`
		CreatedAt string `json:"created_at"`
	}
	list := []logItem{}
	for rows.Next() {
		var it logItem
		if err := rows.Scan(&it.ID, &it.Username, &it.Action, &it.Target, &it.Detail, &it.IP, &it.CreatedAt); err != nil {
			continue
		}
		list = append(list, it)
	}
	writeJSON(w, http.StatusOK, list)
}