package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ATLCNND/ATL-MCPanel/internal/panel/analysis"
	"github.com/ATLCNND/ATL-MCPanel/internal/panel/logshare"
	pb "github.com/ATLCNND/ATL-MCPanel/internal/proto/mcpanel"
)

// 统一的"发起分析"入口：按提供方链依次尝试，并说明每一次回退的原因。
//
// 为什么要有这条新链路（而不是继续在 logshare 那条上打补丁）：
// 现在链上有四类东西 —— 公益合作的 AI（LogShare）、只给分享链接的保底（mclo.gs）、
// 用户自配的平台、以及 V2 的本地规则。它们的返回形态完全不同，但**用户看到的
// 应该是同一件事**："这份日志现在怎么样了、结论在哪、下一步做什么"。

// analysisJob 一次待消费的分析（OpenAI 类提供方用：日志内容只在发起时存在于内存里）。
type analysisJob struct {
	ProviderID int64
	Kind       string
	InstanceID string
	RecordID   int64
	LogshareID string // logshare 类：对方的日志 id（重新拉流用它）
	// UserID 发起这次分析的用户。后台协程里没有请求上下文，
	// 而"私有提供方只能被本人使用"的检查需要它 —— 不带上就会在后台报"无权使用该提供方"。
	UserID    int64
	System    string
	User      string
	CreatedAt time.Time
}

// jobs 待消费的分析任务（key: 记录 id）。
//
// 为什么放内存：用户的 API Key 与日志内容**都不该因为"发起过一次分析"而落库**
// （D4b 的同一原则）。进程重启后这些任务消失，界面上会提示"重新发起"。
var (
	analysisJobsMu sync.Mutex
	analysisJobs   = map[int64]*analysisJob{}
)

func putAnalysisJob(j *analysisJob) {
	analysisJobsMu.Lock()
	defer analysisJobsMu.Unlock()
	// 顺手清理超过 30 分钟的旧任务，避免长期运行后内存里堆着没人取的日志
	for id, old := range analysisJobs {
		if time.Since(old.CreatedAt) > 30*time.Minute {
			delete(analysisJobs, id)
		}
	}
	analysisJobs[j.RecordID] = j
}

func takeAnalysisJob(recordID int64) (*analysisJob, bool) {
	analysisJobsMu.Lock()
	defer analysisJobsMu.Unlock()
	j, ok := analysisJobs[recordID]
	return j, ok
}

func dropAnalysisJob(recordID int64) {
	analysisJobsMu.Lock()
	defer analysisJobsMu.Unlock()
	delete(analysisJobs, recordID)
}

// analysisPrep 一次分析前的公共准备（读日志、截断、过滤聊天）。
type analysisPrep struct {
	Content   string
	Size      int64
	Truncated int64
	Filtered  int
	Path      string
}

// prepareAnalysis 读日志并做与 LogShare 链路一致的处理。
//
// 口径必须一致（截断保留尾部、聊天行过滤）：否则"同一份日志用不同提供方，
// 送到外面的内容不一样"，而这正是隐私承诺要盯住的地方。
func (s *Server) prepareAnalysis(cli pb.DaemonServiceClient, instanceID, path string, filterChat bool, maxBytes int64) (*analysisPrep, error) {
	content, size, err := s.readInstanceFileCapped(cli, instanceID, path, maxBytes)
	if err != nil {
		return nil, fmt.Errorf("读取日志失败：%w", err)
	}
	if strings.TrimSpace(content) == "" {
		return nil, fmt.Errorf("该文件是空的，没有可分析的内容")
	}
	p := &analysisPrep{Path: path, Size: size}
	if size > maxBytes {
		cut := size - maxBytes
		p.Truncated = cut
		if idx := strings.IndexByte(content, '\n'); idx >= 0 {
			content = content[idx+1:]
		}
		content = "（日志过大，已省略前部 " + humanBytes(cut) + "，以下为尾部）\n" + content
	}
	if filterChat {
		content, p.Filtered = stripPlayerChat(content)
	}
	p.Content = content
	return p, nil
}

// maxPhenomenonRunes 「现象」的最长字数。
//
// 它会被插进求助文本再发给第三方：不设上限的话，一段粘贴进来的
// 十万字聊天记录会跟着日志一起上传（既浪费对方额度，也不是用户的本意）。
const maxPhenomenonRunes = 2000

// clampPhenomenon 截断「现象」到上限（多出来的部分直接丢掉，不再报错）。
func clampPhenomenon(s string) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= maxPhenomenonRunes {
		return string(r)
	}
	return string(r[:maxPhenomenonRunes]) + "…（已截断）"
}

