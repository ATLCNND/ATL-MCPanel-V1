package frp

import (
	"strings"
	"testing"
)

// 两份**脱敏**的真实样例（结构照抄，值全部换掉）。
//
// ⚠️ 永远不要把用户给的真实 token / 域名写进仓库：这个仓库有导出到公开仓的流程，
// 而真实凭据一旦进去就跟着分发出去了。下面这些值都是编的。
//
// 样例一对应 Lolia FRP 的风格：单引号字面量、token 在 [metadatas] 表里、
// 带宽限制用的是 [proxies.transport] 表头。
const sampleLolia = `
Lolia FRP 配置（示例）

serverAddr = 'frp1.example.com'
serverPort = 60000
user = '10001'

[metadatas]
token = 'EXAMPLE-TOKEN-LOLIA-0001'

[[proxies]]
name = 'aaaa0000bbbb1111cccc2222dddd3333'
type = 'tcp'
localIP = '0.0.0.0'
localPort = 25568
remotePort = 62722

[proxies.transport]
bandwidthLimit = '10MB'
bandwidthLimitMode = 'server'
`

// 样例二对应 SakuraFRP 的风格：双引号、token 用点号键 auth.token、
// TLS 开关也是点号键；而且 [[proxies]] 出现在顶层键之后。
const sampleSakura = `
user = "s-00000000000000"
auth.token = "EXAMPLE-TOKEN-SAKURA-0002"

serverAddr = "frp-top.example.net"
serverPort = 8088

transport.tls.enable = false
transport.tls.disableCustomTLSFirstByte = false

[[proxies]]
# id = 00000000
name = "mcpanel"
type = "tcp"

localIP = "127.0.0.1"
localPort = 25566
remotePort = 17139
`

func TestImportLoliaStyle(t *testing.T) {
	cfg, err := ImportConfig(sampleLolia)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if cfg.ServerAddr != "frp1.example.com" || cfg.ServerPort != 60000 {
		t.Errorf("服务端解析错：%q:%d", cfg.ServerAddr, cfg.ServerPort)
	}
	if cfg.User != "10001" {
		t.Errorf("user 应为 10001，实际 %q", cfg.User)
	}
	// 关键：token 在 [metadatas] 表里，必须能认出来
	if cfg.Token != "EXAMPLE-TOKEN-LOLIA-0001" {
		t.Errorf("token 没从 [metadatas] 里取到，实际 %q", cfg.Token)
	}
	if len(cfg.Proxies) != 1 {
		t.Fatalf("隧道条数 = %d，期望 1", len(cfg.Proxies))
	}
	p := cfg.Proxies[0]
	if p.Type != "tcp" || p.LocalPort != 25568 || p.RemotePort != 62722 {
		t.Errorf("隧道字段解析错：%+v", p)
	}
	// [proxies.transport] 表头里的带宽限制
	if p.BandwidthLimit != "10MB" || p.BandwidthLimitMode != "server" {
		t.Errorf("带宽限制没解析到：%+v", p)
	}
}

func TestImportSakuraStyle(t *testing.T) {
	cfg, err := ImportConfig(sampleSakura)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if cfg.ServerAddr != "frp-top.example.net" || cfg.ServerPort != 8088 {
		t.Errorf("服务端解析错：%q:%d", cfg.ServerAddr, cfg.ServerPort)
	}
	// 关键：点号键 auth.token
	if cfg.Token != "EXAMPLE-TOKEN-SAKURA-0002" {
		t.Errorf("token 没从 auth.token 里取到，实际 %q", cfg.Token)
	}
	// 关键：点号键的布尔值（false 不能被当成"没写"）
	if cfg.TLS.Enable {
		t.Error("transport.tls.enable = false 应解析为 false")
	}
	if len(cfg.Proxies) != 1 {
		t.Fatalf("隧道条数 = %d，期望 1", len(cfg.Proxies))
	}
	p := cfg.Proxies[0]
	if p.Name != "mcpanel" || p.LocalPort != 25566 || p.RemotePort != 17139 {
		t.Errorf("隧道字段解析错：%+v", p)
	}
	if p.LocalIP != "127.0.0.1" {
		t.Errorf("localIP = %q", p.LocalIP)
	}
}

