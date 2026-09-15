// Package scheduler 提供面板侧的后台任务：定时备份与健康告警。
//
// 设计：
//   - 单 goroutine 定时轮询（默认 1 分钟），避免并发写库
//   - 备份为「到期才执行」而非固定时刻，符合大多数使用习惯
//   - 告警按 (kind, target) 去重：同一问题只保留一条活跃告警，恢复后自动关闭
package scheduler

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"time"

	"github.com/ATLCNND/ATL-MCPanel/internal/panel/cron"
	"github.com/ATLCNND/ATL-MCPanel/internal/panel/retention"
)

// BackupRunner 执行一次实例备份（由 httpapi 层注入，避免包间循环依赖）。
type BackupRunner func(instanceID string, includeConfig bool, policy retention.Policy) error

// MetricsSampler 采集一个实例的实时指标与磁盘用量。
//
// 磁盘用量与指标一并返回，是为了避免为同一实例发起两次 Daemon 调用
// （磁盘体积统计需要遍历目录，本身就不便宜）。
type MetricsSampler func(instanceID string) (cpu, mem int64, players int, tps float64, diskUsed int64, ok bool)

// HealthProbe 返回实例的实际状态（running/stopped/not_found）与错误。
type HealthProbe func(instanceID string) (string, error)

// NodeProbe 返回节点是否在线及磁盘剩余 MB。
type NodeProbe func(nodeID int64, nodeName string) (online bool, freeDiskMB int64, err error)

// Options 调度器配置。
type Options struct {
	DB           *sql.DB
	Logger       *slog.Logger
	Backup       BackupRunner
	InstanceProbe HealthProbe
	// MetricsSampler 采样一个实例的实时指标。由 httpapi 注入（需要访问 Daemon）。
	MetricsSampler MetricsSampler
	// InstanceStopper 停止一个实例（磁盘超限自动停机时使用）。
	// 为 nil 表示不自动停机 —— 停机有破坏性，需管理员显式开启。
	InstanceStopper func(instanceID string) error
	// TaskRunner 执行一条到期的定时指令任务（开机 / 关机 / 游戏指令）。
	// 由 httpapi 注入：调度器只负责「判断谁到期了」，
	// 具体怎么调 Daemon 属于接口层的知识。
	TaskRunner func(taskID int64) error

	NodeProbe    NodeProbe

	// 磁盘告警阈值（MB）
	DiskWarnMB int64
	// 轮询间隔
	Interval time.Duration
}

// Scheduler 后台任务调度器。
type Scheduler struct {
	opts   Options
	stopCh chan struct{}
	doneCh chan struct{}
}

// New 创建调度器。
func New(opts Options) *Scheduler {
	if opts.Interval <= 0 {
		opts.Interval = time.Minute
	}
	if opts.DiskWarnMB <= 0 {
		opts.DiskWarnMB = 2048 // 2GB
	}
	return &Scheduler{
		opts:   opts,
		stopCh: make(chan struct{}),
		doneCh: make(chan struct{}),
	}
}

// Start 启动后台循环。
func (s *Scheduler) Start() {
	go func() {
		defer close(s.doneCh)
		// 启动后稍等再首轮执行，避免与面板初始化竞争
		timer := time.NewTimer(20 * time.Second)
		defer timer.Stop()

		for {
			select {
			case <-s.stopCh:
				return
			case <-timer.C:
				s.RunOnce(context.Background())
				timer.Reset(s.opts.Interval)
			}
		}
	}()
}

// Stop 停止调度器并等待退出。
func (s *Scheduler) Stop() {
	close(s.stopCh)
	<-s.doneCh
}

// RunOnce 执行一轮：定时备份 + 定时指令任务 + 健康检查 + 到期检查 + 指标采样。
func (s *Scheduler) RunOnce(ctx context.Context) {
	s.runDueBackups(ctx)
	s.runDueTasks(ctx)
	s.checkInstances(ctx)
	s.checkNodes(ctx)
	s.checkExpiry(ctx)
	s.sampleMetrics(ctx)
}

