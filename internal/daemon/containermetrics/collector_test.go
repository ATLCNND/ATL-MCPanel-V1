package containermetrics

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ATLCNND/ATL-MCPanel/internal/daemon/cgroup"
	"github.com/ATLCNND/ATL-MCPanel/internal/daemon/container"
)

// fakeSource 可控的数据源替身：单测不需要真容器、真进程，甚至不需要 root。
type fakeSource struct {
	kind  string
	id    string
	snap  Snapshot
	err   error
	calls int
}

// testCIDLong 一个 64 位十六进制的假容器 ID（与真实 docker 的长度一致）。
const testCIDLong = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// TestCIDShort 容器 ID 的短形式（docker 的 cgroup/hostname 都用它）。
const TestCIDShort = "0123456789ab"

func (f *fakeSource) Kind() string { return f.kind }
func (f *fakeSource) ID() string   { return f.id }
func (f *fakeSource) Snapshot(context.Context) (Snapshot, error) {
	f.calls++
	if f.err != nil {
		return Snapshot{}, f.err
	}
	return f.snap, nil
}

// withFakeClock 把 nowFn 换成可控时钟，返回推进函数（恢复由 t.Cleanup 负责）。
//
// 为什么必须注入时钟：CPU 百分比 = Δ计数 / Δ时间，用真实时钟就得 sleep，
// 单测会既慢又飘。这里让"采样间隔恰好 2 秒"成为确定的输入。
func withFakeClock(t *testing.T) (advance func(time.Duration), base time.Time) {
	t.Helper()
	base = time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	cur := base
	old := nowFn
	nowFn = func() time.Time { return cur }
	t.Cleanup(func() { nowFn = old })
	return func(d time.Duration) { cur = cur.Add(d) }, base
}

func TestSampleFirstRoundHasNoCPU(t *testing.T) {
	adv, _ := withFakeClock(t)
	var c Collector
	src := &fakeSource{kind: "native", id: "4242", snap: Snapshot{CPUCount: 5e9, MemUsed: 1024, Threads: 12}}

	snap, pct, ok := c.Sample(context.Background(), src)
	if ok {
		t.Fatalf("首次采样不应给出 CPU 百分比，却得到 %v", pct)
	}
	if pct != 0 {
		t.Fatalf("首次采样 CPU 应为 0，得到 %v", pct)
	}
	// 内存与线程是即时值，第一轮就必须有
	if snap.MemUsed != 1024 || snap.Threads != 12 {
		t.Fatalf("首次采样应带上内存与线程数，得到 mem=%d threads=%d", snap.MemUsed, snap.Threads)
	}
	src.snap = Snapshot{CPUCount: 5e9 + 2e9, MemUsed: 2048, Threads: 13}
	adv(2 * time.Second)
	snap, pct, ok = c.Sample(context.Background(), src)
	if !ok {
		t.Fatal("第二次采样应能算出 CPU 百分比")
	}
	// Δ计数 2e9 纳秒 = 2 秒 CPU 时间，窗口 2 秒 => 100%（1 核跑满）
	if math.Abs(pct-100) > 0.5 {
		t.Fatalf("2 秒内用掉 2 秒 CPU 时间应为 100%%，得到 %v", pct)
	}
	if snap.MemUsed != 2048 {
		t.Fatalf("内存应为 2048，得到 %d", snap.MemUsed)
	}
}

