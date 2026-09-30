package analysis

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 密钥必须能**跨进程重启**解开：面板重启后要还能用已存的 API Key。
//
// 这条守的是一个很容易踩的坑：密钥文件要么没持久化、要么权限不对，
// 现象都是"保存过的 API Key 突然失效"，而报错只会说"解密失败"。
func TestSecretBoxRoundTripAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "analysis.key")

	box, err := OpenSecretBox(path)
	if err != nil {
		t.Fatalf("创建密钥失败: %v", err)
	}
	secret := "sk-test-1234567890abcdef"
	enc, err := box.Encrypt(secret)
	if err != nil {
		t.Fatalf("加密失败: %v", err)
	}
	if bytes.Contains(enc, []byte(secret)) {
		t.Fatal("密文里出现了明文 —— 等于没加密")
	}

	// 模拟重启：重新打开同一个密钥文件
	box2, err := OpenSecretBox(path)
	if err != nil {
		t.Fatalf("重新打开密钥失败: %v", err)
	}
	got, err := box2.Decrypt(enc)
	if err != nil {
		t.Fatalf("解密失败: %v", err)
	}
	if got != secret {
		t.Fatalf("解密结果不对：%q，期望 %q", got, secret)
	}

	// 权限必须是 0600：密钥能被同机器上别的用户读到就白加密了
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Errorf("密钥文件权限应为 0600，实际 %o", st.Mode().Perm())
	}
}

// 换了密钥文件时必须给出**能照着查**的错误，而不是一句 cipher 报错。
func TestSecretBoxWrongKeyMessage(t *testing.T) {
	dir := t.TempDir()
	box1, _ := OpenSecretBox(filepath.Join(dir, "a.key"))
	enc, _ := box1.Encrypt("sk-abcdefghijklmnop")

	box2, _ := OpenSecretBox(filepath.Join(dir, "b.key"))
	_, err := box2.Decrypt(enc)
	if err == nil {
		t.Fatal("用另一个密钥竟能解开，加密形同虚设")
	}
	if !strings.Contains(err.Error(), "analysis.key") {
		t.Errorf("错误信息应提示密钥文件不匹配（便于排查），实际：%v", err)
	}
}

// 空 key：加密得 nil、解密得空串，都不算错误（LogShare/mclo.gs 本来就不需要 key）。
func TestSecretBoxEmptyKey(t *testing.T) {
	box, err := OpenSecretBox(filepath.Join(t.TempDir(), "k"))
	if err != nil {
		t.Fatal(err)
	}
	enc, err := box.Encrypt("")
	if err != nil || enc != nil {
		t.Errorf("空串应加密为 nil，实际 %v / %v", enc, err)
	}
	plain, err := box.Decrypt(nil)
	if err != nil || plain != "" {
		t.Errorf("空密文应解出空串，实际 %q / %v", plain, err)
	}
}

// KeyHint 只回末 4 位；太短的 key 一律 **** ——
// 回显 3 位中的 4 位等于泄露了大半。
func TestKeyHint(t *testing.T) {
	cases := map[string]string{
		"sk-1234567890abcd": "****abcd",
		"short":             "****",
		"":                  "****",
		"12345678":          "****5678",
	}
	for in, want := range cases {
		if got := KeyHint(in); got != want {
			t.Errorf("KeyHint(%q) = %q，期望 %q", in, got, want)
		}
	}
}

// 出站地址白名单：默认拦掉内网/回环/链路本地，管理员打开开关后才放行私有网段。
//
// 这是 D2 的落地断言：`base_url` 是用户可控 URL，而面板会**带着 key** 去请求它，
// 不拦就等于把"面板"变成任何人可用的内网探测器。
func TestCheckOutboundURL(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name         string
		url          string
		allowPrivate bool
		wantErr      bool
	}{
		{"公网 https", "https://api.deepseek.com/v1", false, false},
		{"回环", "http://127.0.0.1:8080/v1", false, true},
		{"回环（管理员允许内网后可放行：自建网关可能在面板本机）", "http://127.0.0.1:8080/v1", true, false},
		{"私有网段", "http://10.1.2.3:8000/v1", false, true},
		{"私有网段但允许内网", "http://10.1.2.3:8000/v1", true, false},
		{"链路本地（云元数据）", "http://169.254.169.254/latest/meta-data", false, true},
		{"链路本地即使允许内网也拦（元数据服务与自建网关无关）", "http://169.254.169.254/latest/meta-data", true, true},
		{"非 http 协议", "file:///etc/passwd", false, true},
		{"无法解析", "https://this-host-does-not-exist-atl.invalid/v1", false, true},
	}
	for _, c := range cases {
		err := CheckOutboundURL(ctx, c.url, c.allowPrivate)
		if c.wantErr && err == nil {
			t.Errorf("%s：%s 应当被拒绝，却放行了", c.name, c.url)
		}
		if !c.wantErr && err != nil {
			t.Errorf("%s：%s 应当放行，却被拒：%v", c.name, c.url, err)
		}
	}
}

