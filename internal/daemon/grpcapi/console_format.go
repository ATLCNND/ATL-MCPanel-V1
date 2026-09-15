package grpcapi

import (
	"fmt"
	"regexp"
	"strings"
)

// 控制台按日志级别整行涂底色。
//
// 为什么用底色而不是改字的颜色：
// 黄色是高明度色，当**文字色**用时，要么在浅色终端上糊成一片（#b45309 只有
// 4.6:1），要么为了可读而加深成琥珀/褐色 —— 结果就是"看着不像黄色"。
// 把黄色放到**底色**上就绕开了这个矛盾：底色不需要和背景"比亮"，
// 它本身就是一块色块，可以一路保持饱和的黄；文字用近黑色，
// 对比度反而比任何黄色文字都高（9.5:1，达到 AAA）。红色同理。
//
// 用 24 位真彩（SGR 38;2 / 48;2）而不是调色板槽位（ESC[30;43m）：
// 调色板槽位会被 xterm 主题里的 black/yellow 定义改写，而 black 槽位
// 必须留给服务器自己输出的黑色文字（映射成可读的浅灰），
// 两者会互相打架 —— 之前 brightYellow 落回内置 #ffff00 就是这个坑。
const (
	// ansiWarnBand 近黑字（#1a1408）+ 饱和黄底（#e6b422），文字对比度 9.51:1
	ansiWarnBand = "\x1b[38;2;26;20;8m\x1b[48;2;230;180;34m"
	// ansiErrorBand 白字（#ffffff）+ 红底（#c22f22），文字对比度 5.64:1
	ansiErrorBand = "\x1b[38;2;255;255;255m\x1b[48;2;194;47;34m"

	// ansiEraseToEOL 用当前底色把该行剩余部分填满 —— 这才是"整行通栏"的关键。
	// 只用底色包住文字的话，底色只覆盖字符串本身（实测占行宽 69%），
	// 看起来像记号笔涂了一道，而不是日志查看器里那种整行高亮。
	ansiEraseToEOL = "\x1b[K"
	ansiSGRReset   = "\x1b[0m"
)

// consoleRule 一种级别的识别规则与配色。
type consoleRule struct {
	level string
	re    *regexp.Regexp
	band  string
}

// 匹配顺序即优先级：错误在前，一行同时命中两者时按更严重的处理。
var consoleRules = []consoleRule{
	{level: "error", re: buildLevelRe(`ERROR|SEVERE|FATAL`), band: ansiErrorBand},
	{level: "warn", re: buildLevelRe(`WARN|WARNING`), band: ansiWarnBand},
}

// buildLevelRe 按级别名构造识别规则。
//
// 覆盖真实存在的四种日志形态（levels 形如 `WARN|WARNING`）：
//
//	[00:49:59 WARN]: 消息                          控制台 stdout（console.log）
//	[00:50:02] [Server thread/WARN]: 消息           Paper / Spigot 带线程名
//	12:34:56 [WARN] 消息                            BungeeCord / Velocity（无冒号）
//	2026-09-10 00:49:56,878 ServerMain WARN 消息     logback（无方括号）
//
// 两条防空措施都来自实测踩坑：
//
//  1. **区分大小写**：这些服务端的日志级别一律大写，而玩家聊天里
//     "<Steve> warning: 别拆我家" 是小写 —— 用 (?i) 会把聊天也涂上底色。
//
//  2. **级别标记必须落在"日志前缀位置"**：行首、时间戳方括号之后，
//     或时间戳方括号紧邻的第二个方括号里。曾经只写了
//     `\[[^\[\]]*WARN[^\[\]]*\]:` 就上线，结果 `<Steve> [WARN]: lol`
//     和 `/say [WARN]: 重启` 这类**消息正文**里的字面量也被涂黄 ——
//     玩家能随手把整行染成警告色，高亮也就没意义了。
//
// 第 2 条里"时间戳方括号"以**数字开头**作为判据（`[00:50:02]`、`[2026-09-11 12:34:56]`），
// 这样 `[Server] [WARN]: 消息` 这种正文不会被当成日志前缀。
func buildLevelRe(levels string) *regexp.Regexp {
	return regexp.MustCompile(fmt.Sprintf(
		`(?:`+
			// A：行首就是级别方括号，且以冒号收尾  →  [00:49:59 WARN]: msg / [WARN]: msg
			`^\[[^\[\]]*\b(?:%[1]s)\b[^\[\]]*\]:`+
			// B：时间戳方括号 + 级别方括号（Paper/Spigot 带线程名）
			`|^\[\d[^\[\]]*\]\s+\[[^\[\]]*\b(?:%[1]s)\b[^\[\]]*\]:`+
			// C：行首（或时间戳之后）的裸级别方括号，无冒号  →  12:34:56 [WARN] msg
			`|^(?:\d{2}:\d{2}:\d{2}\s+)?\[(?:%[1]s)\]`+
			// D：logback：日期 时间 logger 级别
			`|\d{4}-\d{2}-\d{2}[ T]\d{2}:\d{2}:\d{2}[,.]\d+\s+\S+\s+(?:%[1]s)\b`+
			`)`,
		levels))
}

