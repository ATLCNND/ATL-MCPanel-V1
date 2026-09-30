package httpapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ATLCNND/ATL-MCPanel/internal/panel/analysis"
)

// 面板设置里的分析相关键。
//
// 为什么放在 panel_settings（而不是 config.yaml）：这些是"管理员在界面上随手一改
// 就该生效"的项 —— 顺序、内网开关、限速阈值。改配置文件+重启在现实中等于没人改。
const (
	settingAnalysisOrder       = "analysis_provider_order"
	settingAnalysisPrivate     = "analysis_allow_private"
	settingAnalysisRateMin     = "analysis_rate_per_min"
	settingAnalysisRateDay     = "analysis_rate_per_day"
	settingAnalysisRecommended = "analysis_show_free_tip"
)

// analysisSettings 一次读取全部分析设置（带默认值）。
type analysisSettings struct {
	Order        []string
	AllowPrivate bool
	RatePerMin   int
	RatePerDay   int
}

func (s *Server) analysisSettings() analysisSettings {
	out := analysisSettings{
		RatePerMin: analysis.DefaultPerMinute,
		RatePerDay: analysis.DefaultPerDay,
	}
	if v, ok := s.settingGet(settingAnalysisOrder); ok && strings.TrimSpace(v) != "" {
		for _, item := range strings.Split(v, ",") {
			if item = strings.TrimSpace(item); item != "" {
				out.Order = append(out.Order, item)
			}
		}
	}
	if v, ok := s.settingGet(settingAnalysisPrivate); ok {
		out.AllowPrivate = v == "true"
	}
	if v, ok := s.settingGet(settingAnalysisRateMin); ok {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			out.RatePerMin = n
		}
	}
	if v, ok := s.settingGet(settingAnalysisRateDay); ok {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			out.RatePerDay = n
		}
	}
	return out
}

// analysisSecretPath 分析平台主密钥的路径（与数据库同目录，随 data/ 一起备份）。
//
// 放在 data/ 下而不是 /etc：备份/迁移面板时密钥要跟着走，
// 否则把 data/ 拷到新机器后**已保存的 API Key 全部解不开**。
func analysisSecretPath(dataDir string) string {
	if strings.TrimSpace(dataDir) == "" {
		dataDir = "data"
	}
	return filepath.Join(dataDir, "analysis.key")
}

// ---- 提供方存储 ----