// 场景：数据源切换（native → container）。
//
// 换源后上一次的计数来自另一个对象（进程 tick vs 容器 cgroup），两者相减
// 毫无意义 —— 必须跳过一轮，而不是给出一个看着合理的错数。
func TestSampleSkipsRoundOnSourceKindSwitch(t *testing.T) {
	adv, _ := withFakeClock(t)
	var c Collector

	native := &fakeSource{kind: "native", id: "4242", snap: Snapshot{CPUCount: 5e9, MemUsed: 1024}}
	if _, _, ok := c.Sample(context.Background(), native); ok {
		t.Fatal("首次采样不应给出百分比")
	}
	adv(2 * time.Second)
	native.snap = Snapshot{CPUCount: 5e9 + 1e9, MemUsed: 1024}
	if _, pct, ok := c.Sample(context.Background(), native); !ok || math.Abs(pct-50) > 0.5 {
		t.Fatalf("native 第二次采样应约 50%%，得到 pct=%v ok=%v", pct, ok)
	}

	// 切换成容器来源，计数"看起来"是连续的（容器 cgroup 计数恰好更大），
	// 但仍然必须跳过：跨来源的差值不可比。
	adv(2 * time.Second)
	cont := &fakeSource{kind: "container", id: "atl-alpha1", snap: Snapshot{CPUCount: 6e9, MemUsed: 8 << 20}}
	snap, pct, ok := c.Sample(context.Background(), cont)
	if ok {
		t.Fatalf("数据源切换那一轮必须跳过 CPU 计算，却得到 %v", pct)
	}
	// 但这一轮的内存/线程数照常给（它们是即时值，不依赖基线）
	if snap.MemUsed != 8<<20 {
		t.Fatalf("切换轮仍应返回内存，得到 %d", snap.MemUsed)
	}

	adv(2 * time.Second)
	cont.snap = Snapshot{CPUCount: 6e9 + 1e9, MemUsed: 8 << 20}
	if _, pct, ok := c.Sample(context.Background(), cont); !ok || math.Abs(pct-50) > 0.5 {
		t.Fatalf("切换后的下一轮应恢复正常计算（约 50%%），得到 pct=%v ok=%v", pct, ok)
	}
}

// 场景：容器重建 —— 同一个 cgroup 目录被复用、累计计数归零。
func TestSampleSkipsRoundOnCounterRegression(t *testing.T) {
	adv, _ := withFakeClock(t)
	var c Collector
	src := &fakeSource{kind: "container", id: "atl-beta01", snap: Snapshot{CPUCount: 900e9, MemUsed: 1 << 30}}

	if _, _, ok := c.Sample(context.Background(), src); ok {
		t.Fatal("首次采样不应给出百分比")
	}
	adv(2 * time.Second)
	src.snap = Snapshot{CPUCount: 900e9 + 1e9, MemUsed: 1 << 30}
	if _, pct, ok := c.Sample(context.Background(), src); !ok || math.Abs(pct-50) > 0.5 {
		t.Fatalf("正常推进应约 50%%，得到 pct=%v ok=%v", pct, ok)
	}

	// 容器在同一个 cgroup 目录里重建：计数从 0 重新开始。
	// 不检测回退的话这里会算出 ((0 - 900e9) 下溢) 巨大的正数。
	adv(2 * time.Second)
	src.snap = Snapshot{CPUCount: 1000, MemUsed: 64 << 20}
	snap, pct, ok := c.Sample(context.Background(), src)
	if ok {
		t.Fatalf("计数器回退必须跳过，却得到 pct=%v", pct)
	}
	if pct != 0 {
		t.Fatalf("计数器回退时 CPU 必须为 0（而不是负值/跳变），得到 %v", pct)
	}
	if snap.MemUsed != 64<<20 {
		t.Fatalf("回退轮仍应返回内存，得到 %d", snap.MemUsed)
	}

	// 回退之后，以新计数为基线，下一轮恢复正常
	adv(2 * time.Second)
	src.snap = Snapshot{CPUCount: 1000 + 5e8, MemUsed: 64 << 20}
	if _, pct, ok := c.Sample(context.Background(), src); !ok || math.Abs(pct-25) > 0.5 {
		t.Fatalf("回退后下一轮应恢复（0.5 秒/2 秒 = 25%%），得到 pct=%v ok=%v", pct, ok)
	}
}

// 场景：进程重启（native 的 PID 变了）。
func TestSampleSkipsRoundOnPIDChange(t *testing.T) {
	adv, _ := withFakeClock(t)
	var c Collector
	a := &fakeSource{kind: "native", id: "100", snap: Snapshot{CPUCount: 10e9}}
	if _, _, ok := c.Sample(context.Background(), a); ok {
		t.Fatal("首次采样不应给出百分比")
	}
	adv(2 * time.Second)
	b := &fakeSource{kind: "native", id: "200", snap: Snapshot{CPUCount: 1e9}}
	if _, pct, ok := c.Sample(context.Background(), b); ok {
		t.Fatalf("PID 变化必须跳过，得到 pct=%v", pct)
	}
}

