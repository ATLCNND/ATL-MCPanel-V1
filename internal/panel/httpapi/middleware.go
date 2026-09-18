package httpapi

import (
	"context"
	"net/http"
	"strings"
)

type ctxKey string

const (
	ctxKeyUserID ctxKey = "userID"
	ctxKeyRole   ctxKey = "role"
)

// requireAuth 鉴权中间件：校验 JWT，将 userID/role 注入 context。
func (s *Server) requireAuth(next func(http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := extractToken(r)
		if token == "" {
			writeErr(w, http.StatusUnauthorized, "未登录")
			return
		}
		claims, err := s.auth.ParseToken(token)
		if err != nil {
			writeErr(w, http.StatusUnauthorized, "令牌无效或已过期")
			return
		}
		ctx := context.WithValue(r.Context(), ctxKeyUserID, claims.UserID)
		ctx = context.WithValue(ctx, ctxKeyRole, claims.Role)
		// 累计在线时长（内部有写节流，不会每个请求都写库）
		s.touchOnline(claims.UserID)
		next(w, r.WithContext(ctx))
	}
}

// requireAdmin 要求 admin 角色。
func (s *Server) requireAdmin(next func(http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return s.requireAuth(func(w http.ResponseWriter, r *http.Request) {
		role, _ := r.Context().Value(ctxKeyRole).(string)
		if role != "admin" {
			writeErr(w, http.StatusForbidden, "需要管理员权限")
			return
		}
		next(w, r)
	})
}

// extractToken 从 Authorization header 提取 JWT。
func extractToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if strings.HasPrefix(h, "Bearer ") {
		return strings.TrimPrefix(h, "Bearer ")
	}
	return ""
}

// extractTokenWithQuery 在 header 之外还接受 ?token=。
//
// 只给「浏览器直接发起的下载」使用：<a href> / window.open 无法附带
// 自定义请求头，而下载又必须带鉴权。把查询参数鉴权限制在这一条路由上，
// 是为了不让令牌有机会出现在其它接口的 URL 里（URL 会进访问日志与 Referer）。
func extractTokenWithQuery(r *http.Request) string {
	if t := extractToken(r); t != "" {
		return t
	}
	return r.URL.Query().Get("token")
}

// requireAuthQuery 与 requireAuth 等价，但允许用查询参数传令牌。
func (s *Server) requireAuthQuery(next func(http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := extractTokenWithQuery(r)
		if token == "" {
			writeErr(w, http.StatusUnauthorized, "未登录")
			return
		}
		claims, err := s.auth.ParseToken(token)
		if err != nil {
			writeErr(w, http.StatusUnauthorized, "令牌无效或已过期")
			return
		}
		ctx := context.WithValue(r.Context(), ctxKeyUserID, claims.UserID)
		ctx = context.WithValue(ctx, ctxKeyRole, claims.Role)
		s.touchOnline(claims.UserID)
		next(w, r.WithContext(ctx))
	}
}

// requireAdminIn 在 handler 内部校验管理员身份（用于路由已挂 requireAuth 的场景）。
func requireAdminIn(w http.ResponseWriter, r *http.Request) bool {
	role, _ := r.Context().Value(ctxKeyRole).(string)
	if role != "admin" {
		writeErr(w, http.StatusForbidden, "需要管理员权限")
		return false
	}
	return true
}

// optionalAuth 若请求携带合法 token 则注入用户上下文，但不强制要求登录。
// 用于「首个用户可自助注册成为管理员」这类需要区分匿名/登录态、但又不强制登录的接口。
func (s *Server) optionalAuth(next func(http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if token := extractToken(r); token != "" {
			if claims, err := s.auth.ParseToken(token); err == nil {
				ctx := context.WithValue(r.Context(), ctxKeyUserID, claims.UserID)
				ctx = context.WithValue(ctx, ctxKeyRole, claims.Role)
				r = r.WithContext(ctx)
			}
		}
		next(w, r)
	}
}

// currentUserID 从 context 取当前用户 ID。
func currentUserID(r *http.Request) int64 {
	id, _ := r.Context().Value(ctxKeyUserID).(int64)
	return id
}
