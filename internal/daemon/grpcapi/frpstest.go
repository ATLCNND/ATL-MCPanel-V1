package grpcapi

import (
	"context"
	"fmt"
	"log/slog"
	"math/rand"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	pb "github.com/ATLCNND/ATL-MCPanel/internal/proto/mcpanel"
)

// 线路（frps）自检。
//
// 为什么值得做：一条线路填错的代价是**延迟发现** —— 地址写错、token 不符、
// 端口段已被别人占满，这些都要等到某个实例下发隧道、玩家连不上时才会暴露，
// 而那时现象是"公网地址连不上"，排查要翻 frpc 日志。
// 这里在添加线路时就把两层都测掉：
//
//	① 节点到 host:bind_port 的 TCP 通不通（面板能连通 ≠ 节点能连通）
//	② 拿这个 token 登录、并在端口段里真的占到一个端口（= 完整走一遍 frpc 启动）
//
// 自检完立刻把临时代理撤掉，不留痕迹。
const (
	frpsTestDialTimeout = 5 * time.Second
	// 等 frpc 给出结论的上限。
	//
	// 2026-10-21 由 10s 放宽到 15s：同一份配置在内测节点上 300ms 就有结论，
	// 而在构建机上（到服务商 1000ms+ 往返）10 秒都没等到 —— 超时会让用户以为
	// "线路有问题"，其实只是这条链路慢。多等 5 秒换一个确定结论是划算的。
	frpsTestWait        = 15 * time.Second
	frpsTestPollEvery   = 200 * time.Millisecond
	frpsTestTries       = 3 // 端口被占时换一个端口重试的次数
)