// handleHelpPreview POST /api/instances/{id}/analysis/help-preview
//
// body: {phenomenon?}
//
// 「上传之前先看看会生成什么」：确认弹窗里显示**渲染后的求助文本**，用户改「现象」时立刻跟着变。
//
// 为什么值得单独开一个接口：模板现在是管理员可改的，如果界面只显示模板原文
// （一堆 {占位符}），用户根本不知道自己最后会贴出去什么 —— 那正是"描述误区"的温床。
//
// 纯本地渲染：不出网、不计限流、不写审计。
func (s *Server) handleHelpPreview(w http.ResponseWriter, r *http.Request) {
	instanceID := r.PathValue("id")
	// 预览文本里带着实例的**元信息**（显示名/核心/Java/内存/运行方式），
	// 所以它必须和别的实例接口走同一道门。此前这里只有 requireAuth：
	// 任何登录用户拿任意 instance_id 都能读到别人实例的这些信息，
	// 而且"实例不存在"与"权限不足"的差别还顺带成了 id 枚举的探测器。
	if !s.requireInstanceLevel(w, r, instanceID, LevelViewer) {
		return
	}
	var req struct {
		Phenomenon string `json:"phenomenon"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req) // body 可以为空

	// 实例必须存在（否则给一段文本出来只会让人以为"预览成功了"）
	var exists int
	if err := s.db.QueryRow(`SELECT COUNT(1) FROM instances WHERE instance_id = ?`, instanceID).
		Scan(&exists); err != nil || exists == 0 {
		writeErr(w, http.StatusNotFound, "实例不存在")
		return
	}
	// 节点离线时**照样给预览**：环境信息大部分来自数据库，
	// 只有"运行方式"要靠 Daemon 上报 —— 为此整个预览失败太苛刻了。
	cli := cliFor(s, instanceID)
	info := s.instanceBrief(cli, instanceID)
	text := s.helpTextFor(info, clampPhenomenon(req.Phenomenon),
		"（上传成功后会填在这里）", "", 0)

	note := "日志链接、原文地址与 ERROR 行数要等上传完成后才会填进去。"
	if info["runtime"] == "" {
		note += "（该实例所在节点当前不在线，『运行方式』这一行暂时无法确定）"
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"text":           text,
		"template":       s.helpTemplate(),
		"is_default":     strings.TrimSpace(s.analysisSettings().HelpTemplate) == "",
		"pending_fields": []string{"{url}", "{raw_url}", "{errors}"},
		"note":           note,
	})
}

// handleInstanceAnalyse POST /api/instances/{id}/analyse
//
// body: {path, filter_chat, agree, provider_id?, phenomenon?}
//
// 返回：用了哪家、为什么回退、链接/错误行数、以及（mclo.gs 的）求助文本。
func (s *Server) handleInstanceAnalyse(w http.ResponseWriter, r *http.Request) {
	instanceID := r.PathValue("id")
	// 「把日志发出去」是比"看控制台"更强的动作 → owner 级（与上传到第三方一致）
	if !s.requireInstanceLevel(w, r, instanceID, LevelOwner) {
		return
	}
	// 总开关：第三方日志分析被关掉之后，这条链路上**任何**外发都不许发生。
	//
	// 为什么闸必须放在这里（而不是只靠 attemptLogShare 里那一句）：
	// LogShare 那一步失败之后会自动**回退到 mclo.gs** —— 于是"管理员已经关掉
	// 第三方分析"的部署里，日志（含未打码的 IP）仍然会被传到公开的 api.mclo.gs，
	// 而且生成的分享链接是公开的。开关失效比没有开关更糟：用户会以为数据没出门。
	// 措辞与 handleSetLogShareSettings 的提示保持一致（关的是上传入口，
	// 已经传出去的副本仍然能删，见 handleLogShareDelete）。
	if !s.LogShareEnabled() {
		writeErr(w, http.StatusForbidden,
			"第三方日志分析已被管理员关闭：日志不会上传到任何外部服务（已上传的云端副本仍可删除）")
		return
	}
	var req struct {
		Path string `json:"path"`
		// FilterChat 用 *bool：**没传**（nil）按"过滤"处理。
		//
		// 为什么不能是 plain bool：界面上这个勾默认是开着的，配置与文档也都承诺
		// "默认过滤玩家聊天"，但 bool 的零值是 false —— 任何不带这个字段的调用方
		//（老客户端、脚本、手工 curl）都会被当成"用户主动要求不过滤"，
		// 带着 <玩家名> 的聊天行就跟着日志一起上传了。
		// 只有**显式**的 false 才算关闭。
		FilterChat *bool  `json:"filter_chat"`
		Agree      bool   `json:"agree"`
		ProviderID int64  `json:"provider_id"`
		// ProviderKind 按**类型**指定内置提供方（logshare / mclogs）。
		//
		// 为什么需要它：内置的 LogShare 与 mclo.gs 都没有数据库行、id 都是 0 ——
		// 只靠 provider_id 区分不了"自动"与"指定某个内置提供方"（前端下拉里
		// 三者的 value 会撞在一起，选内置等于选自动）。
		ProviderKind string `json:"provider_kind"`
		Phenomenon   string `json:"phenomenon"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "无效请求体")
		return
	}
	if !req.Agree {
		writeErr(w, http.StatusBadRequest, "请先勾选同意：日志会上传到第三方，且可能包含玩家信息")
		return
	}
	if strings.TrimSpace(req.Path) == "" {
		writeErr(w, http.StatusBadRequest, "请选择要分析的日志文件")
		return
	}
	// 没传 filter_chat = 保持"过滤"（见字段注释）；显式 false 才关闭
	filterChat := filterChatEnabled(req.FilterChat)

	cli, _, err := s.getDaemonClient(instanceID)
	if err != nil {
		writeErr(w, http.StatusNotFound, "实例不存在")
		return
	}

	st := s.analysisSettings()
	custom, err := s.listProviders(currentUserID(r))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	var all []analysis.Provider
	all = append(all, s.builtinProviders()...)
	all = append(all, custom...)
	chain := analysis.BuildChain(st.Order, all, req.ProviderID)
	// 按类型指定内置提供方（id 都是 0，只能用类型区分；见 req.ProviderKind 的注释）
	if k := strings.TrimSpace(req.ProviderKind); k != "" && req.ProviderID == 0 {
		filtered := chain[:0:0]
		for _, e := range chain {
			if e.Provider.Kind == k {
				filtered = append(filtered, e)
			}
		}
		chain = filtered
		if len(chain) == 0 {
			writeErr(w, http.StatusBadRequest,
				"指定的分析提供方不可用（可能已被停用，或该类型不存在）")
			return
		}
	}
	if len(chain) == 0 {
		writeErr(w, http.StatusServiceUnavailable, "没有可用的分析提供方（可在「分析平台」里配置，或联系管理员）")
		return
	}

	// 先做公共准备（读日志），失败就没必要往后走
	prep, err := s.prepareAnalysis(cli, instanceID, req.Path, filterChat, s.logShareCfg.MaxUploadBytes)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	var attempts []map[string]interface{}
	for _, entry := range chain {
		p := entry.Provider
		// 列表查询**不带密文**（列表接口永不碰 key），所以这里按 id 重新取一次：
		// 只有走到"要真正调用这一家"时才解密，用完即弃。
		if p.Kind == analysis.KindOpenAI && p.ID > 0 {
			full, err := s.providerByID(p.ID, p.Kind, currentUserID(r))
			if err != nil {
				attempts = append(attempts, map[string]interface{}{"provider": p.Name, "error": err.Error()})
				continue
			}
			p = full
		}
		// 配置不全（缺 key/地址被拦）**不消耗限流额度**，也不回退 —— 那是用户要去改的
		if err := analysis.PreflightError(r.Context(), p, st.AllowPrivate); err != nil {
			attempts = append(attempts, map[string]interface{}{"provider": p.Name, "error": err.Error()})
			continue
		}
		// 限流：每一次对外部提供方的调用都计数（含回退后的第二次）
		if ok, wait, why := s.rateLimiter.Allow("u:" + strconv.FormatInt(currentUserID(r), 10)); !ok {
			w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
			writeErr(w, http.StatusTooManyRequests,
				fmt.Sprintf("%s（约 %d 秒后可再试）", why, int(wait.Seconds())+1))
			return
		}

		resp, err := s.attemptProvider(r, cli, instanceID, p, prep, clampPhenomenon(req.Phenomenon))
		if err != nil {
			attempts = append(attempts, map[string]interface{}{
				"provider": p.Name, "kind": p.Kind, "error": err.Error(), "reason": entry.Reason,
			})
			continue
		}
		// 成功：把"回退过"这件事也说清楚，而不是静默换一家
		resp["provider"] = map[string]interface{}{
			"id": p.ID, "kind": p.Kind, "name": p.Name, "is_ai": p.IsAI(),
			"describe": p.Describe(),
		}
		if len(attempts) > 0 {
			resp["fallbacks"] = attempts
			resp["fallback_note"] = fmt.Sprintf("已自动改用「%s」：%s", p.Name, attempts[len(attempts)-1]["error"])
		}
		writeJSON(w, http.StatusOK, resp)
		return
	}

	// 全链失败：把每一家的原因都给出来（只报最后一家会让人误以为只试了一家）
	msgs := make([]string, 0, len(attempts))
	for _, a := range attempts {
		msgs = append(msgs, fmt.Sprintf("%s：%v", a["provider"], a["error"]))
	}
	writeErr(w, http.StatusBadGateway, "所有分析提供方都失败了 —— "+strings.Join(msgs, "；"))
}

