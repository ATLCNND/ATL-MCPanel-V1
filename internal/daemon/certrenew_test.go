package daemon

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ATLCNND/ATL-MCPanel/internal/common/config"
)

// 2026-11-09 盘点 1.0.0-preview 未做项时发现：节点证书只有 30 天，面板响应里
// 带着 renew_after，但运行期**没有任何代码**去用它（只有命令行把它打印出来）。
// 后果是部署满一个月的节点会自己掉线。这组用例把「到期前自动续期」钉死。

// selfSignedPair 造一对自签证书/私钥，用来扮演「面板签发回来的新证书」。
func selfSignedPair(t *testing.T, cn string, notAfter time.Time) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM
}

// writePair 把证书与私钥落盘（模拟一个已经接入过的节点）。
func writePair(t *testing.T, certDir string, certPEM, keyPEM []byte) (certPath, keyPath string) {
	t.Helper()
	if err := os.MkdirAll(certDir, 0o700); err != nil {
		t.Fatal(err)
	}
	certPath = filepath.Join(certDir, "node.crt")
	keyPath = filepath.Join(certDir, "node.key")
	if err := os.WriteFile(certPath, certPEM, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath
}

// 续期必须沿用 node.key：面板按「首次登记的公钥指纹」认人，换钥匙会被 403 拒掉。
func TestRenewNodeCertReusesNodeKey(t *testing.T) {
	dir := t.TempDir()
	certDir := filepath.Join(dir, "certs")
	stateDir := filepath.Join(dir, "state")
	_ = os.MkdirAll(stateDir, 0o700)
	const uid = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	if err := os.WriteFile(filepath.Join(stateDir, "node_uid"), []byte(uid+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	oldCert, oldKey := selfSignedPair(t, "node-001", time.Now().Add(2*24*time.Hour))
	_, keyPath := writePair(t, certDir, oldCert, oldKey)

	newCert, _ := selfSignedPair(t, "node-001", time.Now().Add(30*24*time.Hour))

	var gotUID, gotCSR string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/enroll/renew" {
			t.Errorf("续期应打 /api/enroll/renew，实际 %s", r.URL.Path)
		}
		var body struct {
			NodeUID string `json:"node_uid"`
			CSR     string `json:"csr"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		gotUID, gotCSR = body.NodeUID, body.CSR
		_, _ = w.Write([]byte(`{"cert":` + jsonString(string(newCert)) + `,"ca":"CAPEM","expires_at":"2030-01-01T00:00:00Z"}`))
	}))
	defer srv.Close()

	certPath, caPath, expires, err := RenewNodeCert(srv.URL, stateDir, certDir, keyPath, "node-001")
	if err != nil {
		t.Fatalf("续期失败: %v", err)
	}
	if gotUID != uid {
		t.Errorf("请求里的 node_uid=%q，期望 %q", gotUID, uid)
	}
	// CSR 的公钥必须就是磁盘上那把 node.key —— 这是面板唯一认人的依据。
	blk, _ := pem.Decode([]byte(gotCSR))
	if blk == nil {
		t.Fatal("请求里的 CSR 不是合法 PEM")
	}
	csr, err := x509.ParseCertificateRequest(blk.Bytes)
	if err != nil {
		t.Fatalf("解析 CSR 失败: %v", err)
	}
	kb, _ := pem.Decode(oldKey)
	oldPriv, err := x509.ParseECPrivateKey(kb.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	pub, ok := csr.PublicKey.(*ecdsa.PublicKey)
	if !ok || !pub.Equal(&oldPriv.PublicKey) {
		t.Error("CSR 用的不是 node.key 那把钥匙（面板会判「续期必须沿用首次登记的密钥」）")
	}

	if b, err := os.ReadFile(certPath); err != nil || string(b) != string(newCert) {
		t.Errorf("node.crt 没有被替换成新证书: err=%v", err)
	}
	if b, err := os.ReadFile(caPath); err != nil || string(b) != "CAPEM" {
		t.Errorf("ca.crt 内容不对: %q err=%v", string(b), err)
	}
	// node.key 必须原样保留（续期不是换钥匙）
	if b, err := os.ReadFile(keyPath); err != nil || string(b) != string(oldKey) {
		t.Errorf("node.key 被改动了（续期不该换私钥）: err=%v", err)
	}
	if expires.IsZero() {
		t.Error("没有解析出新的到期时间")
	}
}

// jsonString 把字符串包成 JSON 字符串字面量（测试里拼假响应体用）。
func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// 面板拒绝时必须把面板原话带出来 —— 尤其是 403「该节点已被撤销」。
func TestRenewNodeCertSurfacesPanelError(t *testing.T) {
	dir := t.TempDir()
	certDir := filepath.Join(dir, "certs")
	stateDir := filepath.Join(dir, "state")
	_ = os.MkdirAll(stateDir, 0o700)
	_ = os.WriteFile(filepath.Join(stateDir, "node_uid"), []byte("uid-z\n"), 0o600)
	certPEM, keyPEM := selfSignedPair(t, "n", time.Now().Add(time.Hour))
	_, keyPath := writePair(t, certDir, certPEM, keyPEM)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"该节点已被撤销，无法续期"}`))
	}))
	defer srv.Close()

	_, _, _, err := RenewNodeCert(srv.URL, stateDir, certDir, keyPath, "n")
	if err == nil || !strings.Contains(err.Error(), "该节点已被撤销") {
		t.Fatalf("应把面板的原话带出来，实际 %v", err)
	}
}

