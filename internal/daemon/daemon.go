// Package daemon 是 Daemon 的核心逻辑。
package daemon

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"runtime"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/ATLCNND/ATL-MCPanel/internal/common/config"
	"github.com/ATLCNND/ATL-MCPanel/internal/common/grpclimits"
	"github.com/ATLCNND/ATL-MCPanel/internal/common/logger"
	"github.com/ATLCNND/ATL-MCPanel/internal/daemon/cgroup"
	"github.com/ATLCNND/ATL-MCPanel/internal/daemon/grpcapi"
	"github.com/ATLCNND/ATL-MCPanel/internal/daemon/hoststats"
	"github.com/ATLCNND/ATL-MCPanel/internal/daemon/mcprocess"
	"github.com/ATLCNND/ATL-MCPanel/internal/daemon/monitor"
	"github.com/ATLCNND/ATL-MCPanel/internal/daemon/registry"
	"github.com/ATLCNND/ATL-MCPanel/internal/frp"
	"github.com/ATLCNND/ATL-MCPanel/internal/pki"
	pb "github.com/ATLCNND/ATL-MCPanel/internal/proto/mcpanel"
)

// Daemon 节点守护进程。
type Daemon struct {
	cfg   *config.DaemonConfig
	log   *logger.Logger
	reg   *registry.Registry
	stats *monitor.Store
	frp   *frp.Manager
	cg    *cgroup.Manager

	// shutdown 关闭 Run() 的心跳循环。
	// 由 main 在收到 SIGTERM/SIGINT 时触发 —— 让 Run 正常返回，
	// 这样它的 defer 能跑完（关 gRPC 连接、停监视协程），而不是被 os.Exit 跳过。
	shutdown     chan struct{}
	shutdownOnce sync.Once
}

// New 创建 Daemon 实例。
func New(cfg config.DaemonConfig, log *logger.Logger) (*Daemon, error) {
	if cfg.NodeID == "" {
		return nil, errors.New("daemon.node_id 不能为空")
	}
	if cfg.PanelAddress == "" {
		return nil, errors.New("daemon.panel_address 不能为空")
	}
	cfg.Defaults()
	reg := registry.New(cfg.InstanceDir)

	// 从磁盘恢复实例（Daemon 重启后不丢实例；对遗留进程做接管）
	loaded, adopted := reg.Load()
	if loaded > 0 {
		log.Info("已从磁盘恢复实例", "count", loaded, "adopted", adopted)
	}

	return &Daemon{
		cfg:      &cfg,
		log:      log,
		reg:      reg,
		stats:    monitor.NewStore(),
		frp:      frp.NewManager(""),
		shutdown: make(chan struct{}),
	}, nil
}

// Shutdown 请求 Daemon 退出：Run() 的心跳循环会返回，各 defer 正常执行。
//
// 幂等：重复调用不会 panic（用 sync.Once 保护 close）。
func (d *Daemon) Shutdown() {
	d.shutdownOnce.Do(func() { close(d.shutdown) })
}

// StopFrpAll 停止本机所有实例的 frpc 进程，返回停止的数量。
//
// 由 main 在 Run() 返回之后调用 —— 见 frp.Manager.StopAll 的注释：
// unit 用 KillMode=process（必须保持，否则停 Daemon 会杀掉用户的实例），
// 所以 Daemon 退出时得自己把 frpc 子进程收走，否则它们会变成孤儿。
func (d *Daemon) StopFrpAll() int {
	return d.frp.StopAll()
}

// transportCredentials 返回连接 Panel 的传输凭据。
// daemon.tls 为 true 时使用 mTLS（需 cert_file / key_file / ca_file），否则使用明文。
func (d *Daemon) transportCredentials() (credentials.TransportCredentials, error) {
	if !d.cfg.TLS {
		d.log.Warn("Daemon 未启用 mTLS（daemon.tls=false），与 Panel 的通信为明文；建议生产环境开启")
		return insecure.NewCredentials(), nil
	}
	if d.cfg.CertFile == "" || d.cfg.KeyFile == "" || d.cfg.CAFile == "" {
		return nil, fmt.Errorf("daemon.tls 已开启，但 cert_file / key_file / ca_file 未配置完整")
	}

	caPEM, err := os.ReadFile(d.cfg.CAFile)
	if err != nil {
		return nil, fmt.Errorf("读取 CA 证书失败: %w", err)
	}
	certPEM, err := os.ReadFile(d.cfg.CertFile)
	if err != nil {
		return nil, fmt.Errorf("读取客户端证书失败: %w", err)
	}
	keyPEM, err := os.ReadFile(d.cfg.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("读取客户端私钥失败: %w", err)
	}

	tlsCfg, err := pki.ClientTLSConfig(caPEM, certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("构造 mTLS 配置失败: %w", err)
	}
	d.log.Info("已启用 mTLS 连接 Panel", "ca", d.cfg.CAFile, "cert", d.cfg.CertFile)
	return credentials.NewTLS(tlsCfg), nil
}

