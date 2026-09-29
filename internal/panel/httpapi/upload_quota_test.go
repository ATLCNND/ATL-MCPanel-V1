package httpapi

import "testing"

// 上传的**写入前**配额预检。
//
// 背景（这轮发现的缺口）：磁盘限额目前只由调度器事后强制 —— 超限就把实例停机。
// 对上传来说这个兜底太晚：单文件 256MB、请求不限次数，租户能在调度器下一轮检查前
// 把节点磁盘写满，而受害的是同节点的其他租户。
// 所以在写之前先算一次账，并把数字说清楚，而不是"传完了、实例被停了、不知道为什么"。
func TestQuotaExceeded(t *testing.T) {
	const mb = int64(1) << 20

	cases := []struct {
		name     string
		limitMB  int64
		used     int64
		incoming int64
		want     bool
	}{
		{"配额为 0（默认）= 不限制", 0, 100 * mb, 200 * mb, false},
		{"负数配额按不限制处理", -1, 100 * mb, 200 * mb, false},
		{"用量为 0、配额充足", 2048, 0, 200 * mb, false},
		{"正好用满配额：允许（软配额可占满）", 1000, 800 * mb, 200 * mb, false},
		{"超出 1 字节：拒绝", 1000, 800*mb + 1, 200 * mb, true},
		{"已远超配额、再传一个小文件：拒绝", 1000, 2000 * mb, 1, true},
		{"本次大小未知（0）：不拦，交给流式上限", 100, 200 * mb, 0, false},
		{"已用 + 本次刚好等于配额：允许", 500, 300 * mb, 200 * mb, false},
	}

	for _, c := range cases {
		got, msg := quotaExceeded(c.limitMB, c.used, c.incoming)
		if got != c.want {
			t.Errorf("%s：quotaExceeded(%d, %d, %d) = %v，期望 %v",
				c.name, c.limitMB, c.used, c.incoming, got, c.want)
		}
		// 拒绝时必须给出可执行的数字（配额/已用/本次），否则用户不知道怎么改
		if c.want {
			for _, want := range []string{"配额", "已用", "本次上传"} {
				if !containsStr(msg, want) {
					t.Errorf("%s：拒绝信息里缺 %q：%s", c.name, want, msg)
				}
			}
		} else if msg != "" {
			t.Errorf("%s：不该拒绝却给了理由：%s", c.name, msg)
		}
	}
}

func containsStr(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
