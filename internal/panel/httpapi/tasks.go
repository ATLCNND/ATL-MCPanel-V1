package httpapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/ATLCNND/ATL-MCPanel/internal/panel/cron"
	pb "github.com/ATLCNND/ATL-MCPanel/internal/proto/mcpanel"
)

// 定时任务支持的动作用类型。
//
// 之所以仅限这几种而不是"任意 shell"：这些动作都能由 Daemon 用
// 结构化接口完成，权限边界清晰、审计可读；而任意 shell 等于把
// 节点上的 root 权限通过 Web 界面开放出去，风险与收益完全不成比例。
const (
	taskActionStart   = "start"
	taskActionStop    = "stop"
	taskActionRestart = "restart"
	taskActionKill    = "kill"
	taskActionCommand = "command"
)

var taskActionLabels = map[string]string{
	taskActionStart:   "开机",
	taskActionStop:    "关机",
	taskActionRestart: "重启",
	taskActionKill:    "强制关闭",
	taskActionCommand: "游戏指令",
}

// isTaskAction 校验动作类型。
func isTaskAction(a string) bool {
	_, ok := taskActionLabels[a]
	return ok
}

// taskView 定时任务的对外视图。
type taskView struct {
	ID         int64  `json:"id"`
	InstanceID string `json:"instance_id"`
	Name       string `json:"name"`
	Action     string `json:"action"`
	ActionLabel string `json:"action_label"`
	Command    string `json:"command"`
	Cron       string `json:"cron"`
	Describe   string `json:"describe"`
	Enabled    bool   `json:"enabled"`

	NextRun   string `json:"next_run"`
	LastRun   string `json:"last_run"`
	LastState string `json:"last_state"`
	LastError string `json:"last_error"`
	RunCount  int    `json:"run_count"`
	FailCount int    `json:"fail_count"`
	CreatedBy int64  `json:"created_by"`
	Creator   string `json:"creator"`
}

// handleListTasks GET /api/instances/{id}/tasks
//
// 读取权限放到 viewer：看得到"这台机器什么时候会自动重启"是使用者的
// 合理知情需求，且不含任何敏感信息。
func (s *Server) handleListTasks(w http.ResponseWriter, r *http.Request) {
	instanceID := r.PathValue("id")
	if !s.requireInstanceLevel(w, r, instanceID, LevelViewer) {
		return
	}
	rows, err := s.db.Query(`
		SELECT t.id, t.instance_id, t.name, t.action, t.command, t.cron, t.enabled,
		       t.last_run, t.last_state, t.last_error, t.run_count, t.fail_count,
		       t.created_by, COALESCE(u.username, '')
		FROM instance_tasks t
		LEFT JOIN users u ON u.id = t.created_by
		WHERE t.instance_id = ?
		ORDER BY t.id`, instanceID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()

	now := time.Now()
	list := []taskView{}
	for rows.Next() {
		var v taskView
		var enabled int
		var lastRun sql.NullTime
		if err := rows.Scan(&v.ID, &v.InstanceID, &v.Name, &v.Action, &v.Command, &v.Cron,
			&enabled, &lastRun, &v.LastState, &v.LastError, &v.RunCount, &v.FailCount,
			&v.CreatedBy, &v.Creator); err != nil {
			continue
		}
		v.Enabled = enabled == 1
		v.ActionLabel = taskActionLabels[v.Action]
		v.Describe = cron.Describe(v.Cron)
		if lastRun.Valid {
			v.LastRun = lastRun.Time.Format(time.RFC3339)
		}
		if v.Enabled {
			if sched, err := cron.Parse(v.Cron); err == nil {
				if next := sched.Next(now); !next.IsZero() {
					v.NextRun = next.Format(time.RFC3339)
				}
			}
		}
		list = append(list, v)
	}
	writeJSON(w, http.StatusOK, list)
}

// handleCreateTask POST /api/instances/{id}/tasks
//
// 权限为 owner：定时任务会在无人值守时自动开关机、下发指令，
// 影响面远大于单次点击操作，因此不给协作者（collab）开放。
// 但**不要求管理员** —— 实例归属者理应能管理自己服务器的作息。
func (s *Server) handleCreateTask(w http.ResponseWriter, r *http.Request) {
	instanceID := r.PathValue("id")
	if !s.requireInstanceLevel(w, r, instanceID, LevelOwner) {
		return
	}
	var req struct {
		Name    string `json:"name"`
		Action  string `json:"action"`
		Command string `json:"command"`
		Cron    string `json:"cron"`
		Enabled *bool  `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "无效请求体")
		return
	}

	action := strings.TrimSpace(strings.ToLower(req.Action))
	if !isTaskAction(action) {
		writeErr(w, http.StatusBadRequest, "不支持的动作类型："+req.Action)
		return
	}
	spec := strings.TrimSpace(req.Cron)
	if _, err := cron.Parse(spec); err != nil {
		writeErr(w, http.StatusBadRequest, "cron 表达式无效："+err.Error())
		return
	}
	cmd := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(req.Command), "/"))
	if action == taskActionCommand && cmd == "" {
		writeErr(w, http.StatusBadRequest, "「游戏指令」必须填写要下发的指令内容")
		return
	}
	if action != taskActionCommand {
		cmd = "" // 非指令类动作不保留 command，避免误用
	}

	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = defaultTaskName(action, cmd, spec)
	}

	enabled := 1
	if req.Enabled != nil && !*req.Enabled {
		enabled = 0
	}
	userID := currentUserID(r)

	res, err := s.db.Exec(`
		INSERT INTO instance_tasks (instance_id, name, action, command, cron, enabled, created_by)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		instanceID, name, action, cmd, spec, enabled, userID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	id, _ := res.LastInsertId()
	s.audit(r, "create_task", instanceID,
		fmt.Sprintf("task=%d action=%s cron=%s command=%s", id, action, spec, cmd))
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"message": "定时任务已创建：" + cron.Describe(spec),
		"id":      id,
	})
}

