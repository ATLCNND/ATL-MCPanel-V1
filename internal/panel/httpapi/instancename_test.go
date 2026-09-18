package httpapi

import (
	"net/http"
	"strings"
	"testing"
)

// nameOfInstance 直接查库确认实例名。
func nameOfInstance(t *testing.T, srv *Server, instanceID string) string {
	t.Helper()
	var name string
	if err := srv.db.QueryRow(`SELECT name FROM instances WHERE instance_id = ?`, instanceID).Scan(&name); err != nil {
		t.Fatalf("查实例名失败: %v", err)
	}
	return name
}

// 实例改名：改的是显示名，实例 ID 与端口一律不动。
//
// 这是本次要解决的问题（原来根本没有改名的接口）。ID 是节点上的目录名，
// 也是授权 / 端口 / 启动记录的钥匙 —— 改名如果动了它，等于给实例搬家换户籍。
func TestRenameInstanceKeepsIDAndPort(t *testing.T) {
	srv, ts := newTestServer(t)
	adminTok := loginAs(t, ts, "admin", "admin123")
	seedInstance(t, srv, "inst-a", 1)

	// 记录改名前后的端口，确保端口没被顺手改掉
	var portBefore int
	if err := srv.db.QueryRow(`SELECT port FROM instances WHERE instance_id = 'inst-a'`).Scan(&portBefore); err != nil {
		t.Fatalf("查端口失败: %v", err)
	}

	code, body := doJSON(t, ts, "PUT", "/api/instances/inst-a", adminTok,
		map[string]string{"name": "我的生存服 1.20.1"})
	if code != http.StatusOK {
		t.Fatalf("改名应 200，实际 %d body=%v", code, body)
	}
	if got := nameOfInstance(t, srv, "inst-a"); got != "我的生存服 1.20.1" {
		t.Fatalf("库里实例名应为新值，实际 %q", got)
	}

	// ID 不变（还能按原 ID 查到）
	var portAfter int
	if err := srv.db.QueryRow(`SELECT port FROM instances WHERE instance_id = 'inst-a'`).Scan(&portAfter); err != nil {
		t.Fatalf("改名后按原 ID 应仍能查到实例: %v", err)
	}
	if portAfter != portBefore {
		t.Errorf("改名不应改变端口：%d → %d", portBefore, portAfter)
	}

	// 审计里同时留下旧名与新名
	var cnt int
	if err := srv.db.QueryRow(
		`SELECT COUNT(*) FROM audit_logs
		 WHERE action = 'rename_instance' AND target = 'inst-a'
		   AND detail LIKE '%测试实例%' AND detail LIKE '%我的生存服%'`,
	).Scan(&cnt); err != nil {
		t.Fatalf("查审计失败: %v", err)
	}
	if cnt != 1 {
		t.Errorf("应有 1 条含新旧名的 rename_instance 审计，实际 %d", cnt)
	}
}