// Run 启动 Daemon：起 gRPC server + 连 Panel 注册 + 心跳。
func (d *Daemon) Run() error {
	// 0. 初始化 cgroup 资源限制（失败只降级，不影响实例运行）
	if d.cfg.CgroupEnabled != nil && !*d.cfg.CgroupEnabled {
		d.log.Warn("cgroup 资源限制已被配置显式关闭（daemon.cgroup_enabled=false）")
	} else {
		d.cg = cgroup.New(d.cfg.CgroupRoot)
		d.cg.Init()
		if d.cg.Enabled() {
			mcprocess.SetResourceLimiter(d.cg)
			d.log.Info("已启用 cgroup 资源限制", "root", d.cfg.CgroupRoot)
		} else {
			d.log.Warn("cgroup 资源限制未启用，实例 CPU 配额不会生效", "reason", d.cg.Reason())
		}
	}

	// 注入 frps 管理 API 客户端（用于实例级流量统计；未配置则为 nil）
	if admin := frp.NewAdminClient(d.cfg.FRPAdminAddr, d.cfg.FRPAdminUser, d.cfg.FRPAdminPassword); admin != nil {
		d.frp.SetAdmin(admin)
		d.log.Info("已启用 frps 管理 API，实例流量将按隧道精确统计", "addr", d.cfg.FRPAdminAddr)
	} else {
		d.log.Info("未配置 frps 管理 API，实例流量将回退为节点整机统计")
	}

	// 启停计数挂在进程生命周期上（覆盖启动/停止/强杀/重启/接管等全部路径）
	mcprocess.SetLifecycleHook(func(instanceID string, started bool) {
		if started {
			d.reg.RecordStart(instanceID)
		} else {
			d.reg.RecordStop(instanceID)
		}
	})

	// 隧道与实例进程**同生共死**：进程一退出就收掉它的 frpc。
	//
	// 为什么挂在这里而不是在 StopInstance 处理器里顺手停：
	// `Stop()` 只是把 "stop" 写进 stdin 就返回（进程退出要等它存盘），
	// 处理器里立刻停 frpc 会造成"实例还在跑、隧道已经没了" —— 服务端没在读
	// 控制台时（下载依赖、JVM 卡住）这个错配能持续很久。
	// 挂在进程退出上还顺带覆盖了**崩溃**（此前实例崩了 frpc 会一直留着）。
	mcprocess.SetExitHook(func(instanceID string) {
		d.frp.StopInstance(instanceID)
	})

	// 隧道的存在也要看实例状态：实例没在跑时，frpc 只登记不启动。
	//
	// 否则「给一个已停止的实例开通/重新下发端口」会立刻把它的 frpc 拉起来 ——
	// 隧道后面什么都没有，却占着 frps 的 remote_port，界面上还会显示
	// "实例 stopped、隧道 running"（与 daemon 启动路径同一个根因，见第 16 项）。
	d.frp.SetInstanceState(func(instanceID string) bool {
		inst, ok := d.reg.Get(instanceID)
		return ok && inst.Status() == "running"
	})

	// 1. 启动 Daemon 自身的 gRPC server（供 Panel 反向调用）
	//
	// 必须显式放开消息大小上限：gRPC 默认只收 4 MB，而上传核心 jar /
	// 节点共享资源都是**一次性把整个文件塞进一条消息**的，
	// 不设这个值就会出现 "ResourceExhausted: received message larger than max
	// (43616069 vs. 4194304)" 这种与真实原因无关的报错。
	serverOpts := []grpc.ServerOption{
		grpc.MaxRecvMsgSize(grpclimits.MaxMessageBytes),
		grpc.MaxSendMsgSize(grpclimits.MaxMessageBytes),
	}
	if d.cfg.TLS {
		tlsCfg, err := pki.ServerTLSConfigFromFiles(d.cfg.CAFile, d.cfg.CertFile, d.cfg.KeyFile)
		if err != nil {
			return fmt.Errorf("构造 Daemon gRPC mTLS 配置失败: %w", err)
		}
		serverOpts = append(serverOpts, grpc.Creds(credentials.NewTLS(tlsCfg)))
	}
	grpcSrv := grpc.NewServer(serverOpts...)
	pb.RegisterDaemonServiceServer(grpcSrv, grpcapi.NewServer(d.cfg, d.reg, d.log, d.stats, d.frp))

	lis, err := net.Listen("tcp", d.cfg.GRPCListen)
	if err != nil {
		return fmt.Errorf("Daemon gRPC 监听失败: %w", err)
	}
	go func() {
		d.log.Info("Daemon gRPC 服务启动", "addr", d.cfg.GRPCListen, "mtls", d.cfg.TLS)
		if err := grpcSrv.Serve(lis); err != nil {
			d.log.Error("Daemon gRPC 退出", "error", err)
		}
	}()

	// 1.2 恢复各实例已下发的穿透隧道
	//
	// 分两步、不要合并：
	//   ① 先把隧道定义读回内存 —— **所有**实例都要读，否则实例启动时 Resume
	//      找不到定义，隧道就再也起不来了（定义是 Resume 的唯一来源）
	//   ② 再**只给在运行的实例**拉起 frpc
	//
	// 已停止的实例不该有 frpc：它停止时 StopInstance() 已经杀过了，Daemon 重启
	// 再拉起来会凭空占住公网隧道，界面上还会出现"实例 stopped / 隧道 running"
	// 这种自相矛盾的组合（2026-09-15 实测到过：实例 11 是 stopped，它的 frpc 却在跑）。
	if !d.frp.Available() {
		d.log.Warn("未检测到 frpc，穿透功能不可用（可在节点上安装 frp 客户端）")
	}
	for _, id := range d.reg.List() {
		inst, ok := d.reg.Get(id)
		if !ok {
			continue
		}
		if err := d.frp.LoadFromDisk(id, inst.Dir); err != nil {
			d.log.Warn("恢复隧道定义失败", "instance", id, "error", err)
			continue
		}
		// 实例进程还在（含被接管的）才拉 frpc
		if inst.Status() != "running" {
			continue
		}
		if err := d.frp.Resume(id); err != nil {
			d.log.Warn("拉起穿透隧道失败", "instance", id, "error", err)
		}
	}

	// 1.5 启动 RCON 监控采集（TPS / 在线玩家）
	stopMonitor := make(chan struct{})
	go monitor.NewPoller(d.reg, d.stats, d.log).Run(stopMonitor)
	defer close(stopMonitor)

	// 2. 连接 Panel 并注册
	transportCreds, err := d.transportCredentials()
	if err != nil {
		return err
	}
	conn, err := grpc.NewClient(d.cfg.PanelAddress,
		grpc.WithTransportCredentials(transportCreds),
	)
	if err != nil {
		return fmt.Errorf("连接 Panel 失败: %w", err)
	}
	defer conn.Close()

	client := pb.NewDaemonServiceClient(conn)

	hostname, _ := os.Hostname()
	regReq := &pb.RegisterRequest{
		NodeId:   d.cfg.NodeID,
		Hostname: hostname,
		Os:       detectOS(),
		Arch:     "amd64",
		CpuCores: int64(runtime.NumCPU()),
		MemTotal: totalMemory(),
		Version:  "0.2.0",
	}
	resp, err := client.Register(context.Background(), regReq)
	if err != nil {
		return fmt.Errorf("注册失败: %w", err)
	}
	if !resp.Accepted {
		return fmt.Errorf("Panel 拒绝注册: %s", resp.Message)
	}
	d.log.Info("注册成功", "node_id", d.cfg.NodeID, "msg", resp.Message)

	// 3. 心跳循环（同时上报主机资源，供「节点监控」页与磁盘告警使用）
	hostCollector := hoststats.New()
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-d.shutdown:
			d.log.Info("收到退出请求，停止心跳")
			return nil
		case <-ticker.C:
		}
		hs := hostCollector.Collect(d.cfg.InstanceDir)
		// 备份盘单独统计：冷存储常位于另一块（机械）盘，水位需独立告警
		backupPath := d.cfg.BackupRoot
		if backupPath == "" {
			backupPath = d.cfg.InstanceDir
		}
		bstat := hostCollector.CollectPath(backupPath)
		_, err := client.Ping(context.Background(), &pb.PingRequest{
			NodeId:     d.cfg.NodeID,
			Timestamp:  time.Now().UnixMilli(),
			CpuPercent: hs.CPUPercent,
			MemUsed:    hs.MemUsed,
			MemTotal:   hs.MemTotal,
			DiskUsed:   hs.DiskUsed,
			DiskTotal:  hs.DiskTotal,
			BackupDiskUsed:  bstat.DiskUsed,
			BackupDiskTotal: bstat.DiskTotal,
		})
		if err != nil {
			d.log.Warn("心跳失败", "error", err)
		}
	}
}