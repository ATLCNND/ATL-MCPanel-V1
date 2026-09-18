// Package daemon 是 Daemon 的核心逻辑。
package daemon

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/ATLCNND/ATL-MCPanel/internal/common/config"
	"github.com/ATLCNND/ATL-MCPanel/internal/common/grpclimits"
	"github.com/ATLCNND/ATL-MCPanel/internal/common/logger"
	"github.com/ATLCNND/ATL-MCPanel/internal/common/version"
	"github.com/ATLCNND/ATL-MCPanel/internal/daemon/cgroup"
	"github.com/ATLCNND/ATL-MCPanel/internal/daemon/container"
	"github.com/ATLCNND/ATL-MCPanel/internal/daemon/grpcapi"
	"github.com/ATLCNND/ATL-MCPanel/internal/daemon/hoststats"
	"github.com/ATLCNND/ATL-MCPanel/internal/daemon/mcprocess"
	"github.com/ATLCNND/ATL-MCPanel/internal/daemon/monitor"
	"github.com/ATLCNND/ATL-MCPanel/internal/daemon/registry"
	"github.com/ATLCNND/ATL-MCPanel/internal/daemon/runas"
	"github.com/ATLCNND/ATL-MCPanel/internal/frp"
	"github.com/ATLCNND/ATL-MCPanel/internal/pki"
	pb "github.com/ATLCNND/ATL-MCPanel/internal/proto/mcpanel"
)

