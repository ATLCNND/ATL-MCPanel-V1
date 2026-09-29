// Package containermetrics 是实例监控采集器的**无 gRPC 依赖**实现。
//
// 为什么单独一个包（2026-09-29）：面板上"容器化实例 CPU 恒为 0、内存只有
// 十几 MB"的根因在采集逻辑里，而那段逻辑原先写在 grpcapi/metrics.go 里、
// 与 gRPC Server 强耦合 —— 想给它写单测就得先搭一个 Server。抽出来之后：
//
//   - CPU 差值的三条坑（首次采样、数据源切换、计数器回退）全部可单测，
//     不需要真容器，也不需要 root；
//   - native（/proc 读进程）与 container（读容器 cgroup）两条路径在同一处
//     并列，改一条不会悄悄影响另一条。
//
// 采集来源的判定：
//   - native：`/proc/<pid>/stat`、`/proc/<pid>/status`（与修复前**逐字节等价**）
//   - container：容器自己的 cgroup（容器内全部进程的累计 CPU / 当前内存）
//
// 为什么容器模式不能继续用 PID（根因）：Daemon 起的子进程是 `docker run`
// 这个 CLI，容器里的 java 是 dockerd 的子孙。拿 CLI 的 PID 读 /proc，
// 读到的是 CLI 自己的 CPU（差值为 0）与内存（十几 MB）。
package containermetrics

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ATLCNND/ATL-MCPanel/internal/daemon/cgroup"
	"github.com/ATLCNND/ATL-MCPanel/internal/daemon/container"
)

// cpuPerTick 换算系数：/proc/<pid>/stat 的 CPU 字段单位是 jiffies（Linux 上
// user_hz=100，即 1 tick = 10ms），换算成纳秒就是 ×10^7，
// 于是"纳秒 / 秒 / 1000 = 百分比"（100% = 1 核）。所有来源统一换算成纳秒，
// 避免 v1（纳秒）与 v2（微秒）的单位差被算成 1000 倍误差。
const cpuPerTick = 10_000_000

// defaultThreadLister 读取进程线程数（/proc/<pid>/status 的 Threads 字段）。
// 做成变量是为了让单测能注入假数据。
var defaultThreadLister = readProcThreads

// nowFn 取当前时间。做成变量是为了让单测能构造"恰好过了 2 秒"这类场景 ——
// 靠 sleep 写单测会又慢又飘（CI 上一抖就失败）。
var nowFn = time.Now

// statTimeout 回退到 docker stats 时的超时。docker CLI 起进程本身要几十毫秒，
// 采样间隔 2 秒的流式监控里不能让它无限期挂着。
const statTimeout = 10 * time.Second

// Source 一次采集的数据来源：native 进程或容器。
//
// 实现方（grpcapi）负责"实例 ID → 容器"的解析，本包只管读。
type Source interface {
	// Kind 返回 "native" 或 "container"，用于识别采集口径是否发生了变化。
	Kind() string
	// ID 返回来源标识：native 是 PID 的字符串，container 是容器名/容器 ID。
	// **它同时充当"实例重启"的判据** —— 容器被重建时容器 ID 必然变化，
	// 用它比用 CLI 的 PID 可靠得多（CLI 的 PID 每次启动都变，等于每轮都判重启）。
	ID() string
	// Snapshot 取一次采集。
	Snapshot(ctx context.Context) (Snapshot, error)
}

// Snapshot 一次采集的结果。
//
// CPU 有两种表达方式，二者互斥：
//
//   - CPUCount：**累计 CPU 时间（纳秒）**，由采集器做差值算百分比。
//     这是 native 与 cgroup 两条路用的方式，精度取决于采样间隔。
//   - DirectPct：**来源直接给出的百分比**（docker stats 回退路径）。
//     此时没有累计计数可存，差值逻辑无从谈起，直接用它的值。
//     早期草案曾想把百分比"伪装"成累计计数塞进 CPUCount ——
//     那是错的：下次采样拿到的是另一时刻的百分比，与差值毫无关系，
//     算出来的数会稳定偏离真实值，而且不会报任何错。
type Snapshot struct {
	CPUCount uint64
	MemUsed  uint64
	MemTotal uint64
	// Threads 线程数；<0 表示该来源取不到（界面按"未知"处理）。
	Threads int
	// DirectPct 为 docker stats 直接给出的百分比；HasDirectPct 表示它有效。
	DirectPct    float64
	HasDirectPct bool
}

