package cgroup

import (
	"os"
	"path/filepath"
	"testing"
)

// writeFake 在临时根下造一个假 cgroup 目录。
func writeFake(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("建目录失败: %v", err)
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatalf("写 %s 失败: %v", name, err)
		}
	}
}

const testCID = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// 场景：systemd 驱动的容器 cgroup 目录（v2 实测形态）能被按容器短 ID 找到。
func TestFindDockerCgroupSystemdLayout(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "system.slice", "docker-"+testCID[:12]+".scope")
	writeFake(t, dir, map[string]string{"cpu.stat": "usage_usec 100\n"})

	restore := SetBaseForTest(root)
	defer restore()

	got := FindDockerCgroup("atl-beta01")
	// atl-beta01 与容器 ID 无关，所以这里应当找不到（证明匹配是真在比对前缀）
	if len(got) != 0 {
		t.Fatalf("按无关名字不应命中，得到 %v", got)
	}
	got = FindDockerCgroup(testCID[:12])
	if len(got) != 1 || got[0] != dir {
		t.Fatalf("按容器短 ID 应命中 %s，得到 %v", dir, got)
	}
	// 完整 ID 也要能命中（真实调用方传的是 Inspect 拿到的完整 ID，
	// 而目录名可能只是短 ID）
	if got = FindDockerCgroup(testCID); len(got) != 1 || got[0] != dir {
		t.Fatalf("按完整容器 ID 应命中 %s，得到 %v", dir, got)
	}
}

// 场景：目录名是完整 ID（cgroupfs driver），按短 ID 查也要命中。
func TestFindDockerCgroupFullIDDirWithShortKey(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "docker", testCID)
	writeFake(t, dir, map[string]string{"cpuacct.usage": "5\n"})

	restore := SetBaseForTest(root)
	defer restore()

	// 短 ID、中等长度、完整 ID 三种调用方都可能传，都要命中同一个目录
	for _, key := range []string{testCID[:12], testCID[:16], testCID} {
		got := FindDockerCgroup(key)
		if len(got) != 1 || got[0] != dir {
			t.Fatalf("按 %q 应命中 %s，得到 %v", key, dir, got)
		}
	}
}

// 场景：cgroupfs 驱动的容器 cgroup 目录（v1/v2 的老形态）。
func TestFindDockerCgroupCgroupfsLayout(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "docker", testCID)
	writeFake(t, dir, map[string]string{"cpuacct.usage": "5\n"})

	restore := SetBaseForTest(root)
	defer restore()

	got := FindDockerCgroup(testCID[:12])
	if len(got) != 1 || got[0] != dir {
		t.Fatalf("应命中 cgroupfs 布局 %s，得到 %v", dir, got)
	}
}

// 场景：cgroup v2 的 CPU 读取（usage_usec 微秒 → 纳秒）。
func TestReadCPUStatV2ConvertsUsecToNs(t *testing.T) {
	dir := t.TempDir()
	writeFake(t, dir, map[string]string{
		"cpu.stat": "usage_usec 1234567\nuser_usec 1000000\nsystem_usec 234567\nnr_periods 42\n",
	})
	got, err := ReadCPUStat(dir)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if want := uint64(1234567) * 1000; got != want {
		t.Fatalf("微秒应换算成纳秒：want %d got %d", want, got)
	}
}

// 场景：cgroup v1 的 CPU 读取（cpuacct.usage 本身就是纳秒，不能乘 1000）。
func TestReadCPUStatV1KeepsNanoseconds(t *testing.T) {
	dir := t.TempDir()
	writeFake(t, dir, map[string]string{"cpuacct.usage": "987654321\n"})
	got, err := ReadCPUStat(dir)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if got != 987654321 {
		t.Fatalf("v1 的 cpuacct.usage 已是纳秒，不应再换算：got %d", got)
	}
}

// 场景：cgroup v2 内存 = memory.current - inactive_file（与 docker stats 口径一致）。
func TestReadMemoryStatV2WorkingSet(t *testing.T) {
	dir := t.TempDir()
	writeFake(t, dir, map[string]string{
		"memory.current": "1017204736\n",
		"memory.stat":    "anon 900000000\nfile 100000000\ninactive_file 50331648\nactive_file 10000000\n",
		"memory.max":     "1610612736\n",
	})
	used, limit, err := ReadMemoryStat(dir)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if want := uint64(1017204736 - 50331648); used != want {
		t.Fatalf("应扣掉不活跃文件缓存：want %d got %d", want, used)
	}
	if limit != 1610612736 {
		t.Fatalf("上限应为 memory.max：got %d", limit)
	}
}

// 场景：memory.max = max（不限制）与 v1 的 -1 都要报成"无上限"。
func TestReadMemoryStatUnlimited(t *testing.T) {
	dirV2 := t.TempDir()
	writeFake(t, dirV2, map[string]string{
		"memory.current": "1024\n",
		"memory.max":     "max\n",
	})
	if used, limit, err := ReadMemoryStat(dirV2); err != nil || used != 1024 || limit != 0 {
		t.Fatalf("v2 不限制时上限应为 0：used=%d limit=%d err=%v", used, limit, err)
	}

	dirV1 := t.TempDir()
	writeFake(t, dirV1, map[string]string{
		"memory.stat":           "total_rss 2048\ncache 999999\n",
		"memory.limit_in_bytes": "-1\n",
	})
	used, limit, err := ReadMemoryStat(dirV1)
	if err != nil || used != 2048 || limit != 0 {
		t.Fatalf("v1 应取 total_rss 且上限为 0：used=%d limit=%d err=%v", used, limit, err)
	}
}

// 场景：文件缺失时必须报错（让上层回退 docker stats），而不是返回 0。
func TestReadCPUStatMissingFile(t *testing.T) {
	if _, err := ReadCPUStat(t.TempDir()); err == nil {
		t.Fatal("文件不存在时应返回错误")
	}
}

// 场景：cgroup.procs 解析（列出容器内所有进程，供线程数统计用）。
func TestReadSubtree(t *testing.T) {
	dir := t.TempDir()
	writeFake(t, dir, map[string]string{"cgroup.procs": "123\n456\n789\n"})
	got := ReadSubtree(dir)
	if len(got) != 3 || got[0] != 123 || got[2] != 789 {
		t.Fatalf("应解析出 3 个 PID，得到 %v", got)
	}
	if ReadSubtree("") != nil {
		t.Fatal("空目录应返回 nil")
	}
}
