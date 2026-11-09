package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
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
		// enabled 保留为"还有任一平台可用"的兼容字段（老前端只认它）；
		// 新前端按 platforms 逐平台渲染开关。
		"enabled":        s.AnyThirdPartyEnabled(),
		"platforms":      s.platformSwitches(),
		"can_manage":     roleOf(r) == RoleAdmin,
		"retention_days": days,
		"site_url":       s.logShareCfg.SiteURL,
	})
}

// platformSwitches 两个内置平台的当前开关与说明（界面直接渲染成两行）。
//
// 为什么要连"说明"一起下发：这两个平台的数据流向完全不同 ——
// LogShare 是 AI 分析（要账号/公益额度），mclo.gs 是把日志变成**公开可访问**的链接。
// 界面上只写"启用/停用"会让人以为关掉的是同一件事。
func (s *Server) platformSwitches() []map[string]interface{} {
	return []map[string]interface{}{
		{
			"key": platformLogShare, "name": "LogShare（AI 分析）",
			"enabled": s.LogShareAIEnabled(), "is_ai": true,
			"note": "把日志上传到 api.logshare.cn 做 AI 分析，返回结论与建议",
		},
		{
			"key": platformMclogs, "name": "mclo.gs（公开分享）",
			"enabled": s.MclogsEnabled(), "is_ai": false,
			"note": "只把日志变成可分享的公开链接（不产生 AI 结论），链接无密码即可打开",
		},
	}
}

// PUT /api/logshare/settings  body: {"enabled": true|false}（仅总管理员）
//
// 兼容老前端：只有 enabled 字段时，两个平台一起开/关（与拆分前行为一致）。
// 新前端传 {"platform": "logshare"|"mclogs", "enabled": bool} 只改一个平台。
func (s *Server) handleSetLogShareSettings(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Enabled  *bool  `json:"enabled"`
		Platform string `json:"platform"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "无效请求体")
		return
	}
	if req.Enabled == nil {
		writeErr(w, http.StatusBadRequest, "缺少 enabled 字段")
		return
	}
	if s.logShare == nil {
		writeErr(w, http.StatusServiceUnavailable, "日志分析客户端未初始化")
		return
	}
	on := *req.Enabled
	affected := []string{}
	switch strings.TrimSpace(req.Platform) {
	case platformLogShare:
		if err := s.settingSet(settingLogShareAI, strconv.FormatBool(on)); err != nil {
			writeErr(w, http.StatusInternalServerError, "保存设置失败："+err.Error())
			return
		}
		affected = append(affected, "LogShare（AI 分析）")
	case platformMclogs:
		if err := s.settingSet(settingMclogsStore, strconv.FormatBool(on)); err != nil {
			writeErr(w, http.StatusInternalServerError, "保存设置失败："+err.Error())
			return
		}
		affected = append(affected, "mclo.gs（公开分享）")
	case "":
		// 老前端：一起改，并且把老的 logshare_enabled 也写一份，
		// 免得回滚到旧版本时两个平台的状态对不上。
		for _, k := range []string{settingLogShareAI, settingMclogsStore, settingLogShareEnabled} {
			if err := s.settingSet(k, strconv.FormatBool(on)); err != nil {
				writeErr(w, http.StatusInternalServerError, "保存设置失败："+err.Error())
				return
			}
		}
		affected = append(affected, "LogShare（AI 分析）", "mclo.gs（公开分享）")
	default:
		writeErr(w, http.StatusBadRequest, "未知的平台："+req.Platform)
		return
	}
	action := "logshare_disable"
	msg := "已关闭" + strings.Join(affected, "、") + "：面板上的对应入口将不可用，已上传的云端副本仍可删除"
	if on {
		action = "logshare_enable"
		msg = "已开启" + strings.Join(affected, "、")
	}
	s.audit(r, action, strings.TrimSpace(req.Platform), msg)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"message":   msg,
		"enabled":   on,
		"platform":  strings.TrimSpace(req.Platform),
		"platforms": s.platformSwitches(),
	})
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
