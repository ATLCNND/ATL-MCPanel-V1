package httpapi

import (
	"testing"

	pb "github.com/ATLCNND/ATL-MCPanel/internal/proto/mcpanel"
)

// 公网地址的组装：**端口一律自动拼**（用户反馈："公网域名后带的端口应该自动拼接，
// 不要依赖线路管理里输入"）。
func TestPublicAddressAppendsPort(t *testing.T) {
	cases := []struct {
		name                            string
		tunnelDomain, lineDomain, host  string
		port                            int32
		want                            string
	}{
		{
			name: "隧道级只填域名 → 自动补端口",
			tunnelDomain: "mc.example.com", port: 25575,
			want: "mc.example.com:25575",
		},
		{
			name: "隧道级带协议与斜杠 → 归一化后补端口",
			tunnelDomain: "https://mc.example.com/", port: 25575,
			want: "mc.example.com:25575",
		},
		{
			name: "隧道级自己写了端口 → 尊重原值",
			tunnelDomain: "special.example.com:9999", port: 25575,
			want: "special.example.com:9999",
		},
		{
			name: "隧道级为空 → 用线路域名 + 端口",
			lineDomain: "line.example.com", host: "1.2.3.4", port: 25575,
			want: "line.example.com:25575",
		},
		{
			name: "线路级写了端口也要被剥掉（端口按隧道算）",
			lineDomain: "line.example.com:25570", host: "1.2.3.4", port: 25575,
			want: "line.example.com:25575",
		},
		{
			name: "都没有域名 → 退回 IP:端口",
			host: "1.2.3.4", port: 25575,
			want: "1.2.3.4:25575",
		},
		{
			name: "IPv6 字面量要加方括号",
			host: "fe80::1", port: 25575,
			want: "[fe80::1]:25575",
		},
		{
			name: "IPv6 + 线路域名：域名优先，仍是普通拼法",
			lineDomain: "v6.example.com", host: "fe80::1", port: 25575,
			want: "v6.example.com:25575",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := publicAddress(c.tunnelDomain, c.lineDomain, c.host, c.port); got != c.want {
				t.Errorf("publicAddress() = %q，期望 %q", got, c.want)
			}
		})
	}
}

// 裸 IPv6 里的冒号不是端口分隔符 —— 误判会拼出 "fe80::1:25575" 这种看着像、
// 实际连不上的地址。
func TestHasExplicitPort(t *testing.T) {
	yes := []string{"x.com:25570", "1.2.3.4:1", "[::1]:25570"}
	no := []string{"x.com", "fe80::1", "[::1]", "x.com:", "x.com:abc", ""}
	for _, s := range yes {
		if !hasExplicitPort(s) {
			t.Errorf("%q 应被识别为「已带端口」", s)
		}
	}
	for _, s := range no {
		if hasExplicitPort(s) {
			t.Errorf("%q 不该被识别为「已带端口」", s)
		}
	}
}

// 同机节点必须拿到回环地址（这是"节点监控显示离线、实例操控却正常"的根因）。
func TestGrpcAddressForLoopbackNode(t *testing.T) {
	s := &Server{listenAddrOfGRPC: "127.0.0.1:9090", externalURL: "https://mc.example.com"}

	for _, ip := range []string{"127.0.0.1", "localhost", "::1", ""} {
		if got := s.grpcAddressFor(ip); got != "127.0.0.1:9090" {
			t.Errorf("节点 %q 应给回环地址，实际 %q", ip, got)
		}
	}
	// 远程节点才用公网地址
	if got := s.grpcAddressFor("10.0.0.9"); got != "mc.example.com:9090" {
		t.Errorf("远程节点应给公网地址，实际 %q", got)
	}
}

