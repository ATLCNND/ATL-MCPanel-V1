// Package cgroup 为实例进程提供资源限制（CPU 配额 / 内存上限）。
//
// 同时支持 **cgroup v2 与 v1**：
//   - v2（内核 ≥ 4.15，统一层级）：cpu.max / memory.max
//   - v1（老内核，如 CentOS 7 的 3.10）：cpu.cfs_quota_us / memory.limit_in_bytes
//
// 为什么必须支持 v1（2026-09-17 决定）：内测节点是 CentOS 7（内核 3.10，**没有 v2**）。
// 只支持 v2 的话，那台机器上"超开几个实例"等于**完全没有内存与 CPU 上限** ——
// 一个实例内存泄漏就吃光 7.8G（而且没有 swap），OOM 会把所有实例一起杀掉。
// 实测那台机器的 cgroup v1 里 memory 与 cpu 控制器都在，所以能把硬限额补上。
//
// 设计原则：
//   - **绝不阻断实例启动**：任何一步失败都只记录并降级为"无限制"，不影响 Minecraft 运行
//   - 默认不限制：管理员显式设置配额后才生效，避免误配导致服务器被限速
//   - 使用硬配额（v2 的 cpu.max / v1 的 cfs_quota），使"上限 X%"可被准确承诺
package cgroup

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// cgroup 版本。0 表示不可用（所有操作降级为空操作）。
const (
	cgDisabled = 0
	cgV1       = 1
	cgV2       = 2
)

// 默认挂载点与配额参数
const (
	DefaultRoot = "/sys/fs/cgroup/atlmcpanel"

	// CPU period（微秒）。100ms 与内核默认一致，v1/v2 通用。
	cpuPeriodUsec = 100000

	// quotaUsecPerPercent 1% CPU = 1000 微秒/100ms（即 1% 个核心）
	quotaUsecPerPercent = cpuPeriodUsec / 100

	// v1 的根分组名：v1 是多层级（每个控制器挂载点一个），
	// 各挂载点下用一个同名分组把我们的实例归到一起。
	v1GroupName = "atlmcpanel"

	// v1 用 -1 表示"不限制"
	v1Unlimited = -1

	// pageSize 用于把内存上限向上取整到页边界。
	// v1 内核对齐到页，传非对齐值虽然多数情况能接受，
	// 但向上取整更稳妥，也避免"设了 1000 字节却立刻 OOM"这种反直觉行为。
	pageSize = 4096
)

// cgroupV2Base cgroup v2 的挂载点。做成变量是为了让测试能指向一个临时目录，
// 从而在不碰真实 /sys 的前提下验证"v2 还是 v1"的选择逻辑。
var cgroupV2Base = "/sys/fs/cgroup"