// Collector 单个实例的监控采集器（CPU 百分比需要跨调用保存上一次采样）。
//
// 并发：面板的「统计」页与实例详情页会同时问同一个实例的指标，
// 所以状态必须加锁 —— 无锁会让两次采样互相把对方的时间基线冲掉，
// 算出来的百分比会莫名其妙地翻倍或归零。
type Collector struct {
	mu       sync.Mutex
	last     Snapshot
	lastAt   time.Time
	lastKind string // 上次采集口径（native / container）
	lastID   string // 上次来源标识（PID 或容器名）
}

// Sample 采集一次指标并计算 CPU 百分比。
//
// 返回的 (cpu, ok) 里 ok=false 表示"这一轮算不出百分比"，调用方应把
// CPU 留空而不是填 0 —— 0 会被用户读成"实例完全没在用 CPU"，
// 而事实是"还没到能算的时候"（首次采样、刚切换数据源、容器刚重建）。
// **这是本次修复的重点之一：错的数据比没有数据更糟。**
func (c *Collector) Sample(ctx context.Context, src Source) (Snapshot, float64, bool) {
	var snap Snapshot
	if src == nil {
		return snap, 0, false
	}
	got, err := src.Snapshot(ctx)
	if err != nil {
		slog.Warn("读取实例监控数据失败", "kind", src.Kind(), "id", src.ID(), "error", err)
		return snap, 0, false
	}
	snap = got

	// 回退路径（docker stats）自带百分比，不经过差值逻辑，也就没有
	// "基线可比性"的问题：这是它的优点，代价是每次采样要起一个进程。
	if snap.HasDirectPct {
		pct := snap.DirectPct
		if math.IsNaN(pct) || math.IsInf(pct, 0) || pct < 0 || pct > 10000 {
			return snap, 0, false
		}
		return snap, pct, true
	}

	kind, id := src.Kind(), src.ID()

	c.mu.Lock()
	defer c.mu.Unlock()

	// 口径变化（native↔container 切换）或来源变化（进程重启 / 容器重建）时，
	// 上一次的计数器与本轮**不可比**：直接比会得到一个巨大的负差值或正跳变。
	// 处理方式是把本轮当基线，跳过这一轮的百分比 —— 下一轮（2 秒后）就正常了。
	if !c.lastAt.IsZero() && (c.lastKind != kind || c.lastID != id) {
		slog.Info("监控数据来源已变化，跳过本轮 CPU 计算",
			"from_kind", c.lastKind, "from_id", c.lastID, "to_kind", kind, "to_id", id)
		c.rebase(snap, nowFn(), kind, id)
		return snap, 0, false
	}
	// 首次采样：没有基线，只能先把计数器存下来。
	if c.lastAt.IsZero() {
		c.rebase(snap, nowFn(), kind, id)
		return snap, 0, false
	}
	// 计数器回退：累计 CPU 时间只会单调增加，变小说明该 cgroup 被复用或
	// 计数被清零（容器在同一个 cgroup 目录下重建）。
	// 不处理的话差值会下溢成大正数，面板上会出现几十万 % 的 CPU 尖峰。
	if snap.CPUCount < c.last.CPUCount {
		slog.Info("监控计数出现回退，跳过本轮 CPU 计算",
			"kind", kind, "id", id, "prev", c.last.CPUCount, "now", snap.CPUCount)
		c.rebase(snap, nowFn(), kind, id)
		return snap, 0, false
	}

	at := nowFn()
	dt := at.Sub(c.lastAt).Seconds()
	delta := snap.CPUCount - c.last.CPUCount
	c.last, c.lastAt = snap, at

	if dt <= 0 {
		return snap, 0, false
	}
	pct := float64(delta) / (dt * 1e9) * 100.0
	// 多核下 >100% 是正常的（100% = 1 核）；这里只拦明显异常的值：
	// 采样间隔过短（同一毫秒内的两次调用）会算出天文数字，留在界面上就是"假数据"。
	if math.IsNaN(pct) || math.IsInf(pct, 0) || pct < 0 || pct > 10000 {
		return snap, 0, false
	}
	return snap, pct, true
}

