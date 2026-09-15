// Package hoststats 采集节点主机的资源使用情况（CPU / 内存 / 磁盘）。
//
// 采集结果随心跳上报给 Panel，用于「节点监控」页展示与磁盘告警。
// 所有函数在读取失败时返回零值而非错误 —— 监控数据缺失不应影响心跳。
package hoststats

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Stats 主机资源快照。
type Stats struct {
	CPUPercent float64 // 整机 CPU 使用率（0-100）
	MemUsed    int64   // 已用内存（字节）
	MemTotal   int64   // 总内存（字节）
	DiskUsed   int64   // 指定路径所在分区已用（字节）
	DiskTotal  int64   // 分区总量（字节）

	// 网络吞吐（聚合所有物理网卡，排除回环）
	NetRxRate  int64 // 下行速率（字节/秒，上次采样至本次的平均值）
	NetTxRate  int64 // 上行速率（字节/秒）
	NetRxTotal int64 // 累计接收（字节，自本次开机起）
	NetTxTotal int64 // 累计发送（字节）
}

// Collector 带状态的采集器（CPU 使用率需要两次采样求差）。
type Collector struct {
	mu       sync.Mutex
	lastIdle uint64
	lastTot  uint64
	lastAt   time.Time

	// 网络速率需要两次采样的差值
	lastRx uint64
	lastTx uint64
	lastNetAt time.Time
}

// New 创建采集器。
func New() *Collector { return &Collector{} }

// Collect 采集一次；diskPath 用于确定统计哪个分区（通常为实例根目录）。
func (c *Collector) Collect(diskPath string) Stats {
	var s Stats
	s.CPUPercent = c.cpuPercent()
	s.MemUsed, s.MemTotal = memInfo()
	s.DiskUsed, s.DiskTotal = diskUsage(diskPath)
	s.NetRxRate, s.NetTxRate, s.NetRxTotal, s.NetTxTotal = c.netThroughput()
	return s
}

// netThroughput 通过 /proc/net/dev 的两次采样差值计算网络速率。
//
// 注意：这是**整机**（聚合所有物理网卡）的吞吐，不是单个实例的。
// 单实例流量需要从 frp 的 admin API 读取每个 proxy 的统计，
// 属于后续增强项。对"节点上主要就跑一个服务端"的场景，
// 两者数值接近，且能反映公网带宽是否跑满。
func (c *Collector) netThroughput() (rxRate, txRate, rxTotal, txTotal int64) {
	rx, tx, ok := readNetDev()
	if !ok {
		return 0, 0, 0, 0
	}
	rxTotal, txTotal = int64(rx), int64(tx)

	c.mu.Lock()
	defer c.mu.Unlock()

	now := time.Now()
	if c.lastNetAt.IsZero() || rx < c.lastRx || tx < c.lastTx {
		// 首次采样或计数器回绕（网卡重置/重启）
		c.lastRx, c.lastTx, c.lastNetAt = rx, tx, now
		return 0, 0, rxTotal, txTotal
	}
	elapsed := now.Sub(c.lastNetAt).Seconds()
	if elapsed < 0.5 {
		// 间隔过短时差值噪声大，不更新基准也不给速率
		return 0, 0, rxTotal, txTotal
	}
	rxRate = int64(float64(rx-c.lastRx) / elapsed)
	txRate = int64(float64(tx-c.lastTx) / elapsed)
	c.lastRx, c.lastTx, c.lastNetAt = rx, tx, now
	return rxRate, txRate, rxTotal, txTotal
}

// readNetDev 读取 /proc/net/dev，返回所有非回环接口的累计 (接收, 发送) 字节数。
func readNetDev() (rx, tx uint64, ok bool) {
	b, err := os.ReadFile("/proc/net/dev")
	if err != nil {
		return 0, 0, false
	}
	for i, line := range strings.Split(string(b), "\n") {
		// 前两行是表头
		if i < 2 {
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}
		iface := strings.TrimSpace(parts[0])
		if iface == "lo" {
			continue // 排除回环，否则本地 frp 转发会把流量算进去
		}
		f := strings.Fields(parts[1])
		if len(f) < 10 {
			continue
		}
		r, err1 := strconv.ParseUint(f[0], 10, 64)
		t, err2 := strconv.ParseUint(f[8], 10, 64)
		if err1 != nil || err2 != nil {
			continue
		}
		rx += r
		tx += t
		ok = true
	}
	return rx, tx, ok
}

