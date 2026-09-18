package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ATLCNND/ATL-MCPanel/internal/panel/logshare"
	pb "github.com/ATLCNND/ATL-MCPanel/internal/proto/mcpanel"
)

// ---- LogShare.CN 日志分析 ----
//
// 这是**第三方免费服务**（https://logshare.cn）：把实例日志上传过去，
// 对方的 AI 给出崩溃根因与修复建议。因此这条链路有三条硬约束：
//
//  1. **默认关闭**（config.logshare.enabled），管理员显式打开才出现入口；
//  2. **每次上传都要用户手动勾选**同意对方的《服务协议》与《隐私政策》——
//     服务端同样校验这个勾选（agree 字段），不能只靠前端藏按钮；
//  3. 提供「过滤玩家聊天行」，默认开启：日志里有玩家名与聊天内容，
//     而对方的自动过滤只打码 IP（实测 /v1/filters 只有 IPv4/IPv6 规则）。
//
// 另外必须把上传返回的 token 落库：对方删除日志要求带 token，
// 丢了就再也删不掉那份含玩家数据的日志（官方文档明确要求自行持久化）。

// logShareFile 可分析的候选日志文件。
type logShareFile struct {
	Path    string `json:"path"`
	Name    string `json:"name"`
	Size    int64  `json:"size"`
	ModTime int64  `json:"mod_time"`
	Kind    string `json:"kind"` // crash / latest / console / rotated
}

