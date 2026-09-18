// dsh-panel 是 ATL-MCPanel 的中心管理面板入口。
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
	flag.Parse()

	if *showVersion {
		fmt.Println(version.String())
		return
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
	} else {
		log.Warn("gRPC 未启用 mTLS（server.grpc_mtls=false），建议生产环境开启")
	}
	grpcSrv := grpc.NewServer(grpcOpts...)
	pb.RegisterDaemonServiceServer(grpcSrv, grpcapi.NewServer(d, log))

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
		ListenAddr:  cfg.Server.Listen,
		TLSListen:   cfg.Server.TLSListen,
		ExternalURL: cfg.Server.ExternalURL,
		PanelFrpDir: cfg.Server.PanelFrpDir,
		TrustProxy:  cfg.Server.TrustProxy,
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
