package daemon

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// 「③ 节点发起」的接入请求。
//
// 与「② 面板发起」的区别只在**谁先说话**：那边是面板签 token、管理员贴到节点执行；
// 这里反过来 —— 节点自己生成密钥对与请求，管理员把一行字贴回面板。
//
// 安全依据是密码学的，不依赖传输信道：
//   · 请求里带节点私钥生成的签名，面板用**请求内 CSR 的公钥**验签；
//     验签通过即证明发起方持有对应私钥 —— 哪怕字符串被旁观者抄走也用不了；
//   · 请求带过期时间，粘贴晚了会被拒；
//   · 面板导入后记录公钥指纹，之后续期必须沿用同一把钥匙。
//
// 这比 ② 更强：② 在首次接入的 15 分钟窗口里，只能靠「管理员亲手粘贴」这个信道保证不被人截走。

// joinRequestVersion 请求格式版本（将来改格式时用它区分）。
const joinRequestVersion = 1

// JoinRequest 节点生成的接入请求载荷。
type JoinRequest struct {
	V         int    `json:"v"`
	NodeUID   string `json:"node_uid"`
	NodeName  string `json:"node_name"`
	Hostname  string `json:"hostname,omitempty"`
	CSR       string `json:"csr"`
	ExpiresAt int64  `json:"exp"`
	Nonce     string `json:"nonce"`
}

// BuildJoinRequest 生成接入请求字符串（形如 atl-join-v1.<payload>.<sig>）。
func BuildJoinRequest(o EnrollOptions, ttl time.Duration) (string, error) {
	name := o.NodeName
	if name == "" {
		name = hostnameOrEmpty()
	}
	if name == "" {
		return "", fmt.Errorf("无法确定节点名，请用 --name 指定")
	}
	uid, err := ensureNodeUIDAt(o.StateDir)
	if err != nil {
		return "", fmt.Errorf("生成/读取 node_uid 失败: %w", err)
	}
	key, err := loadOrCreateJoinKey(o.CertDir)
	if err != nil {
		return "", err
	}
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: name},
	}, key)
	if err != nil {
		return "", fmt.Errorf("生成 CSR 失败: %w", err)
	}
	nonce, err := randomNonce()
	if err != nil {
		return "", err
	}
	req := JoinRequest{
		V:        joinRequestVersion,
		NodeUID:  uid,
		NodeName: name,
		Hostname: hostnameOrEmpty(),
		CSR:      string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER})),
		ExpiresAt: time.Now().Add(ttl).Unix(),
		Nonce:     nonce,
	}
	payload, err := json.Marshal(req)
	if err != nil {
		return "", err
	}
	b64 := base64.RawURLEncoding.EncodeToString(payload)
	sum := sha256.Sum256([]byte(b64))
	sig, err := ecdsa.SignASN1(rand.Reader, key, sum[:])
	if err != nil {
		return "", fmt.Errorf("签名失败: %w", err)
	}
	return "atl-join-v1." + b64 + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// loadOrCreateJoinKey 读取或生成本机的接入密钥（<certDir>/join.key，0600）。
//
// 复用同一把：身份跟着钥匙走。换了钥匙就等于换了一台「机器」——
// 面板那边记录的 pubkey_fp 会对不上，续期会被拒。
func loadOrCreateJoinKey(certDir string) (*ecdsa.PrivateKey, error) {
	if certDir == "" {
		certDir = "certs"
	}
	if err := os.MkdirAll(certDir, 0o700); err != nil {
		return nil, err
	}
	p := filepath.Join(certDir, "join.key")
	if raw, err := os.ReadFile(p); err == nil {
		if blk, _ := pem.Decode(raw); blk != nil {
			if k, err := x509.ParseECPrivateKey(blk.Bytes); err == nil {
				return k, nil
			}
		}
	}
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	der, err := x509.MarshalECPrivateKey(k)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(p, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		return nil, err
	}
	return k, nil
}

func randomNonce() (string, error) {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
