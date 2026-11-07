package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	pb "github.com/ATLCNND/ATL-MCPanel/internal/proto/mcpanel"
)

// handleListBackups GET /api/instances/{id}/backups
func (s *Server) handleListBackups(w http.ResponseWriter, r *http.Request) {
	instanceID := r.PathValue("id")
	if !s.requireInstanceLevel(w, r, instanceID, LevelCollab) {
		return
	}
	cli, _, err := s.getDaemonClient(instanceID)
	if err != nil {
		writeErr(w, http.StatusNotFound, "实例不存在")
		return
	}
	// backup_root：与 Backup 同一口径。备份写到冷存储之后，列表也必须从冷存储读，
	// 否则界面上看不见任何一份**实际**的备份（2026-11-07 真机撞到的就是这个）。
	resp, err := cli.ListBackups(context.Background(), &pb.ListBackupsRequest{
		InstanceId: instanceID,
		BackupRoot: s.nodeBackupRoot(instanceID),
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !resp.Success {
		writeErr(w, http.StatusInternalServerError, resp.Error)
		return
	}
	type item struct {
		BackupID  string `json:"backup_id"`
		Name      string `json:"name"`
		Size      int64  `json:"size"`
		CreatedAt int64  `json:"created_at"`
		// Kind / BaseID 给界面「按链显示」用：自动备份是增量的，一条链 = 一份基线 + 若干增量。
		// 老备份（没有清单）两个字段为空 → 界面按独立备份显示，行为不变。
		Kind   string `json:"kind,omitempty"`
		BaseID string `json:"base_id,omitempty"`
	}
	list := []item{}
	for _, b := range resp.Backups {
		list = append(list, item{
			BackupID:  b.BackupId,
			Name:      b.Name,
			Size:      b.Size,
			CreatedAt: b.CreatedAt,
			Kind:      b.Kind,
			BaseID:    b.BaseId,
		})
	}
	writeJSON(w, http.StatusOK, list)
}

// autoBackupName 自动备份的固定名称。**这是面板与节点之间的约定**：
// 名字等于 auto 的备份 = 自动备份（保留策略按梯度滚动淘汰），其余 = 手动备份。
const autoBackupName = "auto"

// isAutoBackupName 是否是被当作自动备份的名字。
//
// 必须**精确**匹配（不区分大小写），不能写成"以 auto 开头"：
// 用户给手动备份起名"自动化前测试"是完全合理的，前缀匹配会把他的备份
// 当成自动备份删掉 —— 那正是这次要修的 bug。
func isAutoBackupName(name string) bool {
	return strings.EqualFold(strings.TrimSpace(name), autoBackupName)
}

// manualBackupName 给没填名字的手动备份生成一个名字。
//
// 为什么必须生成而不是留空：留空的话 Daemon 会填成 "auto"，
// 于是用户"立即备份"出来的东西在保留策略眼里就是**自动备份**，
// 会被梯度规则滚动淘汰掉 —— 用户看到的现象就是"我手动做的备份自己没了"。
// 手动备份是用户明确的意图，不该与自动备份混为一谈（见 retention 包的说明）。
//
// 用中文前缀 + 时间戳：既一眼看出是手动创建的，又天然不重名。
func manualBackupName(now time.Time) string {
	return "手动-" + now.Format("20060102-150405")
}

// handleCreateBackup POST /api/instances/{id}/backups  {name, include_config}
func (s *Server) handleCreateBackup(w http.ResponseWriter, r *http.Request) {
	instanceID := r.PathValue("id")
	if !s.requireInstanceLevel(w, r, instanceID, LevelOwner) {
		return
	}
	var req struct {
		Name          string `json:"name"`
		IncludeConfig bool   `json:"include_config"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "无效请求体")
		return
	}

	name := strings.TrimSpace(req.Name)
	// 挡住"把手动备份叫 auto"：那不是命名偏好问题，而是会让这份备份
	// 被保留策略当作自动备份滚动淘汰 —— 与其让用户在几天后发现备份没了，
	// 不如现在就明确拒绝并说清原因。
	if isAutoBackupName(name) {
		writeErr(w, http.StatusBadRequest,
			"备份名不能是 auto：这个名字被系统用来标记「自动备份」，"+
				"保留策略会按梯度滚动淘汰它。请换一个名字（留空则自动生成）")
		return
	}
	if name == "" {
		name = manualBackupName(time.Now())
	}

	cli, _, err := s.getDaemonClient(instanceID)
	if err != nil {
		writeErr(w, http.StatusNotFound, "实例不存在")
		return
	}
	// **手动备份 = 完整快照**（用户明确定义）：强制带上配置与插件等全部业务文件，
	// 不再由请求方决定。理由：手动备份的用途就是「出事了能整份恢复」，
	// 一份缺了 server.properties / 权限配置的备份，恢复完还得靠记忆补齐 —— 那不叫完整。
	//
	// 仍然排除的可重建目录（logs / cache / libraries / versions / backups）见 daemon 侧 skipDirs，
	// 它们要么体积大、要么能重新生成，放进快照只会让备份变慢变大。
	_ = req.IncludeConfig // 保留字段以兼容老调用方，但不再生效
	resp, err := cli.Backup(context.Background(), &pb.BackupRequest{
		InstanceId:    instanceID,
		Name:          name,
		IncludeConfig: true,
		// 节点级备份根目录（管理员可配）：改完立即对后续备份生效
		BackupRoot: s.nodeBackupRoot(instanceID),
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !resp.Success {
		writeErr(w, http.StatusInternalServerError, resp.Message)
		return
	}
	s.audit(r, "create_backup", instanceID, resp.BackupId)
	writeJSON(w, http.StatusCreated, map[string]string{"backup_id": resp.BackupId, "message": resp.Message})
}

// handleDeleteBackup DELETE /api/instances/{id}/backups?backup_id=xxx
func (s *Server) handleDeleteBackup(w http.ResponseWriter, r *http.Request) {
	instanceID := r.PathValue("id")
	if !s.requireInstanceLevel(w, r, instanceID, LevelOwner) {
		return
	}
	backupID := r.URL.Query().Get("backup_id")
	cli, _, err := s.getDaemonClient(instanceID)
	if err != nil {
		writeErr(w, http.StatusNotFound, "实例不存在")
		return
	}
	resp, err := cli.DeleteBackup(context.Background(), &pb.DeleteBackupRequest{
		InstanceId: instanceID,
		BackupId:   backupID,
		BackupRoot: s.nodeBackupRoot(instanceID),
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !resp.Success {
		writeErr(w, http.StatusInternalServerError, resp.Error)
		return
	}
	s.audit(r, "delete_backup", instanceID, backupID)
	writeJSON(w, http.StatusOK, map[string]string{"message": resp.Message})
}

// handleRestoreBackup POST /api/instances/{id}/restore  {backup_id}
func (s *Server) handleRestoreBackup(w http.ResponseWriter, r *http.Request) {
	instanceID := r.PathValue("id")
	if !s.requireInstanceLevel(w, r, instanceID, LevelOwner) {
		return
	}
	var req struct {
		BackupID string `json:"backup_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "无效请求体")
		return
	}
	cli, _, err := s.getDaemonClient(instanceID)
	if err != nil {
		writeErr(w, http.StatusNotFound, "实例不存在")
		return
	}
	resp, err := cli.Restore(context.Background(), &pb.RestoreRequest{
		InstanceId: instanceID,
		BackupId:   req.BackupID,
		BackupRoot: s.nodeBackupRoot(instanceID),
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !resp.Success {
		writeErr(w, http.StatusInternalServerError, resp.Error)
		return
	}
	s.audit(r, "restore_backup", instanceID, req.BackupID)
	writeJSON(w, http.StatusOK, map[string]string{"message": resp.Message})
}
