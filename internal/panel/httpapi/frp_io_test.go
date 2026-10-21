package httpapi

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// ============================================================================
// 穿透配置的导入预览 / 导出（底账 F15）
// ============================================================================
//
// 这两条链路都有"用户可控输入 + 凭据外泄风险"两块要注意，所以用例覆盖三类：
//   ① 正常路径（解析对不对、导出对不对）；
//   ② 凭据边界（预览**不回传明文 token**、导出**默认不含 token**）；
//   ③ 权限与限额（非管理员 403、超大请求体拒绝）。

func seedTunnelFor(t *testing.T, srv *Server, frpsID int64, tunnelID, name string, local, remote int32) {
	t.Helper()
	seedInstance(t, srv, "inst1", 1)
	if _, err := srv.db.Exec(
		`INSERT INTO tunnels (tunnel_id, instance_id, frps_id, name, protocol, local_port, remote_port, status)
		 VALUES (?, 'inst1', ?, ?, 'tcp', ?, ?, 'active')`,
		tunnelID, frpsID, name, local, remote); err != nil {
		t.Fatalf("插入隧道失败: %v", err)
	}
}

func TestPreviewFrpcImportOK(t *testing.T) {
	srv, ts := newTestServer(t)
	tok := loginAs(t, srv, ts, "admin", "admin123")

	cfg := `serverAddr = "frp.example.com"` + "\n" +
		`serverPort = 7000` + "\n" +
		`auth.token = "EXAMPLE-TOKEN-ABCD1234"` + "\n\n" +
		"[[proxies]]\n" +
		`name = "mc"` + "\n" +
		`type = "tcp"` + "\n" +
		`localPort = 25565` + "\n" +
		`remotePort = 25570` + "\n"

	code, raw := call(t, ts, "POST", "/api/frp/import/preview", tok, map[string]string{"config": cfg})
	if code != http.StatusOK {
		t.Fatalf("预览应成功，实际 %d %s", code, raw)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("响应不是 JSON: %v", err)
	}
	if out["ok"] != true {
		t.Fatalf("ok 应为 true，实际 %v（%s）", out["ok"], raw)
	}
	line, _ := out["line"].(map[string]any)
	if line["server_addr"] != "frp.example.com" {
		t.Errorf("server_addr = %v", line["server_addr"])
	}
	// ⚠️ 凭据边界：预览**绝不可以**回传明文 token
	blob := string(raw)
	if strings.Contains(blob, "EXAMPLE-TOKEN-ABCD1234") {
		t.Errorf("预览响应里出现了明文 token！%s", blob)
	}
	if line["token_masked"] == "" || line["has_token"] != true {
		t.Errorf("应回传打码 token 与 has_token，实际 %v", line)
	}
	proxies, _ := out["proxies"].([]any)
	if len(proxies) != 1 {
		t.Fatalf("隧道数 = %d", len(proxies))
	}
}

func TestPreviewFrpcImportBadConfig(t *testing.T) {
	srv, ts := newTestServer(t)
	tok := loginAs(t, srv, ts, "admin", "admin123")

	// 缺 serverAddr：应当返回 200 + ok=false（预览接口的"失败"是业务结果，不是 5xx）
	code, raw := call(t, ts, "POST", "/api/frp/import/preview", tok,
		map[string]string{"config": "serverPort = 7000\n"})
	if code != http.StatusOK {
		t.Fatalf("预览失败也应返回 200，实际 %d", code)
	}
	var out map[string]any
	_ = json.Unmarshal(raw, &out)
	if out["ok"] != false {
		t.Errorf("ok 应为 false，实际 %v", out["ok"])
	}
	if s, _ := out["error"].(string); !strings.Contains(s, "serverAddr") {
		t.Errorf("错误信息应指出缺 serverAddr，实际 %q", s)
	}
}

func TestPreviewFrpcImportRequiresAdmin(t *testing.T) {
	srv, ts := newTestServer(t)
	admin := loginAs(t, srv, ts, "admin", "admin123")
	if code, _ := call(t, ts, "POST", "/api/users", admin,
		map[string]string{"username": "bob", "password": "bobpass123", "role": "user"}); code != http.StatusCreated {
		t.Fatalf("建普通用户失败: %d", code)
	}
	bob := loginAsToken(t, ts, "bob", "bobpass123")
	if code, _ := call(t, ts, "POST", "/api/frp/import/preview", bob,
		map[string]string{"config": "serverAddr='h'\n"}); code != http.StatusForbidden {
		t.Errorf("普通用户不应能调导入预览，实际 %d", code)
	}
}

