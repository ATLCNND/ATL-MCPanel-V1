package daemon

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// 凭证交换的**节点侧**（M3 下半）。
//
// 一键部署（SSH）那条路是「面板推」；这里反过来：节点自己生成密钥对与 CSR，
// 带着一次性 token 去面板换证书 —— **私钥永远不出这台机器**。
// 协议见 docs/DESIGN-CREDENTIAL-EXCHANGE.md。

// EnrollOptions enroll 子命令的参数。
type EnrollOptions struct {
	// PanelBase 面板的 HTTP 基地址，如 http://1.2.3.4:25080（带安全入口时要含 /<entry>）
	PanelBase string
	// Token 面板签发的一次性 token
	Token string
	// NodeName 显示名（留空用主机名）
	NodeName string
	// CertDir 证书落盘目录（默认 <安装目录>/certs）
	CertDir string
	// StateDir 存放 node_uid 的目录（与 daemon 运行期一致）
	StateDir string
}

type enrollResponse struct {
	Cert       string `json:"cert"`
	CA         string `json:"ca"`
	NodeUID    string `json:"node_uid"`
	PanelGRPC  string `json:"panel_grpc"`
	ExpiresAt  string `json:"expires_at"`
	RenewAfter string `json:"renew_after"`
	Error      string `json:"error"`
}

// Enroll 执行一次凭证交换：生成密钥对与 CSR → 提交 token → 落盘证书与 CA。
//
// 返回落盘的证书路径与面板建议的续期时间，供命令行打印给人看。
func Enroll(o EnrollOptions) (certPath, caPath string, resp enrollResponse, err error) {
	if strings.TrimSpace(o.PanelBase) == "" {
		return "", "", resp, fmt.Errorf("--panel 不能为空（例如 http://1.2.3.4:25080）")
	}
	if strings.TrimSpace(o.Token) == "" {
		return "", "", resp, fmt.Errorf("--token 不能为空（面板「节点管理 → 凭证交换」里签发）")
	}
	name := o.NodeName
	if name == "" {
		if h, e := os.Hostname(); e == nil {
			name = h
		}
	}
	if name == "" {
		return "", "", resp, fmt.Errorf("无法确定节点名，请用 --name 指定")
	}

	// node_uid：与运行期共用同一份（state/node_uid），保证「换证书不换身份」
	uid, err := ensureNodeUIDAt(o.StateDir)
	if err != nil {
		return "", "", resp, fmt.Errorf("生成/读取 node_uid 失败: %w", err)
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", "", resp, fmt.Errorf("生成密钥失败: %w", err)
	}
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: name},
	}, key)
	if err != nil {
		return "", "", resp, fmt.Errorf("生成 CSR 失败: %w", err)
	}
	csrPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER})

	body, _ := json.Marshal(map[string]string{
		"token": o.Token, "csr": string(csrPEM), "node_uid": uid,
		"node_name": name, "hostname": hostnameOrEmpty(),
	})
	url := strings.TrimRight(o.PanelBase, "/") + "/api/enroll"
	httpReq, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", "", resp, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 30 * time.Second}
	httpResp, err := client.Do(httpReq)
	if err != nil {
		return "", "", resp, fmt.Errorf("请求面板失败: %w", err)
	}
	defer httpResp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(httpResp.Body, 1<<20))
	if err := json.Unmarshal(raw, &resp); err != nil {
		return "", "", resp, fmt.Errorf("面板返回无法解析（HTTP %d）: %s", httpResp.StatusCode, truncate(string(raw), 200))
	}
	if httpResp.StatusCode != http.StatusOK {
		msg := resp.Error
		if msg == "" {
			msg = truncate(string(raw), 200)
		}
		return "", "", resp, fmt.Errorf("面板拒绝了这次交换（HTTP %d）：%s", httpResp.StatusCode, msg)
	}
	if resp.Cert == "" || resp.CA == "" {
		return "", "", resp, fmt.Errorf("面板返回的证书或 CA 为空")
	}

	dir := o.CertDir
	if dir == "" {
		dir = "certs"
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", "", resp, err
	}
	certPath = filepath.Join(dir, "node.crt")
	keyPath := filepath.Join(dir, "node.key")
	caPath = filepath.Join(dir, "ca.crt")
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return "", "", resp, err
	}
	if err := os.WriteFile(certPath, []byte(resp.Cert), 0o644); err != nil {
		return "", "", resp, err
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		return "", "", resp, err
	}
	if err := os.WriteFile(caPath, []byte(resp.CA), 0o644); err != nil {
		return "", "", resp, err
	}
	return certPath, caPath, resp, nil
}

// ensureNodeUIDAt 与运行期保证 node_uid 同一份实现（state/node_uid，0600）。
func ensureNodeUIDAt(stateDir string) (string, error) {
	if stateDir == "" {
		stateDir = "state"
	}
	p := filepath.Join(stateDir, "node_uid")
	if b, err := os.ReadFile(p); err == nil {
		if v := strings.TrimSpace(string(b)); v != "" {
			return v, nil
		}
	}
	u := newUUID()
	if u == "" {
		return "", fmt.Errorf("生成 UUID 失败")
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return "", err
	}
	if err := os.WriteFile(p, []byte(u+"\n"), 0o600); err != nil {
		return "", err
	}
	return u, nil
}

func hostnameOrEmpty() string {
	h, _ := os.Hostname()
	return h
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