// CollectPath 只采集指定路径所在分区的磁盘用量。
// 用于单独统计备份盘（冷存储）的水位——它常常是另一块盘。
func (c *Collector) CollectPath(path string) Stats {
	var s Stats
	s.DiskUsed, s.DiskTotal = diskUsage(path)
	s.NetRxRate, s.NetTxRate, s.NetRxTotal, s.NetTxTotal = c.netThroughput()
	return s
}

// cpuPercent 通过 /proc/stat 的两次采样差值计算整机 CPU 使用率。
func (c *Collector) cpuPercent() float64 {
	idle, total, ok := readCPUTimes()
	if !ok {
		return 0
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	// 首次采样或计数器回绕（重启）：只记录基准，本次返回 0
	if c.lastAt.IsZero() || total < c.lastTot || idle < c.lastIdle {
		c.lastIdle, c.lastTot, c.lastAt = idle, total, time.Now()
		return 0
	}

	// 采样间隔过短时差值噪声大，直接复用上次的基准但不更新
	dIdle := float64(idle - c.lastIdle)
	dTot := float64(total - c.lastTot)
	c.lastIdle, c.lastTot, c.lastAt = idle, total, time.Now()

	if dTot <= 0 {
		return 0
	}
	pct := (dTot - dIdle) / dTot * 100
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	return pct
}

// readCPUTimes 读取 /proc/stat 第一行（cpu 汇总），返回 (idle 时间, 总时间)。
func readCPUTimes() (idle, total uint64, ok bool) {
	b, err := os.ReadFile("/proc/stat")
	if err != nil {
		return 0, 0, false
	}
	line := string(b)
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	fields := strings.Fields(line)
	if len(fields) < 5 || fields[0] != "cpu" {
		return 0, 0, false
	}
	// 字段顺序：user nice system idle iowait irq softirq steal guest guest_nice
	for i, f := range fields[1:] {
		v, err := strconv.ParseUint(f, 10, 64)
		if err != nil {
			continue
		}
		total += v
		// idle(第4个) + iowait(第5个) 视为空闲
		if i == 3 || i == 4 {
			idle += v
		}
	}
	return idle, total, total > 0
}

// memInfo 读取 /proc/meminfo，返回 (已用, 总量)。
// 已用 = MemTotal - MemAvailable（MemAvailable 比 MemFree 更能反映真实可用量）。
func memInfo() (used, total int64) {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0, 0
	}
	var avail int64
	for _, line := range strings.Split(string(b), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		v, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			continue
		}
		switch fields[0] {
		case "MemTotal:":
			total = v * 1024
		case "MemAvailable:":
			avail = v * 1024
		}
	}
	if total == 0 {
		return 0, 0
	}
	used = total - avail
	if used < 0 {
		used = 0
	}
	return used, total
}

// diskUsage 返回指定路径所在文件系统的 (已用, 总量)。
// 路径不存在时回溯到最近的存在目录。
func diskUsage(path string) (used, total int64) {
	p := path
	for p != "" && p != "/" {
		if _, err := os.Stat(p); err == nil {
			break
		}
		p = parentDir(p)
	}
	if p == "" {
		p = "/"
	}

	var st syscall.Statfs_t
	if err := syscall.Statfs(p, &st); err != nil {
		return 0, 0
	}
	bs := int64(st.Bsize)
	total = int64(st.Blocks) * bs
	free := int64(st.Bavail) * bs
	used = total - free
	if used < 0 {
		used = 0
	}
	return used, total
}

// DirUsage 统计目录占用（递归累加普通文件大小），并返回所在分区的
// 总量与剩余。目录不存在时返回 0。
//
// 注意：大目录（含大量区域文件的世界）遍历可能耗时数百毫秒，
// 调用方应做缓存，避免频繁调用。
func DirUsage(dir string) (used, total, free int64) {
	var sum int64
	_ = filepath.WalkDir(dir, func(_ string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // 权限不足等情况跳过，不影响整体统计
		}
		if d.IsDir() {
			return nil
		}
		if info, err := d.Info(); err == nil {
			sum += info.Size()
		}
		return nil
	})
	used = sum
	_, total = diskUsage(dir)
	// diskUsage 只返回 (已用, 总量)，剩余量需单独取
	var st syscall.Statfs_t
	if err := syscall.Statfs(dir, &st); err == nil {
		free = int64(st.Bavail) * int64(st.Bsize)
	}
	return used, total, free
}

func parentDir(p string) string {
	i := strings.LastIndexByte(p, '/')
	if i <= 0 {
		return "/"
	}
	return p[:i]
}