// scanProviders 读提供方列表（owner_id=0 的全局项 + 指定用户的私有项）。
//
// 不返回 api_key 密文：调用方要用 key 时走 providerByID（那里会解密），
// 列表接口只给 key_hint。
func (s *Server) listProviders(userID int64) ([]analysis.Provider, error) {
	rows, err := s.db.Query(`
		SELECT id, name, kind, base_url, model, COALESCE(key_hint,''), max_bytes,
		       timeout_sec, prompt, owner_id, enabled
		FROM analysis_providers
		WHERE owner_id = 0 OR owner_id = ?
		ORDER BY owner_id = 0 DESC, id ASC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []analysis.Provider{}
	for rows.Next() {
		var p analysis.Provider
		if err := rows.Scan(&p.ID, &p.Name, &p.Kind, &p.BaseURL, &p.Model, &p.KeyHint,
			&p.MaxBytes, &p.TimeoutSec, &p.Prompt, &p.OwnerID, &p.Enabled); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// builtinProviders 内置提供方（不落库，永远存在）。
//
// 为什么不入库：它们的可用性由代码与配置决定（LogShare 的开关、mclo.gs 的固定地址），
// 落成数据库行只会带来"管理员误删之后功能静默消失"这类问题。
// 顺序配置里可以用类型名（logshare / mclogs）引用它们。
func (s *Server) builtinProviders() []analysis.Provider {
	return []analysis.Provider{
		{
			ID: 0, Name: "LogShare", Kind: analysis.KindLogShare, Enabled: s.LogShareEnabled(),
			TimeoutSec: s.logShareCfg.TimeoutSeconds,
			BaseURL:    s.logShareCfg.SiteURL,
		},
		{
			ID: 0, Name: "mclo.gs", Kind: analysis.KindMclogs, Enabled: true,
			BaseURL: "https://api.mclo.gs",
		},
	}
}

// providerByID 读单个提供方**并解密 key**（仅内部使用：发起分析/连通性自检时）。
//
// id=0 表示内置提供方（LogShare / mclo.gs），由 kind 决定返回哪一个。
func (s *Server) providerByID(id int64, kind string, userID int64) (analysis.Provider, error) {
	if id == 0 {
		for _, p := range s.builtinProviders() {
			if p.Kind == kind {
				return p, nil
			}
		}
		return analysis.Provider{}, fmt.Errorf("未知的内置提供方：%s", kind)
	}
	var p analysis.Provider
	var enc []byte
	err := s.db.QueryRow(`
		SELECT id, name, kind, base_url, model, COALESCE(api_key_enc,''), COALESCE(key_hint,''),
		       max_bytes, timeout_sec, prompt, owner_id, enabled
		FROM analysis_providers WHERE id = ?`, id).
		Scan(&p.ID, &p.Name, &p.Kind, &p.BaseURL, &p.Model, &enc, &p.KeyHint,
			&p.MaxBytes, &p.TimeoutSec, &p.Prompt, &p.OwnerID, &p.Enabled)
	if err == sql.ErrNoRows {
		return analysis.Provider{}, fmt.Errorf("提供方不存在")
	}
	if err != nil {
		return analysis.Provider{}, err
	}
	// 私有提供方只能被本人（或管理员）使用
	if p.OwnerID != 0 && p.OwnerID != userID && roleOfID(s, userID) != RoleAdmin {
		return analysis.Provider{}, fmt.Errorf("无权使用该提供方")
	}
	if len(enc) > 0 {
		key, err := s.secretBox.Decrypt(enc)
		if err != nil {
			return analysis.Provider{}, err
		}
		p.APIKey = key
	}
	return p, nil
}

// roleOfID 查某个用户 id 的角色（用于"管理员可以动别人的私有配置"这类判断）。
func roleOfID(s *Server, userID int64) string {
	var role string
	if err := s.db.QueryRow(`SELECT role FROM users WHERE id = ?`, userID).Scan(&role); err != nil {
		return ""
	}
	return role
}

// ---- 接口：提供方 CRUD ----

type providerPayload struct {
	Name       string `json:"name"`
	Kind       string `json:"kind"`
	BaseURL    string `json:"base_url"`
	Model      string `json:"model"`
	APIKey     string `json:"api_key"`
	MaxBytes   int64  `json:"max_bytes"`
	TimeoutSec int    `json:"timeout_sec"`
	Prompt     string `json:"prompt"`
	Global     bool   `json:"global"` // 仅管理员可建全局
	Enabled    *bool  `json:"enabled"`
}

// canManageProviders 谁可以配置分析平台（D1：管理员 + 节点用户）。
//
// 普通用户只能用管理员配好的：给他们开自助配置，等于把"用户自己的 key 泄露/被滥用"
// 的责任揽到平台上，而他们本来也没有运维自助的需求。
func canManageProviders(r *http.Request) bool {
	return isAdmin(r) || isNodeUser(r)
}

// handleListAnalysisProviders GET /api/analysis/providers
func (s *Server) handleListAnalysisProviders(w http.ResponseWriter, r *http.Request) {
	uid := currentUserID(r)
	custom, err := s.listProviders(uid)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	st := s.analysisSettings()
	builtin := s.builtinProviders()

	// 链的实际顺序：让界面能显示"当前会按什么顺序尝试"
	var all []analysis.Provider
	all = append(all, builtin...)
	all = append(all, custom...)
	chain := analysis.BuildChain(st.Order, all, 0)
	order := make([]map[string]interface{}, 0, len(chain))
	for _, e := range chain {
		order = append(order, map[string]interface{}{
			"id":     e.Provider.ID,
			"kind":   e.Provider.Kind,
			"name":   e.Provider.Name,
			"reason": e.Reason,
		})
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"builtin":     builtin,
		"custom":      custom,
		"chain":       order,
		"can_manage":  canManageProviders(r),
		"logshare_on": s.LogShareEnabled(),
	})
}

// handleCreateAnalysisProvider POST /api/analysis/providers
func (s *Server) handleCreateAnalysisProvider(w http.ResponseWriter, r *http.Request) {
	if !canManageProviders(r) {
		writeErr(w, http.StatusForbidden, "只有总管理员或节点用户可以配置分析平台")
		return
	}
	var req providerPayload
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "无效请求体")
		return
	}
	if strings.TrimSpace(req.Name) == "" {
		writeErr(w, http.StatusBadRequest, "请填写名称（用于在界面上区分多个平台）")
		return
	}
	kind := strings.TrimSpace(req.Kind)
	if kind == "" {
		kind = analysis.KindOpenAI
	}
	if kind != analysis.KindOpenAI {
		writeErr(w, http.StatusBadRequest, "只能新建 OpenAI 兼容平台（LogShare 与 mclo.gs 是内置的）")
		return
	}
	if err := s.validateProviderTarget(r.Context(), req, currentUserID(r)); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	ownerID := currentUserID(r)
	// 只有总管理员能建"全局"提供方（所有人可见）
	if req.Global {
		if !isAdmin(r) {
			writeErr(w, http.StatusForbidden, "只有总管理员可以创建全局提供方")
			return
		}
		ownerID = 0
	}

	enc, err := s.secretBox.Encrypt(req.APIKey)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "加密 API Key 失败："+err.Error())
		return
	}
	timeout := req.TimeoutSec
	if timeout <= 0 {
		timeout = 300
	}
	enabled := 1
	if req.Enabled != nil && !*req.Enabled {
		enabled = 0
	}
	res, err := s.db.Exec(`
		INSERT INTO analysis_providers
			(name, kind, base_url, model, api_key_enc, key_hint, max_bytes, timeout_sec, prompt, owner_id, enabled)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		strings.TrimSpace(req.Name), kind, strings.TrimSpace(req.BaseURL), strings.TrimSpace(req.Model),
		enc, analysis.KeyHint(req.APIKey), req.MaxBytes, timeout, req.Prompt, ownerID, enabled)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "保存失败："+err.Error())
		return
	}
	id, _ := res.LastInsertId()
	// 审计里**不写 key**：只记"谁建了哪个平台"
	s.audit(r, "analysis_provider_create", strings.TrimSpace(req.Name),
		fmt.Sprintf("id=%d owner=%d model=%s base=%s", id, ownerID, req.Model, req.BaseURL))
	writeJSON(w, http.StatusCreated, map[string]interface{}{"id": id, "message": "已保存"})
}

