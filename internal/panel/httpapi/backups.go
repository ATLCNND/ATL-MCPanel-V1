package httpapi

import (
	"context"
	"encoding/json"
	"net/http"

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
	resp, err := cli.ListBackups(context.Background(), &pb.ListBackupsRequest{InstanceId: instanceID})
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
	}
	list := []item{}
	for _, b := range resp.Backups {
		list = append(list, item{BackupID: b.BackupId, Name: b.Name, Size: b.Size, CreatedAt: b.CreatedAt})
	}
	writeJSON(w, http.StatusOK, list)
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
	cli, _, err := s.getDaemonClient(instanceID)
	if err != nil {
		writeErr(w, http.StatusNotFound, "实例不存在")
		return
	}
	resp, err := cli.Backup(context.Background(), &pb.BackupRequest{
		InstanceId:    instanceID,
		Name:          req.Name,
		IncludeConfig: req.IncludeConfig,
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
	resp, err := cli.DeleteBackup(context.Background(), &pb.DeleteBackupRequest{InstanceId: instanceID, BackupId: backupID})
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
	resp, err := cli.Restore(context.Background(), &pb.RestoreRequest{InstanceId: instanceID, BackupId: req.BackupID})
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