// Daemon 节点守护进程。
type Daemon struct {
	cfg    *config.DaemonConfig
	log    *logger.Logger
	reg    *registry.Registry
	stats  *monitor.Store
	frp    *frp.Manager
	cg     *cgroup.Manager
	runner *runas.Manager

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

	// 实例运行身份。这一步只解析配置、不碰系统；真正的用户创建推迟到
	// 建实例 / 启实例时（见 runas.Ensure）。
	runner, err := runas.New(cfg.InstanceUser, cfg.InstanceUserPrefix)
	if err != nil {
		return nil, err
	}
	// 启动时就把"实例会不会跑成 root"这件事说清楚。以 root 跑实例是本次修掉的
	// 漏洞，所以这里用 Warn/Info 明确打印当前生效的身份，而不是让它默默生效。
	if runas.IsRoot() {
		log.Info("实例进程将以降权身份运行", "模式", runner.Describe())
	} else {
		log.Warn("Daemon 未以 root 运行：无法创建实例专用系统用户，"+
			"实例将与 Daemon 同身份运行（cgroup 资源限制通常也需要 root）",
			"模式", runner.Describe())
	}

	// 目录布局与权限（见 config.StateDir 的说明）：
	//   实例根目录    0711 —— 实例用户必须能**穿过**它（才能进自己那层），
	//                        但不必能列目录：列出来只会让别人知道这台机器上有哪些实例
	//   实例目录      0700 —— 由各实例在创建/启动时设置（见 registry.Create / mcprocess.Start）
	//   状态目录      0700 —— 只有 root 能进：里面是被 root 信任的元数据与 pid
	//   frp 状态目录  0700 —— 同理：frpc 以 root 运行、读这里的配置
	if err := runas.EnsureDir(cfg.InstanceDir, 0o711); err != nil {
		return nil, fmt.Errorf("准备实例根目录失败: %w", err)
	}
	if err := runas.EnsureDir(cfg.StateDir, 0o700); err != nil {
		return nil, fmt.Errorf("准备状态目录失败: %w", err)
	}
	if err := runas.EnsureDir(cfg.FrpStateDir, 0o700); err != nil {
		return nil, fmt.Errorf("准备 frp 状态目录失败: %w", err)
	}

	// 老布局迁移：元数据与 frp 文件原先就在实例目录里，现在要搬到平台状态目录。
	// 放在 Load 之前做 —— Load 是按新位置读元数据的，先搬再读才不会"实例全丢"。
	migrated := migrateLegacyLayout(cfg, log)
	if migrated > 0 {
		log.Info("已把实例元数据与穿透文件迁移到平台状态目录（实例目录不再存放 root 信任的文件）",
			"count", migrated, "state_dir", cfg.StateDir, "frp_state_dir", cfg.FrpStateDir)
	}

	reg := registry.New(cfg.InstanceDir, cfg.StateDir)
	reg.SetRunner(runner)

	// 容器运行时（可选）：节点装了 docker 才有。没有也不影响其它功能，
	// 只是容器化开关在面板上会被拒绝（见 registry.SetContainerMode）。
	//
	// 注意这里**不**检查镜像是否存在：镜像缺失是"启动实例时"的错误，
	// 而 Daemon 启动路径上做 docker 调用会让节点重启变慢、也会在 docker 未起时误判。
	// 面板侧的节点信息里会单独显示 docker 与镜像的就绪状态。
	var ctr *container.Runtime
	if cfg.Container.ContainerEnabledOr(true) {
		ctr = container.Detect(cfg.Container.Image)
	}
	if ctr != nil {
		// 共享资源目录可能是相对路径（config.yaml 的默认值就是 "resources"），
		// 而容器挂载**必须**用绝对路径，否则 docker 会把 "resources:resources:ro"
		// 当成命名卷并直接拒绝启动。这里统一转成绝对路径再交给运行时。
		resDir := cfg.ResourceDir
		if abs, err := filepath.Abs(resDir); err == nil {
			resDir = abs
		}
		reg.SetContainerRuntime(ctr, resDir)
		ver := ctr.Version(context.Background())
		log.Info("容器化隔离可用", "docker", ver, "image", ctr.Image(),
			"resources_dir", resDir)
	} else {
		log.Info("容器化隔离不可用（未安装 docker 或已在配置里关闭）")
	}

	// 从磁盘恢复实例（Daemon 重启后不丢实例；对遗留进程做接管）
	loaded, adopted := reg.Load()
	if loaded > 0 {
		log.Info("已从磁盘恢复实例", "count", loaded, "adopted", adopted)
	}

	// 老装机的属主/权限纠正：以前实例目录是 root:root 0755。
	//
	// 为什么要在这里做一次，而不是只靠"启动实例时顺手改"：
	//   - 权限是**静态**的暴露面。一个已经停了很久的实例，它的 0755 目录
	//     与 0644 存档照样能被同机器上别的实例进程读走 —— 只要它还没被启动过，
	//     "启动时纠正"就永远轮不到它。
	//   - 所以启动时扫一遍：只对"属主或权限不对"的实例动手（各一次 stat），
	//     已经迁移过的实例零成本。
	// 失败只记警告：一个坏目录不该让整个 Daemon 起不来。
	if n, err := fixInstanceOwnership(reg, runner, log); err != nil {
		log.Warn("纠正实例目录属主/权限时出错", "error", err)
	} else if n > 0 {
		log.Info("已把老装机的实例目录交给各自的运行用户并收紧为 0700", "count", n)
	}

	return &Daemon{
		cfg:      &cfg,
		log:      log,
		reg:      reg,
		stats:    monitor.NewStore(),
		frp:      frp.NewManager(""),
		runner:   runner,
		shutdown: make(chan struct{}),
	}, nil
}

// FrpDir 返回某实例的 frpc 工作目录（平台状态目录下，root 0700）。
//
// **不再用实例目录**：frpc.toml 决定"把哪些端口挂到哪个 frps 上"，而 frpc 以 root
// 运行 —— 让实例用户能改写这份配置，等于允许他把节点上任意本地端口
// （22/SSH、9091/Daemon gRPC）挂到自己的 frps 上对外暴露。
func (d *Daemon) FrpDir(instanceID string) string {
	return filepath.Join(d.cfg.FrpStateDir, instanceID)
}

// fixInstanceOwnership 把老装机的实例目录交给各自的运行用户并收紧权限。
//
// 判据是"属主不对或权限不是 0700"——已经迁移过的实例只需一次 stat。
// 返回被纠正的实例数。
func fixInstanceOwnership(reg *registry.Registry, runner *runas.Manager, log *logger.Logger) (int, error) {
	if runner == nil || !runas.IsRoot() {
		return 0, nil
	}
	fixed := 0
	for _, id := range reg.List() {
		dir := reg.Dir(id)
		fi, err := os.Stat(dir)
		if err != nil {
			continue
		}
		st, ok := fi.Sys().(*syscall.Stat_t)
		if !ok {
			continue
		}
		ident, err := runner.Ensure(id, dir)
		if err != nil {
			log.Warn("实例运行用户不可用，跳过属主纠正", "instance", id, "error", err)
			continue
		}
		if st.Uid == ident.UID && st.Gid == ident.GID && fi.Mode().Perm() == 0o700 {
			continue // 已经是目标状态
		}
		if err := runas.ChownTree(dir, ident); err != nil {
			log.Warn("纠正实例目录属主失败", "instance", id, "error", err)
			continue
		}
		if err := os.Chmod(dir, 0o700); err != nil {
			log.Warn("收紧实例目录权限失败", "instance", id, "error", err)
			continue
		}
		fixed++
	}
	return fixed, nil
}

