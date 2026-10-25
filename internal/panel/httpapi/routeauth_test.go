package httpapi

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// 本文件是 docs/V1-CLOSEOUT.md §5.1 里那条「全接口 × 全角色权限矩阵」里
// **最容易做、也最能兜住事的一轴**：哪些端点匿名就能调。
//
// ---------------------------------------------------------------------------
// 为什么要它（2026-10-12 的真实事故）
// ---------------------------------------------------------------------------
// `POST /api/users` 原先挂在 `optionalAuth` 上（"有令牌就注入、没有也放行"），
// 而 handler 里有一段"users 表为空 → 把请求者建为 admin"。于是**任何能访问
// 面板端口的人，一条 POST 就能拿到管理员**，而前端从未调用过这个接口 ——
// 它是一条运维者不知道存在的隐藏入口。
//
// 那次它躲过了 2026-10-01 的整轮安全审查，原因是那次审查**没有做"逐个端点
// 核对鉴权中间件"**这件事。而这类错误的特征恰恰是"机械可查、人眼易漏"：
// 路由表里 150+ 条，中间件写错一个，读代码时看不出来。
//
// 所以这里用**静态核对**（解析 server.go 的路由表）把它变成一条会自己报警的
// 断言：**匿名可达的路由集必须精确等于下面这份白名单**。
// 多一条就失败 —— 那正是"有人注册了一个不需要鉴权的端点"的形态。
//
// ---------------------------------------------------------------------------
// 为什么是"读源码"这种看起来不优雅的做法
// ---------------------------------------------------------------------------
// Go 的 `http.ServeMux` 不暴露已注册的 pattern，运行时无法枚举；而手工维护一张
// 154 条路由的表必然漂移（漏改一次，这条断言就失去意义）。两害相权，读源码最可靠。
// 代价是它对**本文件的解析逻辑**敏感，所以下面加了两道防"静默失效"的闸：
//   - 解析出的路由数不得低于 routeCountFloor；
//   - 白名单里的每一条都必须真的被解析到（否则说明解析坏了或路由被删了）。
// 没有这两道闸，正则一旦失效就会"解析出 0 条路由 → 白名单为空 → 测试通过"，
// 那比没有测试更糟。

// routeCountFloor 路由数下限。
//
// 取值比当前实际条数略低（留出"合并路由"的余地），但**明显高于**任何
// "正则失效"时会得到的数字。改动路由时若低于它，说明要么真的删了大量路由，
// 要么解析坏了 —— 两种情况都该有人看一眼再调这个常量。
const routeCountFloor = 140

// allowedAnonymous 允许匿名可达（不挂任何鉴权中间件）的路由，必须精确匹配。
//
// 每条都写清"为什么可以" —— 这是这份白名单存在的意义：它是一份**需要论证的
// 清单**，不是一份记录现状的快照。
var allowedAnonymous = []struct {
	pattern string // `METHOD /path`；裸 `/` 表示不带方法的兜底路由
	why     string
}{
	{"GET /api/health", "存活探针：部署脚本、负载均衡、systemd 健康检查都要能调，且不返回任何敏感信息"},
	{"GET /api/meta", "登录页在**拿到令牌之前**就要显示面板名与版本；目前会带上 commit / go_version（给扫描器的指纹信息，属可接受的取舍）"},
	{"POST /api/auth/login", "登录本身。它是唯一的凭据入口，失败限流在 ratelimit.go（用户 5 次/15 分钟、IP 50 次/10 分钟）"},
	{"GET /api/avatars/{id}", "用户头像：`<img src>` 带不上 Authorization 头。handler 只返回**已审核通过**的头像，未通过的取不到"},
	{"GET /ws/console/{id}", "控制台 WebSocket：浏览器无法给 WS 握手加自定义头，所以 handler 内部自己校验 `?token=` 的 JWT（缺 token 直接 401）"},
	// 凭证交换：节点此时既没有身份也没有证书，无法先登录。安全性由**一次性 token** 承担
	//（15 分钟、用后即焚、失败不透露原因），并按来源 IP 限速 10 次/分钟（见 enroll.go 的 enrollLimiter）。
	// 签发 token 的 POST /api/enroll/token 仍是 requireAuth 的管理员接口。
	{"POST /api/enroll", "凭证交换换证书；靠一次性 token 鉴权 + IP 限速，不能挂 requireAuth（节点还没有身份）"},
	// 续期：不要求登录 token，凭据是当前仍有效的客户端证书（走 mTLS，handler 自己校验:
	// 证书存在、未过期、URI SAN 里的 node_uid 未被撤销、新 CSR 公钥指纹与首次登记一致）。
	{"POST /api/enroll/renew", "证书续期；靠 mTLS 的当前证书鉴权，不能挂 requireAuth（节点没有登录态）"},
	{"/", "前端静态资源兜底（SPA 路由）。只是个静态文件服务器，不含任何接口"},
}

// routeLine 从 server.go 里解析出的一条路由。
type routeLine struct {
	method  string
	path    string
	handler string
	line    int
	raw     string
}

var (
	reHandleFunc = regexp.MustCompile(`^mux\.HandleFunc\("([^"]+)",\s*(.+)\)$`)
	reAdmin      = regexp.MustCompile(`requireAdmin\(`)
	reAuthQuery  = regexp.MustCompile(`requireAuthQuery\(`)
	reAuth       = regexp.MustCompile(`requireAuth\(`)
)

