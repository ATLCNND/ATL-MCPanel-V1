package scheduler

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"log/slog"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"
)
// 证书到期检查：三档阈值 + 文件异常。
//
// 为什么要测：这条是「面板某天突然打不开」的唯一早期信号，写错了不会有人立刻发现，
// 等到发现时已经过期了。
func TestCheckTLSCertAlerts(t *testing.T) {
	day := 24 * time.Hour
	scenario := func(name string, notAfter time.Time, wantKind string) {
		t.Run(name, func(t *testing.T) {
			d := newTestDB(t)
			dir := t.TempDir()
			certPath := filepath.Join(dir, "panel.crt")
			writeCert(t, certPath, notAfter)
			s := New(Options{DB: d, Logger: slog.Default(), TLSCertPath: certPath})
			s.checkTLSCert(context.Background())
			var kind string
			err := d.QueryRow(`SELECT kind FROM alerts WHERE active = 1`).Scan(&kind)
			if wantKind == "" {
				if err == nil {
					t.Errorf("不该有告警，却有 %s", kind)
				}
				return
			}
			if err != nil || kind != wantKind {
				t.Errorf("期望告警 %s，实际 kind=%s err=%v", wantKind, kind, err)
			}
		})
	}
	scenario("剩余很久不告警", time.Now().Add(200*day), "")
	scenario("剩余 20 天提醒", time.Now().Add(20*day), "tls_cert")
	scenario("剩余 3 天严重", time.Now().Add(3*day), "tls_cert")
	scenario("已过期", time.Now().Add(-2*day), "tls_cert")
}

// writeCert 造一张指定到期时间的自签证书。
func writeCert(t *testing.T, path string, notAfter time.Time) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "panel.local"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     notAfter,
		DNSNames:     []string{"panel.local"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		t.Fatal(err)
	}
}