// handleLogShareFiles GET /api/instances/{id}/logshare/files
//
// 列出"值得分析"的日志：崩溃报告、latest.log、console.log、以及轮转出来的
// 历史日志（服务端崩溃后往往只有轮转文件里还留着现场）。
func (s *Server) handleLogShareFiles(w http.ResponseWriter, r *http.Request) {
	instanceID := r.PathValue("id")
	if !s.requireInstanceLevel(w, r, instanceID, LevelCollab) {
		return
	}
	if !s.LogShareEnabled() {
		writeErr(w, http.StatusServiceUnavailable, "日志分析功能未启用（管理员可在面板配置中打开 logshare.enabled）")
		return
	}
	cli, _, err := s.getDaemonClient(instanceID)
	if err != nil {
		writeErr(w, http.StatusNotFound, "实例不存在")
		return
	}

	out := []logShareFile{}
	// 崩溃报告：crash-reports/*.txt（最新的排前面 —— 用户多半想分析刚崩的那次）
	if resp, err := cli.ListFiles(context.Background(), &pb.ListFilesRequest{InstanceId: instanceID, Path: "/crash-reports"}); err == nil && resp.Success {
		for _, f := range resp.Files {
			if f.IsDir || !strings.HasSuffix(strings.ToLower(f.Name), ".txt") {
				continue
			}
			out = append(out, logShareFile{
				Path: f.Path, Name: f.Name, Size: f.Size, ModTime: f.ModTime, Kind: "crash",
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ModTime > out[j].ModTime })

	// logs/ 下的主日志与轮转日志
	if resp, err := cli.ListFiles(context.Background(), &pb.ListFilesRequest{InstanceId: instanceID, Path: "/logs"}); err == nil && resp.Success {
		var rotated []logShareFile
		for _, f := range resp.Files {
			if f.IsDir {
				continue
			}
			name := strings.ToLower(f.Name)
			lower := name
			item := logShareFile{Path: f.Path, Name: f.Name, Size: f.Size, ModTime: f.ModTime, Kind: "rotated"}
			switch {
			case name == "latest.log":
				item.Kind = "latest"
				out = append(out, item)
			case name == "console.log":
				item.Kind = "console"
				out = append(out, item)
			case strings.HasSuffix(name, ".log.gz") || strings.HasSuffix(lower, ".log"):
				rotated = append(rotated, item)
			}
		}
		// 轮转日志只列最近 5 个：它们往往几百 KB 且内容重复，列太多反而干扰选择
		sort.Slice(rotated, func(i, j int) bool { return rotated[i].ModTime > rotated[j].ModTime })
		if len(rotated) > 5 {
			rotated = rotated[:5]
		}
		out = append(out, rotated...)
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"files": out,
		// 前端要把这三个地址展示出来（归因与"同意协议"都要用到）
		"site_url":    s.logShareCfg.SiteURL,
		"terms_url":   s.logShareCfg.TermsURL,
		"privacy_url": s.logShareCfg.PrivacyURL,
		"max_bytes":   s.logShareCfg.MaxUploadBytes,
		"history":     s.logShareHistory(instanceID),
	})
}

// handleLogShareAnalyse POST /api/instances/{id}/logshare/analyse
// body: {path, filter_chat, agree}
func (s *Server) handleLogShareAnalyse(w http.ResponseWriter, r *http.Request) {
	instanceID := r.PathValue("id")
	// 上传到第三方需要 owner 级：collab 能看控制台，但"把日志发出去"是更强的动作
	if !s.requireInstanceLevel(w, r, instanceID, LevelOwner) {
		return
	}
	if !s.LogShareEnabled() {
		writeErr(w, http.StatusServiceUnavailable, "日志分析功能未启用")
		return
	}
	var req struct {
		Path       string `json:"path"`
		FilterChat bool   `json:"filter_chat"`
		Agree      bool   `json:"agree"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "无效请求体")
		return
	}
	// 服务端也要校验勾选：前端藏按钮挡不住直接调接口的人，
	// 而"用户同意过"是这条链路唯一的合法性依据。
	if !req.Agree {
		writeErr(w, http.StatusBadRequest, "请先勾选同意 LogShare 的《服务协议》与《隐私政策》")
		return
	}
	if strings.TrimSpace(req.Path) == "" {
		writeErr(w, http.StatusBadRequest, "请选择要分析的日志文件")
		return
	}

	cli, _, err := s.getDaemonClient(instanceID)
	if err != nil {
		writeErr(w, http.StatusNotFound, "实例不存在")
		return
	}

	// 1. 读日志（流式，带上限：日志可能几 MB）
	content, size, err := s.readInstanceFileCapped(cli, instanceID, req.Path, s.logShareCfg.MaxUploadBytes)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "读取日志失败："+err.Error())
		return
	}
	if strings.TrimSpace(content) == "" {
		writeErr(w, http.StatusBadRequest, "该文件是空的，没有可分析的内容")
		return
	}

	// 2. 截断（保留**尾部**：崩溃现场在日志后面）
	truncated := int64(0)
	if size > s.logShareCfg.MaxUploadBytes {
		cut := size - s.logShareCfg.MaxUploadBytes
		truncated = cut
		// 按字节切后可能落在多字节字符中间，向前退到一个安全的换行处
		if idx := strings.IndexByte(content, '\n'); idx >= 0 {
			content = content[idx+1:]
		}
		content = "（日志过大，已省略前部 " + humanBytes(cut) + "，以下为尾部）\n" + content
	}

	// 3. 过滤玩家聊天（默认开）
	filtered := 0
	if req.FilterChat {
		content, filtered = stripPlayerChat(content)
	}

	// 4. 附加一份 latest.log 作为上下文：官方明确建议多文件上传
	//（崩溃报告只说"崩了什么"，latest.log 才知道"崩之前发生了什么"）。
	// 选中的就是 latest.log 时不再重复附加。
	files := []logshare.UploadFile{}
	if !strings.HasSuffix(strings.ToLower(req.Path), "latest.log") {
		if extra, _, err := s.readInstanceFileCapped(cli, instanceID, "/logs/latest.log", s.logShareCfg.MaxUploadBytes); err == nil && strings.TrimSpace(extra) != "" {
			if req.FilterChat {
				extra, _ = stripPlayerChat(extra)
			}
			files = append(files, logshare.UploadFile{Name: "latest.log", Content: extra})
		}
	}

	// 5. 上传
	ctx, cancel := context.WithTimeout(context.Background(),
		time.Duration(s.logShareCfg.TimeoutSeconds)*time.Second)
	defer cancel()

	res, err := s.logShare.Upload(ctx, s.logShareVer, content, files)
	if err != nil {
		s.audit(r, "logshare_upload_failed", instanceID, req.Path+"："+err.Error())
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}

	// 6. 落库（含 token —— 没有它以后删不掉）
	lines := strings.Count(content, "\n") + 1
	expires := time.Now().Add(15 * 24 * time.Hour) // 对方默认保留 15 天；拿到元信息后会用真实值覆盖
	if m, err := s.logShare.GetMeta(ctx, res.ID); err == nil && m.Expires > 0 {
		expires = time.Unix(m.Expires, 0)
		lines = m.Lines
	}
	_, _ = s.db.Exec(`
		INSERT INTO logshare_uploads
			(instance_id, logshare_id, token, url, source_path, size, lines,
			 filtered_lines, truncated, uploaded_by, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		instanceID, res.ID, res.Token, res.URL, req.Path, size, lines,
		filtered, boolInt(truncated > 0), currentUserID(r), expires)

	s.audit(r, "logshare_upload", instanceID,
		fmt.Sprintf("%s → %s（%s，过滤聊天 %d 行）", req.Path, res.URL, humanBytes(size), filtered))

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"id":             res.ID,
		"url":            res.URL,
		"size":           size,
		"lines":          lines,
		"filtered_lines": filtered,
		"truncated":      truncated,
		"attached":       len(files),
		"expires_at":     expires.Format(time.RFC3339),
	})
}

