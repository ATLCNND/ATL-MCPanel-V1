package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadAppliesDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	// 仅提供部分配置，其余应走默认值
	content := `
server:
  listen: ":9000"
auth:
  jwt_secret: "abc"
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}

	if cfg.Server.Listen != ":9000" {
		t.Errorf("listen 应为 :9000，实际 %s", cfg.Server.Listen)
	}
	if cfg.Server.GRPCListen != ":9090" {
		t.Errorf("grpc_listen 默认应为 :9090，实际 %s", cfg.Server.GRPCListen)
	}
	if cfg.Server.WebDir != "web/dist" {
		t.Errorf("web_dir 默认应为 web/dist，实际 %s", cfg.Server.WebDir)
	}
	if cfg.Server.PanelFrpDir != "data/panel-frp" {
		t.Errorf("panel_frp_dir 默认应为 data/panel-frp，实际 %s", cfg.Server.PanelFrpDir)
	}
	if cfg.DB.Driver != "sqlite3" || cfg.DB.DSN != "data/mcpanel.db" {
		t.Errorf("DB 默认值不正确: %+v", cfg.DB)
	}
	if cfg.Daemon.GRPCListen != ":9091" {
		t.Errorf("daemon grpc_listen 默认应为 :9091，实际 %s", cfg.Daemon.GRPCListen)
	}
	if cfg.Daemon.InstanceDir != "instances" {
		t.Errorf("instance_dir 默认应为 instances，实际 %s", cfg.Daemon.InstanceDir)
	}
	if cfg.Auth.JWTSecret != "abc" {
		t.Errorf("jwt_secret 应被读取，实际 %q", cfg.Auth.JWTSecret)
	}
}

func TestLoadMissingFile(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "nope.yaml")); err == nil {
		t.Error("文件不存在应返回错误")
	}
}

func TestLoadInvalidYAML(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.yaml")
	_ = os.WriteFile(path, []byte("server: [this is: not valid"), 0o644)
	if _, err := Load(path); err == nil {
		t.Error("非法 YAML 应返回错误")
	}
}

func TestDaemonDefaultsIdempotent(t *testing.T) {
	d := DaemonConfig{}
	d.Defaults()
	d.Defaults() // 重复调用不应改变已填充的值
	if d.GRPCListen != ":9091" || d.InstanceDir != "instances" {
		t.Errorf("默认值被覆盖: %+v", d)
	}
}