// rebase 把本轮当新的基线（不计算百分比）。
func (c *Collector) rebase(snap Snapshot, at time.Time, kind, id string) {
	c.last, c.lastAt, c.lastKind, c.lastID = snap, at, kind, id
}

// ---------------------------------------------------------------------------
// 数据来源实现
// ---------------------------------------------------------------------------

// NativeSource 进程模式：沿用修复前的 /proc 读法（行为一字不改）。
//
// "一字不改"是硬要求：native 实例是现有用户正在用的路径，这次修的是容器，
// 不能顺手把 native 的语义也动了。
type NativeSource struct {
	PID int
}

func (NativeSource) Kind() string { return "native" }
func (s NativeSource) ID() string { return strconv.Itoa(s.PID) }

func (s NativeSource) Snapshot(context.Context) (Snapshot, error) {
	if s.PID <= 0 {
		return Snapshot{}, errors.New("实例进程不存在")
	}
	rss, threads := ReadProcStatus(s.PID)
	snap := Snapshot{
		CPUCount: ReadProcCPUTicks(s.PID) * cpuPerTick,
		MemUsed:  rss,
		Threads:  -1,
	}
	if threads > 0 {
		snap.Threads = threads
	}
	return snap, nil
}

// ContainerSource 容器模式：读容器自己的 cgroup。
type ContainerSource struct {
	InstanceID string
	// ContainerName 容器名（atl-<实例ID>）。docker 会把它设为容器内的
	// hostname，而 systemd 驱动下 cgroup 目录名就是 hostname 的前 12 位。
	ContainerName string
	// ContainerID 容器完整 ID（可选）：调用方若已经从 docker inspect 拿到，
	// 直接传进来能让定位更稳（不必依赖"目录名是短 ID"这个约定）。
	// 留空时只用 ContainerName。
	ContainerID string
	// Runtime 为 nil 表示容器运行时不可用（节点上 docker 被卸载/改名，
	// 但实例元数据里仍写着容器化）。
	Runtime *container.Runtime
}

func (ContainerSource) Kind() string { return "container" }

// ID 返回来源标识：**容器 ID**（拿得到时），否则退回容器名。
//
// 用容器 ID 而不是容器名，是因为它同时充当"实例是否被重建"的判据：
// 容器重建后 ID 必变（采集器据此跳过一轮），而容器名永远不变
// （同名重建会骗过判据，只能靠"计数回退"兜底）。
// 解析带缓存（5 分钟一次 docker inspect），所以这里不做进程开销。
func (s ContainerSource) ID() string {
	if cid := s.ResolveContainerID(); cid != "" {
		return cid
	}
	return s.ContainerName
}

