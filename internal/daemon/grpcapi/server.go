// Package grpcapi 是 Daemon 侧的 gRPC 服务实现（供 Panel 反向调用）。
package grpcapi

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ATLCNND/ATL-MCPanel/internal/common/logger"
	"github.com/ATLCNND/ATL-MCPanel/internal/frp"
	"github.com/ATLCNND/ATL-MCPanel/internal/common/config"
	"github.com/ATLCNND/ATL-MCPanel/internal/daemon/hoststats"
	"github.com/ATLCNND/ATL-MCPanel/internal/daemon/jobqueue"
	"github.com/ATLCNND/ATL-MCPanel/internal/daemon/monitor"
	"github.com/ATLCNND/ATL-MCPanel/internal/daemon/registry"
	"github.com/ATLCNND/ATL-MCPanel/internal/daemon/resources"
	pb "github.com/ATLCNND/ATL-MCPanel/internal/proto/mcpanel"
)

// Server 实现 DaemonService（Daemon 侧）。
type Server struct {
	pb.UnimplementedDaemonServiceServer
	cfg       *config.DaemonConfig
	reg       *registry.Registry
	log       *logger.Logger
	stats     *monitor.Store
	frp       *frp.Manager
	res       *resources.Store
	collectMu sync.Mutex
	collects  map[string]*metricsCollector // instance_id -> 采样器
	jobs      *jobqueue.Queue              // 节点公共任务队列（压缩 / 解压）
}

// NewServer 创建 Daemon gRPC 服务。
func NewServer(cfg *config.DaemonConfig, reg *registry.Registry, log *logger.Logger, stats *monitor.Store, frpMgr *frp.Manager) *Server {
	if stats == nil {
		stats = monitor.NewStore()
	}
	if frpMgr == nil {
		frpMgr = frp.NewManager("")
	}
	s := &Server{
		cfg:      cfg,
		reg:      reg,
		log:      log,
		stats:    stats,
		frp:      frpMgr,
		collects: make(map[string]*metricsCollector),
	}

	// 节点共享资源目录（管理员统一上传、实例复用）。
	// 目录建不出来只记警告不阻断启动 —— 实例管理本身不依赖它，
	// 而"资源功能不可用"比"Daemon 起不来"轻得多。
	if cfg != nil {
		s.res = resources.NewStore(cfg.ResourceDir)
		if err := s.res.Touch(); err != nil {
			log.Warn("创建节点资源目录失败，共享资源功能不可用", "dir", cfg.ResourceDir, "error", err)
		} else {
			log.Info("节点资源目录", "dir", s.res.Dir())
		}
	}

	// 节点公共任务队列。
	//
	// 并发度默认 1：压缩/解压的瓶颈在磁盘，多个任务并行只会互相拖慢，
	// 更会把 IO 抢光导致在线玩家卡顿。需要吞吐的场景（NVMe + 大量小文件）
	// 可在 daemon 配置里用 job_workers 调整。
	workers := 1
	if cfg != nil && cfg.JobWorkers > 0 {
		workers = cfg.JobWorkers
	}
	s.jobs = jobqueue.New(s.runJob, jobqueue.Options{
		Workers: workers,
		Logger:  log,
	})
	s.jobs.Start()
	log.Info("节点公共任务队列已启动", "workers", workers)

	return s
}

// getCollector 获取或创建实例的采样器（CPU 百分比需跨调用共享历史）。
func (s *Server) getCollector(instanceID string) *metricsCollector {
	s.collectMu.Lock()
	defer s.collectMu.Unlock()
	c, ok := s.collects[instanceID]
	if !ok {
		c = &metricsCollector{}
		s.collects[instanceID] = c
	}
	return c
}

// CreateInstance 创建实例。
func (s *Server) CreateInstance(ctx context.Context, req *pb.CreateInstanceRequest) (*pb.OperationResponse, error) {
	s.log.Info("创建实例", "id", req.InstanceId, "core_type", req.CoreType, "jar", req.JarUrl, "start_command", req.StartCommand)
	inst, err := s.reg.Create(registry.Meta{
		ID:           req.InstanceId,
		Name:         req.Name,
		CoreType:     req.CoreType,
		JavaVersion:  req.JavaVersion,
		JarPath:      req.JarUrl,
		MaxMem:       req.MaxMem,
		MinMem:       req.MinMem,
		Port:         req.Port,
		StartCommand: req.StartCommand,
		CPUQuota:     int(req.CpuQuota),
		BackupDir:    req.BackupDir,
		MemLimit:     req.MemLimit,
	})
	if err != nil {
		return &pb.OperationResponse{Success: false, Error: err.Error()}, nil
	}
	_ = inst
	return &pb.OperationResponse{Success: true, Message: "实例已创建"}, nil
}