// highlightConsoleLine 给**单行**涂底色（不看上下文）。
//
// 只关心"这一行自己该不该涂"时用它；要跟着异常堆栈的续行一起涂，
// 用 consoleHighlighter（它是有状态的，见下）。
func highlightConsoleLine(line string) string {
	h := &consoleHighlighter{}
	return h.line(line)
}

// ---- 堆栈续行 ----

// stackContinuationMax 一条错误最多带几条续行涂底色。
//
// 为什么不涂到底：一次异常能跟几十行 `\tat`，全涂会把整屏刷成红底，
// 真正该看的那行（异常类型 + 消息）反而被淹掉 —— 高亮就不再是"指引"了。
// 10 行够覆盖"异常头 + 若干层调用 + Caused by"。
const stackContinuationMax = 10

// stackContinuationRe 堆栈续行的形态。
//
// 这些行**自己没有任何级别标记** —— 服务端把整段堆栈当一条消息打印，
// 只有第一行带 `[ERROR]` 前缀（所以它们此前完全没被涂色）。识别只能靠上下文。
//
// 三种真实形态：
//
//	\tat net.minecraft.server.MinecraftServer.run(MinecraftServer.java:1)
//	... 12 more
//	Caused by: java.io.IOException: disk full
//
// 安全性来自"正常日志行不会这么开头"：日志行以 `[` 或时间戳起头，
// 而以空白开头的基本只有堆栈续行（`Caused by:` / `Suppressed:` 是例外，单独列）。
// 再加"只在错误行之后 10 行内生效"这道闸，误伤面很小。
var stackContinuationRe = regexp.MustCompile(
	`^(?:\s+at\s+\S|\s*\.\.\.\s+\d+\s+more\b|Caused by:|Suppressed:)`)

// consoleHighlighter 逐行装饰控制台输出（**有状态**）。
//
// 为什么必须是有状态的：堆栈续行只能靠"紧跟在错误行之后"来识别，
// 单行函数拿不到这个上下文。每条控制台流各持一个实例
//（历史回放与实时输出是两条独立的流，各自从头开始判断）。
type consoleHighlighter struct {
	contLeft int    // 还能给几条续行涂底色；0 = 不在异常块里
	band     string // 续行沿用触发它的那行的底色
}

// line 装饰一行（传入的行保持原样，含末尾换行）。
func (h *consoleHighlighter) line(line string) string {
	if line == "" {
		return line
	}
	// 幂等保护：已经装饰过的行直接返回。
	//
	// 叠加的后果不只是"多几个转义符"——第二个 ESC[K 会在复位之后执行，
	// 用**默认底色**重新填充该行剩余部分，把刚涂好的底色又擦掉。
	// 目前历史回放与实时输出各只装饰一次，但这两条路径将来很容易被合并
	// 或复用，加一道判断比事后排查"底色有时只剩一半"便宜得多。
	if strings.Contains(line, ansiWarnBand) || strings.Contains(line, ansiErrorBand) {
		return line
	}
	// 先摘掉行尾换行：ESC[K 必须在换行**之前**执行，
	// 否则填色会落到下一行去。
	body, eol := splitEOL(line)
	if body == "" {
		h.contLeft = 0 // 空行：异常块到此为止
		return line
	}
	for _, r := range consoleRules {
		if r.re.MatchString(body) {
			// 只有**错误**行会"武装"后续续行：异常堆栈是错误语义，
			// 而警告通常就一行（跟续行容易把无关的 at-开头行也染色）。
			if r.band == ansiErrorBand {
				h.contLeft = stackContinuationMax
				h.band = r.band
			}
			return r.band + body + ansiEraseToEOL + ansiSGRReset + eol
		}
	}
	// 跟着错误行走的堆栈续行
	if h.contLeft > 0 && stackContinuationRe.MatchString(body) {
		h.contLeft--
		return h.band + body + ansiEraseToEOL + ansiSGRReset + eol
	}
	// 别的行出现了 —— 异常块结束（否则"上一次异常"会一直影响后面几十行）
	h.contLeft = 0
	return line
}

// splitEOL 分离行尾的换行符（\n / \r\n / \r）。
func splitEOL(s string) (body, eol string) {
	switch {
	case strings.HasSuffix(s, "\r\n"):
		return s[:len(s)-2], "\r\n"
	case strings.HasSuffix(s, "\n"):
		return s[:len(s)-1], "\n"
	case strings.HasSuffix(s, "\r"):
		return s[:len(s)-1], "\r"
	}
	return s, ""
}
