package pki

import (
	"crypto/x509"
	"encoding/pem"
	"strings"
	"testing"
)

// newTestCA 用现有的 LoadOrCreateCA 建一个临时 CA，风格与 pki_test.go 保持一致。
func newTestCA(t *testing.T) *CA {
	t.Helper()
	ca, _, err := LoadOrCreateCA(t.TempDir())
	if err != nil {
		t.Fatalf("建 CA 失败: %v", err)
	}
	return ca
}

// parseCert 从 PEM 里取出第一张证书。
func parseCert(t *testing.T, pemBytes []byte) *x509.Certificate {
	t.Helper()
	rest := pemBytes
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			t.Fatal("PEM 里没有 CERTIFICATE 块")
		}
		if block.Type == "CERTIFICATE" {
			c, err := x509.ParseCertificate(block.Bytes)
			if err != nil {
				t.Fatalf("解析证书失败: %v", err)
			}
			return c
		}
	}
}

func TestCertNodeName(t *testing.T) {
	cases := []struct {
		in       string
		wantSame bool   // true = 应与输入相同（纯 ASCII 原样保留）
		wantPfx  string // 期望前缀（wantSame 为 false 时用）
	}{
		{"node-001", true, ""},
		{"HK-8c8g10m", true, ""},
		{"hk.node.example.com", true, ""},
		{"node_1", true, ""},
		// 真实踩到的那个名字
		{"HK测试8c8g10m", false, "HK-8c8g10m-"},
		{"测试节点", false, "node-"},
		{"我的 节点 1", false, "1-"},
		{"  spaced  ", true, ""}, // TrimSpace 之后仍是纯 ASCII
	}
	for _, c := range cases {
		got := certNodeName(c.in)
		if got == "" {
			t.Errorf("certNodeName(%q) 返回空", c.in)
			continue
		}
		if c.wantSame {
			if got != strings.TrimSpace(c.in) {
				t.Errorf("certNodeName(%q) = %q，期望原样保留", c.in, got)
			}
		} else if !strings.HasPrefix(got, c.wantPfx) {
			t.Errorf("certNodeName(%q) = %q，期望以 %q 开头", c.in, got, c.wantPfx)
		}
		// 无论哪条路径，结果都必须是纯 ASCII 且长度合规
		for i := 0; i < len(got); i++ {
			if got[i] > 127 {
				t.Errorf("certNodeName(%q) = %q 含非 ASCII 字节", c.in, got)
				break
			}
		}
		if len(got) > 63 {
			t.Errorf("certNodeName(%q) 长度 %d 超过 63", c.in, len(got))
		}
	}
}

func TestCertNodeNameEmptyAndUnique(t *testing.T) {
	if got := certNodeName(""); got != "" {
		t.Errorf("空名字应返回空串，实际 %q", got)
	}
	if got := certNodeName("   "); got != "" {
		t.Errorf("全空白应返回空串，实际 %q", got)
	}
	// 两个会被清成同一前缀的中文名，必须靠哈希区分开
	a := certNodeName("测试1")
	b := certNodeName("測試1")
	if a == b {
		t.Errorf("不同节点名清洗后撞车了：%q", a)
	}
	// 稳定性：同样的输入永远得到同样的结果（签发与拨号必须一致）
	if certNodeName("测试1") != a {
		t.Error("certNodeName 不稳定")
	}
}

// 关键回归：中文节点名必须能签出证书（这正是当初失败的地方）。
func TestIssueClientCertWithChineseName(t *testing.T) {
	ca := newTestCA(t)
	certPEM, keyPEM, err := ca.IssueClientCert("HK测试8c8g10m")
	if err != nil {
		t.Fatalf("中文节点名签发失败（这正是要修的 bug）: %v", err)
	}
	if len(certPEM) == 0 || len(keyPEM) == 0 {
		t.Fatal("签发出的证书/私钥为空")
	}
	cert := parseCert(t, certPEM)
	want := certNodeName("HK测试8c8g10m")
	if cert.Subject.CommonName != want {
		t.Errorf("CN = %q，期望 %q", cert.Subject.CommonName, want)
	}
	if len(cert.DNSNames) != 1 || cert.DNSNames[0] != want {
		t.Errorf("DNS SAN = %v，期望 [%s]", cert.DNSNames, want)
	}
}

// 成对性：签发用的名字与拨号校验用的名字必须一致，否则 mTLS 必失败。
func TestCertNameAndDialNameAgree(t *testing.T) {
	ca := newTestCA(t)
	name := "HK测试8c8g10m"
	certPEM, keyPEM, err := ca.IssueClientCert(name)
	if err != nil {
		t.Fatalf("签发失败: %v", err)
	}
	cfg, err := ClientTLSConfigWithName(ca.CertPEM, certPEM, keyPEM, name)
	if err != nil {
		t.Fatalf("构造客户端配置失败: %v", err)
	}
	if cfg.ServerName != certNodeName(name) {
		t.Errorf("拨号 ServerName = %q，与证书 SAN %q 不一致 —— mTLS 会失败",
			cfg.ServerName, certNodeName(name))
	}
	// 真的用它校验一次证书链，确保 SAN 匹配
	cert := parseCert(t, certPEM)
	pool := x509.NewCertPool()
	pool.AddCert(ca.Cert)
	if _, err := cert.Verify(x509.VerifyOptions{
		Roots:     pool,
		DNSName:   cfg.ServerName,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}); err != nil {
		t.Errorf("用清洗后的名字校验证书失败: %v", err)
	}
}

// ---- 测试辅助（定义在文件顶部：newTestCA / parseCert）----
