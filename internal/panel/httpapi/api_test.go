package httpapi

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ATLCNND/ATL-MCPanel/internal/panel/auth"
	"github.com/ATLCNND/ATL-MCPanel/internal/panel/db"
	"github.com/ATLCNND/ATL-MCPanel/internal/panel/nodemgr"
)

// newTestServer 构建使用临时数据库的测试服务。
func newTestServer(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	d, err := db.Open("sqlite3", filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("初始化数据库失败: %v", err)
	}
	t.Cleanup(func() { d.Close() })

	srv := NewServer(d, auth.NewService("test-secret"), nodemgr.NewManager(d), Options{
		ListenAddr:  ":8080",
		PanelFrpDir: filepath.Join(t.TempDir(), "panel-frp"),
		// DataDir 指到临时目录：头像等附件会落在它下面（data/avatars）。
		// 不设的话会退化成相对路径 "data/avatars"，把测试数据写进工作目录。
		DataDir: t.TempDir(),
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return srv, ts
}

// call 发起请求并返回状态码与原始响应体。
func call(t *testing.T, ts *httptest.Server, method, path, token string, body any) (int, []byte) {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	req, err := http.NewRequest(method, ts.URL+path, &buf)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw
}

// doJSON 用于对象响应。
func doJSON(t *testing.T, ts *httptest.Server, method, path, token string, body any) (int, map[string]any) {
	t.Helper()
	code, raw := call(t, ts, method, path, token, body)
	out := map[string]any{}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &out)
	}
	return code, out
}

// doJSONArr 用于数组响应。
func doJSONArr(t *testing.T, ts *httptest.Server, method, path, token string, body any) (int, []map[string]any) {
	t.Helper()
	code, raw := call(t, ts, method, path, token, body)
	var out []map[string]any
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &out)
	}
	return code, out
}

// loginAs 注册（若不存在）并登录，返回 token。
func loginAs(t *testing.T, ts *httptest.Server, username, password string) string {
	t.Helper()
	doJSON(t, ts, "POST", "/api/users", "", map[string]string{"username": username, "password": password})

	code, body := doJSON(t, ts, "POST", "/api/auth/login", "", map[string]string{"username": username, "password": password})
	if code != http.StatusOK {
		t.Fatalf("登录失败 code=%d body=%v", code, body)
	}
	token, _ := body["token"].(string)
	if token == "" {
		t.Fatalf("登录未返回 token: %v", body)
	}
	return token
}

// seedInstance 直接写库插入实例（测试权限时不依赖 Daemon）。
// 因启用了外键约束（_foreign_keys=on），需先确保节点存在。
func seedInstance(t *testing.T, srv *Server, instanceID string, nodeID int64) {
	t.Helper()
	if _, err := srv.db.Exec(
		`INSERT OR IGNORE INTO nodes (id, name, ip, ssh_user, ssh_auth, ssh_port, status)
		 VALUES (?, ?, '127.0.0.1', '', '', 22, 'online')`,
		nodeID, "test-node"); err != nil {
		t.Fatalf("插入节点失败: %v", err)
	}
	if _, err := srv.db.Exec(
		`INSERT INTO instances (instance_id, node_id, name, core_type, port, max_mem, status)
		 VALUES (?, ?, ?, 'folia', 25565, '2G', 'stopped')`,
		instanceID, nodeID, "测试实例"); err != nil {
		t.Fatalf("插入实例失败: %v", err)
	}
}

func TestForeignKeyEnforced(t *testing.T) {
	srv, _ := newTestServer(t)
	// 引用不存在的节点应被外键约束拒绝
	_, err := srv.db.Exec(
		`INSERT INTO instances (instance_id, node_id, name) VALUES ('bad', 9999, 'x')`)
	if err == nil {
		t.Error("外键约束应拒绝引用不存在的节点")
	}
}

func TestHealthEndpoint(t *testing.T) {
	_, ts := newTestServer(t)
	code, body := doJSON(t, ts, "GET", "/api/health", "", nil)
	if code != 200 || body["status"] != "ok" {
		t.Errorf("健康检查异常: code=%d body=%v", code, body)
	}
}

