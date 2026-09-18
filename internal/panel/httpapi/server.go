// Package httpapi 是 Panel 的 HTTP API 层。
package httpapi

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/ATLCNND/ATL-MCPanel/internal/common/config"
	"github.com/ATLCNND/ATL-MCPanel/internal/common/logger"
	"github.com/ATLCNND/ATL-MCPanel/internal/common/portguard"
	"github.com/ATLCNND/ATL-MCPanel/internal/common/version"
	"github.com/ATLCNND/ATL-MCPanel/internal/frp"
	"github.com/ATLCNND/ATL-MCPanel/internal/panel/auth"
	"github.com/ATLCNND/ATL-MCPanel/internal/panel/dbbackup"
	"github.com/ATLCNND/ATL-MCPanel/internal/panel/logshare"
	"github.com/ATLCNND/ATL-MCPanel/internal/panel/nodemgr"
	"github.com/ATLCNND/ATL-MCPanel/internal/panel/scheduler"
	"github.com/ATLCNND/ATL-MCPanel/internal/pki"
)

// Options HTTP API 服务的附加配置。
type Options struct {
	// DataDir 面板数据目录（数据库所在目录）；头像等附件放在其下的 avatars/。
	// 为空时退回当前工作目录下的 data/。
	DataDir           string
	WebDir            string            // 前端静态资源目录（空则不托管）
	ListenAddr        string            // 面板 HTTP 监听地址（用于推导穿透的本地端口）
	TLSListen         string            // 面板 HTTPS 监听地址（配置后穿透优先转发到此端口）
	ExternalURL       string            // 面板外部访问地址
	PanelFrpDir       string            // 面板自身穿透工作目录
	TrustProxy        bool              // 信任 X-Forwarded-For / X-Real-IP（位于代理后时开启）
	CA                *pki.CA           // mTLS 证书颁发机构（nil 表示未启用）
	GRPCMTLS          bool              // gRPC 是否启用 mTLS
	Backup            *dbbackup.Manager // 面板数据库备份管理器（nil 表示未启用）
	DaemonBinary      string            // 节点部署下发的 Daemon 二进制路径
	GRPCListen        string            // 面板 gRPC 监听地址
	GRPCPublicAddr    string            // 节点连接面板 gRPC 的地址
	RemoteInstallDir  string            // 节点安装目录
	DaemonServiceName string            // 节点 systemd 单元名
	DaemonGRPCListen  string            // 节点 Daemon gRPC 监听
	Logger            *logger.Logger    // 日志

	// LogShare 第三方日志分析接入配置（见 internal/panel/logshare）。
	// 面板启动时由 config 传入；未配置时功能关闭。
	LogShare config.LogShareConfig
}

// Server HTTP API 服务。
type Server struct {
	db     *sql.DB
	auth   *auth.Service
	nodes  *nodemgr.Manager
	webDir string // 前端静态资源目录

	listenAddr  string
	tlsListen   string
	externalURL string
	panelFrp    *frp.Manager
	panelFrpDir string
	avatarDir   string // 头像存储目录（与数据库同级，便于随面板数据一起备份）
	logger      *logger.Logger
	trustProxy  bool              // 是否信任 X-Forwarded-For（面板位于代理之后时开启）
	userLimiter *loginLimiter     // 按用户名的登录限流
	ipLimiter   *loginLimiter     // 按来源 IP 的登录限流（阈值更宽松）
	ca          *pki.CA           // mTLS 证书颁发机构
	grpcMTLS    bool              // gRPC 是否已启用 mTLS
	backup      *dbbackup.Manager // 面板数据库备份

	daemonBinary      string               // 节点部署时下发的 Daemon 二进制路径
	grpcPublicAddr    string               // 节点连接面板 gRPC 的地址
	listenAddrOfGRPC  string               // 面板 gRPC 监听地址
	remoteDir         string               // 节点安装目录
	daemonServiceName string               // 节点 systemd 单元名
	daemonGRPCListen  string               // 节点 Daemon gRPC 监听
	sched             *scheduler.Scheduler // 后台调度器（定时备份 + 告警）

	// logShare 第三方日志分析客户端（配置未启用时为 nil）。
	logShare    *logshare.Client
	logShareCfg config.LogShareConfig
	logShareVer string // 上报给对方的 source（atl-mcpanel/<版本>）

	// aiRuns 正在后台跑的 AI 分析：分析不绑在浏览器连接上，
	// 用户切页/关页后仍会跑完并落库（见 logshare.go 的 aiRunHub）。
	aiRuns aiRunHub
}