// Snapshot 优先读 cgroup；失败（含目录形态不认识）才回退 docker stats。
//
// ⚠️ 这个方法**不改动接收者**（值接收者，改了也传不出去）。容器 ID 必须在
// 构造 Source 时就解析好（见 grpcapi.sourceFor 调用的 ResolveContainerID），
// 否则 Source.ID() 会在"进入 Snapshot 前"与"之后"给出不同的值 ——
// 采集器据此判断来源是否变化，于是每一轮都被判成"换了来源"并跳过 CPU，
// 界面上就永远是 cpu_percent: 0（实测踩过，见 collector_test.go
// TestContainerSourceIDIsStableAcrossSnapshot）。
func (s ContainerSource) Snapshot(ctx context.Context) (Snapshot, error) {
	var cgErr error
	if dir := s.findCgroup(); dir != "" {
		snap, err := s.readCgroup(dir)
		if err == nil {
			return snap, nil
		}
		cgErr = err
		slog.Warn("读取容器 cgroup 失败，回退 docker stats",
			"instance", s.InstanceID, "dir", dir, "error", err)
	}
	snap, err := s.readDockerStats(ctx)
	if err != nil {
		if cgErr != nil {
			return Snapshot{}, fmt.Errorf("%w；且 %w", cgErr, err)
		}
		return Snapshot{}, err
	}
	return snap, nil
}

// ResolveContainerID 返回该实例容器的完整 ID（带缓存；取不到返回空字符串）。
//
// 为什么需要它：cgroup 目录名是完整容器 ID，而实例侧只知道容器名，
// 二者没有可推导的关系（见 findCgroup 的说明）。这里把"问 docker"的结果
// 缓存起来，把每次采样都要起一个 CLI 进程的代价摊到 5 分钟一次。
func (s ContainerSource) ResolveContainerID() string {
	if s.ContainerID != "" {
		return s.ContainerID
	}
	return resolveContainerID(s.InstanceID, s.Runtime)
}

// findCgroup 定位容器的 cgroup 目录。
//
// 查找顺序（成本从低到高）：
//
//  1. 调用方给了容器 ID（ContainerID）时直接按它查；
//  2. 按容器名查（少数情形下 cgroup 目录名会用 hostname）；
//  3. 解析容器 ID（docker inspect，**带缓存**，见 resolveContainerID）。
//
// ⚠️ **不能只按容器名查**（第一版就是这么写的，实测永远是空）：
// docker 的 cgroup 目录名是**完整容器 ID**（实测 Debian 12 + docker 26.1.5：
// /sys/fs/cgroup/system.slice/docker-<64 位 ID>.scope），而容器名（atl-beta01）
// 与容器 ID 毫无关系 —— 容器内的 hostname 虽然也是 ID 前缀，但那只在容器
// **里面**可见，宿主侧按名字读目录是找不到的。后果是每次都静默回退到
// docker stats：CPU 用的是 docker 自己的采样窗口（数值"看着差不多对"），
// 而线程数拿不到（真机验证第一轮就是 threads 恒为 0）。
func (s ContainerSource) findCgroup() string {
	cid := s.ResolveContainerID()
	for _, want := range []string{cid, s.ContainerName, container.NameOf(s.InstanceID)} {
		if want == "" {
			continue
		}
		for _, dir := range cgroup.FindDockerCgroup(want) {
			// 必须能读到 CPU 与内存统计才算命中：cgroup 目录可能刚被 docker
			// 创建、文件还没就绪（容器正在启动），此时换成 docker stats 更稳。
			if _, err := cgroup.ReadCPUStat(dir); err != nil {
				continue
			}
			if _, _, err := cgroup.ReadMemoryStat(dir); err != nil {
				continue
			}
			return dir
		}
	}
	return ""
}

// containerIDTTL 解析成功的容器 ID 的缓存时间。
//
// 为什么要缓存：拿到 ID 只能问 docker（inspect 起一个 CLI 进程，几十毫秒），
// 而采样间隔只有 2~5 秒 —— 每轮都问一遍等于给节点持续加进程开销。
// 缓存过期不影响正确性：容器重建后 ID 会变，但新 ID 的 cgroup 计数从 0 开始，
// 由采集器的"计数回退"分支拦住（跳过一轮），随后 5 分钟内的解析会拿到新 ID。
const (
	containerIDTTL         = 5 * time.Minute
	containerIDNegativeTTL = 30 * time.Second
)

// containerIDFunc 解析容器 ID（单测可替换，避免依赖真 docker）。
var containerIDFunc = inspectContainerID