// parseRoutes 解析 server.go 的 `mux.HandleFunc(...)` 行。
func parseRoutes(t *testing.T) []routeLine {
	t.Helper()
	src, err := os.ReadFile("server.go")
	if err != nil {
		t.Fatalf("读 server.go 失败（本用例必须在 internal/panel/httpapi 目录下跑）: %v", err)
	}

	var out []routeLine
	for i, l := range strings.Split(string(src), "\n") {
		line := strings.TrimSpace(l)
		if !strings.HasPrefix(line, "mux.HandleFunc(") {
			continue
		}
		// 行尾注释要先去掉：有路由就是带注释的（如头像那条），
		// 不去掉会导致正则匹配不上，于是那条路由**静默地不进任何分类**。
		if idx := strings.LastIndex(line, " // "); idx >= 0 {
			line = strings.TrimSpace(line[:idx])
		}
		m := reHandleFunc.FindStringSubmatch(line)
		if m == nil {
			// 解析不了就必须显式失败：静默跳过等于漏检。
			t.Errorf("server.go:%d 这行 HandleFunc 解析不了，请更新本文件的解析逻辑：\n    %s", i+1, line)
			continue
		}
		pattern, handler := m[1], m[2]
		method, path := "(任意)", pattern
		if sp := strings.IndexByte(pattern, ' '); sp > 0 {
			method, path = pattern[:sp], pattern[sp+1:]
		}
		out = append(out, routeLine{method: method, path: path, handler: handler, line: i + 1, raw: line})
	}
	return out
}

// hasAuthMiddleware 该路由是否挂了鉴权中间件。
func hasAuthMiddleware(handler string) bool {
	return reAdmin.MatchString(handler) || reAuthQuery.MatchString(handler) || reAuth.MatchString(handler)
}

// key 归一成白名单里的写法。
func (r routeLine) key() string {
	if r.method == "(任意)" {
		return r.path
	}
	return r.method + " " + r.path
}

// TestAnonymousRoutesAreExactlyAllowlisted 是本次修复的**机械防线**：
// 匿名可达的路由集必须精确等于 allowedAnonymous，一条不多、一条不少。
//
// 它防的正是 2026-10-12 那次事故：把需要鉴权的端点挂到了放行的中间件上。
func TestAnonymousRoutesAreExactlyAllowlisted(t *testing.T) {
	routes := parseRoutes(t)
	if len(routes) < routeCountFloor {
		t.Fatalf("只解析出 %d 条路由（下限 %d）—— 要么路由被大量删除，要么本文件的解析逻辑已失效。"+
			"**解析失效会让这个用例变成永远通过的空断言**，比没有它更糟，所以这里必须失败。", len(routes), routeCountFloor)
	}

	got := map[string]routeLine{}
	for _, r := range routes {
		if hasAuthMiddleware(r.handler) {
			continue
		}
		got[r.key()] = r
	}

	want := map[string]string{}
	for _, a := range allowedAnonymous {
		want[a.pattern] = a.why
	}

	// 多出来的：新注册了一个匿名可达的端点
	for k, r := range got {
		if _, ok := want[k]; !ok {
			t.Errorf("server.go:%d 这条路由**没有挂鉴权中间件**，但它不在匿名白名单里：\n"+
				"    %s\n"+
				"    要么补上 requireAuth / requireAdmin / requireAuthQuery，"+
				"要么在本文件的 allowedAnonymous 里说明它为什么可以匿名访问。",
				r.line, r.raw)
		}
	}
	// 少掉的：白名单里的路由被删了/改名了（也要提示，否则白名单会慢慢腐烂）
	for k := range want {
		if _, ok := got[k]; !ok {
			t.Errorf("白名单里的 %q 不再是匿名路由（被删除、改名，或已经补上了鉴权）。"+
				"若是有意为之，请同步更新 allowedAnonymous。", k)
		}
	}

	t.Logf("路由 %d 条：匿名 %d 条（白名单 %d 条）", len(routes), len(got), len(want))
}

// TestSensitiveRoutesAlwaysHaveMiddleware 给上面的白名单加一层"按前缀"的兜底。
//
// 白名单那条断言已经足够严格，但它的失败信息是"多了/少了一条"，而这一条能直接
// 指出"**这个前缀下的端点没有鉴权**" —— 对敏感前缀来说，那句话比"白名单不匹配"
// 更容易让人立刻意识到严重性。
func TestSensitiveRoutesAlwaysHaveMiddleware(t *testing.T) {
	// 这些前缀下的端点一旦匿名可达，就是明确的越权面。
	sensitive := []string{
		"/api/users", "/api/nodes", "/api/instances", "/api/pki", "/api/panel-backups",
		"/api/audit-logs", "/api/frps", "/api/tunnels", "/api/panel-tunnel", "/api/monitor",
		"/api/alerts", "/api/backup-policies", "/api/analysis", "/api/admin/", "/api/node-users",
		"/api/logshare", "/api/my/", "/api/tasks", "/api/help", "/api/announcements",
	}

	for _, r := range parseRoutes(t) {
		sens := false
		for _, p := range sensitive {
			if strings.HasPrefix(r.path, p) {
				sens = true
				break
			}
		}
		if !sens {
			continue
		}
		if !hasAuthMiddleware(r.handler) {
			t.Errorf("server.go:%d 敏感前缀下的端点没有鉴权中间件：\n    %s", r.line, r.raw)
		}
	}
}
