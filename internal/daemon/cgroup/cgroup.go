// Package cgroup 通过 cgroup v2 为实例进程提供资源限制（CPU 配额 / 内存上限）。
//
// 设计原则：
//   - **绝不阻断实例启动**：任何一步失败都只记录并降级为"无限制"，不影响 Minecraft 运行
//   - 默认不限制：管理员显式设置配额后才生效，避免误配导致服务器被限速
//   - 使用 cpu.max 硬配额（而非 shares 权重），使"上限 X%"可被准确承诺
//
// cgroup v2 关键约束：
//   - "no internal process"：父 cgroup 若有子 cgroup，则自身不能放进程。
//     因此我们把进程只放进 <root>/<实例ID>/，root 自身保持空。
//   - 控制器需先在父级 cgroup.subtree_control 中启用，子级才能使用。
package cgroup

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// 默认挂载点与配额参数
const (
	DefaultRoot  = "/sys/fs/cgroup/atlmcpanel"
	cgroupV2Base = "/sys/fs/cgroup"

	// cpu.max 的 period（微秒）。100ms 与内核默认一致。
	cpuPeriodUsec = 100000

	// quotaUsecPerPercent 1% CPU = 1000 微秒/100ms（即 1% 个核心）
	quotaUsecPerPercent = cpuPeriodUsec / 100
)

// Manager cgroup 资源限制管理器。
type Manager struct {
	root    string
	enabled bool // 是否实际启用（cgroup v2 可用且初始化成功）
	reason  string
}

// New 创建管理器。root 为空时使用 DefaultRoot。
func New(root string) *Manager {
	if root == "" {
		root = DefaultRoot
	}
	return &Manager{root: root}
}

// Enabled 返回限制是否生效。
func (m *Manager) Enabled() bool { return m.enabled }

// Reason 返回禁用原因（Enabled 为 false 时有意义）。
func (m *Manager) Reason() string { return m.reason }

// Init 检测 cgroup v2 可用性并创建根 cgroup。
// 失败不会返回错误（仅禁用限制），调用方可根据 Enabled() 决定是否告警。
func (m *Manager) Init() {
	// 1. cgroup v2 判定：只有 v2 才有 cgroup.controllers 文件
	if _, err := os.Stat(filepath.Join(cgroupV2Base, "cgroup.controllers")); err != nil {
		m.disable("未检测到 cgroup v2（可能是 cgroup v1 或容器环境未挂载）")
		return
	}
	// 2. 需要 cpu 控制器
	if !m.controllerAvailable("cpu") {
		m.disable("cgroup v2 未提供 cpu 控制器")
		return
	}
	// 3. 需要写权限（Daemon 通常以 root 运行）
	if err := os.MkdirAll(m.root, 0o755); err != nil {
		m.disable("创建 cgroup 根目录失败: " + err.Error())
		return
	}
	// 4. 在根 cgroup 启用控制器，子 cgroup 才能设置 cpu.max
	if err := os.WriteFile(filepath.Join(m.root, "cgroup.subtree_control"), []byte("+cpu +memory"), 0o644); err != nil {
		// 只启用 cpu 也可以工作；内存控制器不可用时忽略
		if err2 := os.WriteFile(filepath.Join(m.root, "cgroup.subtree_control"), []byte("+cpu"), 0o644); err2 != nil {
			m.disable("启用 cgroup 控制器失败: " + err2.Error())
			return
		}
	}
	m.enabled = true
	m.reason = ""
}

func (m *Manager) disable(reason string) {
	m.enabled = false
	m.reason = reason
}

// controllerAvailable 检查 cgroup v2 是否提供指定控制器。
func (m *Manager) controllerAvailable(name string) bool {
	b, err := os.ReadFile(filepath.Join(cgroupV2Base, "cgroup.controllers"))
	if err != nil {
		return false
	}
	for _, f := range strings.Fields(string(b)) {
		if f == name {
			return true
		}
	}
	return false
}

// dir 返回实例的 cgroup 目录。
func (m *Manager) dir(instanceID string) string {
	return filepath.Join(m.root, sanitize(instanceID))
}

// sanitize 保证实例 ID 可安全用作目录名。
func sanitize(id string) string {
	var b strings.Builder
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	if b.Len() == 0 {
		return "instance"
	}
	return b.String()
}

// Apply 为实例创建 cgroup 并设置 CPU 配额。
// cpuPercent <= 0 表示不限制（写入 max）。
func (m *Manager) Apply(instanceID string, cpuPercent int) error {
	if !m.enabled {
		return nil
	}
	dir := m.dir(instanceID)
	// 若旧 cgroup 已无进程，先删除再重建：cgroup v2 的 cpu.stat 是累计值，
	// 复用同一目录会让"本次运行"的 CPU 统计叠加历史数据。
	// 目录中仍有进程时 os.Remove 会失败（EBUSY），此时保持不动。
	_ = os.Remove(dir)

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("创建 cgroup 失败: %w", err)
	}
	return m.setCPUQuota(instanceID, cpuPercent)
}

