package pki

import (
	"crypto/tls"
	"crypto/x509"
	"net"
	"testing"
	"time"
)

// tlsVerifyOptions 构造按名称校验服务端证书的选项。
func tlsVerifyOptions(ca *CA, dnsName string) x509.VerifyOptions {
	return x509.VerifyOptions{
		Roots:     ca.CAPool(),
		DNSName:   dnsName,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
}

// startMTLSServer 启动一个使用 Panel mTLS 配置的 TLS 监听器。
func startMTLSServer(t *testing.T, ca *CA) string {
	t.Helper()
	cfg, err := ca.ServerTLSConfig()
	if err != nil {
		t.Fatalf("构造服务端配置失败: %v", err)
	}
	lis, err := tls.Listen("tcp", "127.0.0.1:0", cfg)
	if err != nil {
		t.Fatalf("监听失败: %v", err)
	}
	t.Cleanup(func() { _ = lis.Close() })

	go func() {
		for {
			conn, err := lis.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_, _ = c.Write([]byte("ok"))
			}(conn)
		}
	}()
	return lis.Addr().String()
}

// dialTLS 尝试用给定配置完成握手，返回是否成功。
func dialTLS(t *testing.T, addr string, cfg *tls.Config) bool {
	t.Helper()
	d := &net.Dialer{Timeout: 3 * time.Second}
	conn, err := tls.DialWithDialer(d, "tcp", addr, cfg)
	if err != nil {
		return false
	}
	defer conn.Close()
	buf := make([]byte, 2)
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, err = conn.Read(buf)
	return err == nil
}

// TestMTLSEnforcement 验证 mTLS 的强制校验行为：
//   - 带合法节点证书 → 成功
//   - 不带客户端证书 → 失败
//   - 用其它 CA 签发的证书 → 失败
//   - 明文非 TLS 连接 → 失败
func TestMTLSEnforcement(t *testing.T) {
	ca, _, err := LoadOrCreateCA(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	addr := startMTLSServer(t, ca)

	// 1. 合法节点证书（CA 签发）
	certPEM, keyPEM, err := ca.IssueClientCert("node-001")
	if err != nil {
		t.Fatal(err)
	}
	good, err := ClientTLSConfig(ca.CertPEM, certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	if !dialTLS(t, addr, good) {
		t.Error("携带合法节点证书应握手成功")
	}

	// 2. 不带客户端证书
	noCert := &tls.Config{RootCAs: ca.CAPool(), ServerName: PanelGRPCServerName, MinVersion: tls.VersionTLS12}
	if dialTLS(t, addr, noCert) {
		t.Error("未提供客户端证书时必须握手失败（否则 mTLS 形同虚设）")
	}

	// 3. 由其它 CA 签发的证书（模拟伪造节点）
	otherCA, _, err := LoadOrCreateCA(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	rogueCert, rogueKey, err := otherCA.IssueClientCert("node-001")
	if err != nil {
		t.Fatal(err)
	}
	rogue, err := ClientTLSConfig(otherCA.CertPEM, rogueCert, rogueKey)
	if err != nil {
		t.Fatal(err)
	}
	if dialTLS(t, addr, rogue) {
		t.Error("其它 CA 签发的证书必须被拒绝（否则可伪装成节点）")
	}

	// 4. 无 TLS 的明文连接
	plain, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err == nil {
		_ = plain.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
		_, _ = plain.Write([]byte("GET / HTTP/1.0\r\n\r\n"))
		buf := make([]byte, 1)
		n, _ := plain.Read(buf)
		_ = plain.Close()
		if n > 0 {
			t.Error("明文连接不应得到有效响应")
		}
	}
}

// TestServerCertVerifiableByClients 验证 Daemon 侧能用 CA 校验 Panel 服务端证书。
func TestServerCertVerifiableByClients(t *testing.T) {
	ca, _, err := LoadOrCreateCA(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	srvCertPEM, _, err := ca.PanelServerCert()
	if err != nil {
		t.Fatal(err)
	}
	cert, err := parseCertPEM(srvCertPEM)
	if err != nil {
		t.Fatal(err)
	}

	// 以 Daemon 使用的名称校验（这正是线上曾因缺少 SAN 而失败的一步）
	if _, err := cert.Verify(tlsVerifyOptions(ca, PanelGRPCServerName)); err != nil {
		t.Errorf("客户端应能校验服务端证书: %v", err)
	}
	// 名称不匹配必须失败
	if _, err := cert.Verify(tlsVerifyOptions(ca, "wrong-name")); err == nil {
		t.Error("名称不匹配时必须校验失败")
	}
}