// 场景：同一毫秒内被问两次（面板「统计」页与详情页同时刷新）。
// Δ时间 ≈ 0 时不能输出天文数字。
func TestSampleRejectsZeroInterval(t *testing.T) {
	_, _ = withFakeClock(t)
	var c Collector
	src := &fakeSource{kind: "native", id: "1", snap: Snapshot{CPUCount: 1e9}}
	if _, _, ok := c.Sample(context.Background(), src); ok {
		t.Fatal("首次采样不应给出百分比")
	}
	src.snap = Snapshot{CPUCount: 2e9}
	if _, pct, ok := c.Sample(context.Background(), src); ok {
		t.Fatalf("采样间隔为 0 时不应给出百分比，得到 %v", pct)
	}
}

// 场景：docker stats 回退路径 —— 它自带百分比，不经过差值逻辑。
func TestSampleUsesDirectPercentFromFallback(t *testing.T) {
	_, _ = withFakeClock(t)
	var c Collector
	src := &fakeSource{kind: "container", id: "atl-x", snap: Snapshot{
		DirectPct: 22.31, HasDirectPct: true, MemUsed: 970 << 20, Threads: -1,
	}}
	snap, pct, ok := c.Sample(context.Background(), src)
	if !ok || math.Abs(pct-22.31) > 1e-9 {
		t.Fatalf("回退路径应直接采用 docker 给的百分比，得到 pct=%v ok=%v", pct, ok)
	}
	if snap.MemUsed != 970<<20 {
		t.Fatalf("回退路径应带上内存，得到 %d", snap.MemUsed)
	}
	// 连续两次都应给出同一数值（不依赖历史基线）
	if _, pct2, ok2 := c.Sample(context.Background(), src); !ok2 || math.Abs(pct2-22.31) > 1e-9 {
		t.Fatalf("回退路径第二轮仍应给出同一百分比，得到 pct=%v ok=%v", pct2, ok2)
	}
}

// 场景：读不到数据（容器没了 / 进程没了）时不能编数。
func TestSampleWithReadErrorReturnsNoCPU(t *testing.T) {
	_, _ = withFakeClock(t)
	var c Collector
	src := &fakeSource{kind: "container", id: "atl-gone", err: errors.New("读不到 cgroup")}
	snap, pct, ok := c.Sample(context.Background(), src)
	if ok || pct != 0 {
		t.Fatalf("读取失败时不应给出 CPU，得到 pct=%v ok=%v", pct, ok)
	}
	if snap != (Snapshot{}) {
		t.Fatalf("读取失败时应返回空快照，得到 %+v", snap)
	}
}

// 场景：native 来源的 /proc 读法（含单位换算）。
// 用当前进程的真实 /proc —— 只读，不需要任何特权。
func TestNativeSourceReadsRealProc(t *testing.T) {
	// 先烧掉一点 CPU：测试进程刚起来时 utime+stime 常常正好是 0，
	// 不烧的话这条断言会随机失败（实测 VM 上就是 0）
	burnCPU(30 * time.Millisecond)

	src := NativeSource{PID: os.Getpid()}
	snap, err := src.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("读取自身 /proc 失败: %v", err)
	}
	if snap.CPUCount == 0 {
		t.Fatal("自身进程的累计 CPU 时间不应为 0（已先烧掉 30ms CPU）")
	}
	// 单位必须是纳秒：30ms 的忙等至少要 >10ms 的 CPU 时间，
	// 若误把 tick 当纳秒（少乘 1e7）这里会拿到几千这个量级
	if snap.CPUCount < uint64(10*time.Millisecond) {
		t.Fatalf("CPU 计数单位应换算成纳秒，得到 %d", snap.CPUCount)
	}
	if snap.MemUsed == 0 {
		t.Fatal("自身进程的 RSS 不应为 0")
	}
	if snap.Threads <= 0 {
		t.Fatalf("自身进程的线程数应为正数，得到 %d", snap.Threads)
	}
	if src.Kind() != "native" || src.ID() == "" {
		t.Fatalf("native 来源标识异常: kind=%q id=%q", src.Kind(), src.ID())
	}
}

// burnCPU 忙等 d 时间，确保进程累计 CPU 时间非 0。
func burnCPU(d time.Duration) {
	deadline := time.Now().Add(d)
	x := 0
	for time.Now().Before(deadline) {
		x++
	}
	_ = x
}