// StartInstance 启动实例。
func (s *Server) StartInstance(ctx context.Context, req *pb.InstanceRequest) (*pb.OperationResponse, error) {
	inst, ok := s.reg.Get(req.InstanceId)
	if !ok {
		return &pb.OperationResponse{Success: false, Error: "实例不存在"}, nil
	}
	if err := inst.Start(); err != nil {
		s.log.Warn("实例启动失败", "instance", req.InstanceId, "error", err)
		return &pb.OperationResponse{Success: false, Error: err.Error()}, nil
	}
	// 记录实际使用的启动方式（start.sh / custom / default），便于排查
	// 「改了脚本却没生效」这类问题
	s.log.Info("实例已启动", "instance", req.InstanceId, "start_mode", inst.StartMode())

	// 实例启动后自动恢复其穿透隧道：停止实例会断开隧道，
	// 若此处不拉起，公网入口会一直不可用，需要管理员手动「重新下发」。
	if err := s.frp.Resume(req.InstanceId); err != nil {
		s.log.Warn("恢复穿透隧道失败", "instance", req.InstanceId, "error", err)
	}
	return &pb.OperationResponse{Success: true, Message: "已启动"}, nil
}

// StopInstance 停止实例（优雅）。同时停止该实例的 frpc（隧道随之断开）。
// StopInstance 停止实例（优雅）。
//
// 这里**不**停 frpc：`Stop()` 只是把 "stop" 指令写进 stdin 就返回，此刻实例
// 往往还在存盘退出。隧道改由进程退出回调收尾（见 mcprocess.SetExitHook），
// 这样"隧道在不在"始终等于"实例进程在不在" —— 否则一旦服务端没在读控制台
// （首次启动下载依赖、JVM 卡住、插件死锁），就会出现"实例 running、隧道没了"
// 且长时间不自愈的错配。
func (s *Server) StopInstance(ctx context.Context, req *pb.InstanceRequest) (*pb.OperationResponse, error) {
	inst, ok := s.reg.Get(req.InstanceId)
	if !ok {
		return &pb.OperationResponse{Success: false, Error: "实例不存在"}, nil
	}
	if err := inst.Stop(); err != nil {
		return &pb.OperationResponse{Success: false, Error: err.Error()}, nil
	}
	return &pb.OperationResponse{Success: true, Message: "已发送停止指令"}, nil
}

// instanceRuntimeCache 缓存目录体积统计。
//
// 遍历世界目录可能耗时数百毫秒，而前端每次打开详情页都会查询，
// 因此缓存一段时间，避免重复遍历大目录。
type instanceRuntimeCache struct {
	mu      sync.Mutex
	entries map[string]runtimeEntry
}

type runtimeEntry struct {
	used, total, free int64
	at                time.Time
}

const runtimeDiskCacheTTL = 60 * time.Second

var runtimeCache = &instanceRuntimeCache{entries: make(map[string]runtimeEntry)}

// runtimeCollector 供运行数据接口复用的采集器。
//
// **必须常驻**：网络速率由两次采样的差值算出。若每次请求都新建采集器，
// 每次都只有"首次采样"，速率会恒为 0。
var runtimeCollector = hoststats.New()

func (c *instanceRuntimeCache) get(id, dir string) (used, total, free int64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.entries[id]; ok && time.Since(e.at) < runtimeDiskCacheTTL {
		return e.used, e.total, e.free
	}
	used, total, free = hoststats.DirUsage(dir)
	c.entries[id] = runtimeEntry{used: used, total: total, free: free, at: time.Now()}
	return used, total, free
}

