package mcprocess

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/ATLCNND/ATL-MCPanel/internal/common/safepath"
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
//
// 读与写都用 O_NOFOLLOW（见 safepath.OpenNoFollow）。server.properties 在实例
// 目录里、租户随手可改，而这里是以 **root** 的身份去读它、改写它：一个
// `server.properties -> /etc/ld.so.preload` 的软链接就能让 Daemon 在下次启动
// 实例时建出/改写节点上的任意文件（写进 ld.so.preload 会让所有动态链接的程序
// 都起不来，等于把整台节点打停）。所以目标是软链接时**跳过校准**并记一条警告：
// 端口对不上只是隧道连不上，写穿软链接是节点级事故。
func syncServerPort(dir string, port int, customStart bool) (bool, int, error) {
	if port <= 0 || port > 65535 {
		return false, 0, nil // 端口没配置/不合法：不猜，保持原样
	}

	path := filepath.Join(dir, serverPropsFile)
	f, err := safepath.OpenNoFollow(path, os.O_RDONLY, 0)
	if err != nil {
		switch {
		case safepath.IsSymlinkRefusal(err):
			slog.Warn("server.properties 是软链接，已跳过端口校准", "dir", dir)
			return false, 0, nil
		case os.IsNotExist(err):
			if customStart {
				return false, 0, nil
			}
			// 还没有 server.properties：默认 java 启动的实例一定是 MC 服务端，
			// 先写一行把端口定下来，服务端首次启动会把其余默认项补齐
			// （缺项会取默认值并在启动时写回完整文件）。
			//
			// **悬空**软链接也在上面那一支被 ELOOP 挡住（O_NOFOLLOW 只看最后
			// 一段是不是软链接，与目标存不存在无关），这里仍用 O_NOFOLLOW 是为了
			// 守住"检查之后、创建之前"被换成软链接的那个窗口。
			w, werr := safepath.OpenNoFollow(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
			if werr != nil {
				if safepath.IsSymlinkRefusal(werr) {
					slog.Warn("server.properties 是软链接，已跳过端口校准", "dir", dir)
					return false, 0, nil
				}
				return false, 0, werr
			}
			werr = writeAllClose(w, fmt.Sprintf("%s=%d\n", serverPortKey, port))
			if werr != nil {
				return false, 0, werr
			}
			return true, 0, nil
		default:
			return false, 0, err
		}
	}
	b, err := io.ReadAll(f)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return false, 0, err
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

	w, err := safepath.OpenNoFollow(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		if safepath.IsSymlinkRefusal(err) {
			// 读到写之间被换成了软链接：同样只跳过，不写
			slog.Warn("server.properties 是软链接，已跳过端口校准", "dir", dir)
			return false, old, nil
		}
		return false, old, err
	}
	if err := writeAllClose(w, strings.Join(lines, "\n")); err != nil {
		return false, old, err
	}
	return true, old, nil
}

// writeAllClose 写入内容并关闭文件，任一环节出错都报出来。
// 只 Write 不看 Close 会漏掉"落盘时才发现磁盘满"这类失败。
func writeAllClose(f *os.File, content string) error {
	_, werr := f.Write([]byte(content))
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	return werr
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