// 权限：实例 owner 能改，collab 不能改。
//
// collab 被排除是刻意的 —— 改名会改变**所有人**看到的名字，
// 与"开端口"一样属于对外呈现，不是"只被授权启停 + 看控制台"的人该做的。
func TestRenameInstancePermission(t *testing.T) {
	srv, ts := newTestServer(t)
	adminTok := loginAs(t, ts, "admin", "admin123")
	seedInstance(t, srv, "inst-b", 1)

	doJSON(t, ts, "POST", "/api/users", adminTok,
		map[string]string{"username": "collabber", "password": "pw123456", "role": "user"})
	doJSON(t, ts, "POST", "/api/users", adminTok,
		map[string]string{"username": "owner1", "password": "pw123456", "role": "user"})
	doJSON(t, ts, "POST", "/api/users", adminTok,
		map[string]string{"username": "stranger", "password": "pw123456", "role": "user"})
	cbTok := loginAs(t, ts, "collabber", "pw123456")
	ownTok := loginAs(t, ts, "owner1", "pw123456")
	stTok := loginAs(t, ts, "stranger", "pw123456")

	for _, c := range []struct {
		user, level string
		tok         string
		want        int
	}{
		{"collabber", "collab", cbTok, http.StatusForbidden},
		{"owner1", "owner", ownTok, http.StatusOK},
		{"stranger", "", stTok, http.StatusForbidden},
	} {
		doJSON(t, ts, "POST", "/api/instances/inst-b/assignments", adminTok,
			map[string]string{"username": c.user, "level": c.level})
		// 先恢复原名，保证每次都在改（否则会命中"名称未变化"分支）
		if _, err := srv.db.Exec(`UPDATE instances SET name = '测试实例' WHERE instance_id = 'inst-b'`); err != nil {
			t.Fatal(err)
		}
		code, body := doJSON(t, ts, "PUT", "/api/instances/inst-b", c.tok,
			map[string]string{"name": "被" + c.user + "改的名字"})
		if code != c.want {
			t.Errorf("%s(%s) 改名应 %d，实际 %d body=%v", c.user, c.level, c.want, code, body)
		}
	}
}

// 非法名称与"名称未变化"。
func TestRenameInstanceValidation(t *testing.T) {
	srv, ts := newTestServer(t)
	adminTok := loginAs(t, ts, "admin", "admin123")
	seedInstance(t, srv, "inst-c", 1)

	// 允许：带普通空格、中文、emoji（服务器名是人话，不该逼用户用下划线）
	for _, ok := range []string{"生存服 1.20.1", "服务器😀", strings.Repeat("名", 32)} {
		code, body := doJSON(t, ts, "PUT", "/api/instances/inst-c", adminTok,
			map[string]string{"name": ok})
		if code != http.StatusOK {
			t.Errorf("%q 应允许，实际 %d body=%v", ok, code, body)
		}
	}

	// 拒绝：空、制表符、换行、不换行空格、超长
	for _, bad := range []string{"", "   ", "a\tb", "a\nb", "a\u00a0b", strings.Repeat("名", 33)} {
		code, body := doJSON(t, ts, "PUT", "/api/instances/inst-c", adminTok,
			map[string]string{"name": bad})
		if code != http.StatusBadRequest {
			t.Errorf("%q 应被拒（400），实际 %d body=%v", bad, code, body)
		}
	}

	// 名称未变化：200 + 明确说明，不是错误
	cur := nameOfInstance(t, srv, "inst-c")
	code, body := doJSON(t, ts, "PUT", "/api/instances/inst-c", adminTok,
		map[string]string{"name": cur})
	if code != http.StatusOK {
		t.Errorf("改成原值应 200，实际 %d body=%v", code, body)
	}
	if msg, _ := body["message"].(string); !strings.Contains(msg, "未变化") {
		t.Errorf("未变化时应明确说明，实际: %s", msg)
	}

	// 不存在的实例
	if code, _ := doJSON(t, ts, "PUT", "/api/instances/nope", adminTok,
		map[string]string{"name": "x"}); code != http.StatusNotFound && code != http.StatusForbidden {
		t.Errorf("不存在的实例应 404/403，实际 %d", code)
	}
}

// checkInstanceName 的边界（纯函数）。
func TestCheckInstanceName(t *testing.T) {
	ok := []string{"a", "生存服 1.20.1", "my server", "服务器😀", strings.Repeat("x", 32)}
	for _, s := range ok {
		if msg := checkInstanceName(s); msg != "" {
			t.Errorf("%q 应通过，实际被拒：%s", s, msg)
		}
	}
	bad := []string{"", " ", "a\tb", "a\nb", "a\rb", "a\u00a0b", strings.Repeat("x", 33)}
	for _, s := range bad {
		if msg := checkInstanceName(s); msg == "" {
			t.Errorf("%q 应被拒", s)
		}
	}
}
