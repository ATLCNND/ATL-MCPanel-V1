package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"time"

	pb "github.com/ATLCNND/ATL-MCPanel/internal/proto/mcpanel"
)

// 面板设置（运行时开关）。
//
// 为什么这些开关要能在界面上改：它们都涉及**对外行为**（把日志发给第三方、给新实例
// 开容器隔离），管理员需要在几秒内开关，而不是"改配置文件 + 重启服务"。
// 现实中后者等于"没人关"。
//
// 存储：panel_settings 键值表；取值优先级 = 表里的记录 > config.yaml 默认值
//（见 Server.settingGet/LogShareEnabled）。这样老配置照旧生效，界面上改过之后以界面为准。

// GET /api/logshare/settings
//
// 任何登录用户都能读"当前是否启用"（前端要据此决定显示禁用态），
// 但只有总管理员能看到/使用开关 —— can_manage 就是给前端判断用的。
func (s *Server) handleGetLogShareSettings(w http.ResponseWriter, r *http.Request) {
	days := int64(logShareRetentionDefault) / 86400
	if s.logShare != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
		defer cancel()
		if d := s.logShareRetentionSeconds(ctx); d > 0 {
			days = d / 86400
		}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"enabled":        s.LogShareEnabled(),
		"can_manage":     roleOf(r) == RoleAdmin,
		"retention_days": days,
		"site_url":       s.logShareCfg.SiteURL,
	})
}

// PUT /api/logshare/settings  body: {"enabled": true|false}（仅总管理员）
func (s *Server) handleSetLogShareSettings(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "无效请求体")
		return
	}
	if s.logShare == nil {
		writeErr(w, http.StatusServiceUnavailable, "日志分析客户端未初始化")
		return
	}
	if err := s.settingSet(settingLogShareEnabled, strconv.FormatBool(req.Enabled)); err != nil {
		writeErr(w, http.StatusInternalServerError, "保存设置失败："+err.Error())
		return
	}
	action := "logshare_disable"
	msg := "已关闭第三方日志分析：面板上的入口将不可用，已上传的云端副本仍可删除"
	if req.Enabled {
		action = "logshare_enable"
		msg = "已开启第三方日志分析：用户可在实例的「日志分析」页上传日志并获取 AI 分析"
	}
	s.audit(r, action, "", msg)
	writeJSON(w, http.StatusOK, map[string]interface{}{"message": msg, "enabled": req.Enabled})
}

// GET /api/nodes/{id}/container
//
// 节点是否具备容器化能力（装了 docker 且已导入基础镜像）。
// 建实例的表单要用它决定"容器化"复选框的默认值与可用性 ——
// 否则用户勾了却在启动时才发现节点不支持。
func (s *Server) handleNodeContainerCapability(w http.ResponseWriter, r *http.Request) {
	nodeID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "节点 ID 不合法")
		return
	}
	cli, err := s.nodes.GetClient(nodeID)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "节点不可达："+err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	cap, err := cli.GetContainerCapability(ctx, &pb.EmptyRequest{})
	if err != nil {
		writeErr(w, http.StatusBadGateway, "查询节点容器化能力失败："+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"available":      cap.Available,
		"docker_present": cap.DockerPresent,
		"image_present":  cap.ImagePresent,
		"docker_version": cap.DockerVersion,
		"image":          cap.Image,
		"reason":         cap.Reason,
	})
}
