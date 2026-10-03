package httpapi

import (
	"net/http"
	"testing"
)

// 令牌撤销：**改密码之后，此前签发的令牌必须立刻失效**。
//
// 2026-10-01 安全审查的 H1：JWT 是自证明的，签发之后面板无从撤回，而有效期
// 是 24 小时。于是"管理员重置了被盗账号的密码"这个动作在最长 24 小时内完全
// 不生效 —— 攻击者拿着旧令牌照用不误。现在令牌里带 users.token_version，
// 改密码时把它 +1，旧令牌立刻对不上。
//
// 这条用例的价值在于它**只能靠查库**才能通过：签名、过期时间、issuer 全都没变，
// 只有数据库里的世代号变了。任何"把校验退回成纯 JWT 解析"的改动都会让它失败。
func TestTokenRevokedAfterPasswordChange(t *testing.T) {
	srv, ts := newTestServer(t)
	admin := loginAs(t, ts, "adm_rev1", "adm_rev1-pass-1234")
	old := mkUser(t, srv, ts, admin, "revoke1", "revoke1-pass-1234", RoleUser)

	if code, _ := doJSON(t, ts, "GET", "/api/auth/me", old, nil); code != http.StatusOK {
		t.Fatalf("改密码前旧令牌就不可用：%d", code)
	}

	code, body := doJSON(t, ts, "POST", "/api/auth/change-password", old,
		map[string]string{"old_password": "revoke1-pass-1234", "new_password": "revoke1-pass-5678"})
	if code != http.StatusOK {
		t.Fatalf("修改密码失败：%d %v", code, body)
	}

	if code, _ := doJSON(t, ts, "GET", "/api/auth/me", old, nil); code != http.StatusUnauthorized {
		t.Errorf("改密码后旧令牌仍然可用（HTTP %d）—— 令牌世代号没有生效，"+
			"这意味着被盗账号重置密码后攻击者最多还能用 24 小时", code)
	}

	// 新密码必须能正常登录，并且新令牌可用（别把功能一起锁死了）
	c, nb := doJSON(t, ts, "POST", "/api/auth/login", "",
		map[string]string{"username": "revoke1", "password": "revoke1-pass-5678"})
	if c != http.StatusOK {
		t.Fatalf("用新密码登录失败：%d %v", c, nb)
	}
	tok, _ := nb["token"].(string)
	if tok == "" {
		t.Fatal("登录没有返回令牌")
	}
	if code, _ := doJSON(t, ts, "GET", "/api/auth/me", tok, nil); code != http.StatusOK {
		t.Errorf("新令牌不可用：%d", code)
	}
}

// 被删除的账号，其令牌必须立刻失效。
//
// 与上一条同一类问题，只是触发方式不同：删号只删掉了 users 行，而旧实现
// 完全不看库，于是"已经不存在的人"还能继续操作面板。
func TestTokenRevokedAfterUserDeleted(t *testing.T) {
	srv, ts := newTestServer(t)
	admin := loginAs(t, ts, "adm_rev2", "adm_rev2-pass-1234")
	tok := mkUser(t, srv, ts, admin, "revoke2", "revoke2-pass-1234", RoleUser)

	if code, _ := doJSON(t, ts, "GET", "/api/auth/me", tok, nil); code != http.StatusOK {
		t.Fatalf("删号前令牌不可用：%d", code)
	}

	if code, body := doJSON(t, ts, "DELETE", "/api/users/"+itoa(uidOf(t, srv, "revoke2")), admin, nil); code != http.StatusOK {
		t.Fatalf("删除用户失败：%d %v", code, body)
	}

	if code, _ := doJSON(t, ts, "GET", "/api/auth/me", tok, nil); code != http.StatusUnauthorized {
		t.Errorf("用户已被删除，旧令牌仍然可用（HTTP %d）", code)
	}
}

// 降级必须立刻生效：令牌里写着 admin 不作数，角色以数据库为准。
//
// 这条比上面两条更"软"——它不会因为不查库就报错，而是继续放行。旧实现里
// 管理员把一个 admin 降级之后，对方的旧令牌在 24 小时内**仍然是管理员**。
func TestRoleDowngradeTakesEffectImmediately(t *testing.T) {
	srv, ts := newTestServer(t)
	admin := loginAs(t, ts, "adm_rev3", "adm_rev3-pass-1234")
	other := mkUser(t, srv, ts, admin, "revoke3", "revoke3-pass-1234", RoleAdmin)

	if code, _ := doJSON(t, ts, "GET", "/api/users", other, nil); code != http.StatusOK {
		t.Fatalf("降级前管理员接口不可用：%d", code)
	}

	if code, body := doJSON(t, ts, "PUT", "/api/users/"+itoa(uidOf(t, srv, "revoke3")), admin,
		map[string]string{"role": RoleUser}); code != http.StatusOK {
		t.Fatalf("降级失败：%d %v", code, body)
	}

	// 旧令牌里仍然写着 admin，但库里已经是 user：必须立刻失去管理员权限
	if code, _ := doJSON(t, ts, "GET", "/api/users", other, nil); code != http.StatusForbidden {
		t.Errorf("降级后旧令牌仍能访问管理员接口（HTTP %d）—— 角色必须以数据库为准", code)
	}
}
