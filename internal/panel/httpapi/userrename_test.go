package httpapi

import (
	"net/http"
	"strings"
	"testing"
)

// nameOfUser 直接查库确认用户名（别只信接口返回值）。
func nameOfUser(t *testing.T, srv *Server, id int64) string {
	t.Helper()
	var name string
	if err := srv.db.QueryRow(`SELECT username FROM users WHERE id = ?`, id).Scan(&name); err != nil {
		t.Fatalf("查用户名失败: %v", err)
	}
	return name
}

// 登录响应必须带 id（UID）。
//
// 这是"用户名可改"的前提：前端要有一个不会变的标识来判断"这是不是我"。
// 少了它，改名之后用户会认不出自己的账号（自己那行不再被拦）。
func TestLoginReturnsUID(t *testing.T) {
	srv, ts := newTestServer(t)
	loginAs(t, ts, "admin", "admin123")
	uid := uidOf(t, srv, "admin")

	code, body := doJSON(t, ts, "POST", "/api/auth/login", "",
		map[string]string{"username": "admin", "password": "admin123"})
	if code != http.StatusOK {
		t.Fatalf("登录应 200，实际 %d", code)
	}
	got, ok := body["id"].(float64)
	if !ok || int64(got) != uid {
		t.Fatalf("登录响应应带 id=%d，实际 body=%v", uid, body)
	}
}

// 改自己的用户名：这是本次要解决的问题（原来根本没有改名的地方）。
func TestRenameSelf(t *testing.T) {
	srv, ts := newTestServer(t)
	adminTok := loginAs(t, ts, "admin", "admin123")
	doJSON(t, ts, "POST", "/api/users", adminTok,
		map[string]string{"username": "oldname", "password": "pw123456", "role": "user"})
	uid := uidOf(t, srv, "oldname")
	tok := loginAs(t, ts, "oldname", "pw123456")

	code, body := doJSON(t, ts, "PUT", "/api/users/"+itoa(uid)+"/username", tok,
		map[string]string{"username": "newname"})
	if code != http.StatusOK {
		t.Fatalf("本人改名应 200，实际 %d body=%v", code, body)
	}
	if got := nameOfUser(t, srv, uid); got != "newname" {
		t.Fatalf("库里用户名应为 newname，实际 %q", got)
	}

	// 旧令牌仍然可用：用户名不在鉴权链路上（只认 UID），所以不必强制重新登录。
	if code, _ := doJSON(t, ts, "GET", "/api/auth/me", tok, nil); code != http.StatusOK {
		t.Errorf("改名后旧令牌应仍然有效，实际 %d", code)
	}
	// /api/auth/me 从库里读用户名 → 刷新页面就能看到新名字，不是等到重新登录
	_, me := doJSON(t, ts, "GET", "/api/auth/me", tok, nil)
	if got, _ := me["username"].(string); got != "newname" {
		t.Errorf("/api/auth/me 应返回新用户名，实际 %q", got)
	}

	// 改动必须进审计，且 target 里同时留下旧名与新名（只留新名翻旧日志就对不上人），
	// detail 里带上 UID
	var cnt int
	if err := srv.db.QueryRow(
		`SELECT COUNT(*) FROM audit_logs
		 WHERE action = 'rename_user' AND target LIKE '%oldname%' AND target LIKE '%newname%'
		   AND detail = ?`, "user_id="+itoa(uid),
	).Scan(&cnt); err != nil {
		t.Fatalf("查审计失败: %v", err)
	}
	if cnt != 1 {
		t.Errorf("应有 1 条含新旧名与 UID 的 rename_user 审计，实际 %d", cnt)
	}
}

