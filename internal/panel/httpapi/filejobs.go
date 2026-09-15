package httpapi

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/ATLCNND/ATL-MCPanel/internal/daemon/fileops"
	"github.com/ATLCNND/ATL-MCPanel/internal/daemon/jobqueue"
	pb "github.com/ATLCNND/ATL-MCPanel/internal/proto/mcpanel"
)

// 面板侧的排队任务（file_jobs）状态。
//
// 与 Daemon 的状态是一一对应的，只是多了一层"面板还没派发出去"的
// queued 阶段 —— 面板不可达节点时任务不会丢，等节点恢复后自动补发。
const (
	jobStateQueued   = "queued"
	jobStateRunning  = "running"
	jobStateSuccess  = "success"
	jobStateFailed   = "failed"
	jobStateCanceled = "canceled"
)

// jobDispatchInterval 派发/回读任务的轮询间隔。
//
// 2 秒是体验与开销的折中：进度条看起来是"连续"的，
// 而每秒一次全表扫描对 SQLite 来说也毫无压力。
const jobDispatchInterval = 2 * time.Second

// jobLostAfter 判定「任务在节点上丢失」的阈值。
//
// Daemon 的队列是内存态，重启即清空。此时面板查到的状态是 unknown，
// 若不处理，任务会永远停在 running —— 用户看到一个卡死的进度条，
// 比看到一条明确的失败信息糟糕得多。
const jobLostAfter = 90 * time.Second

// fileJobView 排队任务的对外视图。
type fileJobView struct {
	JobID      string `json:"job_id"`
	InstanceID string `json:"instance_id"`
	Kind       string `json:"kind"`
	Src        string `json:"src"`
	Dst        string `json:"dst"`
	Format     string `json:"format"`
	State      string `json:"state"`
	Progress   int    `json:"progress"`
	Message    string `json:"message"`
	Error      string `json:"error"`
	TotalBytes int64  `json:"total_bytes"`
	DoneBytes  int64  `json:"done_bytes"`
	CreatedBy  string `json:"created_by"`
	CreatedAt  string `json:"created_at"`
	StartedAt  string `json:"started_at"`
	FinishedAt string `json:"finished_at"`
}

// newJobID 生成任务 ID。
//
// 带随机后缀而不是纯自增：任务 ID 会出现在日志与审计里，
// 同时被用作 Daemon 侧的幂等键，需要全局唯一且不可预测。
func newJobID() string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		// 极端情况下退化为时间戳，仍然可用
		return fmt.Sprintf("job-%d", time.Now().UnixNano())
	}
	return "job-" + hex.EncodeToString(buf)
}

