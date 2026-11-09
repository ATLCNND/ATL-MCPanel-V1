package httpapi

import (
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ATLCNND/ATL-MCPanel/internal/panel/analysis"
)

// 实例级日志分析策略（1.0.0-preview.2 起）。
//
// 解决的问题：第三方日志分析原本只有**面板全局**一个（现在是两个）开关，
// 而实际部署里"这个实例的日志可以发出去、那个实例不行"是常态 ——
// 正式服满屏玩家聊天、测试服随便发，一刀切只能让管理员把整个功能关掉。
//
// 设计取舍：
//   - 三态而不是布尔：键不存在 = 跟随全局。这样既有"全局开、个别关"，
//     也能"全局关、只给某个实例开"，而且**升级前的实例行为完全不变**。
//   - 只存"与全局不同"的那些键。全局怎么变，实例自动跟随；
//     存一份完整的快照反而会让管理员改全局之后旧实例还按老快照跑。
//   - 平台键是动态的（内置两个 + 管理员随时增删的自建平台），所以用
//     JSON 对象存在一张独立表里，而不是给每个平台加一列。

// 内置平台在策略里的键（自建平台用 "openai:<id>"）。
const (
	platformLogShare = "logshare"
	platformMclogs   = "mclogs"
)

// platformKeyOf 某个提供方在实例策略里的键。
func platformKeyOf(p analysis.Provider) string {
	switch p.Kind {
	case analysis.KindLogShare:
		return platformLogShare
	case analysis.KindMclogs:
		return platformMclogs
	}
	if p.ID > 0 {
		return "openai:" + strconv.FormatInt(p.ID, 10)
	}
	return ""
}

// platformNameOf 界面展示名（策略里只存键，名字从提供方现取，避免两处不同步）。
func platformNameOf(p analysis.Provider) string {
	if strings.TrimSpace(p.Name) != "" {
		return p.Name
	}
	return platformKeyOf(p)
}

// instanceAnalysisPolicy 读一个实例的策略（不存在或为空时返回空表 = 全部跟随全局）。
//
// 读失败一律当"没有任何例外"处理：把策略读崩了不该顺带把全局开关也关掉，
// 也不该让整个日志分析页 500。
func (s *Server) instanceAnalysisPolicy(instanceID string) map[string]string {
	out := map[string]string{}
	var raw string
	if err := s.db.QueryRow(
		`SELECT policy FROM instance_analysis_policy WHERE instance_id = ?`, instanceID).Scan(&raw); err != nil {
		return out
	}
	if strings.TrimSpace(raw) == "" {
		return out
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return out
	}
	for k, v := range m {
		// 只接受 on/off；垃圾值一律忽略（等于"跟随全局"）
		if v == "on" || v == "off" {
			out[k] = v
		}
	}
	return out
}

// setInstanceAnalysisPolicy 保存策略（空表等于删除这一行）。
func (s *Server) setInstanceAnalysisPolicy(instanceID string, m map[string]string) error {
	clean := map[string]string{}
	for k, v := range m {
		if strings.TrimSpace(k) == "" {
			continue
		}
		if v == "on" || v == "off" {
			clean[k] = v
		}
	}
	if len(clean) == 0 {
		_, err := s.db.Exec(`DELETE FROM instance_analysis_policy WHERE instance_id = ?`, instanceID)
		return err
	}
	raw, err := json.Marshal(clean)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`INSERT INTO instance_analysis_policy (instance_id, policy, updated_at)
		VALUES (?, ?, ?)
		ON CONFLICT(instance_id) DO UPDATE SET policy = excluded.policy, updated_at = excluded.updated_at`,
		instanceID, string(raw), time.Now().UTC().Format(time.RFC3339))
	return err
}

// instancePlatformAllowed 这个实例上某个平台能不能用。
//
// 实例策略优先；没有显式设置时跟随全局（LogShareAIEnabled / MclogsEnabled）。
// 自建平台（openai:*）的全局可用性由 analysis_providers.enabled 决定，
// 所以这里对自建平台只做"实例禁用"的判断，返回 true 表示"全局层面不拦"。
func (s *Server) instancePlatformAllowed(instanceID, key string) bool {
	if key == "" {
		return false
	}
	switch s.instanceAnalysisPolicy(instanceID)[key] {
	case "on":
		return true
	case "off":
		return false
	}
	switch key {
	case platformLogShare:
		return s.LogShareAIEnabled()
	case platformMclogs:
		return s.MclogsEnabled()
	}
	// 自建平台：全局开关在 analysis_providers.enabled，链上已经按它过滤过
	return true
}

// instanceAnyPlatformAllowed 这个实例上是否还有**任何**一个可用的分析平台。
//
// 用于「日志分析」页的入口闸：两个平台都关时连候选文件都不必列出来。
func (s *Server) instanceAnyPlatformAllowed(instanceID string) bool {
	for _, st := range s.instancePlatformStatus(instanceID, 0) {
		if st.Effective {
			return true
		}
	}
	return false
}

// filterChainForInstance 按实例策略把链上的平台剔掉。
//
// 只做"实例明确禁止"这一步：全局开关与提供方自身的 enabled 已经在
// builtinProviders / BuildChain 里处理过，重复判断会让提示语变得难懂。
func (s *Server) filterChainForInstance(instanceID string, chain []analysis.ChainEntry) []analysis.ChainEntry {
	pol := s.instanceAnalysisPolicy(instanceID)
	if len(pol) == 0 {
		return chain
	}
	out := make([]analysis.ChainEntry, 0, len(chain))
	for _, e := range chain {
		if pol[platformKeyOf(e.Provider)] == "off" {
			continue
		}
		out = append(out, e)
	}
	return out
}

