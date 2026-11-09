package daemon

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// 「③ 节点发起」的**下半段**：把面板已经签好的证书领回本机。
//
// 为什么需要这个：`enroll --init` 只做了一半 —— 它把 CSR 交给管理员贴进面板，
// 面板随后签发证书并通过 HTTP 响应返回给**管理员的浏览器**。节点这边
// 从头到尾没有参与那次响应，于是证书留在面板侧、节点上仍然没有 node.crt。
// 结果是「面板显示证书已签发、节点启动时报 open .../ca.crt: no such file
// or directory」—— 换来换去都动不了。（2026-11-08 在干净环境上真机验收
// A-46 时抓到，此前只在同一台机器上验证过 ②面板发起。）
//
// 领回用的不是 token，而是 `/api/enroll/renew`：它的凭据就是**私钥本身** ——
// 面板拿"首次登记的公钥指纹"与本次 CSR 比对，而 CSR 自带自签名校验，
// 能签出这个 CSR 就证明持有私钥。所以节点不需要保存任何秘密字符串，
// 也不需要管理员再粘一次任何东西。

// ErrJoinNotImported 表示面板那边还查不到这台节点 —— 通常是管理员还没把
// `enroll --init` 打印的那一行贴进面板。调用方可以据此决定"再等等"。
var ErrJoinNotImported = errors.New("面板尚未导入这台节点的接入请求")

// PickupCert 用 `<certDir>/join.key` 与已登记的 node_uid 向面板领回证书。
//
// 成功后把 node.crt / node.key / ca.crt 写到 CertDir（与 Enroll 相同布局）。
func PickupCert(o EnrollOptions) (certPath, caPath string, err error) {
	if strings.TrimSpace(o.PanelBase) == "" {
		return "", "", fmt.Errorf("未配置面板地址（daemon.panel_url），无法自动领回证书")
	}
	uid, err := readNodeUID(o.StateDir)
	if err != nil {
		return "", "", err
	}
	certDir := o.CertDir
	if certDir == "" {
		certDir = "certs"
	}
	keyPath := filepath.Join(certDir, "join.key")
	raw, err := os.ReadFile(keyPath)
	if err != nil {
		return "", "", fmt.Errorf("读取接入密钥失败（这台节点不是用 enroll --init 发起接入的）: %w", err)
	}
	blk, _ := pem.Decode(raw)
	if blk == nil {
		return "", "", fmt.Errorf("接入密钥 %s 不是合法的 PEM", keyPath)
	}
	key, err := x509.ParseECPrivateKey(blk.Bytes)
	if err != nil {
		return "", "", fmt.Errorf("解析接入密钥失败: %w", err)
	}

	csrPEM, err := csrFromKey(key, o.NodeName)
	if err != nil {
		return "", "", err
	}
	body, _ := json.Marshal(map[string]string{"node_uid": uid, "csr": string(csrPEM)})
	url := strings.TrimRight(o.PanelBase, "/") + "/api/enroll/renew"
	httpReq, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", "", err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 30 * time.Second}
	httpResp, err := client.Do(httpReq)
	if err != nil {
		return "", "", fmt.Errorf("请求面板失败: %w", err)
	}
	defer httpResp.Body.Close()
	respRaw, _ := io.ReadAll(io.LimitReader(httpResp.Body, 1<<20))
	var resp enrollResponse
	_ = json.Unmarshal(respRaw, &resp)
	if httpResp.StatusCode != http.StatusOK {
		msg := resp.Error
		if msg == "" {
			msg = truncate(string(respRaw), 200)
		}
		// 面板对「这台节点没登记过」的答复是 403 + 这句话（见 handleEnrollRenew），
		// 把它翻译成一个可判别的错误，好让启动流程"再等一会儿"而不是直接退出。
		if httpResp.StatusCode == http.StatusForbidden && strings.Contains(msg, "找不到该节点记录") {
			return "", "", fmt.Errorf("%w（%s）", ErrJoinNotImported, msg)
		}
		return "", "", fmt.Errorf("面板拒绝了这次领取（HTTP %d）：%s", httpResp.StatusCode, msg)
	}
	if resp.Cert == "" || resp.CA == "" {
		return "", "", fmt.Errorf("面板返回的证书或 CA 为空")
	}

	if err := os.MkdirAll(certDir, 0o700); err != nil {
		return "", "", err
	}
	certPath = filepath.Join(certDir, "node.crt")
	keyOut := filepath.Join(certDir, "node.key")
	caPath = filepath.Join(certDir, "ca.crt")
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return "", "", err
	}
	if err := os.WriteFile(certPath, []byte(resp.Cert), 0o644); err != nil {
		return "", "", err
	}
	if err := os.WriteFile(keyOut, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		return "", "", err
	}
	if err := os.WriteFile(caPath, []byte(resp.CA), 0o644); err != nil {
		return "", "", err
	}
	return certPath, caPath, nil
}

// readNodeUID 读运行期同一份 state/node_uid；不存在说明这台机器还没发起过接入。
func readNodeUID(stateDir string) (string, error) {
	if stateDir == "" {
		stateDir = "state"
	}
	b, err := os.ReadFile(filepath.Join(stateDir, "node_uid"))
	if err != nil {
		return "", fmt.Errorf("读取 node_uid 失败（这台节点还没跑过 enroll --init）: %w", err)
	}
	uid := strings.TrimSpace(string(b))
	if uid == "" {
		return "", fmt.Errorf("node_uid 为空（%s）", filepath.Join(stateDir, "node_uid"))
	}
	return uid, nil
}

// csrFromKey 用已有私钥生成 CSR（CN 只用来说明"这是谁"，真正比对的是公钥指纹）。
func csrFromKey(key *ecdsa.PrivateKey, name string) ([]byte, error) {
	if name == "" {
		name = hostnameOrEmpty()
	}
	if name == "" {
		name = "atl-node"
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: name},
	}, key)
	if err != nil {
		return nil, fmt.Errorf("生成 CSR 失败: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}), nil
}
