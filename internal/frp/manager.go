// Package frp 管理实例侧的 frpc 进程：生成配置、启停进程、上报状态。
//
// 设计：每个实例对应一个 frpc 进程，其所有隧道作为该进程的多个 [[proxies]] 条目，
// 减少进程数量并简化状态管理。配置与隧道定义持久化到实例目录。
package frp

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Tunnel 单条隧道定义。
type Tunnel struct {
	TunnelID   string `json:"tunnel_id"`
	Name       string `json:"name"`
	Protocol   string `json:"protocol"` // tcp / udp / http / https
	LocalPort  int    `json:"local_port"`
	RemotePort int    `json:"remote_port"`

	// vhost 模式（http / https）使用
	CustomDomains []string `json:"custom_domains,omitempty"`
	Subdomain     string   `json:"subdomain,omitempty"`

	// https 模式：由 frpc 侧终止 TLS（https2http 插件）转发到本地 HTTP 服务
	UseTLS   bool   `json:"use_tls,omitempty"`
	CertFile string `json:"cert_file,omitempty"`
	KeyFile  string `json:"key_file,omitempty"`
}

// Server frps 服务端连接信息。
type Server struct {
	Host     string `json:"host"`
	BindPort int    `json:"bind_port"`
	Token    string `json:"token"`
}

// TunnelStatus 隧道运行状态。
type TunnelStatus struct {
	TunnelID   string
	Protocol   string
	LocalPort  int
	RemotePort int
	Status     string // running / stopped / error
	Error      string
}

// instanceFRP 单个实例的 frpc 状态。
type instanceFRP struct {
	server  Server
	tunnels map[string]Tunnel
	dir     string // 实例目录（用于重新拉起 frpc 时定位配置与日志）

	mu      sync.Mutex
	cmd     *exec.Cmd
	logF    *os.File
	lastErr string
}

// Manager frpc 管理器。
type Manager struct {
	// admin frps 管理 API 客户端（可为 nil = 未启用）。
	// 启用后可为每个实例提供**真实隧道流量**，而非节点整机聚合值。
	admin   *AdminClient
	sampler *trafficSampler
	mu      sync.RWMutex
	byInst  map[string]*instanceFRP // instance_id -> 状态

	frpcPath string // frpc 可执行文件路径（默认 PATH 中的 frpc）

	// stateFn 判断实例进程是否在运行（由 Daemon 注入，见 SetInstanceState）。
	// 为 nil 时不做判断（面板自身那条穿透就用这种模式 —— 它没有"实例进程"）。
	stateFn func(instanceID string) bool
}

// SetInstanceState 注入"实例进程是否在运行"的判断（Daemon 启动时调用一次）。
//
// 为什么需要它：frpc 存在的唯一意义是把**实例**的服务暴露出去 ——
// 实例没在跑时，隧道后面什么都没有，却会：
//   - 占住 frps 上的 remote_port
//   - 让界面出现"实例已停止、隧道却在运行"这种自相矛盾的组合
//
// 判断放在 `restart()` 这一个入口上，于是 Apply / Remove / Resume 全都遵守
// 同一条不变式：**frpc 只在实例运行时存在**。定义与配置照旧登记（`Resume`
// 之后要靠内存里的定义把隧道拉起来），只是**不启动进程**。
//
// 反过来说：注入 nil 就等于关掉这个判断（面板自身的穿透不能用它 ——
// 那条隧道后面没有"实例进程"这个概念）。
func (m *Manager) SetInstanceState(fn func(instanceID string) bool) {
	m.mu.Lock()
	m.stateFn = fn
	m.mu.Unlock()
}

// instanceRunning 询问实例是否在运行（未注入判断时一律当作"在运行"）。
func (m *Manager) instanceRunning(instanceID string) bool {
	m.mu.RLock()
	fn := m.stateFn
	m.mu.RUnlock()
	if fn == nil {
		return true
	}
	return fn(instanceID)
}

// NewManager 创建管理器。
func NewManager(frpcPath string) *Manager {
	if frpcPath == "" {
		frpcPath = "frpc"
	}
	return &Manager{
		byInst:   make(map[string]*instanceFRP),
		frpcPath: frpcPath,
		sampler:  &trafficSampler{},
	}
}

