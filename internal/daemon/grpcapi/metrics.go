package grpcapi

import (
	"context"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	pb "github.com/ATLCNND/ATL-MCPanel/internal/proto/mcpanel"
)

// cpuSample 记录上次 CPU 采样（用于计算百分比）。
type cpuSample struct {
	total uint64 // 进程总 tick（utime+stime）
	at    time.Time
}

// metricsCollector 每个实例的监控采集器。
type metricsCollector struct {
	mu      sync.Mutex
	lastCPU cpuSample
	lastPID int // 上次采样的 PID（用于识别进程重启，避免计数器回退）
}

// GetMetrics 返回单次监控快照（带历史采样，CPU 百分比正确）。
func (s *Server) GetMetrics(ctx context.Context, req *pb.InstanceRequest) (*pb.Metrics, error) {
	inst, ok := s.reg.Get(req.InstanceId)
	if !ok {
		return &pb.Metrics{InstanceId: req.InstanceId}, nil
	}
	pid := inst.PID()
	if pid == 0 {
		return &pb.Metrics{InstanceId: req.InstanceId}, nil
	}
	// 使用 Server 上的持久采样器，保证连续调用能算出 CPU 百分比
	m := s.getCollector(req.InstanceId).sample(req.InstanceId, pid)
	s.applyRconStats(m)
	return m, nil
}

// StreamMetrics 持续推送监控数据。
func (s *Server) StreamMetrics(req *pb.InstanceRequest, stream pb.DaemonService_StreamMetricsServer) error {
	inst, ok := s.reg.Get(req.InstanceId)
	if !ok {
		return nil
	}

	col := s.getCollector(req.InstanceId)
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		pid := inst.PID()
		if pid == 0 {
			// 发送一个空 metrics 表示停止
			_ = stream.Send(&pb.Metrics{InstanceId: req.InstanceId})
			continue
		}
		m := col.sample(req.InstanceId, pid)
		s.applyRconStats(m)
		if err := stream.Send(m); err != nil {
			return err
		}
	}
	return nil
}

// applyRconStats 把 RCON 采集到的 TPS/在线玩家合并进监控数据。
func (s *Server) applyRconStats(m *pb.Metrics) {
	if s.stats == nil || m == nil {
		return
	}
	st, ok := s.stats.Get(m.InstanceId)
	if !ok {
		return
	}
	m.Tps = st.TPS
	m.PlayersOnline = st.Players
	m.PlayersMax = st.MaxPlayers
}

// sample 采样并计算 CPU 百分比（基于两次采样差值）。
func (c *metricsCollector) sample(instanceID string, pid int) *pb.Metrics {
	m := &pb.Metrics{InstanceId: instanceID, Timestamp: time.Now().UnixMilli()}

	// 内存 + 线程数（同一个 /proc/<pid>/status，只读一次）
	rss, threads := readProcStatus(pid)
	if rss > 0 {
		m.MemUsed = rss
	}
	m.Threads = int32(threads)

	// CPU（utime + stime ticks）
	total := readProcCPUTicks(pid)
	now := time.Now()
	c.mu.Lock()
	// 首次采样或进程已变化（重启）时重置基线，避免计数器回退导致数值异常
	if c.lastCPU.at.IsZero() || c.lastPID != pid || total < c.lastCPU.total {
		c.lastPID = pid
		c.lastCPU = cpuSample{total: total, at: now}
		c.mu.Unlock()
		return m
	}
	// 计算百分比：delta_ticks / delta_time / (ticks_per_sec) * 100
	dt := now.Sub(c.lastCPU.at).Seconds()
	dtick := total - c.lastCPU.total
	c.lastCPU = cpuSample{total: total, at: now}
	c.mu.Unlock()

	if dt > 0 {
		// 100 是 Linux HZ（user_hz），通常 sysconf(_SC_CLK_TCK)=100
		cpuPct := (float64(dtick) / 100.0 / dt) * 100.0
		// 多核下可能 >100%；异常值（如采样间隔极短造成的尖峰）直接归零
		if cpuPct < 0 || cpuPct > 10000 {
			cpuPct = 0
		}
		m.CpuPercent = cpuPct
	}
	return m
}

// readProcCPUTicks 读取进程 utime+stime（/proc/<pid>/stat 第14、15字段）。
func readProcCPUTicks(pid int) uint64 {
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

// readProcStatus 读取进程 RSS 与线程数（一次读取 /proc/<pid>/status 拿两个值）。
//
// 合并成一次读取是因为两者本来就在同一个文件里：分开读等于把同一个文件
// 读两遍，而这段代码在实例运行时每 5 秒会被调用一次。
// 任一字段缺失/解析失败时返回 0，调用方按"未知"处理。
func readProcStatus(pid int) (rssBytes uint64, threads int) {
	data, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/status")
	if err != nil {
		return 0, 0
	}
	for _, line := range strings.Split(string(data), "\n") {
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