// attemptProvider 用某一家提供方做一次分析；成功返回给前端的响应体。
func (s *Server) attemptProvider(r *http.Request, cli pb.DaemonServiceClient, instanceID string,
	p analysis.Provider, prep *analysisPrep, phenomenon string) (map[string]interface{}, error) {

	ctx, cancel := context.WithTimeout(context.Background(),
		time.Duration(maxInt(s.logShareCfg.TimeoutSeconds, p.TimeoutSec))*time.Second)
	defer cancel()

	switch p.Kind {
	case analysis.KindLogShare:
		return s.attemptLogShare(r, cli, instanceID, p, prep, ctx)

	case analysis.KindMclogs:
		return s.attemptMclogs(r, instanceID, p, prep, phenomenon)

	case analysis.KindOpenAI:
		return s.attemptOpenAI(r, instanceID, p, prep)

	case analysis.KindBuiltinRules:
		return nil, fmt.Errorf("内置规则诊断将在 V2 提供（当前不可用）")
	}
	return nil, fmt.Errorf("未知的提供方类型 %q", p.Kind)
}

// attemptLogShare 走 LogShare：上传（含附加文件）→ 建记录 → 起后台 AI 流。
func (s *Server) attemptLogShare(r *http.Request, cli pb.DaemonServiceClient, instanceID string,
	p analysis.Provider, prep *analysisPrep, ctx context.Context) (map[string]interface{}, error) {

	if !s.LogShareEnabled() {
		return nil, fmt.Errorf("LogShare 未启用")
	}
	// 与原来一致：附带 latest.log 与最近一份崩溃报告（对方建议多文件上传）
	files := []logshare.UploadFile{}
	addFile := func(rel, name string) {
		if len(files) >= 4 {
			return
		}
		extra, _, err := s.readInstanceFileCapped(cli, instanceID, rel, s.logShareCfg.MaxUploadBytes)
		if err != nil || strings.TrimSpace(extra) == "" {
			return
		}
		if prep.Filtered > 0 {
			extra, _ = stripPlayerChat(extra)
		}
		files = append(files, logshare.UploadFile{Name: name, Content: extra})
	}
	sel := strings.ToLower(prep.Path)
	if !strings.HasSuffix(sel, "latest.log") {
		addFile("/logs/latest.log", "latest.log")
	}
	if !strings.Contains(sel, "crash-reports") {
		if list, err := s.listCrashReports(cli, instanceID, 1); err == nil {
			for _, rel := range list {
				addFile(rel, baseName(rel))
			}
		}
	}

	res, err := s.logShare.Upload(ctx, s.logShareVer, prep.Content, files)
	if err != nil {
		return nil, err
	}
	lines := strings.Count(prep.Content, "\n") + 1
	expires := time.Now().Add(time.Duration(s.logShareRetentionSeconds(ctx)) * time.Second)
	if m, err := s.logShare.GetMeta(ctx, res.ID); err == nil && m.Expires > 0 {
		expires = time.Unix(m.Expires, 0)
		lines = m.Lines
	}
	recordID := s.insertUploadRecord(uploadRecord{
		InstanceID: instanceID, RemoteID: res.ID, Token: res.Token, URL: res.URL,
		RawURL: res.Raw, SourcePath: prep.Path, Size: prep.Size, Lines: lines,
		Filtered: prep.Filtered, Truncated: prep.Truncated, UserID: currentUserID(r),
		Expires: expires, ProviderKind: analysis.KindLogShare, ProviderID: p.ID,
	})
	s.audit(r, "analysis_upload", instanceID,
		fmt.Sprintf("%s → %s（%s，过滤聊天 %d 行）", prep.Path, res.URL, humanBytes(prep.Size), prep.Filtered))

	// 后台跑 AI（不绑在这次请求上），前端随后用 /api/analysis/ai/{recordID} 看流
	putAnalysisJob(&analysisJob{
		ProviderID: p.ID, Kind: analysis.KindLogShare, InstanceID: instanceID,
		RecordID: recordID, LogshareID: res.ID, CreatedAt: time.Now(),
	})
	s.startAIAnalysis(instanceID, res.ID, recordID)

	return map[string]interface{}{
		"id":             res.ID,
		"record_id":      recordID,
		"provider_kind":  analysis.KindLogShare,
		"url":            res.URL,
		"raw_url":        res.Raw,
		"size":           prep.Size,
		"lines":          lines,
		"filtered_lines": prep.Filtered,
		"truncated":      prep.Truncated,
		"attached":       len(files),
		"expires_at":     expires.Format(time.RFC3339),
		"ai_available":   true,
	}, nil
}

