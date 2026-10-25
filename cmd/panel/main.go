// atl-panel 是 ATL-MCPanel 的中心管理面板入口。
package main

import (
	"flag"
	"fmt"
	"net"
	"os"
	"path/filepath"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	"github.com/ATLCNND/ATL-MCPanel/internal/common/config"
	"github.com/ATLCNND/ATL-MCPanel/internal/common/grpclimits"
	"github.com/ATLCNND/ATL-MCPanel/internal/common/logger"
	"github.com/ATLCNND/ATL-MCPanel/internal/common/version"
	"github.com/ATLCNND/ATL-MCPanel/internal/panel/auth"
	"github.com/ATLCNND/ATL-MCPanel/internal/panel/db"
	"github.com/ATLCNND/ATL-MCPanel/internal/panel/dbbackup"
	"github.com/ATLCNND/ATL-MCPanel/internal/panel/grpcapi"
	"github.com/ATLCNND/ATL-MCPanel/internal/panel/httpapi"
	"github.com/ATLCNND/ATL-MCPanel/internal/panel/nodemgr"
	"github.com/ATLCNND/ATL-MCPanel/internal/pki"
	pb "github.com/ATLCNND/ATL-MCPanel/internal/proto/mcpanel"
)

func main() {
	cfgPath := flag.String("config", "config.yaml", "配置文件路径")
	logLevel := flag.String("log-level", "info", "日志级别: debug/info/warn/error")
	showVersion := flag.Bool("version", false, "显示版本信息并退出")
	flag.Usage = func() {
		fmt.Fprint(os.Stderr, "ATL-MCPanel 面板\n\n用法:\n"+
			"  atl-panel [-config 配置文件] [-log-level 级别]   启动面板服务\n"+
			"  atl-panel -version                              显示版本\n"+
			"  atl-panel user <create|passwd|list> [...]       账号管理（含创建首个管理员）\n\n"+
			"启动服务的参数:\n")
		flag.PrintDefaults()
		fmt.Fprint(os.Stderr, "\n")
		userUsage()
	}
	flag.Parse()

	if *showVersion {
		fmt.Println(version.String())
		return
	}

	// 子命令分流。必须放在 flag.Parse **之后**：
	//
	//	flag 包遇到第一个非 flag 参数就停止解析，于是包一层"-config X user create"
	//	时全局 flag 已先被吃掉，剩下的 user/create 落在 flag.Args() 里。
	//
	// ⚠️ 早先的写法是判断 os.Args[1]=="user"，看起来能用，实际只在
	//	`atl-panel user create ...`（子命令在最前）时成立；而安装脚本与文档里
	//	写的是 `atl-panel -config /opt/mcpanel/config.yaml user create ...`
	//	—— 那种顺序下分流不生效，程序会**静默地再起一个面板服务**，
	//	而管理员一个也没建（实测踩到，且因为端口被占才露出线索）。
	//	下面这种写法对两种顺序都成立。
	if args := flag.Args(); len(args) > 0 {
		if args[0] == "user" {
			os.Exit(runUserCmd(*cfgPath, args[1:]))
		}
		// 多出来的位置参数一律报错而不是忽略：静默忽略正是上面那个 bug 的形态。
		fmt.Fprintf(os.Stderr, "未知的子命令: %s（只支持 user）\n\n", args[0])
		flag.Usage()
		os.Exit(2)
	}

	log := logger.New(*logLevel)

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		log.Error("加载配置失败", "error", err)
		os.Exit(1)
	}

	// 数据库
	d, err := db.Open(cfg.DB.Driver, cfg.DB.DSN)
	if err != nil {
		log.Error("初始化数据库失败", "error", err)
		os.Exit(1)
	}
	defer d.Close()

	// 认证密钥：显式配置过弱则拒绝启动；未配置则自动生成并持久化
	secret, generated, err := cfg.ResolveJWTSecret(filepath.Join(filepath.Dir(cfg.DB.DSN), "jwt.secret"))
	if err != nil {
		log.Error("JWT 密钥配置有误", "error", err)
		os.Exit(1)
	}
	if generated {
		log.Warn("未配置 auth.jwt_secret，已自动生成随机密钥并持久化",
			"path", filepath.Join(filepath.Dir(cfg.DB.DSN), "jwt.secret"))
	}
	authSvc := auth.NewService(secret)

	// 面板数据库自动备份（SQLite 使用 VACUUM INTO 生成一致性快照）
	var backupMgr *dbbackup.Manager
	if cfg.DB.BackupEnabledOr(true) {
		backupMgr = dbbackup.New(d, dbbackup.Options{
			Dir:      cfg.DB.BackupDir,
			Keep:     cfg.DB.BackupKeep,
			Interval: cfg.DB.BackupIntervalDuration(dbbackup.DefaultInterval),
		})
		backupMgr.Start()
		log.Info("面板数据库自动备份已启用", "dir", backupMgr.Dir(), "keep", backupMgr.Keep())
	}

	// PKI：加载或创建 CA（用于 Panel↔Daemon 的 mTLS 双向认证）
	ca, created, err := pki.LoadOrCreateCA(cfg.Server.PKIDir)
	if err != nil {
		log.Error("初始化 PKI 失败", "error", err)
		os.Exit(1)
	}
	if created {
		certPath, _ := ca.FilePaths()
		log.Info("已生成新的 CA 证书", "ca", certPath)
	}

	// 节点连接管理器（mTLS 开启时使用面板客户端证书连接各节点）
	var nodeMgr *nodemgr.Manager
	if cfg.Server.GRPCMTLS {
		nodeMgr, err = nodemgr.NewMTLSManager(d, ca)
		if err != nil {
			log.Error("初始化 mTLS 节点连接管理器失败", "error", err)
			os.Exit(1)
		}
	} else {
		nodeMgr = nodemgr.NewManager(d)
	}

	// gRPC server（Daemon 接入）
	// 与 Daemon 侧同样放开消息大小上限：面板未来可能接收节点上报的
	// 大块数据（如节点资源清单 / 升级包），保持两端一致省得再踩一次 4 MB 的坑。
	grpcOpts := []grpc.ServerOption{
		grpc.MaxRecvMsgSize(grpclimits.MaxMessageBytes),
		grpc.MaxSendMsgSize(grpclimits.MaxMessageBytes),
	}
	if cfg.Server.GRPCMTLS {
		tlsCfg, err := ca.ServerTLSConfig()
		if err != nil {
			log.Error("构造 gRPC mTLS 配置失败", "error", err)
			os.Exit(1)
		}
		grpcOpts = append(grpcOpts, grpc.Creds(credentials.NewTLS(tlsCfg)))
	} else if !config.IsLoopbackListen(cfg.Server.GRPCListen) && !cfg.Server.AllowInsecureGRPC {
		// 明文 gRPC + 对外监听 = 任何能连上这个端口的主机都可以伪造节点注册
		//（覆盖别的节点的 IP）与心跳。2026-10-01 安全审查后改为**拒绝启动**：
		// 要恢复旧行为必须显式写 server.allow_insecure_grpc: true。
		log.Error("拒绝以明文 gRPC 对外提供服务",
			"grpc_listen", cfg.Server.GRPCListen,
			"hint", "开启 server.grpc_mtls，或把 grpc_listen 改成 127.0.0.1:9090，或显式设置 server.allow_insecure_grpc: true")
		os.Exit(1)
	} else {
		log.Warn("gRPC 未启用 mTLS（server.grpc_mtls=false）",
			"grpc_listen", cfg.Server.GRPCListen,
			"note", "监听范围已限制为回环或已显式允许明文")
	}
	grpcSrv := grpc.NewServer(grpcOpts...)
	// 启用 mTLS 时，Register/Ping 必须校验"对端证书身份 == 它声称的节点名"，
	// 否则任何持有 CA 签发证书的节点都能冒名顶替另一个节点。
	pb.RegisterDaemonServiceServer(grpcSrv, grpcapi.NewServer(d, log, cfg.Server.GRPCMTLS))

	// 在独立 goroutine 启动 gRPC
	lis, err := net.Listen("tcp", cfg.Server.GRPCListen)
	if err != nil {
		log.Error("gRPC 监听失败", "error", err, "addr", cfg.Server.GRPCListen)
		os.Exit(1)
	}
	go func() {
		log.Info("gRPC 服务启动", "addr", cfg.Server.GRPCListen, "mtls", cfg.Server.GRPCMTLS)
		if err := grpcSrv.Serve(lis); err != nil {
			log.Error("gRPC 退出", "error", err)
		}
	}()

	// HTTP API（含前端静态资源托管 + 面板自身穿透管理）
	api := httpapi.NewServer(d, authSvc, nodeMgr, httpapi.Options{
		DataDir:     filepath.Dir(cfg.DB.DSN),
		WebDir:      cfg.Server.WebDir,
		PanelName:   cfg.Server.PanelName,
		ListenAddr:  cfg.Server.Listen,
		TLSListen:   cfg.Server.TLSListen,
		ExternalURL: cfg.Server.ExternalURL,
		PanelFrpDir: cfg.Server.PanelFrpDir,
		TrustProxy:  cfg.Server.TrustProxy,
		// 自定义安全入口：只在 /<entry>/ 下提供服务（见 config.ServerConfig.SecurityEntry）
		SecurityEntry: cfg.Server.SecurityEntry,
		CA:          ca,
		GRPCMTLS:    cfg.Server.GRPCMTLS,
		Backup:      backupMgr,

		DaemonBinary:   cfg.Server.DaemonBinary,
		GRPCListen:     cfg.Server.GRPCListen,
		GRPCPublicAddr: cfg.Server.GRPCPublicAddress,

		RemoteInstallDir:  cfg.Server.RemoteInstallDir,
		DaemonServiceName: cfg.Server.DaemonServiceName,
		DaemonGRPCListen:  cfg.Server.DaemonGRPCListen,
		Logger:            log,
		// 第三方日志分析（LogShare.CN）：未配置时 Enabled 为 nil → 默认关闭
		LogShare: cfg.LogShare,
	})
	handler := api.Handler()
	tlsReady := cfg.Server.TLSCert != "" && cfg.Server.TLSKey != ""

	// ⚠️ 未配置证书时的部署提醒（**非必须**，2026-10-06 用户要求写进来）。
	//
	// 为什么值得提醒：面板的**控制台**走 WebSocket。用 HTTP 访问面板时一切正常；
	// 但如果部署者自己在前端挂了 HTTPS（或用了自签证书），
	// 浏览器会**允许页面"继续访问"、却直接拒绝 WebSocket**（没有"继续访问"这个选项），
	// 表现为"控制台一直显示连接错误"，而页面上其它功能都正常 ——
	// 这个现象很难自己定位（用户实测时浏览器控制台刷了 281 条
	// `ERR_CERT_AUTHORITY_INVALID`，而界面上只有一句笼统的"连接错误"）。
	//
	// 所以这里只说清两件事：控制台需要受信任的证书；以及不配也完全能用。
	// 刻意**不**强制、不报警告级别 —— 内网自用 HTTP 是完全合理的部署方式。
	if !tlsReady {
		log.Info("未配置 TLS 证书：面板将以 HTTP 提供服务（内网自用没问题）。" +
			"若你打算用 HTTPS 访问，请使用「受信任的」证书 —— 自签证书会导致控制台 " +
			"WebSocket 被浏览器拒绝（页面本身仍能通过「继续访问」打开），" +
			"表现为「控制台一直显示连接错误」而其它功能都正常。")
	}

	// 双端口模式：HTTPS 监听 tls_listen，HTTP 监听 listen
	if cfg.Server.TLSListen != "" && tlsReady {
		go func() {
			log.Info("HTTPS 服务启动", "addr", cfg.Server.TLSListen, "cert", cfg.Server.TLSCert)
			if err := httpapi.ListenAndServe(cfg.Server.TLSListen, handler, cfg.Server.TLSCert, cfg.Server.TLSKey); err != nil {
				log.Error("HTTPS 服务退出", "error", err)
			}
		}()
		log.Info("Panel 启动",
			"http", cfg.Server.Listen,
			"https", cfg.Server.TLSListen,
			"grpc", cfg.Server.GRPCListen,
			"web_dir", cfg.Server.WebDir)
		if err := httpapi.ListenAndServe(cfg.Server.Listen, handler, "", ""); err != nil {
			log.Error("HTTP 服务退出", "error", err)
			os.Exit(1)
		}
		return
	}

	// 单端口模式：仅 tls_cert/tls_key 时，listen 直接跑 HTTPS
	scheme := "http"
	if tlsReady {
		scheme = "https"
	}
	log.Info("Panel 启动",
		"scheme", scheme,
		"listen", cfg.Server.Listen,
		"grpc", cfg.Server.GRPCListen,
		"web_dir", cfg.Server.WebDir)

	if err := httpapi.ListenAndServe(cfg.Server.Listen, handler, cfg.Server.TLSCert, cfg.Server.TLSKey); err != nil {
		log.Error("服务退出", "error", err)
		os.Exit(1)
	}
}