// handleCreateJob POST /api/instances/{id}/jobs
//
// 提交一个排队任务（目前支持压缩 / 解压）。
// 立即返回任务 ID，前端凭它轮询进度 —— 大目录打包可能跑十几分钟，
// 同步等待既会撑爆 HTTP 超时，也会让用户以为界面卡死。
func (s *Server) handleCreateJob(w http.ResponseWriter, r *http.Request) {
	instanceID := r.PathValue("id")
	if !s.requireInstanceLevel(w, r, instanceID, LevelOwner) {
		return
	}
	var req struct {
		Kind   string `json:"kind"`
		Src    string `json:"src"`
		Dst    string `json:"dst"`
		Format string `json:"format"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "无效请求体")
		return
	}

	kind := strings.ToLower(strings.TrimSpace(req.Kind))
	if kind != jobqueue.KindCompress && kind != jobqueue.KindExtract {
		writeErr(w, http.StatusBadRequest, "不支持的任务类型："+req.Kind)
		return
	}
	src := normalizeRelPath(req.Src)
	dst := normalizeRelPath(req.Dst)
	if src == "" {
		writeErr(w, http.StatusBadRequest, "请选择要处理的文件或目录")
		return
	}
	if dst == "" {
		writeErr(w, http.StatusBadRequest, "请指定目标路径")
		return
	}

	format := strings.ToLower(strings.TrimSpace(req.Format))
	switch kind {
	case jobqueue.KindCompress:
		if format == "" {
			format = "zip"
		}
		if format != "zip" && format != "tar.gz" && format != "tar" {
			writeErr(w, http.StatusBadRequest, "不支持的压缩格式："+format)
			return
		}
		// 目标必须是明确的文件路径：以 / 结尾说明前端只想给目录，
		// 这时按「源的名字 + 格式扩展名」自动补全，省掉一次往返
		if strings.HasSuffix(req.Dst, "/") {
			dst = path.Join(dst, path.Base(src)+fileops.DefaultExt(format))
		} else if !strings.HasSuffix(dst, fileops.DefaultExt(format)) {
			dst += fileops.DefaultExt(format)
		}
	case jobqueue.KindExtract:
		// 解压不限定格式：Daemon 会按文件魔数识别，
		// 用户把 .tar.gz 命名成 .zip 的情况非常常见
		if format != "" && format != "zip" && format != "tar.gz" && format != "tar" {
			writeErr(w, http.StatusBadRequest, "不支持的压缩格式："+format)
			return
		}
	}

	nodeID, ok := s.instanceNodeID(instanceID)
	if !ok {
		writeErr(w, http.StatusNotFound, "实例不存在")
		return
	}

	jobID := newJobID()
	if _, err := s.db.Exec(`
		INSERT INTO file_jobs (job_id, instance_id, node_id, kind, src, dst, format, state, message, created_by)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		jobID, instanceID, nodeID, kind, src, dst, format, jobStateQueued, "已加入队列，等待派发到节点", currentUserID(r)); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	label := "压缩"
	if kind == jobqueue.KindExtract {
		label = "解压"
	}
	s.audit(r, "create_job", instanceID, fmt.Sprintf("job=%s %s %s -> %s", jobID, kind, src, dst))

	// 立刻尝试派发一次：让用户马上看到进度，而不是等到下一轮轮询
	go s.dispatchJob(jobID)

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"job_id":  jobID,
		"message": label + "任务已提交，可在任务列表中查看进度",
	})
}

// fileJobColumns 任务列表的查询列。
//
// 教训：users 表也有 created_at 列，未加表别名的 `ORDER BY created_at`
// 会被 SQLite 判为「ambiguous column name」并让整个查询失败 ——
// 而失败发生在 3 秒一次的轮询里，用户看到的现象是"任务一直卡在排队中"，
// 排查起来很绕。把列清单抽成常量，就不会再漏写别名。
const fileJobColumns = `f.job_id, f.instance_id, f.kind, f.src, f.dst, f.format, f.state,
	f.progress, f.message, f.error, f.total_bytes, f.done_bytes,
	COALESCE(u.username, ''), f.created_at, f.started_at, f.finished_at`

