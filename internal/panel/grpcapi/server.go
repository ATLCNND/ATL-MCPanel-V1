// Package grpcapi 是 Panel 侧的 gRPC 服务实现（Daemon 接入点）。
package grpcapi

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"

	"github.com/ATLCNND/ATL-MCPanel/internal/common/logger"
	"github.com/ATLCNND/ATL-MCPanel/internal/pki"
	pb "github.com/ATLCNND/ATL-MCPanel/internal/proto/mcpanel"
)

// Server 实现 DaemonService。
type Server struct {
	pb.UnimplementedDaemonServiceServer
	db  *sql.DB
	log *logger.Logger

	// requirePeerCN 为 true 时（面板启用了 mTLS），拒绝任何"证书身份与声称
	// 的节点名不一致"的 Register/Ping —— 见 verifyPeerNode。
	requirePeerCN bool

	mu     sync.RWMutex
	online map[string]time.Time // node_id -> 最近心跳时刻

	heartbeats *heartbeatTracker // 心跳写库节流
	// requireEnrollment 只允许通过凭证交换接入的节点（M4）。见 config.RequireEnrollment。
	requireEnrollment bool
}

// NewServer 创建 gRPC 服务。
//
// requirePeerCN 传 cfg.Server.GRPCMTLS：开启 mTLS 时对端一定有证书，
// 于是可以把"节点名"这个自述字段与证书身份绑定起来。
//
// requireEnrollment 传 cfg.Server.RequireEnrollment（M4）：开启后只允许走过凭证交换的节点接入。
func NewServer(d *sql.DB, log *logger.Logger, requirePeerCN, requireEnrollment bool) *Server {
	return &Server{
		db:                d,
		log:               log,
		requirePeerCN:     requirePeerCN,
		requireEnrollment: requireEnrollment,
		online:            make(map[string]time.Time),
		heartbeats:        newHeartbeatTracker(heartbeatPersistInterval),
	}
}

// peerCertCN 取对端 mTLS 客户端证书的 CN（没有证书时返回空串）。
func peerCertCN(ctx context.Context) string {
	p, ok := peer.FromContext(ctx)
	if !ok || p.AuthInfo == nil {
		return ""
	}
	info, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok || len(info.State.PeerCertificates) == 0 {
		return ""
	}
	return info.State.PeerCertificates[0].Subject.CommonName
}

// verifyPeerNode 校验"对端证书身份 == 它声称的节点名"。
//
// 为什么必须做：Register/Ping 的 NodeId 是**对端自己填的**。原先的实现直接
// 拿它去 `UPDATE nodes SET ip=?`，于是任何一个持有 CA 签发证书的节点（也就是
// 每一个节点）都能冒名把自己注册成**别的节点**：那个节点的行会指向攻击者的
// IP，随后管理员对它执行"探测/部署/重启 Daemon"时，面板会**带着那个节点存的
// SSH 凭据**去连攻击者的机器（凭据被钓走 + 面板被当成跳板）。
//
// 证书 CN 走 pki.CertNodeName（与签发端同一份实现），中文节点名同样成立。
func (s *Server) verifyPeerNode(ctx context.Context, nodeID string) error {
	cn := peerCertCN(ctx)
	if cn == "" {
		if s.requirePeerCN {
			return errors.New("未提供客户端证书")
		}
		return nil // 未启用 mTLS：监听范围已由启动校验限制（见 cmd/panel/main.go）
	}
	if want := pki.CertNodeName(nodeID); cn != want {
		return errors.New("客户端证书身份与节点名不匹配")
	}
	return nil
}