// 证书文件被换掉之后，握手必须用上新证书（否则续期等于白续，重启才能生效，
// 而重启 Daemon 会杀掉全部实例）。
func TestReloadableKeyPairPicksUpNewCert(t *testing.T) {
	dir := t.TempDir()
	certDir := filepath.Join(dir, "certs")
	c1, k1 := selfSignedPair(t, "first", time.Now().Add(24*time.Hour))
	certPath, keyPath := writePair(t, certDir, c1, k1)

	pair := newReloadableKeyPair(certPath, keyPath, slog.Default())
	got, err := pair.get()
	if err != nil {
		t.Fatalf("首次加载失败: %v", err)
	}
	if cn := got.Leaf; cn != nil && cn.Subject.CommonName != "" {
		_ = cn
	}
	first, err := x509.ParseCertificate(got.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}

	// 换一份证书（模拟续期落盘）
	c2, k2 := selfSignedPair(t, "second", time.Now().Add(30*24*time.Hour))
	certPath, keyPath = writePair(t, certDir, c2, k2)
	// 某些文件系统 mtime 精度只到秒：显式推后一点，确保 stamp 变了。
	future := time.Now().Add(2 * time.Second)
	_ = os.Chtimes(certPath, future, future)
	_ = os.Chtimes(keyPath, future, future)

	got2, err := pair.get()
	if err != nil {
		t.Fatalf("重新加载失败: %v", err)
	}
	second, err := x509.ParseCertificate(got2.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if first.Subject.CommonName == second.Subject.CommonName {
		t.Fatalf("文件换了但仍在用旧证书（%s）", first.Subject.CommonName)
	}
	if second.Subject.CommonName != "second" {
		t.Errorf("期望加载到第二份证书，实际 %s", second.Subject.CommonName)
	}
}

// 证书还早的时候不许去打面板（6 小时一轮的巡检不该变成每轮一次请求）。
func TestMaybeRenewNodeCertSkipsWhenFresh(t *testing.T) {
	dir := t.TempDir()
	certPEM, keyPEM := selfSignedPair(t, "node-001", time.Now().Add(20*24*time.Hour))
	certPath, keyPath := writePair(t, filepath.Join(dir, "certs"), certPEM, keyPEM)
	_ = savePanelURL(filepath.Join(dir, "state"), "http://127.0.0.1:1")

	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	defer srv.Close()

	d := &Daemon{
		cfg: &config.DaemonConfig{TLS: true, CertFile: certPath, KeyFile: keyPath,
			PanelURL: srv.URL, StateDir: filepath.Join(dir, "state"), NodeID: "node-001"},
		log:      slog.Default(),
		shutdown: make(chan struct{}),
	}
	renewed, err := d.maybeRenewNodeCert()
	if err != nil {
		t.Fatalf("剩余 20 天不该报错: %v", err)
	}
	if renewed {
		t.Error("剩余 20 天不该续期")
	}
	if called {
		t.Error("剩余 20 天不该请求面板")
	}
}

// 到期前必须真的去续，并且面板地址可以来自「接入时记下的 state/panel_url」
// —— 老安装升级上来的节点配置文件里没有 daemon.panel_url。
func TestMaybeRenewNodeCertUsesRecordedPanelURL(t *testing.T) {
	dir := t.TempDir()
	stateDir := filepath.Join(dir, "state")
	_ = os.MkdirAll(stateDir, 0o700)
	_ = os.WriteFile(filepath.Join(stateDir, "node_uid"), []byte("uid-r\n"), 0o600)
	certPEM, keyPEM := selfSignedPair(t, "node-001", time.Now().Add(2*24*time.Hour)) // 只剩 2 天
	certPath, keyPath := writePair(t, filepath.Join(dir, "certs"), certPEM, keyPEM)

	newCert, _ := selfSignedPair(t, "node-001", time.Now().Add(30*24*time.Hour))
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		_, _ = w.Write([]byte(`{"cert":` + jsonString(string(newCert)) + `,"ca":"CAPEM2"}`))
	}))
	defer srv.Close()
	if err := savePanelURL(stateDir, srv.URL); err != nil {
		t.Fatal(err)
	}

	d := &Daemon{
		cfg: &config.DaemonConfig{TLS: true, CertFile: certPath, KeyFile: keyPath,
			StateDir: stateDir, NodeID: "node-001"}, // PanelURL 故意留空
		log:      slog.Default(),
		shutdown: make(chan struct{}),
	}
	renewed, err := d.maybeRenewNodeCert()
	if err != nil {
		t.Fatalf("续期失败: %v", err)
	}
	if !renewed || hits != 1 {
		t.Fatalf("期望续期一次，renewed=%v hits=%d", renewed, hits)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "certs", "ca.crt")); string(b) != "CAPEM2" {
		t.Errorf("ca.crt 没更新: %q", string(b))
	}
}

// 没有面板地址时必须明确说出来 —— 沉默等于「一个月后神秘掉线」。
func TestMaybeRenewNodeCertWithoutPanelURLExplains(t *testing.T) {
	dir := t.TempDir()
	certPEM, keyPEM := selfSignedPair(t, "node-001", time.Now().Add(24*time.Hour))
	certPath, keyPath := writePair(t, filepath.Join(dir, "certs"), certPEM, keyPEM)

	d := &Daemon{
		cfg: &config.DaemonConfig{TLS: true, CertFile: certPath, KeyFile: keyPath,
			StateDir: filepath.Join(dir, "nostate"), NodeID: "node-001"},
		log:      slog.Default(),
		shutdown: make(chan struct{}),
	}
	_, err := d.maybeRenewNodeCert()
	if err == nil || !strings.Contains(err.Error(), "panel_url") {
		t.Fatalf("应提示配置 daemon.panel_url，实际 %v", err)
	}
}