// Manager cgroup 资源限制管理器（v1 / v2 自适应）。
type Manager struct {
	root    string // v2 的自定义根目录（v1 用挂载点 + v1GroupName）
	enabled bool
	reason  string
	version int

	// mounts 仅 v1 使用：控制器名 → 挂载点，例如
	//   {"cpu": "/sys/fs/cgroup/cpu,cpuacct",
	//    "cpuacct": "/sys/fs/cgroup/cpu,cpuacct",
	//    "memory": "/sys/fs/cgroup/memory"}
	mounts map[string]string
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

// Version 返回实际使用的 cgroup 版本（0=不可用，1=v1，2=v2）。
// 用于日志与节点能力上报 —— 管理员需要知道"配额到底生不生效"。
func (m *Manager) Version() int { return m.version }

// Init 探测系统支持的 cgroup 版本并初始化。
// 失败不会返回错误（仅禁用限制），调用方可根据 Enabled() 决定是否告警。
func (m *Manager) Init() {
	if m.initV2() {
		return
	}
	if m.initV1() {
		return
	}
	if m.reason == "" {
		m.disable("未检测到可用的 cgroup（v2 与 v1 都没有）")
	}
}

func (m *Manager) disable(reason string) {
	m.enabled = false
	m.version = cgDisabled
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

// ---------------------------------------------------------------------------
// 公共 API（按版本分派）
// ---------------------------------------------------------------------------

// Apply 为实例创建 cgroup 并设置 CPU 配额。
// cpuPercent <= 0 表示不限制。
func (m *Manager) Apply(instanceID string, cpuPercent int) error {
	if !m.enabled {
		return nil
	}
	switch m.version {
	case cgV2:
		return m.v2Apply(instanceID, cpuPercent)
	case cgV1:
		return m.v1Apply(instanceID, cpuPercent)
	}
	return nil
}

// SetMemoryLimit 设置内存上限（bytes）。limit <= 0 表示不限制。
// 默认不启用：JVM 的 -Xmx 已限制堆，额外设内存上限需要留出堆外开销余量。
func (m *Manager) SetMemoryLimit(instanceID string, limitBytes int64) error {
	if !m.enabled {
		return nil
	}
	switch m.version {
	case cgV2:
		return m.v2SetMemoryLimit(instanceID, limitBytes)
	case cgV1:
		return m.v1SetMemoryLimit(instanceID, limitBytes)
	}
	return nil
}

// Assign 把进程放入实例的 cgroup（应在进程启动后立即调用）。
func (m *Manager) Assign(instanceID string, pid int) error {
	if !m.enabled || pid <= 0 {
		return nil
	}
	_, err := m.assign(instanceID, []int{pid})
	return err
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
	return m.assign(instanceID, descendants(pid))
}

// assign 把一组 PID 写入实例所属 cgroup。
//
// v1 是**多层级**的：进程必须分别加进 cpu 与 memory 两个层级的同名分组，
// 只加一个的话另一个控制器对该进程不生效 —— 这是 v1 最容易漏的地方。
func (m *Manager) assign(instanceID string, pids []int) (int, error) {
	if len(pids) == 0 {
		return 0, nil
	}
	targets := m.procsFiles(instanceID)
	if len(targets) == 0 {
		return 0, nil
	}

	moved := 0
	var lastErr error
	for _, p := range pids {
		okAll := true
		for _, f := range targets {
			if err := os.WriteFile(f, []byte(strconv.Itoa(p)), 0o644); err != nil {
				lastErr = err
				okAll = false
			}
		}
		if okAll {
			moved++
		}
	}
	if moved == 0 && lastErr != nil {
		return 0, fmt.Errorf("将进程加入 cgroup 失败: %w", lastErr)
	}
	return moved, nil
}

// procsFiles 返回需要写入 PID 的所有 cgroup.procs 路径（v1 每个层级一个）。
func (m *Manager) procsFiles(instanceID string) []string {
	switch m.version {
	case cgV2:
		return []string{filepath.Join(m.dir(instanceID), "cgroup.procs")}
	case cgV1:
		var out []string
		for _, d := range m.v1Dirs(instanceID) {
			out = append(out, filepath.Join(d, "cgroup.procs"))
		}
		return out
	}
	return nil
}

// Stats 读取实例的 CPU 使用统计。
// usageUsec 为累计 CPU 时间（微秒）；throttledUsec 为被限流的总时长（微秒）。
func (m *Manager) Stats(instanceID string) (usageUsec, throttledUsec uint64, err error) {
	if !m.enabled {
		return 0, 0, fmt.Errorf("cgroup 未启用")
	}
	switch m.version {
	case cgV2:
		return m.v2Stats(instanceID)
	case cgV1:
		return m.v1Stats(instanceID)
	}
	return 0, 0, fmt.Errorf("cgroup 未启用")
}

// Remove 删除实例的 cgroup（实例被删除时调用）。
func (m *Manager) Remove(instanceID string) error {
	if !m.enabled {
		return nil
	}
	switch m.version {
	case cgV2:
		return m.v2Remove(instanceID)
	case cgV1:
		return m.v1Remove(instanceID)
	}
	return nil
}

// dir 返回实例在 **v2** 层级下的 cgroup 目录。
// v1 用 v1Dirs（每个控制器一个目录）。
func (m *Manager) dir(instanceID string) string {
	return filepath.Join(m.root, sanitize(instanceID))
}

// CgroupPath 返回便于在日志/界面上展示的 cgroup 位置。
func (m *Manager) CgroupPath(instanceID string) string {
	switch m.version {
	case cgV2:
		return m.dir(instanceID)
	case cgV1:
		dirs := m.v1Dirs(instanceID)
		if len(dirs) == 0 {
			return ""
		}
		return dirs[0]
	}
	return ""
}

// RootPath 返回我们的分组根目录（v1 有多个层级，这里返回 cpu 层级那个）。
// 启动日志里报出来，排查"配额没生效"时一眼能看出实际写到了哪里。
func (m *Manager) RootPath() string {
	switch m.version {
	case cgV2:
		return m.root
	case cgV1:
		if mnt := m.mounts["cpu"]; mnt != "" {
			return filepath.Join(mnt, v1GroupName)
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// 公共工具
// ---------------------------------------------------------------------------

// descendants 返回 pid 自身及其所有后代 PID（广度优先，深度上限防止异常环）。
func descendants(pid int) []int {
	seen := map[int]bool{pid: true}
	queue := []int{pid}
	out := []int{pid}
	depth := 0

	for len(queue) > 0 && depth < 64 {
		next := []int{}
		for _, cur := range queue {
			for _, child := range childrenOf(cur) {
				if seen[child] {
					continue
				}
				seen[child] = true
				out = append(out, child)
				next = append(next, child)
			}
		}
		queue = next
		depth++
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
