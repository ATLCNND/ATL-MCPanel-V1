package cgroup

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// ---------------------------------------------------------------------------
// cgroup v1 支持
// ---------------------------------------------------------------------------
//
// v1 与 v2 的差异（这些差异每一条都能让限额静默失效，逐条对照实现）：
//
//	             v2                          v1
//	层级         统一挂载 /sys/fs/cgroup      每个控制器各自挂载
//	CPU 配额     cpu.max = "<quota> <period>" cpu.cfs_quota_us + cpu.cfs_period_us
//	内存上限     memory.max = "<bytes>|max"   memory.limit_in_bytes = "<bytes>|-1"
//	加进程       cgroup.procs（含全部控制器）  必须**每个层级各写一次** cgroup.procs
//	CPU 统计     cpu.stat 里的 usage_usec      cpuacct/cpuacct.usage（**纳秒**）+ cpu.stat 的 throttled_time（纳秒）
//
// ⚠️ 最容易漏的两点：
//  1. **多层级**：进程只加进 cpu 层级、没加进 memory 层级，内存上限就完全不生效，
//     而且不会有任何报错 —— 表现为"设了内存上限却照样被吃光"。
//  2. **单位**：v1 的 cpuacct.usage 与 throttled_time 都是**纳秒**，
//     v2 的 usage_usec/throttled_usec 是微秒。混淆会让 CPU 使用率放大 1000 倍。
// ---------------------------------------------------------------------------

// v1Candidates v1 探测时允许的挂载点父目录（测试可替换）。
var v1Candidates = "/sys/fs/cgroup"

// procMounts 解析 /proc/mounts 用的路径（测试可替换）。
var procMounts = "/proc/mounts"

// initV1 尝试按 cgroup v1 初始化。成功返回 true。
func (m *Manager) initV1() bool {
	mounts := detectV1Mounts()
	if mounts["cpu"] == "" {
		m.reason = "未检测到 cgroup v1 的 cpu 控制器（可能是 cgroup v2 或容器环境未挂载）"
		return false
	}
	// memory 缺失不算失败：CPU 配额仍然可用，内存上限静默跳过（与 v2 的处理一致）
	m.mounts = mounts

	// 在各层级下建好根分组。v1 没有"父 cgroup 不能放进程"的限制，
	// 所以不需要像 v2 那样处理 subtree_control。
	for _, mnt := range mounts {
		groupRoot := filepath.Join(mnt, v1GroupName)
		if err := os.MkdirAll(groupRoot, 0o755); err != nil {
			m.reason = "创建 cgroup v1 分组失败: " + err.Error()
			return false
		}
	}

	m.enabled = true
	m.version = cgV1
	m.reason = ""
	return true
}

// detectV1Mounts 解析 /proc/mounts，找出 cpu / cpuacct / memory 三个控制器的挂载点。
//
// 为什么解析挂载表而不是猜路径：
// v1 的挂载点名字取决于发行版和 systemd 版本 —— 实测 CentOS 7 是
// `/sys/fs/cgroup/cpu,cpuacct`（cpu 与 cpuacct **合并挂载**），
// 而 Debian 系常见 `/sys/fs/cgroup/cpu` 与 `/sys/fs/cgroup/cpuacct` 分开。
// 硬编码任何一种都会在另一种上失效。
func detectV1Mounts() map[string]string {
	mounts := map[string]string{}
	b, err := os.ReadFile(procMounts)
	if err != nil {
		return mounts
	}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) < 4 || f[2] != "cgroup" {
			continue
		}
		mnt := unescapeMountPath(f[1])
		for _, c := range strings.Split(f[3], ",") {
			switch c {
			case "cpu", "cpuacct", "memory":
				if mounts[c] == "" {
					mounts[c] = mnt
				}
			}
		}
	}
	return mounts
}

// unescapeMountPath 还原 /proc/mounts 里的八进制转义（空格是 \040，反斜杠是 \134）。
func unescapeMountPath(s string) string {
	if !strings.Contains(s, "\\") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if n, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(n))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// v1Dirs 返回实例在各控制器层级下的 cgroup 目录（至少一个）。
//
// 顺序固定为 cpu → memory，方便测试与排查；去重以避免
// `cpu,cpuacct` 合并挂载时同一个目录被算两次。
func (m *Manager) v1Dirs(instanceID string) []string {
	name := sanitize(instanceID)
	seen := map[string]bool{}
	var out []string
	for _, c := range []string{"cpu", "memory"} {
		mnt := m.mounts[c]
		if mnt == "" {
			continue
		}
		d := filepath.Join(mnt, v1GroupName, name)
		if seen[d] {
			continue
		}
		seen[d] = true
		out = append(out, d)
	}
	return out
}