// LogShareEnabled 第三方日志分析是否启用。
//
// 默认**关闭**（config.LogShareConfig 的说明）：多租户面板不该默认把租户的
// 日志（含玩家名与聊天内容）送到第三方。管理员显式打开后前端才显示入口。
func (s *Server) LogShareEnabled() bool {
	return s.logShare != nil && s.logShareCfg.EnabledOr(false)
}

// protectedLocalPorts 面板自己占用的端口（穿透目标不许指向它们）。
//
// 默认清单（internal/common/portguard）只能覆盖默认端口，而部署时端口经常被改，
// 所以这里把**实际配置**里监听的面板端口也加进去 —— 否则改过端口的面板
// 反而把自己暴露出去，正好是这条检查要防的事。
func (s *Server) protectedLocalPorts() map[int]string {
	m := map[int]string{}
	portguard.Set(m, "面板自身监听端口（穿透目标指向它会把管理界面暴露到公网）",
		s.listenAddr, s.tlsListen, s.listenAddrOfGRPC)
	return m
}

// NewServer 创建 HTTP API 服务。
func NewServer(d *sql.DB, a *auth.Service, n *nodemgr.Manager, opts Options) *Server {
	log := opts.Logger
	if log == nil {
		log = logger.New("info")
	}
	s := &Server{
		db:          d,
		auth:        a,
		nodes:       n,
		webDir:      opts.WebDir,
		listenAddr:  opts.ListenAddr,
		tlsListen:   opts.TLSListen,
		externalURL: opts.ExternalURL,
		panelFrpDir: opts.PanelFrpDir,
		avatarDir:   defaultAvatarDir(opts.DataDir),
		panelFrp:    frp.NewManager(""),
		logger:      log,
		trustProxy:  opts.TrustProxy,
		userLimiter: newLoginLimiter(userLimiterCfg),
		ipLimiter:   newLoginLimiter(ipLimiterCfg),
		ca:          opts.CA,
		grpcMTLS:    opts.GRPCMTLS,
		backup:      opts.Backup,

		daemonBinary:      opts.DaemonBinary,
		grpcPublicAddr:    opts.GRPCPublicAddr,
		listenAddrOfGRPC:  opts.GRPCListen,
		remoteDir:         opts.RemoteInstallDir,
		daemonServiceName: opts.DaemonServiceName,
		daemonGRPCListen:  opts.DaemonGRPCListen,
		logShareCfg:       opts.LogShare,
		logShareVer:       "atl-mcpanel/" + version.Short(),
	}
	// 日志分析客户端：只有显式启用时才创建（nil 表示功能关闭）
	if opts.LogShare.EnabledOr(false) {
		s.logShare = logshare.New(opts.LogShare.Endpoint,
			time.Duration(opts.LogShare.TimeoutSeconds)*time.Second)
		log.Info("第三方日志分析已启用（LogShare.CN）",
			"endpoint", opts.LogShare.Endpoint,
			"max_upload_mb", opts.LogShare.MaxUploadBytes>>20)
	}
	// 启动时恢复面板自身穿透
	s.initPanelTunnel()
	// 定期清理限流记录
	s.startLimiterJanitor()
	// 后台调度器（定时备份 + 健康告警）
	s.StartScheduler()
	return s
}

// startLimiterJanitor 周期性清理登录限流中的过期条目。
func (s *Server) startLimiterJanitor() {
	go func() {
		t := time.NewTicker(10 * time.Minute)
		defer t.Stop()
		for range t.C {
			n := s.userLimiter.Cleanup() + s.ipLimiter.Cleanup()
			if n > 0 {
				s.logger.Debug("清理登录限流记录", "removed", n)
			}
		}
	}()
}

// currentURL 返回面板当前的外部访问地址（用于前端展示）。
func (s *Server) currentURL() string {
	if s.externalURL != "" {
		return s.externalURL
	}
	return "http://127.0.0.1" + s.listenAddr
}

