package httpapi

import (
	"context"
	"encoding/json"
	"net/http"

	pb "github.com/ATLCNND/ATL-MCPanel/internal/proto/mcpanel"
)

// getDaemonClient 根据 instance_id 查 node 并返回 Daemon 客户端。
func (s *Server) getDaemonClient(instanceID string) (pb.DaemonServiceClient, int64, error) {
	var nodeID int64
	err := s.db.QueryRow(`SELECT node_id FROM instances WHERE instance_id = ?`, instanceID).Scan(&nodeID)
	if err != nil {
		return nil, 0, err
	}
	cli, err := s.nodes.GetClient(nodeID)
	return cli, nodeID, err
}

// handleListFiles GET /api/instances/{id}/files?path=xxx
func (s *Server) handleListFiles(w http.ResponseWriter, r *http.Request) {
	instanceID := r.PathValue("id")
	if !s.requireInstanceLevel(w, r, instanceID, LevelCollab) {
		return
	}
	path := r.URL.Query().Get("path")
	if path == "" {
		path = "/"
	}
	cli, _, err := s.getDaemonClient(instanceID)
	if err != nil {
		writeErr(w, http.StatusNotFound, "实例不存在")
		return
	}
	resp, err := cli.ListFiles(context.Background(), &pb.ListFilesRequest{InstanceId: instanceID, Path: path})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !resp.Success {
		writeErr(w, http.StatusInternalServerError, resp.Error)
		return
	}
	// 转换为前端友好的 JSON（含父目录标识）
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
	writeJSON(w, http.StatusOK, map[string]interface{}{"path": path, "files": items})
}

// handleReadFile GET /api/instances/{id}/file?path=xxx
func (s *Server) handleReadFile(w http.ResponseWriter, r *http.Request) {
	instanceID := r.PathValue("id")
	if !s.requireInstanceLevel(w, r, instanceID, LevelCollab) {
		return
	}
	path := r.URL.Query().Get("path")
	cli, _, err := s.getDaemonClient(instanceID)
	if err != nil {
		writeErr(w, http.StatusNotFound, "实例不存在")
		return
	}
	resp, err := cli.ReadFile(context.Background(), &pb.ReadFileRequest{InstanceId: instanceID, Path: path})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !resp.Success {
		writeErr(w, http.StatusInternalServerError, resp.Error)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"path": path, "content": resp.Content, "size": resp.Size})
}

// handleWriteFile POST /api/instances/{id}/file
func (s *Server) handleWriteFile(w http.ResponseWriter, r *http.Request) {
	instanceID := r.PathValue("id")
	if !s.requireInstanceLevel(w, r, instanceID, LevelOwner) {
		return
	}
	var req struct {
		Path    string `json:"path"`
		Content string `json:"content"`
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
	resp, err := cli.WriteFile(context.Background(), &pb.WriteFileRequest{InstanceId: instanceID, Path: req.Path, Content: req.Content})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !resp.Success {
		writeErr(w, http.StatusInternalServerError, resp.Error)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": resp.Message})
}

// handleDeleteFile DELETE /api/instances/{id}/file?path=xxx
func (s *Server) handleDeleteFile(w http.ResponseWriter, r *http.Request) {
	instanceID := r.PathValue("id")
	if !s.requireInstanceLevel(w, r, instanceID, LevelOwner) {
		return
	}
	path := r.URL.Query().Get("path")
	cli, _, err := s.getDaemonClient(instanceID)
	if err != nil {
		writeErr(w, http.StatusNotFound, "实例不存在")
		return
	}
	resp, err := cli.DeleteFile(context.Background(), &pb.FileRequest{InstanceId: instanceID, Path: path})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !resp.Success {
		writeErr(w, http.StatusInternalServerError, resp.Error)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": resp.Message})
}

// handleMkdir POST /api/instances/{id}/mkdir
func (s *Server) handleMkdir(w http.ResponseWriter, r *http.Request) {
	instanceID := r.PathValue("id")
	if !s.requireInstanceLevel(w, r, instanceID, LevelOwner) {
		return
	}
	var req struct {
		Path string `json:"path"`
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
	resp, err := cli.Mkdir(context.Background(), &pb.MkdirRequest{InstanceId: instanceID, Path: req.Path})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !resp.Success {
		writeErr(w, http.StatusInternalServerError, resp.Error)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": resp.Message})
}

// handleGetMetrics GET /api/instances/{id}/metrics
func (s *Server) handleGetMetrics(w http.ResponseWriter, r *http.Request) {
	instanceID := r.PathValue("id")
	if !s.requireInstanceLevel(w, r, instanceID, LevelViewer) {
		return
	}
	cli, _, err := s.getDaemonClient(instanceID)
	if err != nil {
		writeErr(w, http.StatusNotFound, "实例不存在")
		return
	}
	m, err := cli.GetMetrics(context.Background(), &pb.InstanceRequest{InstanceId: instanceID})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"cpu_percent":   m.CpuPercent,
		"mem_used":      m.MemUsed,
		"tps":           m.Tps,
		"players":       m.PlayersOnline,
		"players_max":   m.PlayersMax,
		"instance_id":   m.InstanceId,
		"threads":       m.Threads,
	})
}