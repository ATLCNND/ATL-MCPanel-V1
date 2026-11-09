package daemon

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestE2ERenewAgainstRealPanel 是给真机（开发 VM）用的一次性验收：拿真实面板
// 走一遍 /api/enroll/renew，确认面板真的会按 node_uid + 私钥指纹换发一张新证书。
//
// 只在显式设置 RENEW_E2E=1 时跑，且必须给出：
//
//	RENEW_PANEL  面板 HTTP 地址（启用安全入口时要带 /<入口>）
//	RENEW_STATE  从节点复制出来的 state 目录（里面有 node_uid）
//	RENEW_CERTS  从节点复制出来的 certs 目录（里面有 node.key / node.crt）
//
// 用复制出来的副本，不动线上正在用的证书。
func TestE2ERenewAgainstRealPanel(t *testing.T) {
	if os.Getenv("RENEW_E2E") != "1" {
		t.Skip("需要 RENEW_E2E=1 与真实面板地址，默认跳过")
	}
	panel := os.Getenv("RENEW_PANEL")
	stateDir := os.Getenv("RENEW_STATE")
	certDir := os.Getenv("RENEW_CERTS")
	if panel == "" || stateDir == "" || certDir == "" {
		t.Fatal("RENEW_PANEL / RENEW_STATE / RENEW_CERTS 三个都要给")
	}
	keyPath := filepath.Join(certDir, "node.key")
	before, err := readCertLeaf(filepath.Join(certDir, "node.crt"))
	if err != nil {
		t.Fatalf("读旧证书失败: %v", err)
	}
	keyBefore, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatalf("读旧私钥失败: %v", err)
	}

	certPath, caPath, expires, err := RenewNodeCert(panel, stateDir, certDir, keyPath, "node-001")
	if err != nil {
		t.Fatalf("真机续期失败: %v", err)
	}
	if expires.IsZero() {
		t.Error("没解析出到期时间")
	}
	if !expires.After(before.NotAfter) {
		t.Errorf("新证书到期 %s 不比旧的 %s 更晚", expires, before.NotAfter)
	}
	if b, err := os.ReadFile(certPath); err != nil || len(b) == 0 {
		t.Errorf("node.crt 没写出来: %v", err)
	}
	if b, err := os.ReadFile(caPath); err != nil || len(b) == 0 {
		t.Errorf("ca.crt 没写出来: %v", err)
	}
	keyAfter, err := os.ReadFile(keyPath)
	if err != nil || string(keyAfter) != string(keyBefore) {
		t.Error("续期把 node.key 改了（面板下一次会判「换了钥匙」）")
	}
	t.Logf("续期成功：%s -> %s（剩余 %s）", before.NotAfter.Format(time.RFC3339),
		expires.Format(time.RFC3339), time.Until(expires).Round(time.Hour))
}
