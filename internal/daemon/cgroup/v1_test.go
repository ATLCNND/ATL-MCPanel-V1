package cgroup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeV1 搭一个假的 cgroup v1 目录树（模拟 CentOS 7 的"cpu 与 cpuacct 合并挂载"），
// 返回一个已按 v1 启用的 Manager。
//
// 为什么用假树而不是真 /sys：真环境里改 cgroup 会影响整机，
// 而且 CI/开发机上未必有 v1。限额的**换算与写入**是纯逻辑，假树足以覆盖；
// 真机验证另有一步（见 docs/HANDOFF 里的 CentOS 测试）。
func fakeV1(t *testing.T) (*Manager, map[string]string) {
	t.Helper()
	base := t.TempDir()
	cpuMnt := filepath.Join(base, "cpu,cpuacct")
	memMnt := filepath.Join(base, "memory")

	// 控制器文件：探测与写入都依赖它们存在
	for _, d := range []string{
		filepath.Join(cpuMnt, v1GroupName, "i1"),
		filepath.Join(memMnt, v1GroupName, "i1"),
	} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// 控制器的"接口文件"放在挂载点根部：探测逻辑按它们判断控制器可用性
	for f, v := range map[string]string{
		filepath.Join(cpuMnt, "cpu.cfs_quota_us"):      "-1",
		filepath.Join(memMnt, "memory.limit_in_bytes"): "9223372036854771712",
	} {
		if err := os.WriteFile(f, []byte(v), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// 每个实例目录里也要有这些文件（写限额写的是实例目录下的同名文件）
	for f, v := range map[string]string{
		filepath.Join(cpuMnt, v1GroupName, "i1", "cpu.cfs_quota_us"):      "-1",
		filepath.Join(cpuMnt, v1GroupName, "i1", "cpu.cfs_period_us"):     "100000",
		filepath.Join(memMnt, v1GroupName, "i1", "memory.limit_in_bytes"): "9223372036854771712",
	} {
		if err := os.WriteFile(f, []byte(v), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	m := &Manager{
		root:    DefaultRoot,
		enabled: true,
		version: cgV1,
		mounts: map[string]string{
			"cpu":     cpuMnt,
			"cpuacct": cpuMnt, // CentOS 7 就是合并挂载
			"memory":  memMnt,
		},
	}
	return m, map[string]string{"cpu": cpuMnt, "memory": memMnt}
}

// 合并挂载（cpu 与 cpuacct 同一个挂载点）时，实例目录不能被算两次。
func TestV1DirsDeduplicatesCombinedMount(t *testing.T) {
	m, _ := fakeV1(t)
	dirs := m.v1Dirs("i1")
	if len(dirs) != 2 {
		t.Fatalf("应有 cpu 与 memory 两个目录，实际 %d 个: %v", len(dirs), dirs)
	}
	if dirs[0] == dirs[1] {
		t.Errorf("目录重复：%v", dirs)
	}
}

// 关键：CPU 配额换算。100% = 1 核 = 100000us/100ms，0 或负数 = -1（不限制）。
func TestV1CPUQuotaFormat(t *testing.T) {
	m, mnts := fakeV1(t)
	cpuDir := filepath.Join(mnts["cpu"], v1GroupName, "i1")

	cases := []struct {
		percent int
		want    string
	}{
		{100, "100000"},
		{200, "200000"},
		{50, "50000"},
		{0, "-1"},
		{-5, "-1"},
	}
	for _, c := range cases {
		if err := m.v1SetCPUQuota("i1", c.percent); err != nil {
			t.Fatalf("percent=%d 写配额失败: %v", c.percent, err)
		}
		got, err := os.ReadFile(filepath.Join(cpuDir, "cpu.cfs_quota_us"))
		if err != nil {
			t.Fatal(err)
		}
		if strings.TrimSpace(string(got)) != c.want {
			t.Errorf("percent=%d: quota = %q，期望 %q", c.percent, strings.TrimSpace(string(got)), c.want)
		}
		period, _ := os.ReadFile(filepath.Join(cpuDir, "cpu.cfs_period_us"))
		if strings.TrimSpace(string(period)) != "100000" {
			t.Errorf("percent=%d: period = %q，期望 100000", c.percent, strings.TrimSpace(string(period)))
		}
	}
}

// 关键：内存上限。v1 用 -1 表示不限制；正数要向上取整到页边界。
func TestV1MemoryLimit(t *testing.T) {
	m, mnts := fakeV1(t)
	f := filepath.Join(mnts["memory"], v1GroupName, "i1", "memory.limit_in_bytes")

	// 不限制
	if err := m.v1SetMemoryLimit("i1", 0); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(f); strings.TrimSpace(string(b)) != "-1" {
		t.Errorf("limit=0 应写 -1，实际 %q", strings.TrimSpace(string(b)))
	}
	// 1G 正好是页对齐的，应原样写入
	if err := m.v1SetMemoryLimit("i1", 1<<30); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(f); strings.TrimSpace(string(b)) != "1073741824" {
		t.Errorf("1G 应写 1073741824，实际 %q", strings.TrimSpace(string(b)))
	}
	// 非页对齐要向上取整（1000 → 4096）
	if err := m.v1SetMemoryLimit("i1", 1000); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(f); strings.TrimSpace(string(b)) != "4096" {
		t.Errorf("1000 应向上取整到 4096，实际 %q", strings.TrimSpace(string(b)))
	}
}

// 关键单位换算：v1 的 cpuacct.usage 与 throttled_time 都是**纳秒**，
// 而 Stats 的对外语义是**微秒**（与 v2 一致）。搞错就是 1000 倍误差。
func TestV1StatsConvertsNanosecondsToMicroseconds(t *testing.T) {
	m, mnts := fakeV1(t)
	cpuDir := filepath.Join(mnts["cpu"], v1GroupName, "i1")

	// 12.345678 秒 = 12345678000 纳秒 → 应为 12345678 微秒
	if err := os.WriteFile(filepath.Join(cpuDir, "cpuacct.usage"), []byte("12345678000\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 被限流 0.345678 秒 → 345678 微秒
	if err := os.WriteFile(filepath.Join(cpuDir, "cpu.stat"),
		[]byte("nr_periods 900\nnr_throttled 12\nthrottled_time 345678000\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	usage, throttled, err := m.Stats("i1")
	if err != nil {
		t.Fatalf("Stats 失败: %v", err)
	}
	if usage != 12345678 {
		t.Errorf("usage 应为 12345678 微秒，实际 %d（差 1000 倍说明没做纳秒换算）", usage)
	}
	if throttled != 345678 {
		t.Errorf("throttled 应为 345678 微秒，实际 %d（差 1000 倍说明没做纳秒换算）", throttled)
	}
}

// 关键：v1 是**多层级**的，进程必须每个层级各写一次，
// 只写一个会导致另一个控制器对该进程完全不生效（而且不报错）。
func TestV1AssignWritesAllHierarchies(t *testing.T) {
	m, mnts := fakeV1(t)
	n, err := m.assign("i1", []int{4242})
	if err != nil {
		t.Fatalf("assign 失败: %v", err)
	}
	if n != 1 {
		t.Errorf("应移动 1 个进程，实际 %d", n)
	}
	for name, mnt := range map[string]string{"cpu": mnts["cpu"], "memory": mnts["memory"]} {
		p := filepath.Join(mnt, v1GroupName, "i1", "cgroup.procs")
		b, err := os.ReadFile(p)
		if err != nil {
			t.Errorf("%s 层级没有写入 PID（多层级漏写会让该控制器失效）: %v", name, err)
			continue
		}
		if strings.TrimSpace(string(b)) != "4242" {
			t.Errorf("%s 层级的 PID 应为 4242，实际 %q", name, strings.TrimSpace(string(b)))
		}
	}
}

// /proc/mounts 解析：要能处理合并挂载（cpu,cpuacct 同一个挂载点）。
func TestDetectV1MountsParsesCombinedMount(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "mounts")
	content := `sysfs /sys sysfs rw,nosuid 0 0
proc /proc proc rw,nosuid 0 0
cgroup /sys/fs/cgroup/systemd cgroup rw,xattr,name=systemd 0 0
cgroup /sys/fs/cgroup/cpu,cpuacct cgroup rw,nosuid,cpu,cpuacct 0 0
cgroup /sys/fs/cgroup/memory cgroup rw,nosuid,memory 0 0
`
	if err := os.WriteFile(fake, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	old := procMounts
	procMounts = fake
	defer func() { procMounts = old }()

	mounts := detectV1Mounts()
	if mounts["cpu"] != "/sys/fs/cgroup/cpu,cpuacct" {
		t.Errorf("cpu 挂载点 = %q，期望 /sys/fs/cgroup/cpu,cpuacct", mounts["cpu"])
	}
	if mounts["cpuacct"] != "/sys/fs/cgroup/cpu,cpuacct" {
		t.Errorf("cpuacct 挂载点 = %q，期望与 cpu 相同", mounts["cpuacct"])
	}
	if mounts["memory"] != "/sys/fs/cgroup/memory" {
		t.Errorf("memory 挂载点 = %q", mounts["memory"])
	}
}

// 分开挂载的发行版（Debian 系）也要能正确识别。
func TestDetectV1MountsSeparateMounts(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "mounts")
	content := `cgroup /sys/fs/cgroup/cpu cgroup rw,cpu 0 0
cgroup /sys/fs/cgroup/cpuacct cgroup rw,cpuacct 0 0
cgroup /sys/fs/cgroup/memory cgroup rw,memory 0 0
`
	if err := os.WriteFile(fake, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	old := procMounts
	procMounts = fake
	defer func() { procMounts = old }()

	mounts := detectV1Mounts()
	if mounts["cpu"] != "/sys/fs/cgroup/cpu" || mounts["cpuacct"] != "/sys/fs/cgroup/cpuacct" {
		t.Errorf("分开挂载识别错误: %v", mounts)
	}
}

// 没有 cgroup 挂载时应返回空表（调用方据此判定 v1 不可用）。
func TestDetectV1MountsNone(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "mounts")
	if err := os.WriteFile(fake, []byte("sysfs /sys sysfs rw 0 0\nproc /proc proc rw 0 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := procMounts
	procMounts = fake
	defer func() { procMounts = old }()

	if mounts := detectV1Mounts(); mounts["cpu"] != "" {
		t.Errorf("无 cgroup 挂载时不应识别出控制器，实际 %v", mounts)
	}
}

// 转义路径还原（/proc/mounts 用 \040 表示空格）
func TestUnescapeMountPath(t *testing.T) {
	cases := map[string]string{
		"/sys/fs/cgroup/cpu":       "/sys/fs/cgroup/cpu",
		"/sys/fs/cgroup/cpu\\040x": "/sys/fs/cgroup/cpu x",
		"/a\\134b":                 "/a\\b",
	}
	for in, want := range cases {
		if got := unescapeMountPath(in); got != want {
			t.Errorf("unescapeMountPath(%q) = %q，期望 %q", in, got, want)
		}
	}
}

// v1 下 memory 控制器缺失时应静默跳过（不能因此让实例起不来）。
func TestV1MemoryLimitSkippedWithoutMemoryController(t *testing.T) {
	m, _ := fakeV1(t)
	m.mounts["memory"] = "" // 模拟没有 memory 控制器
	if err := m.v1SetMemoryLimit("i1", 1<<30); err != nil {
		t.Errorf("memory 控制器缺失时应静默跳过，实际报错: %v", err)
	}
}