// TestFrps 在**本节点上**自检一条线路。
func (s *Server) TestFrps(ctx context.Context, req *pb.TestFrpsRequest) (*pb.TestFrpsResponse, error) {
	host := strings.TrimSpace(req.Host)
	if host == "" {
		return &pb.TestFrpsResponse{Success: false, Error: "线路地址为空"}, nil
	}
	bindPort := req.BindPort
	if bindPort <= 0 {
		bindPort = 7000
	}
	addr := net.JoinHostPort(host, strconv.Itoa(int(bindPort)))
	resp := &pb.TestFrpsResponse{}

	// ---- ① TCP 可达性 ----
	start := time.Now()
	conn, err := net.DialTimeout("tcp", addr, frpsTestDialTimeout)
	resp.DialMs = time.Since(start).Milliseconds()
	if err != nil {
		resp.DialError = err.Error()
		resp.Error = fmt.Sprintf("节点连不上 %s：%v", addr, err)
		return resp, nil
	}
	_ = conn.Close()
	resp.DialOk = true

	// 只测可达（公网直出线路用）：这类线路没有 frpc 可跑，
	// 再往下走"登录/注册"没有意义，直接返回拨测结果。
	if req.DialOnly {
		resp.Success = true
		return resp, nil
	}

	// ---- ② 真的用 frpc 注册一个临时代理 ----
	frpcPath, err := exec.LookPath("frpc")
	if err != nil {
		resp.Error = "节点上没有 frpc（穿透功能不可用），无法完成注册自检"
		resp.RegisterError = "未在 PATH 中找到 frpc"
		return resp, nil
	}

	portStart, portEnd := req.PortStart, req.PortEnd
	if portStart <= 0 {
		portStart = 25565
	}
	if portEnd < portStart {
		portEnd = portStart + 100
	}
	// 端口段可能很大，试注册只需要几个候选：随机取，避免每次都撞同一个被占的端口
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	span := int(portEnd-portStart) + 1

	var lastLog, lastErr string
	var lastPort int32
	for attempt := 0; attempt < frpsTestTries; attempt++ {
		port := portStart + int32(rng.Intn(span))
		proxyName := fmt.Sprintf("atlmcpanel-selftest-%d", port)
		// 第三方单隧道（底账 F16）：远端端口与代理名**都是服务商分配的**，
		// 自己造一个上去必被拒（实测回「FRPC 配置文件错误」）。
		// 面板会把该线路下真实隧道的参数传进来，这里优先用它。
		if req.RemotePort > 0 {
			port = req.RemotePort
		}
		if req.ProxyName != "" {
			proxyName = req.ProxyName
		}
		lastPort = port

		tmp, err := os.MkdirTemp("", "frps-selftest-")
		if err != nil {
			return nil, err
		}
		logPath := filepath.Join(tmp, "frpc.log")
		confPath := filepath.Join(tmp, "frpc.toml")
		conf := selfTestConfig(host, bindPort, req, logPath, proxyName, port)

		if err := os.WriteFile(confPath, []byte(conf), 0o600); err != nil {
			os.RemoveAll(tmp)
			return nil, err
		}

		ok, decisive, reason, logText := runFrpcSelfTest(frpcPath, tmp, confPath, logPath)
		os.RemoveAll(tmp)
		lastLog, lastErr = logText, reason

		if ok {
			resp.RegisterOk = true
			resp.UsedPort = port
			resp.Success = true
			slog.Info("线路自检通过", "addr", addr, "port", port, "dial_ms", resp.DialMs)
			return resp, nil
		}
		if !decisive {
			// 超时：没有明确结论，换端口再试没有意义
			break
		}
		// 端口被占 → 换个端口重试；其它错误（token 不对、连不上）直接返回
		if !strings.Contains(reason, "port") {
			break
		}
	}

	resp.RegisterError = lastErr
	resp.UsedPort = lastPort
	resp.LogTail = lastLog
	if resp.RegisterError == "" {
		// 超时也要说人话：把"它到底走到哪一步"告诉用户，
		// 否则只看到一句"没有给出结论"，既不知道是不是网络问题，也不知道等了多久。
		switch {
		case strings.Contains(lastLog, "try to connect to server"):
			resp.RegisterError = fmt.Sprintf(
				"frpc 已发起连接，但 %d 秒内既没登录成功、也没报错（到服务商的链路可能很慢或不稳；完整日志见下）",
				int(frpsTestWait.Seconds()))
		case lastLog == "":
			resp.RegisterError = "frpc 没有写出任何日志（二进制没起来？见下）"
		default:
			resp.RegisterError = fmt.Sprintf("frpc 在 %d 秒内没有给出结论（完整日志见下）",
				int(frpsTestWait.Seconds()))
		}
	}
	resp.Error = fmt.Sprintf("线路自检未通过（TCP 可达，但注册失败）：%s", resp.RegisterError)
	return resp, nil
}

// runFrpcSelfTest 起一个临时 frpc 并等它给出结论。
//
// 返回 (成功, 是否有明确结论, 原因, 日志文本)。
// 有明确结论才值得重试；"超时无结论"说明环境有问题（例如 frpc 起不来），重试没意义。
func runFrpcSelfTest(frpcPath, dir, confPath, logPath string) (bool, bool, string, string) {
	cmd := exec.Command(frpcPath, "-c", confPath)
	cmd.Dir = dir
	// 独立进程组：收尾时连同可能派生出来的进程一起终止
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// frpc 的日志走 log.to 指定的文件（与实例隧道一致），这里不需要 stdout
	cmd.Stdout = nil
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		return false, true, "启动 frpc 失败: " + err.Error(), ""
	}
	defer func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
		done := make(chan struct{})
		go func() { _, _ = cmd.Process.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		}
	}()

	deadline := time.Now().Add(frpsTestWait)
	var logText string
	for time.Now().Before(deadline) {
		time.Sleep(frpsTestPollEvery)
		b, _ := os.ReadFile(logPath)
		logText = string(b)
		ok, decisive, reason := classifyFrpcSelfTestLog(logText)
		if decisive {
			return ok, true, reason, logTail(logText, 1200)
		}
	}
	return false, false, "", logTail(logText, 1200)
}

