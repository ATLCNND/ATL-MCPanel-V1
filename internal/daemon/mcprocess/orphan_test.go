package mcprocess

import (
	"os"
	"path/filepath"
	"testing"
)

// TestIsJavaServerProcess 验证孤儿进程判定的精确性。
//
// 这里的误报代价很高：判定过宽会把无关进程当成实例进程，
// 从而错误地阻止用户启动实例。
func TestIsJavaServerProcess(t *testing.T) {
	cases := []struct {
		name    string
		cmdline string
		want    bool
	}{
		{
			name:    "默认 java -jar 启动",
			cmdline: "java\x00-Xms1G\x00-Xmx3G\x00-jar\x00/opt/mcpanel/jars/folia.jar\x00nogui\x00",
			want:    true,
		},
		{
			name:    "带绝对路径的 java",
			cmdline: "/usr/bin/java\x00-jar\x00/server.jar\x00nogui\x00",
			want:    true,
		},
		{
			name:    "start.sh 里 exec 的 java（相对 jar 路径）",
			cmdline: "java\x00-Xmx4G\x00-jar\x00server.jar\x00--nogui\x00",
			want:    true,
		},
		{
			name:    "argv[0] 被改成 java 的无关进程（不应误报）",
			cmdline: "java\x00300\x00",
			want:    false,
		},
		{
			name:    "同目录下的 sh 脚本（不应误报）",
			cmdline: "/bin/sh\x00/opt/mcpanel/instances/test1/start.sh\x00",
			want:    false,
		},
		{
			name:    "java 但没有 jar（如 java -version）",
			cmdline: "java\x00-version\x00",
			want:    false,
		},
		{
			name:    "空 cmdline（僵尸进程）",
			cmdline: "",
			want:    false,
		},
		{
			name:    "frpc（不应误报）",
			cmdline: "frpc\x00-c\x00/opt/mcpanel/instances/test1/frpc.toml\x00",
			want:    false,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := isJavaServerProcess([]byte(c.cmdline))
			if got != c.want {
				t.Errorf("isJavaServerProcess(%q) = %v，期望 %v", c.cmdline, got, c.want)
			}
		})
	}
}

func TestOrphanStartErrorMentionsPID(t *testing.T) {
	err := orphanStartError(1234)
	msg := err.Error()
	for _, want := range []string{"1234", "kill -9", "残留"} {
		if !contains(msg, want) {
			t.Errorf("错误信息应包含 %q，实际: %s", want, msg)
		}
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

// TestFindOrphanReturnsFalseForEmptyDir 空目录不应匹配到任何进程。
func TestFindOrphanReturnsFalseForEmptyDir(t *testing.T) {
	dir := t.TempDir()
	if pid, found := FindOrphan(dir); found {
		t.Errorf("空目录不应检测到孤儿进程，却返回了 PID %d", pid)
	}
}

// TestFindOrphanSelfExclusion 确保不会把测试进程自身当作孤儿。
func TestFindOrphanSelfExclusion(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Skip("无法获取工作目录")
	}
	// 当前测试进程的 cwd 就是包目录，且它不是 java 进程 → 不应命中
	if pid, found := FindOrphan(wd); found {
		t.Errorf("不应把非 java 进程判为孤儿，却返回 PID %d", pid)
	}
	_ = filepath.Base(wd)
}