// 场景：PID 为 0（实例没跑）时明确报错，而不是当成 `/proc/0` 去读。
func TestNativeSourceZeroPID(t *testing.T) {
	if _, err := (NativeSource{}).Snapshot(context.Background()); err == nil {
		t.Fatal("PID=0 应返回错误")
	}
}

// 场景：容器 cgroup 读取（v2 布局，用临时目录里的假 cgroup 文件）。
func TestContainerSourceReadsFakeCgroupV2(t *testing.T) {
	root := t.TempDir()
	cid := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	dir := filepath.Join(root, "system.slice", "docker-"+cid[:12]+".scope")
	mustMkdir(t, dir)
	mustWrite(t, filepath.Join(dir, "cpu.stat"), "usage_usec 1234567\nuser_usec 1000000\nsystem_usec 234567\n")
	// memory.current 含 50MB 文件缓存，其中 48MB 不活跃：working set 应把它扣掉
	mustWrite(t, filepath.Join(dir, "memory.current"), "104857600\n")
	mustWrite(t, filepath.Join(dir, "memory.stat"), "anon 50000000\ninactive_file 50331648\n")
	mustWrite(t, filepath.Join(dir, "memory.max"), "1610612736\n")
	// cgroup.procs 里的这两个 PID 在测试机上不一定属于任何进程，
	// 所以线程数用注入的替身返回（同时断言"遍历的是 cgroup 里的 PID"）
	mustWrite(t, filepath.Join(dir, "cgroup.procs"), "1\n2\n")

	restore := cgroup.SetBaseForTest(root)
	defer restore()
	var seen []int
	restoreThreads := SetThreadListerForTest(func(pid int) int {
		seen = append(seen, pid)
		if pid == 1 {
			return 30
		}
		return 2
	})
	defer restoreThreads()

	src := ContainerSource{InstanceID: "beta01", ContainerName: "atl-beta01", ContainerID: cid}
	snap, err := src.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("读取假 cgroup 失败: %v", err)
	}
	if snap.CPUCount != 1234567*1000 {
		t.Fatalf("usage_usec 应换算成纳秒（1234567 微秒 => 1234567000），得到 %d", snap.CPUCount)
	}
	if snap.MemUsed != 104857600-50331648 {
		t.Fatalf("内存应为 memory.current 减去 inactive_file（%d），得到 %d",
			104857600-50331648, snap.MemUsed)
	}
	if snap.MemTotal != 1610612736 {
		t.Fatalf("内存上限应为 memory.max，得到 %d", snap.MemTotal)
	}
	if snap.Threads != 32 {
		t.Fatalf("线程数应为 cgroup 内所有进程之和（30+2），得到 %d", snap.Threads)
	}
	if len(seen) != 2 || seen[0] != 1 || seen[1] != 2 {
		t.Fatalf("线程数统计应遍历 cgroup.procs 里的每个 PID，实际遍历了 %v", seen)
	}
}

// 场景：**生产路径**——调用方只给容器名，容器 ID 靠（带缓存的）docker inspect 解析。
//
// 这条是最关键的回归点：cgroup 目录名是 64 位容器 ID（这里目录名用短 ID 形态），
// 而容器名（atl-xxx）与容器 ID 毫无关系。第一版实现只按容器名查目录，
// 真机上永远查不到，于是悄悄退化成 docker stats（CPU 数值看着"差不多对"，
// 线程数却是 0）。findCgroup 必须两个方向的前缀都能比中。
func TestContainerSourceResolvesContainerIDForCgroup(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "system.slice", "docker-"+TestCIDShort+".scope")
	mustMkdir(t, dir)
	mustWrite(t, filepath.Join(dir, "cpu.stat"), "usage_usec 2000000\n")
	mustWrite(t, filepath.Join(dir, "memory.current"), "104857600\n")
	mustWrite(t, filepath.Join(dir, "memory.stat"), "inactive_file 1048576\n")
	mustWrite(t, filepath.Join(dir, "cgroup.procs"), "1\n")

	restore := cgroup.SetBaseForTest(root)
	defer restore()
	calls := 0
	restoreResolver := SetContainerIDResolverForTest(func(string, *container.Runtime) string {
		calls++
		return testCIDLong
	})
	defer restoreResolver()
	restoreThreads := SetThreadListerForTest(func(int) int { return 7 })
	defer restoreThreads()

	// 注意 ContainerID 为空、Runtime 非 nil —— 正是生产路径的形态
	src := ContainerSource{
		InstanceID:    "beta07",
		ContainerName: "atl-beta07",
		Runtime:       container.NewWithBin("/usr/bin/docker", "test-image"),
	}
	snap, err := src.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("应通过解析出的容器 ID 命中 cgroup，却失败: %v", err)
	}
	if snap.CPUCount != 2000000*1000 {
		t.Fatalf("应读到 cgroup 的 CPU（2000000 微秒 => 2000000000 纳秒），得到 %d", snap.CPUCount)
	}
	if snap.MemUsed != 104857600-1048576 {
		t.Fatalf("应读到 cgroup 的内存 working set，得到 %d", snap.MemUsed)
	}
	if snap.Threads != 7 {
		t.Fatalf("线程数应从 cgroup.procs 统计，得到 %d", snap.Threads)
	}
	if snap.HasDirectPct {
		t.Fatal("这条路径不该走 docker stats 回退")
	}
	// 第二次采样必须命中缓存，不能再问一次 docker（每次采样起一个 CLI 进程的开销）
	if _, err := src.Snapshot(context.Background()); err != nil {
		t.Fatalf("第二次采样失败: %v", err)
	}
	if calls != 1 {
		t.Fatalf("容器 ID 解析应被缓存（只问一次 docker），实际问了 %d 次", calls)
	}
}

