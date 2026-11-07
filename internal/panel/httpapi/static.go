package httpapi

import (
	"encoding/json"
	"html"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// handleStatic 提供前端静态资源（SPA）。
//
// 规则：
//   - /api/ 与 /ws/ 前缀的未知路径返回 404 JSON（不回退到 index.html）
//   - 命中真实文件则返回该文件（/assets/ 下的带 hash 资源长缓存）
//   - 其余路径回退到 index.html（支持前端路由）
func (s *Server) handleStatic(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		writeErr(w, http.StatusMethodNotAllowed, "方法不允许")
		return
	}

	upath := path.Clean("/" + r.URL.Path)

	// 接口路径不做 SPA 回退
	if strings.HasPrefix(upath, "/api/") || strings.HasPrefix(upath, "/ws/") {
		writeErr(w, http.StatusNotFound, "接口不存在")
		return
	}

	if s.webDir == "" {
		writeErr(w, http.StatusNotFound, "未配置前端目录（server.web_dir）")
		return
	}

	base, err := filepath.Abs(s.webDir)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "前端目录配置错误")
		return
	}

	// 解析目标文件并做目录穿越防护
	target, err := filepath.Abs(filepath.Join(base, filepath.FromSlash(upath)))
	if err != nil || (target != base && !strings.HasPrefix(target, base+string(filepath.Separator))) {
		writeErr(w, http.StatusForbidden, "非法路径")
		return
	}

	if fi, err := os.Stat(target); err == nil && !fi.IsDir() {
		if strings.Contains(upath, "/assets/") {
			// Vite 产物文件名带内容 hash，可长期缓存
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-cache")
		}
		http.ServeFile(w, r, target)
		return
	}

	// SPA 回退到 index.html
	index := filepath.Join(base, "index.html")
	if _, err := os.Stat(index); err != nil {
		writeErr(w, http.StatusNotFound, "前端尚未构建（缺少 index.html），请执行 web 目录下的 npm run build")
		return
	}
	w.Header().Set("Cache-Control", "no-cache")

	// 配了安全入口时，index.html 要注入两样东西：
	//   ① <base href>：让静态资源按前缀解析（配合 vite 的 base: './'）
	//   ② <meta name="atl-base">：让前端知道接口前缀，fetch / WebSocket 自己拼上
	//
	// ⚠️ 这里**不能**用内联 <script> 注入 window.__ATL_BASE__，尽管那样最直接：
	// 面板自己下发的 CSP 是 `script-src 'self'`（这是挡 XSS 外带的关键一条），
	// 内联脚本会被浏览器直接拒绝执行 —— 而"拒绝"只体现为控制台一条 CSP 警告，
	// 页面照常渲染，于是表现成"启用安全入口后全站接口 404、登录页版本号显示未知"。
	// 2026-11-07 真机撞到：0.11.0 的验收只核对了 HTML 里**有没有**那段字符串，
	// 没核对它**有没有执行**，于是这个 bug 一直躺着。改用 <meta> 后不需要放宽 CSP。
	//
	// 同理，<base> 需要 CSP 里 base-uri 允许同源（原来是 'none'，会把 <base> 也挡掉）。
	if s.securityEntry != "" {
		raw, err := os.ReadFile(index)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "读取 index.html 失败")
			return
		}
		prefix := "/" + s.securityEntry
		inject := "<base href=\"" + prefix + "/\">" +
			"<meta name=\"atl-base\" content=\"" + htmlEscape(prefix) + "\">"
		out := strings.Replace(string(raw), "<head>", "<head>\n    "+inject, 1)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(out))
		return
	}
	http.ServeFile(w, r, index)
}

// jsonQuote 把字符串安全地嵌进 JS 字面量（保留给可能的外部消费者）。
// 入口字符集已限定为字母数字与 - _. ，这里仍做一次转义以防将来放宽。
func jsonQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// htmlEscape 用属性值安全的形式转义（入口字符集本来就安全，这里是第二道）。
func htmlEscape(s string) string {
	return html.EscapeString(s)
}
