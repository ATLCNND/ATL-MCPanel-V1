package mcprocess

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 本文件守的是用户 2026-10-17 的要求：
//
//	「把控制台过滤的刷屏信息从控制台保留行数的计数里剔除」
//
// 两层都要：**前端终端**那一层（xterm scrollback）与 **Daemon 回放窗口**那一层
//（consoleHistoryLines = 500）。这里测的是 Daemon 这一层 —— 回放窗口按
// "非刷屏行"计数，被屏蔽的周期性日志不再占额度。
//
// 规则来自前端（预制规则 + 用户自定义正则），所以这里也用"客户端下发的规则"这种形式来测，
// 而不是在 Go 里另立一份判定标准。

const (
	hbPlayers = "[21:00:05 INFO]: There are 0 of a max of 20 players online: "
	hbTPS     = "[21:00:15 INFO]: TPS from last 1m, 5m, 15m: 20.0, 20.0, 20.0"
)

// 前端 HEARTBEAT_PRESETS 里两条默认开启的规则（原样搬过来，source + flags）
func defaultSpamRules() []SpamRule {
	return []SpamRule{
		{ID: "players-online", Pattern: `There are \d+ of a max of \d+ players online`, Flags: "i"},
		{ID: "tps-report", Pattern: `TPS from last 1m, 5m, 15m:`, Flags: "i"},
	}
}

func TestSpamFilterUsefulTailSkipsHeartbeats(t *testing.T) {
	// 造 600 条有用行，每条后面跟两条心跳（心跳占 2/3，模拟长期运行的服务器）
	var lines []string
	for i := 0; i < 600; i++ {
		lines = append(lines,
			fmt.Sprintf("[21:00:%02d INFO]: 玩家 Alice 加入了游戏 #%d", i%60, i),
			hbPlayers,
			hbTPS,
		)
	}
	f, skipped := NewSpamFilter(defaultSpamRules())
	if len(skipped) != 0 {
		t.Fatalf("默认规则不该被跳过，实际跳过 %v", skipped)
	}
	if f == nil {
		t.Fatal("规则编译结果不该是 nil")
	}

	got := f.UsefulTail(lines, 500)
	if len(got) != 500 {
		t.Fatalf("应取满 500 条非刷屏行，实际 %d 条", len(got))
	}
	for _, l := range got {
		if f.IsSpam(l) {
			t.Fatalf("结果里混进了被屏蔽的行：%q", l)
		}
	}
	// 必须是**最后** 500 条有用行（而不是文件开头的）
	if !strings.Contains(got[len(got)-1], "#599") {
		t.Errorf("应取尾部：最后一条应是 #599，实际 %q", got[len(got)-1])
	}
	if !strings.Contains(got[0], "#100") {
		t.Errorf("500 条有用行应从 #100 开始（600-500），实际 %q（取窗口的位置不对）", got[0])
	}
}

func TestSpamFilterBlockRuleSwallowsContinuations(t *testing.T) {
	lines := []string{
		"[21:00:01 INFO]: 普通日志 A",
		"[21:00:02 INFO]: Server Health Report",
		" - Online Players: 0",
		" - Total regions: 8",
		" Chunks: 529",
		"[21:00:03 INFO]: 普通日志 B",
	}
	f, _ := NewSpamFilter([]SpamRule{
		{ID: "health-report", Pattern: `Server Health Report`, Flags: "i", Block: true},
	})
	if f == nil {
		t.Fatal("规则应编译成功")
	}
	got := f.UsefulTail(lines, 10)
	// 期望：A 与 B 保留；块头与三条续行都不在
	joined := strings.Join(got, "\n")
	if !strings.Contains(joined, "普通日志 A") || !strings.Contains(joined, "普通日志 B") {
		t.Errorf("普通日志行被误伤：%v", got)
	}
	for _, bad := range []string{"Server Health Report", "Online Players", "Total regions", "Chunks: 529"} {
		if strings.Contains(joined, bad) {
			t.Errorf("块内容没被跳过：%q 仍在结果里", bad)
		}
	}
}

