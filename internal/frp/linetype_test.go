package frp

import "testing"

func TestNormalizeLineType(t *testing.T) {
	cases := map[string]string{
		"":              LineSelfHosted, // 老数据没这一列 → 按自建处理
		"self_hosted":   LineSelfHosted,
		"third_party":   LineThirdParty,
		"third-party":   LineThirdParty,
		"DIRECT":        LineDirect,
		"public_direct": LineDirect,
	}
	for in, want := range cases {
		if got := NormalizeLineType(in); got != want {
			t.Errorf("NormalizeLineType(%q) = %q，期望 %q", in, got, want)
		}
	}
	if got := NormalizeLineType("nonsense"); got != "" {
		t.Errorf("未知类型应返回空串，实际 %q", got)
	}
}

func TestLineTypeBehaviours(t *testing.T) {
	// 这三种行为组合是**互相咬合**的，任何一条单独改动都会让状态自相矛盾
	//（例如"直连也要端口段"或"第三方由面板分配端口"）。
	cases := []struct {
		line      string
		needsSeg  bool
		runsFrpc  bool
		allocates bool
		label     string
	}{
		{LineSelfHosted, true, true, true, "自建 frps"},
		{LineThirdParty, false, true, false, "第三方单隧道"},
		{LineDirect, false, false, false, "公网直出"},
	}
	for _, c := range cases {
		if got := LineNeedsPortSegment(c.line); got != c.needsSeg {
			t.Errorf("%s LineNeedsPortSegment = %v，期望 %v", c.line, got, c.needsSeg)
		}
		if got := LineRunsFrpc(c.line); got != c.runsFrpc {
			t.Errorf("%s LineRunsFrpc = %v，期望 %v", c.line, got, c.runsFrpc)
		}
		if got := LineAllocatesPort(c.line); got != c.allocates {
			t.Errorf("%s LineAllocatesPort = %v，期望 %v", c.line, got, c.allocates)
		}
		if got := LineTypeLabel(c.line); got != c.label {
			t.Errorf("%s LineTypeLabel = %q，期望 %q", c.line, got, c.label)
		}
	}
	// 直连绝不能跑 frpc、也不能被分配端口 —— 单独再钉一遍，因为这两条最容易在重构里丢
	if LineRunsFrpc(LineDirect) || LineAllocatesPort(LineDirect) {
		t.Error("公网直出不应运行 frpc、也不应分配端口")
	}
}

func TestValidateLineTypeRejectsGarbage(t *testing.T) {
	if _, err := ValidateLineType("frps"); err == nil {
		t.Error("未知类型应当报错")
	}
	if got, err := ValidateLineType("third_party"); err != nil || got != LineThirdParty {
		t.Errorf("合法类型不应报错：%q %v", got, err)
	}
}
