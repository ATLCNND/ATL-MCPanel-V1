package daemon

import (
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 2026-11-08 干净环境上真机验收 A-46 时发现：`enroll --init` 走完、管理员把请求
// 贴进面板、面板也签了证书 —— 节点启动却报 `open .../ca.crt: no such file or
// directory`。原因是证书随 HTTP 响应回到了管理员浏览器，节点自己没参与。
// 这组用例把"节点该去领回来"这条补上的路径钉死。
func TestPickupCertFetchesAndWritesFiles(t *testing.T) {
	dir := t.TempDir()
	certDir := filepath.Join(dir, "certs")
	stateDir := filepath.Join(dir, "state")
	if err := os.MkdirAll(certDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	const uid = "11111111-2222-3333-4444-555555555555"
	if err := os.WriteFile(filepath.Join(stateDir, "node_uid"), []byte(uid+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	key, err := loadOrCreateJoinKey(certDir)
	if err != nil {
		t.Fatal(err)
	}

	var gotUID, gotCSR string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/enroll/renew" {
			t.Errorf("领证书应打 /api/enroll/renew，实际 %s", r.URL.Path)
		}
		var body struct {
			NodeUID string `json:"node_uid"`
			CSR     string `json:"csr"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		gotUID, gotCSR = body.NodeUID, body.CSR
		_, _ = w.Write([]byte(`{"cert":"CERTPEM","ca":"CAPEM"}`))
	}))
	defer srv.Close()

	certPath, caPath, err := PickupCert(EnrollOptions{
		PanelBase: srv.URL, CertDir: certDir, StateDir: stateDir,
	})
	if err != nil {
		t.Fatalf("PickupCert 失败: %v", err)
	}
	if gotUID != uid {
		t.Errorf("请求里的 node_uid=%q，期望 %q（必须与 state 里同一份，否则面板认不出这台机器）", gotUID, uid)
	}
	// CSR 的公钥必须就是我们那把 join.key —— 换钥匙等于换机器，面板会按指纹拒掉。
	blk, _ := pem.Decode([]byte(gotCSR))
	if blk == nil {
		t.Fatal("请求里的 CSR 不是合法 PEM")
	}
	csr, err := x509.ParseCertificateRequest(blk.Bytes)
	if err != nil {
		t.Fatalf("解析 CSR 失败: %v", err)
	}
	pub, ok := csr.PublicKey.(*ecdsa.PublicKey)
	if !ok || !pub.Equal(&key.PublicKey) {
		t.Error("CSR 用的不是 join.key 那把钥匙")
	}

	if b, err := os.ReadFile(certPath); err != nil || string(b) != "CERTPEM" {
		t.Errorf("node.crt 内容不对: %q err=%v", string(b), err)
	}
	if b, err := os.ReadFile(caPath); err != nil || string(b) != "CAPEM" {
		t.Errorf("ca.crt 内容不对: %q err=%v", string(b), err)
	}
	keyPEM, err := os.ReadFile(filepath.Join(certDir, "node.key"))
	if err != nil {
		t.Fatalf("没写出 node.key: %v", err)
	}
	kb, _ := pem.Decode(keyPEM)
	if kb == nil {
		t.Fatal("node.key 不是合法 PEM")
	}
	outKey, err := x509.ParseECPrivateKey(kb.Bytes)
	if err != nil {
		t.Fatalf("node.key 解析失败: %v", err)
	}
	if !outKey.PublicKey.Equal(&key.PublicKey) {
		t.Error("node.key 与 join.key 不是同一把钥匙（面板下一次续期会判「换了钥匙」）")
	}
	if st, err := os.Stat(filepath.Join(certDir, "node.key")); err == nil && st.Mode().Perm() != 0o600 {
		t.Errorf("node.key 权限 = %v，期望 0600", st.Mode().Perm())
	}
}

// 面板还没导入接入请求时，必须能被上层识别成"再等等"，而不是当成致命错误。
func TestPickupCertNotImportedIsRecognizable(t *testing.T) {
	dir := t.TempDir()
	certDir := filepath.Join(dir, "certs")
	stateDir := filepath.Join(dir, "state")
	_ = os.MkdirAll(certDir, 0o700)
	_ = os.MkdirAll(stateDir, 0o700)
	_ = os.WriteFile(filepath.Join(stateDir, "node_uid"), []byte("uid-x\n"), 0o600)
	if _, err := loadOrCreateJoinKey(certDir); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"找不到该节点记录，请重新走一次凭证交换"}`))
	}))
	defer srv.Close()

	_, _, err := PickupCert(EnrollOptions{PanelBase: srv.URL, CertDir: certDir, StateDir: stateDir})
	if !errors.Is(err, ErrJoinNotImported) {
		t.Fatalf("期望 ErrJoinNotImported，实际 %v", err)
	}
	if !strings.Contains(err.Error(), "找不到该节点记录") {
		t.Errorf("错误信息应带上面板原话，便于排查: %v", err)
	}
}

// 没有 join.key 的机器（② 面板发起装的、或压根没接入过）不该被这条路径接管。
func TestPickupCertWithoutJoinKeyFails(t *testing.T) {
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	_ = os.MkdirAll(stateDir, 0o700)
	_ = os.WriteFile(filepath.Join(stateDir, "node_uid"), []byte("uid-y\n"), 0o600)

	_, _, err := PickupCert(EnrollOptions{
		PanelBase: "http://127.0.0.1:1", CertDir: filepath.Join(dir, "certs"), StateDir: stateDir,
	})
	if err == nil || !strings.Contains(err.Error(), "接入密钥") {
		t.Fatalf("缺少 join.key 时应给出可读错误，实际 %v", err)
	}
}
