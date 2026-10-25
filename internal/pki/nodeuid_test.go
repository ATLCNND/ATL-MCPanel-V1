package pki

import (
	"crypto/x509"
	"encoding/pem"
	"testing"
	"time"
)

// node_uid 必须真的出现在证书的 URI SAN 里 —— 这是「改名不换身份」的地基：
// CN/DNS 是显示名（可改），URI 才是这台机器的稳定身份（凭证交换、多面板都靠它）。
func TestIssueClientCertCarriesNodeUID(t *testing.T) {
	dir := t.TempDir()
	ca, _, err := LoadOrCreateCA(dir)
	if err != nil {
		t.Fatalf("建 CA 失败: %v", err)
	}
	const uid = "be8e87c4-1921-48a8-b293-b6cfb00d5137"
	certPEM, _, err := ca.IssueClientCertWithUID("节点A", uid, time.Hour)
	if err != nil {
		t.Fatalf("签发失败: %v", err)
	}
	blk, _ := pem.Decode(certPEM)
	if blk == nil {
		t.Fatal("解码证书失败")
	}
	cert, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		t.Fatalf("解析证书失败: %v", err)
	}
	if got := CertNodeUID(cert); got != uid {
		t.Errorf("证书里的 node_uid = %q，期望 %q", got, uid)
	}
	// 显示名仍在 CN/DNS 上（老校验逻辑不受影响）
	if cert.Subject.CommonName != CertNodeName("节点A") {
		t.Errorf("CN = %q，期望 %q", cert.Subject.CommonName, CertNodeName("节点A"))
	}
	if len(cert.DNSNames) == 0 || cert.DNSNames[0] != CertNodeName("节点A") {
		t.Errorf("DNS SAN = %v", cert.DNSNames)
	}
	// 双向用途仍要保留：Daemon 既是服务端也是客户端
	var client, server bool
	for _, u := range cert.ExtKeyUsage {
		if u == x509.ExtKeyUsageClientAuth {
			client = true
		}
		if u == x509.ExtKeyUsageServerAuth {
			server = true
		}
	}
	if !client || !server {
		t.Errorf("用途缺失: client=%v server=%v", client, server)
	}
}

// 没有 node_uid 时（老节点/老调用方）不能带 URI SAN，且其余行为与以前一致。
func TestIssueClientCertWithoutUIDUnchanged(t *testing.T) {
	dir := t.TempDir()
	ca, _, err := LoadOrCreateCA(dir)
	if err != nil {
		t.Fatalf("建 CA 失败: %v", err)
	}
	certPEM, _, err := ca.IssueClientCert("node-01")
	if err != nil {
		t.Fatalf("签发失败: %v", err)
	}
	blk, _ := pem.Decode(certPEM)
	cert, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		t.Fatalf("解析证书失败: %v", err)
	}
	if got := CertNodeUID(cert); got != "" {
		t.Errorf("没有 uid 时不该带 URI SAN，实际 %q", got)
	}
	if len(cert.URIs) != 0 {
		t.Errorf("URIs = %v，期望为空", cert.URIs)
	}
}