// setCPUQuota 写入 cpu.max。
func (m *Manager) setCPUQuota(instanceID string, cpuPercent int) error {
	val := "max " + strconv.Itoa(cpuPeriodUsec)
	if cpuPercent > 0 {
		val = fmt.Sprintf("%d %d", cpuPercent*quotaUsecPerPercent, cpuPeriodUsec)
	}
	if err := os.WriteFile(filepath.Join(m.dir(instanceID), "cpu.max"), []byte(val), 0o644); err != nil {
		return fmt.Errorf("设置 CPU 配额失败: %w", err)
	}
	return nil
}

// SetMemoryLimit 设置内存上限（bytes）。limit <= 0 表示不限制。
// 默认不启用：JVM 的 -Xmx 已限制堆，额外设 memory.max 需要留出堆外开销余量。
func (m *Manager) SetMemoryLimit(instanceID string, limitBytes int64) error {
	if !m.enabled {
		return nil
	}
	if _, err := os.Stat(filepath.Join(m.dir(instanceID), "memory.max")); err != nil {
		return nil // 内存控制器不可用，静默跳过
	}
	val := "max"
	if limitBytes > 0 {
		val = strconv.FormatInt(limitBytes, 10)
	}
	if err := os.WriteFile(filepath.Join(m.dir(instanceID), "memory.max"), []byte(val), 0o644); err != nil {
		return fmt.Errorf("设置内存上限失败: %w", err)
	}
	return nil
}

// Assign 把进程放入实例的 cgroup（应在进程启动后立即调用）。
func (m *Manager) Assign(instanceID string, pid int) error {
	if !m.enabled || pid <= 0 {
		return nil
	}
	procs := filepath.Join(m.dir(instanceID), "cgroup.procs")
	if err := os.WriteFile(procs, []byte(strconv.Itoa(pid)), 0o644); err != nil {
		return fmt.Errorf("将进程加入 cgroup 失败: %w", err)
	}
	return nil
}

// AssignTree 把 pid 及其**全部后代进程**一并移入实例的 cgroup，返回移动的进程数。
//
// 为什么必须带上后代：
// cgroup 成员是按进程逐一记录的，把父进程移入 **不会** 连带已经存在的子进程。
// 而我们在进程启动之后才施加限制，此时 start.sh 这类启动方式
// （sh 脚本 → fork 出 timeout/java 等真正负载）早已完成 fork，
// 若只移动 sh 本身，真正的负载将完全不受配额约束。
// 默认的 java -jar 方式是直接子进程，不受此影响；
// 若脚本使用 exec（面板生成的脚本即如此），PID 不变，同样不受影响。
func (m *Manager) AssignTree(instanceID string, pid int) (int, error) {
	if !m.enabled || pid <= 0 {
		return 0, nil
	}
	pids := descendants(pid)
	if len(pids) == 0 {
		return 0, nil
	}
	procs := filepath.Join(m.dir(instanceID), "cgroup.procs")

	moved := 0
	var lastErr error
	for _, p := range pids {
		if err := os.WriteFile(procs, []byte(strconv.Itoa(p)), 0o644); err != nil {
			lastErr = err
			continue
		}
		moved++
	}
	if moved == 0 && lastErr != nil {
		return 0, fmt.Errorf("将进程树加入 cgroup 失败: %w", lastErr)
	}
	return moved, nil
}

// descendants 返回 pid 自身及其所有后代 PID（广度优先，深度上限防止异常环）。
func descendants(pid int) []int {
	seen := map[int]bool{pid: true}
	queue := []int{pid}
	out := []int{pid}

	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, child := range childrenOf(cur) {
			if seen[child] {
				continue
			}
			seen[child] = true
			out = append(out, child)
			queue = append(queue, child)
		}
	}
	return out
}

// childrenOf 读取某个进程的直接子进程（来自 /proc/<pid>/task/<tid>/children）。
func childrenOf(pid int) []int {
	taskDir := filepath.Join("/proc", strconv.Itoa(pid), "task")
	tasks, err := os.ReadDir(taskDir)
	if err != nil {
		return nil
	}
	var out []int
	for _, t := range tasks {
		b, err := os.ReadFile(filepath.Join(taskDir, t.Name(), "children"))
		if err != nil {
			continue
		}
		for _, f := range strings.Fields(string(b)) {
			if n, err := strconv.Atoi(f); err == nil {
				out = append(out, n)
			}
		}
	}
	return out
}

// Stats 读取实例的 CPU 使用统计。
// usageUsec 为累计 CPU 时间（微秒）；throttledUsec 为被限流的总时长（微秒）。
func (m *Manager) Stats(instanceID string) (usageUsec, throttledUsec uint64, err error) {
	if !m.enabled {
		return 0, 0, fmt.Errorf("cgroup 未启用")
	}
	b, err := os.ReadFile(filepath.Join(m.dir(instanceID), "cpu.stat"))
	if err != nil {
		return 0, 0, err
	}
	for _, line := range strings.Split(string(b), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		v, _ := strconv.ParseUint(fields[1], 10, 64)
		switch fields[0] {
		case "usage_usec":
			usageUsec = v
		case "throttled_usec":
			throttledUsec = v
		}
	}
	return usageUsec, throttledUsec, nil
}

// Remove 删除实例的 cgroup（实例被删除时调用）。
func (m *Manager) Remove(instanceID string) error {
	if !m.enabled {
		return nil
	}
	// 目录内若仍有进程，删除会失败；先尝试清空
	if err := os.Remove(m.dir(instanceID)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
