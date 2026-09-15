package pki

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoadOrCreateCAIsIdempotent(t *testing.T) {
	dir := t.TempDir()

	ca1, created1, err := LoadOrCreateCA(dir)
	if err != nil {
		t.Fatalf("首次创建失败: %v", err)
	}
	if !created1 {
		t.Error("首次应标记为新建")
	}
	if ca1.Cert.Subject.CommonName != "ATL-MCPanel CA" {
		t.Errorf("CA CN 不正确: %s", ca1.Cert.Subject.CommonName)
	}
	if !ca1.Cert.IsCA {
		t.Error("证书应为 CA")
	}

	// 二次加载应复用同一 CA
	ca2, created2, err := LoadOrCreateCA(dir)
	if err != nil {
		t.Fatalf("二次加载失败: %v", err)
	}
	if created2 {
		t.Error("二次加载不应重建")
	}
	if ca2.Cert.SerialNumber.Cmp(ca1.Cert.SerialNumber) != 0 {
		t.Error("二次加载应返回同一 CA 证书")
	}
}

func TestCAFilePermissions(t *testing.T) {
	dir := t.TempDir()
	ca, _, err := LoadOrCreateCA(dir)
	if err != nil {
		t.Fatal(err)
	}
	_, keyPath := ca.FilePaths()

	fi, err := os.Stat(keyPath)
	if err != nil {
		t.Fatalf("CA 私钥应存在: %v", err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("CA 私钥权限应为 0600，实际 %o", fi.Mode().Perm())
	}
}

func TestIssueClientCert(t *testing.T) {
	dir := t.TempDir()
	ca, _, err := LoadOrCreateCA(dir)
	if err != nil {
		t.Fatal(err)
	}

	certPEM, keyPEM, err := ca.IssueClientCert("node-001")
	if err != nil {
		t.Fatalf("签发失败: %v", err)
	}

	// 用 CA 校验签发的证书
	pool := ca.CAPool()
	cert, err := parseCertPEM(certPEM)
	if err != nil {
		t.Fatalf("解析证书失败: %v", err)
	}
	if _, err := cert.Verify(x509.VerifyOptions{
		Roots:     pool,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}); err != nil {
		t.Errorf("证书应由 CA 校验通过: %v", err)
	}
	if cert.Subject.CommonName != "node-001" {
		t.Errorf("CN 应为节点名，实际 %s", cert.Subject.CommonName)
	}

	// 证书应同时可用于客户端与服务端（双向通信）
	hasClient, hasServer := false, false
	for _, eku := range cert.ExtKeyUsage {
		if eku == x509.ExtKeyUsageClientAuth {
			hasClient = true
		}
		if eku == x509.ExtKeyUsageServerAuth {
			hasServer = true
		}
	}
	if !hasClient || !hasServer {
		t.Errorf("证书应同时支持 clientAuth 与 serverAuth，实际 %v", cert.ExtKeyUsage)
	}

	// 私钥可用
	if _, err := tls.X509KeyPair(certPEM, keyPEM); err != nil {
		t.Errorf("证书与私钥应配对: %v", err)
	}
}

func TestIssueClientCertValidation(t *testing.T) {
	ca, _, _ := LoadOrCreateCA(t.TempDir())
	if _, _, err := ca.IssueClientCert(""); err == nil {
		t.Error("空节点名应报错")
	}
}

func TestClientCertTTL(t *testing.T) {
	ca, _, _ := LoadOrCreateCA(t.TempDir())
	certPEM, _, err := ca.IssueClientCertWithTTL("short-lived", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := parseCertPEM(certPEM)
	life := cert.NotAfter.Sub(cert.NotBefore)
	if life > 2*time.Hour {
		t.Errorf("有效期应约为 1 小时，实际 %v", life)
	}
}

func TestServerTLSConfigRequiresClientCert(t *testing.T) {
	ca, _, _ := LoadOrCreateCA(t.TempDir())
	cfg, err := ca.ServerTLSConfig()
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	if cfg.ClientAuth != tls.RequireAndVerifyClientCert {
		t.Error("服务端应强制校验客户端证书")
	}
	if cfg.ClientCAs == nil {
		t.Error("应配置客户端 CA 池")
	}
	if len(cfg.Certificates) == 0 {
		t.Error("应配置服务端证书")
	}
	if cfg.MinVersion < tls.VersionTLS12 {
		t.Error("最低 TLS 版本不应低于 1.2")
	}
}

// 关键回归测试：证书必须包含 SAN。
// Go 1.15+ 拒绝仅依赖 CN 的证书（x509: certificate relies on legacy Common Name field），
// 该问题曾在线上导致 Daemon 无法连接 Panel。
func TestCertificatesCarrySAN(t *testing.T) {
	dir := t.TempDir()
	ca, _, _ := LoadOrCreateCA(dir)

	// 节点证书：SAN 必须包含节点名（Panel 以节点名校验 Daemon 服务端）
	nodeCertPEM, _, err := ca.IssueClientCert("node-001")
	if err != nil {
		t.Fatal(err)
	}
	nodeCert, err := parseCertPEM(nodeCertPEM)
	if err != nil {
		t.Fatal(err)
	}
	if len(nodeCert.DNSNames) == 0 || nodeCert.DNSNames[0] != "node-001" {
		t.Errorf("节点证书 SAN 应包含节点名，实际 %v", nodeCert.DNSNames)
	}

	// Panel 服务端证书
	srvPEM, _, err := ca.PanelServerCert()
	if err != nil {
		t.Fatal(err)
	}
	srvCert, err := parseCertPEM(srvPEM)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, d := range srvCert.DNSNames {
		if d == PanelGRPCServerName {
			found = true
		}
	}
	if !found {
		t.Errorf("Panel 服务端证书 SAN 应包含 %s，实际 %v", PanelGRPCServerName, srvCert.DNSNames)
	}
	if len(srvCert.IPAddresses) == 0 {
		t.Error("Panel 服务端证书应包含 IP SAN")
	}
}

// 以 SAN 名称校验节点证书，模拟 Panel→Daemon 的服务端校验。
func TestNodeCertVerifiesWithServerName(t *testing.T) {
	ca, _, _ := LoadOrCreateCA(t.TempDir())
	certPEM, _, err := ca.IssueClientCert("node-001")
	if err != nil {
		t.Fatal(err)
	}
	cert, _ := parseCertPEM(certPEM)

	if _, err := cert.Verify(x509.VerifyOptions{
		Roots:     ca.CAPool(),
		DNSName:   "node-001",
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}); err != nil {
		t.Errorf("按 SAN 名称校验服务端证书应通过: %v", err)
	}

	if _, err := cert.Verify(x509.VerifyOptions{
		Roots:     ca.CAPool(),
		DNSName:   "node-999",
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}); err == nil {
		t.Error("名称不匹配时必须校验失败")
	}
}

func TestPanelServerCertPersisted(t *testing.T) {
	dir := t.TempDir()
	ca, _, _ := LoadOrCreateCA(dir)

	c1, _, err := ca.PanelServerCert()
	if err != nil {
		t.Fatal(err)
	}
	c2, _, err := ca.PanelServerCert()
	if err != nil {
		t.Fatal(err)
	}
	if string(c1) != string(c2) {
		t.Error("重复调用应返回同一份持久化服务端证书")
	}
}

func TestClientTLSConfig(t *testing.T) {
	ca, _, _ := LoadOrCreateCA(t.TempDir())
	certPEM, keyPEM, _ := ca.IssueClientCert("node-x")

	cfg, err := ClientTLSConfig(ca.CertPEM, certPEM, keyPEM)
	if err != nil {
		t.Fatalf("构造客户端配置失败: %v", err)
	}
	if len(cfg.Certificates) != 1 {
		t.Error("应包含客户端证书")
	}
	if cfg.RootCAs == nil {
		t.Error("应包含根证书池")
	}

	// 覆盖 ServerName
	cfg2, err := ClientTLSConfigWithName(ca.CertPEM, certPEM, keyPEM, "node-x")
	if err != nil {
		t.Fatal(err)
	}
	if cfg2.ServerName != "node-x" {
		t.Errorf("ServerName 应被覆盖，实际 %s", cfg2.ServerName)
	}
}

func TestClientTLSConfigRejectsBadInput(t *testing.T) {
	ca, _, _ := LoadOrCreateCA(t.TempDir())
	certPEM, keyPEM, _ := ca.IssueClientCert("n")

	if _, err := ClientTLSConfig([]byte("not a pem"), certPEM, keyPEM); err == nil {
		t.Error("非法 CA 应报错")
	}
	if _, err := ClientTLSConfig(ca.CertPEM, []byte("bad"), keyPEM); err == nil {
		t.Error("非法客户端证书应报错")
	}
	// 证书与私钥不匹配
	_, otherKey, _ := ca.IssueClientCert("other")
	if _, err := ClientTLSConfig(ca.CertPEM, certPEM, otherKey); err == nil {
		t.Error("证书与私钥不匹配应报错")
	}
}

func TestPanelClientCertPersisted(t *testing.T) {
	dir := t.TempDir()
	ca, _, _ := LoadOrCreateCA(dir)

	c1, k1, err := ca.PanelClientCert()
	if err != nil {
		t.Fatalf("生成面板客户端证书失败: %v", err)
	}
	c2, k2, err := ca.PanelClientCert()
	if err != nil {
		t.Fatal(err)
	}
	if string(c1) != string(c2) || string(k1) != string(k2) {
		t.Error("重复调用应返回同一份持久化证书")
	}

	// 私钥文件权限
	fi, err := os.Stat(filepath.Join(dir, "panel-client.key"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Errorf("面板客户端私钥权限应为 0600，实际 %o", fi.Mode().Perm())
	}
}

func TestServerTLSConfigFromFiles(t *testing.T) {
	dir := t.TempDir()
	ca, _, _ := LoadOrCreateCA(t.TempDir())

	// 使用节点证书作为服务端证书
	certPEM, keyPEM, _ := ca.IssueClientCert("node-server")
	caFile := filepath.Join(dir, "ca.crt")
	certFile := filepath.Join(dir, "node.crt")
	keyFile := filepath.Join(dir, "node.key")
	_ = os.WriteFile(caFile, ca.CertPEM, 0o644)
	_ = os.WriteFile(certFile, certPEM, 0o644)
	_ = os.WriteFile(keyFile, keyPEM, 0o600)

	cfg, err := ServerTLSConfigFromFiles(caFile, certFile, keyFile)
	if err != nil {
		t.Fatalf("构造失败: %v", err)
	}
	if cfg.ClientAuth != tls.RequireAndVerifyClientCert {
		t.Error("应强制校验客户端证书")
	}

	// 缺失文件应报错
	if _, err := ServerTLSConfigFromFiles("/no/ca", certFile, keyFile); err == nil {
		t.Error("缺失 CA 文件应报错")
	}
	if _, err := ServerTLSConfigFromFiles(caFile, "/no/cert", keyFile); err == nil {
		t.Error("缺失证书文件应报错")
	}
}

func TestDialerServerName(t *testing.T) {
	// Panel gRPC 服务端使用固定的专用证书，因此无论连接地址如何都用同一校验名
	// 用 TEST-NET-3（203.0.113.0/24，RFC 5737 专供文档/测试）而不是 192.168.x ——
	// 后者看着像真实内网地址，公开仓库里没必要留这种字样
	if got := DialerServerName("203.0.113.7"); got != PanelGRPCServerName {
		t.Errorf("IP 地址应使用固定 ServerName，实际 %s", got)
	}
	if got := DialerServerName("node.example.com"); got != PanelGRPCServerName {
		t.Errorf("域名同样使用固定 ServerName，实际 %s", got)
	}
}

func parseCertPEM(certPEM []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return nil, x509.ErrUnsupportedAlgorithm
	}
	return x509.ParseCertificate(block.Bytes)
}
