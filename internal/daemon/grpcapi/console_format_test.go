package grpcapi

import (
	"strings"
	"testing"
)

// TestHighlightConsoleLine 覆盖真实日志形态与"不该被涂"的干扰样本。
//
// 这条规则的风险全在**误伤**上：玩家聊天里出现 warn/error 字样被涂上底色，
// 或者普通 INFO 行被涂，都会让高亮失去意义（用户会很快忽略它）。
func TestHighlightConsoleLine(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string // "" = 不该被涂；否则是期望的底色序列
	}{
		// ---- 警告：应当涂黄底 ----
		{"控制台 WARN", "[00:49:59 WARN]: YOU ARE RUNNING THIS SERVER AS ROOT.\n", ansiWarnBand},
		{"Paper 带线程名", "[00:50:02] [Server thread/WARN]: Can't keep up! Is the server overloaded?\n", ansiWarnBand},
		{"纯 [WARN]", "[WARN]: something happened\n", ansiWarnBand},
		{"WARNING 全称", "[00:51:00 WARNING]: disk almost full\n", ansiWarnBand},
		{"Bungee 风格 [WARN]", "12:34:56 [WARN] something happened\n", ansiWarnBand},
		{"logback 形态 WARN", "2026-09-10 00:49:56,878 ServerMain WARN Advanced terminal features are not available\n", ansiWarnBand},

		// ---- 错误：应当涂红底 ----
		{"控制台 ERROR", "[00:50:13 ERROR]: something broke\n", ansiErrorBand},
		{"Paper 带线程名 ERROR", "[00:50:14] [Server thread/ERROR]: Failed to load plugin\n", ansiErrorBand},
		{"vanilla SEVERE", "[00:50:15 SEVERE]: Unable to access world\n", ansiErrorBand},
		{"FATAL", "[00:50:16 FATAL]: cannot continue\n", ansiErrorBand},
		{"Bungee 风格 [ERROR]", "12:34:56 [ERROR] boom\n", ansiErrorBand},
		{"logback 形态 ERROR", "2026-09-11 17:18:14,990 ServerMain ERROR Failed to start\n", ansiErrorBand},

		// ---- 不该涂 ----
		{"普通 INFO 行", "[00:49:55 INFO]: Preparing level \"world\"\n", ""},
		{"玩家聊天里的小写 warning", "[00:50:11 INFO]: <Steve> warning: dont break my house\n", ""},
		{"聊天里的大写 WARN（无双引号）", "[00:50:11 INFO]: <Steve> WARN me please\n", ""},
		{"聊天里带方括号的小写", "[00:50:11 INFO]: <Steve> [warn] look at this\n", ""},
		{"聊天里带方括号的大写", "[00:50:11 INFO]: <Steve> [ERROR] look at this\n", ""},
		{"聊天里 [WARN] 紧跟冒号", "[00:50:11 INFO]: <Steve> [WARN]: lol\n", ""},
		{"聊天里 [ERROR] 紧跟冒号", "[00:50:11 INFO]: <Steve> [ERROR]: lol\n", ""},
		{"say 出来的警告样式文本", "[00:50:11 INFO]: [Server] [WARN]: not really a warning\n", ""},
		{"say 出来的错误样式文本", "[00:50:11 INFO]: [Server] [ERROR]: not really an error\n", ""},
		{"非时间戳方括号 + 级别方括号", "[Server] [WARN]: 正文而非日志前缀\n", ""},
		{"方括号里是别的词", "[00:50:12 INFO]: [Server thread/INFO]: hello\n", ""},
		{"空行", "\n", ""},
		{"空字符串", "", ""},
		{"只有换行符", "\r\n", ""},
		{"时间戳但 logger 不是错误级别", "2026-09-10 00:49:56,878 ServerMain INFO all good\n", ""},
		{"WARN 只是单词的一部分", "[00:50:14 INFO]: WARNINGLY speaking\n", ""},
		{"logback 形态缺 logger 名", "2026-09-10 00:49:56,878 WARN hello\n", ""},

		// ---- 边界 ----
		{"无换行的最后一行（WARN）", "[00:52:00 WARN]: no trailing newline", ansiWarnBand},
		{"无换行的最后一行（ERROR）", "[00:52:01 ERROR]: no trailing newline", ansiErrorBand},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := highlightConsoleLine(c.in)
			switch {
			case c.want == "":
				if got != c.in {
					t.Errorf("不该被涂，却被改动：\n输入: %q\n输出: %q", c.in, got)
				}
			default:
				if !strings.HasPrefix(got, c.want) {
					t.Fatalf("底色不对：期望以 %q 开头\n输入: %q\n输出: %q", c.want, c.in, got)
				}
				// 结构要求：ESC[K 在换行之前，随后复位，正文完整保留
				kIdx := strings.Index(got, ansiEraseToEOL)
				eolIdx := strings.IndexAny(got, "\r\n")
				if kIdx < 0 {
					t.Fatalf("缺少 ESC[K（无法整行通栏）：%q", got)
				}
				if eolIdx >= 0 && kIdx > eolIdx {
					t.Errorf("ESC[K 必须在换行之前：%q", got)
				}
				if !strings.Contains(got, ansiEraseToEOL+ansiSGRReset) {
					t.Errorf("ESC[K 之后缺少 SGR 复位：%q", got)
				}
				body, _ := splitEOL(strings.TrimPrefix(got, c.want))
				body = strings.TrimSuffix(body, ansiEraseToEOL+ansiSGRReset)
				wantBody, _ := splitEOL(c.in)
				if body != wantBody {
					t.Errorf("正文被改动：\n期望: %q\n实际: %q", wantBody, body)
				}
				hadEOL := strings.HasSuffix(c.in, "\n") || strings.HasSuffix(c.in, "\r")
				gotEOL := strings.HasSuffix(got, "\n") || strings.HasSuffix(got, "\r")
				if hadEOL != gotEOL {
					t.Errorf("行尾换行被改动：输入 %q → 输出 %q", c.in, got)
				}
			}
		})
	}
}

