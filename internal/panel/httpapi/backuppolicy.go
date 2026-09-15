package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ATLCNND/ATL-MCPanel/internal/panel/retention"
	pb "github.com/ATLCNND/ATL-MCPanel/internal/proto/mcpanel"
)

// ---- 备份保留策略（管理员可配置的「备份组」）----

type backupPolicyView struct {
	ID                int64            `json:"id"`
	Name              string           `json:"name"`
	Remark            string           `json:"remark"`
	AutoIntervalHours int              `json:"auto_interval_hours"`
	ManualKeep        int              `json:"manual_keep"`
	Tiers             []retention.Tier `json:"tiers"`
	IncludeConfig     bool             `json:"include_config"`
	IsDefault         bool             `json:"is_default"`
	UsedBy            int              `json:"used_by"` // 被多少个实例引用
	Describe          string           `json:"describe"`
}

// loadPolicy 读取指定策略；id<=0 时返回默认策略。
func (s *Server) loadPolicy(id int64) (retention.Policy, int, bool, error) {
	var name, tiersJSON string
	var manualKeep, interval, includeCfg int
	if id <= 0 {
		err := s.db.QueryRow(`
			SELECT name, manual_keep, tiers, auto_interval_hours, include_config
			FROM backup_policies WHERE is_default = 1 LIMIT 1`).
			Scan(&name, &manualKeep, &tiersJSON, &interval, &includeCfg)
		if err != nil {
			// 默认策略缺失时退回内置默认值，保证调度器始终能工作
			return retention.DefaultPolicy(), 6, true, nil
		}
	} else {
		err := s.db.QueryRow(`
			SELECT name, manual_keep, tiers, auto_interval_hours, include_config
			FROM backup_policies WHERE id = ?`, id).
			Scan(&name, &manualKeep, &tiersJSON, &interval, &includeCfg)
		if err != nil {
			return retention.DefaultPolicy(), 6, true, nil
		}
	}

	var tiers []retention.Tier
	if err := json.Unmarshal([]byte(tiersJSON), &tiers); err != nil {
		tiers = retention.DefaultPolicy().Tiers
	}
	p := retention.Policy{ManualKeep: manualKeep, Tiers: tiers}.Normalize()
	if interval <= 0 {
		interval = 6
	}
	return p, interval, includeCfg == 1, nil
}

// handleListBackupPolicies GET /api/backup-policies （仅管理员）
func (s *Server) handleListBackupPolicies(w http.ResponseWriter, r *http.Request) {
	if !requireAdminIn(w, r) {
		return
	}
	rows, err := s.db.Query(`
		SELECT id, name, remark, auto_interval_hours, manual_keep, tiers, include_config, is_default
		FROM backup_policies ORDER BY is_default DESC, id`)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()

	list := []backupPolicyView{}
	for rows.Next() {
		var v backupPolicyView
		var tiersJSON string
		var isDefault, includeCfg int
		if err := rows.Scan(&v.ID, &v.Name, &v.Remark, &v.AutoIntervalHours, &v.ManualKeep,
			&tiersJSON, &includeCfg, &isDefault); err != nil {
			continue
		}
		v.IsDefault = isDefault == 1
		v.IncludeConfig = includeCfg == 1
		_ = json.Unmarshal([]byte(tiersJSON), &v.Tiers)
		v.Describe = retention.Policy{ManualKeep: v.ManualKeep, Tiers: v.Tiers}.
			Normalize().Describe()
		_ = s.db.QueryRow(`SELECT COUNT(*) FROM backup_schedules WHERE policy_id = ?`, v.ID).Scan(&v.UsedBy)
		list = append(list, v)
	}
	writeJSON(w, http.StatusOK, list)
}

