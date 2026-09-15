package httpapi

import (
	"net/http"
	"strings"
	"testing"
)

// roleOfUser 直接查库确认角色（别只信接口返回值）。
func roleOfUser(t *testing.T, srv *Server, username string) string {
	t.Helper()
	var role string
	if err := srv.db.QueryRow(`SELECT role FROM users WHERE username = ?`, username).Scan(&role); err != nil {
		t.Fatalf("查角色失败: %v", err)
	}
	return role
}

// 升/降都要能改，且改完 DB 里真的变了 —— 这是本项要解决的问题：
// 以前角色只能在建号时定，改角色要删号重建（会丢实例授权）。
func TestSetUserRoleUpgradeAndDowngrade(t *testing.T) {
	srv, ts := newTestServer(t)
	adminTok := loginAs(t, ts, "admin", "admin123")
	doJSON(t, ts, "POST", "/api/users", adminTok,
		map[string]string{"username": "r1", "password": "r1234567", "role": "user"})
	uid := uidOf(t, srv, "r1")

	code, body := doJSON(t, ts, "PUT", "/api/users/"+itoa(uid), adminTok, map[string]string{"role": "nodeuser"})
	if code != http.StatusOK {
		t.Fatalf("升级应 200，实际 %d body=%v", code, body)
	}
	if got := roleOfUser(t, srv, "r1"); got != RoleNodeUser {
		t.Errorf("库里角色应为 nodeuser，实际 %s", got)
	}
	// 反馈里必须写明"重新登录后生效" —— 角色写在令牌里，改完不会立刻生效
	msg, _ := body["message"].(string)
	if !strings.Contains(msg, "重新登录") {
		t.Errorf("响应应提示重新登录后生效，实际: %s", msg)
	}

	code, body = doJSON(t, ts, "PUT", "/api/users/"+itoa(uid), adminTok, map[string]string{"role": "user"})
	if code != http.StatusOK {
		t.Fatalf("降级应 200，实际 %d body=%v", code, body)
	}
	if got := roleOfUser(t, srv, "r1"); got != RoleUser {
		t.Errorf("库里角色应为 user，实际 %s", got)
	}
}

// 降级**保留**节点授权与端口配额：权限判定的第一关（角色）过不去就够了，
// 将来再升回来配置还在。顺手清掉属于"降级即销毁配置"，更难挽回。
func TestSetUserRoleKeepsNodeConfig(t *testing.T) {
	srv, ts := newTestServer(t)
	adminTok := loginAs(t, ts, "admin", "admin123")
	doJSON(t, ts, "POST", "/api/users", adminTok,
		map[string]string{"username": "r2", "password": "r1234567", "role": "nodeuser"})
	uid := uidOf(t, srv, "r2")

	// 造一条节点授权 + 一条端口配额
	if _, err := srv.db.Exec(`INSERT INTO nodes (id, name, ip, ssh_user, ssh_auth, ssh_port) VALUES (7, 'n7', '10.0.0.7', 'root', 'pw', 22)`); err != nil {
		t.Fatalf("建节点失败: %v", err)
	}
	if _, err := srv.db.Exec(`INSERT INTO node_users (user_id, node_id, granted_by) VALUES (?, 7, 1)`, uid); err != nil {
		t.Fatalf("建节点授权失败: %v", err)
	}
	if _, err := srv.db.Exec(`INSERT INTO frps_servers (id, name, host, bind_port, token, port_start, port_end) VALUES (3, 'l3', '1.2.3.4', 7000, 'tok', 25600, 25700)`); err != nil {
		t.Fatalf("建线路失败: %v", err)
	}
	if _, err := srv.db.Exec(`INSERT INTO node_user_ports (user_id, frps_id, quota, granted_by) VALUES (?, 3, 2, 1)`, uid); err != nil {
		t.Fatalf("建端口配额失败: %v", err)
	}

	if code, body := doJSON(t, ts, "PUT", "/api/users/"+itoa(uid), adminTok, map[string]string{"role": "user"}); code != http.StatusOK {
		t.Fatalf("降级应 200，实际 %d body=%v", code, body)
	}

	var nodes, ports int
	_ = srv.db.QueryRow(`SELECT COUNT(*) FROM node_users WHERE user_id = ?`, uid).Scan(&nodes)
	_ = srv.db.QueryRow(`SELECT COUNT(*) FROM node_user_ports WHERE user_id = ?`, uid).Scan(&ports)
	if nodes != 1 {
		t.Errorf("降级后节点授权应保留，实际 %d 行", nodes)
	}
	if ports != 1 {
		t.Errorf("降级后端口配额应保留，实际 %d 行", ports)
	}
}

