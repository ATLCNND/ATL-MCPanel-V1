package httpapi

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strconv"

	pb "github.com/ATLCNND/ATL-MCPanel/internal/proto/mcpanel"
)

// handleInstanceIcon GET /api/instances/{id}/icon?token=
//
// 实例图标（节点上实例目录里的 server-icon.png / icon.png）。
//
// 三个刻意的设计点：
//   - 走 `requireAuthQuery`：图片由 `<img src>` 直接发起，浏览器**无法**附加
//     Authorization 头（与文件下载、头像预览同一类问题）。所以这条路由允许
//     `?token=` 传令牌 —— 也正因如此它必须只吐图标这一种东西，不能变成
//     通用文件读取口子。
//   - 权限只要求 **viewer**：列表页里每个能看见该实例的人都要看到图标，
//     没必要卡到 collab（文件下载那条路才是 collab）。
//   - 缓存：URL 上带 `?v=<icon_mtime>`（前端拼的），所以这里可以放心给
//     `max-age`；再加 `private`，避免带令牌的响应落进共享缓存。
func (s *Server) handleInstanceIcon(w http.ResponseWriter, r *http.Request) {
	instanceID := r.PathValue("id")
	if !s.requireInstanceLevel(w, r, instanceID, LevelViewer) {
		return
	}
	cli, _, err := s.getDaemonClient(instanceID)
	if err != nil {
		s.iconNotFound(w, "实例不在线或节点不可达")
		return
	}

	// 图标很小且已知上限（Daemon 侧 <= 2MiB），不必像大文件下载那样省内存
	stream, err := cli.GetInstanceIcon(context.Background(), &pb.InstanceRequest{InstanceId: instanceID})
	if err != nil {
		s.iconNotFound(w, err.Error())
		return
	}

	first, err := stream.Recv()
	if err != nil {
		s.iconNotFound(w, err.Error())
		return
	}

	// 按**内容**判断类型而不是按文件名：用户可能把 jpg 改名成 icon.png，
	// 而 Content-Type 报错会让浏览器直接不渲染（配合 nosniff 更不会猜）。
	ct := sniffImageType(first.Data)
	if ct == "" {
		s.iconNotFound(w, "图标不是可识别的图片格式（支持 PNG / JPEG / GIF / WebP）")
		return
	}

	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "private, max-age=300")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if first.Total > 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(first.Total, 10))
	}

	if _, err := w.Write(first.Data); err != nil {
		return
	}
	for {
		chunk, err := stream.Recv()
		if err == io.EOF {
			return
		}
		if err != nil {
			// 响应头已发出，只能中断连接 —— 浏览器会显示为图片加载失败，
			// 前端 Avatar 的 onError 会回退到首字母，不会留裂图。
			s.logger.Warn("图标下发中断", "instance", instanceID, "error", err)
			return
		}
		if len(chunk.Data) == 0 {
			continue
		}
		if _, err := w.Write(chunk.Data); err != nil {
			return
		}
	}
}

// iconNotFound 统一按 404 处理"没有图标"的各种情况。
//
// 刻意不用 500：图标是可选装饰，缺它完全正常（前端回退首字母），
// 而 500 会让人以为接口坏了。真的要排查时原因会进日志。
func (s *Server) iconNotFound(w http.ResponseWriter, reason string) {
	s.logger.Debug("实例图标不可用", "reason", reason)
	writeErr(w, http.StatusNotFound, "该实例没有图标")
}

// sniffImageType 按文件头判断图片类型（空串 = 不认识）。
//
// 只认浏览器都支持的四种，够用且不引入依赖。
func sniffImageType(b []byte) string {
	switch {
	case len(b) >= 8 && bytes.Equal(b[:8], []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a}):
		return "image/png"
	case len(b) >= 3 && b[0] == 0xff && b[1] == 0xd8 && b[2] == 0xff:
		return "image/jpeg"
	case len(b) >= 6 && (bytes.Equal(b[:6], []byte("GIF87a")) || bytes.Equal(b[:6], []byte("GIF89a"))):
		return "image/gif"
	case len(b) >= 12 && bytes.Equal(b[:4], []byte("RIFF")) && bytes.Equal(b[8:12], []byte("WEBP")):
		return "image/webp"
	}
	return ""
}