// checkExpiry 处理实例到期：临近到期告警、到期后按配置自动停止。
//
// 语义（与用户确认过）：
//   - 到期**不删数据**，只是停掉。想清盘由管理员显式执行"彻底删除"。
//   - 到期自动停止默认开启，但可按实例关闭（只告警不动作）。
//   - 只要还在提前提醒窗口内就产生告警；续期或删除实例后告警自动消解。
//
// 为什么列成告警而不是只写日志：到期是"需要在到期**之前**做点什么"的事，
// 而告警中心是运维唯一会主动看的地方。
func (s *Scheduler) checkExpiry(ctx context.Context) {
	if s.opts.InstanceStopper == nil {
		return
	}
	rows, err := s.opts.DB.Query(`
		SELECT instance_id, expires_at, expiry_notice_days, expiry_autostop, status
		FROM instances WHERE expires_at IS NOT NULL`)
	if err != nil {
		return
	}
	type item struct {
		id        string
		expires   time.Time
		notice    int
		autostop  bool
		status    string
	}
	var list []item
	for rows.Next() {
		var it item
		var autostop int
		if err := rows.Scan(&it.id, &it.expires, &it.notice, &autostop, &it.status); err != nil {
			continue
		}
		it.autostop = autostop == 1
		if it.notice <= 0 {
			it.notice = 3 // 未设置时用默认提前 3 天
		}
		list = append(list, it)
	}
	rows.Close()

	now := time.Now()
	for _, it := range list {
		left := it.expires.Sub(now)
		switch {
		case left <= 0:
			// 已到期
			s.raise(ctx, "instance_expired", "critical", it.id,
				fmt.Sprintf("实例 %s 已到期", it.id),
				fmt.Sprintf("到期时间 %s。%s",
					it.expires.Local().Format("2006-01-02 15:04"),
					map[bool]string{
						true:  "已按配置自动停止（数据保留），续期后可重新启动。",
						false: "当前配置为「仅告警不自动停止」，实例仍在运行。",
					}[it.autostop]))
			s.resolve(ctx, "instance_expiring", it.id)

			if it.autostop && it.status == "running" {
				if err := s.opts.InstanceStopper(it.id); err != nil {
					s.log().Warn("实例到期自动停止失败", "instance", it.id, "error", err)
				} else {
					s.log().Warn("实例已到期，已自动停止", "instance", it.id,
						"expires", it.expires.Local().Format(time.RFC3339))
				}
			}

		case left <= time.Duration(it.notice)*24*time.Hour:
			// 临近到期。天数与面板列表保持一致（都向上取整）——
			// 告警说"还有 1 天"而界面上写"还有 2 天"会让人怀疑哪个是真的。
			days := int(math.Ceil(left.Hours() / 24))
			s.raise(ctx, "instance_expiring", "warning", it.id,
				fmt.Sprintf("实例 %s 将于 %d 天后到期", it.id, days),
				fmt.Sprintf("到期时间 %s（%s）。%s",
					it.expires.Local().Format("2006-01-02 15:04"),
					humanLeft(left),
					map[bool]string{
						true:  "到期后会自动停止（数据保留）。",
						false: "当前配置为「仅告警不自动停止」。",
					}[it.autostop]))
			s.resolve(ctx, "instance_expired", it.id)

		default:
			// 还早，清掉可能残留的告警（例如刚续期过）
			s.resolve(ctx, "instance_expiring", it.id)
			s.resolve(ctx, "instance_expired", it.id)
		}
	}
}

// humanLeft 把剩余时长说成人话。
func humanLeft(d time.Duration) string {
	if d <= 0 {
		return "已到期"
	}
	days := int(d.Hours() / 24)
	if days >= 1 {
		return fmt.Sprintf("剩余 %d 天 %d 小时", days, int(d.Hours())%24)
	}
	if h := int(d.Hours()); h >= 1 {
		return fmt.Sprintf("剩余 %d 小时", h)
	}
	return fmt.Sprintf("剩余 %d 分钟", int(d.Minutes()))
}