// containerIDCache 容器 ID 缓存（进程内，按实例）。
var (
	containerIDMu    sync.Mutex
	containerIDCache = map[string]containerIDEntry{}
)

type containerIDEntry struct {
	id string
	at time.Time
}

// resolveContainerID 返回实例容器的完整 ID（取不到返回空字符串）。
//
// 解析失败（容器还没起来 / docker 暂时不可用）时只缓存很短时间：
// 否则一次瞬时失败会在 5 分钟内一直退化成 docker stats。
func resolveContainerID(instanceID string, rt *container.Runtime) string {
	if instanceID == "" || rt == nil {
		return ""
	}
	now := nowFn()
	containerIDMu.Lock()
	if e, ok := containerIDCache[instanceID]; ok {
		ttl := containerIDTTL
		if e.id == "" {
			ttl = containerIDNegativeTTL
		}
		if now.Sub(e.at) < ttl {
			containerIDMu.Unlock()
			return e.id
		}
	}
	containerIDMu.Unlock()

	id := containerIDFunc(instanceID, rt)
	containerIDMu.Lock()
	containerIDCache[instanceID] = containerIDEntry{id: id, at: now}
	containerIDMu.Unlock()
	if id != "" {
		slog.Debug("已解析实例容器 ID", "instance", instanceID, "container_id", id)
	}
	return id
}

// inspectContainerID 问 docker 要容器 ID。
func inspectContainerID(instanceID string, rt *container.Runtime) string {
	ctx, cancel := context.WithTimeout(context.Background(), statTimeout)
	defer cancel()
	st, err := rt.Inspect(ctx, instanceID)
	if err != nil {
		slog.Warn("查询容器 ID 失败，本轮按 docker stats 采集", "instance", instanceID, "error", err)
		return ""
	}
	return st.ID
}

// SetContainerIDResolverForTest 替换容器 ID 解析函数并清空缓存（供单测）。
func SetContainerIDResolverForTest(fn func(instanceID string, rt *container.Runtime) string) func() {
	old := containerIDFunc
	containerIDFunc = fn
	containerIDMu.Lock()
	oldCache := containerIDCache
	containerIDCache = map[string]containerIDEntry{}
	containerIDMu.Unlock()
	return func() {
		containerIDFunc = old
		containerIDMu.Lock()
		containerIDCache = oldCache
		containerIDMu.Unlock()
	}
}

// readCgroup 读容器的 cgroup 统计。
//
// 内存必须与 CPU 同源：两个数分别来自"不同时刻的不同来源"时，界面上会出现
// 自相矛盾的组合（CPU 用容器、内存用 CLI 自己的），这正是这次要修的缺陷形态。
func (s ContainerSource) readCgroup(dir string) (Snapshot, error) {
	usage, err := cgroup.ReadCPUStat(dir)
	if err != nil {
		return Snapshot{}, fmt.Errorf("读取容器 CPU 统计失败: %w", err)
	}
	used, limit, err := cgroup.ReadMemoryStat(dir)
	if err != nil {
		return Snapshot{}, fmt.Errorf("读取容器内存统计失败: %w", err)
	}
	return Snapshot{
		CPUCount: usage,
		MemUsed:  used,
		MemTotal: limit,
		Threads:  threadsInCgroup(dir),
	}, nil
}

// readDockerStats 回退路径：问 docker 自己。
//
// 代价是每次采样起一个 docker CLI（采样间隔 2~5 秒，开销不小），
// 所以只在 cgroup 读不到时用。拿到的百分比直接采用（docker 自己按两次
// 采样算出来的），不再参与差值逻辑。
func (s ContainerSource) readDockerStats(ctx context.Context) (Snapshot, error) {
	if s.Runtime == nil {
		return Snapshot{}, errors.New("容器运行时不可用，且容器 cgroup 不可读")
	}
	cctx, cancel := context.WithTimeout(ctx, statTimeout)
	defer cancel()
	u, err := s.Runtime.Stats(cctx, s.InstanceID)
	if err != nil {
		return Snapshot{}, fmt.Errorf("读取 docker stats 失败: %w", err)
	}
	return Snapshot{
		DirectPct:    u.CPUPct,
		HasDirectPct: true,
		MemUsed:      u.MemBytes,
		Threads:      -1,
	}, nil
}

