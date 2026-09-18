package grpcapi

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// 备份名按**字符**截断，不能把中文切成半个字符。
//
// 老实现是 `if len(s) > 40 { s = s[:40] }` —— Go 的 len 是字节数，
// 中文一个字 3 字节，于是 14 个字的备份名会在第 40 字节处被切断，
// 留下非法 UTF-8 序列：文件名与界面里显示成乱码，而且长度检查看起来"没问题"。
func TestSanitizeNameRuneSafe(t *testing.T) {
	// 20 个中文 = 60 字节 > 40，但只有 20 个字符 < 40 → 不该被截断
	cn := strings.Repeat("开荒", 10)
	got := sanitizeName(cn)
	if got != cn {
		t.Errorf("40 字符以内的中文名不该被截断：\n  输入 %q\n  输出 %q", cn, got)
	}
	if !utf8.ValidString(got) {
		t.Error("输出必须是合法 UTF-8")
	}

	// 60 个中文 → 截到 40 个字符，且仍是合法 UTF-8（末字完整）
	long := strings.Repeat("测", 60)
	out := sanitizeName(long)
	if utf8.RuneCountInString(out) != maxBackupNameRunes {
		t.Errorf("应截断到 %d 个字符，实际 %d", maxBackupNameRunes, utf8.RuneCountInString(out))
	}
	if !utf8.ValidString(out) {
		t.Errorf("截断后必须是合法 UTF-8（不能把字符切开），实际字节 %q", out)
	}

	// 非法字符仍要折成下划线；中文与常见符号保留
	if got := sanitizeName("a/b:c*d?e"); strings.ContainsAny(got, `/:*?`) {
		t.Errorf("路径分隔符等危险字符应被替换，实际 %q", got)
	}
	if got := sanitizeName("  开荒前  "); got != "开荒前" {
		t.Errorf("首尾空白应去掉，实际 %q", got)
	}
	// 空名返回空（由调用方决定默认值）
	if got := sanitizeName("   "); got != "" {
		t.Errorf("全空白应返回空串，实际 %q", got)
	}
	// 引号/星号等也是危险的（会进 shell 或让文件名怪异）
	if got := sanitizeName(`a"b'c`); strings.ContainsAny(got, `"'`) {
		t.Errorf("引号应被替换，实际 %q", got)
	}
}