// taskGraceWindow 新建任务在首次匹配后允许的补跑窗口。
//
// 没有历史记录时（last_run 为空），只有当"最近一次应触发时刻"落在
// 这个窗口内才执行。否则会出现：用户新建一条「每天 03:00 关机」的任务，
// 恰好在 15:00 保存 —— 若不加限制，Prev() 会返回当天 03:00，
// 面板立刻就把实例关掉，这不是用户想要的。
const taskGraceWindow = 90 * time.Second

// runDueTasks 执行到期的定时指令任务。
//
// 判断方式：取「最近一次应触发时刻」（Prev），只要它晚于上次执行时间，
// 就说明这一档还没跑过。相比"在整点触发"的写法，这种幂等式判断对
// 面板短暂重启、调度器 tick 抖动都免疫，不会漏跑也不会重复跑。
//
// 已知取舍：面板停机期间错过的任务**不补跑**。开机/关机这类动作
// 补跑往往比不跑更糟（比如深夜的关机任务在早上补跑）。
func (s *Scheduler) runDueTasks(ctx context.Context) {
	if s.opts.TaskRunner == nil {
		return
	}
	rows, err := s.opts.DB.Query(`SELECT id, cron, last_run FROM instance_tasks WHERE enabled = 1`)
	if err != nil {
		return
	}
	type due struct {
		id int64
	}
	var list []due
	now := time.Now()
	for rows.Next() {
		var (
			id      int64
			spec    string
			lastRun sql.NullTime
		)
		if err := rows.Scan(&id, &spec, &lastRun); err != nil {
			continue
		}
		sched, err := cron.Parse(spec)
		if err != nil {
			// 表达式非法：禁用该任务而不是每轮重试刷日志
			s.log().Warn("定时任务表达式非法，已自动禁用", "task", id, "cron", spec, "error", err)
			if _, err := s.opts.DB.Exec(
				`UPDATE instance_tasks SET enabled = 0, last_error = ? WHERE id = ?`,
				"cron 表达式非法："+err.Error(), id); err != nil {
				s.log().Warn("禁用非法定时任务失败", "task", id, "error", err)
			}
			continue
		}
		prev := sched.Prev(now)
		if prev.IsZero() {
			continue
		}
		switch {
		case lastRun.Valid:
			if prev.After(lastRun.Time) {
				list = append(list, due{id})
			}
		case now.Sub(prev) <= taskGraceWindow:
			list = append(list, due{id})
		}
	}
	rows.Close()

	for _, d := range list {
		if err := s.opts.TaskRunner(d.id); err != nil {
			s.log().Warn("定时任务执行失败", "task", d.id, "error", err)
			continue
		}
		s.log().Info("定时任务已执行", "task", d.id)
	}
}

