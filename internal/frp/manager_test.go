package frp

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
)

func newTestState(t *testing.T, proto string, tls bool) *instanceFRP {
	t.Helper()
	dir := t.TempDir()
	tn := Tunnel{
		TunnelID:   "test1-tcp-25570",
		Name:       "测试线路",
		Protocol:   proto,
		LocalPort:  25565,
		RemotePort: 25570,
	}
	if proto == "http" || proto == "https" {
		tn.CustomDomains = []string{"panel.example.com"}
	}
	if tls {
		crt := filepath.Join(dir, "c.crt")
		key := filepath.Join(dir, "c.key")
		_ = os.WriteFile(crt, []byte("CERT"), 0o600)
		_ = os.WriteFile(key, []byte("KEY"), 0o600)
		tn.UseTLS = true
		tn.CertFile = crt
		tn.KeyFile = key
	}
	return &instanceFRP{
		server:  Server{Host: "1.2.3.4", BindPort: 7000, Token: "tok"},
		tunnels: map[string]Tunnel{tn.TunnelID: tn},
	}
}

func TestRenderConfigTCP(t *testing.T) {
	dir := t.TempDir()
	cfg, err := renderConfig(newTestState(t, "tcp", false), dir)
	if err != nil {
		t.Fatalf("生成配置失败: %v", err)
	}
	for _, want := range []string{
		`serverAddr = "1.2.3.4"`,
		`serverPort = 7000`,
		`auth.token = "tok"`,
		`[[proxies]]`,
		`name = "test1-tcp-25570"`,
		`type = "tcp"`,
		`localPort = 25565`,
		`remotePort = 25570`,
	} {
		if !strings.Contains(cfg, want) {
			t.Errorf("配置应包含 %q\n实际:\n%s", want, cfg)
		}
	}
	// 日志路径必须为绝对路径（相对路径会因工作目录被二次拼接）
	if !strings.Contains(cfg, filepath.Join(dir, "logs", "frpc.log")) {
		t.Errorf("日志路径应为绝对路径\n实际:\n%s", cfg)
	}
}

func TestRenderConfigHTTPVhost(t *testing.T) {
	cfg, err := renderConfig(newTestState(t, "http", false), t.TempDir())
	if err != nil {
		t.Fatalf("生成配置失败: %v", err)
	}
	if !strings.Contains(cfg, `customDomains = ["panel.example.com"]`) {
		t.Errorf("http 模式应包含 customDomains\n实际:\n%s", cfg)
	}
	// vhost 模式不应设置 remotePort
	if strings.Contains(cfg, "remotePort") {
		t.Errorf("vhost 模式不应包含 remotePort\n实际:\n%s", cfg)
	}
}

func TestRenderConfigHTTPSWithTLSPlugin(t *testing.T) {
	cfg, err := renderConfig(newTestState(t, "https", true), t.TempDir())
	if err != nil {
		t.Fatalf("生成配置失败: %v", err)
	}
	for _, want := range []string{
		`type = "https"`,
		`[proxies.plugin]`,
		`type = "https2http"`,
		`localAddr = "127.0.0.1:25565"`,
		`crt = `,
		`key = `,
	} {
		if !strings.Contains(cfg, want) {
			t.Errorf("https2http 配置应包含 %q\n实际:\n%s", want, cfg)
		}
	}
	// 使用插件时不能同时设置 localPort（frpc 会报错）
	if strings.Contains(cfg, "\nlocalPort = ") {
		t.Errorf("插件模式不应设置 localPort\n实际:\n%s", cfg)
	}
}