// ---------------------------------------------------------------------------
// /proc 读取（与修复前逐字节等价，改为导出以便单测与复用）
// ---------------------------------------------------------------------------

// ReadProcCPUTicks 读取进程 utime+stime（/proc/<pid>/stat 第14、15字段）。
func ReadProcCPUTicks(pid int) uint64 {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0
	}
	// stat 格式：pid (comm) state ...，comm 可能含空格，需从最后一个 ')' 后解析
	s := string(data)
	idx := strings.LastIndex(s, ")")
	if idx < 0 {
		return 0
	}
	rest := strings.Fields(s[idx+2:])
	// rest[0]=state(第3字段)，utime=第14字段=>rest[11]，stime=第15=>rest[12]
	if len(rest) < 13 {
		return 0
	}
	utime, _ := strconv.ParseUint(rest[11], 10, 64)
	stime, _ := strconv.ParseUint(rest[12], 10, 64)
	return utime + stime
}

// ReadProcStatus 读取进程 RSS 与线程数（一次读取 /proc/<pid>/status 拿两个值）。
//
// 合并成一次读取是因为两者本来就在同一个文件里：分开读等于把同一个文件
// 读两遍，而这段代码在实例运行时每 5 秒会被调用一次。
// 任一字段缺失/解析失败时返回 0，调用方按"未知"处理。
func ReadProcStatus(pid int) (rssBytes uint64, threads int) {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/status")
	if err != nil {
		return 0, 0
	}
	return parseProcStatus(string(data))
}

// parseProcStatus 解析 /proc/<pid>/status 里的 VmRSS 与 Threads。
func parseProcStatus(data string) (rssBytes uint64, threads int) {
	for _, line := range strings.Split(data, "\n") {
		switch {
		case strings.HasPrefix(line, "VmRSS:"):
			f := strings.Fields(line)
			if len(f) >= 2 {
				if kb, err := strconv.ParseUint(f[1], 10, 64); err == nil {
					rssBytes = kb * 1024
				}
			}
		case strings.HasPrefix(line, "Threads:"):
			f := strings.Fields(line)
			if len(f) >= 2 {
				if n, err := strconv.Atoi(f[1]); err == nil {
					threads = n
				}
			}
		}
	}
	return rssBytes, threads
}

// readProcThreads 读一个进程的线程数（读不到返回 -1，与 0 区分开：
// "线程数为 0"在真实进程上不可能，用它当哨兵值会把界面搞乱）。
func readProcThreads(pid int) int {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "status"))
	if err != nil {
		return -1
	}
	_, threads := parseProcStatus(string(data))
	if threads == 0 {
		return -1
	}
	return threads
}

// threadsInCgroup 统计容器 cgroup 内所有进程的线程数。
//
// 为什么统计全部进程而不是只取一个：容器里的负载可能是
// `sh start.sh → java`，PID 1 只是那个 shell（1 个线程），
// 只读它会让界面显示"线程数 1"，看起来像实例没跑起来。
// 读不到的进程直接跳过（进程刚退出时会这样，属于正常竞态）。
func threadsInCgroup(dir string) int {
	pids := cgroup.ReadSubtree(dir)
	if len(pids) == 0 {
		return -1
	}
	total := 0
	for _, pid := range pids {
		if n := defaultThreadLister(pid); n > 0 {
			total += n
		}
	}
	if total == 0 {
		return -1
	}
	return total
}

// SetThreadListerForTest 替换线程数读取函数（供单测注入假数据），返回恢复函数。
func SetThreadListerForTest(fn func(pid int) int) func() {
	old := defaultThreadLister
	defaultThreadLister = fn
	return func() { defaultThreadLister = old }
}