// attemptMclogs 走保底通道：只把日志变成可分享链接，不做 AI。
//
// 返回体里带上**面板替用户拼好的求助文本**（含社区看不到的环境信息）——
// 这才是这个功能真正的落点（见 docs/ANALYSIS-PLUGGABLE.md 3.6.2）。
func (s *Server) attemptMclogs(r *http.Request, instanceID string,
	p analysis.Provider, prep *analysisPrep, phenomenon string) (map[string]interface{}, error) {

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	base := p.BaseURL
	if strings.TrimSpace(base) == "" {
		base = "https://api.mclo.gs"
	}
	cli := analysis.NewMclogsClient(base)

	// mclo.gs 有自己的上限（实测 /1/limits：10MB / 25000 行），比面板默认的 16MB 小 ——
	// 不在这里再截一次的话，稍大的日志会被对方直接拒（错误原文是 "Content is too long"），
	// 而用户看到的只是一句失败。这里按"尾部保留"再截一次，并把截断情况带进结果。
	content, cut := capForMclogs(prep.Content, mclogsMaxBytes, mclogsMaxLines)

	res, err := cli.Upload(ctx, content, s.logShareVer)
	if err != nil {
		return nil, err
	}
	expires := time.Now().Add(90 * 24 * time.Hour)
	if res.Expires > 0 {
		expires = time.Unix(res.Expires, 0)
	}
	recordID := s.insertUploadRecord(uploadRecord{
		InstanceID: instanceID, RemoteID: res.ID, Token: res.Token, URL: res.URL,
		RawURL: res.Raw, SourcePath: prep.Path, Size: prep.Size, Lines: res.Lines,
		Errors: res.Errors, Filtered: prep.Filtered, Truncated: prep.Truncated + cut,
		UserID: currentUserID(r), Expires: expires, ProviderKind: analysis.KindMclogs, ProviderID: p.ID,
	})
	s.audit(r, "analysis_upload", instanceID,
		fmt.Sprintf("%s → %s（分享链接，mclo.gs，ERROR %d 行）", prep.Path, res.URL, res.Errors))

	// 求助文本：把"社区看不到、但很可能是原因"的环境信息一起拼好。
	// 措辞来自**管理员配置的模板**（analysis_help_template），面板只负责填占位符。
	info := s.instanceBrief(cliFor(s, instanceID), instanceID)
	help := s.helpTextFor(info, phenomenon, res.URL, res.Raw, res.Errors)

	return map[string]interface{}{
		"id":            res.ID,
		"record_id":     recordID,
		"provider_kind": analysis.KindMclogs,
		"url":           res.URL,
		"raw_url":       res.Raw,
		"size":          prep.Size,
		"lines":         res.Lines,
		"errors":        res.Errors,
		"expires_at":    expires.Format(time.RFC3339),
		"ai_available":  false,
		"help_text":     help,
		// 界面必须写明这不是 AI 分析，避免用户误以为会拿到结论
		"notice": "mclo.gs 只提供**可分享的日志链接**与 ERROR 行计数，**不会给出 AI 结论**。" +
			"把上面的链接（连同求助文本）发到 MC 社区/模组作者/群里，请人帮你看看。",
	}, nil
}