func TestRenderConfigRejectsIncomplete(t *testing.T) {
	// frps 信息缺失
	bad := &instanceFRP{server: Server{}, tunnels: map[string]Tunnel{"x": {TunnelID: "x", Protocol: "tcp", LocalPort: 1, RemotePort: 2}}}
	if _, err := renderConfig(bad, t.TempDir()); err == nil {
		t.Error("缺少 frps 信息应报错")
	}

	// vhost 模式缺少域名
	noDomain := &instanceFRP{
		server:  Server{Host: "1.2.3.4", BindPort: 7000},
		tunnels: map[string]Tunnel{"x": {TunnelID: "x", Protocol: "http", LocalPort: 1}},
	}
	if _, err := renderConfig(noDomain, t.TempDir()); err == nil {
		t.Error("http 模式缺少域名应报错")
	}

	// 证书文件不存在
	missingCert := &instanceFRP{
		server: Server{Host: "1.2.3.4", BindPort: 7000},
		tunnels: map[string]Tunnel{"x": {
			TunnelID: "x", Protocol: "https", LocalPort: 1,
			CustomDomains: []string{"a.com"}, UseTLS: true,
			CertFile: "/no/such.crt", KeyFile: "/no/such.key",
		}},
	}
	if _, err := renderConfig(missingCert, t.TempDir()); err == nil {
		t.Error("证书文件不存在应报错")
	}
}

func TestSanitizeProxyName(t *testing.T) {
	cases := map[string]string{
		"test1-tcp-25570": "test1-tcp-25570",
		"中文线路名":           "_____",
		"a b/c":           "a_b_c",
		"":                "proxy",
		"ok_name-1":       "ok_name-1",
	}
	for in, want := range cases {
		if got := sanitizeProxyName(in); got != want {
			t.Errorf("sanitizeProxyName(%q) = %q，期望 %q", in, got, want)
		}
	}
}

func TestAbsDir(t *testing.T) {
	if got := absDir("data/panel-frp"); !filepath.IsAbs(got) {
		t.Errorf("应返回绝对路径，实际 %q", got)
	}
	if got := absDir(""); got != "" {
		t.Errorf("空路径应原样返回，实际 %q", got)
	}
}

func TestApplyValidatesInput(t *testing.T) {
	m := NewManager("")
	dir := t.TempDir()

	if err := m.Apply("i1", dir, Server{Host: "1.2.3.4", BindPort: 7000}, Tunnel{TunnelID: ""}); err == nil {
		t.Error("缺少 tunnel_id 应报错")
	}
	if err := m.Apply("i1", dir, Server{Host: "1.2.3.4", BindPort: 7000}, Tunnel{TunnelID: "t", Protocol: "tcp", LocalPort: 0, RemotePort: 1}); err == nil {
		t.Error("local_port 为 0 应报错")
	}
	if err := m.Apply("i1", dir, Server{Host: "1.2.3.4", BindPort: 7000}, Tunnel{TunnelID: "t", Protocol: "tcp", LocalPort: 1, RemotePort: 0}); err == nil {
		t.Error("tcp 模式 remote_port 为 0 应报错")
	}
}

// ---- Daemon 退出时的 frpc 回收（StopAll） ----

// startFakeFrpc 起一个「假装是 frpc」的常驻进程，独立进程组与真实 frpc 一致。
//
// 不用真 frpc：测试机未必装了它，而这里要验证的是**进程回收**本身。
func startFakeFrpc(t *testing.T) *exec.Cmd {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("依赖 POSIX 进程组信号，仅在 Linux 上验证")
	}
	cmd := exec.Command("sleep", "60")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("启动替身进程失败: %v", err)
	}
	t.Cleanup(func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) })
	return cmd
}

// pidAlive 判断进程是否还在（信号 0 只做存在性检查）。
func pidAlive(pid int) bool { return syscall.Kill(pid, 0) == nil }

// StopAll 必须真的把 frpc 收走，并如实返回「本来在运行」的数量。
//
// 它是 Daemon 退出时唯一的兜底：unit 用 KillMode=process（不能改 —— 改成
// control-group 会让停 Daemon 连坐杀掉用户的 MC 实例），于是 Daemon 被
// SIGTERM 杀掉时没人回收 frpc，只能靠自己停，否则留下 PPID=1 的孤儿
// （2026-09-13 实测到过一个 `frpc -c …/instances/11/frpc.toml`）。
func TestStopAllStopsRunningFrpc(t *testing.T) {
	cmd := startFakeFrpc(t)
	pid := cmd.Process.Pid

	m := NewManager("")
	m.byInst["inst1"] = &instanceFRP{
		dir:     t.TempDir(),
		tunnels: map[string]Tunnel{"t1": {TunnelID: "t1", Protocol: "tcp", LocalPort: 25565, RemotePort: 25565}},
		cmd:     cmd,
	}

	if n := m.StopAll(); n != 1 {
		t.Fatalf("StopAll 应返回 1（1 个在运行），实际 %d", n)
	}
	if pidAlive(pid) {
		t.Fatal("StopAll 之后 frpc 仍在运行 —— 正是它要修的孤儿进程问题")
	}

	// 内存状态也要一并清干净，否则之后的状态查询会看到「还在跑」的假象
	st := m.byInst["inst1"]
	st.mu.Lock()
	stillSet := st.cmd != nil
	st.mu.Unlock()
	if stillSet {
		t.Error("StopAll 之后 st.cmd 未清空")
	}
}

