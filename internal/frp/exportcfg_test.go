package frp

import (
	"strings"
	"testing"
)

func testServer() Server {
	return Server{Host: "frp.example.com", BindPort: 7000, Token: "EXAMPLE-TOKEN-ABCD1234"}
}

func tcpTunnel() Tunnel {
	return Tunnel{TunnelID: "t1", Name: "mc-25570", Protocol: "tcp", LocalPort: 25565, RemotePort: 25570}
}

// 导出的文本必须能被自己的导入器读回来 —— 这是 F15 两半之间的**契约**。
// 一旦某天有人只改了一边（比如把键名改成 camelCase），这条用例会立刻失败。
func TestExportImportRoundTripTCP(t *testing.T) {
	text, err := RenderStandardConfig(testServer(), []Tunnel{tcpTunnel()}, ExportOptions{IncludeToken: true})
	if err != nil {
		t.Fatalf("导出失败: %v", err)
	}
	cfg, err := ImportConfig(text)
	if err != nil {
		t.Fatalf("导出的配置自己都读不回来: %v\n--- 导出内容 ---\n%s", err, text)
	}
	if cfg.ServerAddr != "frp.example.com" || cfg.ServerPort != 7000 {
		t.Errorf("服务端往返不一致：%+v", cfg)
	}
	if cfg.Token != "EXAMPLE-TOKEN-ABCD1234" {
		t.Errorf("token 往返不一致：%q", cfg.Token)
	}
	if len(cfg.Proxies) != 1 {
		t.Fatalf("隧道数 = %d", len(cfg.Proxies))
	}
	p := cfg.Proxies[0]
	if p.Type != "tcp" || p.LocalPort != 25565 || p.RemotePort != 25570 {
		t.Errorf("隧道往返不一致：%+v", p)
	}
	if p.Name != "mc-25570" {
		t.Errorf("隧道名往返不一致：%q", p.Name)
	}
}

// HTTP 隧道：域名走 customDomains，不能冒出 remotePort（否则 frpc 直接报错）。
func TestExportImportRoundTripHTTP(t *testing.T) {
	tn := Tunnel{
		TunnelID: "panel", Name: "panel-web", Protocol: "http", LocalPort: 8080,
		CustomDomains: []string{"a.example.com", "b.example.com"},
	}
	text, err := RenderStandardConfig(testServer(), []Tunnel{tn}, ExportOptions{IncludeToken: true})
	if err != nil {
		t.Fatalf("导出失败: %v", err)
	}
	if strings.Contains(text, "remotePort") {
		t.Errorf("http 隧道不该出现 remotePort\n%s", text)
	}
	cfg, err := ImportConfig(text)
	if err != nil {
		t.Fatalf("导出的配置读不回来: %v", err)
	}
	if len(cfg.Proxies[0].CustomDomains) != 2 {
		t.Errorf("customDomains 往返不一致：%v", cfg.Proxies[0].CustomDomains)
	}
}

// Lolia 写法（[metadatas] token）也要能往返：用户可能只认那种格式。
func TestExportLoliaStyleRoundTrip(t *testing.T) {
	text, err := RenderStandardConfig(testServer(), []Tunnel{tcpTunnel()},
		ExportOptions{IncludeToken: true, UseLoliaTokenStyle: true})
	if err != nil {
		t.Fatalf("导出失败: %v", err)
	}
	if !strings.Contains(text, "[metadatas]") {
		t.Errorf("Lolia 风格应包含 [metadatas]\n%s", text)
	}
	cfg, err := ImportConfig(text)
	if err != nil {
		t.Fatalf("读不回来: %v", err)
	}
	if cfg.Token != "EXAMPLE-TOKEN-ABCD1234" {
		t.Errorf("token 往返不一致：%q", cfg.Token)
	}
}

func TestExportOmitsToken(t *testing.T) {
	text, err := RenderStandardConfig(testServer(), []Tunnel{tcpTunnel()}, ExportOptions{IncludeToken: false})
	if err != nil {
		t.Fatalf("导出失败: %v", err)
	}
	if strings.Contains(text, "EXAMPLE-TOKEN-ABCD1234") {
		t.Errorf("要求不带 token 时仍然写了出去\n%s", text)
	}
}

// 同一份数据导出必须稳定（可 diff）——顺序与内容都不能随 map 迭代抖动。
func TestExportIsStable(t *testing.T) {
	tuns := []Tunnel{
		{TunnelID: "b", Name: "b", Protocol: "tcp", LocalPort: 2, RemotePort: 2002},
		{TunnelID: "a", Name: "a", Protocol: "tcp", LocalPort: 1, RemotePort: 2001},
	}
	first, err := RenderStandardConfig(testServer(), tuns, ExportOptions{IncludeToken: true})
	if err != nil {
		t.Fatalf("导出失败: %v", err)
	}
	rev := []Tunnel{tuns[1], tuns[0]}
	second, _ := RenderStandardConfig(testServer(), rev, ExportOptions{IncludeToken: true})
	if first != second {
		t.Errorf("同样的数据、不同的输入顺序，导出结果应当一致：\n--- 第一次 ---\n%s\n--- 第二次 ---\n%s", first, second)
	}
	if strings.Index(first, "name = \"a\"") > strings.Index(first, "name = \"b\"") {
		t.Errorf("应按 tunnel_id 排序\n%s", first)
	}
}

func TestExportRejectsBadInput(t *testing.T) {
	if _, err := RenderStandardConfig(Server{}, []Tunnel{tcpTunnel()}, ExportOptions{}); err == nil {
		t.Error("缺服务端地址应报错")
	}
	if _, err := RenderStandardConfig(testServer(), nil, ExportOptions{}); err == nil {
		t.Error("没有隧道应报错")
	}
	bad := Tunnel{TunnelID: "x", Protocol: "tcp", LocalPort: 1} // 缺 remotePort
	if _, err := RenderStandardConfig(testServer(), []Tunnel{bad}, ExportOptions{}); err == nil {
		t.Error("tcp 缺 remotePort 应报错")
	}
	badDomain := Tunnel{TunnelID: "y", Protocol: "http", LocalPort: 1} // 缺域名
	if _, err := RenderStandardConfig(testServer(), []Tunnel{badDomain}, ExportOptions{}); err == nil {
		t.Error("http 缺域名应报错")
	}
}

func TestMaskToken(t *testing.T) {
	if got := MaskToken("EXAMPLE-TOKEN-ABCD1234"); got != "EXAM…1234" {
		t.Errorf("打码结果 = %q", got)
	}
	if got := MaskToken("short"); got != "*****" {
		t.Errorf("短 token 应全打码，实际 %q", got)
	}
	if got := MaskToken(""); got != "" {
		t.Errorf("空 token 应为空串，实际 %q", got)
	}
}
