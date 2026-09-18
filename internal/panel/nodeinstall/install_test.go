package nodeinstall

import (
	"os"
	"strings"
	"testing"
)

func TestRenderDaemonConfigWithTLS(t *testing.T) {
	cfg := renderDaemonConfig(Options{
		NodeID:       "node-001",
		PanelAddress: "10.0.0.1:9090",
		GRPCListen:   ":9092",
	}, "/opt/mcpanel", true)

	for _, want := range []string{
		`node_id: "node-001"`,
		`panel_address: "10.0.0.1:9090"`,
		`grpc_listen: ":9092"`,
		`instance_dir: "/opt/mcpanel/instances"`,
		"tls: true",
		`cert_file: "/opt/mcpanel/certs/node.crt"`,
		`key_file: "/opt/mcpanel/certs/node.key"`,
		`ca_file: "/opt/mcpanel/certs/ca.crt"`,
	} {
		if !strings.Contains(cfg, want) {
			t.Errorf("配置应包含 %q\n实际:\n%s", want, cfg)
		}
	}
}

func TestRenderDaemonConfigWithoutTLS(t *testing.T) {
	cfg := renderDaemonConfig(Options{NodeID: "n", PanelAddress: "p:9090"}, "/opt/x", false)
	if !strings.Contains(cfg, "tls: false") {
		t.Errorf("未启用 mTLS 时应为 tls: false\n实际:\n%s", cfg)
	}
	// 默认 grpc 端口
	if !strings.Contains(cfg, `grpc_listen: ":9091"`) {
		t.Errorf("未指定端口时应使用默认 :9091\n实际:\n%s", cfg)
	}
}

func TestSystemdUnitHasKillModeProcess(t *testing.T) {
	unit := systemdUnit("/opt/mcpanel", "atlmcpanel-daemon")

	// KillMode=process 是硬性要求：默认的 control-group 会在服务重启时
	// 连带杀死 Minecraft 子进程，导致实例意外中断
	if !strings.Contains(unit, "KillMode=process") {
		t.Error("systemd 单元必须设置 KillMode=process")
	}
	for _, want := range []string{
		"ExecStart=/opt/mcpanel/bin/dsh-daemon -config /opt/mcpanel/config.yaml",
		"WorkingDirectory=/opt/mcpanel",
		"Restart=on-failure",
		"WantedBy=multi-user.target",
		"atlmcpanel-daemon",
	} {
		if !strings.Contains(unit, want) {
			t.Errorf("单元文件应包含 %q\n实际:\n%s", want, unit)
		}
	}
}

func TestShellQuote(t *testing.T) {
	cases := map[string]string{
		"/opt/mcpanel":     "'/opt/mcpanel'",
		"/path with space": "'/path with space'",
		"/it's":            `'/it'\''s'`,
		"/a;rm -rf /":      "'/a;rm -rf /'",
	}
	for in, want := range cases {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %q，期望 %q", in, got, want)
		}
	}
}

func TestDialValidation(t *testing.T) {
	if _, err := Dial(Options{Host: ""}); err == nil {
		t.Error("空主机名应报错")
	}
	// 无法连接时应返回错误而非 panic
	if _, err := Dial(Options{Host: "127.0.0.1", Port: 1, User: "root", Auth: "x"}); err == nil {
		t.Error("连接不存在端口应报错")
	}
}

func TestDeployRequiresBinary(t *testing.T) {
	// 未提供二进制时应立即报错（不尝试连接）
	c := &Client{}
	if _, err := c.Deploy(Options{RemoteDir: "/tmp/x"}); err == nil {
		t.Error("缺少二进制应报错")
	} else if !strings.Contains(err.Error(), "二进制") {
		t.Errorf("错误信息应说明缺少二进制，实际: %v", err)
	}
}

func TestFileModeConstants(t *testing.T) {
	// 私钥应以 0600 上传（保护证书私钥）
	if os.FileMode(0o600).Perm() != 0o600 {
		t.Error("权限常量异常")
	}
}
