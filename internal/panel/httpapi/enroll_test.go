package httpapi

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"path/filepath"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/ATLCNND/ATL-MCPanel/internal/panel/auth"
	"github.com/ATLCNND/ATL-MCPanel/internal/panel/db"
	"github.com/ATLCNND/ATL-MCPanel/internal/panel/nodemgr"
	"github.com/ATLCNND/ATL-MCPanel/internal/pki"
)

// 造一份合法的 CSR（节点自己持有私钥），供 /api/enroll 用例使用。
func makeCSR(t *testing.T, cn string) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("生成密钥失败: %v", err)
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: cn},
	}, key)
	if err != nil {
		t.Fatalf("生成 CSR 失败: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}))
}

// issueJoinToken 走管理端接口签一个 token（顺带覆盖它自己的鉴权与落库）。
func issueJoinToken(t *testing.T, ts *httptest.Server, admin, mode, nodeUID string) string {
	t.Helper()
	code, body := doJSON(t, ts, "POST", "/api/enroll/token", admin, map[string]interface{}{
		"mode": mode, "node_uid": nodeUID, "node_name": "n1",
	})
	if code != http.StatusOK {
		t.Fatalf("签发 token 失败: %d %v", code, body)
	}
	tok, _ := body["token"].(string)
	if tok == "" {
		t.Fatal("token 为空")
	}
	return tok
}

// enrollOnce 调一次 /api/enroll，返回状态码与响应体。
func enrollOnce(t *testing.T, ts *httptest.Server, token, csr, uid string) (int, map[string]interface{}) {
	t.Helper()
	return doJSON(t, ts, "POST", "/api/enroll", "", map[string]interface{}{
		"token": token, "csr": csr, "node_uid": uid,
		"node_name": "n1", "hostname": "host1",
	})
}

// 一次成功的凭证交换：拿到证书，且证书的 CN 与 node_uid（URI SAN）都对。
func TestEnrollIssuesCertWithNodeUID(t *testing.T) {
	srv, ts := newTestServerWithCA(t)
	admin := loginAs(t, srv, ts, "adm-e1", "adm-e1-pass-1234")
	uid := "11111111-2222-4333-8444-555555555555"
	token := issueJoinToken(t, ts, admin, "panel", uid)
	code, out := enrollOnce(t, ts, token, makeCSR(t, "n1"), uid)
	if code != http.StatusOK {
		t.Fatalf("enroll 失败: %d %v", code, out)
	}
	certPEM, _ := out["cert"].(string)
	if certPEM == "" {
		t.Fatal("响应里没有证书")
	}
	blk, _ := pem.Decode([]byte(certPEM))
	if blk == nil {
		t.Fatal("证书不是合法 PEM")
	}
	cert, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		t.Fatalf("解析证书失败: %v", err)
	}
	if cert.Subject.CommonName == "" {
		t.Error("证书缺少 CN")
	}
	if got := nodeUIDFromCert(cert); got != uid {
		t.Errorf("证书里的 node_uid = %q，期望 %q", got, uid)
	}
	// 节点行应当被建出来，且没有 SSH 凭据
	var mode, auth string
	_ = srv.db.QueryRow(`SELECT COALESCE(enroll_mode,''), COALESCE(ssh_auth,'') FROM nodes WHERE node_uid = ?`, uid).
		Scan(&mode, &auth)
	if mode == "" {
		t.Error("节点行的 enroll_mode 未写入")
	}
	if auth != "" {
		t.Error("凭证交换接入的节点不该有 SSH 凭据")
	}
}

// token 只能用一次 —— 这是防重放的核心，必须有测试。
func TestEnrollTokenIsSingleUse(t *testing.T) {
	srv, ts := newTestServerWithCA(t)
	admin := loginAs(t, srv, ts, "adm-e1", "adm-e1-pass-1234")
	uid := "aaaaaaaa-2222-4333-8444-555555555555"
	token := issueJoinToken(t, ts, admin, "panel", uid)
	if code, out := enrollOnce(t, ts, token, makeCSR(t, "n1"), uid); code != http.StatusOK {
		t.Fatalf("第一次应当成功: %d %v", code, out)
	}
	if code, _ := enrollOnce(t, ts, token, makeCSR(t, "n1"), uid); code != http.StatusForbidden {
		t.Errorf("第二次应当被拒（Forbidden），实际 %d", code)
	}
}

// 过期的 token 必须被拒。
func TestEnrollRejectsExpiredToken(t *testing.T) {
	srv, ts := newTestServerWithCA(t)
	admin := loginAs(t, srv, ts, "adm-e1", "adm-e1-pass-1234")
	uid := "bbbbbbbb-2222-4333-8444-555555555555"
	token := issueJoinToken(t, ts, admin, "panel", uid)
	// 直接把过期时间推到过去
	if _, err := srv.db.Exec(`UPDATE join_tokens SET expires_at = ? WHERE jti = ?`,
		time.Now().Add(-time.Minute).Unix(), strings.TrimPrefix(token, "atl-join-v1.")); err != nil {
		t.Fatalf("改过期时间失败: %v", err)
	}
	if code, _ := enrollOnce(t, ts, token, makeCSR(t, "n1"), uid); code != http.StatusForbidden {
		t.Errorf("过期 token 应被拒，实际 %d", code)
	}
}