// platformStatus 界面上「这个实例每个分析平台能不能用」的一行。
type platformStatus struct {
	Key       string `json:"key"`
	Name      string `json:"name"`
	Kind      string `json:"kind"`
	IsAI      bool   `json:"is_ai"`
	Global    bool   `json:"global"`   // 面板全局开关（自建平台 = 该行 enabled）
	Override  string `json:"override"` // "" = 跟随全局，on / off
	Effective bool   `json:"effective"`
}

// instancePlatformStatus 列出该实例可用的平台与三态取值（顺带给界面用）。
func (s *Server) instancePlatformStatus(instanceID string, userID int64) []platformStatus {
	pol := s.instanceAnalysisPolicy(instanceID)
	providers := append(s.builtinProviders(), s.mustListProviders(userID)...)
	out := make([]platformStatus, 0, len(providers))
	for _, p := range providers {
		key := platformKeyOf(p)
		if key == "" {
			continue
		}
		st := platformStatus{
			Key: key, Name: platformNameOf(p), Kind: p.Kind, IsAI: p.IsAI(),
			Global: p.Enabled, Override: pol[key],
		}
		switch st.Override {
		case "on":
			st.Effective = true
		case "off":
			st.Effective = false
		default:
			st.Effective = st.Global
		}
		out = append(out, st)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// mustListProviders 读提供方列表，出错时返回空表（界面上少显示几行比整页 500 好）。
func (s *Server) mustListProviders(userID int64) []analysis.Provider {
	ps, err := s.listProviders(userID)
	if err != nil {
		return nil
	}
	return ps
}

// canViewInstanceAnalysisPolicy 判断能否看见这台实例的平台状态。
//
// 三拨人都要能看：
//   - 实例参与者（viewer 及以上）：他们要看"我的日志到底会不会被外发"；
//   - 管着这台实例所在节点的节点用户、以及总管理员：他们**能改**，
//     若看不到这块卡片，界面上就没有可改的地方。
//
// 写权限单独由 canManageInstance 判（见 handleSetInstanceAnalysisPolicy）。
func (s *Server) canViewInstanceAnalysisPolicy(userID int64, role, instanceID string) bool {
	if s.canManageInstance(userID, role, instanceID) {
		return true
	}
	level, ok := s.instanceLevel(userID, role, instanceID)
	return ok && levelAtLeast(level, LevelViewer)
}

// handleGetInstanceAnalysisPolicy GET /api/instances/{id}/analysis-policy
//
// 读：实例的参与者、以及能改它的管理员/节点用户都能看。
// 响应里的 `can_edit` 决定界面上那三个下拉能不能动 ——
// 与写接口的判断走同一个函数，避免"界面给点、点了 403"。
func (s *Server) handleGetInstanceAnalysisPolicy(w http.ResponseWriter, r *http.Request) {
	instanceID := r.PathValue("id")
	uid, role := currentUserID(r), roleOf(r)
	if !s.canViewInstanceAnalysisPolicy(uid, role, instanceID) {
		writeErr(w, http.StatusForbidden, "无权访问该实例")
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"instance_id": instanceID,
		"policy":      s.instanceAnalysisPolicy(instanceID),
		"platforms":   s.instancePlatformStatus(instanceID, uid),
		"can_edit":    s.canManageInstance(uid, role, instanceID),
	})
}

// handleSetInstanceAnalysisPolicy PUT /api/instances/{id}/analysis-policy
//
// body: {"policy": {"logshare": "off", "mclogs": "on", "openai:3": "off"}}
// 传空对象 = 清除所有例外（全部跟随全局）。
//
// 权限：**总管理员，或管着这台实例所在节点的节点用户**（`canManageInstance`）。
//
// 为什么不给实例 owner（这里原先判的是 `LevelOwner`，2026-11-09 用户要求收紧）：
// 这几个开关决定「这台实例的日志会不会被上传到第三方平台、要不要出 AI 结论」。
// 谁能关掉它，谁就能让别人的日志分析静默失效 —— 而面板上显示的还是"一切正常，
// 只是没分析出东西"。与容器化开关、每实例专用用户同级：属于平台侧设置，
// 由被影响方之外的人控制。
func (s *Server) handleSetInstanceAnalysisPolicy(w http.ResponseWriter, r *http.Request) {
	instanceID := r.PathValue("id")
	if !s.canManageInstance(currentUserID(r), roleOf(r), instanceID) {
		writeErr(w, http.StatusForbidden,
			"只有总管理员，或管着这台实例所在节点的节点用户能改这些开关")
		return
	}
	var req struct {
		Policy map[string]string `json:"policy"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "无效请求体")
		return
	}
	if err := s.setInstanceAnalysisPolicy(instanceID, req.Policy); err != nil {
		writeErr(w, http.StatusInternalServerError, "保存失败："+err.Error())
		return
	}
	s.audit(r, "set_instance_analysis_policy", instanceID, policySummary(req.Policy))
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"message":   "已保存：这个实例的日志分析开关只影响它自己",
		"policy":    s.instanceAnalysisPolicy(instanceID),
		"platforms": s.instancePlatformStatus(instanceID, currentUserID(r)),
	})
}

// policySummary 审计日志里的一句话摘要。
func policySummary(m map[string]string) string {
	if len(m) == 0 {
		return "全部跟随面板全局开关"
	}
	keys := make([]string, 0, len(m))
	for k, v := range m {
		keys = append(keys, k+"="+v)
	}
	sort.Strings(keys)
	return strings.Join(keys, ",")
}
