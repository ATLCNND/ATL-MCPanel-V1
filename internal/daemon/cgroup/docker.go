// 容器 cgroup 指标读取。
//
// 为什么需要它（2026-09-29 修「容器化实例监控显示 0」）：
// 实例跑在容器里时，容器进程是 dockerd 的子孙，**不是 Daemon 的子进程**；
// Daemon 手上的 PID 是 `docker run` 这个 **CLI 进程**的 PID（见
// mcprocess.Instance.PID 与 docs/CONTAINERIZATION.md）。拿它去读
// /proc/<pid>/stat 得到的是 CLI 自己的 CPU（≈0）与内存（十几 MB），
// 于是面板上容器实例的 CPU 恒为 0、内存只有 docker stats 的千分之几。
//
// 容器自己那棵 cgroup 才是权威来源：里面有这个容器**全部进程**（含 java
// 及其子进程）的累计 CPU 时间与当前内存占用，且由内核维护、不需要起进程。
//
// cgroup v1 与 v2 都要支持（内测节点是 CentOS 7 / 3.10，只有 v1）：
//
//	v1: /sys/fs/cgroup/memory/docker/<id>/memory.usage_in_bytes
//	    /sys/fs/cgroup/cpuacct/docker/<id>/cpuacct.usage          （**纳秒**）
//	    /sys/fs/cgroup/memory/docker/<id>/memory.stat: total_rss
//	v2: /sys/fs/cgroup/system.slice/docker-<id>.scope/memory.current
//	    /sys/fs/cgroup/system.slice/docker-<id>.scope/cpu.stat: usage_usec（**微秒**）
//	    /sys/fs/cgroup/system.slice/docker-<id>.scope/memory.stat: inactive_file
//
// 单位不一致是这里最容易出错的地方（v1 纳秒 / v2 微秒），所以本文件对外
// **统一返回纳秒**，把换算集中在一处。
package cgroup

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// 容器 cgroup 的目录形态（同一个容器只会有其中一种）。
//
// 为什么三种都要认：目录名由 docker 的 cgroup driver 决定 ——
//
//	systemd driver：<base>/system.slice/docker-<完整 ID>.scope
//	cgroupfs driver：<base>/docker/<完整 ID>
//	（个别版本/发行版直接把 scope 放在 <base> 下）
//
// 实测（Debian 12 + docker 26.1.5）就是第一种：/sys/fs/cgroup/system.slice/
// docker-<id>.scope。把它写成 "system.slice/docker/<id>" 是**错的**，
// 表现为永远找不到容器、每次都静默回退到昂贵的 docker stats。
var dockerCgroupDirs = []string{
	"",                    // <base>/docker-<id>.scope
	"system.slice",        // <base>/system.slice/docker-<id>.scope
	"docker",              // <base>/docker/<id>
	"system.slice/docker", // 少数版本会把 scope 再放进一层子目录
}

// FindDockerCgroup 按容器 ID 或其任意一侧前缀定位容器的 cgroup 目录。
// 找不到返回空切片（调用方据此回退到 docker stats）。
//
// 两个方向都要比：
//
//	目录名是短 ID（docker 默认把 hostname 设为短 ID）← 传入完整 ID 时靠"目录名是它的前缀"命中
//	目录名是完整 ID（cgroupfs driver）            ← 传入短 ID 时靠"传入值是它的前缀"命中
//
// 只支持一个方向就会在另一种情形下静默回退到昂贵的 docker stats
// （实测：目录名是完整 ID、调用方传短 ID 那次就漏掉了）。
func FindDockerCgroup(idPrefix string) []string {
	if idPrefix == "" {
		return nil
	}
	key := strings.ToLower(idPrefix)
	short := key
	if len(short) > 12 {
		short = short[:12]
	}
	var out []string
	seen := map[string]bool{}
	for _, e := range dockerCgroupEntries() {
		// e.id 是目录名里解析出的 ID（可能是完整 ID，也可能是 12 位短 ID）
		if !strings.HasPrefix(e.id, short) && !strings.HasPrefix(key, e.id) {
			continue
		}
		if !seen[e.path] {
			seen[e.path] = true
			out = append(out, e.path)
		}
	}
	return out
}

// dockerCgroupEntry 一个候选的容器 cgroup 目录。
type dockerCgroupEntry struct {
	path string
	id   string // 从目录名解析出的容器 ID（小写）
}

