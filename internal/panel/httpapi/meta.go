package httpapi

import (
	"net/http"

	"github.com/ATLCNND/ATL-MCPanel/internal/common/config"
	"github.com/ATLCNND/ATL-MCPanel/internal/common/version"
)

// handleMeta GET /api/meta —— 面板的"身份信息"：名称、版本、图标地址。
//
// 为什么单独一个接口，而不是塞进 /api/health：
//   - health 是给监控探针用的（只看 status），往里面加品牌信息会让探针的判断变复杂；
//   - 这个接口**必须免登录**：登录页本身就要显示名称、图标与版本号，
//     而那时用户还没有令牌。多暴露的只是"这是哪个面板、哪个版本"，
//     不含任何用户数据、内网地址或配置内容。
//
// 为什么版本号要走接口而不是前端写死：前端写死过一次（Login.tsx 里那个
// `PANEL_VERSION = 'v0.1.0'`，那是 package.json 的版本，与面板版本根本不是一回事），
// 于是界面上显示 0.1.0、二进制却是 0.9.14 —— 报 bug 时这个号码会把人带偏。
// 版本只有一个来源：二进制里由 -ldflags 注入的那份。
func (s *Server) handleMeta(w http.ResponseWriter, r *http.Request) {
	// 品牌区/登录页每次加载都会读一次，但不该被浏览器缓存太久：
	// 升级面板之后，用户刷新页面就该看到新版本号（缓存住会显示旧号码）。
	w.Header().Set("Cache-Control", "no-cache")
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"name":        s.displayName(),
		"version":     version.Version,
		"commit":      version.Commit,
		"built_at":    version.BuildTime,
		"go_version":  version.GoVersion(),
		"logo_url":    "/branding/logo-128.png",
		"favicon_url": "/branding/favicon-64.png",
	})
}

// displayName 面板显示名（配置为空时回退到默认值）。
//
// 方法名不叫 panelName：那个名字被字段占用了（字段存的是配置值，
// 这个方法负责"配置为空时给默认值"这层语义）—— 同名会导致编译不过。
func (s *Server) displayName() string {
	if s.panelName != "" {
		return s.panelName
	}
	return config.DefaultPanelName
}
