package grpcapi

import (
	"os"
	"path/filepath"
	"testing"
)

// 上传路径的净化规则必须被测试锁住。
//
// 这是"用户可控文件名 + 服务端路径"的接口 —— 历史上出问题的正是这一类。
// 实测（在 VM 上真跑）：客户端传 `../evil.txt` / `/etc/evil.txt` /
// `uploadtest/../../../tmp/evil.txt` 时，**服务端不会报错，而是把它们锚定回
// 实例目录**（分别落成 evil.txt / etc/evil.txt / tmp/evil.txt），
// 一个字节都没写到实例之外。
//
// 这个"净化而不是拒绝"的行为是有意的（resolvePath 用 filepath.Clean("/"+rel)
// 把路径钉在根上）。它比"报错"更好用，但**必须有人把这条不变量钉住** ——
// 一旦哪天有人把 Clean 去掉，表现会是"文件默默写到实例外"，而接口仍然返回成功。
func TestResolvePath_SanitizesTraversalIntoInstanceDir(t *testing.T) {
	dir := t.TempDir()
	inst := filepath.Join(dir, "instances", "beta01")
	if err := os.MkdirAll(inst, 0o700); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		in      string
		wantRel string // 期望落在实例目录内的哪个相对路径
		why     string
	}{
		{"plugins/x.jar", "plugins/x.jar", "正常相对路径应原样保留"},
		{"../evil.txt", "evil.txt", "../ 应被锚定回实例根，而不是写到上级"},
		{"../../../../etc/passwd", "etc/passwd", "多级 ../ 同样只能落到实例内"},
		{"/etc/evil.txt", "etc/evil.txt", "绝对路径应被当作实例内路径（开头的 / 被吃掉）"},
		{"uploadtest/../../../tmp/evil.txt", "tmp/evil.txt", "中段 ../ 同样被折叠在实例内"},
		{"./a/./b.txt", "a/b.txt", "单点与重复斜杠应被规范化"},
		{"a//b.txt", "a/b.txt", "重复斜杠应被规范化"},
	}

	for _, c := range cases {
		got, err := resolvePath(inst, c.in)
		if err != nil {
			t.Errorf("%s：resolvePath(%q) 报错 %v（该实现是净化而非拒绝）", c.why, c.in, err)
			continue
		}
		want := filepath.Join(inst, c.wantRel)
		if got != want {
			t.Errorf("%s：resolvePath(%q) = %s，期望 %s", c.why, c.in, got, want)
		}
		// 最关键的一条：结果必须在实例目录之内
		rel, err := filepath.Rel(inst, got)
		if err != nil || rel == ".." || len(rel) >= 3 && rel[:3] == ".."+string(filepath.Separator) {
			t.Errorf("%s：resolvePath(%q) 逃出了实例目录：%s", c.why, c.in, got)
		}
	}
}

// 受保护路径必须被单独挡住（与净化是两件事）。
//
// instance.json 即使"在实例目录内"也不允许通过文件接口读写：它是平台元数据
// （决定资源配额与实例身份），让它可写等于把平台设置交给租户。
//
// 注意：**不要**在这里断言 frpc.toml / frpc.ini —— T0 修复之后 frpc 配置已经搬到
// 实例目录之外（<frp_state_dir>/<实例ID>/，root 0700），实例目录里就算有同名文件
// 也只是租户自己的遗留数据，不是平台文件。（我第一版断言写了 frpc.ini，跑出来失败，
// 说明"凭印象写断言"不可靠；这条注释就是留给下一个人的。）
func TestIsProtectedPath_BlocksPlatformFiles(t *testing.T) {
	mustBlock := []string{
		"instance.json",
		"frpc.toml", // 含 frps 地址与 auth token
		"tunnels.json",
		"frpc.pid",
		"instance.json/../instance.json", // 规范化之后仍是它，不能靠路径写法绕过
		"/instance.json",
		"./instance.json",
	}
	for _, p := range mustBlock {
		if !isProtectedPath(p) {
			t.Errorf("%q 应被保护（平台元数据，不应能通过文件管理读写）", p)
		}
	}
	// 普通文件不能被误伤。名单以 file.go 里的 protectedFiles/protectedLogFiles 为准
	// （frpc.toml / tunnels.json / frpc.pid / instance.json 都在里面 —— 它们含 frps
	//  token 或平台状态，不能从文件管理里读写）。
	for _, p := range []string{
		"server.properties", "plugins/x.jar", "logs/latest.log",
		"frpc.ini",          // 旧版 frp 的配置名：平台已不用它，实例内同名文件只是租户遗留数据
		"instance.json.bak", // 近似名不能被当成受保护文件
		"plugins/frpc.toml", // 只保护根目录下的那一份，子目录里的同名文件不该误伤
	} {
		if isProtectedPath(p) {
			t.Errorf("%q 是普通文件，不该被保护", p)
		}
	}
}
