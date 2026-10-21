package httpapi

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// 导入落库（底账 F15 的最后一块）+ 线路类型（F16）。
//
// 这组用例盯的都是**写操作**，所以除了正常路径，重点在"不该发生的事"：
// 重名线路不许悄悄叠加、失败的导入不许留下半截线路、直连线路不许分配端口。

const importSample = `serverAddr = "frp.example.com"` + "\n" +
	`serverPort = 7000` + "\n" +
	`auth.token = "EXAMPLE-TOKEN-ABCD1234"` + "\n\n" +
	"[[proxies]]\n" +
	`name = "mc"` + "\n" +
	`type = "tcp"` + "\n" +
	`localPort = 25565` + "\n" +
	`remotePort = 25570` + "\n" +
	"\n[[proxies]]\n" +
	`name = "web"` + "\n" +
	`type = "http"` + "\n" +
	`localPort = 8080` + "\n" +
	`customDomains = ["panel.example.com"]` + "\n"

func TestApplyImportCreatesLineAndTunnels(t *testing.T) {
	srv, ts := newTestServer(t)
	tok := loginAs(t, srv, ts, "admin", "admin123")

	code, raw := call(t, ts, "POST", "/api/frp/import/apply", tok,
		map[string]string{"config": importSample, "name": "导入线路"})
	if code != http.StatusCreated {
		t.Fatalf("导入应成功，实际 %d %s", code, raw)
	}

	// 线路按第三方类型落库，且**没有端口段**（第三方不分配端口）
	var lineType string
	var portStart, portEnd int32
	if err := srv.db.QueryRow(`SELECT line_type, port_start, port_end FROM frps_servers WHERE name = '导入线路'`).
		Scan(&lineType, &portStart, &portEnd); err != nil {
		t.Fatalf("线路没建出来: %v", err)
	}
	if lineType != "third_party" {
		t.Errorf("line_type = %q，期望 third_party", lineType)
	}
	if portStart != 0 || portEnd != 0 {
		t.Errorf("第三方线路不该有端口段，实际 %d-%d", portStart, portEnd)
	}

	// 两条隧道都进去了，且域名方式保留下来了
	var n int
	if err := srv.db.QueryRow(`SELECT COUNT(*) FROM tunnels WHERE protocol IN ('tcp','http') AND status = 'imported'`).Scan(&n); err != nil {
		t.Fatalf("统计隧道失败: %v", err)
	}
	if n != 2 {
		t.Errorf("隧道数 = %d，期望 2", n)
	}
	var domain string
	if err := srv.db.QueryRow(`SELECT custom_domain FROM tunnels WHERE name = 'web'`).Scan(&domain); err != nil {
		t.Fatalf("取域名失败: %v", err)
	}
	if !strings.Contains(domain, "panel.example.com") {
		t.Errorf("custom_domain = %q", domain)
	}
}

// 重名必须拒绝：悄悄再建一条同名线路，界面上分不清、删的时候更分不清。
func TestApplyImportRejectsDuplicateName(t *testing.T) {
	srv, ts := newTestServer(t)
	tok := loginAs(t, srv, ts, "admin", "admin123")

	if code, _ := call(t, ts, "POST", "/api/frp/import/apply", tok,
		map[string]string{"config": importSample, "name": "线路A"}); code != http.StatusCreated {
		t.Fatalf("第一次导入应成功")
	}
	code, raw := call(t, ts, "POST", "/api/frp/import/apply", tok,
		map[string]string{"config": importSample, "name": "线路A"})
	if code != http.StatusConflict {
		t.Errorf("重名应 409，实际 %d %s", code, raw)
	}
	var n int
	_ = srv.db.QueryRow(`SELECT COUNT(*) FROM frps_servers`).Scan(&n)
	if n != 1 {
		t.Errorf("重名被拒后线路数应仍为 1，实际 %d（说明写进了半截数据）", n)
	}
}

// 解析失败时**一条数据都不许写**（事务回滚的实证）。
func TestApplyImportBadConfigLeavesNothing(t *testing.T) {
	srv, ts := newTestServer(t)
	tok := loginAs(t, srv, ts, "admin", "admin123")

	code, _ := call(t, ts, "POST", "/api/frp/import/apply", tok,
		map[string]string{"config": "serverPort = 7000\n", "name": "坏的"})
	if code != http.StatusBadRequest {
		t.Errorf("坏配置应 400，实际 %d", code)
	}
	var lines, tuns int
	_ = srv.db.QueryRow(`SELECT COUNT(*) FROM frps_servers`).Scan(&lines)
	_ = srv.db.QueryRow(`SELECT COUNT(*) FROM tunnels`).Scan(&tuns)
	if lines != 0 || tuns != 0 {
		t.Errorf("失败后不该留下任何数据：线路 %d 条、隧道 %d 条", lines, tuns)
	}
}

// 直连线路：建隧道时**不分配端口**，远端端口就是本地端口，且线路类型要能读回来。
func TestDirectLineTunnelDoesNotAllocate(t *testing.T) {
	srv, ts := newTestServer(t)
	tok := loginAs(t, srv, ts, "admin", "admin123")
	seedInstance(t, srv, "inst1", 1)

	code, raw := call(t, ts, "POST", "/api/frps", tok, map[string]interface{}{
		"name": "直连线路", "host": "203.0.113.10", "line_type": "direct",
	})
	if code != http.StatusCreated {
		t.Fatalf("建直连线路失败 %d %s", code, raw)
	}
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	lineID := int64(body["id"].(float64))

	// 端口段应为 0（直连没有这个概念）
	var portStart, portEnd int32
	if err := srv.db.QueryRow(`SELECT port_start, port_end FROM frps_servers WHERE id = ?`, lineID).
		Scan(&portStart, &portEnd); err != nil {
		t.Fatalf("读线路失败: %v", err)
	}
	if portStart != 0 || portEnd != 0 {
		t.Errorf("直连线路不该有端口段，实际 %d-%d", portStart, portEnd)
	}

	// 建隧道：远端口 = 本地端口，且状态是 direct（没有下发 frpc）
	code, raw = call(t, ts, "POST", "/api/tunnels", tok, map[string]interface{}{
		"instance_id": "inst1", "frps_id": lineID, "protocol": "tcp", "local_port": 25565,
	})
	if code != http.StatusCreated {
		t.Fatalf("建直连隧道失败 %d %s", code, raw)
	}
	var tres map[string]any
	_ = json.Unmarshal(raw, &tres)
	if got := tres["remote_port"].(float64); int(got) != 25565 {
		t.Errorf("直连的远端端口应等于本地端口 25565，实际 %v", got)
	}
	if got, _ := tres["status"].(string); got != "direct" {
		t.Errorf("直连隧道状态应为 direct，实际 %q", got)
	}
}

// 未知线路类型必须在建线路时就拒绝（而不是先建一条再去猜怎么用它）。
func TestCreateLineRejectsUnknownType(t *testing.T) {
	srv, ts := newTestServer(t)
	tok := loginAs(t, srv, ts, "admin", "admin123")
	code, raw := call(t, ts, "POST", "/api/frps", tok, map[string]interface{}{
		"name": "x", "host": "h", "line_type": "nonsense",
	})
	if code != http.StatusBadRequest {
		t.Errorf("未知类型应 400，实际 %d %s", code, raw)
	}
}