// dockerCgroupEntries 列出所有形如容器 cgroup 的目录。
func dockerCgroupEntries() []dockerCgroupEntry {
	var out []dockerCgroupEntry
	seen := map[string]bool{}
	for _, base := range dockerCgroupBase() {
		for _, sub := range dockerCgroupDirs {
			dir := filepath.Join(base, sub)
			ents, err := os.ReadDir(dir)
			if err != nil {
				continue
			}
			for _, e := range ents {
				id, ok := dockerIDFromDirName(e.Name())
				if !ok {
					continue
				}
				full := filepath.Join(dir, e.Name())
				if seen[full] {
					continue
				}
				seen[full] = true
				out = append(out, dockerCgroupEntry{path: full, id: id})
			}
		}
	}
	return out
}

// dockerIDFromDirName 从 cgroup 目录名里取出容器 ID。
// 不是容器目录（名字对不上）时返回 ok=false。
func dockerIDFromDirName(name string) (string, bool) {
	name = strings.TrimSuffix(name, ".scope")
	name = strings.TrimPrefix(name, "docker-")
	if name == "" || name == "docker" {
		return "", false
	}
	// 容器 ID 是十六进制：长度对不上或含非十六进制字符的目录直接排除
	//（否则 /sys/fs/cgroup 下任何目录都来撞一次前缀）
	if len(name) < 12 || len(name) > 64 {
		return "", false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return "", false
		}
	}
	return name, true
}

// dockerCgroupBase 返回 cgroup 层级根目录的候选（v2 统一层级 + v1 各控制器层级）。
//
// v1 的挂载点名字随发行版变化（CentOS 7 是 cpu,cpuacct 合并挂载、Debian 分开），
// 所以这里复用 detectV1Mounts 解析 /proc/mounts，而不是硬编码路径。
func dockerCgroupBase() []string {
	bases := []string{cgroupV2Base}
	seen := map[string]bool{cgroupV2Base: true}
	addBase := func(p string) {
		if p != "" && !seen[p] {
			seen[p] = true
			bases = append(bases, p)
		}
	}
	for _, mnt := range detectV1Mounts() {
		addBase(mnt)
	}
	// v1 的多层级也可能**挂在同一个父目录下**（例如 /sys/fs/cgroup/memory/docker/…），
	// 而 /proc/mounts 在容器/受限环境里有时读不到或读不全。这里兜一层：
	// 把根目录下形如控制器的子目录也当候选。
	// （实测踩过：单测里造的 v1 假目录正是这种形态，第一版实现因此永远找不到容器。）
	for _, mnt := range v1ControllerMounts() {
		addBase(mnt)
	}
	return bases
}

// cgroupControllers v1 的控制器名（合并挂载时是 "cpu,cpuacct" 这种形式）。
var cgroupControllers = []string{
	"cpu", "cpuacct", "memory", "blkio", "cpuset", "devices", "freezer",
	"net_cls", "net_prio", "hugetlb", "perf_event", "pids", "rdma", "misc",
}

// v1ControllerMounts 列出 cgroup 根目录下形如控制器的子目录。
func v1ControllerMounts() []string {
	ents, err := os.ReadDir(cgroupV2Base)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		if !e.IsDir() || !isControllerDirName(e.Name()) {
			continue
		}
		out = append(out, filepath.Join(cgroupV2Base, e.Name()))
	}
	return out
}

// isControllerDirName 判断目录名是否由 cgroup 控制器名组成（逗号分隔）。
func isControllerDirName(name string) bool {
	for _, part := range strings.Split(name, ",") {
		known := false
		for _, c := range cgroupControllers {
			if part == c {
				known = true
				break
			}
		}
		if !known {
			return false
		}
	}
	return name != ""
}

// ReadCPUStat 读取容器 cgroup 的累计 CPU 时间，单位统一为**纳秒**。
func ReadCPUStat(dir string) (usageNs uint64, err error) {
	// v2：cpu.stat 的 usage_usec（微秒）
	if b, e := os.ReadFile(filepath.Join(dir, "cpu.stat")); e == nil {
		for _, line := range strings.Split(string(b), "\n") {
			f := strings.Fields(line)
			if len(f) == 2 && f[0] == "usage_usec" {
				v, perr := strconv.ParseUint(f[1], 10, 64)
				if perr != nil {
					return 0, perr
				}
				return v * 1000, nil
			}
		}
	}
	// v1：cpuacct.usage（纳秒）
	b, e := os.ReadFile(filepath.Join(dir, "cpuacct.usage"))
	if e != nil {
		return 0, e
	}
	return strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
}