// 场景：Source.ID() 必须在采集前后**稳定**。
//
// 这条是踩出来的：采集器用它判断"来源是否变化"，而它会在一轮里调用两次
// （Snapshot 之前与之后）。第一版实现的 Snapshot 里才去解析容器 ID，
// 于是"之前"读到空、"之后"读到容器 ID，**每一轮都被判成换了来源**，
// CPU 永远被跳过 —— 面板上就是"其它数字都对、cpu_percent 恒为 0"，
// 而且不报任何错。
func TestContainerSourceIDIsStableAcrossSnapshot(t *testing.T) {
	adv, _ := withFakeClock(t)
	root := t.TempDir()
	dir := filepath.Join(root, "system.slice", "docker-"+TestCIDShort+".scope")
	mustMkdir(t, dir)
	mustWrite(t, filepath.Join(dir, "cpu.stat"), "usage_usec 1000\n")
	mustWrite(t, filepath.Join(dir, "memory.current"), "4096\n")
	mustWrite(t, filepath.Join(dir, "memory.stat"), "inactive_file 0\n")

	restore := cgroup.SetBaseForTest(root)
	defer restore()
	restoreResolver := SetContainerIDResolverForTest(func(string, *container.Runtime) string {
		return testCIDLong
	})
	defer restoreResolver()

	src := ContainerSource{
		InstanceID:    "beta11",
		ContainerName: "atl-beta11",
		Runtime:       container.NewWithBin("/usr/bin/docker", "test-image"),
	}
	// 生产路径：构造时解析一次
	src.ContainerID = src.ResolveContainerID()
	before := src.ID()
	if before == "" {
		t.Fatal("容器 ID 应在构造后就可用（否则采集器每轮都会判成换了来源）")
	}
	if _, err := src.Snapshot(context.Background()); err != nil {
		t.Fatalf("Snapshot 失败: %v", err)
	}
	if after := src.ID(); after != before {
		t.Fatalf("Snapshot 前后 ID 必须一致：before=%q after=%q", before, after)
	}

	// 再验证采集器在连续两轮里确实算出了 CPU（而不是每轮都当"新来源"跳过）
	var col Collector
	if _, _, ok := col.Sample(context.Background(), src); ok {
		t.Fatal("第一轮是基线，不应给出百分比")
	}
	// 推进 2 秒并让累计计数增加 1 秒（= 50%）：
	mustWrite(t, filepath.Join(dir, "cpu.stat"), "usage_usec 1001000\n")
	adv(2 * time.Second)
	_, pct, ok := col.Sample(context.Background(), src)
	if !ok {
		t.Fatal("第二轮必须能算出百分比（同一来源、计数未回退）—— 恒被跳过正是这次修掉的缺陷")
	}
	if math.Abs(pct-50) > 0.5 {
		t.Fatalf("2 秒内用掉 1 秒 CPU 时间应为 50%%，得到 %v", pct)
	}
}