// GetInstanceRuntime 返回运行时长、启停次数与磁盘用量（详情页右栏使用）。
func (s *Server) GetInstanceRuntime(ctx context.Context, req *pb.InstanceRequest) (*pb.InstanceRuntime, error) {
	inst, ok := s.reg.Get(req.InstanceId)
	if !ok {
		return &pb.InstanceRuntime{Success: false, Error: "实例不存在"}, nil
	}

	startCount, stopCount, lastStart := s.reg.Runtime(req.InstanceId)
	status := inst.Status()

	var uptime int64
	if status == "running" && lastStart > 0 {
		uptime = time.Now().Unix() - lastStart
		if uptime < 0 {
			uptime = 0
		}
	}

	dir := s.reg.Dir(req.InstanceId)
	used, total, free := runtimeCache.get(req.InstanceId, dir)

	// 网络吞吐：整机聚合值（见 hoststats.netThroughput 的说明）
	hs := runtimeCollector.CollectPath(dir)

	// 若配置了 frps 管理 API，优先使用**该实例隧道**的真实流量 ——
	// 整机聚合值无法区分是哪个实例在占带宽。
	netIn, netOut := hs.NetRxTotal, hs.NetTxTotal
	netInRate, netOutRate := hs.NetRxRate, hs.NetTxRate
	netLevel := "node"
	if ti, to, tir, tor, _, ok := s.frp.InstanceTraffic(req.InstanceId); ok {
		netIn, netOut = ti, to
		netInRate, netOutRate = tir, tor
		netLevel = "instance"
	}

	return &pb.InstanceRuntime{
		Success:       true,
		InstanceId:    req.InstanceId,
		Status:        status,
		UptimeSeconds: uptime,
		LastStartAt:   lastStart,
		StartCount:    int32(startCount),
		StopCount:     int32(stopCount),
		DiskUsed:      used,
		DiskTotal:     total,
		DiskFree:      free,
		NetRxRate:     netInRate,
		NetTxRate:     netOutRate,
		NetRxTotal:    netIn,
		NetTxTotal:    netOut,
		NetScope:      netLevel,
		// 这两个提示一直是"算出来了没人用"（访问器存在但全仓库无调用方）。
		// 它们回答的是界面上很容易被误解的两件事：为什么选了 JDK 17 却在跑 21、
		// 为什么设了 CPU/内存上限却没生效。空字符串 = 一切正常。
		JavaNote:     inst.JavaNote(),
		LimitWarning: inst.LimitWarning(),
	}, nil
}

// KillInstance 强制关闭实例（SIGKILL 整个进程组）。
//
// 与 StopInstance 的区别：Stop 发送 "stop" 命令让服务端正常保存并退出，
// Kill 直接杀进程，**可能丢失未落盘的数据**。仅在实例卡死无响应时使用。
func (s *Server) KillInstance(ctx context.Context, req *pb.InstanceRequest) (*pb.OperationResponse, error) {
	inst, ok := s.reg.Get(req.InstanceId)
	if !ok {
		return &pb.OperationResponse{Success: false, Error: "实例不存在"}, nil
	}
	if inst.Status() == "stopped" {
		return &pb.OperationResponse{Success: false, Error: "实例未在运行"}, nil
	}
	if err := inst.Kill(); err != nil {
		s.log.Warn("强制关闭失败", "instance", req.InstanceId, "error", err)
		return &pb.OperationResponse{Success: false, Error: err.Error()}, nil
	}
	// 隧道随实例一起断开
	s.frp.StopInstance(req.InstanceId)

	s.log.Warn("实例已被强制关闭（SIGKILL）", "instance", req.InstanceId)
	return &pb.OperationResponse{Success: true, Message: "已强制关闭（未保存的数据可能丢失）"}, nil
}

// RestartInstance 重启实例。
// 重启需要等待进程退出（可能数十秒），因此异步执行并立即返回，避免请求超时。
func (s *Server) RestartInstance(ctx context.Context, req *pb.InstanceRequest) (*pb.OperationResponse, error) {
	inst, ok := s.reg.Get(req.InstanceId)
	if !ok {
		return &pb.OperationResponse{Success: false, Error: "实例不存在"}, nil
	}
	s.log.Info("重启实例", "id", req.InstanceId)
	go func() {
		if err := inst.Restart(90 * time.Second); err != nil {
			s.log.Error("重启失败", "id", req.InstanceId, "error", err)
		} else {
			s.log.Info("重启完成", "id", req.InstanceId)
		}
	}()
	return &pb.OperationResponse{Success: true, Message: "重启中（请稍候查看状态）"}, nil
}