// 不认识的键要变成 warning，不能因此让用户没法导入（服务商随时会加新选项）。
func TestImportUnknownKeyIsWarning(t *testing.T) {
	text := sampleSakura + "\nsomeFutureOption = \"x\"\n"
	cfg, err := ImportConfig(text)
	if err != nil {
		t.Fatalf("不认识的键不该导致失败: %v", err)
	}
	found := false
	for _, w := range cfg.Warnings {
		if strings.Contains(w, "someFutureOption") {
			found = true
		}
	}
	if !found {
		t.Errorf("应当对不认识的键给出 warning，实际 warnings=%v", cfg.Warnings)
	}
}

// 注释里的 # 与字符串里的 # 不能混淆。
func TestImportCommentAndHashInString(t *testing.T) {
	text := strings.Replace(sampleSakura, `"mcpanel"`, `"mc#panel"`, 1)
	cfg, err := ImportConfig(text)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if cfg.Proxies[0].Name != "mc#panel" {
		t.Errorf("字符串里的 # 被当成注释切掉了：%q", cfg.Proxies[0].Name)
	}
}

func TestImportErrors(t *testing.T) {
	cases := []struct {
		name string
		text string
		want string
	}{
		{
			name: "没有 serverAddr",
			text: "serverPort = 7000\n[[proxies]]\ntype='tcp'\nlocalPort=1\nremotePort=2\n",
			want: "serverAddr",
		},
		{
			name: "没有隧道",
			text: "serverAddr='h'\nserverPort=7000\n",
			want: "proxies",
		},
		{
			name: "tcp 却给了 subdomain",
			text: "serverAddr='h'\nserverPort=7000\n[[proxies]]\ntype='tcp'\nlocalPort=1\nremotePort=2\nsubdomain='x'\n",
			want: "二选一",
		},
		{
			name: "http 却给了 remotePort",
			text: "serverAddr='h'\nserverPort=7000\n[[proxies]]\ntype='http'\nlocalPort=1\nremotePort=2\ncustomDomains=['a.com']\n",
			want: "二选一",
		},
		{
			name: "端口超范围",
			text: "serverAddr='h'\nserverPort=99999\n[[proxies]]\ntype='tcp'\nlocalPort=1\nremotePort=2\n",
			want: "serverPort",
		},
		{
			name: "端口值不是数字",
			text: "serverAddr='h'\nserverPort='abc'\n[[proxies]]\ntype='tcp'\nlocalPort=1\nremotePort=2\n",
			want: "serverPort",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := ImportConfig(c.text)
			if err == nil {
				t.Fatalf("应当报错，实际没有")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("错误信息应包含 %q，实际 %q", c.want, err.Error())
			}
		})
	}
}

// 报错必须**具体到行**：用户改配置时最需要知道是哪一行。
func TestImportErrorHasLineNumber(t *testing.T) {
	text := "serverAddr='h'\nserverPort=7000\n[[proxies]]\ntype='tcp'\nlocalPort=1\nremotePort=2\nsubdomain='x'\n"
	_, err := ImportConfig(text)
	if err == nil {
		t.Fatal("应当报错")
	}
	// 这条是语义错误（不是行内语法错误），至少要能指出是哪一个隧道
	if !strings.Contains(err.Error(), "proxy") && !strings.Contains(err.Error(), "隧道") {
		t.Errorf("错误信息应指明是哪条隧道：%q", err.Error())
	}
}

func TestImportHTTPWithDomain(t *testing.T) {
	text := `
serverAddr = "h"
serverPort = 7000
auth.token = "t"

[[proxies]]
name = "web"
type = "http"
localPort = 8080
customDomains = ["a.example.com", "b.example.com"]
`
	cfg, err := ImportConfig(text)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	p := cfg.Proxies[0]
	if len(p.CustomDomains) != 2 || p.CustomDomains[1] != "b.example.com" {
		t.Errorf("customDomains 解析错：%v", p.CustomDomains)
	}
}

// token 出自哪个位置必须记下来 —— 服务端只认自己那个位置（见 ImportedConfig.TokenField）。
func TestImportRecordsTokenField(t *testing.T) {
	cfg, err := ImportConfig(sampleLolia)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if cfg.TokenField != "metadatas" {
		t.Errorf("Lolia 样例的 token 来自 [metadatas]，实际记成 %q", cfg.TokenField)
	}
	cfg2, err := ImportConfig(sampleSakura)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if cfg2.TokenField != "auth" {
		t.Errorf("Sakura 样例的 token 来自 auth.token，实际记成 %q", cfg2.TokenField)
	}
	// user 也要能读出来（Lolia 不写 user 登不上）
	if cfg.User != "10001" {
		t.Errorf("user = %q", cfg.User)
	}
}
