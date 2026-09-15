// Package httpapi 是 Panel 的 HTTP API 层。
package httpapi

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/ATLCNND/ATL-MCPanel/internal/common/logger"
	"github.com/ATLCNND/ATL-MCPanel/internal/frp"
	"github.com/ATLCNND/ATL-MCPanel/internal/pki"
	"github.com/ATLCNND/ATL-MCPanel/internal/panel/auth"
	"github.com/ATLCNND/ATL-MCPanel/internal/panel/dbbackup"
	"github.com/ATLCNND/ATL-MCPanel/internal/panel/nodemgr"
	"github.com/ATLCNND/ATL-MCPanel/internal/panel/scheduler"
)

// Options HTTP API 服务的附加配置。
type Options struct {
	// DataDir 面板数据目录（数据库所在目录）；头像等附件放在其下的 avatars/。
	// 为空时退回当前工作目录下的 data/。
	DataDir string
	WebDir      string         // 前端静态资源目录（空则不托管）
	ListenAddr  string         // 面板 HTTP 监听地址（用于推导穿透的本地端口）
	TLSListen   string         // 面板 HTTPS 监听地址（配置后穿透优先转发到此端口）
	ExternalURL string         // 面板外部访问地址
	PanelFrpDir string         // 面板自身穿透工作目录
	TrustProxy  bool           // 信任 X-Forwarded-For / X-Real-IP（位于代理后时开启）
	CA          *pki.CA            // mTLS 证书颁发机构（nil 表示未启用）
	GRPCMTLS    bool               // gRPC 是否启用 mTLS
	Backup      *dbbackup.Manager  // 面板数据库备份管理器（nil 表示未启用）
	DaemonBinary   string          // 节点部署下发的 Daemon 二进制路径
	GRPCListen     string          // 面板 gRPC 监听地址
	GRPCPublicAddr string          // 节点连接面板 gRPC 的地址
	RemoteInstallDir  string       // 节点安装目录
	DaemonServiceName string       // 节点 systemd 单元名
	DaemonGRPCListen  string       // 节点 Daemon gRPC 监听
	Logger         *logger.Logger  // 日志
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
	trustProxy  bool          // 是否信任 X-Forwarded-For（面板位于代理之后时开启）
	userLimiter *loginLimiter // 按用户名的登录限流
	ipLimiter   *loginLimiter // 按来源 IP 的登录限流（阈值更宽松）
	ca          *pki.CA           // mTLS 证书颁发机构
	grpcMTLS    bool              // gRPC 是否已启用 mTLS
	backup      *dbbackup.Manager // 面板数据库备份

	daemonBinary      string // 节点部署时下发的 Daemon 二进制路径
	grpcPublicAddr    string // 节点连接面板 gRPC 的地址
	listenAddrOfGRPC  string // 面板 gRPC 监听地址
	remoteDir         string // 节点安装目录
	daemonServiceName string // 节点 systemd 单元名
	daemonGRPCListen  string // 节点 Daemon gRPC 监听
	sched             *scheduler.Scheduler // 后台调度器（定时备份 + 告警）
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
	mux.HandleFunc("GET /api/avatars/{id}", s.handleGetAvatar)          // 公开：仅返回已审核通过的
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

	return mux
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

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req loginReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "无效的请求体")
		return
	}

	// 限流：按「用户名」严格限制 + 按「来源 IP」宽松限制（双维度防爆破）
	ip := s.clientIP(r)
	userKey := "u:" + strings.ToLower(strings.TrimSpace(req.Username))
	ipKey := "ip:" + ip
	if ok, wait := s.userLimiter.Allow(userKey); !ok {
		s.auditAs(0, r, "login_blocked", req.Username, "该账号登录尝试过于频繁（已限流）")
		w.Header().Set("Retry-After", fmt.Sprintf("%d", int(wait.Seconds())+1))
		writeErr(w, http.StatusTooManyRequests, retryAfterMessage(wait))
		return
	}
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
	if err != nil || state != "active" || !auth.CheckPassword(hash, req.Password) {
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
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"token":    token,
		"username": req.Username,
		"role":     role,
	})
}