// runDueBackups 执行到期的备份计划。
// 间隔与保留策略都取自实例指派的策略（policy_id=0 → 默认策略）。
func (s *Scheduler) runDueBackups(ctx context.Context) {
	rows, err := s.opts.DB.Query(`
		SELECT s.instance_id, s.interval_hours, s.include_config, s.policy_id, s.last_run,
		       COALESCE(p.auto_interval_hours, 0), COALESCE(p.manual_keep, 0),
		       COALESCE(p.tiers, ''), COALESCE(p.include_config, 1)
		FROM backup_schedules s
		LEFT JOIN backup_policies p ON p.id = s.policy_id
		WHERE s.enabled = 1`)
	if err != nil {
		return
	}
	type job struct {
		instanceID string
		policy     retention.Policy
		includeCfg bool
	}
	var jobs []job
	for rows.Next() {
		var id string
		var hours, includeCfg, policyID int
		var lastRun sql.NullTime
		var pInterval, pManualKeep, pIncludeCfg int
		var pTiers string
		if err := rows.Scan(&id, &hours, &includeCfg, &policyID, &lastRun,
			&pInterval, &pManualKeep, &pTiers, &pIncludeCfg); err != nil {
			continue
		}

		// 生效的策略：实例指派了策略就用它，否则用内置默认
		policy := retention.DefaultPolicy()
		effectiveInclude := includeCfg == 1
		if policyID > 0 {
			var tiers []retention.Tier
			if json.Unmarshal([]byte(pTiers), &tiers) == nil && len(tiers) > 0 {
				policy.Tiers = tiers
			}
			if pManualKeep > 0 {
				policy.ManualKeep = pManualKeep
			}
			effectiveInclude = pIncludeCfg == 1
			if pInterval > 0 {
				hours = pInterval
			}
		}
		policy = policy.Normalize()

		if hours <= 0 {
			hours = 6
		}
		if lastRun.Valid && time.Since(lastRun.Time) < time.Duration(hours)*time.Hour {
			continue
		}
		jobs = append(jobs, job{instanceID: id, policy: policy, includeCfg: effectiveInclude})
	}
	rows.Close()

	for _, j := range jobs {
		if s.opts.Backup == nil {
			return
		}
		err := s.opts.Backup(j.instanceID, j.includeCfg, j.policy)
		if err != nil {
			s.opts.DB.Exec(`UPDATE backup_schedules SET last_run = CURRENT_TIMESTAMP, last_error = ? WHERE instance_id = ?`,
				err.Error(), j.instanceID)
			s.raise(ctx, "backup_failed", "warning", j.instanceID,
				fmt.Sprintf("实例 %s 定时备份失败", j.instanceID), err.Error())
			continue
		}
		s.opts.DB.Exec(`UPDATE backup_schedules SET last_run = CURRENT_TIMESTAMP, last_error = '' WHERE instance_id = ?`, j.instanceID)
		s.resolve(ctx, "backup_failed", j.instanceID)
		s.log().Info("定时备份完成", "instance", j.instanceID)
	}
}

// checkInstances 检查实例状态：期望运行但实际停止（崩溃）时告警。
func (s *Scheduler) checkInstances(ctx context.Context) {
	if s.opts.InstanceProbe == nil {
		return
	}
	rows, err := s.opts.DB.Query(`SELECT instance_id, status FROM instances`)
	if err != nil {
		return
	}
	type item struct {
		id, status string
	}
	var list []item
	for rows.Next() {
		var it item
		if err := rows.Scan(&it.id, &it.status); err == nil {
			list = append(list, it)
		}
	}
	rows.Close()

	for _, it := range list {
		actual, err := s.opts.InstanceProbe(it.id)
		if err != nil || actual == "" || actual == "not_found" {
			continue
		}

		// 状态自愈：把实测状态回写到数据库。
		//
		// 此前只有「打开实例列表」时才会自愈（instances.go 里也有一次回写），
		// 若长期无人访问界面，数据库会一直停留在"运行中"，
		// 而实际进程早已退出 —— 界面与事实不符，容易误判。
		// 健康检查每分钟必跑，是更可靠的回写时机。
		if actual != it.status {
			if _, err := s.opts.DB.Exec(
				`UPDATE instances SET status = ? WHERE instance_id = ?`, actual, it.id); err != nil {
				s.log().Warn("回写实例状态失败", "instance", it.id, "error", err)
			} else {
				s.log().Info("实例状态已同步", "instance", it.id, "from", it.status, "to", actual)
			}
		}

		if actual == "stopped" && it.status == "running" {
			s.raise(ctx, "instance_crash", "critical", it.id,
				fmt.Sprintf("实例 %s 已停止（非面板操作）", it.id),
				"进程可能崩溃或被系统终止，请检查控制台日志")
			continue
		}

		// 仅当**确认实例在运行**时才认为崩溃告警已恢复。
		//
		// 注意不能沿用上面写入后的 it.status 判断：状态自愈会把数据库更新为
		// stopped，下一轮检查就成了「数据库=stopped 且 实际=stopped」，
		// 若不看 actual 就会立刻把刚产生的崩溃告警消掉，告警形同虚设。
		if actual == "running" {
			s.resolve(ctx, "instance_crash", it.id)
		}
	}
}