// handleUpdateTask PUT /api/tasks/{id}
func (s *Server) handleUpdateTask(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "id 无效")
		return
	}
	instanceID, ok := s.taskInstanceID(id)
	if !ok {
		writeErr(w, http.StatusNotFound, "任务不存在")
		return
	}
	if !s.requireInstanceLevel(w, r, instanceID, LevelOwner) {
		return
	}

	var req struct {
		Name    *string `json:"name"`
		Action  *string `json:"action"`
		Command *string `json:"command"`
		Cron    *string `json:"cron"`
		Enabled *bool   `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "无效请求体")
		return
	}

	// 读出现值，做"部分更新"：前端只改启用状态时不必回传整个对象
	var (
		name, action, command, spec string
		enabled                     int
	)
	if err := s.db.QueryRow(
		`SELECT name, action, command, cron, enabled FROM instance_tasks WHERE id = ?`, id).
		Scan(&name, &action, &command, &spec, &enabled); err != nil {
		writeErr(w, http.StatusNotFound, "任务不存在")
		return
	}

	if req.Name != nil {
		name = strings.TrimSpace(*req.Name)
	}
	if req.Action != nil {
		a := strings.TrimSpace(strings.ToLower(*req.Action))
		if !isTaskAction(a) {
			writeErr(w, http.StatusBadRequest, "不支持的动作类型："+*req.Action)
			return
		}
		action = a
	}
	if req.Cron != nil {
		c := strings.TrimSpace(*req.Cron)
		if _, err := cron.Parse(c); err != nil {
			writeErr(w, http.StatusBadRequest, "cron 表达式无效："+err.Error())
			return
		}
		spec = c
	}
	if req.Command != nil {
		command = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(*req.Command), "/"))
	}
	if req.Enabled != nil {
		if *req.Enabled {
			enabled = 1
		} else {
			enabled = 0
		}
	}
	if action != taskActionCommand {
		command = ""
	}
	if action == taskActionCommand && command == "" {
		writeErr(w, http.StatusBadRequest, "「游戏指令」必须填写要下发的指令内容")
		return
	}
	if name == "" {
		name = defaultTaskName(action, command, spec)
	}

	if _, err := s.db.Exec(`
		UPDATE instance_tasks SET name = ?, action = ?, command = ?, cron = ?, enabled = ?,
		       updated_at = CURRENT_TIMESTAMP WHERE id = ?`,
		name, action, command, spec, enabled, id); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.audit(r, "update_task", instanceID,
		fmt.Sprintf("task=%d enabled=%d cron=%s", id, enabled, spec))
	writeJSON(w, http.StatusOK, map[string]string{"message": "定时任务已更新"})
}

// handleDeleteTask DELETE /api/tasks/{id}
func (s *Server) handleDeleteTask(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "id 无效")
		return
	}
	instanceID, ok := s.taskInstanceID(id)
	if !ok {
		writeErr(w, http.StatusNotFound, "任务不存在")
		return
	}
	if !s.requireInstanceLevel(w, r, instanceID, LevelOwner) {
		return
	}
	if _, err := s.db.Exec(`DELETE FROM instance_tasks WHERE id = ?`, id); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.audit(r, "delete_task", instanceID, fmt.Sprintf("task=%d", id))
	writeJSON(w, http.StatusOK, map[string]string{"message": "定时任务已删除"})
}

// handleRunTask POST /api/tasks/{id}/run
//
// 立刻执行一次（不影响原有排期）。用途：配好任务后先验证一遍。
func (s *Server) handleRunTask(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "id 无效")
		return
	}
	instanceID, ok := s.taskInstanceID(id)
	if !ok {
		writeErr(w, http.StatusNotFound, "任务不存在")
		return
	}
	if !s.requireInstanceLevel(w, r, instanceID, LevelOwner) {
		return
	}
	if err := s.RunInstanceTask(id); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.audit(r, "run_task", instanceID, fmt.Sprintf("task=%d", id))
	writeJSON(w, http.StatusOK, map[string]string{"message": "任务已执行"})
}

// taskInstanceID 查询任务所属实例。
func (s *Server) taskInstanceID(id int64) (string, bool) {
	var instanceID string
	if err := s.db.QueryRow(`SELECT instance_id FROM instance_tasks WHERE id = ?`, id).
		Scan(&instanceID); err != nil {
		return "", false
	}
	return instanceID, true
}

// defaultTaskName 在用户没起名时生成一个能看懂的名字。
func defaultTaskName(action, command, spec string) string {
	label := taskActionLabels[action]
	if label == "" {
		label = action
	}
	switch {
	case action == taskActionCommand && command != "":
		return label + "：" + command
	case action == taskActionStart:
		return "定时开机"
	case action == taskActionStop:
		return "定时关机"
	default:
		return label + "（" + spec + "）"
	}
}

// RunInstanceTask 执行一条定时任务并记录结果。
//
// 同时被 HTTP 接口（「立即执行」）和后台调度器调用，
// 因此这里的一切都必须自带超时与错误回写 —— 不能依赖请求上下文。
func (s *Server) RunInstanceTask(taskID int64) error {
	var (
		instanceID, action, command, name string
	)
	if err := s.db.QueryRow(
		`SELECT instance_id, action, command, name FROM instance_tasks WHERE id = ?`, taskID).
		Scan(&instanceID, &action, &command, &name); err != nil {
		return fmt.Errorf("任务不存在: %w", err)
	}

	err := s.execInstanceTask(instanceID, action, command)

	// 无论成败都记录：成功也要刷新 last_run，否则调度器下一轮会重复触发
	state, errText := "success", ""
	if err != nil {
		state, errText = "failed", err.Error()
	}
	if _, dbErr := s.db.Exec(`
		UPDATE instance_tasks SET last_run = CURRENT_TIMESTAMP, last_state = ?, last_error = ?,
		       run_count = run_count + 1, fail_count = fail_count + ? WHERE id = ?`,
		state, errText, boolToInt(err != nil), taskID); dbErr != nil {
		s.logger.Warn("回写定时任务执行结果失败", "task", taskID, "error", dbErr)
	}
	if err != nil {
		return fmt.Errorf("%s 执行失败: %w", name, err)
	}
	return nil
}

// execInstanceTask 把动作翻译成 Daemon 调用。
func (s *Server) execInstanceTask(instanceID, action, command string) error {
	cli, _, err := s.getDaemonClient(instanceID)
	if err != nil {
		return fmt.Errorf("实例不存在或节点不可达: %w", err)
	}
	// 开机/关机的确可能慢：Daemon 侧 Stop 会等存档落盘，Start 要等 JVM 起来
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	req := &pb.InstanceRequest{InstanceId: instanceID}
	var resp *pb.OperationResponse
	switch action {
	case taskActionStart:
		resp, err = cli.StartInstance(ctx, req)
	case taskActionStop:
		resp, err = cli.StopInstance(ctx, req)
	case taskActionRestart:
		resp, err = cli.RestartInstance(ctx, req)
	case taskActionKill:
		resp, err = cli.KillInstance(ctx, req)
	case taskActionCommand:
		resp, err = cli.SendCommand(ctx, &pb.CommandRequest{InstanceId: instanceID, Command: command})
	default:
		return fmt.Errorf("未知动作类型：%s", action)
	}
	if err != nil {
		return err
	}
	if resp != nil && !resp.Success {
		// 「已经是目标状态」不算失败。
		//
		// 定时任务里这种情况非常常见：配了每天 06:00 开机，而服务器整夜没关；
		// 配了每天 23:30 关机，而服务器早就停了。若把它们记成失败，
		// 任务页会堆满红色记录，用户很快就会对告警脱敏 ——
		// 真正出问题的那次反而没人看。
		if isIdempotentNoop(action, resp.Error) {
			return nil
		}
		return fmt.Errorf("%s", resp.Error)
	}
	return nil
}

// isIdempotentNoop 判断错误是否只是"状态已经是想要的"。
//
// 匹配的是 Daemon 侧的原文（"实例已在运行" / "实例未运行"）。
// 这些字符串在同一仓库内定义，改动会被同时看到；相比引入新的错误码
// 体系，这里用文本匹配的代价更小。
func isIdempotentNoop(action, errMsg string) bool {
	switch action {
	case taskActionStart:
		return strings.Contains(errMsg, "已在运行")
	case taskActionStop, taskActionKill:
		// Kill 对已停止实例同样报"实例未在运行"
		return strings.Contains(errMsg, "未运行") || strings.Contains(errMsg, "未在运行")
	}
	return false
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