func TestLoginFlow(t *testing.T) {
	_, ts := newTestServer(t)

	code, _ := doJSON(t, ts, "POST", "/api/auth/login", "", map[string]string{"username": "ghost", "password": "whatever"})
	if code != http.StatusUnauthorized {
		t.Errorf("不存在的用户应返回 401，实际 %d", code)
	}

	token := loginAs(t, ts, "admin", "admin123")

	code, _ = doJSON(t, ts, "POST", "/api/auth/login", "", map[string]string{"username": "admin", "password": "wrong"})
	if code != http.StatusUnauthorized {
		t.Errorf("错误密码应返回 401，实际 %d", code)
	}

	code, _ = doJSON(t, ts, "GET", "/api/instances", token, nil)
	if code != http.StatusOK {
		t.Errorf("实例列表应可访问，实际 %d", code)
	}
}

func TestAuthRequiredOnProtectedRoutes(t *testing.T) {
	_, ts := newTestServer(t)
	routes := []struct{ method, path string }{
		{"GET", "/api/instances"},
		{"GET", "/api/users"},
		{"GET", "/api/tunnels"},
		{"GET", "/api/frps"},
		{"GET", "/api/panel-tunnel"},
		{"GET", "/api/audit-logs"},
	}
	for _, r := range routes {
		if code, _ := doJSON(t, ts, r.method, r.path, "", nil); code != http.StatusUnauthorized {
			t.Errorf("%s %s 未携带 token 应 401，实际 %d", r.method, r.path, code)
		}
		// 伪造 token 同样应被拒绝
		if code, _ := doJSON(t, ts, r.method, r.path, "forged.token.value", nil); code != http.StatusUnauthorized {
			t.Errorf("%s %s 伪造 token 应 401，实际 %d", r.method, r.path, code)
		}
	}
}

func TestAdminOnlyRoutes(t *testing.T) {
	_, ts := newTestServer(t)
	adminTok := loginAs(t, ts, "admin", "admin123")

	if code, _ := doJSON(t, ts, "POST", "/api/users", adminTok, map[string]string{
		"username": "bob", "password": "bob12345", "role": "user",
	}); code != http.StatusCreated {
		t.Fatalf("管理员创建用户应成功，实际 %d", code)
	}
	bobTok := loginAs(t, ts, "bob", "bob12345")

	adminOnly := []struct {
		method, path string
		body         any
	}{
		{"GET", "/api/users", nil},
		{"POST", "/api/users", map[string]string{"username": "x", "password": "xxxxxx"}},
		{"GET", "/api/frps", nil},
		{"POST", "/api/frps", map[string]string{"name": "n", "host": "1.2.3.4"}},
		{"GET", "/api/tunnels", nil},
		{"GET", "/api/audit-logs", nil},
		{"GET", "/api/panel-tunnel", nil},
		{"POST", "/api/instances", map[string]any{"node_id": 1, "instance_id": "z"}},
	}
	for _, r := range adminOnly {
		if code, _ := doJSON(t, ts, r.method, r.path, bobTok, r.body); code != http.StatusForbidden {
			t.Errorf("普通用户 %s %s 应 403，实际 %d", r.method, r.path, code)
		}
	}
}

