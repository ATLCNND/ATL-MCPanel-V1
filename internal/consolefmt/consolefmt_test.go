package consolefmt

import (
	"strings"
	"testing"
)

// 级别判定：这是「实时日志筛选」的地基。
//
// Panel 转发时靠行首配色反推级别，前端才能做"只看警告/错误"。
// 这里把四种真实形态都钉住，改动配色或判定顺序时能立刻发现。
func TestLevelOfDecorated(t *testing.T) {
	plain := "[12:00:00] [Server thread/INFO]: Done (3.2s)!"
	warnDeco := AnsiWarnBand + "[12:00:01] [Server thread/WARN]: Can't keep up!" + AnsiEraseToEOL + AnsiSGRReset + "\n"
	errDeco := AnsiErrorBand + "[12:00:02] [Server thread/ERROR]: Failed" + AnsiEraseToEOL + AnsiSGRReset + "\n"
	// Daemon 自己发的系统提示（不是服务端日志级别配色）
	noticeWarn := AnsiNoticeWarn + "[控制台丢帧] 上面有 3 行未推送" + AnsiSGRReset + "\n"
	noticeErr := AnsiNoticeError + "[命令未执行] 实例未运行" + AnsiSGRReset + "\n"

	cases := []struct {
		name string
		in   string
		want string
	}{
		{"普通输出", plain, LevelInfo},
		{"警告底色", warnDeco, LevelWarn},
		{"错误底色", errDeco, LevelError},
		{"系统警告提示", noticeWarn, LevelWarn},
		{"系统错误提示", noticeErr, LevelError},
		{"空行", "", LevelInfo},
		{"纯换行", "\n", LevelInfo},
		// 行首被别的转义序列包住时也要认得出（Daemon 有时先复位再上色）
		{"前置复位后的警告底色", AnsiSGRReset + warnDeco, LevelWarn},
		{"前置复位后的错误提示", AnsiSGRReset + noticeErr, LevelError},
		// 正文里出现配色序列不算（只有行首才算）
		{"正文含警告底色", "hello " + AnsiWarnBand + " world\n", LevelInfo},
	}
	for _, c := range cases {
		if got := LevelOfDecorated(c.in); got != c.want {
			t.Errorf("%s：应为 %s，实际 %s（输入 %q）", c.name, c.want, got, c.in)
		}
	}
}

// 严格解析 SGR：`\x1b[2Ja message` 里有个 'm'，但它不是 SGR 的结束符。
// 若实现是"找第一个 m"，这里会把正文吃掉再拿残句比对 —— 真出现时会极难排查。
func TestSkipLeadingSGRStrict(t *testing.T) {
	// 擦除序列（CSI 2J）不是 SGR，不该被跳过
	if _, ok := skipLeadingSGR("\x1b[2J" + AnsiNoticeWarn); ok {
		t.Error("\x1b[2J 不是 SGR，不应被跳过")
	}
	// 正常 SGR 应能被跳过
	if rest, ok := skipLeadingSGR(AnsiSGRReset + "abc"); !ok || rest != "abc" {
		t.Errorf("应跳过 \\x1b[0m，实际 ok=%v rest=%q", ok, rest)
	}
	if rest, ok := skipLeadingSGR("\x1b[38;2;1;2;3mhi"); !ok || rest != "hi" {
		t.Errorf("应跳过 24 位真彩 SGR，实际 ok=%v rest=%q", ok, rest)
	}
	// 非转义开头直接失败
	if _, ok := skipLeadingSGR("plain"); ok {
		t.Error("普通文本不该被当成 SGR")
	}
	// 未闭合（\x1b[ 后面没有 m）也不该硬吃
	if _, ok := skipLeadingSGR("\x1b[38;2;1"); ok {
		t.Error("未闭合的 CSI 不该被跳过")
	}
}

// 配色常量必须是**24 位真彩**而不是调色板槽位。
//
// 调色板槽位会被 xterm 主题里的 black/yellow 定义改写，而 black 槽位
// 必须留给服务器自己输出的黑色文字（映射成可读的浅灰）—— 两者会互相打架。
func TestBandConstsAreTrueColor(t *testing.T) {
	for name, s := range map[string]string{"warn": AnsiWarnBand, "error": AnsiErrorBand} {
		if len(s) == 0 || s[0] != 0x1b {
			t.Errorf("%s 底色应以 ESC 开头", name)
		}
		if !strings.Contains(s, "38;2;") || !strings.Contains(s, "48;2;") {
			t.Errorf("%s 底色应使用 24 位真彩（38;2 前景 / 48;2 背景），实际 %q", name, s)
		}
	}
	if AnsiWarnBand == AnsiErrorBand {
		t.Error("警告与错误的底色不能相同")
	}
}
