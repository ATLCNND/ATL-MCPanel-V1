package mcprocess

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// 实例的「端口」有三处出现，必须指向同一个值：
//
//	panel DB instances.port ──► instance.json 的 port ──► 隧道下发的 local_port
//	                         └─► 服务端**真正监听**的端口（server.properties）
//
// 面板能保证上面那条链（隧道就是它按 instances.port 生成的），
// 但服务端监听哪个端口完全由它自己的 server.properties 决定 —— 面板与 Daemon
// 此前都不写这个文件，于是出现了一个沉默的错配：
//
//	实例 port=25567 → 隧道 local_port=25567 → 服务端却监听 25565
//
// 结果：公网地址连不上，而面板里隧道显示 running（frpc 确实活着，只是它后面
// 没有任何服务在监听）。只有 port 恰好是默认值 25565 的实例能侥幸正常工作，
// 所以这个 bug 长期没被发现（test1 就是 25565）。
//
// 这里在**每次启动前**把 server-port 校准为实例的 port —— 这是"实例的 port"
// 唯一的落地处。放在启动前而不是创建时，是因为还要覆盖这些情况：
// 本功能上线前建的老实例、用户手工改过 server-port、以及服务端首次启动才
// 生成 server.properties 的实例。
const (
	serverPropsFile = "server.properties"
	serverPortKey   = "server-port"
)

// syncServerPort 把 server.properties 里的 server-port 校准为实例配置的 port。
//
// 返回 (changed, oldPort, err)：
//   - changed=true 表示确实改写了文件；oldPort 是改之前的值（0 表示原本没有这一行）
//   - err 只在真的读写失败时非 nil —— 调用方应**继续启动**，端口校准失败
//     不该拦住实例（它是修正一致性的动作，不是启动的前置条件）
//
// 行为上刻意做的两个克制：
//   - 已有 server.properties 时**只改 server-port 这一行**，其余内容逐字节保留
//     （用户可能调过 motd / 难度 / 白名单，整体覆盖是不可接受的）
//   - 值已经一致时**不写文件**（避免每次启动都动 mtime，也让"文件被谁改了"可追溯）
//
// customStart 为 true 且文件不存在时不创建：自定义启动命令的实例可能根本不是
// Minecraft 服务端（脚本、代理、机器人），凭空塞一个 server.properties 没有意义。
func syncServerPort(dir string, port int, customStart bool) (bool, int, error) {
	if port <= 0 || port > 65535 {
		return false, 0, nil // 端口没配置/不合法：不猜，保持原样
	}

	path := filepath.Join(dir, serverPropsFile)
	b, err := os.ReadFile(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return false, 0, err
		}
		if customStart {
			return false, 0, nil
		}
		// 还没有 server.properties：默认 java 启动的实例一定是 MC 服务端，
		// 先写一行把端口定下来，服务端首次启动会把其余默认项补齐
		// （缺项会取默认值并在启动时写回完整文件）。
		if err := os.WriteFile(path, []byte(fmt.Sprintf("%s=%d\n", serverPortKey, port)), 0o644); err != nil {
			return false, 0, err
		}
		return true, 0, nil
	}

	lines := strings.Split(string(b), "\n")
	old, found, same := 0, false, false
	for idx, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		key, val, ok := strings.Cut(trimmed, "=")
		if !ok || strings.TrimSpace(key) != serverPortKey {
			continue
		}
		found = true
		if n, convErr := strconv.Atoi(strings.TrimSpace(val)); convErr == nil {
			old = n
		}
		if old == port {
			same = true
			break
		}
		lines[idx] = replacePortValue(line, port)
		break
	}

	if same {
		return false, old, nil // 已经一致：不动文件
	}
	if !found {
		// 缺这一行：追加。文件通常以 \n 结尾，Split 会产生一个空尾元素，
		// 直接 append 会多出一个空行 —— 用它来放新行。
		if n := len(lines); n > 0 && lines[n-1] == "" {
			lines[n-1] = fmt.Sprintf("%s=%d", serverPortKey, port)
		} else {
			lines = append(lines, fmt.Sprintf("%s=%d", serverPortKey, port))
		}
	}

	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")), 0o644); err != nil {
		return false, old, err
	}
	return true, old, nil
}

// replacePortValue 只替换 "=" 之后的值，保留缩进与行尾的 \r（CRLF 文件）。
func replacePortValue(line string, port int) string {
	eq := strings.Index(line, "=")
	if eq < 0 {
		return line
	}
	suffix := ""
	if strings.HasSuffix(line, "\r") {
		suffix = "\r"
	}
	return line[:eq+1] + strconv.Itoa(port) + suffix
}

// syncServerPortLogged 执行校准并把结果写进日志（Daemon 日志里能看到"端口被改了"）。
func (i *Instance) syncServerPortLogged() {
	changed, old, err := syncServerPort(i.Dir, i.Port, i.StartCommand != "")
	if err != nil {
		slog.Warn("校准服务端监听端口失败", "instance", i.ID, "port", i.Port, "error", err)
		return
	}
	if !changed {
		return
	}
	slog.Info("已把服务端监听端口校准为实例端口",
		"instance", i.ID, "port", i.Port, "old_port", old, "file", serverPropsFile)
}
