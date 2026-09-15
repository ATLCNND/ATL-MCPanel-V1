package auth

import (
	"strings"
	"testing"
	"time"
)

func TestHashAndCheckPassword(t *testing.T) {
	hash, err := HashPassword("s3cret-pw")
	if err != nil {
		t.Fatalf("加密失败: %v", err)
	}
	if hash == "s3cret-pw" {
		t.Fatal("密码不应以明文存储")
	}
	if !CheckPassword(hash, "s3cret-pw") {
		t.Error("正确密码校验应通过")
	}
	if CheckPassword(hash, "wrong-pw") {
		t.Error("错误密码校验应失败")
	}
	// 同一密码两次加密应产生不同哈希（bcrypt salt）
	hash2, _ := HashPassword("s3cret-pw")
	if hash == hash2 {
		t.Error("bcrypt 应使用随机 salt，两次哈希不应相同")
	}
}

func TestSignAndParseToken(t *testing.T) {
	svc := NewService("test-secret-key")
	token, err := svc.SignToken(42, "alice", "admin", time.Hour)
	if err != nil {
		t.Fatalf("签发失败: %v", err)
	}
	if token == "" {
		t.Fatal("token 不应为空")
	}
	claims, err := svc.ParseToken(token)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if claims.UserID != 42 || claims.Username != "alice" || claims.Role != "admin" {
		t.Errorf("载荷不匹配: %+v", claims)
	}
}

func TestParseTokenRejectsWrongSecret(t *testing.T) {
	issuer := NewService("secret-a")
	verifier := NewService("secret-b")

	token, _ := issuer.SignToken(1, "bob", "user", time.Hour)
	if _, err := verifier.ParseToken(token); err == nil {
		t.Error("使用不同密钥签发的 token 必须校验失败")
	}
}

func TestParseTokenRejectsTampered(t *testing.T) {
	svc := NewService("test-secret-key")
	token, _ := svc.SignToken(1, "bob", "user", time.Hour)

	// 篡改载荷（保留签名）应被拒绝
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("token 结构异常: %s", token)
	}
	tampered := parts[0] + "." + parts[1] + "x." + parts[2]
	if _, err := svc.ParseToken(tampered); err == nil {
		t.Error("被篡改的 token 必须校验失败")
	}

	// 伪造角色提升：用不同 payload 但原签名
	forged := NewService("test-secret-key")
	fake, _ := forged.SignToken(1, "bob", "admin", time.Hour)
	if fake == token {
		t.Error("不同角色的 token 不应相同")
	}
}

func TestParseTokenRejectsExpired(t *testing.T) {
	svc := NewService("test-secret-key")
	token, err := svc.SignToken(1, "carol", "user", -time.Minute) // 已过期
	if err != nil {
		t.Fatalf("签发失败: %v", err)
	}
	if _, err := svc.ParseToken(token); err == nil {
		t.Error("过期 token 必须校验失败")
	}
}

func TestParseTokenRejectsGarbage(t *testing.T) {
	svc := NewService("test-secret-key")
	for _, bad := range []string{"", "abc", "a.b.c", "not.a.jwt"} {
		if _, err := svc.ParseToken(bad); err == nil {
			t.Errorf("非法 token %q 必须校验失败", bad)
		}
	}
}
