package httpapi

import (
	"net/http"
	"testing"
)

// ============================================================================
// 线路对外域名 与 公网地址组装
// ============================================================================

// TestNormalizeDisplayHost 管理员填的东西千奇百怪，写入前必须归一化。
//
// 最要紧的一条是**端口必须剥掉**：线路级域名要给该线路上所有端口复用，
// 留着端口会拼出 `mc.example.com:25570:25571` 这种地址。
func TestNormalizeDisplayHost(t *testing.T) {
	cases := []struct{ in, want string }{
		{"mc.example.com", "mc.example.com"},
		{"  mc.example.com  ", "mc.example.com"},
		{"https://mc.example.com", "mc.example.com"},
		{"http://mc.example.com/", "mc.example.com"},
		{"mc.example.com/", "mc.example.com"},
		{"mc.example.com:25570", "mc.example.com"},   // 端口要剥掉
		{"https://mc.example.com:8443/x?y=1", "mc.example.com"},
		{"", ""},
		{"   ", ""},
		// IPv6 字面量里本就有多个冒号，不能当端口削掉
		{"fe80::1", "fe80::1"},
		{"[::1]", "[::1]"},
		// 冒号后不是纯数字 → 不是端口，保留
		{"host:abc", "host:abc"},
	}
	for _, c := range cases {
		if got := normalizeDisplayHost(c.in); got != c.want {
			t.Errorf("normalizeDisplayHost(%q) = %q，期望 %q", c.in, got, c.want)
		}
	}
}

// TestPublicAddress 组装优先级：隧道级 > 线路级 > IP:端口。
func TestPublicAddress(t *testing.T) {
	cases := []struct {
		name         string
		tunnelDomain string
		lineDomain   string
		lineHost     string
		port         int32
		want         string
	}{
		{"隧道级优先", "a.example.com:1234", "b.example.com", "10.0.0.1", 25570, "a.example.com:1234"},
		{"线路级加端口", "", "b.example.com", "10.0.0.1", 25570, "b.example.com:25570"},
		{"没有域名时兜底到 IP", "", "", "10.0.0.1", 25570, "10.0.0.1:25570"},
		{"线路域名也归一化", "", "https://b.example.com/", "10.0.0.1", 25570, "b.example.com:25570"},
		{"空白域名视为未配置", "", "   ", "10.0.0.1", 25570, "10.0.0.1:25570"},
	}
	for _, c := range cases {
		got := publicAddress(c.tunnelDomain, c.lineDomain, c.lineHost, c.port)
		if got != c.want {
			t.Errorf("%s: publicAddress = %q，期望 %q", c.name, got, c.want)
		}
	}
}

// TestInstancePortsUseLineDomain 是这一项改动的**核心断言**：
// 给线路配上对外域名之后，实例页展示给用户的公网地址必须是域名，
// 而不是 `节点IP:端口` —— 后者会让用户一转发就把节点真实入口公布出去。
func TestInstancePortsUseLineDomain(t *testing.T) {
	srv, ts := newTestServer(t)
	adminTok := loginAs(t, ts, "admin", "admin123")

	frpsID := seedFrps(t, srv, "10.0.0.1", 25565, 25600)
	seedInstance(t, srv, "inst1", 1)
	seedTunnel(t, srv, "inst1-tcp-25570", "inst1", frpsID, 25570)

	// ① 还没配域名 → 只能给 IP:端口（这也是要促使用户配域名的原因）
	_, body := doJSON(t, ts, "GET", "/api/instances/inst1/ports", adminTok, nil)
	ports, _ := body["ports"].([]any)
	if len(ports) != 1 {
		t.Fatalf("应有 1 条端口，实际 %d", len(ports))
	}
	first, _ := ports[0].(map[string]any)
	if first["public_address"] != "10.0.0.1:25570" {
		t.Errorf("未配域名时应回落 IP:端口，实际 %v", first["public_address"])
	}

	// ② 给线路配上域名（故意填得"脏"一点，验证归一化）
	code, resp := doJSON(t, ts, "PUT", "/api/frps/"+itoa(frpsID), adminTok,
		map[string]any{"display_domain": "https://mc.example.com/"})
	if code != http.StatusOK {
		t.Fatalf("设置线路域名应 200，实际 %d body=%v", code, resp)
	}
	if resp["display_domain"] != "mc.example.com" {
		t.Errorf("域名应被归一化为 mc.example.com，实际 %v", resp["display_domain"])
	}

	// ③ 现在实例页应当显示域名
	_, body = doJSON(t, ts, "GET", "/api/instances/inst1/ports", adminTok, nil)
	ports, _ = body["ports"].([]any)
	first, _ = ports[0].(map[string]any)
	if first["public_address"] != "mc.example.com:25570" {
		t.Errorf("配了线路域名后应显示 mc.example.com:25570，实际 %v", first["public_address"])
	}

	// ④ 隧道级域名仍然优先于线路级（个别隧道要特殊域名时不能被线路盖掉）
	if _, err := srv.db.Exec(
		`UPDATE tunnels SET display_domain = 'special.example.com:9999' WHERE tunnel_id = ?`,
		"inst1-tcp-25570"); err != nil {
		t.Fatalf("写隧道域名失败: %v", err)
	}
	_, body = doJSON(t, ts, "GET", "/api/instances/inst1/ports", adminTok, nil)
	ports, _ = body["ports"].([]any)
	first, _ = ports[0].(map[string]any)
	if first["public_address"] != "special.example.com:9999" {
		t.Errorf("隧道级域名应优先，实际 %v", first["public_address"])
	}
}

