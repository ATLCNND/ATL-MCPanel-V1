package httpapi

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/ATLCNND/ATL-MCPanel/internal/common/grpclimits"
	pb "github.com/ATLCNND/ATL-MCPanel/internal/proto/mcpanel"
)

// ============================================================================
// 节点共享资源（管理员统一上传，节点上的实例复用）
// ============================================================================

// maxResourceUpload 上传体积上限，与 Daemon 侧、gRPC 消息上限共用同一常量。
const maxResourceUpload = grpclimits.MaxUploadBytes

type resourceView struct {
	Name       string   `json:"name"`
	Path       string   `json:"path"`
	Size       int64    `json:"size"`
	ModTime    int64    `json:"mod_time"`
	Refs       []string `json:"refs"`
	Referenced bool     `json:"referenced"`
}

// handleListNodeResources GET /api/nodes/{id}/resources
//
// 读权限放开给所有登录用户：他们需要知道"节点上有哪些现成核心可选"，
// 这与节点监控同理（只读、不含敏感信息）。
// 上传/删除仍然是管理员专属。
func (s *Server) handleListNodeResources(w http.ResponseWriter, r *http.Request) {
	nodeID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "节点 id 无效")
		return
	}
	cli, err := s.nodes.GetClient(nodeID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "节点不可达："+err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	resp, err := cli.ListResources(ctx, &pb.EmptyRequest{})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !resp.Success {
		// 未配置资源目录不算"错误"，而是"这台节点还没开放共享资源" ——
		// 用 200 + 空列表返回，前端据此显示引导语而不是红色报错
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"dir": resp.Dir, "files": []resourceView{}, "available": false, "message": resp.Error,
		})
		return
	}
	list := make([]resourceView, 0, len(resp.Files))
	for _, f := range resp.Files {
		list = append(list, resourceView{
			Name: f.Name, Path: f.Path, Size: f.Size, ModTime: f.ModTime,
			Refs: f.Refs, Referenced: len(f.Refs) > 0,
		})
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"dir": resp.Dir, "files": list, "available": true,
	})
}

// handleUploadNodeResource POST /api/nodes/{id}/resources （multipart，字段 file）
func (s *Server) handleUploadNodeResource(w http.ResponseWriter, r *http.Request) {
	if !requireAdminIn(w, r) {
		return
	}
	nodeID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "节点 id 无效")
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxResourceUpload+1<<20)
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		// 明确区分"超过上限"与"表单格式错误"：前者用户只要换个文件，
		// 后者要重新提交；而且必须把**具体上限**说出来，
		// 否则用户只能反复试（"文件过大或表单格式错误"就属于这种说不清的报错）。
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeErr(w, http.StatusRequestEntityTooLarge,
				fmt.Sprintf("文件过大，单次上传上限 %.0f MB（若文件确实更大，请先手动放到节点的资源目录再引用）",
					float64(maxResourceUpload)/1024/1024))
			return
		}
		writeErr(w, http.StatusBadRequest, "表单格式错误: "+err.Error())
		return
	}
	file, hdr, err := r.FormFile("file")
	if err != nil {
		writeErr(w, http.StatusBadRequest, "缺少 file 字段")
		return
	}
	defer file.Close()
	if hdr.Size > maxResourceUpload {
		writeErr(w, http.StatusRequestEntityTooLarge,
			fmt.Sprintf("文件过大（%.1f MB），单次上传上限 %.0f MB",
				float64(hdr.Size)/1024/1024, float64(maxResourceUpload)/1024/1024))
		return
	}

	content, err := io.ReadAll(file)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "读取上传内容失败: "+err.Error())
		return
	}

	cli, err := s.nodes.GetClient(nodeID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "节点不可达："+err.Error())
		return
	}
	// 上传可能上百 MB，超时给足
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	overwrite := r.FormValue("overwrite") == "1" || r.FormValue("overwrite") == "true"
	resp, err := cli.UploadResource(ctx, &pb.UploadResourceRequest{
		Filename:  hdr.Filename,
		Content:   content,
		Overwrite: overwrite,
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, friendlyUploadErr(err, int64(len(content))))
		return
	}
	if !resp.Success {
		// 按 Daemon 给出的 code 决定状态码，而不是靠 error 文案匹配 ——
		// 文案会改，匹配会静默失效（曾经就是因此把"扩展名不支持"也返回成了 409）。
		//
		// 不做"没有 code 就当成冲突"的兼容回退：面板与 Daemon 是同一次部署
		// 一起构建、一起重启的，回退分支只会把真正的失败误报成冲突。
		code := http.StatusInternalServerError
		if resp.Code == "name_conflict" {
			code = http.StatusConflict
		}
		writeErr(w, code, resp.Error)
		return
	}

	s.audit(r, "upload_resource", strconv.FormatInt(nodeID, 10),
		fmt.Sprintf("%s（%.1f MB）", hdr.Filename, float64(len(content))/1024/1024))
	writeJSON(w, http.StatusOK, map[string]string{"message": resp.Message})
}

// handleDeleteNodeResource DELETE /api/nodes/{id}/resources?name=&force=1
func (s *Server) handleDeleteNodeResource(w http.ResponseWriter, r *http.Request) {
	if !requireAdminIn(w, r) {
		return
	}
	nodeID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "节点 id 无效")
		return
	}
	name := r.URL.Query().Get("name")
	if name == "" {
		writeErr(w, http.StatusBadRequest, "缺少 name 参数")
		return
	}
	cli, err := s.nodes.GetClient(nodeID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "节点不可达："+err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	force := r.URL.Query().Get("force") == "1"
	resp, err := cli.DeleteResource(ctx, &pb.DeleteResourceRequest{Name: name, Force: force})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !resp.Success {
		// 被实例引用 → 409，前端据此提示"是否强制删除"
		code := http.StatusInternalServerError
		if !force {
			code = http.StatusConflict
		}
		writeErr(w, code, resp.Error)
		return
	}
	s.audit(r, "delete_resource", strconv.FormatInt(nodeID, 10), name)
	writeJSON(w, http.StatusOK, map[string]string{"message": resp.Message})
}

// handleListNodeJava GET /api/nodes/{id}/java
//
// 返回节点上**实际安装**的 JDK 列表，供创建实例时选择。
// 读权限对所有登录用户开放：创建实例的表单需要它。
func (s *Server) handleListNodeJava(w http.ResponseWriter, r *http.Request) {
	nodeID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "节点 id 无效")
		return
	}
	cli, err := s.nodes.GetClient(nodeID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "节点不可达："+err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	resp, err := cli.ListJavaRuntimes(ctx, &pb.EmptyRequest{})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	type rt struct {
		Label   string `json:"label"`
		Path    string `json:"path"`
		Home    string `json:"home"`
		Version string `json:"version"`
	}
	list := make([]rt, 0, len(resp.Runtimes))
	for _, x := range resp.Runtimes {
		list = append(list, rt{Label: x.Label, Path: x.Path, Home: x.Home, Version: x.Version})
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"runtimes": list,
		"fallback": resp.Fallback,
	})
}