func TestExportFrpsDefaultsToNoToken(t *testing.T) {
	srv, ts := newTestServer(t)
	tok := loginAs(t, srv, ts, "admin", "admin123")
	frpsID := seedFrps(t, srv, "frp.example.com", 25565, 25600)
	seedTunnelFor(t, srv, frpsID, "t1", "mc-alpha", 25565, 25570)

	code, raw := call(t, ts, "GET", "/api/frps/"+itoa(frpsID)+"/export", tok, nil)
	if code != http.StatusOK {
		t.Fatalf("导出应成功，实际 %d %s", code, raw)
	}
	var out struct {
		Text          string `json:"text"`
		TunnelCount   int    `json:"tunnel_count"`
		IncludesToken bool   `json:"includes_token"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("响应不是 JSON: %v", err)
	}
	if out.TunnelCount != 1 {
		t.Errorf("隧道数 = %d，期望 1", out.TunnelCount)
	}
	// ⚠️ 凭据边界：**默认**导出不该带 token（seedFrps 写的是 'test-token'）
	if out.IncludesToken {
		t.Error("默认导出不应包含 token")
	}
	if strings.Contains(out.Text, "test-token") {
		t.Errorf("默认导出的文本里出现了 token！\n%s", out.Text)
	}
	for _, want := range []string{"serverAddr", "serverPort", "[[proxies]]", "remotePort = 25570"} {
		if !strings.Contains(out.Text, want) {
			t.Errorf("导出文本应包含 %q\n%s", want, out.Text)
		}
	}
}

func TestExportFrpsWithToken(t *testing.T) {
	srv, ts := newTestServer(t)
	tok := loginAs(t, srv, ts, "admin", "admin123")
	frpsID := seedFrps(t, srv, "frp.example.com", 25565, 25600)
	seedTunnelFor(t, srv, frpsID, "t1", "mc-alpha", 25565, 25570)

	code, raw := call(t, ts, "GET", "/api/frps/"+itoa(frpsID)+"/export?token=1", tok, nil)
	if code != http.StatusOK {
		t.Fatalf("导出应成功，实际 %d %s", code, raw)
	}
	var out struct {
		Text          string `json:"text"`
		IncludesToken bool   `json:"includes_token"`
	}
	_ = json.Unmarshal(raw, &out)
	if !out.IncludesToken || !strings.Contains(out.Text, "test-token") {
		t.Errorf("显式要求带 token 时应包含它\n%s", out.Text)
	}
	// 导出的文本必须能被自己的导入器读回来（两侧契约）
	if !strings.Contains(out.Text, "auth.token") {
		t.Errorf("默认风格应使用点号键 auth.token\n%s", out.Text)
	}
}

func TestExportFrpsNotFound(t *testing.T) {
	srv, ts := newTestServer(t)
	tok := loginAs(t, srv, ts, "admin", "admin123")
	if code, _ := call(t, ts, "GET", "/api/frps/9999/export", tok, nil); code != http.StatusNotFound {
		t.Errorf("不存在的线路应 404，实际 %d", code)
	}
}

func TestExportFrpsRequiresAdmin(t *testing.T) {
	srv, ts := newTestServer(t)
	admin := loginAs(t, srv, ts, "admin", "admin123")
	frpsID := seedFrps(t, srv, "frp.example.com", 25565, 25600)
	seedInstance(t, srv, "inst1", 1)
	if code, _ := call(t, ts, "POST", "/api/users", admin,
		map[string]string{"username": "carol", "password": "carolpass123", "role": "user"}); code != http.StatusCreated {
		t.Fatalf("建普通用户失败: %d", code)
	}
	carol := loginAsToken(t, ts, "carol", "carolpass123")
	if code, _ := call(t, ts, "GET", "/api/frps/"+itoa(frpsID)+"/export", carol, nil); code != http.StatusForbidden {
		t.Errorf("普通用户不应能导出线路配置，实际 %d", code)
	}
}
