package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/ATLCNND/ATL-MCPanel/internal/panel/retention"
	"github.com/ATLCNND/ATL-MCPanel/internal/panel/scheduler"
	pb "github.com/ATLCNND/ATL-MCPanel/internal/proto/mcpanel"
)

// StartScheduler 构造并启动后台调度器（定时备份 + 健康告警）。
func (s *Server) StartScheduler() {
	if s.sched != nil {
		return
	}
	s.sched = scheduler.New(scheduler.Options{
		DB:     s.db,
		Logger: s.logger,
		Backup: s.scheduledBackup,
		// 指标采样：供「统计」页积累历史趋势
		MetricsSampler: func(instanceID string) (int64, int64, int, float64, int64, bool) {
			cli, _, err := s.getDaemonClient(instanceID)
			if err != nil {
				return 0, 0, 0, 0, 0, false
			}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			m, err := cli.GetMetrics(ctx, &pb.InstanceRequest{InstanceId: instanceID})
			if err != nil {
				return 0, 0, 0, 0, 0, false
			}
			// 磁盘用量与指标一起取回：目录体积统计要遍历文件，
			// 单独再问一次 Daemon 是浪费（Daemon 侧有 60 秒缓存，代价可接受）。
			var diskUsed int64
			if rt, err := cli.GetInstanceRuntime(ctx, &pb.InstanceRequest{InstanceId: instanceID}); err == nil && rt.Success {
				diskUsed = rt.DiskUsed
			}
			return int64(m.CpuPercent), int64(m.MemUsed), int(m.PlayersOnline), m.Tps, diskUsed, true
		},
		// 磁盘超限时的自动停机。仅当实例开启了 disk_autostop 才会被调用。
		InstanceStopper: func(instanceID string) error {
			cli, _, err := s.getDaemonClient(instanceID)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			resp, err := cli.StopInstance(ctx, &pb.InstanceRequest{InstanceId: instanceID})
			if err != nil {
				return err
			}
			if !resp.Success {
				return fmt.Errorf("%s", resp.Error)
			}
			return nil
		},
		InstanceProbe: func(instanceID string) (string, error) {
			cli, _, err := s.getDaemonClient(instanceID)
			if err != nil {
				return "", err
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			resp, err := cli.GetInstanceStatus(ctx, &pb.InstanceRequest{InstanceId: instanceID})
			if err != nil {
				return "", err
			}
			return resp.Status, nil
		},
		NodeProbe: func(nodeID int64, nodeName string) (bool, int64, error) {
			var lastSeen *time.Time
			var status string
			if err := s.db.QueryRow(`SELECT status, last_seen FROM nodes WHERE id = ?`, nodeID).
				Scan(&status, &lastSeen); err != nil {
				return false, 0, err
			}
			// 心跳间隔 10 秒，超过 90 秒未上报视为离线
			online := status == "online" && lastSeen != nil && time.Since(*lastSeen) < 90*time.Second
			return online, s.nodeFreeDiskMB(nodeID), nil
		},
		// 定时指令任务（开机 / 关机 / 游戏指令）的执行体。
		// 由调度器判断"谁到期了"，这里只负责把动作落到 Daemon。
		TaskRunner: s.RunInstanceTask,
	})
	s.sched.Start()
	s.logger.Info("后台调度器已启动（定时备份 + 定时指令任务 + 告警）")

	// 排队任务派发器（压缩 / 解压）：与调度器分开跑 ——
	// 它的轮询间隔是秒级（要跟进度条），而调度器是分钟级，
	// 混在一起会让每轮 tick 都去扫一遍 file_jobs，没有必要。
	s.StartJobDispatcher()
}

// StopScheduler 停止调度器。
func (s *Server) StopScheduler() {
	if s.sched != nil {
		s.sched.Stop()
	}
}

// nodeFreeDiskMB 返回节点磁盘剩余空间（MB）。
// 数据来自 Daemon 随心跳上报的主机资源；未上报时返回 0（表示未知，不告警）。
func (s *Server) nodeFreeDiskMB(nodeID int64) int64 {
	var used, total int64
	if err := s.db.QueryRow(`SELECT disk_used, disk_total FROM nodes WHERE id = ?`, nodeID).
		Scan(&used, &total); err != nil {
		return 0
	}
	if total <= 0 {
		return 0 // 尚未上报
	}
	free := (total - used) / 1024 / 1024
	if free < 0 {
		return 0
	}
	return free
}

// scheduledBackup 执行一次定时备份，并按保留策略淘汰旧备份。
func (s *Server) scheduledBackup(instanceID string, includeConfig bool, policy retention.Policy) error {
	cli, _, err := s.getDaemonClient(instanceID)
	if err != nil {
		return fmt.Errorf("实例不存在或节点不可达: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	// name 留空 → Daemon 使用 "auto" 命名，从而与手动备份区分开
	resp, err := cli.Backup(ctx, &pb.BackupRequest{
		InstanceId:    instanceID,
		IncludeConfig: includeConfig,
	})
	if err != nil {
		return fmt.Errorf("备份失败: %w", err)
	}
	if !resp.Success {
		return fmt.Errorf("备份失败: %s", resp.Message)
	}

	s.pruneByPolicy(ctx, cli, instanceID, policy)
	return nil
}

// ---- 定时备份计划 API ----

type scheduleView struct {
	InstanceID    string `json:"instance_id"`
	Enabled       bool   `json:"enabled"`
	IntervalHours int    `json:"interval_hours"`
	Keep          int    `json:"keep"`
	IncludeConfig bool   `json:"include_config"`
	LastRun       string `json:"last_run"`
	LastError     string `json:"last_error"`
	NextRun       string `json:"next_run"`

	// 生效的保留策略
	PolicyID      int64  `json:"policy_id"`      // 0 = 默认策略
	PolicyName    string `json:"policy_name"`
	PolicyDesc    string `json:"policy_desc"`
	ManualKeep    int    `json:"manual_keep"`
	EffectiveHours int   `json:"effective_hours"` // 实际生效的间隔（策略优先）
}

// handleGetSchedule GET /api/instances/{id}/schedule
func (s *Server) handleGetSchedule(w http.ResponseWriter, r *http.Request) {
	instanceID := r.PathValue("id")
	if !s.requireInstanceLevel(w, r, instanceID, LevelCollab) {
		return
	}
	// 默认展示（尚未配置计划时）采用默认策略的参数
	defPolicy, defInterval, _, _ := s.loadPolicy(0)
	v := scheduleView{
		InstanceID: instanceID, IntervalHours: defInterval, IncludeConfig: true,
		PolicyName: "默认策略", PolicyDesc: defPolicy.Describe(), ManualKeep: defPolicy.ManualKeep,
		EffectiveHours: defInterval,
	}

	var lastRun *time.Time
	var enabled, includeCfg int
	err := s.db.QueryRow(`
		SELECT enabled, interval_hours, keep, include_config, last_run, last_error, policy_id
		FROM backup_schedules WHERE instance_id = ?`, instanceID).
		Scan(&enabled, &v.IntervalHours, &v.Keep, &includeCfg, &lastRun, &v.LastError, &v.PolicyID)
	if err == nil {
		v.Enabled = enabled == 1
		v.IncludeConfig = includeCfg == 1
	}

	// 解析生效策略：指派了就用它，否则用默认
	pol, polInterval, _, _ := s.loadPolicy(v.PolicyID)
	v.PolicyDesc = pol.Describe()
	v.ManualKeep = pol.ManualKeep
	v.EffectiveHours = polInterval
	if v.PolicyID == 0 {
		v.PolicyName = "默认策略"
	} else {
		_ = s.db.QueryRow(`SELECT name FROM backup_policies WHERE id = ?`, v.PolicyID).Scan(&v.PolicyName)
	}
	if !v.Enabled {
		v.EffectiveHours = v.IntervalHours // 未启用时展示实例自身配置的间隔
	}

	if lastRun != nil {
		v.LastRun = lastRun.Format(time.RFC3339)
		next := lastRun.Add(time.Duration(v.EffectiveHours) * time.Hour)
		v.NextRun = next.Format(time.RFC3339)
	} else if v.Enabled {
		v.NextRun = time.Now().Format(time.RFC3339) + "（即将首次执行）"
	}

	writeJSON(w, http.StatusOK, v)
}

// handleSetSchedule POST /api/instances/{id}/schedule
func (s *Server) handleSetSchedule(w http.ResponseWriter, r *http.Request) {
	instanceID := r.PathValue("id")
	if !s.requireInstanceLevel(w, r, instanceID, LevelOwner) {
		return
	}
	var req struct {
		Enabled       bool  `json:"enabled"`
		IntervalHours int   `json:"interval_hours"`
		Keep          int   `json:"keep"`
		IncludeConfig bool  `json:"include_config"`
		PolicyID      int64 `json:"policy_id"` // 0 = 使用默认策略
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "无效请求体")
		return
	}
	if req.IntervalHours <= 0 {
		req.IntervalHours = 6
	}
	if req.IntervalHours < 1 {
		writeErr(w, http.StatusBadRequest, "备份间隔至少 1 小时")
		return
	}

	// 指派了策略时，间隔与保留规则以策略为准（此处仅保存归属关系）
	if req.PolicyID > 0 {
		var exists int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM backup_policies WHERE id = ?`, req.PolicyID).
			Scan(&exists); err != nil || exists == 0 {
			writeErr(w, http.StatusBadRequest, "指定的保留策略不存在")
			return
		}
	}

	enabled, includeCfg := 0, 0
	if req.Enabled {
		enabled = 1
	}
	if req.IncludeConfig {
		includeCfg = 1
	}

	_, err := s.db.Exec(`
		INSERT INTO backup_schedules (instance_id, enabled, interval_hours, keep, include_config, policy_id, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(instance_id) DO UPDATE SET
			enabled = excluded.enabled, interval_hours = excluded.interval_hours,
			keep = excluded.keep, include_config = excluded.include_config,
			policy_id = excluded.policy_id,
			updated_at = CURRENT_TIMESTAMP`,
		instanceID, enabled, req.IntervalHours, req.Keep, includeCfg, req.PolicyID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	s.audit(r, "set_backup_schedule", instanceID,
		fmt.Sprintf("enabled=%v policy=%d", req.Enabled, req.PolicyID))
	msg := fmt.Sprintf("备份计划已保存（每 %d 小时）", req.IntervalHours)
	if req.PolicyID > 0 {
		p, _, _, _ := s.loadPolicy(req.PolicyID)
		msg = fmt.Sprintf("备份计划已保存（使用策略，梯度：%s）", p.Describe())
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": msg})
}

// ---- 告警 API ----

type alertView struct {
	ID         int64  `json:"id"`
	Kind       string `json:"kind"`
	Severity   string `json:"severity"`
	Target     string `json:"target"`
	Message    string `json:"message"`
	Detail     string `json:"detail"`
	Active     bool   `json:"active"`
	CreatedAt  string `json:"created_at"`
	ResolvedAt string `json:"resolved_at"`
}

// handleListAlerts GET /api/alerts?active=1&limit=100
func (s *Server) handleListAlerts(w http.ResponseWriter, r *http.Request) {
	if !requireAdminIn(w, r) {
		return
	}
	onlyActive := r.URL.Query().Get("active") != "0"
	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 1000 {
			limit = n
		}
	}

	q := `SELECT id, kind, severity, target, message, detail, active, created_at, resolved_at FROM alerts`
	if onlyActive {
		q += ` WHERE active = 1`
	}
	q += ` ORDER BY id DESC LIMIT ?`

	rows, err := s.db.Query(q, limit)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()

	list := []alertView{}
	activeCount := 0
	for rows.Next() {
		var v alertView
		var active int
		var created string
		var resolved *string
		if err := rows.Scan(&v.ID, &v.Kind, &v.Severity, &v.Target, &v.Message, &v.Detail, &active, &created, &resolved); err != nil {
			continue
		}
		v.Active = active == 1
		v.CreatedAt = created
		if resolved != nil {
			v.ResolvedAt = *resolved
		}
		if v.Active {
			activeCount++
		}
		list = append(list, v)
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"alerts":       list,
		"active_count": activeCount,
	})
}

// handleResolveAlert POST /api/alerts/{id}/resolve
func (s *Server) handleResolveAlert(w http.ResponseWriter, r *http.Request) {
	if !requireAdminIn(w, r) {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "id 无效")
		return
	}
	if _, err := s.db.Exec(
		`UPDATE alerts SET active = 0, resolved_at = CURRENT_TIMESTAMP WHERE id = ?`, id); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.audit(r, "resolve_alert", strconv.FormatInt(id, 10), "")
	writeJSON(w, http.StatusOK, map[string]string{"message": "告警已标记为已处理"})
}