// 没有任何实例时是安全空操作 —— Daemon 启动失败、无隧道的节点都会走到这条路径。
func TestStopAllEmpty(t *testing.T) {
	if n := NewManager("").StopAll(); n != 0 {
		t.Fatalf("无实例时 StopAll 应返回 0，实际 %d", n)
	}
}

// 已停止的实例不计入数量，也不影响在运行实例的回收。
func TestStopAllCountsOnlyRunning(t *testing.T) {
	running := startFakeFrpc(t)

	m := NewManager("")
	m.byInst["running"] = &instanceFRP{dir: t.TempDir(), cmd: running}
	m.byInst["stopped"] = &instanceFRP{
		dir:     t.TempDir(),
		tunnels: map[string]Tunnel{"t1": {TunnelID: "t1", Protocol: "tcp", LocalPort: 1, RemotePort: 2}},
	}

	if n := m.StopAll(); n != 1 {
		t.Fatalf("只有 1 个在运行，应返回 1，实际 %d", n)
	}
	if pidAlive(running.Process.Pid) {
		t.Error("在运行的实例未被停止")
	}
}

// LoadFromDisk 只恢复**定义**、不启动 frpc —— 要不要拉起由调用方按实例状态决定。
//
// 这条断言是"已停止的实例在 Daemon 启动时也被拉起 frpc"（2026-09-15 实测到）
// 那个问题的护栏：谁要是把 restart() 加回 LoadFromDisk，这里就会红。
func TestLoadFromDiskDoesNotStartFrpc(t *testing.T) {
	dir := t.TempDir()
	saved := struct {
		Server  Server            `json:"server"`
		Tunnels map[string]Tunnel `json:"tunnels"`
	}{
		Server:  Server{Host: "1.2.3.4", BindPort: 7000},
		Tunnels: map[string]Tunnel{"t1": {TunnelID: "t1", Protocol: "tcp", LocalPort: 25565, RemotePort: 25570}},
	}
	b, err := json.MarshalIndent(saved, "", "  ")
	if err != nil {
		t.Fatalf("构造测试数据失败: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tunnels.json"), b, 0o600); err != nil {
		t.Fatalf("写入 tunnels.json 失败: %v", err)
	}

	m := NewManager("")
	if err := m.LoadFromDisk("inst1", dir); err != nil {
		t.Fatalf("LoadFromDisk 失败: %v", err)
	}

	st, ok := m.byInst["inst1"]
	if !ok {
		t.Fatal("隧道定义应已恢复到内存（否则实例启动时 Resume 无从拉起）")
	}
	if len(st.tunnels) != 1 {
		t.Errorf("应恢复 1 条隧道定义，实际 %d", len(st.tunnels))
	}
	if m.isRunning(st) {
		t.Error("LoadFromDisk 不该启动 frpc：已停止的实例不该有 frpc")
	}
}

// ---- 隧道跟着实例状态（SetInstanceState） ----

// applyTestServer 一份合法的 frps 信息。
func applyTestServer() Server { return Server{Host: "1.2.3.4", BindPort: 7000, Token: "tok"} }

// applyTestTunnel 一条合法的隧道定义。
func applyTestTunnel() Tunnel {
	return Tunnel{TunnelID: "t1", Protocol: "tcp", LocalPort: 25565, RemotePort: 25570}
}