// Available 检测 frpc 是否可用。
func (m *Manager) Available() bool {
	_, err := exec.LookPath(m.frpcPath)
	return err == nil
}

// Apply 应用一条隧道（新增或更新），并在需要时重启 frpc。
//
// 「需要时」包含两层意思：隧道定义有变化，**且实例正在运行**。
// 实例没在跑时只登记定义与配置，不启动进程（见 SetInstanceState）——
// 实例下一次启动时 `Resume` 会按内存里的定义把隧道拉起来。
func (m *Manager) Apply(instanceID, instanceDir string, srv Server, t Tunnel) error {
	if t.TunnelID == "" {
		return fmt.Errorf("tunnel_id 不能为空")
	}
	if t.LocalPort <= 0 {
		return fmt.Errorf("local_port 必须为正整数")
	}
	if (t.Protocol == "" || t.Protocol == "tcp" || t.Protocol == "udp") && t.RemotePort <= 0 {
		return fmt.Errorf("remote_port 必须为正整数")
	}
	if t.Protocol == "" {
		t.Protocol = "tcp"
	}
	instanceDir = absDir(instanceDir)

	m.mu.Lock()
	st, ok := m.byInst[instanceID]
	if !ok {
		st = &instanceFRP{
			server:  srv,
			tunnels: make(map[string]Tunnel),
			dir:     instanceDir,
		}
		m.byInst[instanceID] = st
	}
	// 服务端信息以最新下发为准
	st.server = srv
	st.dir = instanceDir
	st.tunnels[t.TunnelID] = t
	m.mu.Unlock()

	if err := m.persist(instanceDir, st); err != nil {
		return err
	}
	return m.restart(instanceID, instanceDir, st)
}

// Remove 移除一条隧道。
func (m *Manager) Remove(instanceID, instanceDir, tunnelID string) error {
	instanceDir = absDir(instanceDir)
	m.mu.Lock()
	st, ok := m.byInst[instanceID]
	if !ok {
		m.mu.Unlock()
		return nil // 无该实例的 frpc，视为已移除
	}
	delete(st.tunnels, tunnelID)
	empty := len(st.tunnels) == 0
	m.mu.Unlock()

	if empty {
		m.stopProcess(st)
		CleanInstanceFiles(instanceDir)
	} else {
		if err := m.persist(instanceDir, st); err != nil {
			return err
		}
		if err := m.restart(instanceID, instanceDir, st); err != nil {
			return err
		}
	}
	m.mu.Lock()
	if empty {
		delete(m.byInst, instanceID)
	}
	m.mu.Unlock()
	return nil
}