// attemptOpenAI 走用户自配平台：建记录 + 起后台流式分析。
func (s *Server) attemptOpenAI(r *http.Request, instanceID string,
	p analysis.Provider, prep *analysisPrep) (map[string]interface{}, error) {

	// 记录先落库（拿到 recordID），AI 结论跑完后回填到 logshare_analyses
	recordID := s.insertUploadRecord(uploadRecord{
		InstanceID: instanceID, RemoteID: "", Token: "", URL: "", RawURL: "",
		SourcePath: prep.Path, Size: prep.Size, Lines: strings.Count(prep.Content, "\n") + 1,
		Filtered: prep.Filtered, Truncated: prep.Truncated, UserID: currentUserID(r),
		Expires: time.Time{}, ProviderKind: analysis.KindOpenAI, ProviderID: p.ID,
	})
	putAnalysisJob(&analysisJob{
		ProviderID: p.ID, Kind: analysis.KindOpenAI, InstanceID: instanceID, RecordID: recordID,
		UserID: currentUserID(r),
		System: openAISystemPrompt(), User: buildLogPrompt(prep), CreatedAt: time.Now(),
	})
	s.audit(r, "analysis_run", instanceID,
		fmt.Sprintf("%s → 自配平台「%s」（%s，模型 %s）", prep.Path, p.Name, humanBytes(prep.Size), p.Model))

	return map[string]interface{}{
		"record_id":      recordID,
		"provider_kind":  analysis.KindOpenAI,
		"size":           prep.Size,
		"lines":          strings.Count(prep.Content, "\n") + 1,
		"filtered_lines": prep.Filtered,
		"truncated":      prep.Truncated,
		"ai_available":   true,
	}, nil
}

// uploadRecord 一条上传/分析记录（provider_kind 决定它属于哪家）。
type uploadRecord struct {
	InstanceID   string
	RemoteID     string
	Token        string
	URL          string
	RawURL       string
	SourcePath   string
	Size         int64
	Lines        int
	Errors       int
	Filtered     int
	Truncated    int64
	UserID       int64
	Expires      time.Time
	ProviderKind string
	ProviderID   int64
}

// mclo.gs 的硬上限（实测 GET /1/limits：storageTime=90 天 / maxLength=10MiB / maxLines=25000）。
const (
	mclogsMaxBytes = 10 << 20
	mclogsMaxLines = 25000
)

// capForMclogs 把日志裁到对方能接受的大小，返回裁剪后的内容与被砍掉的字节数。
//
// 两条规则与 LogShare 那条链路一致：**保留尾部**（崩溃现场在后面）、
// 在文件头注明截断量（用户要能一眼看出"分析的不是完整日志"）。
// 行数超限时同样保留尾部 —— 只留前 25000 行会把最关键的崩溃现场丢掉。
func capForMclogs(content string, maxBytes int64, maxLines int) (string, int64) {
	var cut int64
	if int64(len(content)) > maxBytes {
		over := int64(len(content)) - maxBytes
		cut = over
		tail := content[over:]
		if i := strings.IndexByte(tail, '\n'); i >= 0 {
			tail = tail[i+1:]
		}
		content = "（日志过大，为适配 mclo.gs 的上限已省略前部 " + humanBytes(over) + "，以下为尾部）\n" + tail
	}
	if lines := strings.Count(content, "\n") + 1; lines > maxLines {
		all := strings.SplitN(content, "\n", lines)
		keep := all[lines-maxLines:]
		cut += int64(len(content) - len(strings.Join(keep, "\n")))
		content = "（日志行数超过 mclo.gs 上限，已省略前部 " + strconv.Itoa(lines-maxLines) + " 行）\n" +
			strings.Join(keep, "\n")
	}
	return content, cut
}

