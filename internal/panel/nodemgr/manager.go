// Package nodemgr 管理 Panel 到各 Daemon 的 gRPC 连接。
package nodemgr

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

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

// scrubInterceptor 把节点返回的 gRPC 错误**换成不含部署细节的说法**。
//
// 为什么在拦截器里做，而不是逐个 handler 改：Daemon 的错误文本里带的是节点上的
// 绝对路径（"stat /opt/atl-node/instances/beta03/etc/passwd: no such file"），
// 而这些错误会被上百处 `writeErr(w, 500, err.Error())` 原样回给浏览器。
// 逐个改既容易漏、也会随新接口不断回潮；把闸放在**客户端边界**上，
// 一处生效、新接口自动被覆盖。
//
// 细节并没有丢：仍然记进面板日志（含原始错误），排查时按时间点去日志里找。
// 对外只保留"哪一类操作失败了"，这足以让用户判断该不该重试，
// 而不足以让他摸清节点的目录布局、部署路径与内部组件名。
//
// 只处理"节点返回了错误"这一类。参数校验、权限不足这类**本地**错误
// （由 httpapi 自己产生）不经这里，它们本来就不含节点信息。
func scrubInterceptor(
	ctx context.Context, method string, req, reply interface{},
	cc *grpc.ClientConn, invoker grpc.UnaryInvoker, opts ...grpc.CallOption,
) error {
	err := invoker(ctx, method, req, reply, cc, opts...)
	if err == nil {
		return nil
	}
	st, ok := status.FromError(err)
	// 这几类是可操作的语义，不泄露任何节点内部信息，原样透出更好用
	switch st.Code() {
	case codes.NotFound, codes.PermissionDenied, codes.Unauthenticated,
		codes.ResourceExhausted, codes.AlreadyExists, codes.FailedPrecondition,
		codes.Unimplemented, codes.DeadlineExceeded:
		return err
	}
	_ = ok
	slog.Warn("节点调用失败（详情仅记录在服务端日志）",
		"method", method, "code", st.Code().String(), "error", err.Error())
	// 把原始错误挂在 context 里没用（调用方只看 error），因此这里返回一个
	// 保留了"可判断性"的通用错误：调用方仍能看出是节点侧失败。
	return status.Error(st.Code(), "节点执行失败："+nodeErrorSummary(st.Code()))
}

// nodeErrorSummary 给每类节点错误一句**不含内部细节**的人话。
func nodeErrorSummary(c codes.Code) string {
	switch c {
	case codes.Internal, codes.Unknown:
		return "节点内部错误（详情见面板日志）"
	case codes.Unavailable:
		return "节点不可达（Daemon 可能未运行或网络不通）"
	case codes.Canceled:
		return "操作已取消"
	default:
		return "请稍后重试，或查看面板日志"
	}
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
		grpc.WithChainUnaryInterceptor(timeoutInterceptor, scrubInterceptor),
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