// ReadMemoryStat 读取容器当前内存占用（字节）与上限（上限 0 表示不限制）。
//
// 读法刻意与 `docker stats` 对齐，否则同一时刻面板和 docker stats 会给出
// 两个不同的数（用户第一反应是"面板在编数据"）：
//
//	v2: memory.current - memory.stat:inactive_file  （dsh 的 Working Set 口径）
//	v1: memory.stat:total_rss                       （docker stats 在 v1 上用的就是它）
func ReadMemoryStat(dir string) (used, limit uint64, err error) {
	if b, e := os.ReadFile(filepath.Join(dir, "memory.current")); e == nil {
		cur, perr := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
		if perr != nil {
			return 0, 0, perr
		}
		// 不活跃的文件页缓存不算"占用"：它是可以随时回收的镜像/文件缓存，
		// 算进去会让每个容器看起来都比实际多占几百 MB。
		cur -= minUint64(cur, readStatField(filepath.Join(dir, "memory.stat"), "inactive_file"))
		return cur, readMemoryLimit(dir), nil
	}
	// v1：total_rss 才是真正的驻留内存（memory.usage_in_bytes 含 page cache）
	if v := readStatField(filepath.Join(dir, "memory.stat"), "total_rss"); v > 0 {
		return v, readMemoryLimit(dir), nil
	}
	b, e := os.ReadFile(filepath.Join(dir, "memory.usage_in_bytes"))
	if e != nil {
		return 0, 0, e
	}
	cur, perr := strconv.ParseUint(strings.TrimSpace(string(b)), 10, 64)
	if perr != nil {
		return 0, 0, perr
	}
	return cur, readMemoryLimit(dir), nil
}

// readMemoryLimit 读取内存上限。不限制（v2 的 "max" / v1 的 -1）返回 0。
func readMemoryLimit(dir string) uint64 {
	if b, err := os.ReadFile(filepath.Join(dir, "memory.max")); err == nil {
		s := strings.TrimSpace(string(b))
		if s != "max" {
			if v, err := strconv.ParseUint(s, 10, 64); err == nil {
				return v
			}
		}
		return 0
	}
	if b, err := os.ReadFile(filepath.Join(dir, "memory.limit_in_bytes")); err == nil {
		s := strings.TrimSpace(string(b))
		// v1 用 -1 表示不限制：先按有符号解析，再判断是否为正的上限，
		// 这样既不会把 -1 当成一个巨大的 uint64，也不会在解析处报错。
		if v, err := strconv.ParseInt(s, 10, 64); err == nil && v > 0 {
			return uint64(v)
		}
	}
	return 0
}

// readStatField 读取 cgroup 的键值对统计文件里某个字段（如 memory.stat）。
// 文件不存在或字段缺失返回 0。
func readStatField(path, key string) uint64 {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && f[0] == key {
			v, _ := strconv.ParseUint(f[1], 10, 64)
			return v
		}
	}
	return 0
}

// ReadSubtree 列出 cgroup 目录下所有进程 PID（cgroup.procs）。
// 容器内进程对宿主 /proc 是可见的，因此可以用它统计线程数等进程级信息。
func ReadSubtree(dir string) []int {
	if dir == "" {
		return nil
	}
	b, err := os.ReadFile(filepath.Join(dir, "cgroup.procs"))
	if err != nil {
		return nil
	}
	var out []int
	for _, f := range strings.Fields(string(b)) {
		if pid, err := strconv.Atoi(f); err == nil && pid > 0 {
			out = append(out, pid)
		}
	}
	return out
}

func minUint64(a, b uint64) uint64 {
	if a < b {
		return a
	}
	return b
}

// SetBaseForTest 把 cgroup 层级根目录换成 tmpRoot（供单测指向假 cgroup 文件），
// 返回恢复函数。
//
// 为什么需要它：生产环境是 cgroup v2 的统一层级 /sys/fs/cgroup，而 v1 节点上
// 还要解析 /proc/mounts。单测既不能往真 /sys 里建容器目录，也不该依赖跑测试
// 那台机器的 cgroup 版本，所以这里把"根目录候选"整个接管掉。
// 只影响当前进程，且必须由调用方 defer 恢复。
func SetBaseForTest(tmpRoot string) func() {
	oldBase, oldCandidates, oldMounts := cgroupV2Base, v1Candidates, procMounts
	cgroupV2Base = tmpRoot
	v1Candidates = tmpRoot
	procMounts = filepath.Join(tmpRoot, "proc-mounts-not-used")
	return func() {
		cgroupV2Base, v1Candidates, procMounts = oldBase, oldCandidates, oldMounts
	}
}
