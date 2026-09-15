package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestIsPlaceholderSecret(t *testing.T) {
	placeholders := []string{
		"", "   ", "CHANGE_ME_TO_A_RANDOM_SECRET", "change_me",
		"your_secret", "secret", "jwt_secret", "placeholder-here",
	}
	for _, s := range placeholders {
		if !IsPlaceholderSecret(s) {
			t.Errorf("%q 应被识别为占位密钥", s)
		}
	}
	// 这些是「弱密钥」而非占位符，应走长度校验被拒绝
	notPlaceholders := []string{
		"9f8a7b6c5d4e3f2a1b0c9d8e7f6a5b4c",
		"short-secret",
		"MySecret",
	}
	for _, s := range notPlaceholders {
		if IsPlaceholderSecret(s) {
			t.Errorf("%q 不应被识别为占位密钥（应按长度校验处理）", s)
		}
	}
}

func TestResolveJWTSecretUsesConfigured(t *testing.T) {
	strong := strings.Repeat("a1b2c3d4", 4) // 32 字符
	cfg := &Config{Auth: AuthConfig{JWTSecret: strong}}

	got, generated, err := cfg.ResolveJWTSecret(filepath.Join(t.TempDir(), "jwt.secret"))
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if got != strong {
		t.Errorf("应使用配置中的密钥，实际 %q", got)
	}
	if generated {
		t.Error("使用既有配置时不应标记为新生成")
	}
}

func TestResolveJWTSecretRejectsWeak(t *testing.T) {
	cfg := &Config{Auth: AuthConfig{JWTSecret: "short-secret"}}
	if _, _, err := cfg.ResolveJWTSecret(filepath.Join(t.TempDir(), "jwt.secret")); err == nil {
		t.Error("过短的显式密钥应被拒绝")
	}
}

func TestResolveJWTSecretGeneratesAndPersists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "jwt.secret")
	cfg := &Config{} // 未配置密钥

	got, generated, err := cfg.ResolveJWTSecret(path)
	if err != nil {
		t.Fatalf("生成失败: %v", err)
	}
	if !generated {
		t.Error("应标记为新生成")
	}
	if len(got) < MinJWTSecretLength {
		t.Errorf("生成的密钥长度不足: %d", len(got))
	}

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatalf("密钥文件应已落盘: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("密钥文件权限应为 0600，实际 %o", fi.Mode().Perm())
	}

	// 二次调用应复用同一密钥（保证重启后旧会话仍有效）
	cfg2 := &Config{}
	again, generated2, err := cfg2.ResolveJWTSecret(path)
	if err != nil {
		t.Fatalf("二次读取失败: %v", err)
	}
	if generated2 {
		t.Error("二次调用不应重新生成")
	}
	if again != got {
		t.Error("二次调用应返回相同密钥")
	}
}

func TestResolveJWTSecretReplacesPlaceholder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "jwt.secret")
	cfg := &Config{Auth: AuthConfig{JWTSecret: "CHANGE_ME_TO_A_RANDOM_SECRET"}}

	got, generated, err := cfg.ResolveJWTSecret(path)
	if err != nil {
		t.Fatalf("不应报错: %v", err)
	}
	if !generated {
		t.Error("占位密钥应被替换为新生成的密钥")
	}
	if IsPlaceholderSecret(got) {
		t.Error("替换后的密钥不应仍是占位符")
	}
}