// handleListJobs GET /api/instances/{id}/jobs
func (s *Server) handleListJobs(w http.ResponseWriter, r *http.Request) {
	instanceID := r.PathValue("id")
	if !s.requireInstanceLevel(w, r, instanceID, LevelViewer) {
		return
	}
	rows, err := s.db.Query(`
		SELECT `+fileJobColumns+`
		FROM file_jobs f
		LEFT JOIN users u ON u.id = f.created_by
		WHERE f.instance_id = ?
		ORDER BY f.created_at DESC, f.rowid DESC
		LIMIT 50`, instanceID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()

	list := []fileJobView{}
	for rows.Next() {
		var v fileJobView
		var created string
		var started, finished sql.NullString
		if err := rows.Scan(&v.JobID, &v.InstanceID, &v.Kind, &v.Src, &v.Dst, &v.Format,
			&v.State, &v.Progress, &v.Message, &v.Error, &v.TotalBytes, &v.DoneBytes,
			&v.CreatedBy, &created, &started, &finished); err != nil {
			continue
		}
		v.CreatedAt = created
		if started.Valid {
			v.StartedAt = started.String
		}
		if finished.Valid {
			v.FinishedAt = finished.String
		}
		list = append(list, v)
	}
	writeJSON(w, http.StatusOK, list)
}

// handleCancelJob DELETE /api/jobs/{job_id}
func (s *Server) handleCancelJob(w http.ResponseWriter, r *http.Request) {
	jobID := r.PathValue("job_id")
	instanceID, state, ok := s.jobInfo(jobID)
	if !ok {
		writeErr(w, http.StatusNotFound, "任务不存在")
		return
	}
	if !s.requireInstanceLevel(w, r, instanceID, LevelOwner) {
		return
	}
	switch state {
	case jobStateSuccess, jobStateFailed, jobStateCanceled:
		writeErr(w, http.StatusBadRequest, "任务已结束，无法取消")
		return
	}

	// 先在本地面板标记，再尽力通知节点。
	// 顺序不能反：若先通知节点而面板写库失败，用户会看到任务仍在跑但实际已停。
	if _, err := s.db.Exec(`
		UPDATE file_jobs SET state = ?, message = '已请求取消', finished_at = CURRENT_TIMESTAMP
		WHERE job_id = ?`, jobStateCanceled, jobID); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.cancelJobOnDaemon(jobID, instanceID)
	s.audit(r, "cancel_job", instanceID, "job="+jobID)
	writeJSON(w, http.StatusOK, map[string]string{"message": "任务已取消"})
}

// jobInfo 查询任务所属实例与当前状态。
func (s *Server) jobInfo(jobID string) (instanceID, state string, ok bool) {
	err := s.db.QueryRow(`SELECT instance_id, state FROM file_jobs WHERE job_id = ?`, jobID).
		Scan(&instanceID, &state)
	return instanceID, state, err == nil
}

// cancelJobOnDaemon 尽力通知节点取消任务（失败不影响面板状态）。
func (s *Server) cancelJobOnDaemon(jobID, instanceID string) {
	cli, _, err := s.getDaemonClient(instanceID)
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := cli.CancelJob(ctx, &pb.JobRequest{JobId: jobID, InstanceId: instanceID}); err != nil {
		s.logger.Warn("通知节点取消任务失败", "job", jobID, "error", err)
	}
}

// StartJobDispatcher 启动排队任务的派发与进度回读循环。
func (s *Server) StartJobDispatcher() {
	go func() {
		// 启动后稍等，避开面板初始化与节点重连的高峰
		time.Sleep(10 * time.Second)
		t := time.NewTicker(jobDispatchInterval)
		defer t.Stop()
		for range t.C {
			s.pumpJobs()
		}
	}()
	s.logger.Info("排队任务派发器已启动", "interval", jobDispatchInterval.String())
}

// pumpJobs 一轮：派发未提交的任务 + 回读执行中任务的进度。
func (s *Server) pumpJobs() {
	rows, err := s.db.Query(`
		SELECT job_id FROM file_jobs
		WHERE state IN (?, ?)
		ORDER BY created_at ASC, rowid ASC
		LIMIT 50`, jobStateQueued, jobStateRunning)
	if err != nil {
		return
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()

	for _, id := range ids {
		s.dispatchJob(id)
	}
}

// dispatchJob 派发或回读单个任务。
func (s *Server) dispatchJob(jobID string) {
	var (
		instanceID, kind, src, dst, format, state string
		startedAt                                 sql.NullTime
	)
	err := s.db.QueryRow(`
		SELECT instance_id, kind, src, dst, format, state, started_at
		FROM file_jobs WHERE job_id = ?`, jobID).
		Scan(&instanceID, &kind, &src, &dst, &format, &state, &startedAt)
	if err != nil {
		return
	}
	if state != jobStateQueued && state != jobStateRunning {
		return
	}

	cli, _, err := s.getDaemonClient(instanceID)
	if err != nil {
		s.markJobFailed(jobID, "节点不可达："+err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	if state == jobStateQueued {
		resp, err := cli.SubmitJob(ctx, &pb.SubmitJobRequest{
			JobId:      jobID,
			InstanceId: instanceID,
			Kind:       kind,
			Src:        src,
			Dst:        dst,
			Format:     format,
		})
		if err != nil {
			// 节点暂时不可达不应立刻判失败：任务先留在队列里，
			// 等节点恢复或 Daemon 重启后下一轮会自动重试派发。
			s.logger.Warn("派发任务失败，将在下一轮重试", "job", jobID, "error", err)
			return
		}
		if !resp.Success {
			s.markJobFailed(jobID, resp.Error)
			return
		}
		msg := "已派发到节点执行"
		if resp.QueuePosition > 0 {
			msg = fmt.Sprintf("节点排队中（前面还有 %d 个任务）", resp.QueuePosition)
		}
		if _, err := s.db.Exec(`
			UPDATE file_jobs SET state = ?, message = ?, started_at = COALESCE(started_at, CURRENT_TIMESTAMP)
			WHERE job_id = ?`, jobStateRunning, msg, jobID); err != nil {
			s.logger.Warn("回写任务派发状态失败", "job", jobID, "error", err)
		}
		return
	}

	// state == running：回读节点上的实际状态
	st, err := cli.GetJob(ctx, &pb.JobRequest{JobId: jobID, InstanceId: instanceID})
	if err != nil {
		s.logger.Warn("回读任务进度失败", "job", jobID, "error", err)
		return
	}
	switch st.State {
	case jobqueue.StateQueued, jobqueue.StateRunning:
		msg := st.Message
		if st.State == jobqueue.StateQueued {
			msg = "节点排队中"
		}
		if _, err := s.db.Exec(`
			UPDATE file_jobs SET state = ?, progress = ?, message = ?, total_bytes = ?, done_bytes = ?
			WHERE job_id = ?`, jobStateRunning, st.Progress, msg, st.TotalBytes, st.DoneBytes, jobID); err != nil {
			s.logger.Warn("回写任务进度失败", "job", jobID, "error", err)
		}
	case jobqueue.StateSuccess:
		if _, err := s.db.Exec(`
			UPDATE file_jobs SET state = ?, progress = 100, message = ?, total_bytes = ?, done_bytes = ?,
			       finished_at = CURRENT_TIMESTAMP WHERE job_id = ?`,
			jobStateSuccess, st.Message, st.TotalBytes, st.DoneBytes, jobID); err != nil {
			s.logger.Warn("回写任务完成状态失败", "job", jobID, "error", err)
		}
	case jobqueue.StateFailed:
		s.markJobFailed(jobID, st.Error)
	case jobqueue.StateCanceled:
		if _, err := s.db.Exec(`
			UPDATE file_jobs SET state = ?, message = '已取消', finished_at = CURRENT_TIMESTAMP
			WHERE job_id = ?`, jobStateCanceled, jobID); err != nil {
			s.logger.Warn("回写任务取消状态失败", "job", jobID, "error", err)
		}
	default:
		// unknown：Daemon 重启丢了内存队列，或任务已被清理
		if startedAt.Valid && time.Since(startedAt.Time) > jobLostAfter {
			s.markJobFailed(jobID, "任务在节点上已丢失（Daemon 可能重启过），请重新提交")
		}
	}
}

// markJobFailed 把任务标记为失败。
func (s *Server) markJobFailed(jobID, reason string) {
	if _, err := s.db.Exec(`
		UPDATE file_jobs SET state = ?, message = '执行失败', error = ?, finished_at = CURRENT_TIMESTAMP
		WHERE job_id = ?`, jobStateFailed, reason, jobID); err != nil {
		s.logger.Warn("回写任务失败状态出错", "job", jobID, "error", err)
	}
}

// normalizeRelPath 规范化实例内相对路径（统一用 /，去掉前后多余斜杠）。
func normalizeRelPath(p string) string {
	p = strings.ReplaceAll(strings.TrimSpace(p), "\\", "/")
	p = strings.Trim(p, "/")
	if p == "." {
		return ""
	}
	return p
}
