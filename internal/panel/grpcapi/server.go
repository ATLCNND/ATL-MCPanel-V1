// Package grpcapi 是 Panel 侧的 gRPC 服务实现（Daemon 接入点）。
package grpcapi

import (
	"context"
	"database/sql"
	"errors"
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
}

// NewServer 创建 gRPC 服务。
//
// requirePeerCN 传 cfg.Server.GRPCMTLS：开启 mTLS 时对端一定有证书，
// 于是可以把"节点名"这个自述字段与证书身份绑定起来。
func NewServer(d *sql.DB, log *logger.Logger, requirePeerCN bool) *Server {
	return &Server{
		db:            d,
		log:           log,
		requirePeerCN: requirePeerCN,
		online:        make(map[string]time.Time),
		heartbeats:    newHeartbeatTracker(heartbeatPersistInterval),
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
	s.log.Info("节点注册", "node_id", req.NodeId, "hostname", req.Hostname, "os", req.Os, "cores", req.CpuCores, "mem", req.MemTotal)

	// 提取对端 IP（用于 Panel 反向连接 Daemon）
	daemonIP := ""
	if p, ok := peer.FromContext(ctx); ok {
		daemonIP = extractIP(p.Addr.String())
	}

	// 更新或插入 nodes 表（保存 daemon IP）
	var existingID int64
	err := s.db.QueryRow(`SELECT id FROM nodes WHERE name = ?`, req.NodeId).Scan(&existingID)
	if err != nil {
		// 不存在则插入
		_, err2 := s.db.Exec(
			`INSERT INTO nodes (name, ip, ssh_user, ssh_auth, ssh_port, status, cpu, mem, last_seen)
			 VALUES (?, ?, '', '', 22, 'online', ?, ?, CURRENT_TIMESTAMP)`,
			req.NodeId, daemonIP, req.CpuCores, req.MemTotal,
		)
		if err2 != nil {
			return &pb.RegisterResponse{Accepted: false, Message: "写入节点失败: " + err2.Error()}, nil
		}
	} else {
		_, err2 := s.db.Exec(
			`UPDATE nodes SET ip=?, status='online', cpu=?, mem=?, last_seen=CURRENT_TIMESTAMP WHERE id=?`,
			daemonIP, req.CpuCores, req.MemTotal, existingID,
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