// classifyFrpcSelfTestLog 从 frpc 日志判断自检结果。
//
// 单独抽出来是为了能**单测**：frp 的日志措辞随版本变，这里只认几种关键句式，
// 并且把"有结论"与"没结论"分开 —— 后者要继续等，前者可以立刻下结论。
//
// 成功需要两条都出现：
//
//	login to server success   —— token / 地址没问题
//	start proxy success       —— 端口段里真的占到了一个端口
//
// 只见 login 成功、proxy 失败（如端口被占）也算"有结论但不成功"。
func classifyFrpcSelfTestLog(log string) (ok bool, decisive bool, reason string) {
	lower := strings.ToLower(log)
	loginOK := strings.Contains(lower, "login to server success")
	proxyOK := strings.Contains(lower, "start proxy success")

	if loginOK && proxyOK {
		return true, true, ""
	}
	// 明确的失败信号（按"最容易看懂的结论"优先）
	failures := []struct {
		needle string
		reason string
	}{
		{"token in login doesn't match", "token 与 frps 配置不一致（服务端拒绝了登录）"},
		{"authorization failed", "frps 拒绝授权（token 不对？）"},
		{"port already used", "端口段里的端口已被占用"},
		{"start error: port already used", "端口段里的端口已被占用"},
		{"connect to server error", "frpc 连不上 frps（地址/端口/防火墙）"},
		{"i/o timeout", "连接 frps 超时（地址不可达或端口被拦）"},
		{"connection refused", "frps 拒绝连接（服务没起？端口不对？）"},
		{"no such host", "线路地址解析不了（域名写错？）"},
	}
	// ⚠️ 「服务端/客户端给的原话」优先级最高。
	//
	// 2026-10-21 真机案例：第三方服务商（Lolia / Sakura 这类跑自制 frps 的）在登录阶段
	// 直接回一句中文「FRPC 配置文件错误,请检查后重试,请反馈给管理员以解决这个问题」，
	// 而 frpc 把它包在 "connect to server error: …" 里。旧逻辑命中 connect to server error
	// 就一口咬定「连不上 frps（地址/端口/防火墙）」—— 把服务端的拒绝说成了网络问题，
	// 用户于是去查防火墙，方向完全错了。
	//
	// 现在的规则：只要 connect to server error 后面跟了具体内容，就**原样呈现**，
	// 并明确区分「网络层连不上」与「连上了但被服务端拒绝」。
	if i := strings.Index(lower, "connect to server error"); i >= 0 {
		msg := serverErrorMessage(log, i)
		if msg != "" {
			return false, true, describeServerRefusal(msg)
		}
		return false, true, "frpc 连不上 frps（地址/端口/防火墙）"
	}
	for _, f := range failures {
		if f.needle == "connect to server error" {
			continue // 上面已单独处理
		}
		if strings.Contains(lower, f.needle) {
			return false, true, f.reason
		}
	}
	// 登录失败但没命中上面的措辞：至少知道登录没过
	if strings.Contains(lower, "login to server failed") {
		return false, true, "登录 frps 失败（详见日志）"
	}
	return false, false, ""
}

// logTail 取日志尾部若干字节（失败时给人看原始原因）。
func logTail(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return "…" + s[len(s)-max:]
}

// serverErrorMessage 从日志里取出 "connect to server error: <msg>" 的 <msg>。
//
// idx 是 lc（小写副本）里那个关键字的位置；真正要截取的是**原文**，
// 因为服务端消息可能是中文，而小写只用于匹配。
func serverErrorMessage(log string, idx int) string {
	const key = "connect to server error"
	if idx < 0 || idx+len(key) > len(log) {
		return ""
	}
	rest := strings.TrimSpace(log[idx+len(key):])
	rest = strings.TrimPrefix(rest, ":")
	rest = strings.TrimSpace(rest)
	if i := strings.IndexByte(rest, '\n'); i >= 0 {
		rest = strings.TrimSpace(rest[:i])
	}
	return rest
}

