// Package nodeinstall 通过 SSH 在远程节点上部署 / 管理 Daemon。
package nodeinstall

import (
	"fmt"
	"strings"
)

// 二进制架构探测：直接读 ELF 头的 e_machine 字段。
//
// 为什么不用 `debug/elf`：`elf.NewFile` 会校验整个 ELF 结构（节头表等），
// 遇到裁剪过的或结构不完整的 ELF 会直接报错返回 —— 而我们只需要第 18 字节起的
// 那两个字节。手写解析反而更稳、也更少依赖。
//
// 为什么必须有这个校验（2026-09-17 发现）：
// 一键部署下发的二进制是**面板自己那份**（`<面板目录>/dsh-daemon`），
// 它的架构在编译时就固定了。而节点可能是 arm64。代码里 `Probe` 明明探测了
// 远端架构，`Deploy` 却从来不比对 —— 于是 arm64 节点会被装上一个 amd64 二进制，
// 直到 `systemctl start` 才报 `Exec format error`，
// 而那个错误离真正的原因（架构不匹配）隔了好几层，排查很费劲。
// 现在 V1 会同时发布 amd64 与 arm64 两个包，混装是真实存在的风险。
//
// 返回归一化后的架构名（与 `uname -m` 对齐），识别不出时返回空串（不阻断部署）。
func binaryArch(bin []byte) string {
	// ELF 头至少 20 字节才够读到 e_machine
	if len(bin) < 20 {
		return ""
	}
	if !(bin[0] == 0x7f && bin[1] == 'E' && bin[2] == 'L' && bin[3] == 'F') {
		return ""
	}
	var machine uint16
	switch bin[5] { // EI_DATA
	case 1: // 小端
		machine = uint16(bin[18]) | uint16(bin[19])<<8
	case 2: // 大端
		machine = uint16(bin[18])<<8 | uint16(bin[19])
	default:
		return ""
	}
	switch machine {
	case 62: // EM_X86_64
		return "x86_64"
	case 183: // EM_AARCH64
		return "aarch64"
	case 40: // EM_ARM
		return "armv7l"
	case 3: // EM_386
		return "i686"
	default:
		return ""
	}
}

// normalizeUnameArch 把 `uname -m` 的输出归一化，便于与 ELF 架构比较。
func normalizeUnameArch(m string) string {
	s := strings.ToLower(strings.TrimSpace(m))
	switch s {
	case "x86_64", "amd64":
		return "x86_64"
	case "aarch64", "arm64":
		return "aarch64"
	case "armv7l", "armv6l", "arm":
		return "armv7l"
	case "i386", "i686", "x86":
		return "i686"
	default:
		return s
	}
}

// checkArch 比对面板侧二进制与节点架构，不匹配就返回可操作的错误。
func (c *Client) checkArch(bin []byte) error {
	want := binaryArch(bin)
	if want == "" {
		return nil // 识别不出来就不阻断（例如未来的新架构）
	}
	out, err := c.Run("uname -m")
	if err != nil {
		return nil // 探测失败不阻断，交给后面的步骤报错
	}
	got := normalizeUnameArch(out)
	if got == "" || got == want {
		return nil
	}
	return fmt.Errorf(
		"架构不匹配：节点是 %s，而面板要下发的 dsh-daemon 是 %s。\n"+
			"一键部署只能下发**面板自己同目录那一份**二进制，所以面板所在架构必须与节点一致。\n"+
			"解决办法：在节点上手动安装对应架构的发布包（linux-%s），或用与节点同架构的面板。",
		got, want, archToGoArch(got))
}

// archToGoArch 把 uname 架构名映射成发布包里的 GOARCH 写法，便于给出可直接照做的提示。
func archToGoArch(uname string) string {
	switch uname {
	case "x86_64":
		return "amd64"
	case "aarch64":
		return "arm64"
	case "armv7l":
		return "arm"
	default:
		return uname
	}
}
