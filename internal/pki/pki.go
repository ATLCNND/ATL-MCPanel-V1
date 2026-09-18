// Package pki 提供面板的证书颁发能力（用于 Panel↔Daemon 的 mTLS）。
//
// 设计：
//   - Panel 首次启动时自动生成自签 CA（存放在 pki 目录，权限 0600）
//   - 每个节点由管理员为其签发独立的客户端证书（CN = 节点名）
//   - Panel 的 gRPC 服务端要求客户端证书并由该 CA 校验，从而实现双向认证：
//     未经签发的进程无法伪装成 Daemon 接入
package pki

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	caCertFile = "ca.crt"
	caKeyFile  = "ca.key"

	caTTL          = 10 * 365 * 24 * time.Hour // CA 有效期
	defaultCertTTL = 3 * 365 * 24 * time.Hour  // 节点证书有效期

	// PanelGRPCServerName Panel gRPC 服务端证书的名称。
	// Daemon 据此校验服务端身份（证书 SAN 中必须包含该名称）。
	PanelGRPCServerName = "atlmcpanel-panel"

	// ServerName 客户端校验 Panel 服务端时使用的名称（兼容别名）。
	ServerName = PanelGRPCServerName
)

// CA 证书颁发机构。
type CA struct {
	Cert    *x509.Certificate
	Key     *ecdsa.PrivateKey
	CertPEM []byte
	dir     string
}

// LoadOrCreateCA 加载已有 CA；不存在时自动生成并落盘。
// 返回的 bool 表示是否为本次新建。
func LoadOrCreateCA(dir string) (*CA, bool, error) {
	certPath := filepath.Join(dir, caCertFile)
	keyPath := filepath.Join(dir, caKeyFile)

	if certPEM, keyPEM, err := readPair(certPath, keyPath); err == nil {
		ca, err := parseCA(certPEM, keyPEM)
		if err != nil {
			return nil, false, fmt.Errorf("解析已有 CA 失败: %w", err)
		}
		ca.dir = dir
		return ca, false, nil
	}

	ca, err := generateCA()
	if err != nil {
		return nil, false, err
	}
	ca.dir = dir

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, false, fmt.Errorf("创建 PKI 目录失败: %w", err)
	}
	if err := os.WriteFile(certPath, ca.CertPEM, 0o644); err != nil {
		return nil, false, fmt.Errorf("写入 CA 证书失败: %w", err)
	}
	keyPEM, err := marshalKey(ca.Key)
	if err != nil {
		return nil, false, err
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		return nil, false, fmt.Errorf("写入 CA 私钥失败: %w", err)
	}
	return ca, true, nil
}

// generateCA 生成自签 CA。
func generateCA() (*CA, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("生成 CA 密钥失败: %w", err)
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}

	now := time.Now()
	tpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "ATL-MCPanel CA", Organization: []string{"ATL-MCPanel"}},
		NotBefore:             now.Add(-5 * time.Minute),
		NotAfter:              now.Add(caTTL),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
		// 现代校验要求 SAN；虽 CA 自身一般不被校验名称，仍显式提供以便运维工具使用
		DNSNames: []string{"atlmcpanel-ca"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("签发 CA 证书失败: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	return &CA{
		Cert:    cert,
		Key:     key,
		CertPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
	}, nil
}