// List 返回实例下所有隧道状态。
func (m *Manager) List(instanceID string) []TunnelStatus {
	m.mu.RLock()
	st, ok := m.byInst[instanceID]
	m.mu.RUnlock()
	if !ok {
		return nil
	}

	running := m.isRunning(st)
	st.mu.Lock()
	lastErr := st.lastErr
	st.mu.Unlock()

	out := make([]TunnelStatus, 0, len(st.tunnels))
	for _, t := range st.tunnels {
		status := "stopped"
		if running {
			status = "running"
		} else if lastErr != "" {
			status = "error"
		}
		out = append(out, TunnelStatus{
			TunnelID:   t.TunnelID,
			Protocol:   t.Protocol,
			LocalPort:  t.LocalPort,
			RemotePort: t.RemotePort,
			Status:     status,
			Error:      lastErr,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TunnelID < out[j].TunnelID })
	return out
}

// LoadFromDisk 从实例目录恢复隧道定义（Daemon 启动时调用），**不启动 frpc**。
//
// 「只加载、不启动」是刻意的 —— 实例停止时 StopInstance() 已经杀过它的 frpc，
// Daemon 重启时若无条件再拉起来，等于废掉那次停止：
//   - 公网隧道会被一个「已停止」的实例占着
//   - 界面上会出现"实例 stopped / 隧道 running"这种自相矛盾的组合
//
// 要不要拉起由调用方按**实例的真实状态**决定（`daemon.Run()` 里只对在运行的
// 实例调 Resume）。定义已经落在内存里，所以实例之后启动时 Resume 照常能拉起，
// 「不启动」不会让隧道永久失效。
func (m *Manager) LoadFromDisk(instanceID, instanceDir string) error {
	instanceDir = absDir(instanceDir)
	b, err := os.ReadFile(metaPath(instanceDir))
	if err != nil {
		return nil // 无持久化文件
	}
	var saved struct {
		Server  Server            `json:"server"`
		Tunnels map[string]Tunnel `json:"tunnels"`
	}
	if err := json.Unmarshal(b, &saved); err != nil {
		return fmt.Errorf("解析隧道配置失败: %w", err)
	}
	if len(saved.Tunnels) == 0 {
		return nil
	}

	st := &instanceFRP{server: saved.Server, tunnels: saved.Tunnels, dir: instanceDir}
	m.mu.Lock()
	m.byInst[instanceID] = st
	m.mu.Unlock()

	return nil
}

// StopInstance 停止实例的 frpc（实例停止时调用）。
// 注意：隧道定义仍保留在内存中，实例再次启动时可用 Resume 重新拉起。
// StopInstance 停止该实例的 frpc 进程，但**保留隧道定义**。
//
// ⚠️ "保留定义"是刻意的：实例可能只是被**停止**，之后 `Resume` 要把隧道接回来。
// 但如果实例是被**删除**的，就必须用 `RemoveInstance` —— 否则留下的定义会在
// 下次实例启动时被 `LoadFromDisk` 读回来，变成继续占端口的"僵尸隧道"
// （2026-09-17 实测踩到，见 RemoveInstance 的注释）。
func (m *Manager) StopInstance(instanceID string) {
	m.mu.RLock()
	st, ok := m.byInst[instanceID]
	m.mu.RUnlock()
	if ok {
		m.stopProcess(st)
	}
}

// RemoveInstance 彻底移除某实例的穿透：停进程 + 丢弃全部隧道定义 + 清理残留文件。
//
// ---------------------------------------------------------------------------
// 为什么必须与 StopInstance 分开（"僵尸隧道"的根因，2026-09-17 实测）
// ---------------------------------------------------------------------------
// 事故现场：面板上把隧道删干净了、实例也删了，但 frpc 每次启动仍然把旧隧道一起拉起来：
//
//	tunnels.json: { "beta01-tcp-25565": {"remote_port": 25565}, ... }   ← 库已删，文件还在
//	frpc.log:     proxy added: [beta01-tcp-25565 beta01-tcp-25566]
//
// 后果是那个公网端口一直被占着，**实例自己反而绑不上**（Failed to bind to port），
// 而且**面板上完全看不出还有这么一条隧道** —— 换端口、重建实例都没用，极难自查。
//
// 两个缺口叠加才造成它：
//  1. `Remove()` 移除**最后一条**隧道时只删了 frpc.toml，漏删 tunnels.json
//  2. 删除实例走的是 `StopInstance`（保留定义），而面板侧只删了数据库记录
func (m *Manager) RemoveInstance(instanceID string) {
	m.mu.Lock()
	st, ok := m.byInst[instanceID]
	if !ok {
		m.mu.Unlock()
		return
	}
	dir := st.dir
	delete(m.byInst, instanceID)
	m.mu.Unlock()

	m.stopProcess(st)
	CleanInstanceFiles(dir)
}

// CleanInstanceFiles 清掉实例目录里由穿透产生的残留文件。
//
// ⚠️ **tunnels.json 必须一起删**：它保存着隧道定义，只要还在，
// 下次实例启动 `LoadFromDisk` 就会把"已经删掉的隧道"重新拉起来。
// 这是上面那个僵尸隧道的直接成因，别再退回成只删 frpc.toml。
//
// 只删这三个明确的文件、不做任何递归删除 —— 它会被用在"实例已被删除"的路径上，
// 万一 dir 传错也不该造成大面积误删。
func CleanInstanceFiles(dir string) {
	if dir == "" {
		return
	}
	dir = absDir(dir)
	for _, f := range []string{configPath(dir), metaPath(dir), pidFilePath(dir)} {
		_ = os.Remove(f)
	}
}

// StopAll 停止**所有实例**的 frpc 进程，返回其中本来在运行的数量。
//
// 用途：Daemon 收到 SIGTERM 退出时调用（见 cmd/daemon/main.go）。
//
// 为什么必须显式做这件事 —— 两个因素叠加，缺一不会出问题：
//  1. unit 用的是 **KillMode=process**，而这是**必须保持的**：
//     改成 control-group 的话，停 Daemon 会**连带杀掉用户的 Minecraft 实例**
//  2. 于是 Daemon 被信号杀掉时，它的子进程没人回收 ——
//     frpc 会变成 PPID=1 的孤儿继续跑
//     （2026-09-13 收工实测到一个：`frpc -c …/instances/11/frpc.toml`）
//
// ⚠️ **只停 frpc，绝不碰 java** —— 用户的实例该继续跑，这正是 KillMode=process 的意义。
// 本函数只操作 m.byInst 里的 frpc 进程，与实例进程无关。
//
// 隧道定义仍保留在内存中（与 StopInstance 一致）。Daemon 下次启动时
// LoadFromDisk 会重新拉起 frpc（`restart()` 里还会先 killStaleFrpc 兜底），
// 所以这里停掉不会让穿透永久失效。
func (m *Manager) StopAll() int {
	// 先在读锁里把实例收集出来，再逐个停：stopProcess 每个最多等 3 秒，
	// 攥着 RLock 会把其它操作全堵住。
	m.mu.RLock()
	all := make([]*instanceFRP, 0, len(m.byInst))
	for _, st := range m.byInst {
		all = append(all, st)
	}
	m.mu.RUnlock()

	// **并行**停，不串行：stopProcess 对不肯退出的 frpc 要等满 3 秒，
	// 串行就是 3N 秒，很容易撞上 unit 的 TimeoutStopSec=15 —— 一旦被 SIGKILL，
	// 剩下的 frpc 就照样变成孤儿，正是这个函数要避免的结果。并行时最坏 ~3 秒。
	var (
		wg      sync.WaitGroup
		stopped int
	)
	for _, st := range all {
		if m.isRunning(st) {
			stopped++
		}
		wg.Add(1)
		go func(st *instanceFRP) {
			defer wg.Done()
			m.stopProcess(st)
		}(st)
	}
	wg.Wait()
	return stopped
}

// Resume 重新拉起该实例的 frpc（定义须已在内存中）。
//
// 两个调用点，都是"实例该有 frpc、但 frpc 没在跑"：
//   - 实例**重新启动**后：停止实例会顺带断开隧道（StopInstance 杀掉 frpc），
//     若无人拉起，公网入口会一直不可用，需要管理员手动点「重新下发」
//   - Daemon 启动时对**仍在运行**（已被接管）的实例：把隧道接回来
func (m *Manager) Resume(instanceID string) error {
	m.mu.RLock()
	st, ok := m.byInst[instanceID]
	m.mu.RUnlock()
	if !ok || len(st.tunnels) == 0 {
		return nil // 该实例没有隧道
	}
	if m.isRunning(st) {
		return nil // 已在运行
	}
	m.log().Info("重新拉起穿透隧道", "instance", instanceID, "tunnels", len(st.tunnels))
	return m.restart(instanceID, st.dir, st)
}

// HasTunnels 判断实例是否配置了隧道。
func (m *Manager) HasTunnels(instanceID string) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	st, ok := m.byInst[instanceID]
	return ok && len(st.tunnels) > 0
}