// handleUpdateAnalysisProvider PUT /api/analysis/providers/{id}
func (s *Server) handleUpdateAnalysisProvider(w http.ResponseWriter, r *http.Request) {
	if !canManageProviders(r) {
		writeErr(w, http.StatusForbidden, "只有总管理员或节点用户可以配置分析平台")
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, http.StatusBadRequest, "提供方 ID 不合法")
		return
	}
	cur, err := s.providerByID(id, "", currentUserID(r))
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	if cur.OwnerID != 0 && cur.OwnerID != currentUserID(r) && !isAdmin(r) {
		writeErr(w, http.StatusForbidden, "只能修改自己的提供方")
		return
	}

	var req providerPayload
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "无效请求体")
		return
	}
	if err := s.validateProviderTarget(r.Context(), req, currentUserID(r)); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	// api_key 留空 = 不修改（用户只想改模型名时不必重填 key）
	sets := []string{"name = ?", "base_url = ?", "model = ?", "max_bytes = ?",
		"timeout_sec = ?", "prompt = ?", "updated_at = CURRENT_TIMESTAMP"}
	args := []interface{}{strings.TrimSpace(req.Name), strings.TrimSpace(req.BaseURL),
		strings.TrimSpace(req.Model), req.MaxBytes, maxInt(req.TimeoutSec, 1), req.Prompt}
	if strings.TrimSpace(req.APIKey) != "" {
		enc, err := s.secretBox.Encrypt(req.APIKey)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, "加密 API Key 失败："+err.Error())
			return
		}
		sets = append(sets, "api_key_enc = ?", "key_hint = ?")
		args = append(args, enc, analysis.KeyHint(req.APIKey))
	}
	if req.Enabled != nil {
		sets = append(sets, "enabled = ?")
		if *req.Enabled {
			args = append(args, 1)
		} else {
			args = append(args, 0)
		}
	}
	args = append(args, id)
	if _, err := s.db.Exec(`UPDATE analysis_providers SET `+strings.Join(sets, ", ")+` WHERE id = ?`, args...); err != nil {
		writeErr(w, http.StatusInternalServerError, "保存失败："+err.Error())
		return
	}
	s.audit(r, "analysis_provider_update", req.Name, fmt.Sprintf("id=%d（key %s）", id, keyChangeNote(req.APIKey)))
	writeJSON(w, http.StatusOK, map[string]interface{}{"message": "已保存"})
}

