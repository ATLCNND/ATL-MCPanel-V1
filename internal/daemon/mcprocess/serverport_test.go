package mcprocess

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeProps 写一份 server.properties，返回其路径。
func writeProps(t *testing.T, dir, content string) string {
	t.Helper()
	p := filepath.Join(dir, serverPropsFile)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatalf("写入 server.properties 失败: %v", err)
	}
	return p
}

func readProps(t *testing.T, dir string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, serverPropsFile))
	if err != nil {
		t.Fatalf("读取 server.properties 失败: %v", err)
	}
	return string(b)
}

// 端口的唯一落地处：服务端监听的端口必须被校准成实例的 port。
// 这条链断了就是"公网地址连不上、面板里隧道却显示 running"。
func TestSyncServerPortRewritesMismatch(t *testing.T) {
	dir := t.TempDir()
	writeProps(t, dir, "motd=我的服\nserver-port=25565\nmax-players=20\n")

	changed, old, err := syncServerPort(dir, 25567, false)
	if err != nil {
		t.Fatalf("校准失败: %v", err)
	}
	if !changed || old != 25565 {
		t.Fatalf("应改写且原值为 25565，实际 changed=%v old=%d", changed, old)
	}

	got := readProps(t, dir)
	if !strings.Contains(got, "server-port=25567") {
		t.Errorf("server-port 应为 25567\n实际:\n%s", got)
	}
	// 其余内容必须逐字节保留 —— 整体覆盖会丢掉用户调的 motd / 难度 / 白名单
	for _, want := range []string{"motd=我的服", "max-players=20"} {
		if !strings.Contains(got, want) {
			t.Errorf("不应改动其它配置项，丢了 %q\n实际:\n%s", want, got)
		}
	}
}

// 已经一致时不该动文件（避免每次启动都改 mtime，也让改动可追溯）。
func TestSyncServerPortNoopWhenSame(t *testing.T) {
	dir := t.TempDir()
	p := writeProps(t, dir, "server-port=25567\n")
	before, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}

	changed, old, err := syncServerPort(dir, 25567, false)
	if err != nil {
		t.Fatalf("校准失败: %v", err)
	}
	if changed {
		t.Error("值已一致时不应改写文件")
	}
	if old != 25567 {
		t.Errorf("原值应解析为 25567，实际 %d", old)
	}
	after, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Error("值已一致时不应触碰文件（mtime 变了）")
	}
}

// 缺 server-port 这一行时补上（服务端读缺项会用默认 25565，不是我们要的端口）。
func TestSyncServerPortAppendsMissingKey(t *testing.T) {
	dir := t.TempDir()
	writeProps(t, dir, "motd=hi\n")

	changed, old, err := syncServerPort(dir, 25570, false)
	if err != nil {
		t.Fatalf("校准失败: %v", err)
	}
	if !changed || old != 0 {
		t.Fatalf("应追加热且原值为 0，实际 changed=%v old=%d", changed, old)
	}

	got := readProps(t, dir)
	if !strings.Contains(got, "server-port=25570") {
		t.Errorf("应补上 server-port\n实际:\n%s", got)
	}
	if strings.Contains(got, "\n\n") {
		t.Errorf("不应产生多余空行\n实际:\n%q", got)
	}
}

// 文件还不存在：默认 java 启动的实例一定是 MC 服务端，先写一行把端口定下来
// （否则首次启动会监听 25565，与隧道错配）。
func TestSyncServerPortCreatesForDefaultStart(t *testing.T) {
	dir := t.TempDir()
	changed, _, err := syncServerPort(dir, 25567, false)
	if err != nil {
		t.Fatalf("校准失败: %v", err)
	}
	if !changed {
		t.Fatal("文件不存在时应创建")
	}
	if got := readProps(t, dir); !strings.Contains(got, "server-port=25567") {
		t.Errorf("新文件应含 server-port=25567，实际 %q", got)
	}
}

// 自定义启动命令 + 没有 server.properties → **不要**凭空塞一个：
// 那类实例可能根本不是 MC 服务端（脚本、代理、机器人）。
func TestSyncServerPortSkipsCreateForCustomStart(t *testing.T) {
	dir := t.TempDir()
	changed, _, err := syncServerPort(dir, 25567, true)
	if err != nil {
		t.Fatalf("校准失败: %v", err)
	}
	if changed {
		t.Error("自定义启动命令且文件不存在时不应创建 server.properties")
	}
	if _, err := os.Stat(filepath.Join(dir, serverPropsFile)); !os.IsNotExist(err) {
		t.Error("不应创建 server.properties")
	}
}

// 但自定义启动命令 + **已有** server.properties → 仍要校准：
// 那说明它确实是 MC 实例（用户只是自己写了启动脚本）。
func TestSyncServerPortSyncsExistingEvenForCustomStart(t *testing.T) {
	dir := t.TempDir()
	writeProps(t, dir, "server-port=25565\n")
	if changed, _, err := syncServerPort(dir, 25567, true); err != nil || !changed {
		t.Fatalf("已有文件时应校准，changed=%v err=%v", changed, err)
	}
	if got := readProps(t, dir); !strings.Contains(got, "server-port=25567") {
		t.Errorf("应改写为 25567，实际 %q", got)
	}
}

// 非法/未配置的端口不猜、不写（宁可保持原样，也不要把服务端改到奇怪端口）。
func TestSyncServerPortIgnoresInvalidPort(t *testing.T) {
	for _, port := range []int{0, -1, 70000} {
		dir := t.TempDir()
		writeProps(t, dir, "server-port=25565\n")
		changed, _, err := syncServerPort(dir, port, false)
		if err != nil {
			t.Fatalf("port=%d 不应报错: %v", port, err)
		}
		if changed {
			t.Errorf("port=%d 不该改写文件", port)
		}
	}
}

// 注释行里的 server-port 不算配置项（不能把注释当值改了）。
func TestSyncServerPortIgnoresCommentedLine(t *testing.T) {
	dir := t.TempDir()
	writeProps(t, dir, "#server-port=25565\nmotd=x\n")

	changed, old, err := syncServerPort(dir, 25567, false)
	if err != nil || !changed {
		t.Fatalf("应追加真实配置项，changed=%v err=%v", changed, err)
	}
	if old != 0 {
		t.Errorf("注释行不该被当成原值，实际 old=%d", old)
	}
	got := readProps(t, dir)
	if !strings.Contains(got, "#server-port=25565") {
		t.Errorf("注释行应原样保留\n实际:\n%s", got)
	}
	if !strings.Contains(got, "server-port=25567") {
		t.Errorf("应追加 server-port=25567\n实际:\n%s", got)
	}
}

// CRLF 文件不该被顺手改成 LF（只替换值，行尾照旧）。
func TestSyncServerPortPreservesCRLF(t *testing.T) {
	dir := t.TempDir()
	writeProps(t, dir, "motd=x\r\nserver-port=25565\r\n")

	if _, _, err := syncServerPort(dir, 25567, false); err != nil {
		t.Fatalf("校准失败: %v", err)
	}
	got := readProps(t, dir)
	if !strings.Contains(got, "server-port=25567\r\n") {
		t.Errorf("应保留 CRLF\n实际:\n%q", got)
	}
}