// 限流：分钟窗口与天窗口都要生效，且**被拒时不消耗额度**（否则重试会把自己锁死）。
func TestRateLimiter(t *testing.T) {
	rl := NewRateLimiter(3, 5)
	now := time.Now()
	rl.now = func() time.Time { return now }

	for i := 0; i < 3; i++ {
		if ok, _, why := rl.Allow("u1"); !ok {
			t.Fatalf("第 %d 次应当放行，却被拒：%s", i+1, why)
		}
	}
	ok, wait, why := rl.Allow("u1")
	if ok {
		t.Fatal("第 4 次应被分钟限额拒绝")
	}
	if wait <= 0 || !strings.Contains(why, "频繁") {
		t.Errorf("被拒时应给出等待时间与原因，实际 wait=%v why=%q", wait, why)
	}

	// 另一个用户不受影响（限额是按用户的）
	if ok, _, _ := rl.Allow("u2"); !ok {
		t.Error("限流不该影响其它用户")
	}

	// 时间前进 1 分钟：分钟窗口清空，但天窗口仍在。
	// 注意 u1 到此刻**只成功调用过 3 次** —— 被拒的那次不计数（否则重试会把自己越锁越死）。
	now = now.Add(61 * time.Second)
	if ok, _, why := rl.Allow("u1"); !ok {
		t.Errorf("过了分钟窗口后应放行，却被拒：%s", why)
	}
	// 第 5 次：仍在天限额（5）之内
	if ok, _, why := rl.Allow("u1"); !ok {
		t.Errorf("第 5 次应放行（天限额 5），却被拒：%s", why)
	}
	// 第 6 次：天限额用尽
	ok, wait, why = rl.Allow("u1")
	if ok {
		t.Fatal("达到天限额后应被拒绝")
	}
	if !strings.Contains(why, "每天") {
		t.Errorf("天限额的提示应说明是每日额度，实际 %q（wait=%v）", why, wait)
	}
}

// 提供方链的排序与兜底：内置两家始终在链里，自配平台排最后。
func TestBuildChain(t *testing.T) {
	providers := []Provider{
		{ID: 1, Name: "LogShare", Kind: KindLogShare, Enabled: true},
		{ID: 2, Name: "mclo.gs", Kind: KindMclogs, Enabled: true},
		{ID: 3, Name: "我的网关", Kind: KindOpenAI, Enabled: true},
	}

	// 默认顺序
	chain := BuildChain(nil, providers, 0)
	if len(chain) != 3 {
		t.Fatalf("默认链应含 3 家，实际 %d", len(chain))
	}
	if chain[0].Provider.Kind != KindLogShare || chain[1].Provider.Kind != KindMclogs {
		t.Errorf("默认顺序应为 logshare → mclogs，实际 %s → %s",
			chain[0].Provider.Kind, chain[1].Provider.Kind)
	}
	if chain[2].Provider.Kind != KindOpenAI {
		t.Errorf("自配平台应排最后，实际第 3 位是 %s", chain[2].Provider.Kind)
	}

	// 自定义顺序：自配平台提到最前
	chain = BuildChain([]string{"3", "logshare"}, providers, 0)
	if chain[0].Provider.ID != 3 {
		t.Errorf("按 id 指定顺序应生效，实际第 1 位是 %d", chain[0].Provider.ID)
	}
	// logshare 已在顺序里，mclogs 作为兜底仍应出现，且不重复
	seen := map[string]int{}
	for _, e := range chain {
		seen[e.Provider.Kind]++
	}
	if seen[KindMclogs] != 1 || seen[KindLogShare] != 1 {
		t.Errorf("内置提供方应各出现一次（不重复、不丢失）：%v", seen)
	}

	// 指定具体提供方：只尝试它
	chain = BuildChain(nil, providers, 3)
	if len(chain) != 1 || chain[0].Provider.ID != 3 {
		t.Errorf("指定提供方时应只尝试它，实际 %d 项", len(chain))
	}

	// 停用的不进链
	off := []Provider{{ID: 1, Kind: KindLogShare, Name: "LS", Enabled: false},
		{ID: 2, Kind: KindMclogs, Name: "MC", Enabled: true}}
	chain = BuildChain(nil, off, 0)
	for _, e := range chain {
		if e.Provider.Kind == KindLogShare {
			t.Error("停用的提供方不该进链")
		}
	}
}

// 配置不全要在**进链之前**被拦住：这类错误不该消耗限流额度，
// 也不该回退到别家（那会让用户以为"我配的平台没生效"）。
func TestPreflightError(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name    string
		p       Provider
		wantErr bool
	}{
		{"自配平台缺地址", Provider{Name: "X", Kind: KindOpenAI, APIKey: "k"}, true},
		{"自配平台缺 key", Provider{Name: "X", Kind: KindOpenAI, BaseURL: "https://api.deepseek.com/v1"}, true},
		{"自配平台地址是内网", Provider{Name: "X", Kind: KindOpenAI, APIKey: "k", BaseURL: "http://10.0.0.5:8000/v1"}, true},
		{"内置提供方无需配置", Provider{Name: "mclo.gs", Kind: KindMclogs}, false},
		{"自配平台配置齐全", Provider{Name: "X", Kind: KindOpenAI, APIKey: "k", BaseURL: "https://api.deepseek.com/v1"}, false},
	}
	for _, c := range cases {
		err := PreflightError(ctx, c.p, false)
		if c.wantErr && err == nil {
			t.Errorf("%s：应当报错却通过了", c.name)
		}
		if !c.wantErr && err != nil {
			t.Errorf("%s：不该报错，实际 %v", c.name, err)
		}
	}
}

// 求助文本要带上"社区看不到、但很可能是原因"的环境信息。
func TestHelpText(t *testing.T) {
	s := HelpText("beta01", "Paper 1.21.1", "21", "2G",
		"容器化运行（容器内存上限 2G）", "启动后约 30 秒崩溃",
		"https://mclo.gs/abc", "https://api.mclo.gs/1/raw/abc", 12)
	for _, want := range []string{"beta01", "https://mclo.gs/abc", "12", "Paper 1.21.1", "Java 21",
		"2G", "容器化运行", "启动后约 30 秒崩溃"} {
		if !strings.Contains(s, want) {
			t.Errorf("求助文本缺少 %q：\n%s", want, s)
		}
	}
}
