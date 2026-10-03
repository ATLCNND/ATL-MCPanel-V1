package mcprocess

import "testing"

// 内存上限字面量的换算必须与 cgroup/内核一致。
//
// 这条以前是错的，而且**错在会静默丢掉用户设的上限**（2026-10-02 做配额修改
// 接口时发现）：
//
//  1. 不带单位一律按 M 算 —— `"1073741824"` 被当成 1 PB，而内核/docker 按字节。
//     用户以为自己填了 1G，实际限制等于不存在。
//  2. `"512MB"` 解析失败后退化成 0 = **不限制** —— 老的 switch 先判 M 后缀再判 B，
//     于是 `512MB` 走 M 分支留下 `512B`，`ParseFloat` 失败返回 0。
//     也就是说用户设的上限被无声丢掉：最典型的那种"改了却没变"。
//
// 它落在哪里：mem_limit 会被 Daemon 以 root 写进 memory.max（或容器的 --memory），
// 所以这里的每一个数字都直接对应内核看到的上限。
func TestParseMemBytesMatchesCgroupSyntax(t *testing.T) {
	cases := []struct {
		in   string
		want int64
		why  string
	}{
		{"512M", 512 << 20, "带 M 后缀"},
		{"512MB", 512 << 20, "结尾的 B 只表示单位结束（旧实现会返回 0 = 不限制）"},
		{"512m", 512 << 20, "大小写不敏感"},
		{"4G", 4 << 30, "带 G 后缀"},
		{"4GB", 4 << 30, "GB 与 G 等价"},
		{"2T", 2 << 40, "带 T 后缀"},
		{"1024K", 1024 << 10, "带 K 后缀"},
		{"1073741824", 1 << 30, "**不带单位 = 字节**（旧实现按 MB 算，差 1M 倍）"},
		{"536870912", 512 << 20, "同上：纯字节数写法"},
		{"512B", 512, "B 表示字节"},
		{"1.5G", 3 << 29, "小数（cgroup 不认，但这里历史上支持，保持兼容）"},
		{"", 0, "空 = 不限制"},
		{"  4G  ", 4 << 30, "两端空白容忍"},
		{"abc", 0, "无法解析 = 不限制（但会记一条 WARN 日志，不再完全静默）"},
		{"-4G", 0, "负数非法 = 不限制"},
		{"0", 0, "0 表示不限制"},
	}
	for _, c := range cases {
		if got := ParseMemBytes(c.in); got != c.want {
			t.Errorf("ParseMemBytes(%q) = %d，期望 %d（%s）", c.in, got, c.want, c.why)
		}
	}
}

// 「带 B 后缀」这一族必须与不带 B 的完全等价 —— 它们是同一个数量的两种写法，
// 面板侧的校验正则两种都放行，节点侧就不能解读成两个不同的值。
func TestParseMemBytesSuffixBIsEquivalent(t *testing.T) {
	pairs := [][2]string{{"512M", "512MB"}, {"4G", "4GB"}, {"2T", "2TB"}, {"1024K", "1024KB"}}
	for _, p := range pairs {
		a, b := ParseMemBytes(p[0]), ParseMemBytes(p[1])
		if a != b {
			t.Errorf("%s 与 %s 应等价，实际 %d vs %d", p[0], p[1], a, b)
		}
	}
}