// DeleteInstance 删除实例。若实例运行中，先停止（优雅→超时强杀）再注销。
func (s *Server) DeleteInstance(ctx context.Context, req *pb.DeleteInstanceRequest) (*pb.OperationResponse, error) {
	if inst, ok := s.reg.Get(req.InstanceId); ok && inst.Status() == "running" {
		s.log.Info("删除前先停止实例", "id", req.InstanceId)
		_ = inst.Stop()
		deadline := time.Now().Add(20 * time.Second)
		for time.Now().Before(deadline) && inst.Status() == "running" {
			time.Sleep(300 * time.Millisecond)
		}
		if inst.Status() == "running" {
			_ = inst.Kill()
			for n := 0; n < 20 && inst.Status() == "running"; n++ {
				time.Sleep(200 * time.Millisecond)
			}
		}
		if inst.Status() == "running" {
			return &pb.OperationResponse{Success: false, Error: "实例无法停止，删除已取消"}, nil
		}
	}

	// 目录要在注销之前取出来：注销之后注册表里就没有这条记录了
	dir := s.reg.Dir(req.InstanceId)

	// 幂等：节点上根本没注册过这个实例时，"删除"的**目标状态已经达成**，
	// 不该当成失败。
	//
	// 原来的实现让 registry.Delete 返回错误「实例不存在」，而面板只在
	// resp.Success 时才清理数据库记录 —— 于是一条"节点上已不存在"的
	// 孤儿记录会**永远删不掉**，白占实例 ID 与游戏端口。
	// 实测库里就积了 3 条这样的记录（节点目录早已不在）。
	//
	// 注意这里必须只针对"未注册"这一种情况放宽：
	// 上面"实例无法停止，删除已取消"那条路径**仍然返回失败** ——
	// 若把那种情况也当成成功，面板会把还活着的实例记录删掉，实例就失管了。
	alreadyGone := false
	if _, ok := s.reg.Get(req.InstanceId); !ok {
		alreadyGone = true
		s.log.Warn("实例未在节点上注册，按已删除处理", "id", req.InstanceId)
	} else if err := s.reg.Delete(req.InstanceId); err != nil {
		return &pb.OperationResponse{Success: false, Error: err.Error()}, nil
	}
	s.frp.StopInstance(req.InstanceId)

	// 连文件一起删：这是**不可恢复**的操作，只在管理员显式要求时执行。
	// 默认只注销（保留世界存档），见 registry.Delete 的注释。
	if req.RemoveFiles {
		if dir == "" {
			return &pb.OperationResponse{Success: true, Message: "已删除（实例目录路径未知，文件未清理）"}, nil
		}
		// 防呆：绝不允许对过短/可疑的路径执行 RemoveAll
		clean := filepath.Clean(dir)
		if clean == "/" || clean == "." || len(strings.Split(strings.Trim(clean, "/"), "/")) < 2 {
			s.log.Error("拒绝删除可疑路径", "dir", clean)
			return &pb.OperationResponse{
				Success: true,
				Message: "已删除实例记录，但目录路径可疑（" + clean + "），已跳过文件清理",
			}, nil
		}
		if err := os.RemoveAll(clean); err != nil {
			s.log.Warn("实例已注销但删除文件失败", "dir", clean, "error", err)
			return &pb.OperationResponse{Success: true,
				Message: "已删除实例，但清理目录失败：" + err.Error() + "（目录：" + clean + "）"}, nil
		}
		s.log.Warn("实例与其全部文件已彻底删除", "id", req.InstanceId, "dir", clean)
		if alreadyGone {
			return &pb.OperationResponse{Success: true, Message: "节点上本已无该实例，已清理残留目录"}, nil
		}
		return &pb.OperationResponse{Success: true, Message: "已彻底删除（含磁盘文件）"}, nil
	}

	if alreadyGone {
		return &pb.OperationResponse{Success: true, Message: "节点上本已无该实例，已视为删除"}, nil
	}
	return &pb.OperationResponse{Success: true, Message: "已删除（磁盘文件保留）"}, nil
}

// GetInstanceStatus 获取实例状态。
func (s *Server) GetInstanceStatus(ctx context.Context, req *pb.InstanceRequest) (*pb.InstanceStatus, error) {
	inst, ok := s.reg.Get(req.InstanceId)
	if !ok {
		return &pb.InstanceStatus{InstanceId: req.InstanceId, Status: "not_found"}, nil
	}
	return &pb.InstanceStatus{
		InstanceId: req.InstanceId,
		Status:     inst.Status(),
		// 图标是"可选装饰"：这里只上报修改时间（0 = 没有），
		// 前端据此决定要不要请求图标本体，并把它当缓存击穿参数。
		IconMtime: instanceIconMtime(inst.Dir),
	}, nil
}

// consoleHistoryLines 控制台附加时回放的历史输出行数。
const consoleHistoryLines = 500