// 撤销后的 token 必须被拒。
func TestEnrollRejectsRevokedToken(t *testing.T) {
	srv, ts := newTestServerWithCA(t)
	admin := loginAs(t, srv, ts, "adm-e1", "adm-e1-pass-1234")
	uid := "cccccccc-2222-4333-8444-555555555555"
	token := issueJoinToken(t, ts, admin, "panel", uid)
	if _, err := srv.db.Exec(`UPDATE join_tokens SET revoked_at = CURRENT_TIMESTAMP WHERE jti = ?`,
		strings.TrimPrefix(token, "atl-join-v1.")); err != nil {
		t.Fatalf("撤销失败: %v", err)
	}
	if code, _ := enrollOnce(t, ts, token, makeCSR(t, "n1"), uid); code != http.StatusForbidden {
		t.Errorf("撤销后应被拒，实际 %d", code)
	}
}

// token 与 node_uid 不匹配时必须拒绝（防拿 A 的 token 给 B 换证书）。
func TestEnrollRejectsUIDMismatch(t *testing.T) {
	srv, ts := newTestServerWithCA(t)
	admin := loginAs(t, srv, ts, "adm-e1", "adm-e1-pass-1234")
	uid := "dddddddd-2222-4333-8444-555555555555"
	token := issueJoinToken(t, ts, admin, "panel", uid)
	if code, _ := enrollOnce(t, ts, token, makeCSR(t, "other"), "eeeeeeee-2222-4333-8444-555555555555"); code != http.StatusForbidden {
		t.Errorf("node_uid 不匹配应被拒，实际 %d", code)
	}
}

// CSR 自签名无效（伪造/被篡改）必须拒绝。
func TestEnrollRejectsBadCSR(t *testing.T) {
	srv, ts := newTestServerWithCA(t)
	admin := loginAs(t, srv, ts, "adm-e1", "adm-e1-pass-1234")
	uid := "ffffffff-2222-4333-8444-555555555555"
	token := issueJoinToken(t, ts, admin, "panel", uid)
	bad := base64.StdEncoding.EncodeToString([]byte("not a csr"))
	if code, _ := enrollOnce(t, ts, token, bad, uid); code != http.StatusBadRequest {
		t.Errorf("非法 CSR 应 400，实际 %d", code)
	}
}

// newTestServerWithCA 起一个**装配了 PKI** 的测试服务：凭证交换要签证书，没有 CA 跑不了。
func newTestServerWithCA(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	d, err := db.Open("sqlite3", filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("初始化数据库失败: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	ca, _, err := pki.LoadOrCreateCA(t.TempDir())
	if err != nil {
		t.Fatalf("建 CA 失败: %v", err)
	}
	srv := NewServer(d, auth.NewService("test-secret"), nodemgr.NewManager(d), Options{
		ListenAddr:  ":8080",
		PanelFrpDir: filepath.Join(t.TempDir(), "panel-frp"),
		DataDir:     t.TempDir(),
		CA:          ca,
		GRPCMTLS:    false,
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return srv, ts
}

// nodeUIDFromCert 取证书 URI SAN 里的 node_uid（与 pki.CertNodeUID 同一约定）。
func nodeUIDFromCert(c *x509.Certificate) string {
	for _, u := range c.URIs {
		if u != nil && u.Scheme == "atlmcpanel" && u.Host == "node" {
			return strings.TrimPrefix(u.Path, "/")
		}
	}
	return ""
}

// makeCSRWithKey 用**指定密钥**造 CSR —— 续期用例需要「同一把钥匙再签一次」。
func makeCSRWithKey(t *testing.T, cn string, key *ecdsa.PrivateKey) string {
	t.Helper()
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: cn},
	}, key)
	if err != nil {
		t.Fatalf("生成 CSR 失败: %v", err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}))
}

// 续期必须与接入使用**同一个公钥指纹**：同一把钥匙 → 200；换一把钥匙 → 403。
//
// 这条用例防的是「两处各算一个指纹」——真机验证抓到过一次：接入存 csr 文本的哈希、
// 续期比公钥 DER 的哈希，二者永远不等，于是同一把钥匙续期也被拒。单测若只走一条路径
// 是发现不了的，所以这里刻意让接入与续期各走一次真实 HTTP。
func TestRenewAcceptsSameKeyRejectsOtherKey(t *testing.T) {
	srv, ts := newTestServerWithCA(t)
	admin := loginAs(t, srv, ts, "adm-r1", "adm-r1-pass-1234")
	uid := "abcdabcd-1111-4222-8333-444455556666"
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("生成密钥失败: %v", err)
	}

	token := issueJoinToken(t, ts, admin, "panel", uid)
	code, body := doJSON(t, ts, "POST", "/api/enroll", "", map[string]interface{}{
		"token": token, "csr": makeCSRWithKey(t, "n1", key), "node_uid": uid, "node_name": "n1",
	})
	if code != http.StatusOK {
		t.Fatalf("接入应成功: %d %v", code, body)
	}

	// 同一把钥匙续期：必须成功
	code, body = doJSON(t, ts, "POST", "/api/enroll/renew", "", map[string]interface{}{
		"node_uid": uid, "csr": makeCSRWithKey(t, "n1", key),
	})
	if code != http.StatusOK {
		t.Fatalf("同一把钥匙续期应成功，实际 %d %v", code, body)
	}
	if _, ok := body["cert"]; !ok {
		t.Error("续期响应里没有新证书")
	}

	// 换一把钥匙续期：必须拒绝（否则等于把身份让给了别人）
	other, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	code, _ = doJSON(t, ts, "POST", "/api/enroll/renew", "", map[string]interface{}{
		"node_uid": uid, "csr": makeCSRWithKey(t, "n1", other),
	})
	if code != http.StatusForbidden {
		t.Errorf("换钥匙续期应 403，实际 %d", code)
	}
}