// 不能改自己的角色：当场把自己降级会立刻失去管理权限（而且旧令牌还能用 24 小时，
// 表现会非常费解）。升/降都拒。
func TestSetUserRoleRejectsSelf(t *testing.T) {
	srv, ts := newTestServer(t)
	adminTok := loginAs(t, ts, "admin", "admin123")
	uid := uidOf(t, srv, "admin")

	code, body := doJSON(t, ts, "PUT", "/api/users/"+itoa(uid), adminTok, map[string]string{"role": "user"})
	if code != http.StatusForbidden {
		t.Fatalf("改自己的角色应 403，实际 %d body=%v", code, body)
	}
	if got := roleOfUser(t, srv, "admin"); got != RoleAdmin {
		t.Errorf("自己的角色不该被改动，实际 %s", got)
	}
}

// 非法角色要挡在建表之前（别把 users.role 写进奇怪的值，鉴权全靠它）。
func TestSetUserRoleRejectsInvalidRole(t *testing.T) {
	srv, ts := newTestServer(t)
	adminTok := loginAs(t, ts, "admin", "admin123")
	doJSON(t, ts, "POST", "/api/users", adminTok,
		map[string]string{"username": "r3", "password": "r1234567", "role": "user"})
	uid := uidOf(t, srv, "r3")

	for _, bad := range []string{"root", "superadmin", "", "ADMIN"} {
		code, _ := doJSON(t, ts, "PUT", "/api/users/"+itoa(uid), adminTok, map[string]string{"role": bad})
		if code != http.StatusBadRequest {
			t.Errorf("角色 %q 应 400，实际 %d", bad, code)
		}
	}
	if got := roleOfUser(t, srv, "r3"); got != RoleUser {
		t.Errorf("非法请求不该改动角色，实际 %s", got)
	}
}

// 只有总管理员能改角色。
func TestSetUserRoleForbiddenForNonAdmin(t *testing.T) {
	srv, ts := newTestServer(t)
	adminTok := loginAs(t, ts, "admin", "admin123")
	doJSON(t, ts, "POST", "/api/users", adminTok,
		map[string]string{"username": "r4", "password": "r1234567", "role": "user"})
	doJSON(t, ts, "POST", "/api/users", adminTok,
		map[string]string{"username": "r5", "password": "r1234567", "role": "user"})
	uid := uidOf(t, srv, "r5")

	userTok := loginAs(t, ts, "r4", "r1234567")
	code, _ := doJSON(t, ts, "PUT", "/api/users/"+itoa(uid), userTok, map[string]string{"role": "admin"})
	if code != http.StatusForbidden {
		t.Fatalf("普通用户改角色应 403，实际 %d", code)
	}
	if got := roleOfUser(t, srv, "r5"); got != RoleUser {
		t.Errorf("普通用户不该改得动角色，实际 %s", got)
	}
}

// 目标用户不存在 → 404（别静默成功）。
func TestSetUserRoleUnknownUser(t *testing.T) {
	_, ts := newTestServer(t)
	adminTok := loginAs(t, ts, "admin", "admin123")
	code, _ := doJSON(t, ts, "PUT", "/api/users/99999", adminTok, map[string]string{"role": "user"})
	if code != http.StatusNotFound {
		t.Fatalf("不存在的用户应 404，实际 %d", code)
	}
}

// 角色没变时给一句人话，而不是假装改动成功。
func TestSetUserRoleSameRole(t *testing.T) {
	srv, ts := newTestServer(t)
	adminTok := loginAs(t, ts, "admin", "admin123")
	doJSON(t, ts, "POST", "/api/users", adminTok,
		map[string]string{"username": "r6", "password": "r1234567", "role": "user"})
	uid := uidOf(t, srv, "r6")

	code, body := doJSON(t, ts, "PUT", "/api/users/"+itoa(uid), adminTok, map[string]string{"role": "user"})
	if code != http.StatusOK {
		t.Fatalf("应 200，实际 %d body=%v", code, body)
	}
	if msg, _ := body["message"].(string); !strings.Contains(msg, "未变化") {
		t.Errorf("应提示角色未变化，实际: %s", msg)
	}
}