// handleLogShareAI GET /api/instances/{id}/logshare/ai/{logshare_id}
//
// SSE 代理：把对方的 AI 分析流原样转给浏览器。
//
// 为什么不让前端直连 api.logshare.cn：① 跨域（对方未声明 CORS）；
// ② 令牌与限流都该在服务端统一处理；③ 顺便把最终结论落库。
//
// **分析跑在后台、不绑在这一次 HTTP 请求上**（见 aiRunHub）：
// 用户中途切页/关页时，分析照样跑完并落库，下次打开直接看结论；
// 同一份日志被多个页面同时打开时，也只会消耗对方一次 AI。
func (s *Server) handleLogShareAI(w http.ResponseWriter, r *http.Request) {
	instanceID := r.PathValue("id")
	if !s.requireInstanceLevel(w, r, instanceID, LevelCollab) {
		return
	}
	if !s.LogShareEnabled() {
		writeErr(w, http.StatusServiceUnavailable, "日志分析功能未启用")
		return
	}
	logID := r.PathValue("logshare_id")
	if logID == "" {
		writeErr(w, http.StatusBadRequest, "缺少 logshare_id")
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "当前服务器不支持流式响应")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no") // 反代下禁用缓冲，否则流会被攒住
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	run := s.aiRuns.attach(
		instanceID+"/"+logID,
		func() *aiRun { return &aiRun{subs: map[chan logshare.AIEvent]struct{}{}} },
		func(r *aiRun) { s.runAIRun(r, instanceID, logID, instanceID+"/"+logID) },
	)

	// 先补发已经产生的事件（后加入的页面也能看到完整过程）
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
			// 客户端走了：**不取消上游**，后台继续跑并落库
			return
		case ev, ok := <-ch:
			if !ok {
				// 上游结束
				return
			}
			if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Event, ev.Data); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// ---- 后台 AI 分析：一次运行、多个订阅者、结果落库 ----

// aiRun 一次正在进行（或已完成）的 AI 分析。
type aiRun struct {
	mu     sync.Mutex
	events []logshare.AIEvent // 全量事件（回放给后加入的订阅者）
	subs   map[chan logshare.AIEvent]struct{}
	closed bool
	answer strings.Builder
}

const aiSubBuf = 2048

