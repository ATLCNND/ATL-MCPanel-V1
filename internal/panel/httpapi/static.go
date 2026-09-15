package httpapi

import (
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
	http.ServeFile(w, r, index)
}