// migrateLegacyLayout 把老装机里放在实例目录下的平台状态搬到状态目录。
//
// 迁移的对象只有三样，都是"root 会去读、因此不能被租户改写"的文件：
//   - instance.json（元数据：配额、jar 路径、启停统计）
//   - daemon.pid（PID 记录：接管与停止时的依据）
//   - frpc.toml / tunnels.json / frpc.pid / logs/frpc.log（穿透配置与状态）
//
// 幂等：目标目录已有该文件时不覆盖（可能已经是新的、正在用的那份）。
// 单个实例失败只记警告并继续 —— 一个坏目录不该让整个 Daemon 起不来。
func migrateLegacyLayout(cfg config.DaemonConfig, log *logger.Logger) int {
	entries, err := os.ReadDir(cfg.InstanceDir)
	if err != nil {
		return 0
	}
	moved := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		id := e.Name()
		instDir := filepath.Join(cfg.InstanceDir, id)
		stateDir := filepath.Join(cfg.StateDir, id)
		frpDir := filepath.Join(cfg.FrpStateDir, id)

		type pair struct{ from, to string }
		var moves []pair
		for _, f := range []string{"instance.json", "daemon.pid"} {
			moves = append(moves, pair{filepath.Join(instDir, f), filepath.Join(stateDir, f)})
		}
		for _, f := range []string{"frpc.toml", "tunnels.json", "frpc.pid"} {
			moves = append(moves, pair{filepath.Join(instDir, f), filepath.Join(frpDir, f)})
		}
		// frpc 的日志在 <实例目录>/logs/frpc.log，搬到 <frp 状态目录>/logs/frpc.log
		moves = append(moves, pair{filepath.Join(instDir, "logs", "frpc.log"), filepath.Join(frpDir, "logs", "frpc.log")})

		did := 0
		for _, mv := range moves {
			if _, err := os.Stat(mv.from); err != nil {
				continue // 源不存在：无需迁移
			}
			if _, err := os.Stat(mv.to); err == nil {
				continue // 目标已在：不覆盖
			}
			if err := os.MkdirAll(filepath.Dir(mv.to), 0o700); err != nil {
				log.Warn("迁移状态文件失败（建目录）", "instance", id, "path", mv.to, "error", err)
				continue
			}
			if err := os.Rename(mv.from, mv.to); err != nil {
				log.Warn("迁移状态文件失败", "instance", id, "from", mv.from, "to", mv.to, "error", err)
				continue
			}
			did++
		}
		if did > 0 {
			moved++
		}
	}
	return moved
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
			// 报出用的是 v1 还是 v2：两者文件名与单位都不同，
			// 排查"设了配额却没生效"时，第一个要确认的就是这个。
			ver := "v2"
			if d.cg.Version() == 1 {
				ver = "v1（老内核，如 CentOS 7）"
			}
			d.log.Info("已启用 cgroup 资源限制", "version", ver, "path", d.cg.RootPath(), "root", d.cfg.CgroupRoot)
		} else {
			d.log.Warn("cgroup 资源限制未启用，实例 CPU/内存配额不会生效",
				"reason", d.cg.Reason(),
				"hint", "需要 cgroup v2（内核≥4.15 且统一层级）或 cgroup v1（memory 与 cpu 控制器已挂载）")
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
		if err := d.frp.LoadFromDisk(id, d.FrpDir(id)); err != nil {
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
		// 原来这里是硬编码的 "amd64" 与 "0.2.0" —— 在 arm64 包和多版本并存时都是错的：
		// 面板按上报的版本判断节点是否配套（文档明确要求面板与节点版本一致），
		// 写死一个常量等于把这条检查废掉；arch 写死则让 arm64 节点上报成 amd64。
		Arch:     runtime.GOARCH,
		CpuCores: int64(runtime.NumCPU()),
		MemTotal: totalMemory(),
		Version:  version.Short(),
	}
	resp, err := d.registerWithRetry(client, regReq)
	if err != nil {
		return err
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
			NodeId:          d.cfg.NodeID,
			Timestamp:       time.Now().UnixMilli(),
			CpuPercent:      hs.CPUPercent,
			MemUsed:         hs.MemUsed,
			MemTotal:        hs.MemTotal,
			DiskUsed:        hs.DiskUsed,
			DiskTotal:       hs.DiskTotal,
			BackupDiskUsed:  bstat.DiskUsed,
			BackupDiskTotal: bstat.DiskTotal,
		})
		if err != nil {
			d.log.Warn("心跳失败", "error", err)
		}
	}
}