// accept 收下一个上游事件：转发给订阅者，并攒下最终结论。
//
// 只攒 content 增量、不攒 thinking：结论要落库给用户看，
// 思考过程只是过程展示（前端把它收在折叠块里）。
func (r *aiRun) accept(ev logshare.AIEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	// 已经结束时什么都不做：否则落库结论会被后到的事件追加上半句
	// （runAIRun 是 finish() 之后立刻写库，这个窗口虽小但确实存在）。
	if r.closed {
		return
	}
	r.appendLocked(ev)
	if ev.Event != "status" {
		return
	}
	var m struct {
		Type  string `json:"type"`
		Delta string `json:"delta"`
	}
	if json.Unmarshal([]byte(ev.Data), &m) != nil || m.Type != "content" || m.Delta == "" {
		return
	}
	r.answer.WriteString(m.Delta)
}

func (r *aiRun) subscribe() ([]logshare.AIEvent, chan logshare.AIEvent, func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	backlog := append([]logshare.AIEvent(nil), r.events...)
	ch := make(chan logshare.AIEvent, aiSubBuf)
	if r.closed {
		close(ch)
		return backlog, ch, func() {}
	}
	r.subs[ch] = struct{}{}
	return backlog, ch, func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		if _, ok := r.subs[ch]; ok {
			delete(r.subs, ch)
			close(ch)
		}
	}
}

func (r *aiRun) push(ev logshare.AIEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	r.appendLocked(ev)
}

// appendLocked 记事件并广播（调用方必须已持锁）。
func (r *aiRun) appendLocked(ev logshare.AIEvent) {
	r.events = append(r.events, ev)
	for ch := range r.subs {
		select {
		case ch <- ev:
		default:
			// 订阅者彻底跟不上（浏览器卡死等）：**断开它**而不是丢掉事件，
			// 免得它拿到一份残缺的结论还以为是完整的。它重新打开页面时，
			// 会从 backlog（仍在跑）或已落库的结论（已跑完）拿到完整内容。
			delete(r.subs, ch)
			close(ch)
		}
	}
}

func (r *aiRun) finish() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed = true
	for ch := range r.subs {
		delete(r.subs, ch)
		close(ch)
	}
	return r.answer.String()
}

// aiRunHub 正在进行的分析（key: instanceID + "/" + logshareID）。
type aiRunHub struct {
	mu   sync.Mutex
	runs map[string]*aiRun
}

// attach 复用或新建一次分析。**先入表再启动**：否则 start 里跑得极快的
// 失败分支会先 release，把一个已结束的 run 永久留在表里。
func (h *aiRunHub) attach(key string, newRun func() *aiRun, launch func(*aiRun)) *aiRun {
	h.mu.Lock()
	if h.runs == nil {
		h.runs = map[string]*aiRun{}
	}
	if run, ok := h.runs[key]; ok {
		h.mu.Unlock()
		return run
	}
	run := newRun()
	h.runs[key] = run
	h.mu.Unlock() // 先放锁再启动，免得 goroutine 里的 release 白等

	launch(run)
	return run
}

func (h *aiRunHub) release(key string, run *aiRun) {
	h.mu.Lock()
	defer h.mu.Unlock()
	// 只清理自己那一次：同一 key 可能已经被后来的一次分析占用
	if cur, ok := h.runs[key]; ok && cur == run {
		delete(h.runs, key)
	}
}

