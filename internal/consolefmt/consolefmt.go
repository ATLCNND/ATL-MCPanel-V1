// Package consolefmt 定义控制台行的**级别**与对应的 ANSI 配色。
//
// 为什么要独立成包：这层信息有两个消费者，分属两个进程 ——
//   - Daemon（internal/daemon/grpcapi）：识别级别并给整行涂底色；
//   - Panel（internal/panel/httpapi）：把控制台流转发给浏览器时，
//     要告诉前端每一行是什么级别，前端才能做"只看警告/错误"的筛选。
//
// protobuf 的 ConsoleFrame 里没有"级别"这个字段，而它目前只有
// type/instance_id/data/is_stdout/timestamp —— 加字段要改 proto 并重新生成
// （需要 protoc 工具链），代价远大于收益。所以级别走**带内**传递：
// 装饰用的底色本身就是级别的标记，Panel 认这几个常量即可。
//
// 这样做的关键是**只有一份定义**：Daemon 涂色用的是这里的常量，
// Panel 认的也是这里的常量，两边不可能写歪。若哪天改了配色，
// 两边会同时生效；如果是各自抄一份常量，改一边就会出现
// "筛选按钮点了没反应"这种极难查的问题。
package consolefmt

import "strings"

// 日志级别（同时也是传给前端的取值）。
const (
	LevelInfo  = "info"
	LevelWarn  = "warn"
	LevelError = "error"
)

// 服务端日志级别的整行底色。
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
	// AnsiWarnBand 近黑字（#1a1408）+ 饱和黄底（#e6b422），文字对比度 9.51:1
	AnsiWarnBand = "\x1b[38;2;26;20;8m\x1b[48;2;230;180;34m"
	// AnsiErrorBand 白字（#ffffff）+ 红底（#c22f22），文字对比度 5.64:1
	AnsiErrorBand = "\x1b[38;2;255;255;255m\x1b[48;2;194;47;34m"

	// AnsiEraseToEOL 用当前底色把该行剩余部分填满 —— 这才是"整行通栏"的关键。
	// 只用底色包住文字的话，底色只覆盖字符串本身（实测占行宽 69%），
	// 看起来像记号笔涂了一道，而不是日志查看器里那种整行高亮。
	AnsiEraseToEOL = "\x1b[K"
	// AnsiSGRReset 复位。
	AnsiSGRReset = "\x1b[0m"
)

// 系统提示用的前景色（不是服务端日志级别的配色，而是 Daemon / Panel
// 自己发的那几条提示）：它们同样需要在筛选里被算作警告/错误。
const (
	// AnsiNoticeWarn 亮黄前景：接管状态提示、控制台丢帧提示等。
	AnsiNoticeWarn = "\x1b[93m"
	// AnsiNoticeError 亮红前景：命令未执行、连接错误等。
	AnsiNoticeError = "\x1b[91m"
)

// LevelOfDecorated 判断一行控制台输出（**已装饰**，即 Daemon 已经涂过底色
// 或加过提示色的那种）属于哪个级别。
//
// 判据是"行首（跳过若干样式序列之后）出现的是哪一种配色"：Daemon 的装饰一律
// 把配色放在正文之前（见 consoleHighlighter.line），所以配色序列本身就等价于级别。
//
// 为什么要**跳过前导的样式序列**再判：装饰不总是从第一个字节开始 ——
// "先复位再上色"（`\x1b[0m` + 底色）这种写法出现过，而它同样是一行警告。
// 只认"第 0 个字节"的判定会在这种情况下把警告当成普通信息，
// 表现为"筛选警告时漏了几行"，且极难复现。
//
// 一旦判据不成立（比如正文自己以同样的序列开头，服务端几乎不可能这么做），
// 最坏结果也只是这一行的筛选归类不准，不影响正文显示 —— 这个失败模式是可接受的。
func LevelOfDecorated(line string) string {
	rest := strings.TrimLeft(line, "\r\n")
	for i := 0; i < 8; i++ {
		switch {
		case strings.HasPrefix(rest, AnsiErrorBand), strings.HasPrefix(rest, AnsiNoticeError):
			return LevelError
		case strings.HasPrefix(rest, AnsiWarnBand), strings.HasPrefix(rest, AnsiNoticeWarn):
			return LevelWarn
		}
		next, ok := skipLeadingSGR(rest)
		if !ok {
			return LevelInfo
		}
		rest = next
	}
	return LevelInfo
}

// skipLeadingSGR 跳过一个行首的 SGR（样式/颜色）序列，返回剩余部分。
//
// 解析是**严格**的：\x1b[ 之后只允许数字与分号，直到 m。不能简单地找第一个 'm'
// —— `\x1b[2Ja message` 这种"擦除后接正文"的行里就有一个 m，
// 那样会把正文当成转义序列的一部分吃掉，然后拿残句去比对，属于极难排查的误判。
func skipLeadingSGR(s string) (string, bool) {
	if !strings.HasPrefix(s, "\x1b[") {
		return s, false
	}
	j := 2
	for j < len(s) && (s[j] == ';' || (s[j] >= '0' && s[j] <= '9')) {
		j++
	}
	if j >= len(s) || s[j] != 'm' {
		return s, false
	}
	return s[j+1:], true
}
