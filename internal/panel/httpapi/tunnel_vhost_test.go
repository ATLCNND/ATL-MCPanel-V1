package httpapi

import (
	"encoding/json"
	"net/http"
	"testing"
)

// 第三方单隧道的 vhost 路由（底账 F16）：
// 协议与域名必须自洽，否则 frpc 会甩一句难懂的报错。这几条把边界钉死。
func TestCreateTunnelVhostValidation(t *testing.T) {
	srv, ts := newTestServer(t)
	tok := loginAs(t, srv, ts, "admin", "admin123")
	seedInstance(t, srv, "inst1", 1)
	frpsID := seedFrps(t, srv, "frp.example.com", 25565, 25600)

	base := map[string]interface{}{
		// ⚠️ 别用 8080 —— 那是面板自己的端口，portguard 会（正确地）拒绝，
		// 于是所有用例都因"本地端口被拦"而失败，看起来像校验逻辑坏了。
		"instance_id": "inst1", "frps_id": frpsID, "local_port": 8999,
	}
	mk := func(extra map[string]interface{}) map[string]interface{} {
		m := map[string]interface{}{}
		for k, v := range base {
			m[k] = v
		}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}

	cases := []struct {
		name string
		body map[string]interface{}
		code int
	}{
		{"http 缺域名", mk(map[string]interface{}{"protocol": "http"}), http.StatusBadRequest},
		{"http 两个都给", mk(map[string]interface{}{"protocol": "http", "subdomain": "mc", "custom_domain": "a.com"}), http.StatusBadRequest},
		{"tcp 给了域名", mk(map[string]interface{}{"protocol": "tcp", "subdomain": "mc", "remote_port": 25570}), http.StatusBadRequest},
		{"未知协议", mk(map[string]interface{}{"protocol": "quic"}), http.StatusBadRequest},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if code, raw := call(t, ts, "POST", "/api/tunnels", tok, c.body); code != c.code {
				t.Errorf("%s：期望 %d，实际 %d %s", c.name, c.code, code, raw)
			}
		})
	}

	// 正常路径：http + custom_domain 应当建成功，且域名落库
	code, raw := call(t, ts, "POST", "/api/tunnels", tok,
		mk(map[string]interface{}{"protocol": "http", "custom_domain": "panel.example.com"}))
	if code != http.StatusCreated {
		t.Fatalf("http + 域名 应当建成功，实际 %d %s", code, raw)
	}
	var got string
	if err := srv.db.QueryRow(`SELECT custom_domain FROM tunnels WHERE protocol = 'http'`).Scan(&got); err != nil {
		t.Fatalf("读库失败: %v", err)
	}
	if got != "panel.example.com" {
		t.Errorf("custom_domain 落库 = %q", got)
	}
}

// 直连线路的隧道：状态是 direct，且**不发公网端口**（远端=本地）。
func TestCreateTunnelOnDirectLine(t *testing.T) {
	srv, ts := newTestServer(t)
	tok := loginAs(t, srv, ts, "admin", "admin123")
	seedInstance(t, srv, "inst1", 1)

	code, raw := call(t, ts, "POST", "/api/frps", tok, map[string]interface{}{
		"name": "直连", "host": "203.0.113.10", "line_type": "direct",
	})
	if code != http.StatusCreated {
		t.Fatalf("建直连线路失败 %d %s", code, raw)
	}
	var line struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(raw, &line); err != nil {
		t.Fatalf("解析失败: %v", err)
	}

	code, raw = call(t, ts, "POST", "/api/tunnels", tok, map[string]interface{}{
		"instance_id": "inst1", "frps_id": line.ID, "protocol": "tcp", "local_port": 25565,
		// 故意给一个公网端口：直连线路下应当被忽略（远端就是本地端口）
		"remote_port": 30000,
	})
	if code != http.StatusCreated {
		t.Fatalf("建直连隧道失败 %d %s", code, raw)
	}
	var tun struct {
		RemotePort int32  `json:"remote_port"`
		Status     string `json:"status"`
	}
	if err := json.Unmarshal(raw, &tun); err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if tun.RemotePort != 25565 {
		t.Errorf("直连的远端端口应等于本地端口 25565，实际 %d", tun.RemotePort)
	}
	if tun.Status != "direct" {
		t.Errorf("直连隧道状态应为 direct，实际 %q", tun.Status)
	}
}