// TestFrpsDomainInList 列表接口也要带上 display_domain，否则前端没法回填输入框。
func TestFrpsDomainInList(t *testing.T) {
	srv, ts := newTestServer(t)
	adminTok := loginAs(t, ts, "admin", "admin123")
	frpsID := seedFrps(t, srv, "10.0.0.1", 25565, 25600)

	doJSON(t, ts, "PUT", "/api/frps/"+itoa(frpsID), adminTok,
		map[string]any{"display_domain": "mc.example.com"})

	_, list := doJSONArr(t, ts, "GET", "/api/frps", adminTok, nil)
	if len(list) != 1 {
		t.Fatalf("应有 1 条线路，实际 %d", len(list))
	}
	if list[0]["display_domain"] != "mc.example.com" {
		t.Errorf("列表应带 display_domain，实际 %v", list[0]["display_domain"])
	}
	// 顺带确认原有字段没被这次改动弄丢
	if list[0]["host"] != "10.0.0.1" {
		t.Errorf("host 字段异常: %v", list[0]["host"])
	}
}

// TestUpdateFrpsGuards 非法 id / 空名称要有明确状态码，且**不允许**改 host 等字段。
func TestUpdateFrpsGuards(t *testing.T) {
	srv, ts := newTestServer(t)
	adminTok := loginAs(t, ts, "admin", "admin123")
	frpsID := seedFrps(t, srv, "10.0.0.1", 25565, 25600)

	if code, _ := doJSON(t, ts, "PUT", "/api/frps/99999", adminTok,
		map[string]any{"display_domain": "x.com"}); code != http.StatusNotFound {
		t.Errorf("改不存在的线路应 404，实际 %d", code)
	}
	if code, _ := doJSON(t, ts, "PUT", "/api/frps/"+itoa(frpsID), adminTok,
		map[string]any{"name": "   "}); code != http.StatusBadRequest {
		t.Errorf("空名称应 400，实际 %d", code)
	}
	// 传 host 应被忽略（不在可改字段里）——改它要让该线路已下发的配置全部重写
	doJSON(t, ts, "PUT", "/api/frps/"+itoa(frpsID), adminTok,
		map[string]any{"display_domain": "x.com", "host": "9.9.9.9"})
	var host string
	if err := srv.db.QueryRow(`SELECT host FROM frps_servers WHERE id = ?`, frpsID).Scan(&host); err != nil {
		t.Fatalf("查线路失败: %v", err)
	}
	if host != "10.0.0.1" {
		t.Errorf("host 不应被 PUT 改掉，实际 %q", host)
	}
}

// TestCreateFrpsWithDomain 创建时就填域名（归一化同样生效）。
func TestCreateFrpsWithDomain(t *testing.T) {
	srv, ts := newTestServer(t)
	adminTok := loginAs(t, ts, "admin", "admin123")

	code, body := doJSON(t, ts, "POST", "/api/frps", adminTok,
		map[string]any{
			"name": "新线路", "host": "1.2.3.4", "bind_port": 7000,
			"port_start": 25565, "port_end": 25600,
			"display_domain": " mc.example.com:9999 ",
		})
	if code != http.StatusCreated {
		t.Fatalf("创建线路应 201，实际 %d body=%v", code, body)
	}
	var domain string
	if err := srv.db.QueryRow(`SELECT display_domain FROM frps_servers ORDER BY id DESC LIMIT 1`).Scan(&domain); err != nil {
		t.Fatalf("查线路失败: %v", err)
	}
	if domain != "mc.example.com" {
		t.Errorf("创建时应归一化域名，实际 %q", domain)
	}
}
