package cgroup

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// initV2 尝试按 cgroup v2 初始化。成功返回 true。
func (m *Manager) initV2() bool {
	// 1. cgroup v2 判定：只有 v2 才有 cgroup.controllers 文件
	if _, err := os.Stat(filepath.Join(cgroupV2Base, "cgroup.controllers")); err != nil {
		return false
	}
	// 2. 需要 cpu 控制器
	if !m.controllerAvailable("cpu") {
		m.reason = "cgroup v2 未提供 cpu 控制器"
		return false
	}
	// 3. 需要写权限（Daemon 通常以 root 运行）
	if err := os.MkdirAll(m.root, 0o755); err != nil {
		m.reason = "创建 cgroup 根目录失败: " + err.Error()
		return false
	}
	// 4. 在根 cgroup 启用控制器，子 cgroup 才能设置 cpu.max
	if err := os.WriteFile(filepath.Join(m.root, "cgroup.subtree_control"), []byte("+cpu +memory"), 0o644); err != nil {
		// 只启用 cpu 也可以工作；内存控制器不可用时忽略
		if err2 := os.WriteFile(filepath.Join(m.root, "cgroup.subtree_control"), []byte("+cpu"), 0o644); err2 != nil {
			m.reason = "启用 cgroup 控制器失败: " + err2.Error()
			return false
		}
	}
	m.enabled = true
	m.version = cgV2
	m.reason = ""
	return true
}

// v2Apply 创建实例 cgroup 并写入 cpu.max。
func (m *Manager) v2Apply(instanceID string, cpuPercent int) error {
	dir := m.dir(instanceID)
	// 若旧 cgroup 已无进程，先删除再重建：cgroup v2 的 cpu.stat 是累计值，
	// 复用同一目录会让"本次运行"的 CPU 统计叠加历史数据。
	// 目录中仍有进程时 os.Remove 会失败（EBUSY），此时保持不动。
	_ = os.Remove(dir)

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("创建 cgroup 失败: %w", err)
	}
	return m.v2SetCPUQuota(instanceID, cpuPercent)
}

// v2SetCPUQuota 写入 cpu.max。
func (m *Manager) v2SetCPUQuota(instanceID string, cpuPercent int) error {
	val := "max " + strconv.Itoa(cpuPeriodUsec)
	if cpuPercent > 0 {
		val = fmt.Sprintf("%d %d", cpuPercent*quotaUsecPerPercent, cpuPeriodUsec)
	}
	if err := os.WriteFile(filepath.Join(m.dir(instanceID), "cpu.max"), []byte(val), 0o644); err != nil {
		return fmt.Errorf("设置 CPU 配额失败: %w", err)
	}
	return nil
}

// v2SetMemoryLimit 写入 memory.max。控制器不可用时静默跳过。
func (m *Manager) v2SetMemoryLimit(instanceID string, limitBytes int64) error {
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

// v2Stats 解析 cpu.stat（v2 的字段就是微秒）。
func (m *Manager) v2Stats(instanceID string) (usageUsec, throttledUsec uint64, err error) {
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

func (m *Manager) v2Remove(instanceID string) error {
	// 目录内若仍有进程，删除会失败；交由调用方决定是否重试
	if err := os.Remove(m.dir(instanceID)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
