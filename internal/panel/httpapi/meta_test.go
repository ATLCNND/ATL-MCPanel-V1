package httpapi

import (
	"testing"

	"github.com/ATLCNND/ATL-MCPanel/internal/common/config"
	"github.com/ATLCNND/ATL-MCPanel/internal/common/version"
)

// /api/meta 必须**免登录**可用。
//
// 为什么这条要单独钉住：登录页本身就要显示面板名称、图标与版本号，
// 那时用户手上还没有令牌。若哪天有人顺手把它挪到 requireAuth 后面，
// 表现是"登录页品牌区空白/版本号显示不出来"，而接口本身返回 401 ——
// 这类改动不会让任何现有测试失败，所以这里用匿名请求锁住。
func TestMetaRequiresNoAuth(t *testing.T) {
	_, ts := newTestServer(t)

	code, body := doJSON(t, ts, "GET", "/api/meta", "", nil)
	if code != 200 {
		t.Fatalf("/api/meta 应允许匿名访问（登录页要用），实际 HTTP %d %v", code, body)
	}
	if body["name"] == nil || body["name"] == "" {
		t.Errorf("应返回面板名称，实际 %v", body["name"])
	}
	if body["version"] != version.Version {
		t.Errorf("版本号应来自二进制（version.Version=%q），实际 %v", version.Version, body["version"])
	}
	if body["logo_url"] == nil || body["favicon_url"] == nil {
		t.Errorf("应给出图标地址，实际 logo=%v favicon=%v", body["logo_url"], body["favicon_url"])
	}
}

// 面板名可以配置（换品牌），未配置时回退默认值。
//
// 这条守的是"改品牌只要改一行配置"：如果哪天面板名又被写死在代码里，
// 用户换了配置却看不到变化，而界面上不会有任何报错。
func TestMetaPanelNameFromConfig(t *testing.T) {
	cases := []struct {
		name string
		cfg  string
		want string
	}{
		{"配置了就用配置值", "我的云面板", "我的云面板"},
		{"留空回退默认名", "", config.DefaultPanelName},
		{"默认名与内置默认一致", config.DefaultPanelName, config.DefaultPanelName},
	}
	for _, c := range cases {
		srv, ts := newTestServer(t)
		srv.panelName = c.cfg
		code, body := doJSON(t, ts, "GET", "/api/meta", "", nil)
		if code != 200 {
			t.Fatalf("%s：HTTP %d", c.name, code)
		}
		if body["name"] != c.want {
			t.Errorf("%s：面板名 = %v，期望 %q", c.name, body["name"], c.want)
		}
	}
}

// 面板显示名只影响展示，不参与鉴权：改了名字之后路由权限不变。
//
// 这条是个"防呆"——它保证了将来若要给 panel_name 加校验（比如限制长度），
// 也不会顺手把鉴权逻辑卷进去。
func TestPanelNameDoesNotAffectAuth(t *testing.T) {
	srv, ts := newTestServer(t)
	srv.panelName = "renamed-panel"

	// 需要管理员权限的接口在匿名访问时仍应 401（而不是因为名字变了就放行）
	if code, _ := doJSON(t, ts, "GET", "/api/users", "", nil); code != 401 {
		t.Errorf("改名后匿名访问管理接口仍应 401，实际 %d", code)
	}
	// 免登录接口仍应可访问
	if code, _ := doJSON(t, ts, "GET", "/api/meta", "", nil); code != 200 {
		t.Errorf("改名后 /api/meta 仍应 200，实际 %d", code)
	}
}