// 场景：cgroup v1 布局下的容器读取（cpuacct.usage 纳秒 + total_rss）。
func TestContainerSourceReadsFakeCgroupV1(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "memory", "docker", testCIDLong)
	mustMkdir(t, dir)
	// v1 的 cpuacct 与 memory 可能分在两个层级；cpuacct.usage 只在 cpu 层级有，
	// 但这里图省事放同一个目录：findCgroup 只要求"读得到 CPU 与内存"。
	mustWrite(t, filepath.Join(dir, "cpuacct.usage"), "987654321\n")
	mustWrite(t, filepath.Join(dir, "memory.stat"), "total_rss 402653184\ncache 12345\n")
	mustWrite(t, filepath.Join(dir, "memory.limit_in_bytes"), "1610612736\n")
	mustWrite(t, filepath.Join(dir, "cgroup.procs"), "7\n")

	restore := cgroup.SetBaseForTest(root)
	defer restore()
	restoreThreads := SetThreadListerForTest(func(int) int { return 11 })
	defer restoreThreads()

	// cgroupfs 布局的目录名就是完整 ID（没有 docker- 前缀），查找必须能命中
	src := ContainerSource{InstanceID: "beta09", ContainerName: "atl-beta09", ContainerID: testCIDLong}
	snap, err := src.Snapshot(context.Background())
	if err != nil {
		t.Fatalf("读取 v1 假 cgroup 失败: %v", err)
	}
	if snap.CPUCount != 987654321 {
		t.Fatalf("v1 的 cpuacct.usage 已是纳秒，不应换算：得到 %d", snap.CPUCount)
	}
	if snap.MemUsed != 402653184 {
		t.Fatalf("v1 应取 total_rss，得到 %d", snap.MemUsed)
	}
	if snap.MemTotal != 1610612736 {
		t.Fatalf("内存上限应为 memory.limit_in_bytes，得到 %d", snap.MemTotal)
	}
	if snap.Threads != 11 {
		t.Fatalf("线程数应为 11，得到 %d", snap.Threads)
	}
}

// 场景：容器 cgroup 读不到时必须回退 docker stats，且回退失败也不会 panic。
//
// 这里刻意不给 Runtime（nil）：预期得到"容器运行时不可用"的错误，
// 而不是静默返回一组零值 —— 零值会变成界面上的"CPU 0 / 内存 0"。
func TestContainerSourceWithoutCgroupAndRuntime(t *testing.T) {
	root := t.TempDir()
	restore := cgroup.SetBaseForTest(root)
	defer restore()

	src := ContainerSource{InstanceID: "beta02", ContainerName: "atl-beta02"}
	if _, err := src.Snapshot(context.Background()); err == nil {
		t.Fatal("既没有 cgroup 也没有 docker 时应返回错误")
	}
}

// mustMkdir / mustWrite 建假 cgroup 文件。
func mustMkdir(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("建目录失败: %v", err)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("写文件失败: %v", err)
	}
}

// 场景：docker stats 回退路径的百分比直接采用，且不需要任何基线。
// 用真实 Runtime（不起进程：Stats 才会起），只验类型与标签。
func TestContainerSourceLabels(t *testing.T) {
	restoreResolver := SetContainerIDResolverForTest(func(string, *container.Runtime) string {
		return testCIDLong
	})
	defer restoreResolver()

	src := ContainerSource{InstanceID: "beta03", ContainerName: "atl-beta03", Runtime: container.NewWithBin("/usr/bin/docker", "img")}
	if src.Kind() != "container" {
		t.Fatalf("Kind 应为 container，得到 %q", src.Kind())
	}
	// 拿得到容器 ID 时用它当来源标识（容器重建后 ID 必变，是可靠的判据）
	if src.ID() != testCIDLong {
		t.Fatalf("ID 应为容器 ID，得到 %q", src.ID())
	}
	// 解析不到时退回容器名（至少比空字符串强：空的 ID 会让采集器每轮都判"换了来源"）
	restoreResolver2 := SetContainerIDResolverForTest(func(string, *container.Runtime) string { return "" })
	defer restoreResolver2()
	if src.ID() != "atl-beta03" {
		t.Fatalf("解析不到 ID 时应退回容器名，得到 %q", src.ID())
	}
	_ = container.NameOf // 容器名规则由 container 包保证（atl- 前缀），这里只做引用
}
