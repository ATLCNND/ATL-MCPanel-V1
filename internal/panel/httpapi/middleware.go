package httpapi

import (
	"context"
	"net/http"
	"strings"

	"github.com/ATLCNND/ATL-MCPanel/internal/panel/auth"
)

type ctxKey string

const (
	ctxKeyUserID ctxKey = "userID"
	ctxKeyRole   ctxKey = "role"
)

// authUser 在令牌签名校验通过之后，再用**数据库**确认这个用户仍然存在、
// 状态正常、且令牌世代号对得上，并返回**库里**的角色。
//
// 为什么必须查库（2026-10-01 安全审查）：JWT 是自证明的，签发之后面板无法
// 撤销，有效期 24 小时。于是下面这些"应该立刻生效"的管理动作原先全都要等
// 令牌自然过期：
//   - 管理员删掉被盗账号   → 库里查不到 → 立刻 401
//   - 管理员重置密码       → token_version 已 +1 → 立刻 401
//   - 管理员把 admin 降级  → 角色以库里为准 → 立刻失去管理权限
//
// 只按主键查一行，且用 QueryRow（Scan 完连接立即归还，不占着 rows），
// 对连接池的压力可以忽略。
func (s *Server) authUser(claims *auth.Claims) (int64, string, bool) {
	var role, state string
	var tv int64
	err := s.db.QueryRow(
		`SELECT role, COALESCE(status,''), COALESCE(token_version,0) FROM users WHERE id = ?`,
		claims.UserID).Scan(&role, &state, &tv)
	if err != nil {
		return 0, "", false
	}
	if state != "" && state != "active" {
		return 0, "", false
	}
	if tv != claims.TokenVersion {
		return 0, "", false
	}
	return claims.UserID, role, true
}

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
		uid, role, ok := s.authUser(claims)
		if !ok {
			writeErr(w, http.StatusUnauthorized, "登录状态已失效，请重新登录")
			return
		}
		ctx := context.WithValue(r.Context(), ctxKeyUserID, uid)
		ctx = context.WithValue(ctx, ctxKeyRole, role)
		// 累计在线时长（内部有写节流，不会每个请求都写库）
		s.touchOnline(uid)
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
		uid, role, ok := s.authUser(claims)
		if !ok {
			writeErr(w, http.StatusUnauthorized, "登录状态已失效，请重新登录")
			return
		}
		ctx := context.WithValue(r.Context(), ctxKeyUserID, uid)
		ctx = context.WithValue(ctx, ctxKeyRole, role)
		s.touchOnline(uid)
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

// 这里原先还有一个 optionalAuth（"有令牌就注入上下文、没有也照常放行"）。
// 2026-10-12 随 POST /api/users 一起删掉了，理由是它是那条漏洞的载体：
//
//	它唯一的用途是给「首个用户可自助注册成为管理员」开口子，而那条路让
//	**未初始化面板**的管理员可以被任何能访问该端口的人抢注 —— 体检时实测
//	匿名 POST /api/users 直接返回 201 + role=admin。更要命的是前端从未调用过
//	它，运维者根本不知道这条入口存在。
//
//	"初始化端点必须免鉴权"这个前提本身就是漏洞源：Portainer 的 /api/restore
//	为了让"首管创建前也能恢复备份"而故意不鉴权，结果未初始化实例可被接管
//	（GHSA-x626-fcwx-f5pc, High）。首个管理员现在只能由本机 CLI 创建
//	（cmd/panel/user.go 的 `user create`，默认角色就是 admin），那需要 shell 权限。
//
// 将来若真需要"匿名与登录态都接受"的接口，请在该接口处显式论证为什么安全，
// 不要恢复这个通用中间件 —— 它太容易被顺手接到新的初始化端点上。

// currentUserID 从 context 取当前用户 ID。
func currentUserID(r *http.Request) int64 {
	id, _ := r.Context().Value(ctxKeyUserID).(int64)
	return id
}
