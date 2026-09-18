package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	pb "github.com/ATLCNND/ATL-MCPanel/internal/proto/mcpanel"
)

// 容器化隔离（按实例开关）。
//
// 为什么这个开关要有**独立的一组接口**、而不是塞进实例编辑里：
//   - 它是**安全属性**：决定实例进程能不能看见宿主、能不能连宿主回环服务。
//     面板上必须能直接看到"这台实例现在到底有没有隔离"，而不是靠管理员记得。
//   - 它的可用性依赖节点（装了 docker 且导入了基础镜像），所以要么能点、
//     要么给出**具体**原因，不能让开关看起来可点却点了没反应。
//
// 权限：owner 及以上。协作者能操作实例内容，但"是否与宿主隔离"属于平台侧
// 安全设置，不该由协作者改。

// handleGetContainer GET /api/instances/{id}/container
//
// 返回：该实例当前是否容器化、节点是否具备条件、不可用时的一句话原因。
func (s *Server) handleGetContainer(w http.ResponseWriter, r *http.Request) {
	instanceID := r.PathValue("id")
	// 读操作放宽到协作者：运行信息本来就对协作者可见，不算敏感
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

	resp := map[string]interface{}{
		"enabled":   false,
		"available": false,
		"running":   false,
		"reason":    "",
	}

	// 实例当前状态（元数据里的开关 + 正在跑的进程是不是容器）
	// 例：节点没装 docker 时这里会说清"节点不支持容器化"
	if rt, err := cli.GetInstanceRuntime(ctx, &pb.InstanceRequest{InstanceId: instanceID}); err == nil && rt.Success {
		resp["enabled"] = rt.Containerized
		resp["container_note"] = rt.ContainerNote
		resp["running"] = rt.Status == "running" || rt.Status == "starting"
		resp["status"] = rt.Status
	}

	// 节点能力
	if cap, err := cli.GetContainerCapability(ctx, &pb.EmptyRequest{}); err == nil {
		resp["available"] = cap.Available
		resp["docker_present"] = cap.DockerPresent
		resp["image_present"] = cap.ImagePresent
		resp["docker_version"] = cap.DockerVersion
		resp["image"] = cap.Image
		resp["reason"] = cap.Reason
	} else {
		resp["reason"] = "无法查询节点容器化能力：" + err.Error()
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleSetContainer PUT /api/instances/{id}/container
//
// body: {"enabled": true|false}
//
// 权限：**仅总管理员**（路由上挂 requireAdmin）。
//
// 为什么是管理员而不是实例 owner —— 这一条是这次改动里最要紧的判断：
// 容器化隔离限制的正是实例里的进程，而实例 owner 能在启动脚本、控制台、
// 插件里执行任意命令。**如果他自己就能关掉隔离**，那么"开了容器化"这件事
// 就只是他自己的一句承诺，随时可以撤回 —— 隔离必须由被隔离者之外的一方控制。
// 与"每实例专用用户""目录 0700"一样，这是平台侧设置，不是租户设置。
//
// 读操作（GET）仍开放给协作者与只读成员：他们能看到实例到底有没有隔离，
// 这属于运行信息，不算敏感。
//
// 校验都在 Daemon 侧（实例必须停止、节点必须有运行时），这里只把失败原因
// 原样回给前端 —— 那些原因正是用户需要看到的。
func (s *Server) handleSetContainer(w http.ResponseWriter, r *http.Request) {
	instanceID := r.PathValue("id")
	var req struct {
		Enabled bool `json:"enabled"`
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
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// 实例运行中不允许改：改了也不会立刻生效，而"界面显示改了、实际还在旧模式跑"
	// 会让管理员对隔离状态产生错误判断。Daemon 侧也挡了一道，这里提前给更清楚的提示。
	if rt, err := cli.GetInstanceRuntime(ctx, &pb.InstanceRequest{InstanceId: instanceID}); err == nil && rt.Success {
		if rt.Status == "running" || rt.Status == "starting" {
			writeErr(w, http.StatusConflict, "实例正在运行，请先停止实例再修改容器化设置")
			return
		}
	}

	resp, err := cli.SetInstanceContainer(ctx, &pb.SetInstanceContainerRequest{
		InstanceId: instanceID,
		Enabled:    req.Enabled,
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !resp.Success {
		writeErr(w, http.StatusBadRequest, resp.Error)
		return
	}

	action := "disable_container"
	if req.Enabled {
		action = "enable_container"
	}
	s.audit(r, action, instanceID, resp.Message)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"message": resp.Message,
		"enabled": req.Enabled,
	})
}