// v1Apply 创建实例在各层级的 cgroup 目录并设置 CPU 配额。
func (m *Manager) v1Apply(instanceID string, cpuPercent int) error {
	dirs := m.v1Dirs(instanceID)
	if len(dirs) == 0 {
		return fmt.Errorf("没有可用的 cgroup v1 层级")
	}
	for _, d := range dirs {
		// 与 v2 同理：旧目录若已无进程先删掉，避免 cpuacct 累计统计叠加历史运行
		_ = os.Remove(d)
		if err := os.MkdirAll(d, 0o755); err != nil {
			return fmt.Errorf("创建 cgroup 失败: %w", err)
		}
	}
	return m.v1SetCPUQuota(instanceID, cpuPercent)
}

// v1SetCPUQuota 写入 cpu.cfs_quota_us / cpu.cfs_period_us。
//
// 顺序有讲究：**先写 period 再写 quota**。反过来时，若新 period 比旧的小，
// 旧 quota 在新 period 下会短暂对应一个更高的上限，中间那一刻实例是超额的。
func (m *Manager) v1SetCPUQuota(instanceID string, cpuPercent int) error {
	mnt := m.mounts["cpu"]
	if mnt == "" {
		return nil
	}
	dir := filepath.Join(mnt, v1GroupName, sanitize(instanceID))

	if err := os.WriteFile(filepath.Join(dir, "cpu.cfs_period_us"),
		[]byte(strconv.Itoa(cpuPeriodUsec)), 0o644); err != nil {
		return fmt.Errorf("设置 CPU period 失败: %w", err)
	}
	quota := v1Unlimited
	if cpuPercent > 0 {
		quota = cpuPercent * quotaUsecPerPercent
	}
	if err := os.WriteFile(filepath.Join(dir, "cpu.cfs_quota_us"),
		[]byte(strconv.Itoa(quota)), 0o644); err != nil {
		return fmt.Errorf("设置 CPU 配额失败: %w", err)
	}
	return nil
}

// v1SetMemoryLimit 写入 memory.limit_in_bytes。
// memory 控制器不可用时静默跳过（与 v2 的行为保持一致）。
func (m *Manager) v1SetMemoryLimit(instanceID string, limitBytes int64) error {
	mnt := m.mounts["memory"]
	if mnt == "" {
		return nil
	}
	dir := filepath.Join(mnt, v1GroupName, sanitize(instanceID))
	if _, err := os.Stat(filepath.Join(dir, "memory.limit_in_bytes")); err != nil {
		return nil
	}
	val := strconv.Itoa(v1Unlimited)
	if limitBytes > 0 {
		// 向上取整到页边界：非对齐值容易被内核拒绝或产生反直觉的结果
		if r := limitBytes % pageSize; r != 0 {
			limitBytes += pageSize - r
		}
		val = strconv.FormatInt(limitBytes, 10)
	}
	if err := os.WriteFile(filepath.Join(dir, "memory.limit_in_bytes"), []byte(val), 0o644); err != nil {
		return fmt.Errorf("设置内存上限失败: %w", err)
	}
	return nil
}

// v1Stats 读取 v1 的 CPU 统计并**换算成微秒**（与 v2 的 Stats 语义保持一致）。
//
// usage 来自 cpuacct/cpuacct.usage（纳秒）；
// throttled 来自 cpu/cpu.stat 的 throttled_time（纳秒）。
func (m *Manager) v1Stats(instanceID string) (usageUsec, throttledUsec uint64, err error) {
	name := sanitize(instanceID)

	usageNs, usageErr := m.readUint64File(filepath.Join(m.mounts["cpuacct"], v1GroupName, name, "cpuacct.usage"))
	if usageErr != nil && m.mounts["cpuacct"] == "" {
		// cpuacct 缺失时退回 cpu 层级（合并挂载的机器上两者是同一个目录）
		usageNs, _ = m.readUint64File(filepath.Join(m.mounts["cpu"], v1GroupName, name, "cpuacct.usage"))
	}

	throttledNs, _ := m.readThrottledTime(filepath.Join(m.mounts["cpu"], v1GroupName, name, "cpu.stat"))

	if m.mounts["cpuacct"] == "" && m.mounts["cpu"] == "" {
		return 0, 0, fmt.Errorf("cgroup v1 未挂载 cpu 相关控制器")
	}
	return usageNs / 1000, throttledNs / 1000, nil
}

// readUint64File 读取一个只含整数的 cgroup 文件。
func (m *Manager) readUint64File(path string) (uint64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
}

// readThrottledTime 从 v1 的 cpu.stat 里取 throttled_time（纳秒）。
func (m *Manager) readThrottledTime(path string) (uint64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && f[0] == "throttled_time" {
			return strconv.ParseUint(f[1], 10, 64)
		}
	}
	return 0, nil
}

// v1Remove 删除实例在各层级的 cgroup 目录。
func (m *Manager) v1Remove(instanceID string) error {
	var firstErr error
	for _, d := range m.v1Dirs(instanceID) {
		if err := os.Remove(d); err != nil && !os.IsNotExist(err) {
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}
