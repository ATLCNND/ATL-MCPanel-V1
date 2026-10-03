package httpapi

import (
	"strings"
	"testing"
)

// 玩家聊天过滤必须真的过滤**服务端实际打出来的那几种格式**。
//
// 这条原来是坏的，而且坏得没有症状：chatLineRe 的前缀写成 `(?:^|\]\s*)`，
// 要求 `]` 之后只跟空白就到 `<`，而真实日志是
//
//	[10:01:10] [Server thread/INFO]: <Steve> 你好
//
// `]` 与 `<` 之间隔着 `]: `。于是配置与界面都承诺"默认过滤玩家聊天"，
// 实际一行都没过滤，接口还如实报告"过滤 0 行" —— 用户只会以为"这服没人聊天"。
// 隐私承诺一旦只做了一半，比不做更糟：用户是**据此**才敢上传日志的。
//
// 所以这里把三种真实格式都钉住，另外确认**不能误伤**普通日志行
//（过滤过头会把崩溃现场删掉，那是另一个方向的错误）。
func TestStripPlayerChatMatchesRealServerFormats(t *testing.T) {
	cases := []struct {
		why  string
		in   string
		want int // 期望被判定为聊天的行数
	}{
		{
			why: "Paper/Spigot 标准格式（[time] [thread/INFO]: <名字> 内容）",
			in: "[10:01:10] [Server thread/INFO]: <Steve> 你好\n" +
				"[10:01:11] [Server thread/INFO]: Steve joined the game",
			want: 1,
		},
		{
			why: "简写格式（[time INFO]: <名字> 内容）",
			in: "[10:01:10 INFO]: <Alex> hello there\n" +
				"[10:01:12 INFO]: Done (3.456s)! For help, type \"help\"",
			want: 1,
		},
		{
			why: "[Not Secure] 变体（签名/非签名聊天的额外标记）",
			in: "[10:01:10] [Server thread/INFO]: [Not Secure] <Herobrine> boo\n" +
				"[10:01:11] [Server thread/INFO]: <Steve> [Not Secure] 这条也还是聊天",
			want: 2,
		},
		{
			why: "无时间戳的裸格式（<名字> 内容）",
			in:  "<Steve> hi\nSaving the game",
			want: 1,
		},
		{
			why: "普通日志行不能被误伤（尖括号出现在报错里）",
			in: "[10:01:10] [Server thread/WARN]: Missing block <minecraft:stone> at chunk 3\n" +
				"[10:01:11] [Server thread/ERROR]: Unexpected key <foo> in config\n" +
				"[10:01:12] [Server thread/INFO]: Preparing spawn area: 42%",
			want: 0,
		},
	}

	for _, c := range cases {
		out, got := stripPlayerChat(c.in)
		if got != c.want {
			t.Errorf("%s：过滤了 %d 行，期望 %d 行\n输入：\n%s\n输出：\n%s",
				c.why, got, c.want, c.in, out)
			continue
		}
		// 过滤掉的行必须真的**不在**输出里（不能只动计数）
		if c.want > 0 && strings.Contains(out, "<Steve> 你好") {
			t.Errorf("%s：计数对了但聊天内容仍留在输出里:\n%s", c.why, out)
		}
		// 反过来：没被过滤的行必须原样保留（别把现场删了）
		if c.want == 0 && !strings.Contains(out, "Missing block") {
			t.Errorf("%s：普通日志行被误删了:\n%s", c.why, out)
		}
	}
}

// 过滤后要留下"这里原本有聊天"的说明，否则 AI 会把缺失的上下文当成日志异常。
func TestStripPlayerChatAnnouncesFiltering(t *testing.T) {
	out, n := stripPlayerChat("[10:01:10] [Server thread/INFO]: <Steve> 你好\n")
	if n != 1 {
		t.Fatalf("应过滤 1 行，实际 %d", n)
	}
	if !strings.Contains(out, "过滤 1 行") {
		t.Errorf("输出里应有一行说明告诉分析方「聊天被去掉了」，实际:\n%s", out)
	}
}