// Handler 返回路由处理器。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", s.handleHealth)
	mux.HandleFunc("POST /api/auth/login", s.handleLogin)

	// 账号管理
	// 注册由管理员控制，但「首个用户」需允许匿名自助创建为管理员 → 使用可选鉴权
	mux.HandleFunc("POST /api/users", s.optionalAuth(s.handleRegister))
	mux.HandleFunc("GET /api/users", s.requireAdmin(s.handleListUsers))
	mux.HandleFunc("PUT /api/users/{id}", s.requireAdmin(s.handleSetUserRole))
	// 改用户名：**不是** requireAdmin —— 本人可以改自己（所以鉴权在 handler 里做）。
	// 用户名已不是标识（标识是 UID），改名不再影响任何授权关联。
	mux.HandleFunc("PUT /api/users/{id}/username", s.requireAuth(s.handleSetUsername))
	mux.HandleFunc("DELETE /api/users/{id}", s.requireAdmin(s.handleDeleteUser))
	mux.HandleFunc("POST /api/auth/change-password", s.requireAuth(s.handleChangePassword))

	// 当前用户的权限视图（供前端做 UI 控制）
	mux.HandleFunc("GET /api/my/instances", s.requireAuth(s.handleMyPermissions))
	// 我可以在哪些节点上创建实例（节点管理员 / 总管理员）
	mux.HandleFunc("GET /api/my/nodes", s.requireAuth(s.handleMyNodes))

	// 用户资料与头像（头像需管理员审核后才对外可见）
	mux.HandleFunc("GET /api/auth/me", s.requireAuth(s.handleMe))
	mux.HandleFunc("POST /api/my/avatar", s.requireAuth(s.handleUploadAvatar))
	mux.HandleFunc("DELETE /api/my/avatar", s.requireAuth(s.handleDeleteMyAvatar))
	mux.HandleFunc("GET /api/avatars/{id}", s.handleGetAvatar) // 公开：仅返回已审核通过的
	mux.HandleFunc("GET /api/admin/avatars", s.requireAuth(s.handleListAvatars))
	// 待审头像预览：由 <img src> 直接发起，浏览器**无法**附加 Authorization 头，
	// 所以必须允许 ?token= 传令牌 —— 否则管理员看到的永远是裂图。
	// 与实例文件下载同属一类问题（见 fileops.go 的 handleDownloadFile）。
	mux.HandleFunc("GET /api/admin/avatars/{id}/raw", s.requireAuthQuery(s.handleGetAvatarRaw))
	mux.HandleFunc("POST /api/admin/avatars/{id}/review", s.requireAuth(s.handleReviewAvatar))

	// 公告与帮助文档：**读给所有登录用户，写只给总管理员**。
	// 不做"已读/未读"（那要再加一张已读表），先只做展示 ——
	// 加了未读标记很容易变成"红点永远消不掉"的噪音源。
	mux.HandleFunc("GET /api/announcements", s.requireAuth(s.handleListAnnouncements))
	mux.HandleFunc("POST /api/announcements", s.requireAuth(s.handleCreateAnnouncement))
	mux.HandleFunc("PUT /api/announcements/{id}", s.requireAuth(s.handleUpdateAnnouncement))
	mux.HandleFunc("DELETE /api/announcements/{id}", s.requireAuth(s.handleDeleteAnnouncement))
	mux.HandleFunc("GET /api/help", s.requireAuth(s.handleGetHelp))
	mux.HandleFunc("PUT /api/help", s.requireAuth(s.handleSaveHelp))

	// 实例管理
	// 创建：requireAuth + handler 内按节点判权（总管理员 / 该节点的节点管理员）
	mux.HandleFunc("POST /api/instances", s.requireAuth(s.handleCreateInstance))
	mux.HandleFunc("GET /api/instances", s.requireAuth(s.handleListInstances))
	// 实例改名：改的是**显示名**（面板数据库里的 instances.name），
	// 不影响实例 ID —— ID 是目录名，也是所有关联（授权、端口、许可证）的钥匙。
	mux.HandleFunc("PUT /api/instances/{id}", s.requireAuth(s.handleRenameInstance))
	// 实例图标：由 <img src> 直接发起 → 必须允许 ?token=（同文件下载/头像预览）
	mux.HandleFunc("GET /api/instances/{id}/icon", s.requireAuthQuery(s.handleInstanceIcon))
	mux.HandleFunc("POST /api/instances/{id}/start", s.requireAuth(func(w http.ResponseWriter, r *http.Request) { s.handleInstanceAction(w, r, "start") }))
	mux.HandleFunc("POST /api/instances/{id}/stop", s.requireAuth(func(w http.ResponseWriter, r *http.Request) { s.handleInstanceAction(w, r, "stop") }))
	mux.HandleFunc("POST /api/instances/{id}/restart", s.requireAuth(func(w http.ResponseWriter, r *http.Request) { s.handleInstanceAction(w, r, "restart") }))
	mux.HandleFunc("DELETE /api/instances/{id}", s.requireAuth(func(w http.ResponseWriter, r *http.Request) { s.handleInstanceAction(w, r, "delete") }))
	mux.HandleFunc("POST /api/instances/{id}/kill", s.requireAuth(func(w http.ResponseWriter, r *http.Request) { s.handleInstanceAction(w, r, "kill") }))
	// 到期时间（总管理员 / 该节点的节点管理员）
	mux.HandleFunc("POST /api/instances/{id}/expiry", s.requireAuth(s.handleSetExpiry))

	// 实例公网端口（**所有能看到该实例的用户**可见；改/删需管理员或节点用户）
	mux.HandleFunc("GET /api/instances/{id}/ports", s.requireAuth(s.handleListInstancePorts))
	// 第三方日志分析（LogShare.CN）。默认关闭，见 config.LogShareConfig。
	// 分析（上传）与删除要 owner；列表与取结论 collab 即可（与看控制台同级）。
	mux.HandleFunc("GET /api/instances/{id}/logshare/files", s.requireAuth(s.handleLogShareFiles))
	mux.HandleFunc("GET /api/instances/{id}/logshare", s.requireAuth(s.handleLogShareHistory))
	mux.HandleFunc("POST /api/instances/{id}/logshare/analyse", s.requireAuth(s.handleLogShareAnalyse))
	mux.HandleFunc("GET /api/instances/{id}/logshare/ai/{logshare_id}", s.requireAuth(s.handleLogShareAI))
	mux.HandleFunc("DELETE /api/instances/{id}/logshare/{logshare_id}", s.requireAuth(s.handleLogShareDelete))
	mux.HandleFunc("POST /api/instances/{id}/ports", s.requireAuth(s.handleAddInstancePort))
	mux.HandleFunc("POST /api/instances/{id}/ports/{tunnel_id}", s.requireAuth(s.handleUpdateInstancePort))
	mux.HandleFunc("DELETE /api/instances/{id}/ports/{tunnel_id}", s.requireAuth(s.handleDeleteInstancePort))

	// 节点用户授权与端口配额（仅总管理员）
	mux.HandleFunc("GET /api/node-users", s.requireAuth(s.handleListNodeUsers))
	mux.HandleFunc("POST /api/node-users", s.requireAuth(s.handleGrantNodeUser))
	mux.HandleFunc("DELETE /api/node-users", s.requireAuth(s.handleRevokeNodeUser))
	mux.HandleFunc("GET /api/node-users/ports", s.requireAuth(s.handleListPortGrants))
	mux.HandleFunc("POST /api/node-users/ports", s.requireAuth(s.handleSetPortGrant))
	mux.HandleFunc("DELETE /api/node-users/ports", s.requireAuth(s.handleDeletePortGrant))
	// 我自己的端口配额（创建实例时选线路与数量）
	mux.HandleFunc("GET /api/my/ports", s.requireAuth(s.handleMyPorts))

	// 节点共享资源（管理员上传，所有登录用户可读）
	mux.HandleFunc("GET /api/nodes/{id}/resources", s.requireAuth(s.handleListNodeResources))
	mux.HandleFunc("POST /api/nodes/{id}/resources", s.requireAuth(s.handleUploadNodeResource))
	mux.HandleFunc("DELETE /api/nodes/{id}/resources", s.requireAuth(s.handleDeleteNodeResource))
	// 节点上实际安装的 JDK（创建实例时选版本用）
	mux.HandleFunc("GET /api/nodes/{id}/java", s.requireAuth(s.handleListNodeJava))

	// 实例授权管理
	mux.HandleFunc("GET /api/instances/{id}/assignments", s.requireAuth(s.handleListAssignments))
	mux.HandleFunc("POST /api/instances/{id}/assignments", s.requireAuth(s.handleGrantAssignment))
	mux.HandleFunc("DELETE /api/instances/{id}/assignments", s.requireAuth(s.handleRevokeAssignment))

	// 控制台（WS 自行解析 token，见 console.go）
	mux.HandleFunc("GET /ws/console/{id}", s.handleConsole)

	// 文件管理
	mux.HandleFunc("GET /api/instances/{id}/files", s.requireAuth(s.handleListFiles))
	mux.HandleFunc("GET /api/instances/{id}/files/search", s.requireAuth(s.handleSearchFiles))
	mux.HandleFunc("GET /api/instances/{id}/file", s.requireAuth(s.handleReadFile))
	mux.HandleFunc("POST /api/instances/{id}/file", s.requireAuth(s.handleWriteFile))
	mux.HandleFunc("DELETE /api/instances/{id}/file", s.requireAuth(s.handleDeleteFile))
	mux.HandleFunc("POST /api/instances/{id}/mkdir", s.requireAuth(s.handleMkdir))
	mux.HandleFunc("POST /api/instances/{id}/file/rename", s.requireAuth(s.handleRenameFile))
	mux.HandleFunc("POST /api/instances/{id}/file/copy", s.requireAuth(s.handleCopyFile))
	// 下载：浏览器直接发起，无法带自定义请求头 → 允许 ?token= 传令牌
	mux.HandleFunc("GET /api/instances/{id}/download", s.requireAuthQuery(s.handleDownloadFile))

	// 排队任务（压缩 / 解压；走节点公共资源）
	mux.HandleFunc("GET /api/instances/{id}/jobs", s.requireAuth(s.handleListJobs))
	mux.HandleFunc("POST /api/instances/{id}/jobs", s.requireAuth(s.handleCreateJob))
	mux.HandleFunc("DELETE /api/jobs/{job_id}", s.requireAuth(s.handleCancelJob))

	// 备份/回滚
	mux.HandleFunc("GET /api/instances/{id}/backups", s.requireAuth(s.handleListBackups))
	mux.HandleFunc("POST /api/instances/{id}/backups", s.requireAuth(s.handleCreateBackup))
	mux.HandleFunc("DELETE /api/instances/{id}/backups", s.requireAuth(s.handleDeleteBackup))
	mux.HandleFunc("POST /api/instances/{id}/restore", s.requireAuth(s.handleRestoreBackup))

	// 监控
	// 实例运行数据（运行时长 / 启停次数 / 磁盘用量 / 对外域名）
	mux.HandleFunc("GET /api/instances/{id}/runtime", s.requireAuth(s.handleInstanceRuntime))

	// 指标历史（统计页）
	mux.HandleFunc("GET /api/instances/{id}/stats", s.requireAuth(s.handleInstanceStats))

	mux.HandleFunc("GET /api/instances/{id}/metrics", s.requireAuth(s.handleGetMetrics))

	// 玩家管理（白名单 / OP / 封禁）
	mux.HandleFunc("GET /api/instances/{id}/players", s.requireAuth(s.handleListPlayers))
	mux.HandleFunc("GET /api/instances/{id}/players/all", s.requireAuth(s.handlePlayerOverview))
	mux.HandleFunc("POST /api/instances/{id}/players", s.requireAuth(s.handleAddPlayer))
	mux.HandleFunc("DELETE /api/instances/{id}/players", s.requireAuth(s.handleRemovePlayer))
	// OP 等级（1~4）：/op 命令表达不了等级，只能改 ops.json 的 level 字段
	mux.HandleFunc("POST /api/instances/{id}/players/op-level", s.requireAuth(s.handleSetOpLevel))
	mux.HandleFunc("POST /api/instances/{id}/whitelist", s.requireAuth(s.handleSetWhitelistSwitch))

	// 定时指令任务（开机 / 关机 / 重启 / 游戏指令；实例 owner 即可管理）
	mux.HandleFunc("GET /api/instances/{id}/tasks", s.requireAuth(s.handleListTasks))
	mux.HandleFunc("POST /api/instances/{id}/tasks", s.requireAuth(s.handleCreateTask))
	mux.HandleFunc("PUT /api/tasks/{id}", s.requireAuth(s.handleUpdateTask))
	mux.HandleFunc("DELETE /api/tasks/{id}", s.requireAuth(s.handleDeleteTask))
	mux.HandleFunc("POST /api/tasks/{id}/run", s.requireAuth(s.handleRunTask))

	// 核心 jar 管理
	mux.HandleFunc("GET /api/instances/{id}/jars", s.requireAuth(s.handleListJars))
	mux.HandleFunc("POST /api/instances/{id}/jars", s.requireAuth(s.handleUploadJar))
	mux.HandleFunc("POST /api/instances/{id}/jar", s.requireAuth(s.handleSetJar))

	// 启动脚本（start.sh）
	mux.HandleFunc("GET /api/instances/{id}/start-script", s.requireAuth(s.handleGetStartScript))
	mux.HandleFunc("POST /api/instances/{id}/start-script", s.requireAuth(s.handleSetStartScript))

	// 容器化隔离（按实例开关）：
	//   读 —— 协作者/只读成员都能看到"这个实例到底有没有隔离"（运行信息）
	//   写 —— **仅总管理员**。理由见 handleSetContainer：被隔离的一方
	//         （实例 owner）能执行任意命令，若能自己关掉隔离，隔离就不成立。
	mux.HandleFunc("GET /api/instances/{id}/container", s.requireAuth(s.handleGetContainer))
	mux.HandleFunc("PUT /api/instances/{id}/container", s.requireAdmin(s.handleSetContainer))

	// 审计日志（仅 admin）
	// 审计日志仅管理员可见
	mux.HandleFunc("GET /api/audit-logs", s.requireAdmin(s.handleListAuditLogs))

	// 穿透管理（frps 服务器 + 隧道；仅 admin）
	mux.HandleFunc("GET /api/frps", s.requireAuth(s.handleListFrps))
	mux.HandleFunc("POST /api/frps", s.requireAuth(s.handleCreateFrps))
	// 改线路（主要是「对外域名」）：host / bind_port / token 不在改动范围内 ——
	// 它们一动，该线路上已下发的所有 frpc 配置都得重写，属于"删掉重建"更安全
	mux.HandleFunc("PUT /api/frps/{id}", s.requireAuth(s.handleUpdateFrps))
	// 线路自检：保存前可测（传表单值），也可测已保存的线路
	mux.HandleFunc("POST /api/frps/test", s.requireAuth(s.handleTestFrps))
	mux.HandleFunc("POST /api/frps/{id}/test", s.requireAuth(s.handleTestFrpsSaved))
	mux.HandleFunc("DELETE /api/frps/{id}", s.requireAuth(s.handleDeleteFrps))
	mux.HandleFunc("GET /api/tunnels", s.requireAuth(s.handleListTunnels))
	mux.HandleFunc("POST /api/tunnels", s.requireAuth(s.handleCreateTunnel))
	mux.HandleFunc("DELETE /api/tunnels/{id}", s.requireAuth(s.handleDeleteTunnel))
	mux.HandleFunc("POST /api/tunnels/{id}/reapply", s.requireAuth(s.handleReapplyTunnel))

	// 面板自身穿透（仅 admin）
	mux.HandleFunc("GET /api/panel-tunnel", s.requireAuth(s.handleGetPanelTunnel))
	mux.HandleFunc("POST /api/panel-tunnel", s.requireAuth(s.handleSetPanelTunnel))

	// 节点证书 / PKI（仅 admin）
	mux.HandleFunc("GET /api/pki", s.requireAuth(s.handleGetPKIInfo))
	mux.HandleFunc("GET /api/nodes/{id}/cert", s.requireAuth(s.handleGetNodeCert))
	mux.HandleFunc("POST /api/nodes/cert", s.requireAuth(s.handleIssueNodeCert))

	// 节点监控（**所有登录用户可见**，只读；不含 SSH 凭据等敏感字段）
	mux.HandleFunc("GET /api/monitor/nodes", s.requireAuth(s.handleMonitorNodes))
	mux.HandleFunc("GET /api/monitor/instances", s.requireAuth(s.handleMonitorInstances))

	// 节点管理（仅 admin）
	mux.HandleFunc("GET /api/nodes", s.requireAuth(s.handleListNodes))
	mux.HandleFunc("POST /api/nodes", s.requireAuth(s.handleCreateNode))
	mux.HandleFunc("PUT /api/nodes/{id}", s.requireAuth(s.handleUpdateNode))
	mux.HandleFunc("DELETE /api/nodes/{id}", s.requireAuth(s.handleDeleteNode))
	mux.HandleFunc("POST /api/nodes/{id}/probe", s.requireAuth(s.handleProbeNode))
	mux.HandleFunc("POST /api/nodes/{id}/deploy", s.requireAuth(s.handleDeployNode))
	mux.HandleFunc("POST /api/nodes/{id}/daemon/restart", s.requireAuth(s.handleRestartDaemon))
	mux.HandleFunc("GET /api/nodes/{id}/daemon/logs", s.requireAuth(s.handleDaemonLogs))

	// 面板数据库备份（仅 admin）
	mux.HandleFunc("GET /api/panel-backups", s.requireAuth(s.handleListPanelBackups))
	mux.HandleFunc("POST /api/panel-backups", s.requireAuth(s.handleCreatePanelBackup))

	// 备份保留策略（管理员配置的「备份组」）
	mux.HandleFunc("GET /api/backup-policies", s.requireAuth(s.handleListBackupPolicies))
	mux.HandleFunc("POST /api/backup-policies", s.requireAuth(s.handleSaveBackupPolicy))
	mux.HandleFunc("DELETE /api/backup-policies/{id}", s.requireAuth(s.handleDeleteBackupPolicy))

	// 定时备份计划
	mux.HandleFunc("GET /api/instances/{id}/schedule", s.requireAuth(s.handleGetSchedule))
	mux.HandleFunc("POST /api/instances/{id}/schedule", s.requireAuth(s.handleSetSchedule))

	// 告警（仅 admin）
	mux.HandleFunc("GET /api/alerts", s.requireAuth(s.handleListAlerts))
	mux.HandleFunc("POST /api/alerts/{id}/resolve", s.requireAuth(s.handleResolveAlert))

	// 前端静态资源（SPA 回退）；作为兜底路由，必须最后注册
	mux.HandleFunc("/", s.handleStatic)

	return securityHeaders(mux)
}

