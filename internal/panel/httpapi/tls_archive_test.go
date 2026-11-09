package httpapi

import (
	"archive/zip"
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// 证书压缩包上传（tls.go 的 extractCertKey / handleUploadCertArchive）。
//
// 要防的三件事：
//   1. 从 IDC 后台下载的证书包文件名五花八门，按后缀猜一定会猜错
//      → 必须按内容（PEM 块类型）识别；
//   2. 包里常同时有旧证书与新证书，盲取第一个会配不上私钥
//      → 必须挨个组合试配对，而不是报"证书与私钥不匹配"；
//   3. 校验口径必须与"粘贴 PEM"那条路完全一致（配对、未过期、含 SAN），
//      否则压缩包成了绕过 SAN 检查的后门。

// testCertPEM 造一张自签证书（含 SAN），返回 certificate PEM 与私钥 PEM。
func testCertPEM(t *testing.T, cn string, notAfter time.Time) (string, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{cn},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return string(certPEM), string(keyPEM)
}

// zipWith 把若干 (文件名, 内容) 打成一个 zip。
func zipWith(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func TestExtractCertKeyIgnoresFilenames(t *testing.T) {
	cert, key := testCertPEM(t, "panel.example.com", time.Now().Add(90*24*time.Hour))
	// 故意用"看起来不像证书"的文件名：识别必须靠内容
	data := zipWith(t, map[string]string{
		"a1b2c3.txt":  cert,
		"readme.txt":  "这是服务商给的说明文件，不是证书",
		"k9f8e7.data": key,
		"中间证书.pem":    "-----BEGIN CERTIFICATE-----\nQUJD\n-----END CERTIFICATE-----\n",
	})
	gotCert, gotKey, source, err := extractCertKey(data)
	if err != nil {
		t.Fatalf("应能从 zip 里认出证书与私钥，却报错：%v", err)
	}
	if _, err := checkCertPair(gotCert, gotKey); err != nil {
		t.Fatalf("取出的证书与私钥必须能配对：%v", err)
	}
	if !strings.Contains(source, "a1b2c3.txt") || !strings.Contains(source, "k9f8e7.data") {
		t.Fatalf("来源说明应指出用了哪两个文件，实际 %q", source)
	}
}

func TestExtractCertKeyPicksTheMatchingPair(t *testing.T) {
	oldCert, _ := testCertPEM(t, "old.example.com", time.Now().Add(10*24*time.Hour))
	newCert, newKey := testCertPEM(t, "new.example.com", time.Now().Add(200*24*time.Hour))
	// 包里同时有旧证书（配不上新私钥）与新证书
	data := zipWith(t, map[string]string{
		"cert-old.pem": oldCert,
		"cert.pem":     newCert,
		"privkey.pem":  newKey,
	})
	gotCert, gotKey, source, err := extractCertKey(data)
	if err != nil {
		t.Fatalf("应能挑出能配对的那一组：%v", err)
	}
	leaf, err := checkCertPair(gotCert, gotKey)
	if err != nil {
		t.Fatalf("挑出来的必须能通过校验：%v", err)
	}
	if leaf.Subject.CommonName != "new.example.com" {
		t.Fatalf("应当选中新证书，实际选中 %s（来源 %s）", leaf.Subject.CommonName, source)
	}
}

func TestExtractCertKeyAcceptsCombinedPEM(t *testing.T) {
	cert, key := testCertPEM(t, "combined.example.com", time.Now().Add(30*24*time.Hour))
	combined := cert + key
	gotCert, gotKey, _, err := extractCertKey([]byte(combined))
	if err != nil {
		t.Fatalf("合并 PEM 应被接受：%v", err)
	}
	if _, err := checkCertPair(gotCert, gotKey); err != nil {
		t.Fatalf("合并 PEM 取出的应能配对：%v", err)
	}
}

func TestExtractCertKeyRejectsJunk(t *testing.T) {
	if _, _, _, err := extractCertKey([]byte("hello world")); err == nil {
		t.Fatal("既没有证书也没有私钥的内容必须报错")
	}
	// zip 里只有说明文件
	data := zipWith(t, map[string]string{"readme.txt": "nothing here"})
	if _, _, _, err := extractCertKey(data); err == nil {
		t.Fatal("zip 里没有证书时必须报错（而不是静默成功）")
	}
	// 只有证书没有私钥
	cert, _ := testCertPEM(t, "only.example.com", time.Now().Add(time.Hour))
	if _, _, _, err := extractCertKey(zipWith(t, map[string]string{"cert.pem": cert})); err == nil {
		t.Fatal("缺私钥必须报错")
	}
}

// TestUploadCertArchiveEndpoint 走完整的 HTTP 路径（含落盘与响应）。
func TestUploadCertArchiveEndpoint(t *testing.T) {
	srv, ts := newTestServer(t)
	token := loginAs(t, srv, ts, "tlsadmin", "pw")

	cert, key := testCertPEM(t, "panel.example.com", time.Now().Add(120*24*time.Hour))
	blob := zipWith(t, map[string]string{"fullchain.pem": cert, "privkey.key": key})

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, err := mw.CreateFormFile("file", "panel-cert.zip")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write(blob); err != nil {
		t.Fatal(err)
	}
	mw.Close()

	req := httptest.NewRequest("POST", "/api/tls/upload-archive", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+token)
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("上传压缩包应 200，实际 %d：%s", w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp["subject"] != "panel.example.com" {
		t.Fatalf("响应里的证书主体不对：%v", resp)
	}
	// 证书必须真的落盘，且私钥权限是 0600
	certPath, _ := resp["cert_path"].(string)
	keyPath, _ := resp["key_path"].(string)
	if certPath == "" || keyPath == "" {
		t.Fatalf("没有返回落盘路径：%v", resp)
	}
	if _, err := os.Stat(certPath); err != nil {
		t.Fatalf("证书文件没落盘：%v", err)
	}
	st, err := os.Stat(keyPath)
	if err != nil {
		t.Fatalf("私钥文件没落盘：%v", err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("私钥权限应为 0600，实际 %o", st.Mode().Perm())
	}
}

// TestUploadCertArchiveRejectsExpired 压缩包路径同样要拦过期证书（不能比粘贴那条路松）。
func TestUploadCertArchiveRejectsExpired(t *testing.T) {
	srv, _ := newTestServer(t)
	cert, key := testCertPEM(t, "expired.example.com", time.Now().Add(-24*time.Hour))
	if _, err := checkCertPair(cert, key); err == nil || !strings.Contains(err.Error(), "过期") {
		t.Fatalf("过期证书必须被拦下，实际 err=%v", err)
	}
	_ = srv
}