// log 返回日志器（manager 内目前不持有 logger，保留扩展点）。
func (m *Manager) log() *slog.Logger { return slog.Default() }

// ---- 内部实现 ----

// absDir 规范化为绝对路径。
// frpc 以实例目录为工作目录运行，配置与日志若使用相对路径会被二次拼接而找不到文件。
func absDir(dir string) string {
	if dir == "" {
		return dir
	}
	if abs, err := filepath.Abs(dir); err == nil {
		return abs
	}
	return dir
}

func configPath(dir string) string  { return filepath.Join(dir, "frpc.toml") }
func logPath(dir string) string     { return filepath.Join(dir, "logs", "frpc.log") }
func metaPath(dir string) string    { return filepath.Join(dir, "tunnels.json") }
func pidFilePath(dir string) string { return filepath.Join(dir, "frpc.pid") }

// killStaleFrpc 若上次运行的 frpc 仍在（进程未随管理器退出），先将其终止。
// 否则会出现「frpc 重复启动 → 代理名已存在」的冲突。
func killStaleFrpc(dir string) {
	b, err := os.ReadFile(pidFilePath(dir))
	if err != nil {
		return
	}
	var pid int
	if _, err := fmt.Sscanf(strings.TrimSpace(string(b)), "%d", &pid); err != nil || pid <= 0 {
		return
	}
	if err := syscall.Kill(pid, 0); err != nil {
		_ = os.Remove(pidFilePath(dir)) // 进程已不存在
		return
	}
	_ = syscall.Kill(-pid, syscall.SIGTERM)
	_ = syscall.Kill(pid, syscall.SIGTERM)
	for i := 0; i < 20; i++ {
		time.Sleep(150 * time.Millisecond)
		if err := syscall.Kill(pid, 0); err != nil {
			break
		}
	}
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	_ = syscall.Kill(pid, syscall.SIGKILL)
	_ = os.Remove(pidFilePath(dir))
}