// Console 控制台双向流。
//
// 附加时会先回放最近的历史输出（来自实例 logs/console.log 尾部），
// 使前端重连后能立即看到之前的日志，随后再转入实时输出。
func (s *Server) Console(stream pb.DaemonService_ConsoleServer) error {
	var instanceID string

	// 先接收第一个 frame，确定要附加的实例
	first, err := stream.Recv()
	if err != nil {
		return err
	}
	if first.Type != pb.ConsoleFrame_ATTACH {
		return fmt.Errorf("第一个 frame 必须是 ATTACH")
	}
	instanceID = first.InstanceId

	inst, ok := s.reg.Get(instanceID)
	if !ok {
		return fmt.Errorf("实例 %s 不存在", instanceID)
	}

	// 先订阅控制台输出（开始缓冲新输出），再回放历史，避免两者之间丢行
	outCh, cancel := inst.Subscribe()
	defer cancel()

	s.log.Info("控制台附加", "instance", instanceID)

	// 发送附加成功
	_ = stream.Send(&pb.ConsoleFrame{Type: pb.ConsoleFrame_STATUS, InstanceId: instanceID, Data: "attached"})

	// 回放最近的历史输出，使重连后能看到之前的日志
	if history := inst.RecentOutput(consoleHistoryLines); len(history) > 0 {
		_ = stream.Send(&pb.ConsoleFrame{
			Type:       pb.ConsoleFrame_OUTPUT,
			InstanceId: instanceID,
			Data:       fmt.Sprintf("\x1b[90m[以下为最近 %d 行历史输出]\x1b[0m\n", len(history)),
			IsStdout:   true,
		})
		// 历史回放与实时输出各用一个装饰器实例：堆栈续行靠"上一行是不是错误"
		// 判断，两条流各从头开始，不能共用状态
		hl := &consoleHighlighter{}
		for _, line := range history {
			if err := stream.Send(&pb.ConsoleFrame{
				Type:       pb.ConsoleFrame_OUTPUT,
				InstanceId: instanceID,
				// 历史回放与实时输出走同一套行装饰，重连前后观感一致
				Data:     hl.line(line),
				IsStdout: true,
			}); err != nil {
				return err
			}
		}
		_ = stream.Send(&pb.ConsoleFrame{
			Type:       pb.ConsoleFrame_OUTPUT,
			InstanceId: instanceID,
			Data:       "\x1b[90m[历史输出结束，以下为实时输出]\x1b[0m\n",
			IsStdout:   true,
		})
	}

	// 接管状态提示（无 stdin，无法执行命令）
	if inst.Adopted() {
		_ = stream.Send(&pb.ConsoleFrame{
			Type:       pb.ConsoleFrame_OUTPUT,
			InstanceId: instanceID,
			Data:       "\x1b[93m[提示] 该实例处于接管状态（Daemon 曾重启），控制台为只读；重启实例可恢复完整控制。\x1b[0m\n",
			IsStdout:   true,
		})
	}

	// 命令执行结果通过 notify 通道回传，避免与输出转发并发 Send
	notify := make(chan string, 16)
	done := make(chan struct{})

	// 读命令 goroutine
	go func() {
		defer close(done)
		for {
			frame, err := stream.Recv()
			if err != nil {
				return
			}
			switch frame.Type {
			case pb.ConsoleFrame_COMMAND:
				if err := inst.SendCommand(frame.Data); err != nil {
					select {
					case notify <- "\x1b[91m[命令未执行] " + err.Error() + "\x1b[0m\n":
					default:
					}
				}
			case pb.ConsoleFrame_DETACH:
				return
			}
		}
	}()

	// 主循环：转发实例输出 + 命令执行通知
	//
	// 装饰器放在循环外：它记着"上一条错误行之后还能涂几条堆栈续行"，
	// 每条控制台流一个实例（见 console_format.go）。
	hl := &consoleHighlighter{}
	for {
		select {
		case <-done:
			return nil
		case line, ok := <-outCh:
			if !ok {
				return nil
			}
			// 警告/错误行加底色、异常堆栈的后续行跟着涂（见 console_format.go）。
			// 放在这里而不是前端：历史回放与实时输出共用同一处装饰，不用维护两套逻辑。
			if err := stream.Send(&pb.ConsoleFrame{
				Type:       pb.ConsoleFrame_OUTPUT,
				InstanceId: instanceID,
				Data:       hl.line(line),
				IsStdout:   true,
			}); err != nil {
				return err
			}
		case msg := <-notify:
			if err := stream.Send(&pb.ConsoleFrame{
				Type:       pb.ConsoleFrame_OUTPUT,
				InstanceId: instanceID,
				Data:       msg,
				IsStdout:   true,
			}); err != nil {
				return err
			}
		}
	}
}