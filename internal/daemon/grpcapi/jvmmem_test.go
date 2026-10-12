package grpcapi

import "testing"

// ============================================================================
// JVM 堆取值校验（SetInstanceJvmMem 的入参）
//
// 为什么值得单独测：这两个值最终会被**原样拼进启动命令**
//（见 mcprocess 的 `-Xmx{max_mem}` 模板替换 / 默认模板
// `{java} -Xms{min_mem} -Xmx{max_mem} -jar {jar} nogui`）。
// 一个带空格、分号或 `$()` 的取值等于往命令里注入内容；
// 而 0 / 负数会让 JVM 直接拒绝启动 —— 错误现场在节点上，
// 用户根本联想不到是面板上的这个字段。
// ============================================================================

func TestValidJvmMemAcceptsRealForms(t *testing.T) {
	for _, v := range []string{"4G", "4g", "1536m", "512M", "1024k", "8T", "2048"} {
		if !validJvmMem(v) {
			t.Errorf("%q 应被接受", v)
		}
	}
}

func TestValidJvmMemRejectsInjectionAndBadValues(t *testing.T) {
	bad := []struct {
		v, why string
	}{
		{"", "空串不是合法堆值（调用方各自处理空串语义）"},
		{"   ", "只有空格"},
		{"0", "-Xmx0 会让 JVM 拒绝启动"},
		{"0G", "同上，带单位也是 0"},
		{"-4G", "负数是硬错误"},
		{"1.5G", "JVM 参数不认小数点"},
		{"512MB", "结尾的 B 是内核写法，JVM 不认"},
		{"1GiB", "不认 i 后缀"},
		{"4 G", "带空格：会变成两个参数"},
		{"4G 8G", "两个 token"},
		{"4G;rm -rf /", "命令注入"},
		{"4G$(id)", "命令替换注入"},
		{"4G`id`", "反引号注入"},
		{"4G\n1G", "换行"},
		{"../../etc/passwd", "路径穿越"},
		{"99999999999999999999999", "超出 int64"},
		{"4", "纯数字（会被当成 4 字节，虽合法但无意义 —— 仍接受，见下面单独断言）"},
	}
	for _, c := range bad {
		got := validJvmMem(c.v)
		// 最后一条是"接受"的边界说明，单独处理
		if c.v == "4" {
			if !got {
				t.Errorf("%q：纯数字在格式上是合法的（4 字节），本函数应接受", c.v)
			}
			continue
		}
		if got {
			t.Errorf("%q 应被拒绝（%s）", c.v, c.why)
		}
	}
}

func TestValidJvmMemRejectsTooLong(t *testing.T) {
	// 长度上限是防御性的：正常的堆取值不会超过十来个字符。
	long := "1"
	for i := 0; i < 32; i++ {
		long += "0"
	}
	if validJvmMem(long + "G") {
		t.Error("超长取值应被拒绝")
	}
}

func TestMemBytesParsesUnits(t *testing.T) {
	cases := map[string]int64{
		"1k": 1 << 10,
		"1K": 1 << 10,
		"1m": 1 << 20,
		"1M": 1 << 20,
		"1g": 1 << 30,
		"1G": 1 << 30,
		"1t": 1 << 40,
		"1T": 1 << 40,
		"4":  4,
	}
	for in, want := range cases {
		if got := memBytes(in); got != want {
			t.Errorf("memBytes(%q) = %d，应为 %d", in, got, want)
		}
	}
	// 0 与非法值都返回 0（调用方据此判断"不合法"）
	for _, in := range []string{"0", "0G", "abc", "-1G"} {
		if got := memBytes(in); got != 0 {
			t.Errorf("memBytes(%q) 应为 0，实际 %d", in, got)
		}
	}
}

func TestMemNoteOnlyShowsWhenSet(t *testing.T) {
	if got := memNote(""); got != "" {
		t.Errorf("min_mem 为空时不该产生后缀，实际 %q", got)
	}
	if got := memNote("1G"); got != " -Xms1G" {
		t.Errorf("memNote(\"1G\") = %q，应为 \" -Xms1G\"", got)
	}
}