// checkNodes 检查节点在线状态与磁盘空间。
func (s *Scheduler) checkNodes(ctx context.Context) {
	if s.opts.NodeProbe == nil {
		return
	}
	rows, err := s.opts.DB.Query(`SELECT id, name FROM nodes`)
	if err != nil {
		return
	}
	type node struct {
		id   int64
		name string
	}
	var list []node
	for rows.Next() {
		var n node
		if err := rows.Scan(&n.id, &n.name); err == nil {
			list = append(list, n)
		}
	}
	rows.Close()

	for _, n := range list {
		online, freeMB, err := s.opts.NodeProbe(n.id, n.name)
		if err != nil {
			continue
		}
		if !online {
			s.raise(ctx, "node_offline", "critical", n.name,
				fmt.Sprintf("节点 %s 离线", n.name), "Daemon 已失去联系，请检查节点状态")
		} else {
			s.resolve(ctx, "node_offline", n.name)
		}

		if online && freeMB > 0 && freeMB < s.opts.DiskWarnMB {
			s.raise(ctx, "disk_low", "warning", n.name,
				fmt.Sprintf("节点 %s 磁盘空间不足", n.name),
				fmt.Sprintf("剩余 %d MB，低于阈值 %d MB", freeMB, s.opts.DiskWarnMB))
		} else if online && freeMB >= s.opts.DiskWarnMB {
			s.resolve(ctx, "disk_low", n.name)
		}
	}
}

// raise 新增告警（同一 kind+target 已有活跃告警时不重复创建）。
func (s *Scheduler) raise(ctx context.Context, kind, severity, target, message, detail string) {
	var cnt int
	if err := s.opts.DB.QueryRow(
		`SELECT COUNT(*) FROM alerts WHERE kind = ? AND target = ? AND active = 1`,
		kind, target).Scan(&cnt); err != nil {
		return
	}
	if cnt > 0 {
		return
	}
	if _, err := s.opts.DB.Exec(
		`INSERT INTO alerts (kind, severity, target, message, detail) VALUES (?, ?, ?, ?, ?)`,
		kind, severity, target, message, detail); err != nil {
		return
	}
	s.log().Warn("产生告警", "kind", kind, "target", target, "message", message)
}

// resolve 关闭指定告警。
func (s *Scheduler) resolve(ctx context.Context, kind, target string) {
	res, err := s.opts.DB.Exec(
		`UPDATE alerts SET active = 0, resolved_at = CURRENT_TIMESTAMP WHERE kind = ? AND target = ? AND active = 1`,
		kind, target)
	if err != nil {
		return
	}
	if n, _ := res.RowsAffected(); n > 0 {
		s.log().Info("告警已恢复", "kind", kind, "target", target)
	}
}

func (s *Scheduler) log() *slog.Logger {
	if s.opts.Logger != nil {
		return s.opts.Logger
	}
	return slog.Default()
}

// metricsRetentionDays 指标历史保留天数。
// 每分钟一行 → 7 天约 1 万行/实例，SQLite 完全无压力。
const metricsRetentionDays = 7

// sampleMetrics 采样所有运行中实例的指标并落库。
//
// 采样频率与调度器 tick 一致（默认 1 分钟）。不做更密的原因：
// 统计页展示的是分钟级趋势，更密只会徒增数据量。
func (s *Scheduler) sampleMetrics(ctx context.Context) {
	if s.opts.MetricsSampler == nil {
		return
	}
	rows, err := s.opts.DB.Query(
		`SELECT instance_id FROM instances WHERE status = 'running'`)
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
		cpu, mem, players, tps, diskUsed, ok := s.opts.MetricsSampler(id)
		if !ok {
			continue
		}
		// 磁盘配额检查与采样共用这次调用，避免重复遍历目录
		s.checkDiskLimit(ctx, id, diskUsed)
		if _, err := s.opts.DB.Exec(
			`INSERT INTO instance_metrics (instance_id, cpu_percent, mem_used, players, tps)
			 VALUES (?, ?, ?, ?, ?)`, id, cpu, mem, players, tps); err != nil {
			s.log().Warn("写入指标采样失败", "instance", id, "error", err)
		}
	}

	// 清理过期数据（按天做一次即可，用 id 取模避开每 tick 都执行）
	if time.Now().Minute() == 7 {
		if _, err := s.opts.DB.Exec(
			`DELETE FROM instance_metrics WHERE sampled_at < datetime('now', ?)`,
			fmt.Sprintf("-%d days", metricsRetentionDays)); err != nil {
			s.log().Warn("清理过期指标失败", "error", err)
		}
	}
}

