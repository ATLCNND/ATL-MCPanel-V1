package grpcapi

import "testing"

// 节点剩余空间检查：保护**整台机器**的最后一道。
//
// 面板侧按实例配额拦（可以没有配额），这一层管"磁盘还能不能写"。磁盘写到 100%
// 的后果不是"这次上传失败"，而是所有实例都存不了档、日志写不下去 —— 严重得多。
func TestEnoughSpace(t *testing.T) {
	const gb = int64(1) << 30
	const mb = int64(1) << 20

	// 语义是"**写完之后**剩余 ≥ uploadFreeMargin(1GB)"，所以边界要按 free-need 算。
	// （第一版我把 1200MB-200MB 当成"刚好 1GB"写了断言，实际是 1000MB —— 跑出来
	//   失败才发现；边界用例必须把算式写在旁边，不能凭感觉。）
	cases := []struct {
		name string
		free int64
		need int64
		want bool
	}{
		{"空间充足", 50 * gb, 200 * mb, true},                    // 50GB-200MB = 49.8GB ≥ 1GB
		{"写完后正好剩 1GB：允许（边界相等）", gb + 200*mb, 200 * mb, true}, // 剩 1GB
		{"写完后剩 1GB-1 字节：拒绝", gb + 200*mb - 1, 200 * mb, false},
		{"写完后只剩 1000MB：拒绝", 1200 * mb, 200 * mb, false}, // 剩 1000MB < 1GB
		{"磁盘几乎满了", 100 * mb, 50 * mb, false},            // 剩 50MB
		{"需求远大于可用", 1 * gb, 10 * gb, false},             // 负数
	}
	for _, c := range cases {
		if got := enoughSpace(c.free, c.need); got != c.want {
			t.Errorf("%s：enoughSpace(free=%d, need=%d) 剩 %d = %v，期望 %v",
				c.name, c.free, c.need, c.free-c.need, got, c.want)
		}
	}
}

// freeBytes 至少要能在正常目录上读出正数（读不出来时代码选择"不拦"，见注释）。
func TestFreeBytes(t *testing.T) {
	free, err := freeBytes(t.TempDir())
	if err != nil {
		t.Skipf("该环境读不到 Statfs（%v），跳过", err)
	}
	if free <= 0 {
		t.Errorf("可用空间应为正数，实际 %d", free)
	}
	// 与"1GB 余量"这条策略对齐做一次量级检查：测试机不可能少于 1GB 可用
	if free < 1<<30 {
		t.Logf("注意：本机可用空间仅 %d 字节，接近 uploadFreeMargin，相关用例可能拒绝上传", free)
	}
}

// "没发完"必须被识别出来 —— 这条守住的是**数据完整性**，不只是错误提示。
//
// 背景：流式接收时"客户端提前半步关流"与"正常发完"在服务端都是 io.EOF。
// 少了这个判断，客户端中断会把截断内容 commit 到目标路径（插件 jar 变成半截），
// 而接口看起来还像是成功的。
func TestUploadTruncated(t *testing.T) {
	cases := []struct {
		name     string
		declared int64
		written  int64
		want     bool
		why      string
	}{
		{"发完（相等）", 1024, 1024, false, "正常完成不能误判"},
		{"只收到一半", 1024, 512, true, "中断必须识别（半截 jar 会被当完整插件加载）"},
		{"一个字节都没收到", 1024, 0, true, "空文件也要识别"},
		{"收到的比声明的还多", 100, 200, true, "不可能的情况：宁可拒绝也不要写下去"},
		{"客户端没声明长度（分块传输）", 0, 4096, false, "无法判断，按发完处理，不误伤"},
		{"长度声明为负数（异常输入）", -1, 4096, false, "按未声明处理"},
		{"大文件发完", 256 << 20, 256 << 20, false, "上限附近同样要判等"},
	}
	for _, c := range cases {
		if got := uploadTruncated(c.declared, c.written); got != c.want {
			t.Errorf("%s：uploadTruncated(declared=%d, written=%d) = %v，期望 %v（%s）",
				c.name, c.declared, c.written, got, c.want, c.why)
		}
	}
}