// certNodeName 把节点名归一化成**可放进 X.509 的 ASCII 标识**。
//
// ---------------------------------------------------------------------------
// 为什么需要它（2026-09-17 实测踩到）
// ---------------------------------------------------------------------------
// X.509 的 CommonName 与 DNS SAN 都必须能编码成 IA5String（即 ASCII）。
// 节点名里只要有一个中文，x509.CreateCertificate 就直接失败：
//
//	x509: "HK测试8c8g10m" cannot be encoded as an IA5String
//
// 表现为**一键部署点了没反应，报一个和"节点名"毫无关系的错误**。
// 而在中文环境下给节点起中文名是完全正常的用法，所以不该反过来要求用户改名字。
//
// ---------------------------------------------------------------------------
// ⚠️ 签发与拨号必须共用这一个函数
// ---------------------------------------------------------------------------
// 节点证书的 DNS SAN 是 `certNodeName(名字)`，而面板连节点时会把
// `certNodeName(名字)` 当作 TLS 的 ServerName 去校验（nodemgr → ClientTLSConfigWithName）。
// 两边算出来必须是**同一个字符串**，否则 mTLS 校验必然失败 ——
// 这也是为什么清洗逻辑放在 pki 包内部、由收发两端共用，而不是各自实现一份。
//
// 做过转换的名字会附上原名的短哈希：纯 ASCII 的名字原样使用，
// 而 "测试1" / "測試1" 这类会被清成同一个前缀的名字，靠哈希区分开，避免
// 两个节点的证书互相通过校验。
func certNodeName(nodeName string) string {
	s := strings.TrimSpace(nodeName)
	if s == "" {
		return ""
	}

	var b strings.Builder
	prevDash := false
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_', r == '.':
			b.WriteRune(r)
			prevDash = false
		default:
			// 其余字符（中文、空格、符号…）一律折叠成一个 '-'
			if !prevDash {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	out := strings.Trim(b.String(), "-._")

	// 纯 ASCII 且长度合适 → 原样使用，保持可读性
	if out == s && len(out) <= 63 {
		return out
	}
	if out == "" {
		out = "node"
	}
	sum := sha256.Sum256([]byte(s))
	suffix := fmt.Sprintf("-%x", sum[:4]) // 9 个字符
	if len(out) > 63-len(suffix) {
		out = out[:63-len(suffix)]
		out = strings.TrimRight(out, "-._")
	}
	if out == "" {
		out = "node"
	}
	return out + suffix
}

// IssueClientCert 为节点签发客户端证书，CN 使用节点名（ASCII 化后），有效期默认 3 年。
func (ca *CA) IssueClientCert(nodeName string) (certPEM, keyPEM []byte, err error) {
	return ca.IssueClientCertWithTTL(nodeName, defaultCertTTL)
}

// IssueClientCertWithTTL 指定有效期的签发（节点证书，含 SAN = 节点名）。
//
// 节点名会先经过 certNodeName 归一化 —— 中文名、含空格的名字都能正常签发。
func (ca *CA) IssueClientCertWithTTL(nodeName string, ttl time.Duration) (certPEM, keyPEM []byte, err error) {
	if nodeName == "" {
		return nil, nil, fmt.Errorf("节点名不能为空")
	}
	name := certNodeName(nodeName)
	if name == "" {
		return nil, nil, fmt.Errorf("节点名 %q 无法转换成可用的证书标识", nodeName)
	}
	return ca.issueCert(issueOptions{
		commonName: name,
		dnsNames:   []string{name},
		// 同时用于双向通信：Daemon 作为服务端（Panel 调用它）与客户端（它连 Panel）
		usages: []x509.ExtKeyUsage{
			x509.ExtKeyUsageClientAuth,
			x509.ExtKeyUsageServerAuth,
		},
		ttl:          ttl,
		organization: "ATL-MCPanel Node",
	})
}

// CAPool 返回用于校验客户端证书的池。
func (ca *CA) CAPool() *x509.CertPool {
	pool := x509.NewCertPool()
	pool.AddCert(ca.Cert)
	return pool
}

// ServerTLSConfig 生成 gRPC 服务端的 mTLS 配置（强制校验客户端证书）。
// 服务端使用专用的 Panel 证书（含 SAN），而非 CA 证书本身。
func (ca *CA) ServerTLSConfig() (*tls.Config, error) {
	certPEM, keyPEM, err := ca.PanelServerCert()
	if err != nil {
		return nil, err
	}
	return ca.ServerTLSConfigFor(certPEM, keyPEM)
}

// PanelServerCert 返回 Panel gRPC 服务端证书（首次调用时生成并持久化）。
// 证书含 SAN: DNS:atlmcpanel-panel —— 现代 TLS 校验依赖 SAN，仅 CN 会被拒绝。
func (ca *CA) PanelServerCert() (certPEM, keyPEM []byte, err error) {
	certPath := filepath.Join(ca.dir, "panel-server.crt")
	keyPath := filepath.Join(ca.dir, "panel-server.key")

	if c, k, err := readPair(certPath, keyPath); err == nil {
		if _, err := tls.X509KeyPair(c, k); err == nil {
			return c, k, nil
		}
		// 证书损坏或已过期则重新签发
	}

	c, k, err := ca.issueCert(issueOptions{
		commonName:   PanelGRPCServerName,
		dnsNames:     []string{PanelGRPCServerName, "localhost"},
		ipAddresses:  []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
		usages:       []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		ttl:          defaultCertTTL,
		organization: "ATL-MCPanel",
	})
	if err != nil {
		return nil, nil, err
	}
	if err := os.WriteFile(certPath, c, 0o644); err != nil {
		return nil, nil, err
	}
	if err := os.WriteFile(keyPath, k, 0o600); err != nil {
		return nil, nil, err
	}
	return c, k, nil
}

// issueOptions 签发参数。
type issueOptions struct {
	commonName   string
	dnsNames     []string
	ipAddresses  []net.IP
	usages       []x509.ExtKeyUsage
	ttl          time.Duration
	organization string
}

// issueCert 按参数签发证书。
func (ca *CA) issueCert(o issueOptions) (certPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, nil, err
	}
	now := time.Now()
	tpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: o.commonName, Organization: []string{o.organization}},
		NotBefore:    now.Add(-5 * time.Minute),
		NotAfter:     now.Add(o.ttl),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  o.usages,
		DNSNames:     o.dnsNames,
		IPAddresses:  o.ipAddresses,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, ca.Cert, &key.PublicKey, ca.Key)
	if err != nil {
		return nil, nil, fmt.Errorf("签发证书失败: %w", err)
	}
	keyDER, err := marshalKey(key)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), keyDER, nil
}