func TestInstanceVisibilityAndAccess(t *testing.T) {
	srv, ts := newTestServer(t)
	adminTok := loginAs(t, ts, "admin", "admin123")
	doJSON(t, ts, "POST", "/api/users", adminTok, map[string]string{"username": "carol", "password": "carol123", "role": "user"})
	carolTok := loginAs(t, ts, "carol", "carol123")

	seedInstance(t, srv, "inst1", 1)

	// 管理员可见全部
	code, list := doJSONArr(t, ts, "GET", "/api/instances", adminTok, nil)
	if code != http.StatusOK || len(list) != 1 {
		t.Fatalf("管理员应看到 1 个实例，code=%d list=%v", code, list)
	}
	if list[0]["level"] != "owner" {
		t.Errorf("管理员对实例应为 owner，实际 %v", list[0]["level"])
	}

	// 未授权的普通用户看不到任何实例
	code, list = doJSONArr(t, ts, "GET", "/api/instances", carolTok, nil)
	if code != http.StatusOK || len(list) != 0 {
		t.Errorf("未授权用户应看到空列表，code=%d list=%v", code, list)
	}

	// 未授权用户操作实例应 403（而非 404，避免信息泄露之外还误报）
	if code, _ := doJSON(t, ts, "POST", "/api/instances/inst1/start", carolTok, nil); code != http.StatusForbidden {
		t.Errorf("未授权用户启停实例应 403，实际 %d", code)
	}

	// 管理员授予 viewer 后可见，但 viewer 不能启停
	if code, _ := doJSON(t, ts, "POST", "/api/instances/inst1/assignments", adminTok,
		map[string]string{"username": "carol", "level": "viewer"}); code != http.StatusOK {
		t.Fatalf("授权应成功，实际 %d", code)
	}
	code, list = doJSONArr(t, ts, "GET", "/api/instances", carolTok, nil)
	if code != http.StatusOK || len(list) != 1 || list[0]["level"] != "viewer" {
		t.Fatalf("授权后应看到实例且级别为 viewer，code=%d list=%v", code, list)
	}
	if code, _ := doJSON(t, ts, "POST", "/api/instances/inst1/start", carolTok, nil); code != http.StatusForbidden {
		t.Errorf("viewer 不应能启停实例，实际 %d", code)
	}
}

func TestAssignmentGuards(t *testing.T) {
	srv, ts := newTestServer(t)
	adminTok := loginAs(t, ts, "admin", "admin123")
	seedInstance(t, srv, "inst1", 1)

	// 不存在的实例
	if code, _ := doJSON(t, ts, "GET", "/api/instances/nope/assignments", adminTok, nil); code != http.StatusNotFound {
		t.Errorf("不存在实例应 404，实际 %d", code)
	}
	// 给管理员授权应被拒绝
	if code, _ := doJSON(t, ts, "POST", "/api/instances/inst1/assignments", adminTok,
		map[string]string{"username": "admin", "level": "owner"}); code != http.StatusBadRequest {
		t.Errorf("给管理员授权应 400，实际 %d", code)
	}
	// 非法级别
	if code, _ := doJSON(t, ts, "POST", "/api/instances/inst1/assignments", adminTok,
		map[string]string{"username": "admin", "level": "root"}); code != http.StatusBadRequest {
		t.Errorf("非法级别应 400，实际 %d", code)
	}
}

func TestPasswordValidation(t *testing.T) {
	_, ts := newTestServer(t)
	adminTok := loginAs(t, ts, "admin", "admin123")

	if code, body := doJSON(t, ts, "POST", "/api/users", adminTok, map[string]string{"username": "short", "password": "123"}); code != http.StatusBadRequest {
		t.Errorf("过短密码应 400，实际 %d body=%v", code, body)
	}
	if code, _ := doJSON(t, ts, "POST", "/api/users", adminTok, map[string]string{"username": "", "password": "123456"}); code != http.StatusBadRequest {
		t.Errorf("空用户名应 400，实际 %d", code)
	}
	doJSON(t, ts, "POST", "/api/users", adminTok, map[string]string{"username": "dup", "password": "123456"})
	if code, _ := doJSON(t, ts, "POST", "/api/users", adminTok, map[string]string{"username": "dup", "password": "123456"}); code != http.StatusConflict {
		t.Errorf("重复用户名应 409，实际 %d", code)
	}
}