func TestSpamFilterSkipsUncompilableRules(t *testing.T) {
	// lookahead 是 JS 支持、RE2 不支持的写法 —— 必须**跳过这一条**而不是让整批失败，
	// 并且要让调用方知道跳过了谁（Daemon 会记日志，否则用户以为规则没生效）。
	f, skipped := NewSpamFilter([]SpamRule{
		{ID: "custom-lookahead", Pattern: `foo(?=bar)`, Flags: "i"},
		{ID: "ok-rule", Pattern: `心跳刷屏`, Flags: "i"},
	})
	if f == nil {
		t.Fatal("可编译的那条应当仍然生效")
	}
	if len(skipped) != 1 || skipped[0] != "custom-lookahead" {
		t.Errorf("应报告跳过了 custom-lookahead，实际 %v", skipped)
	}
	if !f.IsSpam("这是一条心跳刷屏") {
		t.Error("可编译的规则应仍然生效")
	}
}

func TestSpamFilterNilKeepsOldBehavior(t *testing.T) {
	// 向后兼容：没有规则时（老前端 / 第三方客户端 / 用户关掉了总开关）
	// 行为必须与改动前**完全一致** —— 老老实实取尾部 N 行，心跳也在里面。
	lines := []string{"a", hbPlayers, "b", hbTPS, "c"}
	var f *SpamFilter
	got := f.UsefulTail(lines, 3)
	if len(got) != 3 || got[0] != "b" || got[1] != hbTPS || got[2] != "c" {
		t.Errorf("无规则时应原样取尾部 3 行，实际 %v", got)
	}
}

func TestGoRegexSourceFlags(t *testing.T) {
	cases := []struct{ pattern, flags, want string }{
		{`abc`, "", `abc`},
		{`abc`, "i", `(?i)abc`},
		{`abc`, "im", `(?im)abc`},
		{`abc`, "gi", `(?i)abc`}, // g 对逐行匹配无意义，忽略
		{`abc`, "ii", `(?i)abc`}, // 去重
		{`abc`, "uy", `abc`},     // u/y 不翻译
	}
	for _, c := range cases {
		if got := goRegexSource(c.pattern, c.flags); got != c.want {
			t.Errorf("goRegexSource(%q,%q) = %q，期望 %q", c.pattern, c.flags, got, c.want)
		}
	}
}

// TestRecentOutputFilteredCountsNonSpam 端到端一点：真的从实例日志文件里读，
// 过滤后**所有返回行都不是刷屏行**，且未过滤的那个入口行为不变。
func TestRecentOutputFilteredCountsNonSpam(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "logs"), 0o755); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(dir, "logs", "console.log")
	var b strings.Builder
	for i := 0; i < 600; i++ {
		fmt.Fprintf(&b, "[21:00:00 INFO]: 有用输出 %d\n", i)
		b.WriteString(hbPlayers + "\n")
		b.WriteString(hbTPS + "\n")
	}
	if err := os.WriteFile(logPath, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	inst := testInstance("spam01", dir, "", "1G", "1G")

	f, _ := NewSpamFilter(defaultSpamRules())
	got := inst.RecentOutputFiltered(500, f)
	if len(got) != 500 {
		t.Fatalf("应取满 500 条非刷屏行，实际 %d", len(got))
	}
	for _, l := range got {
		if f.IsSpam(l) {
			t.Fatalf("过滤后的回放里混进了刷屏行：%q", l)
		}
		if !strings.HasSuffix(l, "\n") {
			t.Fatalf("每行都该带换行（xterm 是按行写的）：%q", l)
		}
	}
	// 未过滤的入口：仍是"最后 500 行"，心跳在里面（向后兼容）
	plain := inst.RecentOutput(500)
	if len(plain) != 500 {
		t.Fatalf("不过滤时应返回 500 行，实际 %d", len(plain))
	}
	spam := 0
	for _, l := range plain {
		if f.IsSpam(l) {
			spam++
		}
	}
	if spam == 0 {
		t.Error("不过滤的那条路应当仍包含刷屏行（否则就是把新行为串到旧入口上了）")
	}
}