func (s *Server) insertUploadRecord(u uploadRecord) int64 {
	var exp interface{}
	if !u.Expires.IsZero() {
		exp = u.Expires
	}
	res, err := s.db.Exec(`
		INSERT INTO logshare_uploads
			(instance_id, logshare_id, token, url, raw_url, source_path, size, lines, errors,
			 filtered_lines, truncated, uploaded_by, expires_at, provider_kind, provider_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		u.InstanceID, u.RemoteID, u.Token, u.URL, u.RawURL, u.SourcePath, u.Size, u.Lines, u.Errors,
		u.Filtered, boolInt(u.Truncated > 0), u.UserID, exp, u.ProviderKind, u.ProviderID)
	if err != nil {
		// 落库失败不该让用户白等一次分析（日志已经传出去了），但**必须留痕** ——
		// 否则"这份日志去哪了、结论存哪"事后完全查不出来（LogShare 那条链路踩过这个坑）。
		log.Printf("[analysis] 上传记录落库失败（已忽略，不影响本次分析）：%v", err)
		return 0
	}
	id, _ := res.LastInsertId()
	return id
}

// handleAnalysisAI GET /api/analysis/ai/{record_id}
//
// 统一的 AI 流入口：按记录里的 provider_kind 决定从哪取流。
// 与原来那条 logshare 专用入口的关系：那条保留（老前端与兼容性），
// 新的界面走这条 —— 它多支持了"自配平台"。
func (s *Server) handleAnalysisAI(w http.ResponseWriter, r *http.Request) {
	recordID, err := strconv.ParseInt(r.PathValue("record_id"), 10, 64)
	if err != nil || recordID <= 0 {
		writeErr(w, http.StatusBadRequest, "记录 ID 不合法")
		return
	}
	rec, err := s.analysisRecordByID(recordID)
	if err != nil {
		writeErr(w, http.StatusNotFound, "记录不存在")
		return
	}
	if !s.requireInstanceLevel(w, r, rec.InstanceID, LevelCollab) {
		return
	}
	job, hasJob := takeAnalysisJob(recordID)

	// 已经有结论就直接回放：不重复消耗对方的额度，也让"再看一次"是瞬时的
	if cached := s.cachedAnalysis(rec.InstanceID, rec.CacheKey); strings.TrimSpace(cached) != "" {
		writeSSEAnswer(w, cached)
		return
	}

	if rec.ProviderKind == analysis.KindMclogs {
		writeErr(w, http.StatusBadRequest, "mclo.gs 只提供日志分享链接，不提供 AI 分析；请改用 LogShare 或自配平台")
		return
	}
	if rec.ProviderKind == analysis.KindOpenAI && !hasJob {
		// 进程重启或超过 30 分钟：内存里的日志已经没了，无法重跑
		writeErr(w, http.StatusGone, "本次分析的内容已不在内存中（面板重启或超时），请重新发起一次分析")
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "当前服务器不支持流式响应")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	key := fmt.Sprintf("rec:%d", recordID)
	run := s.aiRuns.attach(key, func() *aiRun { return &aiRun{subs: map[chan logshare.AIEvent]struct{}{}} },
		func(run *aiRun) {
			if rec.ProviderKind == analysis.KindLogShare {
				s.runAIRun(run, rec.InstanceID, rec.LogShareID, key)
				return
			}
			s.runOpenAIRun(run, job, key)
		})

	backlog, ch, detach := run.subscribe()
	defer detach()
	for _, ev := range backlog {
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Event, ev.Data); err != nil {
			return
		}
	}
	flusher.Flush()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-ch:
			if !ok {
				return
			}
			if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Event, ev.Data); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// runOpenAIRun 后台消费一次自配平台的分析。
//
// 与 runAIRun 同样的原则：脱离请求的 context（客户端断开也跑完并落库），
// 但因为日志内容只在内存里，**跑完即释放**（不落库）。
func (s *Server) runOpenAIRun(run *aiRun, job *analysisJob, key string) {
	go func() {
		defer s.aiRuns.release(key, run)
		if job == nil {
			run.push(logshare.AIEvent{Event: "error", Data: jsonString("分析内容已失效，请重新发起")})
			run.finish()
			return
		}
		defer dropAnalysisJob(job.RecordID)

		st := s.analysisSettings()
		// 用**发起者**的身份取提供方：私有提供方只能被本人使用，
		// 而这里跑在后台协程里、没有请求上下文（UserID 随任务带过来）。
		p, err := s.providerByID(job.ProviderID, job.Kind, job.UserID)
		if err != nil {
			run.push(logshare.AIEvent{Event: "error", Data: jsonString(err.Error())})
			run.finish()
			return
		}
		cli := analysis.NewOpenAIClient(analysis.OpenAIConfig{
			BaseURL: p.BaseURL, APIKey: p.APIKey, Model: p.Model, Prompt: p.Prompt,
			Timeout:         time.Duration(maxInt(p.TimeoutSec, 300)) * time.Second,
			AllowPrivateURL: st.AllowPrivate,
		})
		ctx, cancel := context.WithTimeout(context.Background(),
			time.Duration(maxInt(p.TimeoutSec, 300)+30)*time.Second)
		defer cancel()

		err = cli.AnalyzeStream(ctx, job.System, job.User, func(ev analysis.AIEvent) error {
			// 用 acceptText：正文已经是纯文本，不能再走"解析 JSON 形态"的 accept
			// （否则前端看得到结论、结论却不落库 —— 见 acceptText 的注释）
			run.acceptText(logshare.AIEvent{Event: ev.Event, Data: jsonString(ev.Data)}, ev.Data)
			return nil
		})
		if err != nil {
			run.push(logshare.AIEvent{Event: "error", Data: jsonString(err.Error())})
		}
		answer := run.finish()
		if strings.TrimSpace(answer) != "" {
			s.execLogged(`
				INSERT INTO logshare_analyses (instance_id, logshare_id, content) VALUES (?, ?, ?)
				ON CONFLICT(instance_id, logshare_id) DO UPDATE SET content = excluded.content,
					created_at = CURRENT_TIMESTAMP`,
				job.InstanceID, recKey(job.RecordID), answer)
		}
	}()
}

// recKey 自配平台没有远端 id，用 `local-<记录id>` 作为结论的关联键。
func recKey(recordID int64) string { return "local-" + strconv.FormatInt(recordID, 10) }

// cachedAnalysis 取已缓存的结论（自配平台按 local-<id> 存，LogShare 按远端 id 存）。
func (s *Server) cachedAnalysis(instanceID, logshareID string) string {
	var content string
	if err := s.db.QueryRow(
		`SELECT content FROM logshare_analyses WHERE instance_id = ? AND logshare_id = ?`,
		instanceID, logshareID).Scan(&content); err != nil {
		return ""
	}
	return content
}

type analysisRecord struct {
	ID           int64  `json:"id"`
	InstanceID   string `json:"instance_id"`
	LogShareID   string `json:"logshare_id"`
	URL          string `json:"url"`
	RawURL       string `json:"raw_url"`
	SourcePath   string `json:"source_path"`
	Size         int64  `json:"size"`
	Lines        int    `json:"lines"`
	Errors       int    `json:"errors"`
	Filtered     int    `json:"filtered_lines"`
	Truncated    bool   `json:"truncated"`
	CreatedAt    string `json:"created_at"`
	ExpiresAt    string `json:"expires_at"`
	Deleted      bool   `json:"deleted"`
	ProviderKind string `json:"provider_kind"`
	ProviderID   int64  `json:"provider_id"`
	Analysis     string `json:"analysis"`
	CacheKey     string `json:"cache_key"`
}

func (s *Server) analysisRecordByID(id int64) (*analysisRecord, error) {
	var rec analysisRecord
	var created, expires string
	err := s.db.QueryRow(`
		SELECT id, instance_id, logshare_id, url, COALESCE(raw_url,''), source_path, size, lines,
		       COALESCE(errors,0), filtered_lines, truncated,
		       COALESCE(created_at,''), COALESCE(expires_at,''), deleted_at IS NOT NULL,
		       COALESCE(provider_kind,'logshare'), COALESCE(provider_id,0)
		FROM logshare_uploads WHERE id = ?`, id).
		Scan(&rec.ID, &rec.InstanceID, &rec.LogShareID, &rec.URL, &rec.RawURL, &rec.SourcePath,
			&rec.Size, &rec.Lines, &rec.Errors, &rec.Filtered, &rec.Truncated,
			&created, &expires, &rec.Deleted, &rec.ProviderKind, &rec.ProviderID)
	if err != nil {
		return nil, err
	}
	rec.CreatedAt, rec.ExpiresAt = created, expires
	if rec.ProviderKind == analysis.KindOpenAI {
		rec.CacheKey = recKey(rec.ID)
	} else {
		rec.CacheKey = rec.LogShareID
	}
	rec.Analysis = s.cachedAnalysis(rec.InstanceID, rec.CacheKey)
	return &rec, nil
}

// handleInstanceAnalysisHistory GET /api/instances/{id}/analysis
func (s *Server) handleInstanceAnalysisHistory(w http.ResponseWriter, r *http.Request) {
	instanceID := r.PathValue("id")
	if !s.requireInstanceLevel(w, r, instanceID, LevelViewer) {
		return
	}
	rows, err := s.db.Query(`
		SELECT id, instance_id, logshare_id, url, COALESCE(raw_url,''), source_path, size, lines,
		       COALESCE(errors,0), filtered_lines, truncated,
		       COALESCE(created_at,''), COALESCE(expires_at,''), deleted_at IS NOT NULL,
		       COALESCE(provider_kind,'logshare'), COALESCE(provider_id,0)
		FROM logshare_uploads WHERE instance_id = ? ORDER BY id DESC LIMIT 20`, instanceID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()
	out := []analysisRecord{}
	for rows.Next() {
		var rec analysisRecord
		var created, expires string
		if err := rows.Scan(&rec.ID, &rec.InstanceID, &rec.LogShareID, &rec.URL, &rec.RawURL,
			&rec.SourcePath, &rec.Size, &rec.Lines, &rec.Errors, &rec.Filtered, &rec.Truncated,
			&created, &expires, &rec.Deleted, &rec.ProviderKind, &rec.ProviderID); err != nil {
			continue
		}
		rec.CreatedAt, rec.ExpiresAt = created, expires
		if rec.ProviderKind == analysis.KindOpenAI {
			rec.CacheKey = recKey(rec.ID)
		} else {
			rec.CacheKey = rec.LogShareID
		}
		rec.Analysis = s.cachedAnalysis(rec.InstanceID, rec.CacheKey)
		out = append(out, rec)
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"history": out,
		"limits": map[string]interface{}{
			"per_min": s.analysisSettings().RatePerMin,
			"per_day": s.analysisSettings().RatePerDay,
		},
	})
}

// handleDeleteAnalysisRecord DELETE /api/analysis/{record_id}
//
// 删除云端副本：**按 provider_kind 路由**（LogShare 用对方的 token，mclo.gs 用 Bearer 头）。
func (s *Server) handleDeleteAnalysisRecord(w http.ResponseWriter, r *http.Request) {
	recordID, err := strconv.ParseInt(r.PathValue("record_id"), 10, 64)
	if err != nil || recordID <= 0 {
		writeErr(w, http.StatusBadRequest, "记录 ID 不合法")
		return
	}
	rec, err := s.analysisRecordByID(recordID)
	if err != nil {
		writeErr(w, http.StatusNotFound, "记录不存在")
		return
	}
	if !s.requireInstanceLevel(w, r, rec.InstanceID, LevelOwner) {
		return
	}
	var token string
	if err := s.db.QueryRow(`SELECT token FROM logshare_uploads WHERE id = ?`, recordID).Scan(&token); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()

	switch rec.ProviderKind {
	case analysis.KindLogShare:
		if err := s.logShare.Delete(ctx, rec.LogShareID, token); err != nil {
			writeErr(w, http.StatusBadGateway, "删除失败："+err.Error())
			return
		}
	case analysis.KindMclogs:
		base := "https://api.mclo.gs"
		if p, err := s.providerByID(rec.ProviderID, analysis.KindMclogs, currentUserID(r)); err == nil && p.BaseURL != "" {
			base = p.BaseURL
		}
		if err := analysis.NewMclogsClient(base).Delete(ctx, rec.LogShareID, token); err != nil {
			writeErr(w, http.StatusBadGateway, "删除失败："+err.Error())
			return
		}
	case analysis.KindOpenAI:
		// 自配平台是"我们把日志发过去"，没有云端副本可删 —— 说清楚而不是假装删了
		writeErr(w, http.StatusBadRequest,
			"自配平台没有「云端副本」可删：日志是直接发给你自己配置的平台的，请到那边清理")
		return
	default:
		writeErr(w, http.StatusBadRequest, "该记录不支持删除云端副本")
		return
	}
	s.execLogged(`UPDATE logshare_uploads SET deleted_at = CURRENT_TIMESTAMP WHERE id = ?`, recordID)
	// 本地结论**必须一起删**，否则"删除我的数据"只做了一半：
	// logshare_analyses.content 通常整段引用日志原文（玩家名、聊天内容、绝对路径），
	// 只清远端副本等于"要求第三方删掉、自己在库里留一份"，而且此后每个
	// collab（甚至只是打开历史接口的人）都还能读到它。
	// 用 rec.CacheKey 定位那一行：LogShare 与 mclo.gs 就是远端日志 id，
	// 自配平台是 local-<id>（而自配平台在上面已经返回"没有云端副本可删"了）。
	s.execLogged(`DELETE FROM logshare_analyses WHERE instance_id = ? AND logshare_id = ?`,
		rec.InstanceID, rec.CacheKey)
	s.audit(r, "analysis_delete", rec.InstanceID,
		fmt.Sprintf("删除云端副本 %s（%s，含本地结论）", rec.URL, rec.ProviderKind))
	writeJSON(w, http.StatusOK, map[string]interface{}{"message": "云端副本与本地 AI 结论已删除"})
}

// ---- 小工具 ----

// openAISystemPrompt 送给自配平台的系统提示词。
//
// 要求它按"崩在哪/为什么/怎么修/证据行"来回答：用户要的是能照着改的东西，
// 不是一段泛泛的解释。同时明确让它**不要编造**日志里没有的东西。
func openAISystemPrompt() string {
	return "你是 Minecraft 服务端排障助手。用户会给你一段服务端日志。请：\n" +
		"1) 指出最可能的根因（如果无法确定，就说无法确定，不要编造）；\n" +
		"2) 引用日志里的**具体行**作为证据；\n" +
		"3) 给出可执行的修复步骤（改哪个文件、改成什么、为什么）；\n" +
		"4) 用简洁的中文回答，先给结论再给细节。"
}

// buildLogPrompt 把日志包成一条 user 消息（带上"尾部截断"这类上下文）。
func buildLogPrompt(prep *analysisPrep) string {
	var b strings.Builder
	b.WriteString("下面是服务器日志")
	if prep.Truncated > 0 {
		b.WriteString(fmt.Sprintf("（文件原始大小 %s，已省略前部 %s）", humanBytes(prep.Size), humanBytes(prep.Truncated)))
	}
	if prep.Filtered > 0 {
		b.WriteString(fmt.Sprintf("（已过滤 %d 行玩家聊天）", prep.Filtered))
	}
	b.WriteString("：\n\n```\n")
	b.WriteString(prep.Content)
	b.WriteString("\n```\n")
	return b.String()
}

// instanceBrief 取实例的一段"人类可读"信息（用于求助文本）。
func (s *Server) instanceBrief(cli pb.DaemonServiceClient, instanceID string) map[string]string {
	out := map[string]string{}
	var name, core, java, maxMem, memLimit string
	if err := s.db.QueryRow(
		`SELECT name, core_type, java_version, max_mem, COALESCE(mem_limit,'') FROM instances WHERE instance_id = ?`,
		instanceID).Scan(&name, &core, &java, &maxMem, &memLimit); err == nil {
		out["name"], out["core"], out["java"] = name, core, java
		out["mem"] = maxMem
		if memLimit != "" {
			out["mem"] = maxMem + "（cgroup 上限 " + memLimit + "）"
		}
	}
	if cli != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		if rt, err := cli.GetInstanceRuntime(ctx, &pb.InstanceRequest{InstanceId: instanceID}); err == nil && rt != nil {
			if rt.Containerized {
				note := "容器化运行"
				if rt.ContainerNote != "" {
					note = rt.ContainerNote
				}
				if memLimit != "" {
					note += "（容器内存上限 " + memLimit + "）"
				}
				out["runtime"] = note
			} else {
				out["runtime"] = "以普通进程运行在节点上"
			}
		}
	}
	return out
}