// 实例没在跑时：定义要登记（否则实例启动后 Resume 无从拉起），但**不能**启动 frpc。
//
// 修的是"给已停止的实例开通/重新下发端口，却把它的 frpc 拉起来"（2026-09-15 实测到）：
// 隧道后面什么都没有，却占着 frps 的 remote_port，界面还显示"实例 stopped、隧道 running"。
func TestApplyDefersStartWhenInstanceStopped(t *testing.T) {
	dir := t.TempDir()
	m := NewManager("")
	m.SetInstanceState(func(string) bool { return false }) // 实例已停止

	if err := m.Apply("inst1", dir, applyTestServer(), applyTestTunnel()); err != nil {
		t.Fatalf("Apply 不该失败（只是不启动）: %v", err)
	}

	st, ok := m.byInst["inst1"]
	if !ok {
		t.Fatal("定义必须登记到内存 —— 否则实例启动时 Resume 拉不起隧道")
	}
	if len(st.tunnels) != 1 {
		t.Errorf("应登记 1 条隧道，实际 %d", len(st.tunnels))
	}
	if m.isRunning(st) {
		t.Error("实例没在跑，不该启动 frpc")
	}
	// 定义要落盘（Daemon 重启后靠它恢复）
	if _, err := os.Stat(filepath.Join(dir, "tunnels.json")); err != nil {
		t.Errorf("隧道定义应已持久化: %v", err)
	}
	// 状态查询要如实反映"没有 frpc"
	for _, s := range m.List("inst1") {
		if s.Status != "stopped" {
			t.Errorf("实例停止时隧道状态应为 stopped，实际 %q", s.Status)
		}
	}
}

// 实例在跑时必须照旧真的去启动 frpc（这里用"frpc 不存在"反证它确实走到了启动那一步）。
func TestApplyStartsWhenInstanceRunning(t *testing.T) {
	dir := t.TempDir()
	m := NewManager("no-such-frpc-binary-xyz") // 故意用不存在的二进制
	m.SetInstanceState(func(string) bool { return true })

	err := m.Apply("inst2", dir, applyTestServer(), applyTestTunnel())
	if err == nil {
		t.Fatal("frpc 不存在时应报错 —— 说明它确实尝试启动了（而不是被状态判断挡掉）")
	}
	if !strings.Contains(err.Error(), "frpc") {
		t.Errorf("错误应说明 frpc 问题，实际: %v", err)
	}

	st := m.byInst["inst2"]
	if st == nil {
		t.Fatal("定义仍应登记（失败的是启动，不是登记）")
	}
	st.mu.Lock()
	lastErr := st.lastErr
	st.mu.Unlock()
	if lastErr == "" {
		t.Error("启动失败的原因应记进状态，供界面展示")
	}
}

// 「实例已停止、frpc 却还活着」的遗留状态要顺手纠正：
// restart() 先收进程，再判断实例状态 —— 顺序反了就会把遗留 frpc 继续留着占端口。
func TestRestartStopsLeftoverFrpcWhenInstanceStopped(t *testing.T) {
	cmd := startFakeFrpc(t)
	pid := cmd.Process.Pid
	dir := t.TempDir()

	st := &instanceFRP{
		dir:     dir,
		tunnels: map[string]Tunnel{"t1": {TunnelID: "t1", Protocol: "tcp", LocalPort: 25565, RemotePort: 25570}},
		cmd:     cmd,
	}
	m := NewManager("")
	m.byInst["inst3"] = st
	m.SetInstanceState(func(string) bool { return false }) // 实例已停止

	if err := m.restart("inst3", dir, st); err != nil {
		t.Fatalf("restart 不该报错: %v", err)
	}
	if pidAlive(pid) {
		t.Error("实例已停止时，遗留的 frpc 必须被收掉（它正占着公网端口）")
	}
	if m.isRunning(st) {
		t.Error("收掉之后不该再启动新的 frpc")
	}
}

// 未注入状态判断时保持原行为（面板自身那条穿透就靠它：它没有"实例进程"）。
func TestApplyWithoutStateFnAlwaysStarts(t *testing.T) {
	dir := t.TempDir()
	m := NewManager("no-such-frpc-binary-xyz")

	err := m.Apply("panel", dir, applyTestServer(), applyTestTunnel())
	if err == nil || !strings.Contains(err.Error(), "frpc") {
		t.Fatalf("未注入判断时应照旧尝试启动 frpc，实际 err=%v", err)
	}
}

