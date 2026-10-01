package httpapi

import (
	"strings"
	"testing"
)

// mclo.gs 的截断必须**保留尾部**：崩溃现场在日志后面，
// 只留前 N 行会把最该看的那几行丢掉（这正是 mclo.gs/LogShare 两条链路都要守的规则）。
//
// 这条也守"上限取值"：对方的实测上限是 10MiB / 25000 行，比面板默认的 16MiB 小，
// 少了本地截断，稍大的日志会被对方一句 "Content is too long" 拒掉。
func TestCapForMclogs(t *testing.T) {
	t.Run("不超限时原样返回", func(t *testing.T) {
		in := "line1\nline2\nline3"
		got, cut := capForMclogs(in, 1<<20, 100)
		if got != in || cut != 0 {
			t.Errorf("不该改动内容：cut=%d got=%q", cut, got)
		}
	})

	t.Run("超字节上限：保留尾部并注明", func(t *testing.T) {
		// 造一个明显超限的内容，尾部放一个标记行
		head := strings.Repeat("old-line\n", 2000)
		in := head + "TAIL-MARKER\n"
		got, cut := capForMclogs(in, 1024, 100000)
		if cut <= 0 {
			t.Fatalf("应报告截断字节数，实际 cut=%d", cut)
		}
		if !strings.Contains(got, "TAIL-MARKER") {
			t.Error("尾部内容被丢掉了 —— 崩溃现场就在尾部")
		}
		if !strings.Contains(got, "省略前部") {
			t.Error("应在文件头注明省略了多少（用户要能看出分析的不是完整日志）")
		}
		if len(got) > 2048 {
			t.Errorf("截断后仍过大：%d 字节", len(got))
		}
		if len(got) >= len(in) {
			t.Error("截断后内容不该比原内容还长")
		}
	})

	t.Run("超行数上限：同样保留尾部", func(t *testing.T) {
		var b strings.Builder
		for i := 0; i < 300; i++ {
			b.WriteString("filler\n")
		}
		b.WriteString("LAST-LINE\n")
		got, cut := capForMclogs(b.String(), 1<<20, 50)
		if cut <= 0 {
			t.Fatalf("应报告截断，实际 cut=%d", cut)
		}
		if !strings.Contains(got, "LAST-LINE") {
			t.Error("尾部行被丢掉了")
		}
		if !strings.Contains(got, "已省略前部") {
			t.Error("应注明省略了多少行")
		}
		if n := strings.Count(got, "\n"); n > 52 {
			t.Errorf("截断后行数仍过多：%d（上限 50 + 说明行）", n)
		}
	})

	t.Run("上限为 0 或负数时不 panic", func(t *testing.T) {
		// 真实调用不会传 0，但边界输入不该把一个"旁路功能"变成崩溃点
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("capForMclogs 在极端参数下 panic：%v", r)
			}
		}()
		_, _ = capForMclogs("abc", 0, 0)
	})
}