// TestErrorWinsOverWarn 一行同时命中错误与警告时，取更严重的（红底）。
func TestErrorWinsOverWarn(t *testing.T) {
	// 构造一个同时含两段级别标记的行（现实中少见，但规则顺序必须明确）
	in := "[00:50:20 ERROR]: [WARN]: both markers present\n"
	got := highlightConsoleLine(in)
	if !strings.HasPrefix(got, ansiErrorBand) {
		t.Errorf("应优先按错误级别涂红底，实际: %q", got)
	}
}

// TestHighlightConsoleLinePreservesCRLF 保留 \r\n，避免 Windows 风格日志被吃掉 \r。
func TestHighlightConsoleLinePreservesCRLF(t *testing.T) {
	in := "[00:49:59 WARN]: crlf line\r\n"
	got := highlightConsoleLine(in)
	if !strings.HasSuffix(got, "\r\n") {
		t.Errorf("CRLF 未被保留: %q", got)
	}
	kIdx := strings.Index(got, ansiEraseToEOL)
	crIdx := strings.Index(got, "\r\n")
	if kIdx < 0 || crIdx < 0 || kIdx > crIdx {
		t.Errorf("ESC[K 应在 \\r\\n 之前: %q", got)
	}
}

// TestHighlightConsoleLineDoesNotStack 确认重复调用不会叠加转义序列。
//
// 历史回放与实时输出走的是同一个函数；如果某条路径被调用两次，
// 叠加的底色序列会让复位时机错乱 —— 第二个 ESC[K 会在复位之后
// 用默认底色重新填充，把刚涂好的底色擦掉。
func TestHighlightConsoleLineDoesNotStack(t *testing.T) {
	for _, in := range []string{
		"[00:49:59 WARN]: once only\n",
		"[00:50:00 ERROR]: once only\n",
	} {
		once := highlightConsoleLine(in)
		twice := highlightConsoleLine(once)
		if once != twice {
			t.Errorf("重复调用产生了变化：\n一次: %q\n两次: %q", once, twice)
		}
	}
}

func TestSplitEOL(t *testing.T) {
	cases := map[string][2]string{
		"a\n":   {"a", "\n"},
		"a\r\n": {"a", "\r\n"},
		"a\r":   {"a", "\r"},
		"a":     {"a", ""},
		"\n":    {"", "\n"},
	}
	for in, want := range cases {
		body, eol := splitEOL(in)
		if body != want[0] || eol != want[1] {
			t.Errorf("splitEOL(%q) = (%q,%q)，期望 (%q,%q)", in, body, eol, want[0], want[1])
		}
	}
}

// ---- 堆栈续行（第十节第 4 项） ----

// isBand 判断某行被涂了指定底色。
func isBand(line, band string) bool { return strings.HasPrefix(line, band) }

