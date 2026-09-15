package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	pb "github.com/ATLCNND/ATL-MCPanel/internal/proto/mcpanel"
)

// handleRenameFile POST /api/instances/{id}/file/rename
func (s *Server) handleRenameFile(w http.ResponseWriter, r *http.Request) {
	instanceID := r.PathValue("id")
	if !s.requireInstanceLevel(w, r, instanceID, LevelOwner) {
		return
	}
	var req struct {
		Path    string `json:"path"`
		NewName string `json:"new_name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "无效请求体")
		return
	}
	cli, _, err := s.getDaemonClient(instanceID)
	if err != nil {
		writeErr(w, http.StatusNotFound, "实例不存在")
		return
	}
	resp, err := cli.RenameFile(context.Background(), &pb.RenameFileRequest{
		InstanceId: instanceID, Path: req.Path, NewName: req.NewName,
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !resp.Success {
		writeErr(w, http.StatusInternalServerError, resp.Error)
		return
	}
	s.audit(r, "rename_file", instanceID, fmt.Sprintf("%s -> %s", req.Path, req.NewName))
	writeJSON(w, http.StatusOK, map[string]string{"message": resp.Message})
}

// handleCopyFile POST /api/instances/{id}/file/copy
//
// 复制或移动（move=true 即"剪切 + 粘贴"）。
func (s *Server) handleCopyFile(w http.ResponseWriter, r *http.Request) {
	instanceID := r.PathValue("id")
	if !s.requireInstanceLevel(w, r, instanceID, LevelOwner) {
		return
	}
	var req struct {
		Src       string `json:"src"`
		Dst       string `json:"dst"`
		Move      bool   `json:"move"`
		Overwrite bool   `json:"overwrite"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "无效请求体")
		return
	}
	cli, _, err := s.getDaemonClient(instanceID)
	if err != nil {
		writeErr(w, http.StatusNotFound, "实例不存在")
		return
	}
	// 大目录的复制可能跑到分钟级，超时给足；但前端对超大目录会引导用户
	// 改用「压缩」（走排队任务），避免长时间占住一个 HTTP 连接。
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	resp, err := cli.CopyFile(ctx, &pb.CopyFileRequest{
		InstanceId: instanceID, Src: req.Src, Dst: req.Dst,
		Move: req.Move, Overwrite: req.Overwrite,
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !resp.Success {
		writeErr(w, http.StatusInternalServerError, resp.Error)
		return
	}
	action := "copy_file"
	if req.Move {
		action = "move_file"
	}
	s.audit(r, action, instanceID, fmt.Sprintf("%s -> %s", req.Src, req.Dst))
	writeJSON(w, http.StatusOK, map[string]string{"message": resp.Message})
}

// handleSearchFiles GET /api/instances/{id}/files/search?path=&keyword=&limit=
func (s *Server) handleSearchFiles(w http.ResponseWriter, r *http.Request) {
	instanceID := r.PathValue("id")
	if !s.requireInstanceLevel(w, r, instanceID, LevelCollab) {
		return
	}
	keyword := r.URL.Query().Get("keyword")
	if strings.TrimSpace(keyword) == "" {
		writeErr(w, http.StatusBadRequest, "请输入搜索关键字")
		return
	}
	limit := 200
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 1000 {
			limit = n
		}
	}

	cli, _, err := s.getDaemonClient(instanceID)
	if err != nil {
		writeErr(w, http.StatusNotFound, "实例不存在")
		return
	}
	// 搜索需要遍历目录树，给足超时；Daemon 侧另有遍历条目上限兜底
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	resp, err := cli.SearchFiles(ctx, &pb.SearchFilesRequest{
		InstanceId: instanceID,
		Path:       r.URL.Query().Get("path"),
		Keyword:    keyword,
		Limit:      int32(limit),
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !resp.Success {
		writeErr(w, http.StatusInternalServerError, resp.Error)
		return
	}

	type fileItem struct {
		Name    string `json:"name"`
		Path    string `json:"path"`
		IsDir   bool   `json:"is_dir"`
		Size    int64  `json:"size"`
		ModTime int64  `json:"mod_time"`
	}
	items := []fileItem{}
	for _, f := range resp.Files {
		items = append(items, fileItem{Name: f.Name, Path: f.Path, IsDir: f.IsDir, Size: f.Size, ModTime: f.ModTime})
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"keyword": keyword,
		"files":   items,
		"limit":   limit,
	})
}

// handleDownloadFile GET /api/instances/{id}/download?path=&token=
//
// 把 Daemon 的下载流原样转给浏览器。整条链路都是流式的：
// 不落临时文件、不在内存里攒整份文件，因此下载几个 GB 的世界存档
// 也不会让面板内存暴涨。
func (s *Server) handleDownloadFile(w http.ResponseWriter, r *http.Request) {
	instanceID := r.PathValue("id")
	if !s.requireInstanceLevel(w, r, instanceID, LevelCollab) {
		return
	}
	rel := r.URL.Query().Get("path")
	if strings.TrimSpace(rel) == "" {
		writeErr(w, http.StatusBadRequest, "缺少 path 参数")
		return
	}
	cli, _, err := s.getDaemonClient(instanceID)
	if err != nil {
		writeErr(w, http.StatusNotFound, "实例不存在")
		return
	}

	// 不设超时：下载时长完全取决于文件大小与用户带宽
	stream, err := cli.DownloadFile(context.Background(), &pb.DownloadFileRequest{
		InstanceId: instanceID, Path: rel,
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	var total int64
	// 先读第一片再决定响应头：这样"文件不存在/是目录"这类错误
	// 还能以 JSON 形式返回，而不是先发一个 200 再断流。
	first, err := stream.Recv()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "读取文件失败："+err.Error())
		return
	}
	total = first.Total

	name := pathBase(rel)
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", contentDisposition(name))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if total > 0 {
		w.Header().Set("Content-Length", strconv.FormatInt(total, 10))
	}

	write := func(b []byte) bool {
		if _, err := w.Write(b); err != nil {
			return false
		}
		if f, ok := w.(http.Flusher); ok {
			f.Flush() // 边读边发，避免大文件在缓冲区里"卡住不动"
		}
		return true
	}

	for {
		if len(first.Data) > 0 && !write(first.Data) {
			return
		}
		chunk, err := stream.Recv()
		if err == io.EOF {
			return
		}
		if err != nil {
			// 响应头已发出，无法再改成错误码；只能中断连接，
			// 浏览器会表现为下载失败 —— 这是流式下载的固有代价。
			s.logger.Warn("下载中断", "instance", instanceID, "path", rel, "error", err)
			return
		}
		if len(chunk.Data) > 0 && !write(chunk.Data) {
			return
		}
	}
}

// contentDisposition 生成兼容中文文件名的 Content-Disposition。
//
// 直接写 filename="中文" 在部分浏览器上会乱码，标准做法是同时给出
// ASCII 回退名与 RFC 5987 的 filename*。
func contentDisposition(name string) string {
	ascii := make([]rune, 0, len(name))
	for _, c := range name {
		if c < 128 && c != '"' && c != '\\' {
			ascii = append(ascii, c)
		} else {
			ascii = append(ascii, '_')
		}
	}
	return fmt.Sprintf("attachment; filename=%q; filename*=UTF-8''%s",
		string(ascii), url.PathEscape(name))
}

// pathBase 取路径最后一段（避免为了一个函数引入 path 包与局部变量重名）。
func pathBase(rel string) string {
	rel = strings.TrimRight(rel, "/")
	if i := strings.LastIndex(rel, "/"); i >= 0 {
		return rel[i+1:]
	}
	return rel
}
