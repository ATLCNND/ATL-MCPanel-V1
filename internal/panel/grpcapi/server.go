// Package grpcapi 是 Panel 侧的 gRPC 服务实现（Daemon 接入点）。
package grpcapi

import (
	"context"
	"database/sql"
	"sync"
	"time"

	"google.golang.org/grpc/peer"

	"github.com/ATLCNND/ATL-MCPanel/internal/common/logger"
	pb "github.com/ATLCNND/ATL-MCPanel/internal/proto/mcpanel"
)

// Server 实现 DaemonService。
type Server struct {
	pb.UnimplementedDaemonServiceServer
	db  *sql.DB
	log *logger.Logger

	mu     sync.RWMutex
	online map[string]time.Time // node_id -> 最近心跳时刻

	heartbeats *heartbeatTracker // 心跳写库节流
}

// NewServer 创建 gRPC 服务。
func NewServer(d *sql.DB, log *logger.Logger) *Server {
	return &Server{
		db:         d,
		log:        log,
		online:     make(map[string]time.Time),
		heartbeats: newHeartbeatTracker(heartbeatPersistInterval),
	}
}

// Register 处理节点注册。
func (s *Server) Register(ctx context.Context, req *pb.RegisterRequest) (*pb.RegisterResponse, error) {
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