// runAIRun 后台消费一次分析。
//
// 用**脱离请求的 context**（带自己的超时）：客户端断开不会打断分析，
// 结论仍会落库；用户下次打开直接看到结果，也不会重复消耗对方的 AI 资源。
func (s *Server) runAIRun(run *aiRun, instanceID, logID, key string) {
	go func() {
		defer s.aiRuns.release(key, run)

		ctx, cancel := context.WithTimeout(context.Background(),
			time.Duration(s.logShareCfg.TimeoutSeconds+30)*time.Second)
		defer cancel()

		err := s.logShare.StreamAI(ctx, logID, func(ev logshare.AIEvent) error {
			run.accept(ev)
			return nil
		})

		if err != nil {
			// 已经进入 SSE 阶段，只能用事件报错
			run.push(logshare.AIEvent{Event: "error", Data: jsonString(err.Error())})
		}
		answer := run.finish()
		if strings.TrimSpace(answer) != "" {
			_, _ = s.db.Exec(`
				INSERT INTO logshare_analyses (instance_id, logshare_id, content) VALUES (?, ?, ?)
				ON CONFLICT(instance_id, logshare_id) DO UPDATE SET content = excluded.content,
					created_at = CURRENT_TIMESTAMP`,
				instanceID, logID, answer)
		}
	}()
}

// handleLogShareDelete DELETE /api/instances/{id}/logshare/{logshare_id}
//
// 删除云端副本（隐私用）。**必须带 token**，所以要读库。
func (s *Server) handleLogShareDelete(w http.ResponseWriter, r *http.Request) {
	instanceID := r.PathValue("id")
	if !s.requireInstanceLevel(w, r, instanceID, LevelOwner) {
		return
	}
	if !s.LogShareEnabled() {
		writeErr(w, http.StatusServiceUnavailable, "日志分析功能未启用")
		return
	}
	logID := r.PathValue("logshare_id")

	var token string
	if err := s.db.QueryRow(
		`SELECT token FROM logshare_uploads WHERE instance_id = ? AND logshare_id = ?`,
		instanceID, logID).Scan(&token); err != nil {
		writeErr(w, http.StatusNotFound, "没有这条上传记录（可能是别的实例上传的）")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := s.logShare.Delete(ctx, logID, token); err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	_, _ = s.db.Exec(
		`UPDATE logshare_uploads SET deleted_at = CURRENT_TIMESTAMP WHERE instance_id = ? AND logshare_id = ?`,
		instanceID, logID)
	s.audit(r, "logshare_delete", instanceID, logID)
	writeJSON(w, http.StatusOK, map[string]string{"message": "云端副本已删除"})
}

// handleLogShareHistory GET /api/instances/{id}/logshare
// 已上传记录（供界面显示"这份日志分析过了"与删除入口）。
func (s *Server) handleLogShareHistory(w http.ResponseWriter, r *http.Request) {
	instanceID := r.PathValue("id")
	if !s.requireInstanceLevel(w, r, instanceID, LevelCollab) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"enabled": s.LogShareEnabled(),
		"history": s.logShareHistory(instanceID),
	})
}

type logShareRecord struct {
	ID            int64  `json:"id"`
	LogShareID    string `json:"logshare_id"`
	URL           string `json:"url"`
	SourcePath    string `json:"source_path"`
	Size          int64  `json:"size"`
	Lines         int    `json:"lines"`
	FilteredLines int    `json:"filtered_lines"`
	Truncated     bool   `json:"truncated"`
	CreatedAt     string `json:"created_at"`
	ExpiresAt     string `json:"expires_at"`
	Deleted       bool   `json:"deleted"`
	// Analysis 已经拿到的 AI 结论（Markdown）。有值时前端直接展示，
	// 不必再向对方请求一次（省钱省时间，也少一次隐私暴露）。
	Analysis string `json:"analysis"`
}

func (s *Server) logShareHistory(instanceID string) []logShareRecord {
	rows, err := s.db.Query(`
		SELECT u.id, u.logshare_id, u.url, u.source_path, u.size, u.lines,
		       u.filtered_lines, u.truncated,
		       COALESCE(u.created_at,''), COALESCE(u.expires_at,''),
		       u.deleted_at IS NOT NULL,
		       COALESCE(a.content,'')
		FROM logshare_uploads u
		LEFT JOIN logshare_analyses a
		  ON a.instance_id = u.instance_id AND a.logshare_id = u.logshare_id
		WHERE u.instance_id = ?
		ORDER BY u.id DESC LIMIT 20`, instanceID)
	if err != nil {
		return []logShareRecord{}
	}
	defer rows.Close()
	out := []logShareRecord{}
	for rows.Next() {
		var it logShareRecord
		var created, expires string
		if err := rows.Scan(&it.ID, &it.LogShareID, &it.URL, &it.SourcePath, &it.Size,
			&it.Lines, &it.FilteredLines, &it.Truncated, &created, &expires,
			&it.Deleted, &it.Analysis); err != nil {
			continue
		}
		it.CreatedAt = created
		it.ExpiresAt = expires
		out = append(out, it)
	}
	return out
}