// ErrShutdown 表示"因为收到退出信号而提前结束"，**不是故障**。
//
// 为什么要单独区分：main 对 Run() 返回的任何 error 都会记 `ERROR 运行失败` 并 exit 1。
// 于是 `systemctl stop` 会在日志里留下一条 ERROR（"收到退出请求，放弃注册"），
// 看到的人会以为服务崩了 —— 而实际是我们自己请求的、完全正常的退出。
// 这类"把正常路径记成错误"的日志会污染告警与排查，值得单独一个哨兵值。
var ErrShutdown = errors.New("收到退出请求")

// registerWithRetry 反复尝试注册，直到成功、被面板明确拒绝、或收到退出信号。
//
// ---------------------------------------------------------------------------
// 为什么不能"失败一次就 return err"（这是 2026-09-17 在 CentOS 7 上实测出来的）
// ---------------------------------------------------------------------------
// 节点的启动时机和面板无关：面板可能正在重启、还没起来、或者配置里地址暂时写错。
// 原来一失败就退出，systemd（Restart=on-failure）就会每 5 秒拉起一次 ——
// 实测 5 分钟重启了 16 次，日志被同一段错误刷满，而且 `systemctl status` 显示的是
// **failed**，用户看到的是"服务坏了"，而不是"还没连上面板"这个真实状态。
//
// 更糟的是：`deploy/install.sh` 里装了单元就 `systemctl is-active` 检查，
// 于是**全新机器上装节点包必然报"启动失败"** —— 因为 install.sh 刚生成的配置里
// panel_address 还是占位符 `PANEL_IP:9090`，用户甚至还没来得及改。
//
// 所以改成退避重试：连不上就一直试（这是节点的正常待机状态），
// 只有"面板明确拒绝"才是真的不该重试。
func (d *Daemon) registerWithRetry(client pb.DaemonServiceClient, req *pb.RegisterRequest) (*pb.RegisterResponse, error) {
	const (
		firstBackoff = 2 * time.Second
		maxBackoff   = 60 * time.Second
		callTimeout  = 15 * time.Second
	)
	backoff := firstBackoff
	for attempt := 1; ; attempt++ {
		// 每次尝试都要有超时：地址能解析但端口被墙时，没有 deadline 的 RPC
		// 会一直挂着，退避逻辑就永远不会执行到（表现和"卡死"一样）。
		ctx, cancel := context.WithTimeout(context.Background(), callTimeout)
		resp, err := client.Register(ctx, req)
		cancel()

		if err == nil {
			if !resp.Accepted {
				// 面板明确拒绝（例如未授权/版本不兼容）—— 重试没有意义，如实上报
				return nil, fmt.Errorf("Panel 拒绝注册: %s", resp.Message)
			}
			if attempt > 1 {
				d.log.Info("已连接上面板", "attempt", attempt)
			}
			return resp, nil
		}

		if attempt == 1 {
			d.log.Warn("暂时连不上面板，将持续重试（节点已就绪，等面板可达）",
				"panel", d.cfg.PanelAddress, "error", err)
		} else {
			d.log.Warn("仍未连上面板", "attempt", attempt,
				"next_retry_in", backoff.String(), "error", err)
		}

		select {
		case <-d.shutdown:
			return nil, ErrShutdown
		case <-time.After(backoff):
		}

		if backoff < maxBackoff {
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
		}
	}
}