// Register 处理节点注册。
func (s *Server) Register(ctx context.Context, req *pb.RegisterRequest) (*pb.RegisterResponse, error) {
	if err := s.verifyPeerNode(ctx, req.NodeId); err != nil {
		// 只在**服务端日志**里记下证书 CN，不回显给对端（那等于告诉他该冒充谁）
		s.log.Warn("节点注册被拒绝：证书身份与节点名不匹配",
			"node_id", req.NodeId, "cert_cn", peerCertCN(ctx), "error", err)
		return &pb.RegisterResponse{Accepted: false, Message: "节点身份校验失败"}, nil
	}
	if err := s.checkEnrollmentRequired(req.NodeId, req.NodeUid); err != nil {
		s.log.Warn("节点注册被拒绝：未通过凭证交换", "node_id", req.NodeId, "error", err)
		return &pb.RegisterResponse{Accepted: false, Message: err.Error()}, nil
	}
	s.log.Info("节点注册", "node_id", req.NodeId, "hostname", req.Hostname, "os", req.Os, "cores", req.CpuCores, "mem", req.MemTotal)

	// 提取对端 IP（用于 Panel 反向连接 Daemon）
	daemonIP := ""
	if p, ok := peer.FromContext(ctx); ok {
		daemonIP = extractIP(p.Addr.String())
	}

	// 更新或插入 nodes 表（保存 daemon IP）
	var existingID int64
	// 先按 node_uid 找：它是**稳定身份**，优先级高于显示名（name）。
	//
	// 为什么必须先查它（真机踩到）：v32 给 node_uid 建了唯一索引，而一台机器可能
	// 因为多次接入/改名在库里留下**另一行**同名 uid 的记录。此前只按 name 查重，
	// 于是 INSERT 撞唯一索引 → 注册失败 → Daemon 直接退出（节点整个掉线）。
	var uidID int64
	if uid := strings.TrimSpace(req.NodeUid); uid != "" {
		if e := s.db.QueryRow(`SELECT id FROM nodes WHERE node_uid = ?`, uid).Scan(&uidID); e == nil {
			existingID = uidID
			// 顺带把显示名改成它现在自报的，避免库里留一个再也对不上的旧名字
			_, _ = s.db.Exec(`UPDATE nodes SET name = ? WHERE id = ?`, req.NodeId, uidID)
		}
	}
	err := error(nil)
	if existingID == 0 {
		err = s.db.QueryRow(`SELECT id FROM nodes WHERE name = ?`, req.NodeId).Scan(&existingID)
	}
	if err != nil {
		// 不存在则插入
		_, err2 := s.db.Exec(
			`INSERT INTO nodes (name, ip, ssh_user, ssh_auth, ssh_port, status, cpu, mem, last_seen, node_uid)
			 VALUES (?, ?, '', '', 22, 'online', ?, ?, CURRENT_TIMESTAMP, ?)`,
			req.NodeId, daemonIP, req.CpuCores, req.MemTotal, strings.TrimSpace(req.NodeUid),
		)
		if err2 != nil {
			return &pb.RegisterResponse{Accepted: false, Message: "写入节点失败: " + err2.Error()}, nil
		}
	} else {
		_, err2 := s.db.Exec(
			// node_uid 只在原本为空时写入：它是稳定身份，不该被后来的上报改写
			`UPDATE nodes SET ip=?, status='online', cpu=?, mem=?, last_seen=CURRENT_TIMESTAMP,
			       node_uid = CASE WHEN COALESCE(node_uid,'') = '' THEN ? ELSE node_uid END
			 WHERE id=?`,
			daemonIP, req.CpuCores, req.MemTotal, strings.TrimSpace(req.NodeUid), existingID,
		)
		if err2 != nil {
			return &pb.RegisterResponse{Accepted: false, Message: "更新节点失败: " + err2.Error()}, nil
		}
	}

	s.mu.Lock()
	s.online[req.NodeId] = time.Now()
	s.mu.Unlock()

	return &pb.RegisterResponse{Accepted: true, Message: "ok"}, nil
}

// extractIP 从 "1.2.3.4:5678" 提取 IP。
func extractIP(addr string) string {
	for i := len(addr) - 1; i >= 0; i-- {
		if addr[i] == ':' {
			return addr[:i]
		}
	}
	return addr
}

