package grpcapi

import (
	"context"
	"log/slog"
	"time"

	"github.com/ATLCNND/ATL-MCPanel/internal/daemon/container"
	"github.com/ATLCNND/ATL-MCPanel/internal/daemon/containermetrics"
	"github.com/ATLCNND/ATL-MCPanel/internal/daemon/mcprocess"
	pb "github.com/ATLCNND/ATL-MCPanel/internal/proto/mcpanel"
)

// metricsCollector 每个实例的监控采集器。
//
// 只是 containermetrics.Collector 的壳：采集与 CPU 差值逻辑都在那个包里，
// 这样它可以被单测直接覆盖（不需要 gRPC Server，也不需要真容器）。
type metricsCollector struct {
	containermetrics.Collector
}

// GetMetrics 返回单次监控快照（带历史采样，CPU 百分比正确）。
func (s *Server) GetMetrics(ctx context.Context, req *pb.InstanceRequest) (*pb.Metrics, error) {
	inst, ok := s.reg.Get(req.InstanceId)
	if !ok {
		return &pb.Metrics{InstanceId: req.InstanceId}, nil
	}
	src := sourceFor(inst)
	if src == nil {
		return &pb.Metrics{InstanceId: req.InstanceId}, nil
	}
	// 使用 Server 上的持久采样器，保证连续调用能算出 CPU 百分比
	m := s.getCollector(req.InstanceId).snapshot(ctx, req.InstanceId, src)
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
		src := sourceFor(inst)
		if src == nil {
			// 发送一个空 metrics 表示停止
			_ = stream.Send(&pb.Metrics{InstanceId: req.InstanceId})
			continue
		}
		m := col.snapshot(stream.Context(), req.InstanceId, src)
		s.applyRconStats(m)
		if err := stream.Send(m); err != nil {
			return err
		}
	}
	return nil
}

// snapshot 采集一次并组装成 pb.Metrics。
func (c *metricsCollector) snapshot(ctx context.Context, instanceID string, src containermetrics.Source) *pb.Metrics {
	m := &pb.Metrics{InstanceId: instanceID, Timestamp: time.Now().UnixMilli()}
	snap, cpu, ok := c.Sample(ctx, src)
	if snap.MemUsed > 0 {
		m.MemUsed = snap.MemUsed
	}
	// 容器模式下内存上限由 docker 施加（cgroup 的 memory.max），直接报出来，
	// 免得界面上"已用"与"上限"两个数出自两个地方、互相对不上。
	// 取不到时保持 0，与修复前的 native 行为一致。
	if snap.MemTotal > 0 {
		m.MemTotal = snap.MemTotal
	}
	// Threads 取不到时是 -1（见 containermetrics.Snapshot），此时保持 0：
	// 面板把这个字段当作"未知"，不会拿 -1 去显示。
	if snap.Threads > 0 {
		m.Threads = int32(snap.Threads)
	}
	// CPU 只在"这一轮真的算得出来"时才写入：ok=false 表示还没有可比基线
	//（首次采样 / 数据源刚切换 / 容器刚重建），这时留 0 而不是编一个数。
	if ok {
		m.CpuPercent = cpu
	}
	return m
}

// sourceFor 决定这个实例的监控数据从哪里读。
//
// 这是本次修复（2026-09-29「容器化实例监控指标显示 0」）的核心分叉点：
//
//   - 容器化实例：**绝不能**用 inst.PID()。容器模式下那个 PID 是 `docker run`
//     这个 CLI 进程的 PID（容器里的 java 是 dockerd 的子孙，不是 Daemon 的子进程），
//     读 /proc 得到的是 CLI 自己的 CPU（差值为 0）与内存（十几 MB）——
//     面板上就是"cpu_percent 0、mem_used 17MB"。
//     改为读容器自己的 cgroup（见 containermetrics.ContainerSource）。
//   - 非容器实例：与修复前**完全一致**的 /proc 读法，一个字都没改。
//
// 返回 nil 表示"现在没有可读的来源"（实例未运行 / 容器运行时不可用），
// 调用方按原有的空数据返回。
func sourceFor(inst *mcprocess.Instance) containermetrics.Source {
	if inst.Containerized() {
		// 容器模式下 PID() 是 CLI 的 PID，与容器内进程无关，因此这里不等它：
		// 只要元数据说容器化、运行时可用，就按容器采集。容器没跑时读不到 cgroup，
		// 会退到 docker stats 并如实失败 —— 而不是显示 CLI 自己的数字。
		rt := inst.Runtime()
		if rt == nil {
			// 元数据说容器化但运行时不可用：不退回读 CLI 的 /proc，
			// 那只会把"监控不可用"伪装成一组看着正常的假数字。
			slog.Warn("实例标记为容器化，但容器运行时不可用，监控数据暂不可采", "instance", inst.ID)
			return nil
		}
		src := containermetrics.ContainerSource{
			InstanceID:    inst.ID,
			ContainerName: container.NameOf(inst.ID),
			Runtime:       rt,
		}
		// **必须在构造时就解析容器 ID**（内部带缓存，5 分钟一次 docker inspect）：
		// 采集器把 Source.ID() 当作"实例是否被重建"的判据，而它每轮会调用两次
		// （Snapshot 前 / 后）—— 如果 ID 留到 Snapshot 里再解析，两次读到的值
		// 不一样，每一轮都会被当成"换了来源"而跳过 CPU，界面永远显示 0。
		src.ContainerID = src.ResolveContainerID()
		return src
	}
	pid := inst.PID()
	if pid == 0 {
		return nil
	}
	return containermetrics.NativeSource{PID: pid}
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