// cliFor 取实例所在节点的客户端（取不到就返回 nil，调用方要容忍）。
func cliFor(s *Server, instanceID string) pb.DaemonServiceClient {
	cli, _, err := s.getDaemonClient(instanceID)
	if err != nil {
		return nil
	}
	return cli
}

// writeSSEAnswer 把已缓存的结论当作一条流回放（用户"再看一次"时是瞬时的）。
func writeSSEAnswer(w http.ResponseWriter, answer string) {
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprintf(w, "event: status\ndata: %s\n\n", jsonString("cached"))
	_, _ = fmt.Fprintf(w, "event: content\ndata: %s\n\n", jsonString(answer))
	_, _ = fmt.Fprintf(w, "event: done\ndata: %s\n\n", jsonString("cached"))
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// baseName 取路径最后一段（避免再引 path 包）。
func baseName(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}

// startAIAnalysis 起一次 LogShare 的后台 AI 分析（记录 id 作为 key）。
func (s *Server) startAIAnalysis(instanceID, logshareID string, recordID int64) {
	key := fmt.Sprintf("rec:%d", recordID)
	s.aiRuns.attach(key, func() *aiRun { return &aiRun{subs: map[chan logshare.AIEvent]struct{}{}} },
		func(run *aiRun) { s.runAIRun(run, instanceID, logshareID, key) })
}