// handleSaveBackupPolicy POST /api/backup-policies  （仅管理员，新建或更新）
func (s *Server) handleSaveBackupPolicy(w http.ResponseWriter, r *http.Request) {
	if !requireAdminIn(w, r) {
		return
	}
	var req struct {
		ID                int64            `json:"id"`
		Name              string           `json:"name"`
		Remark            string           `json:"remark"`
		AutoIntervalHours int              `json:"auto_interval_hours"`
		ManualKeep        int              `json:"manual_keep"`
		Tiers             []retention.Tier `json:"tiers"`
		IncludeConfig     bool             `json:"include_config"`
		IsDefault         bool             `json:"is_default"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "无效请求体")
		return
	}
	if strings.TrimSpace(req.Name) == "" {
		writeErr(w, http.StatusBadRequest, "策略名称不能为空")
		return
	}
	if req.AutoIntervalHours <= 0 {
		req.AutoIntervalHours = 6
	}
	if req.ManualKeep < 0 {
		req.ManualKeep = 0
	}

	// 校验梯度：区间须为正且升序、保留份数为正
	norm := retention.Policy{ManualKeep: req.ManualKeep, Tiers: req.Tiers}.Normalize()
	if len(req.Tiers) > 0 && len(norm.Tiers) != len(req.Tiers) {
		writeErr(w, http.StatusBadRequest, "梯度配置非法：区间小时数与保留份数都必须为正整数")
		return
	}
	for i := 1; i < len(norm.Tiers); i++ {
		if norm.Tiers[i].WithinHours == norm.Tiers[i-1].WithinHours {
			writeErr(w, http.StatusBadRequest, "梯度区间不能重复")
			return
		}
	}

	tiersJSON, _ := json.Marshal(norm.Tiers)
	includeCfg := 0
	if req.IncludeConfig {
		includeCfg = 1
	}

	tx, err := s.db.Begin()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer func() { _ = tx.Rollback() }()

	// 同一时间只允许一个默认策略
	if req.IsDefault {
		if _, err := tx.Exec(`UPDATE backup_policies SET is_default = 0`); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
	}

	if req.ID > 0 {
		_, err = tx.Exec(`
			UPDATE backup_policies SET name=?, remark=?, auto_interval_hours=?, manual_keep=?,
			       tiers=?, include_config=?, is_default=?, updated_at=CURRENT_TIMESTAMP
			WHERE id=?`,
			req.Name, req.Remark, req.AutoIntervalHours, req.ManualKeep,
			string(tiersJSON), includeCfg, boolInt(req.IsDefault), req.ID)
	} else {
		_, err = tx.Exec(`
			INSERT INTO backup_policies (name, remark, auto_interval_hours, manual_keep, tiers, include_config, is_default)
			VALUES (?, ?, ?, ?, ?, ?, ?)`,
			req.Name, req.Remark, req.AutoIntervalHours, req.ManualKeep,
			string(tiersJSON), includeCfg, boolInt(req.IsDefault))
	}
	if err != nil {
		writeErr(w, http.StatusConflict, "保存失败（策略名可能重复）: "+err.Error())
		return
	}
	if err := tx.Commit(); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	s.audit(r, "save_backup_policy", req.Name,
		fmt.Sprintf("间隔 %dh 手动保留 %d 份 梯度 %s", req.AutoIntervalHours, req.ManualKeep, norm.Describe()))
	writeJSON(w, http.StatusOK, map[string]string{"message": "策略已保存"})
}

// handleDeleteBackupPolicy DELETE /api/backup-policies/{id} （仅管理员）
func (s *Server) handleDeleteBackupPolicy(w http.ResponseWriter, r *http.Request) {
	if !requireAdminIn(w, r) {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "id 无效")
		return
	}
	var isDefault int
	var usedBy int
	if err := s.db.QueryRow(`SELECT is_default FROM backup_policies WHERE id = ?`, id).Scan(&isDefault); err != nil {
		writeErr(w, http.StatusNotFound, "策略不存在")
		return
	}
	if isDefault == 1 {
		writeErr(w, http.StatusBadRequest, "默认策略不能删除，请先把其它策略设为默认")
		return
	}
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM backup_schedules WHERE policy_id = ?`, id).Scan(&usedBy)
	if usedBy > 0 {
		writeErr(w, http.StatusConflict,
			fmt.Sprintf("该策略正被 %d 个实例使用，请先改派后再删除", usedBy))
		return
	}
	if _, err := s.db.Exec(`DELETE FROM backup_policies WHERE id = ?`, id); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.audit(r, "delete_backup_policy", strconv.FormatInt(id, 10), "")
	writeJSON(w, http.StatusOK, map[string]string{"message": "策略已删除"})
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// pruneByPolicy 依据策略淘汰备份。
//
// 关键：**先按名称区分手动/自动** —— 自动备份由面板创建（名称固定为 "auto"），
// 手动备份带用户填写或自动生成的名称。两者分别计数，手动备份不会被
// 自动备份的滚动淘汰挤掉。
func (s *Server) pruneByPolicy(ctx context.Context, cli pb.DaemonServiceClient, instanceID string, policy retention.Policy) {
	list, err := cli.ListBackups(ctx, &pb.ListBackupsRequest{InstanceId: instanceID})
	if err != nil || !list.Success {
		return
	}
	entries := make([]retention.Entry, 0, len(list.Backups))
	for _, b := range list.Backups {
		entries = append(entries, retention.Entry{
			ID:        b.BackupId,
			CreatedAt: time.Unix(b.CreatedAt, 0),
			Manual:    !isAutoBackup(b.Name),
		})
	}
	if len(entries) == 0 {
		return
	}

	expired := retention.SelectExpired(entries, policy, time.Now())
	if len(expired) == 0 {
		return
	}
	sort.Slice(expired, func(i, j int) bool { return expired[i].CreatedAt.Before(expired[j].CreatedAt) })

	for _, e := range expired {
		if _, err := cli.DeleteBackup(ctx, &pb.DeleteBackupRequest{InstanceId: instanceID, BackupId: e.ID}); err == nil {
			s.logger.Info("按保留策略淘汰备份",
				"instance", instanceID, "backup", e.ID, "manual", e.Manual,
				"age_hours", int(time.Since(e.CreatedAt).Hours()))
		}
	}
}

// isAutoBackup 判断备份是否由自动计划创建（面板创建时名称为 "auto"）。
func isAutoBackup(name string) bool {
	return strings.EqualFold(strings.TrimSpace(name), "auto")
}