// handleDeleteAnalysisProvider DELETE /api/analysis/providers/{id}
func (s *Server) handleDeleteAnalysisProvider(w http.ResponseWriter, r *http.Request) {
	if !canManageProviders(r) {
		writeErr(w, http.StatusForbidden, "只有总管理员或节点用户可以配置分析平台")
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, http.StatusBadRequest, "提供方 ID 不合法")
		return
	}
	cur, err := s.providerByID(id, "", currentUserID(r))
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	if cur.OwnerID != 0 && cur.OwnerID != currentUserID(r) && !isAdmin(r) {
		writeErr(w, http.StatusForbidden, "只能删除自己的提供方")
		return
	}
	if _, err := s.db.Exec(`DELETE FROM analysis_providers WHERE id = ?`, id); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.audit(r, "analysis_provider_delete", cur.Name, fmt.Sprintf("id=%d", id))
	writeJSON(w, http.StatusOK, map[string]interface{}{"message": "已删除（连同保存的 API Key）"})
}

// validateProviderTarget 保存时的快速校验（D2：内网开关在这里生效）。
//
// 只做语法与字面 IP 检查，**不解析 DNS**：保存配置不该依赖"此刻 DNS 通不通"。
// 真正的出站防护在使用时（PreflightError）与 HTTP 客户端的重定向检查里。
func (s *Server) validateProviderTarget(ctx context.Context, req providerPayload, uid int64) error {
	if req.Kind != "" && req.Kind != analysis.KindOpenAI {
		return nil
	}
	if strings.TrimSpace(req.BaseURL) == "" {
		return fmt.Errorf("请填写平台地址（base_url）")
	}
	st := s.analysisSettings()
	if err := analysis.ValidateURLSyntax(req.BaseURL, st.AllowPrivate); err != nil {
		return fmt.Errorf("平台地址不被允许：%w", err)
	}
	return nil
}

