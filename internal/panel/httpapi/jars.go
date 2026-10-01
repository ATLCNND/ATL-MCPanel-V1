package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/ATLCNND/ATL-MCPanel/internal/common/grpclimits"
	pb "github.com/ATLCNND/ATL-MCPanel/internal/proto/mcpanel"
)

// maxJarUpload 上传 jar 的大小上限。
// 与节点共享资源、Daemon 侧限制、gRPC 消息上限共用同一常量（见 grpclimits）。
const maxJarUpload = grpclimits.MaxUploadBytes

// handleListJars GET /api/instances/{id}/jars
func (s *Server) handleListJars(w http.ResponseWriter, r *http.Request) {
	instanceID := r.PathValue("id")
	if !s.requireInstanceLevel(w, r, instanceID, LevelCollab) {
		return
	}
	cli, _, err := s.getDaemonClient(instanceID)
	if err != nil {
		writeErr(w, http.StatusNotFound, "实例不存在")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	resp, err := cli.ListJars(ctx, &pb.ListJarsRequest{InstanceId: instanceID})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !resp.Success {
		writeErr(w, http.StatusInternalServerError, resp.Error)
		return
	}

	type jarItem struct {
		Filename string `json:"filename"`
		Path     string `json:"path"`
		Size     int64  `json:"size"`
		Active   bool   `json:"active"`
	}
	items := []jarItem{}
	for _, j := range resp.Jars {
		items = append(items, jarItem{Filename: j.Filename, Path: j.Path, Size: j.Size, Active: j.Active})
	}
	// 当前核心（可能位于实例目录之外，例如共享 jar 目录）
	activeName := resp.ActiveJar
	if i := strings.LastIndexAny(activeName, "/\\"); i >= 0 {
		activeName = activeName[i+1:]
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"jars":       items,
		"active_jar": activeName,
	})
}

// handleUploadJar POST /api/instances/{id}/jars  （multipart/form-data，字段名 file）
func (s *Server) handleUploadJar(w http.ResponseWriter, r *http.Request) {
	instanceID := r.PathValue("id")
	if !s.requireInstanceLevel(w, r, instanceID, LevelOwner) {
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxJarUpload+1<<20)
	if err := r.ParseMultipartForm(16 << 20); err != nil {
		// 同 handleUploadNodeResource：把"超过上限"与"表单错误"分开，
		// 并把具体上限说出来，避免用户反复试。
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeErr(w, http.StatusRequestEntityTooLarge,
				fmt.Sprintf("jar 过大，单次上传上限 %.0f MB", float64(maxJarUpload)/1024/1024))
			return
		}
		writeErr(w, http.StatusBadRequest, "解析上传内容失败: "+err.Error())
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		writeErr(w, http.StatusBadRequest, "缺少 file 字段")
		return
	}
	defer file.Close()

	if !strings.HasSuffix(strings.ToLower(header.Filename), ".jar") {
		writeErr(w, http.StatusBadRequest, "仅支持 .jar 文件")
		return
	}
	if header.Size > maxJarUpload {
		writeErr(w, http.StatusRequestEntityTooLarge,
			fmt.Sprintf("文件过大（%d MB），上限 %d MB", header.Size>>20, maxJarUpload>>20))
		return
	}

	data, err := io.ReadAll(file)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "读取上传内容失败: "+err.Error())
		return
	}

	cli, _, err := s.getDaemonClient(instanceID)
	if err != nil {
		writeErr(w, http.StatusNotFound, "实例不存在")
		return
	}
	// 大文件上传给足时间（受网络与磁盘速度影响）
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	resp, err := cli.UploadJar(ctx, &pb.UploadJarRequest{
		InstanceId: instanceID,
		Filename:   header.Filename,
		Content:    data,
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "上传失败: "+friendlyUploadErr(err, int64(len(data))))
		return
	}
	if !resp.Success {
		writeErr(w, http.StatusInternalServerError, resp.Error)
		return
	}

	s.audit(r, "upload_jar", instanceID, fmt.Sprintf("%s (%d bytes)", header.Filename, len(data)))
	writeJSON(w, http.StatusOK, map[string]string{
		"message": resp.Message,
		"file":    header.Filename,
	})
}

// handleSetJar POST /api/instances/{id}/jar  {"jar_path":"folia-1.20.1.jar"}
func (s *Server) handleSetJar(w http.ResponseWriter, r *http.Request) {
	instanceID := r.PathValue("id")
	if !s.requireInstanceLevel(w, r, instanceID, LevelOwner) {
		return
	}
	var req struct {
		JarPath string `json:"jar_path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.JarPath) == "" {
		writeErr(w, http.StatusBadRequest, "jar_path 必填")
		return
	}

	cli, _, err := s.getDaemonClient(instanceID)
	if err != nil {
		writeErr(w, http.StatusNotFound, "实例不存在")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	resp, err := cli.SetInstanceJar(ctx, &pb.SetInstanceJarRequest{
		InstanceId: instanceID,
		JarPath:    req.JarPath,
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !resp.Success {
		writeErr(w, http.StatusBadRequest, resp.Error)
		return
	}

	s.audit(r, "switch_jar", instanceID, req.JarPath)
	writeJSON(w, http.StatusOK, map[string]string{"message": resp.Message})
}

// handleSetJava PUT /api/instances/{id}/java  {"java_version":"21"}
//
// 给**已有实例**换 JDK（2026-09-30 用户反馈："之前的实例内 JDK 修改没实现"——
// 确实没有：java_version 只在建实例时写进节点元数据，之后面板改不了）。
//
// 三件事必须一起做，否则会出现"面板显示改了、节点上还是旧的"：
//  1. 让 Daemon 改写 instance.json（那是节点侧的权威元数据）；
//  2. 更新面板数据库（列表页要显示新值）；
//  3. 审计里记一笔（换 JDK 会影响下次启动能不能起来，属于要能追溯的操作）。
//
// 空串 = 自动（用 PATH 上的 java）：这是有意义的取值，不是"没填"。
func (s *Server) handleSetJava(w http.ResponseWriter, r *http.Request) {
	instanceID := r.PathValue("id")
	if !s.requireInstanceLevel(w, r, instanceID, LevelOwner) {
		return
	}
	var req struct {
		JavaVersion string `json:"java_version"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "无效请求体")
		return
	}
	val := strings.TrimSpace(req.JavaVersion)
	if len([]rune(val)) > 512 {
		writeErr(w, http.StatusBadRequest, "JDK 取值过长")
		return
	}

	cli, _, err := s.getDaemonClient(instanceID)
	if err != nil {
		writeErr(w, http.StatusNotFound, "实例不存在或节点不可达")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	resp, err := cli.SetInstanceJava(ctx, &pb.SetInstanceJavaRequest{
		InstanceId:  instanceID,
		JavaVersion: val,
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !resp.Success {
		writeErr(w, http.StatusBadRequest, resp.Error)
		return
	}

	if _, err := s.db.Exec(`UPDATE instances SET java_version = ? WHERE instance_id = ?`, val, instanceID); err != nil {
		// 节点已改、面板库没改 → 必须让用户知道，否则列表页会一直显示旧值
		writeErr(w, http.StatusInternalServerError,
			"节点上的 JDK 已切换，但面板记录更新失败："+err.Error())
		return
	}

	shown := val
	if shown == "" {
		shown = "自动（PATH 上的 java）"
	}
	s.audit(r, "switch_java", instanceID, shown)
	writeJSON(w, http.StatusOK, map[string]string{"message": resp.Message, "java_version": val})
}
