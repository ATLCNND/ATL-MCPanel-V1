// Package nodemgr 管理 Panel 到各 Daemon 的 gRPC 连接。
package nodemgr

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"

	"github.com/ATLCNND/ATL-MCPanel/internal/common/grpclimits"
	"github.com/ATLCNND/ATL-MCPanel/internal/pki"
	pb "github.com/ATLCNND/ATL-MCPanel/internal/proto/mcpanel"
)

// daemonCallTimeout 单次一元调用的默认超时。
// 防止 Daemon 异常（死锁/无响应）时 Panel 的 HTTP 请求永久挂起。
const daemonCallTimeout = 30 * time.Second

// DaemonGRPCPort Daemon 反向 gRPC 服务端口。
const DaemonGRPCPort = 9091

// timeoutInterceptor 为未设置 deadline 的一元调用自动追加超时。
func timeoutInterceptor(
	ctx context.Context, method string, req, reply interface{},
	cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption,
) error {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, daemonCallTimeout)
		defer cancel()
	}
	return invoker(ctx, method, req, reply, cc, opts...)
}

// Manager 维护到各节点 Daemon 的 gRPC 客户端连接。
type Manager struct {
	db  *sql.DB
	mu  sync.RWMutex
	cli map[int64]pb.DaemonServiceClient
	cc  map[int64]*grpc.ClientConn

	// mTLS 材料（为 nil 时使用明文连接）
	ca         *pki.CA
	clientCert []byte
	clientKey  []byte
}

// NewManager 创建连接管理器（明文，用于开发或迁移期）。
func NewManager(db *sql.DB) *Manager {
	return &Manager{
		db:  db,
		cli: make(map[int64]pb.DaemonServiceClient),
		cc:  make(map[int64]*grpc.ClientConn),
	}
}

// NewMTLSManager 创建启用 mTLS 的连接管理器。
// 连接时以节点名作为服务端校验名称（节点证书 CN = 节点名）。
func NewMTLSManager(db *sql.DB, ca *pki.CA) (*Manager, error) {
	certPEM, keyPEM, err := ca.PanelClientCert()
	if err != nil {
		return nil, fmt.Errorf("准备面板客户端证书失败: %w", err)
	}
	return &Manager{
		db:         db,
		cli:        make(map[int64]pb.DaemonServiceClient),
		cc:         make(map[int64]*grpc.ClientConn),
		ca:         ca,
		clientCert: certPEM,
		clientKey:  keyPEM,
	}, nil
}

// GetClient 获取指定节点的 Daemon 客户端（按需建立连接）。
// 节点地址从数据库读取（nodes.ip + Daemon gRPC 端口）。
func (m *Manager) GetClient(nodeID int64) (pb.DaemonServiceClient, error) {
	m.mu.RLock()
	if c, ok := m.cli[nodeID]; ok {
		m.mu.RUnlock()
		return c, nil
	}
	m.mu.RUnlock()

	var ip, name string
	var grpcPort int
	err := m.db.QueryRow(`SELECT ip, name, grpc_port FROM nodes WHERE id = ?`, nodeID).Scan(&ip, &name, &grpcPort)
	if err != nil {
		return nil, fmt.Errorf("节点 %d 不存在: %w", nodeID, err)
	}
	if ip == "" {
		return nil, fmt.Errorf("节点 %d 未登记 IP", nodeID)
	}
	if grpcPort <= 0 {
		grpcPort = DaemonGRPCPort
	}

	addr := fmt.Sprintf("%s:%d", ip, grpcPort)
	creds, err := m.transportCredentials(name)
	if err != nil {
		return nil, err
	}

	conn, err := grpc.NewClient(addr,
		grpc.WithTransportCredentials(creds),
		grpc.WithChainUnaryInterceptor(timeoutInterceptor),
		// 与 Daemon 侧的 server 上限对齐：客户端默认只收 4 MB，
		// 而下载文件、读取大响应都可能超过它。
		grpc.WithDefaultCallOptions(
			grpc.MaxCallRecvMsgSize(grpclimits.MaxMessageBytes),
			grpc.MaxCallSendMsgSize(grpclimits.MaxMessageBytes),
		),
	)
	if err != nil {
		return nil, err
	}

	client := pb.NewDaemonServiceClient(conn)
	m.mu.Lock()
	m.cc[nodeID] = conn
	m.cli[nodeID] = client
	m.mu.Unlock()

	return client, nil
}

// transportCredentials 返回连接 Daemon 的传输凭据。
func (m *Manager) transportCredentials(nodeName string) (credentials.TransportCredentials, error) {
	if m.ca == nil {
		return insecure.NewCredentials(), nil
	}
	tlsCfg, err := pki.ClientTLSConfigWithName(m.ca.CertPEM, m.clientCert, m.clientKey, nodeName)
	if err != nil {
		return nil, err
	}
	return credentials.NewTLS(tlsCfg), nil
}

// Invalidate 丢弃指定节点的连接（节点信息变更或证书轮换后调用）。
func (m *Manager) Invalidate(nodeID int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if c, ok := m.cc[nodeID]; ok {
		_ = c.Close()
		delete(m.cc, nodeID)
		delete(m.cli, nodeID)
	}
}

// MTLSEnabled 是否启用了 mTLS 连接。
func (m *Manager) MTLSEnabled() bool { return m.ca != nil }

// Close 关闭所有连接。
func (m *Manager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, c := range m.cc {
		_ = c.Close()
	}
}