// securityHeaders 给所有响应补上安全响应头。
//
// 逐条说明为什么是这些值（以及为什么有的必须条件性下发）：
//
//   - X-Content-Type-Options: nosniff —— 阻止浏览器"猜"类型。上传的
//     server-icon / 头像 / 备份文件即使被当成 HTML 返回，也不会被当页面执行。
//
//   - X-Frame-Options: DENY + CSP frame-ancestors 'none' —— 防点击劫持。
//     面板上"停止实例/删除实例"这类按钮点错一次就是真实损失，
//     不该允许被别人用 iframe 套住骗点击。两条都发是因为老浏览器只认前者。
//
//   - Referrer-Policy: no-referrer —— 面板有把 JWT 放在查询串里的接口
//     （下载、图标，浏览器无法给这些请求加请求头，只能用 ?token=）。
//     只要页面上出现一个外链、或用户点了跳转，Referer 就会把令牌带出去。
//     一条 no-referrer 直接堵死这条外泄路径。
//
//   - Content-Security-Policy —— 限制脚本/外链来源。这里**没有**用最严的写法：
//     React 会写内联 style 属性（进度条宽度之类），CSP 会拦掉，
//     所以 style-src 必须留 'unsafe-inline'；connect-src 放开 ws/wss
//     是为了控制台 WebSocket（同源）。script-src 保持 'self'，
//     这才是真正挡住 XSS 外带的那一条。
//
//   - Strict-Transport-Security —— **只在本次请求确实走 TLS 时才发**。
//     这是必须的：面板默认也支持纯 HTTP（内网/反代后端），
//     而 HSTS 一旦被浏览器记住，之后用 http:// 访问会被强制升级到 https，
//     在没有证书的部署上等于把面板彻底锁死 —— 一个响应头把自己关在门外。
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy",
			"default-src 'self'; "+
				"script-src 'self'; "+
				"style-src 'self' 'unsafe-inline'; "+
				"img-src 'self' data: blob:; "+
				"font-src 'self' data:; "+
				"connect-src 'self' ws: wss:; "+
				"object-src 'none'; "+
				"base-uri 'none'; "+
				"frame-ancestors 'none'")
		if r.TLS != nil {
			h.Set("Strict-Transport-Security", "max-age=31536000")
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "time": time.Now().Format(time.RFC3339)})
}