// 错误行之后的堆栈续行要跟着涂红底。
//
// 这些续行**自己没有任何级别标记**（服务端把整段堆栈当一条消息打印，
// 只有第一行带 [ERROR]），所以此前是完全没被涂色的 —— 而它们恰恰是
// "到底哪一行出的错"的关键信息。
func TestStackContinuationIsHighlighted(t *testing.T) {
	hl := &consoleHighlighter{}
	lines := []string{
		"[12:00:00 ERROR]: java.lang.RuntimeException: boom\n",
		"\tat net.minecraft.Foo.bar(Foo.java:42)\n",
		"\tat net.minecraft.Baz.qux(Baz.java:7)\n",
		"Caused by: java.io.IOException: disk full\n",
		"... 12 more\n",
	}
	for i, in := range lines {
		got := hl.line(in)
		if !isBand(got, ansiErrorBand) {
			t.Errorf("第 %d 行应涂红底，实际: %q", i, got)
		}
		// 正文不能被改动（先摘换行，再剥前缀底色与尾部 ESC[K/复位 —— 顺序反了就剥不干净）
		body, _ := splitEOL(got)
		body = strings.TrimPrefix(body, ansiErrorBand)
		body = strings.TrimSuffix(body, ansiEraseToEOL+ansiSGRReset)
		wantBody, _ := splitEOL(in)
		if body != wantBody {
			t.Errorf("第 %d 行正文被改动：%q → %q", i, wantBody, body)
		}
	}
}

// 续行**只跟 N 行**：一次异常几十行 \tat，全涂会把整屏刷红、把头一行淹掉。
func TestStackContinuationIsCapped(t *testing.T) {
	hl := &consoleHighlighter{}
	hl.line("[12:00:00 ERROR]: boom\n")

	// 前 stackContinuationMax 行跟随
	for i := 0; i < stackContinuationMax; i++ {
		if got := hl.line("\tat A.b(A.java:1)\n"); !isBand(got, ansiErrorBand) {
			t.Fatalf("第 %d 条续行应涂色", i+1)
		}
	}
	// 超出后不再涂
	if got := hl.line("\tat A.b(A.java:1)\n"); got != "\tat A.b(A.java:1)\n" {
		t.Errorf("超过上限的续行不该再涂，实际: %q", got)
	}
}

// 中间插了别的行 → 异常块结束，后面的 at-行不再跟着涂。
//
// 少了这条重置，一次异常会一直影响后面几十行 —— 那比不涂更糟。
func TestStackContinuationEndsAtOtherLine(t *testing.T) {
	hl := &consoleHighlighter{}
	hl.line("[12:00:00 ERROR]: boom\n")
	hl.line("\tat A.b(A.java:1)\n")
	if got := hl.line("[12:00:01 INFO]: server started\n"); got != "[12:00:01 INFO]: server started\n" {
		t.Fatalf("INFO 行不该被涂: %q", got)
	}
	if got := hl.line("\tat C.d(C.java:2)\n"); got != "\tat C.d(C.java:2)\n" {
		t.Errorf("异常块已结束，后续 at-行不该被涂: %q", got)
	}
}

// **警告不武装续行**：异常堆栈是错误语义，警告通常只有一行 ——
// 若警告也跟续行，像 "<Steve> at home" 这类以空白/at 起始的正文会被误染。
func TestWarnDoesNotArmContinuation(t *testing.T) {
	hl := &consoleHighlighter{}
	if got := hl.line("[12:00:00 WARN]: Can't keep up!\n"); !isBand(got, ansiWarnBand) {
		t.Fatalf("警告行应涂黄底: %q", got)
	}
	if got := hl.line("\tat A.b(A.java:1)\n"); got != "\tat A.b(A.java:1)\n" {
		t.Errorf("警告之后的 at-行不该被涂: %q", got)
	}
}

// 普通行（不以空白开头、也不是 Caused by）永远不当作续行 —— 这是这套规则的
// 安全边界：日志行以 [ 或时间戳起头，不会命中。
func TestPlainLinesNeverTreatedAsContinuation(t *testing.T) {
	hl := &consoleHighlighter{}
	hl.line("[12:00:00 ERROR]: boom\n")
	for _, in := range []string{
		"at home with Steve\n",            // 没有前导空白
		"Caused by something else\n",      // 不是 "Caused by:"
		"[12:00:01 INFO]: at the beach\n", // 正常日志行
		"\t\tindented info line\n",        // 前导空白但不是 at/...
	} {
		if got := hl.line(in); got != in {
			t.Errorf("不该被当作续行却被涂：%q → %q", in, got)
		}
	}
}
