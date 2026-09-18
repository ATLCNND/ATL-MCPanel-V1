package cgroup

import (
	"os"
	"path/filepath"
	"testing"
)

// 版本选择逻辑：有 cgroup.controllers 就走 v2，否则回落 v1。
//
// 为什么要把这条路走通：内测节点是 CentOS 7（内核 3.10，没有 v2），
// 而这些机器上"超开几个实例"必须靠 v1 的 memory/cpu 限额兜底 ——
// 选择逻辑一旦出错，配额就会静默失效（只告警、不报错），
// 直到某个实例把整机内存吃光才暴露。
//
// 这两个用例都用临时目录替换 /sys 与 /proc/mounts，**不碰真实系统**。
func TestInitSelectsV2WhenAvailable(t *testing.T) {
	base := t.TempDir()
	if err := os.WriteFile(filepath.Join(base, "cgroup.controllers"), []byte("cpu memory pids\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	oldBase := cgroupV2Base
	cgroupV2Base = base
	defer func() { cgroupV2Base = oldBase }()

	m := New(filepath.Join(base, "atlmcpanel"))
	m.Init()

	if !m.Enabled() {
		t.Fatalf("应启用，实际禁用了：%s", m.Reason())
	}
	if m.Version() != cgV2 {
		t.Errorf("应选 v2，实际 version=%d", m.Version())
	}
}

func TestInitFallsBackToV1WhenNoV2(t *testing.T) {
	base := t.TempDir() // 没有 cgroup.controllers → 不是 v2

	mounts := t.TempDir()
	cpuMnt := filepath.Join(mounts, "cpu,cpuacct")
	memMnt := filepath.Join(mounts, "memory")
	if err := os.MkdirAll(cpuMnt, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(memMnt, 0o755); err != nil {
		t.Fatal(err)
	}
	fake := filepath.Join(mounts, "mounts")
	content := "cgroup " + cpuMnt + " cgroup rw,cpu,cpuacct 0 0\n" +
		"cgroup " + memMnt + " cgroup rw,memory 0 0\n"
	if err := os.WriteFile(fake, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	oldBase, oldProc := cgroupV2Base, procMounts
	cgroupV2Base = base
	procMounts = fake
	defer func() { cgroupV2Base, procMounts = oldBase, oldProc }()

	m := New("") // 这个用例里 v1 路径由探测结果决定
	m.Init()

	if !m.Enabled() {
		t.Fatalf("v2 不可用时应回落到 v1，实际禁用了：%s", m.Reason())
	}
	if m.Version() != cgV1 {
		t.Errorf("应选 v1，实际 version=%d", m.Version())
	}
	if m.RootPath() != filepath.Join(cpuMnt, v1GroupName) {
		t.Errorf("v1 分组根目录 = %q，期望 %q", m.RootPath(), filepath.Join(cpuMnt, v1GroupName))
	}
	// initV1 应在各层级下建好分组目录
	for _, mnt := range []string{cpuMnt, memMnt} {
		if st, err := os.Stat(filepath.Join(mnt, v1GroupName)); err != nil || !st.IsDir() {
			t.Errorf("未创建分组目录 %s: %v", filepath.Join(mnt, v1GroupName), err)
		}
	}
}

// 两者都没有时应干净地禁用，并给出可操作的原因（而不是静默无限制）。
func TestInitDisablesWhenNeitherAvailable(t *testing.T) {
	base := t.TempDir()
	mounts := t.TempDir()
	fake := filepath.Join(mounts, "mounts")
	if err := os.WriteFile(fake, []byte("sysfs /sys sysfs rw 0 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	oldBase, oldProc := cgroupV2Base, procMounts
	cgroupV2Base = base
	procMounts = fake
	defer func() { cgroupV2Base, procMounts = oldBase, oldProc }()

	m := New("")
	m.Init()

	if m.Enabled() {
		t.Error("两者都不可用时不应启用")
	}
	if m.Version() != cgDisabled {
		t.Errorf("version 应为 0，实际 %d", m.Version())
	}
	if m.Reason() == "" {
		t.Error("禁用时必须给出原因，否则日志里只说'未启用'，排查无从下手")
	}
}