type loginReq struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

// dummyPasswordHash 一个固定的、永远不可能匹配的 bcrypt 哈希。
//
// 用途只有一个：用户不存在时也走一遍密码校验，让"用户名不存在"与"密码错误"
// 的响应时间处在同一量级。文案早就统一了（都是"用户名或密码错误"），
// 但时间不统一等于没统一 —— 攻击者用响应时间就能枚举出哪些用户名存在，
// 而"知道 admin 存在"正是后续定向爆破与账号锁定 DoS 的第一步。
//
// 这个值必须是**合法**的 bcrypt 哈希：写错了的话 CheckPassword 会在解析阶段
// 立刻返回，白算一趟都省了，等于什么都没做（runtime_test.go 里有断言守着这一点）。
// 下面是 bcrypt 的公开测试向量（"password" 在 cost=10 下的哈希）。
const dummyPasswordHash = "$2a$10$N9qo8uLOickgx2ZMRZoMyeIjZAgcfl7p92ldGxad68LJZdL17lhWy"

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req loginReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "无效的请求体")
		return
	}

	// ---- 限流的两条维度，职责刻意不同 ----
	//
	//  · **来源 IP**：硬限流，放在验密码**之前**。它既是防撞库的第一道闸，
	//    也顺带护住 CPU（bcrypt 很贵，不能让单机把面板算爆）。
	//  · **用户名**：只限制"猜错"的次数，**绝不阻止正确密码登录**（见下）。
	//
	// 原来的实现在验密码之前就按用户名硬锁，于是任何人只要知道用户名、
	// 每隔一会儿发几个错密码，就能让这个账号（包括 admin）持续登录不了 ——
	// 防护措施本身成了 DoS 工具。现在把顺序倒过来：先验凭据，
	// 凭据正确一律放行并清零计数；只有猜错的那些才计入并触发 429。
	// 攻击者在这条路径上得到的信息量没有变化（他不知道密码，只会拿到 429），
	// 但代价是"锁定期间每次尝试仍要算一次 bcrypt" —— 那已经由 IP 限流兜住。
	ip := s.clientIP(r)
	ipKey := "ip:" + ip
	userKey := "u:" + strings.ToLower(strings.TrimSpace(req.Username))
	if ok, wait := s.ipLimiter.Allow(ipKey); !ok {
		s.auditAs(0, r, "login_blocked", req.Username, "来源 IP 登录尝试过于频繁（已限流）")
		w.Header().Set("Retry-After", fmt.Sprintf("%d", int(wait.Seconds())+1))
		writeErr(w, http.StatusTooManyRequests, retryAfterMessage(wait))
		return
	}

	var (
		id    int64
		hash  string
		role  string
		state string
	)
	err := s.db.QueryRow(`SELECT id, password_hash, role, status FROM users WHERE username = ?`, req.Username).
		Scan(&id, &hash, &role, &state)
	userExists := err == nil
	if !userExists {
		// 用户不存在时也要走一遍 bcrypt：否则"用户名不存在"会比"密码错误"
		// 快上两个数量级，响应时间本身就泄露了账号是否存在
		//（文案统一了，时间没统一，等于没统一）。
		_ = auth.CheckPassword(dummyPasswordHash, req.Password)
	}
	ok := userExists && state == "active" && auth.CheckPassword(hash, req.Password)

	if !ok {
		// 先看"这次之前是否已经在锁定窗口里"：只有**已经锁定**的账号才回 429，
		// 触发锁定的那一次本身仍然是 401 —— 凭据确实是错的，401 才是诚实的答案，
		// 而"从下一次开始限流"也更符合直觉（也保住了原有的可观测行为）。
		alreadyLocked, wait := s.userLimiter.Allow(userKey)
		lockedUser := s.userLimiter.RecordFailure(userKey)
		lockedIP := s.ipLimiter.RecordFailure(ipKey)
		detail := "密码错误或账号不可用"
		if lockedUser {
			detail += "（该账号已触发登录锁定）"
		}
		if lockedIP {
			detail += "（来源 IP 已触发限流）"
		}
		s.auditAs(0, r, "login_failed", req.Username, detail)

		if !alreadyLocked {
			w.Header().Set("Retry-After", fmt.Sprintf("%d", int(wait.Seconds())+1))
			writeErr(w, http.StatusTooManyRequests, retryAfterMessage(wait))
			return
		}
		writeErr(w, http.StatusUnauthorized, "用户名或密码错误")
		return
	}

	// 登录成功：清除失败计数
	s.userLimiter.Reset(userKey)
	s.ipLimiter.Reset(ipKey)

	token, err := s.auth.SignToken(id, req.Username, role, 24*time.Hour)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "签发令牌失败")
		return
	}
	s.auditAs(id, r, "login", req.Username, "登录成功")
	// 带上 id（UID）：它是**不会变**的那个标识。
	// 用户名是可以改的（见 handleSetUsername），凡是"记住这个人是谁"的地方
	// 都该用 id 而不是用户名 —— 前端把它存进登录态，用于"是不是我自己的账号"
	// 这类比较（按用户名比较的话，改完名就认不出来了）。
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"token":    token,
		"id":       id,
		"username": req.Username,
		"role":     role,
	})
}