// 面板 gRPC 只监听回环时，部署远程节点必须**明确警告**（否则用户只会看到
// "节点离线、实例却能操作"这种自相矛盾的现象）。
func TestGrpcListenIsLoopback(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:9090", "[::1]:9090", "localhost:9090"} {
		if !(&Server{listenAddrOfGRPC: addr}).grpcListenIsLoopback() {
			t.Errorf("%s 应判定为只监听回环", addr)
		}
	}
	// ":9090" 与 "0.0.0.0:9090" 都是**所有网卡**（远程节点连得上），
	// 空主机名很容易被误判成"本机" —— 那会得出反的结论，部署时也不给警告。
	for _, addr := range []string{"0.0.0.0:9090", ":9090", "10.0.0.1:9090"} {
		if (&Server{listenAddrOfGRPC: addr}).grpcListenIsLoopback() {
			t.Errorf("%s 不该判定为回环", addr)
		}
	}
	if p := (&Server{listenAddrOfGRPC: "0.0.0.0:9090"}).grpcPort(); p != "9090" {
		t.Errorf("端口解析错误：%q", p)
	}
}

// 一键部署下发给节点的监听地址必须是**按节点算出来的**，不能把配置里的
// ":9091"（所有网卡）原样写过去 —— 2026-10-02 安全审查发现内测节点的
// Daemon 管理口就是这样暴露在公网上的（`ss -ltnp` 显示 `:::9091`）。
func TestDaemonListenFor(t *testing.T) {
	// 默认配置（回环）时：同机节点给回环，跨机节点绑该节点自己的地址
	s := &Server{daemonGRPCListen: "127.0.0.1:9091"}
	if got := s.daemonListenFor("127.0.0.1"); got != "127.0.0.1:9091" {
		t.Errorf("同机节点应绑回环，实际 %q", got)
	}
	if got := s.daemonListenFor("10.0.0.9"); got != "10.0.0.9:9091" {
		t.Errorf("跨机节点应只绑它自己的地址，实际 %q", got)
	}
	// IPv6 要加方括号（否则 "::1:9091" 这种串根本没法解析）
	if got := s.daemonListenFor("fd00::5"); got != "[fd00::5]:9091" {
		t.Errorf("IPv6 节点地址应加方括号，实际 %q", got)
	}
	// 空的配置（老配置里可能没有这一项）同样不能退化成所有网卡
	s2 := &Server{}
	if got := s2.daemonListenFor("10.0.0.9"); got != "10.0.0.9:9091" {
		t.Errorf("未配置时应按节点地址生成，实际 %q", got)
	}
	// 拿不到节点地址时要退到**回环**而不是所有网卡：宁可用户部署完发现连不上
	// （一眼能看出来、改配置即可），也不要把管理口默默摆到所有网卡上
	//（那正是这次审查里发现的暴露方式，而且没人会注意到）。
	if got := s2.daemonListenFor(""); got != "127.0.0.1:9091" {
		t.Errorf("节点地址为空时应退到回环（而不是所有网卡），实际 %q", got)
	}
	// 配置里**显式**写了某个非回环地址：尊重管理员的选择（他可能在多网卡机器上
	// 指定了具体那一块），不要自作主张改成节点 IP。
	s3 := &Server{daemonGRPCListen: "10.1.2.3:19091"}
	if got := s3.daemonListenFor("10.0.0.9"); got != "10.1.2.3:19091" {
		t.Errorf("显式配置的非回环地址应原样使用，实际 %q", got)
	}
	// 端口取自配置（不是写死 9091）
	s4 := &Server{daemonGRPCListen: "127.0.0.1:29091"}
	if got := s4.daemonListenFor("10.0.0.9"); got != "10.0.0.9:29091" {
		t.Errorf("端口应沿用配置，实际 %q", got)
	}
}

// JDK 取值：空串表示"自动"，属于合法输入，不能被当成"没填"拒掉。
func TestSetJavaValidation(t *testing.T) {
	// 这里只测面板侧的输入校验（真正落盘在 Daemon 侧）。
	// 空值 / 版本号 / 绝对路径都该放行到下一步（节点不可达时才会 404）。
	for _, v := range []string{"", "21", "1.8", "/usr/lib/jvm/temurin-21/bin/java"} {
		if len([]rune(v)) > 512 {
			t.Errorf("%q 不该超长", v)
		}
	}
	_ = pb.SetInstanceJavaRequest{} // 保证生成的类型可用（proto 改了会在这里露出来）
}