// 管理员可以改别人；普通用户改不了别人（但能改自己）。
func TestRenamePermission(t *testing.T) {
	srv, ts := newTestServer(t)
	adminTok := loginAs(t, ts, "admin", "admin123")
	doJSON(t, ts, "POST", "/api/users", adminTok,
		map[string]string{"username": "ua", "password": "pw123456", "role": "user"})
	doJSON(t, ts, "POST", "/api/users", adminTok,
		map[string]string{"username": "ub", "password": "pw123456", "role": "user"})
	uaID := uidOf(t, srv, "ua")
	ubID := uidOf(t, srv, "ub")
	uaTok := loginAs(t, ts, "ua", "pw123456")

	// 普通用户改别人 → 403
	if code, _ := doJSON(t, ts, "PUT", "/api/users/"+itoa(ubID)+"/username", uaTok,
		map[string]string{"username": "hacked"}); code != http.StatusForbidden {
		t.Errorf("普通用户改别人应 403，实际 %d", code)
	}
	if got := nameOfUser(t, srv, ubID); got != "ub" {
		t.Errorf("被拒的改名不应生效，实际 %q", got)
	}

	// 管理员改别人 → 200
	if code, body := doJSON(t, ts, "PUT", "/api/users/"+itoa(ubID)+"/username", adminTok,
		map[string]string{"username": "ub2"}); code != http.StatusOK {
		t.Errorf("管理员改别人应 200，实际 %d body=%v", code, body)
	}
	if got := nameOfUser(t, srv, ubID); got != "ub2" {
		t.Errorf("管理员改名应生效，实际 %q", got)
	}

	// UID 不变：改名不换身份
	if got := uidOf(t, srv, "ub2"); got != ubID {
		t.Errorf("改名后 UID 应不变（%d），实际 %d", ubID, got)
	}
	if got := uidOf(t, srv, "ua"); got != uaID {
		t.Errorf("无关用户不应受影响")
	}
}

// 重名与非法输入。
func TestRenameValidation(t *testing.T) {
	srv, ts := newTestServer(t)
	adminTok := loginAs(t, ts, "admin", "admin123")
	doJSON(t, ts, "POST", "/api/users", adminTok,
		map[string]string{"username": "taken", "password": "pw123456", "role": "user"})
	doJSON(t, ts, "POST", "/api/users", adminTok,
		map[string]string{"username": "me", "password": "pw123456", "role": "user"})
	meID := uidOf(t, srv, "me")
	meTok := loginAs(t, ts, "me", "pw123456")

	cases := []struct {
		name string
		body map[string]string
		want int
	}{
		{"重名", map[string]string{"username": "taken"}, http.StatusConflict},
		{"空名", map[string]string{"username": "   "}, http.StatusBadRequest},
		{"带空格", map[string]string{"username": "a b"}, http.StatusBadRequest},
		{"带换行", map[string]string{"username": "a\nb"}, http.StatusBadRequest},
		{"超长", map[string]string{"username": strings.Repeat("字", 33)}, http.StatusBadRequest},
	}
	for _, c := range cases {
		code, body := doJSON(t, ts, "PUT", "/api/users/"+itoa(meID)+"/username", meTok, c.body)
		if code != c.want {
			t.Errorf("%s：应 %d，实际 %d body=%v", c.name, c.want, code, body)
		}
		if got := nameOfUser(t, srv, meID); got != "me" {
			t.Fatalf("%s：被拒的改名不应生效，实际 %q", c.name, got)
		}
	}

	// 中文名允许（长度按字符数算，不是字节数）
	if code, body := doJSON(t, ts, "PUT", "/api/users/"+itoa(meID)+"/username", meTok,
		map[string]string{"username": "小明"}); code != http.StatusOK {
		t.Errorf("中文用户名应允许，实际 %d body=%v", code, body)
	}

	// 改成原值：不算错误，也不该报"已占用"
	code, body := doJSON(t, ts, "PUT", "/api/users/"+itoa(meID)+"/username", meTok,
		map[string]string{"username": "小明"})
	if code != http.StatusOK {
		t.Errorf("改成原值应 200，实际 %d body=%v", code, body)
	}

	// 不存在的用户
	if code, _ := doJSON(t, ts, "PUT", "/api/users/99999/username", adminTok,
		map[string]string{"username": "nobody"}); code != http.StatusNotFound {
		t.Errorf("不存在的用户应 404，实际 %d", code)
	}
}

// checkUsername 的边界（纯函数，直接测）。
func TestCheckUsername(t *testing.T) {
	ok := []string{"a", "小明", "user_1", "a-b", strings.Repeat("x", 32), "emoji😀"}
	for _, s := range ok {
		if msg := checkUsername(s); msg != "" {
			t.Errorf("%q 应通过，实际被拒：%s", s, msg)
		}
	}
	bad := []string{"", " ", "a b", "a\tb", "a\nb", "a\u00a0b", strings.Repeat("x", 33), "a\x00b"}
	for _, s := range bad {
		if msg := checkUsername(s); msg == "" {
			t.Errorf("%q 应被拒", s)
		}
	}
}