// handleTestAnalysisProvider POST /api/analysis/providers/{id}/test
//
// 填完就点一下：key 错、地址少 /v1、模型名不对，只有当场看到原文才改得动。
func (s *Server) handleTestAnalysisProvider(w http.ResponseWriter, r *http.Request) {
	if !canManageProviders(r) {
		writeErr(w, http.StatusForbidden, "只有总管理员或节点用户可以测试分析平台")
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, http.StatusBadRequest, "提供方 ID 不合法")
		return
	}
	p, err := s.providerByID(id, "", currentUserID(r))
	if err != nil {
		writeErr(w, http.StatusNotFound, err.Error())
		return
	}
	st := s.analysisSettings()
	ctx, cancel := context.WithTimeout(r.Context(), 40*time.Second)
	defer cancel()

	if err := analysis.PreflightError(ctx, p, st.AllowPrivate); err != nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{"ok": false, "error": err.Error()})
		return
	}
	cli := analysis.NewOpenAIClient(analysis.OpenAIConfig{
		BaseURL: p.BaseURL, APIKey: p.APIKey, Model: p.Model, Prompt: p.Prompt,
		Timeout:         time.Duration(maxInt(p.TimeoutSec, 300)) * time.Second,
		AllowPrivateURL: st.AllowPrivate,
	})
	took, err := cli.Ping(ctx)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{"ok": false, "error": err.Error(), "took_ms": took.Milliseconds()})
		return
	}
	s.audit(r, "analysis_provider_test", p.Name, fmt.Sprintf("id=%d 连通（%dms）", p.ID, took.Milliseconds()))
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"ok": true, "took_ms": took.Milliseconds(),
		"message": fmt.Sprintf("连通成功，用时 %d ms（模型 %s）", took.Milliseconds(), p.Model),
	})
}

// ---- 接口：分析设置 ----

// handleGetAnalysisSettings GET /api/analysis/settings
func (s *Server) handleGetAnalysisSettings(w http.ResponseWriter, r *http.Request) {
	st := s.analysisSettings()
	perMin, perDay := s.rateLimiter.Limits()
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"order":            st.Order,
		"allow_private":    st.AllowPrivate,
		"rate_per_min":     perMin,
		"rate_per_day":     perDay,
		"can_manage":       isAdmin(r),
		"logshare_on":      s.LogShareEnabled(),
		"rate_default_min": analysis.DefaultPerMinute,
		"rate_default_day": analysis.DefaultPerDay,
	})
}

// handleSetAnalysisSettings PUT /api/analysis/settings（仅总管理员）
func (s *Server) handleSetAnalysisSettings(w http.ResponseWriter, r *http.Request) {
	if !isAdmin(r) {
		writeErr(w, http.StatusForbidden, "只有总管理员可以修改分析平台设置")
		return
	}
	var req struct {
		Order        *[]string `json:"order"`
		AllowPrivate *bool     `json:"allow_private"`
		RatePerMin   *int      `json:"rate_per_min"`
		RatePerDay   *int      `json:"rate_per_day"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "无效请求体")
		return
	}
	if req.Order != nil {
		var items []string
		for _, it := range *req.Order {
			if it = strings.TrimSpace(it); it != "" {
				items = append(items, it)
			}
		}
		if err := s.settingSet(settingAnalysisOrder, strings.Join(items, ",")); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	if req.AllowPrivate != nil {
		if err := s.settingSet(settingAnalysisPrivate, strconv.FormatBool(*req.AllowPrivate)); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	if req.RatePerMin != nil || req.RatePerDay != nil {
		cur := s.analysisSettings()
		perMin, perDay := cur.RatePerMin, cur.RatePerDay
		if req.RatePerMin != nil {
			perMin = *req.RatePerMin
		}
		if req.RatePerDay != nil {
			perDay = *req.RatePerDay
		}
		if perMin <= 0 || perDay <= 0 {
			writeErr(w, http.StatusBadRequest, "速率限制必须是正整数（每分钟 1~600、每天 1~100000）")
			return
		}
		if perMin > 600 || perDay > 100000 {
			writeErr(w, http.StatusBadRequest, "速率限制过大（每分钟上限 600、每天上限 100000）")
			return
		}
		if err := s.settingSet(settingAnalysisRateMin, strconv.Itoa(perMin)); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		if err := s.settingSet(settingAnalysisRateDay, strconv.Itoa(perDay)); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		// 立刻生效（限流器是常驻对象）
		s.rateLimiter.SetLimits(perMin, perDay)
	}
	s.audit(r, "analysis_settings_update", "", "更新了分析平台设置（顺序/内网开关/速率限制）")
	writeJSON(w, http.StatusOK, map[string]interface{}{"message": "设置已保存并立即生效"})
}

func keyChangeNote(key string) string {
	if strings.TrimSpace(key) == "" {
		return "未改动"
	}
	return "已更新"
}
