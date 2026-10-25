package pki

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"net/url"
	"time"
)

// SignCSR 为**节点自己生成的密钥**签发客户端证书（凭证交换的核心）。
//
// 为什么必须签 CSR 而不是像一键部署那样下发私钥：
// 走凭证交换时，节点是从公网/异地接入的，私钥一旦经过网络，token 泄露 ≈ 身份泄露。
// 签 CSR 的语义是「你给我公钥，我给你签名」—— 私钥始终不出节点。
//
// 校验要求（任何一条不过就拒绝，不做宽容）：
//   1. CSR 的**自签名有效**（证明申请方持有对应私钥，防止拿别人公钥来冒名）；
//   2. 公钥类型限定 ECDSA P-256 或 Ed25519（不接受 RSA：密钥太大、与现有实现不一致）；
//   3. nodeName 能归一化成证书标识、nodeUID 非空（稳定身份是这套流程的意义所在）。
func (ca *CA) SignCSR(csrPEM []byte, nodeName, nodeUID string, ttl time.Duration) (certPEM []byte, err error) {
	if len(csrPEM) == 0 {
		return nil, fmt.Errorf("CSR 为空")
	}
	if nodeUID == "" {
		return nil, fmt.Errorf("node_uid 不能为空（凭证交换以稳定身份为前提）")
	}
	name := certNodeName(nodeName)
	if name == "" {
		return nil, fmt.Errorf("节点名 %q 无法转换成可用的证书标识", nodeName)
	}

	blk, _ := pem.Decode(csrPEM)
	if blk == nil {
		return nil, fmt.Errorf("CSR 不是合法的 PEM")
	}
	csr, err := x509.ParseCertificateRequest(blk.Bytes)
	if err != nil {
		return nil, fmt.Errorf("解析 CSR 失败: %w", err)
	}
	if err := csr.CheckSignature(); err != nil {
		return nil, fmt.Errorf("CSR 自签名无效（申请方并不持有对应私钥）: %w", err)
	}
	switch pub := csr.PublicKey.(type) {
	case *ecdsa.PublicKey:
		if pub.Curve != elliptic.P256() {
			return nil, fmt.Errorf("只接受 ECDSA P-256 公钥，收到 %s", pub.Curve.Params().Name)
		}
	case ed25519.PublicKey:
		// 允许，长度由类型保证
	default:
		return nil, fmt.Errorf("不支持的公钥类型 %T（只接受 ECDSA P-256 与 Ed25519）", csr.PublicKey)
	}

	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	uri, err := NodeUIDURI(nodeUID)
	if err != nil {
		return nil, fmt.Errorf("node_uid 无法编码进证书: %w", err)
	}
	var uris []*url.URL
	if uri != nil {
		uris = []*url.URL{uri}
	}
	tpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: name, Organization: []string{"ATL-MCPanel Node"}},
		NotBefore:    now.Add(-5 * time.Minute),
		NotAfter:     now.Add(ttl),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		// 与一键部署签发保持一致：Daemon 既是服务端（Panel 调它）也是客户端（它连 Panel）
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
		DNSNames:    []string{name},
		URIs:        uris,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, ca.Cert, csr.PublicKey, ca.Key)
	if err != nil {
		return nil, fmt.Errorf("签发证书失败: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), nil
}