// 磁盘配额的告警阈值（占配额的比例）。
//
// 分两档是为了给用户留出处理时间：警告档提示"该清理了"，
// 严重档提示"再不处理就要出问题"。
const (
	diskWarnRatio     = 0.85
	diskCriticalRatio = 0.95
)

// checkDiskLimit 检查实例磁盘用量是否触及配额。
//
// 这是**软配额**：面板定期巡检、分级告警，超过硬上限时（若管理员开启了
// 自动停机）停止实例。相比内核实时配额，它无法阻止瞬时写入，
// 但能在写满整盘之前介入 —— 而"写满整盘"才是真正的风险。
func (s *Scheduler) checkDiskLimit(ctx context.Context, instanceID string, diskUsed int64) {
	var limitMB, autostop int
	if err := s.opts.DB.QueryRow(
		`SELECT disk_limit_mb, disk_autostop FROM instances WHERE instance_id = ?`,
		instanceID).Scan(&limitMB, &autostop); err != nil {
		return
	}
	if limitMB <= 0 {
		return // 未设配额
	}

	limitBytes := int64(limitMB) * 1024 * 1024
	ratio := float64(diskUsed) / float64(limitBytes)
	usedMB := diskUsed / 1024 / 1024

	switch {
	case ratio >= diskCriticalRatio:
		s.raise(ctx, "instance_disk_critical", "critical", instanceID,
			fmt.Sprintf("实例 %s 磁盘占用已达配额 %.0f%%", instanceID, ratio*100),
			fmt.Sprintf("已用 %d MB / 配额 %d MB。请清理存档或调高配额，否则将影响该节点上所有实例。", usedMB, limitMB))
		if autostop == 1 {
			s.stopForDiskLimit(ctx, instanceID, usedMB, int64(limitMB))
		}
	case ratio >= diskWarnRatio:
		s.raise(ctx, "instance_disk_high", "warning", instanceID,
			fmt.Sprintf("实例 %s 磁盘占用接近配额", instanceID),
			fmt.Sprintf("已用 %d MB / 配额 %d MB（%.0f%%）", usedMB, limitMB, ratio*100))
		s.resolve(ctx, "instance_disk_critical", instanceID)
	default:
		// 回落到安全水位，自动清除两类磁盘告警
		s.resolve(ctx, "instance_disk_critical", instanceID)
		s.resolve(ctx, "instance_disk_high", instanceID)
	}
}

// stopForDiskLimit 因超出磁盘硬上限而停止实例。
//
// 只在管理员显式开启 disk_autostop 时才会走到这里 ——
// 停机有破坏性（玩家会被踢下线、未保存进度可能丢失），
// 不应是默认行为。
func (s *Scheduler) stopForDiskLimit(ctx context.Context, instanceID string, usedMB, limitMB int64) {
	if s.opts.InstanceStopper == nil {
		return
	}
	if err := s.opts.InstanceStopper(instanceID); err != nil {
		s.log().Warn("磁盘超限自动停机失败", "instance", instanceID, "error", err)
		return
	}
	s.log().Warn("实例磁盘超限，已自动停止",
		"instance", instanceID, "used_mb", usedMB, "limit_mb", limitMB)
	s.raise(ctx, "instance_disk_autostop", "critical", instanceID,
		fmt.Sprintf("实例 %s 因磁盘超限被自动停止", instanceID),
		fmt.Sprintf("已用 %d MB 超过配额 %d MB。请清理后手动启动。", usedMB, limitMB))
}