// persist 把隧道定义写入实例目录（目录不存在时自动创建）。
func (m *Manager) persist(dir string, st *instanceFRP) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("创建穿透工作目录失败: %w", err)
	}

	m.mu.RLock()
	payload := struct {
		Server  Server            `json:"server"`
		Tunnels map[string]Tunnel `json:"tunnels"`
	}{Server: st.server, Tunnels: st.tunnels}
	m.mu.RUnlock()

	b, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(metaPath(dir), b, 0o600)
}

// restart 重新生成配置并重启 frpc 进程。
//
// 这是"要不要启动 frpc"的**唯一入口**，因此不变式也守在这里：
// 实例没在运行时只登记定义、不启动进程（见 SetInstanceState）。
func (m *Manager) restart(instanceID, dir string, st *instanceFRP) error {
	dir = absDir(dir)
	m.stopProcess(st)
	killStaleFrpc(dir) // 清理上次遗留的 frpc（如管理器崩溃）

	// 实例没在跑：把该实例的 frpc 收干净，但**不要**再拉起来。
	//
	// 位置刻意放在 stopProcess/killStaleFrpc **之后**：这样"实例已停止、
	// frpc 却还活着"的遗留状态（历史上由本函数无条件启动造成）会被顺手纠正，
	// 而不是继续留着占住公网端口。
	if !m.instanceRunning(instanceID) {
		m.log().Info("实例未运行，隧道只登记不启动",
			"instance", instanceID, "tunnels", len(st.tunnels))
		return nil
	}

	if !m.Available() {
		st.mu.Lock()
		st.lastErr = "未安装 frpc（未在 PATH 中找到）"
		st.mu.Unlock()
		return fmt.Errorf("未安装 frpc，无法建立穿透；请在节点上安装 frp 客户端")
	}

	if err := os.MkdirAll(filepath.Join(dir, "logs"), 0o755); err != nil {
		return err
	}
	cfg, err := renderConfig(st, dir)
	if err != nil {
		return err
	}
	if err := os.WriteFile(configPath(dir), []byte(cfg), 0o600); err != nil {
		return fmt.Errorf("写入 frpc 配置失败: %w", err)
	}

	lf, err := os.OpenFile(logPath(dir), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}

	cmd := exec.Command(m.frpcPath, "-c", configPath(dir))
	cmd.Dir = dir
	cmd.Stdout = lf
	cmd.Stderr = lf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		lf.Close()
		st.mu.Lock()
		st.lastErr = "启动 frpc 失败: " + err.Error()
		st.mu.Unlock()
		return fmt.Errorf("启动 frpc 失败: %w", err)
	}

	st.mu.Lock()
	st.cmd = cmd
	st.logF = lf
	st.lastErr = ""
	st.mu.Unlock()

	// 记录 PID，便于下次启动时清理遗留进程
	_ = os.WriteFile(pidFilePath(dir), []byte(fmt.Sprintf("%d", cmd.Process.Pid)), 0o644)

	// 等待进程退出（若立即退出，记录错误信息供状态查询）
	go func() {
		err := cmd.Wait()
		_ = os.Remove(pidFilePath(dir))
		st.mu.Lock()
		if st.cmd == cmd {
			st.cmd = nil
			if st.logF != nil {
				st.logF.Close()
				st.logF = nil
			}
			if err != nil {
				// 带上日志中的关键原因：frpc 日志不再对文件管理开放，
				// 管理员需要从状态里直接看到失败原因
				reason := lastLogReason(dir)
				msg := "frpc 已退出: " + err.Error()
				if reason != "" {
					msg += " — " + reason
				}
				st.lastErr = msg
			} else {
				st.lastErr = "frpc 已退出"
			}
		}
		st.mu.Unlock()
	}()

	return nil
}