// describeServerRefusal 把「服务端拒绝登录」的措辞整理成用户可以照着做的结论。
//
// 关键是把**责任方**说对：连上了但被拒绝 ≠ 网络不通。
// 这类服务商（自制 frps 的那种）通常要求使用**它们自己提供的客户端**，
// 标准 frpc 无论如何配置都过不了握手 —— 与其让用户反复改 token，不如直接说清。
func describeServerRefusal(msg string) string {
	low := strings.ToLower(msg)
	looksLikeAuth := strings.Contains(msg, "配置") || strings.Contains(low, "config") ||
		strings.Contains(low, "token") || strings.Contains(msg, "参数") ||
		strings.Contains(msg, "认证") || strings.Contains(low, "auth")
	if looksLikeAuth {
		return "frps 拒绝登录（TCP 已连通，是服务端在握手阶段拒绝）：" + msg +
			"\n提示：这类服务商若使用自制的 frps，通常只认**他们自己提供的客户端**，标准 frpc 改 token 也过不去。"
	}
	return "frps 拒绝了这次连接：" + msg
}

// selfTestConfig 生成自检用的 frpc 配置。
//
// 与 renderConfig 分开写（而不是复用）：自检的配置是**一次性的探测**，要的是
// 「用最小集合复现一次登录 + 注册」；复用生产那条路会把日志路径、证书、
// vhost 等一堆无关变量带进来，出问题反而更难判断。
//
// 但**认证方式必须与生产一致**：第三方服务商的 token 位置不同，
// 这里若固定写 auth.token，对 Lolia 这类就永远自检失败（实测就是这样）。
func selfTestConfig(host string, bindPort int32, req *pb.TestFrpsRequest, logPath, proxyName string, remotePort int32) string {
	var sb strings.Builder
	sb.WriteString("# 由 ATL-MCPanel 线路自检生成，用完即删\n")
	fmt.Fprintf(&sb, "serverAddr = %q\n", host)
	fmt.Fprintf(&sb, "serverPort = %d\n", bindPort)
	if req.User != "" {
		fmt.Fprintf(&sb, "user = %q\n", req.User)
	}
	if req.Token != "" {
		if req.AuthMode == "metadatas" {
			fmt.Fprintf(&sb, "metadatas.token = %q\n", req.Token)
		} else {
			fmt.Fprintf(&sb, "auth.method = \"token\"\nauth.token = %q\n", req.Token)
		}
	}
	fmt.Fprintf(&sb, "log.to = %q\n", logPath)
	sb.WriteString("log.level = \"info\"\n")
	sb.WriteString("\n[[proxies]]\n")
	fmt.Fprintf(&sb, "name = %q\n", proxyName)
	sb.WriteString("type = \"tcp\"\n")
	sb.WriteString("localIP = \"127.0.0.1\"\n")
	fmt.Fprintf(&sb, "localPort = %d\n", remotePort)
	fmt.Fprintf(&sb, "remotePort = %d\n", remotePort)
	// 服务商的传输参数必须一并带上：实测 Lolia 缺了这段就回
	// 「FRPC 配置文件错误」（同 name + remotePort 带上就成功）。
	if req.BandwidthLimit != "" || req.BandwidthLimitMode != "" {
		sb.WriteString("\n[proxies.transport]\n")
		if req.BandwidthLimit != "" {
			fmt.Fprintf(&sb, "bandwidthLimit = %q\n", req.BandwidthLimit)
		}
		if req.BandwidthLimitMode != "" {
			fmt.Fprintf(&sb, "bandwidthLimitMode = %q\n", req.BandwidthLimitMode)
		}
	}
	return sb.String()
}