// ---- 工具 ----

// readInstanceFileCapped 通过 Daemon 读文件内容，最多读 max 字节。
//
// 用流式 DownloadFile 而不是 ReadFile：后者在 Daemon 侧有 1MB 上限，
// 而 latest.log 经常超过它（几 MB 很常见）。这里按字节累加、到上限就停，
// 不把整份大日志读进内存。
func (s *Server) readInstanceFileCapped(cli pb.DaemonServiceClient, instanceID, path string, max int64) (string, int64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	stream, err := cli.DownloadFile(ctx, &pb.DownloadFileRequest{InstanceId: instanceID, Path: path})
	if err != nil {
		return "", 0, err
	}
	var buf strings.Builder
	var total int64
	for {
		chunk, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			if buf.Len() > 0 {
				break // 已经读到一部分就用这部分（日志尾部通常更有用，但流式只能顺序读）
			}
			return "", 0, err
		}
		total += int64(len(chunk.Data))
		if buf.Len() < int(max) {
			buf.Write(chunk.Data)
		}
	}
	return buf.String(), total, nil
}

// stripPlayerChat 去掉玩家聊天行，返回新内容与被过滤的行数。
//
// 只删"玩家发出的聊天"，不删加入/退出/成就等事件 —— 那些对诊断崩溃有用
// （"这个玩家一进来就崩"正是要靠它看出来）。
//
// 覆盖三种真实形态：
//
//	[10:01:10] [Server thread/INFO]: <Steve> 你好
//	[10:01:10] [Server thread/INFO]: [Not Secure] <Steve> 你好
//	[10:01:10 INFO]: <Steve> hi
func stripPlayerChat(content string) (string, int) {
	lines := strings.Split(content, "\n")
	out := make([]string, 0, len(lines))
	filtered := 0
	for _, line := range lines {
		if isChatLine(line) {
			filtered++
			continue
		}
		out = append(out, line)
	}
	head := ""
	if filtered > 0 {
		// 明确写一行说明：让 AI 知道"这里原本有聊天，被主动去掉了"，
		// 否则它可能把"缺少上下文"当成日志异常
		head = fmt.Sprintf("# （已按用户要求过滤 %d 行玩家聊天内容）\n", filtered)
	}
	return head + strings.Join(out, "\n"), filtered
}

// chatLineRe 匹配"玩家聊天"行：`<名字> 内容`，名字不含尖括号与换行，长度受限。
//
// 用 <...> 作为判据是服务端自己的格式（所有核心都这么打），
// 而不是靠关键词猜 —— 靠猜会把 `<` 出现在报错里的行也删掉。
var chatLineRe = regexp.MustCompile(`(?:^|\]\s*)(?:\[Not Secure\]\s*)?<[^<>\n]{1,32}>\s`)

func isChatLine(line string) bool {
	return chatLineRe.MatchString(line)
}

// jsonString 把字符串编成 JSON（用于 SSE 的 data 行，避免换行破坏协议）。
func jsonString(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		return `""`
	}
	return string(b)
}

// humanBytes 字节数转人话（与 fileops 的口径一致，但那个包在 daemon 侧）。
func humanBytes(n int64) string {
	switch {
	case n >= 1<<30:
		return fmt.Sprintf("%.2f GB", float64(n)/(1<<30))
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	default:
		return fmt.Sprintf("%d B", n)
	}
}
