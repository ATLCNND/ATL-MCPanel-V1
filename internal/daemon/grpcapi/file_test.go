package grpcapi

import (
	"os"
	"path/filepath"
	"testing"
)

// TestProtectedPaths 校验 frp 相关内部文件被识别为受保护。
func TestProtectedPaths(t *testing.T) {
	protected := []string{
		"frpc.toml",
		"/frpc.toml",
		"./frpc.toml",
		"tunnels.json",
		"frpc.pid",
		"instance.json", // 实例元数据：面板侧才是权威，Daemon 这份只是建实例时的快照
		"logs/frpc.log",
		"/logs/frpc.log",
	}
	for _, p := range protected {
		if !isProtectedPath(p) {
			t.Errorf("%q 应被识别为受保护文件", p)
		}
	}

	notProtected := []string{
		"server.properties",
		"logs/latest.log",
		"logs/console.log",
		"plugins/Essentials.jar",
		"world/level.dat",
		"frpc.toml.bak",        // 不是精确匹配
		"backups/tunnels.json", // 仅根目录下的 tunnels.json 受保护
		"",
	}
	for _, p := range notProtected {
		if isProtectedPath(p) {
			t.Errorf("%q 不应被识别为受保护文件", p)
		}
	}
}

// TestIsProtectedName 校验目录列表过滤逻辑。
func TestIsProtectedName(t *testing.T) {
	// 实例根目录下的 frp 文件应被隐藏
	if !isProtectedName("/", "frpc.toml") {
		t.Error("根目录的 frpc.toml 应被隐藏")
	}
	if !isProtectedName("", "tunnels.json") {
		t.Error("根目录的 tunnels.json 应被隐藏")
	}
	// logs 目录下的 frpc.log 应被隐藏
	if !isProtectedName("logs", "frpc.log") {
		t.Error("logs/frpc.log 应被隐藏")
	}
	// 普通文件不应受影响
	for _, tc := range []struct{ dir, name string }{
		{"/", "server.properties"},
		{"/", "logs"},
		{"/", "world"},
		{"logs", "latest.log"},
		{"logs", "console.log"},
		{"plugins", "essentials.jar"},
	} {
		if isProtectedName(tc.dir, tc.name) {
			t.Errorf("%s/%s 不应被隐藏", tc.dir, tc.name)
		}
	}
}

// TestProtectedFilesNotListed 端到端校验：列出目录时不返回受保护文件。
func TestProtectedFilesNotListed(t *testing.T) {
	dir := t.TempDir()
	files := []string{
		"server.properties", "frpc.toml", "tunnels.json", "frpc.pid",
		"eula.txt", "world",
	}
	for _, f := range files {
		if f == "world" {
			if err := os.MkdirAll(filepath.Join(dir, f), 0o755); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.WriteFile(filepath.Join(dir, f), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(dir, "logs"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"latest.log", "frpc.log"} {
		if err := os.WriteFile(filepath.Join(dir, "logs", f), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// 模拟 ListFiles 的过滤逻辑
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	visible := map[string]bool{}
	for _, e := range entries {
		if isProtectedName("/", e.Name()) {
			continue
		}
		visible[e.Name()] = true
	}

	for _, want := range []string{"server.properties", "eula.txt", "world", "logs"} {
		if !visible[want] {
			t.Errorf("%s 应可见", want)
		}
	}
	for _, hidden := range []string{"frpc.toml", "tunnels.json", "frpc.pid"} {
		if visible[hidden] {
			t.Errorf("%s 应被隐藏（含 frp 凭据）", hidden)
		}
	}

	logEntries, _ := os.ReadDir(filepath.Join(dir, "logs"))
	logVisible := map[string]bool{}
	for _, e := range logEntries {
		if isProtectedName("logs", e.Name()) {
			continue
		}
		logVisible[e.Name()] = true
	}
	if !logVisible["latest.log"] {
		t.Error("logs/latest.log 应可见")
	}
	if logVisible["frpc.log"] {
		t.Error("logs/frpc.log 应被隐藏")
	}
}