// Ping 处理心跳。
//
// 除内存记录外，还会把心跳时间写回 nodes.last_seen —— 面板的健康告警与
// 界面展示都以该字段判断节点是否在线。写库按 heartbeatPersistInterval 节流，
// 避免每 10 秒一次写库。
func (s *Server) Ping(ctx context.Context, req *pb.PingRequest) (*pb.PingResponse, error) {
	// 与 Register 同一条规矩：心跳里的 NodeId 也是自述的，不校验就能伪造
	// 任意节点的 last_seen / 在线状态 / CPU 内存磁盘数字（告警与界面都看它）。
	if err := s.verifyPeerNode(ctx, req.NodeId); err != nil {
		s.log.Warn("节点心跳被拒绝：证书身份与节点名不匹配",
			"node_id", req.NodeId, "cert_cn", peerCertCN(ctx), "error", err)
		return nil, status.Error(codes.PermissionDenied, "节点身份校验失败")
	}

	// 撤销检查：节点的 enrollment 被撤销、或节点记录已被删除时，**拒绝心跳**。
	//
	// 为什么放在心跳而不是依赖面板去拨节点：撤销必须能在「面板根本连不到节点」的场景下生效
	//（这正是凭证交换要解决的部署形态）。节点每 10 秒心跳一次，所以撤销最迟 10 秒生效。
	// 拒绝用 PermissionDenied：daemon 侧会把它当作「不要再连」处理，而不是普通网络错误重试。
	if err := s.checkEnrollmentRequired(req.NodeId, ""); err != nil {
		s.log.Warn("节点心跳被拒绝：未通过凭证交换", "node_id", req.NodeId)
		return nil, err
	}
	if revoked, reason := s.nodeRevoked(req.NodeId); revoked {
		s.log.Warn("节点心跳被拒绝：已被撤销", "node_id", req.NodeId, "reason", reason)
		return nil, status.Error(codes.PermissionDenied, "节点已被移除（"+reason+"），请重新接入")
	}

	now := time.Now()

	s.mu.Lock()
	s.online[req.NodeId] = now
	s.mu.Unlock()

	if s.heartbeats.ShouldPersist(req.NodeId) {
		res, err := s.db.Exec(
			`UPDATE nodes SET last_seen = CURRENT_TIMESTAMP, status = 'online',
			        cpu_percent = ?, mem_used = ?, disk_used = ?, disk_total = ?,
			        backup_disk_used = ?, backup_disk_total = ?
			 WHERE name = ?`,
			req.CpuPercent, req.MemUsed, req.DiskUsed, req.DiskTotal,
			req.BackupDiskUsed, req.BackupDiskTotal, req.NodeId)
		if err != nil {
			s.log.Warn("更新节点心跳失败", "node_id", req.NodeId, "error", err)
		} else if n, _ := res.RowsAffected(); n == 0 {
			// 0 行更新说明节点名与数据库不一致（注册时可能用的是其它标识）
			s.log.Warn("心跳更新未匹配到节点", "node_id", req.NodeId)
		}
	}
	return &pb.PingResponse{Timestamp: now.UnixMilli()}, nil
}

// heartbeatPersistInterval 心跳写库的最小间隔。
const heartbeatPersistInterval = 30 * time.Second

// nodeRevoked 该节点是否已被撤销/移除。返回 (是否撤销, 原因)。
//
// 判据两条：① 它的 node_uid 有被撤销的 enrollment；② nodes 表里根本没有这一行
//（管理员删了节点）。第二条是「删了节点它还连得回来」这个问题的直接答案。
func (s *Server) nodeRevoked(nodeID string) (bool, string) {
	var uid string
	err := s.db.QueryRow(`SELECT COALESCE(node_uid,'') FROM nodes WHERE name = ?`, nodeID).Scan(&uid)
	if err == sql.ErrNoRows {
		return true, "节点记录已被删除"
	}
	if err != nil {
		// 查不动数据库时不误杀：放行，让上面那条 UPDATE 去报错
		return false, ""
	}
	if uid == "" {
		return false, ""
	}
	var revoked int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM node_enrollments WHERE node_uid = ? AND revoked_at IS NOT NULL`, uid).Scan(&revoked)
	if revoked > 0 {
		return true, "凭证已被撤销"
	}
	return false, ""
}


// checkEnrollmentRequired 开启 require_enrollment 时，拒绝**没有走过凭证交换**的节点。
//
// 判据：节点行上有 node_uid，且存在未撤销的 enrollment 记录。老的一键部署节点两样都没有，
// 于是会被拒 —— 这正是这个开关的用途（强制迁移到「面板不存 SSH 凭据」的接入方式）。
// 默认关闭：升级不会把现存节点直接踢下线。
func (s *Server) checkEnrollmentRequired(nodeID, nodeUID string) error {
	if !s.requireEnrollment {
		return nil
	}
	uid := strings.TrimSpace(nodeUID)
	if uid == "" {
		_ = s.db.QueryRow("SELECT COALESCE(node_uid,'') FROM nodes WHERE name = ?", nodeID).Scan(&uid)
	}
	if uid == "" {
		return status.Error(codes.PermissionDenied,
			"本面板要求通过凭证交换接入：请在节点上执行 atl-daemon enroll --panel <面板地址> --token <一次性token>")
	}
	var n int
	_ = s.db.QueryRow("SELECT COUNT(*) FROM node_enrollments WHERE node_uid = ? AND revoked_at IS NULL", uid).Scan(&n)
	if n == 0 {
		return status.Error(codes.PermissionDenied,
			"该节点没有有效的凭证记录：请重新执行一次凭证交换（atl-daemon enroll）")
	}
	return nil
}