// PanelClientCert 返回 Panel 自身的客户端证书（用于调用 Daemon 的 gRPC）。
// 首次调用时生成并持久化到 pki 目录。
func (ca *CA) PanelClientCert() (certPEM, keyPEM []byte, err error) {
	certPath := filepath.Join(ca.dir, "panel-client.crt")
	keyPath := filepath.Join(ca.dir, "panel-client.key")

	if c, k, err := readPair(certPath, keyPath); err == nil {
		return c, k, nil
	}

	c, k, err := ca.IssueClientCertWithTTL("atlmcpanel-panel", defaultCertTTL)
	if err != nil {
		return nil, nil, err
	}
	if err := os.WriteFile(certPath, c, 0o644); err != nil {
		return nil, nil, err
	}
	if err := os.WriteFile(keyPath, k, 0o600); err != nil {
		return nil, nil, err
	}
	return c, k, nil
}

// ServerTLSConfigFor 构造服务端 mTLS 配置（校验由本 CA 签发的客户端证书）。
func (ca *CA) ServerTLSConfigFor(certPEM, keyPEM []byte) (*tls.Config, error) {
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("服务端证书无效: %w", err)
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    ca.CAPool(),
		MinVersion:   tls.VersionTLS12,
	}, nil
}

// ClientTLSConfigWithName 构造客户端 mTLS 配置，并指定服务端校验名称。
//
// ⚠️ 这里的 serverName 也要走 certNodeName：节点证书的 SAN 就是按那个规则生成的，
// 两边不一致的话 mTLS 必然校验失败（中文节点名前就是这么炸的）。
func ClientTLSConfigWithName(caPEM, certPEM, keyPEM []byte, serverName string) (*tls.Config, error) {
	cfg, err := ClientTLSConfig(caPEM, certPEM, keyPEM)
	if err != nil {
		return nil, err
	}
	if serverName != "" {
		cfg.ServerName = certNodeName(serverName)
	}
	return cfg, nil
}

// ServerTLSConfigFromFiles 从文件构造服务端 mTLS 配置（Daemon 侧使用）。
func ServerTLSConfigFromFiles(caFile, certFile, keyFile string) (*tls.Config, error) {
	caPEM, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("读取 CA 证书失败: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("CA 证书解析失败")
	}
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("加载服务端证书失败: %w", err)
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    pool,
		MinVersion:   tls.VersionTLS12,
	}, nil
}

// FilePaths 返回 CA 文件路径（供运维查看/分发）。
func (ca *CA) FilePaths() (certPath, keyPath string) {
	return filepath.Join(ca.dir, caCertFile), filepath.Join(ca.dir, caKeyFile)
}

// ---- 内部辅助 ----

func readPair(certPath, keyPath string) (certPEM, keyPEM []byte, err error) {
	certPEM, err = os.ReadFile(certPath)
	if err != nil {
		return nil, nil, err
	}
	keyPEM, err = os.ReadFile(keyPath)
	if err != nil {
		return nil, nil, err
	}
	return certPEM, keyPEM, nil
}

func parseCA(certPEM, keyPEM []byte) (*CA, error) {
	certBlock, _ := pem.Decode(certPEM)
	if certBlock == nil {
		return nil, fmt.Errorf("CA 证书 PEM 解析失败")
	}
	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return nil, err
	}
	key, err := parseKey(keyPEM)
	if err != nil {
		return nil, err
	}
	return &CA{Cert: cert, Key: key, CertPEM: certPEM}, nil
}

func parseKey(keyPEM []byte) (*ecdsa.PrivateKey, error) {
	block, _ := pem.Decode(keyPEM)
	if block == nil {
		return nil, fmt.Errorf("私钥 PEM 解析失败")
	}
	if k, err := x509.ParseECPrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	// 兼容 PKCS#8
	any, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("解析私钥失败: %w", err)
	}
	k, ok := any.(*ecdsa.PrivateKey)
	if !ok {
		return nil, fmt.Errorf("私钥类型不受支持")
	}
	return k, nil
}

func marshalKey(key *ecdsa.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), nil
}

func randomSerial() (*big.Int, error) {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, limit)
	if err != nil {
		return nil, fmt.Errorf("生成序列号失败: %w", err)
	}
	return serial, nil
}

// ClientTLSConfig 供 Daemon 侧构造 mTLS 客户端配置。
// caPEM 为 CA 证书内容；certPEM/keyPEM 为节点客户端证书。
func ClientTLSConfig(caPEM, certPEM, keyPEM []byte) (*tls.Config, error) {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, fmt.Errorf("CA 证书解析失败")
	}
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("客户端证书或私钥无效: %w", err)
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      pool,
		ServerName:   ServerName, // 服务端使用 CA 证书，故按此名称校验
		MinVersion:   tls.VersionTLS12,
	}, nil
}

// DialerServerName 返回校验 Panel gRPC 服务端时使用的名称。
// Panel 服务端使用固定的专用证书（SAN: atlmcpanel-panel），因此不随地址变化。
func DialerServerName(host string) string {
	return PanelGRPCServerName
}