func TestLoginRateLimitIntegration(t *testing.T) {
	_, ts := newTestServer(t)
	loginAs(t, ts, "admin", "admin123")

	// 连续失败至阈值
	for i := 0; i < userLimiterCfg.maxFailures; i++ {
		code, _ := doJSON(t, ts, "POST", "/api/auth/login", "", map[string]string{
			"username": "admin", "password": "wrong",
		})
		if code != http.StatusUnauthorized {
			t.Fatalf("第 %d 次失败应 401，实际 %d", i+1, code)
		}
	}

	// 即使密码正确也应被限流
	code, body := doJSON(t, ts, "POST", "/api/auth/login", "", map[string]string{
		"username": "admin", "password": "admin123",
	})
	if code != http.StatusTooManyRequests {
		t.Errorf("触发限流后应 429，实际 %d body=%v", code, body)
	}
	if body["error"] == nil {
		t.Error("应返回错误说明")
	}

	// 账号维度限流不应波及其他账号（IP 维度阈值更宽松，此处未触发）
	doJSON(t, ts, "POST", "/api/users", "", map[string]string{"username": "other", "password": "other123"})
	// 注意：首个用户已存在，此处应 403（匿名无法创建）——仅用于确认请求未被限流中间件拦截
	code, _ = doJSON(t, ts, "POST", "/api/auth/login", "", map[string]string{
		"username": "other", "password": "other123",
	})
	if code == http.StatusTooManyRequests {
		t.Error("其他账号不应被该账号的锁定牵连")
	}
}

func TestLoginAuditRecorded(t *testing.T) {
	srv, ts := newTestServer(t)
	adminTok := loginAs(t, ts, "admin", "admin123")

	doJSON(t, ts, "POST", "/api/auth/login", "", map[string]string{"username": "admin", "password": "bad"})

	var actions []string
	rows, err := srv.db.Query(`SELECT action FROM audit_logs ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var a string
		_ = rows.Scan(&a)
		actions = append(actions, a)
	}
	joined := strings.Join(actions, ",")
	if !strings.Contains(joined, "login_failed") {
		t.Errorf("应记录登录失败审计，实际: %s", joined)
	}
	if !strings.Contains(joined, "login") {
		t.Errorf("应记录登录成功审计，实际: %s", joined)
	}
	_ = adminTok
}

func TestFirstUserBecomesAdmin(t *testing.T) {
	_, ts := newTestServer(t)
	code, body := doJSON(t, ts, "POST", "/api/users", "", map[string]string{"username": "first", "password": "123456"})
	if code != http.StatusCreated {
		t.Fatalf("首个用户应创建成功，实际 %d", code)
	}
	if role, _ := body["role"].(string); role != "admin" {
		t.Errorf("首个用户应自动成为管理员，实际 role=%v", body["role"])
	}
}

func TestUserDeletionGuards(t *testing.T) {
	srv, ts := newTestServer(t)
	adminTok := loginAs(t, ts, "admin", "admin123")
	doJSON(t, ts, "POST", "/api/users", adminTok, map[string]string{"username": "temp", "password": "temp1234"})

	var tmpID, adminID int64
	_ = srv.db.QueryRow(`SELECT id FROM users WHERE username='temp'`).Scan(&tmpID)
	_ = srv.db.QueryRow(`SELECT id FROM users WHERE username='admin'`).Scan(&adminID)

	// 不能删除自己
	if code, _ := doJSON(t, ts, "DELETE", "/api/users/"+itoa(adminID), adminTok, nil); code != http.StatusForbidden {
		t.Errorf("删除自己应 403，实际 %d", code)
	}
	// 可以删除他人
	if code, _ := doJSON(t, ts, "DELETE", "/api/users/"+itoa(tmpID), adminTok, nil); code != http.StatusOK {
		t.Errorf("删除普通用户应成功，实际 %d", code)
	}
	// 删除后无法登录
	if code, _ := doJSON(t, ts, "POST", "/api/auth/login", "", map[string]string{"username": "temp", "password": "temp1234"}); code != http.StatusUnauthorized {
		t.Errorf("已删除用户登录应 401，实际 %d", code)
	}
}

func TestSPAFallbackAndAPINotFound(t *testing.T) {
	_, ts := newTestServer(t)

	// 未配置前端目录时，API 未知路径应返回 404 JSON（不回退到 index.html）
	code, _ := call(t, ts, "GET", "/api/nonexistent", "", nil)
	if code != http.StatusNotFound {
		t.Errorf("未知 API 路径应 404，实际 %d", code)
	}
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}