// stopProcess 停止 frpc 进程。
func (m *Manager) stopProcess(st *instanceFRP) {
	st.mu.Lock()
	cmd := st.cmd
	logF := st.logF
	st.cmd = nil
	st.logF = nil
	st.mu.Unlock()

	if cmd != nil && cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		done := make(chan struct{})
		go func() { _, _ = cmd.Process.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
	}
	if logF != nil {
		_ = logF.Close()
	}
}

// isRunning 判断 frpc 是否在运行。
func (m *Manager) isRunning(st *instanceFRP) bool {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.cmd != nil && st.cmd.Process != nil
}

// lastLogReason 从 frpc 日志尾部提取最关键的一行，用于状态展示。
//
// frpc 的原始日志不再通过文件管理暴露（其中包含 frp 内部信息），
// 因此在进程异常退出时把真正的原因摘出来放进状态里，
// 管理员在「穿透管理」即可看到失败原因，无需接触日志文件。
func lastLogReason(dir string) string {
	f, err := os.Open(logPath(dir))
	if err != nil {
		return ""
	}
	defer f.Close()

	const maxTail = 32 * 1024
	fi, err := f.Stat()
	if err != nil {
		return ""
	}
	if size := fi.Size(); size > maxTail {
		if _, err := f.Seek(size-maxTail, io.SeekStart); err != nil {
			return ""
		}
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return ""
	}

	lines := strings.Split(string(data), "\n")
	// 优先取错误/告警行，否则取最后一行非空内容
	fallback := ""
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" {
			continue
		}
		if fallback == "" {
			fallback = line
		}
		if strings.Contains(line, "[E]") || strings.Contains(line, "[W]") ||
			strings.Contains(line, "error") || strings.Contains(line, "failed") {
			return truncate(line, 300)
		}
	}
	return truncate(fallback, 300)
}

