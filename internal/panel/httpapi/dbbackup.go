package httpapi

import (
	"net/http"
	"time"
)

// handleListPanelBackups GET /api/panel-backups
//
// 列出面板自身数据库的备份（仅管理员）。
func (s *Server) handleListPanelBackups(w http.ResponseWriter, r *http.Request) {
	if !requireAdminIn(w, r) {
		return
	}
	if s.backup == nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"enabled": false,
			"dir":     "",
			"backups": []any{},
		})
		return
	}
	list, err := s.backup.List()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	lastRun := ""
	if t := s.backup.LastRun(); !t.IsZero() {
		lastRun = t.Format(time.RFC3339)
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"enabled":  true,
		"dir":      s.backup.Dir(),
		"keep":     s.backup.Keep(),
		"last_run": lastRun,
		"backups":  list,
	})
}

// handleCreatePanelBackup POST /api/panel-backups
//
// 立即创建一份面板数据库备份（仅管理员）。
func (s *Server) handleCreatePanelBackup(w http.ResponseWriter, r *http.Request) {
	if !requireAdminIn(w, r) {
		return
	}
	if s.backup == nil {
		writeErr(w, http.StatusServiceUnavailable, "面板数据库自动备份未启用")
		return
	}
	path, err := s.backup.Backup()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "备份失败: "+err.Error())
		return
	}
	s.audit(r, "backup_panel_db", path, "")
	writeJSON(w, http.StatusOK, map[string]string{
		"message": "面板数据库备份完成",
		"path":    path,
	})
}
