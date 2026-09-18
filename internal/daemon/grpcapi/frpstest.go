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
	frpsTestWait        = 10 * time.Second // 等 frpc 给出结论的上限
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
		lastPort = port

		tmp, err := os.MkdirTemp("", "frps-selftest-")
		if err != nil {
			return nil, err
		}
		logPath := filepath.Join(tmp, "frpc.log")
		confPath := filepath.Join(tmp, "frpc.toml")
		conf := fmt.Sprintf(`# 由 ATL-MCPanel 线路自检生成，用完即删
serverAddr = %q
serverPort = %d
auth.method = "token"
auth.token = %q
log.to = %q
log.level = "info"

[[proxies]]
name = %q
type = "tcp"
localIP = "127.0.0.1"
localPort = %d
remotePort = %d
`, host, bindPort, req.Token, logPath,
			fmt.Sprintf("atlmcpanel-selftest-%d", port), port, port)

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
		resp.RegisterError = "frpc 没有在限定时间内给出结论"
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
	for _, f := range failures {
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