// truncate 限制字符串长度（避免超长日志行塞满状态字段）。
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// renderConfig 生成 frpc TOML 配置。
// frp 的代理名必须是 ASCII 安全字符，因此统一使用 tunnel_id（唯一且安全），
// 面板中的显示名仅用于 UI。
func renderConfig(st *instanceFRP, dir string) (string, error) {
	mServer := st.server
	if mServer.Host == "" || mServer.BindPort <= 0 {
		return "", fmt.Errorf("frps 服务端信息不完整")
	}

	var sb strings.Builder
	sb.WriteString("# 由 ATL-MCPanel 自动生成，请勿手工修改\n")
	fmt.Fprintf(&sb, "serverAddr = %q\n", mServer.Host)
	fmt.Fprintf(&sb, "serverPort = %d\n", mServer.BindPort)
	if mServer.Token != "" {
		fmt.Fprintf(&sb, "auth.method = \"token\"\nauth.token = %q\n", mServer.Token)
	}
	fmt.Fprintf(&sb, "log.to = %q\n", filepath.Join(dir, "logs", "frpc.log"))
	sb.WriteString("log.level = \"info\"\n")

	ids := make([]string, 0, len(st.tunnels))
	for id := range st.tunnels {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	for _, id := range ids {
		t := st.tunnels[id]
		proto := t.Protocol
		if proto == "" {
			proto = "tcp"
		}
		sb.WriteString("\n[[proxies]]\n")
		fmt.Fprintf(&sb, "name = %q\n", sanitizeProxyName(t.TunnelID))
		fmt.Fprintf(&sb, "type = %q\n", proto)

		switch proto {
		case "http", "https":
			// vhost 模式：按域名路由
			if len(t.CustomDomains) > 0 {
				fmt.Fprintf(&sb, "customDomains = [%s]\n", quoteList(t.CustomDomains))
			} else if t.Subdomain != "" {
				fmt.Fprintf(&sb, "subdomain = %q\n", t.Subdomain)
			} else {
				return "", fmt.Errorf("%s 模式需要指定 customDomains 或 subdomain", proto)
			}

			// https + https2http 插件：frpc 终止 TLS，转发到本地 HTTP 服务
			if proto == "https" && t.UseTLS && t.CertFile != "" && t.KeyFile != "" {
				if _, err := os.Stat(t.CertFile); err != nil {
					return "", fmt.Errorf("证书文件不存在: %s", t.CertFile)
				}
				if _, err := os.Stat(t.KeyFile); err != nil {
					return "", fmt.Errorf("私钥文件不存在: %s", t.KeyFile)
				}
				sb.WriteString("\n[proxies.plugin]\n")
				sb.WriteString("type = \"https2http\"\n")
				fmt.Fprintf(&sb, "localAddr = \"127.0.0.1:%d\"\n", t.LocalPort)
				fmt.Fprintf(&sb, "crt = %q\n", t.CertFile)
				fmt.Fprintf(&sb, "key = %q\n", t.KeyFile)
				// 插件模式下不能再设置 localIP/localPort
			} else {
				sb.WriteString("localIP = \"127.0.0.1\"\n")
				fmt.Fprintf(&sb, "localPort = %d\n", t.LocalPort)
			}
		default: // tcp / udp
			sb.WriteString("localIP = \"127.0.0.1\"\n")
			fmt.Fprintf(&sb, "localPort = %d\n", t.LocalPort)
			fmt.Fprintf(&sb, "remotePort = %d\n", t.RemotePort)
		}
	}
	return sb.String(), nil
}

// quoteList 把字符串切片转为 TOML 字符串数组。
func quoteList(items []string) string {
	parts := make([]string, 0, len(items))
	for _, s := range items {
		parts = append(parts, fmt.Sprintf("%q", strings.TrimSpace(s)))
	}
	return strings.Join(parts, ", ")
}

// sanitizeProxyName frp 代理名只允许字母数字与 _ -。
func sanitizeProxyName(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	if b.Len() == 0 {
		return "proxy"
	}
	return b.String()
}

// SetAdmin 注入 frps 管理 API 客户端（Daemon 启动时调用）。
// 未注入时 InstanceTraffic 返回 ok=false，调用方应回退到节点整机统计。
func (m *Manager) SetAdmin(c *AdminClient) {
	m.mu.Lock()
	m.admin = c
	m.mu.Unlock()
}

// InstanceTraffic 返回该实例隧道的流量。
//
// 返回值为**当日累计**收发字节、当前连接数，以及相对上次采样的速率。
// ok=false 表示未启用管理 API 或该实例没有隧道，调用方应回退到整机统计。
func (m *Manager) InstanceTraffic(instanceID string) (inTotal, outTotal, inRate, outRate, conns int64, ok bool) {
	m.mu.RLock()
	admin := m.admin
	st, has := m.byInst[instanceID]
	m.mu.RUnlock()

	if admin == nil || !has || len(st.tunnels) == 0 {
		return 0, 0, 0, 0, 0, false
	}

	names := make(map[string]bool, len(st.tunnels))
	st.mu.Lock()
	for id := range st.tunnels {
		names[id] = true
	}
	st.mu.Unlock()

	in, out, cn, err := admin.trafficTotal(names)
	if err != nil {
		return 0, 0, 0, 0, 0, false
	}
	if m.sampler != nil {
		inRate, outRate = m.sampler.Rate(in, out)
	}
	return in, out, inRate, outRate, cn, true
}
