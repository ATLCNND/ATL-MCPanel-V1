package httpapi

import (
	"net/http"
	"time"
)

// nodeMonitorView 节点监控视图（**所有登录用户可见**）。
//
// 有意不包含 SSH 账号/凭据等敏感字段 —— 这类信息只在管理员专属的
// 「节点管理」中可见。普通用户只需要知道"实例跑在哪个节点、节点是否健康"。
type nodeMonitorView struct {
	ID          int64   `json:"id"`
	Name        string  `json:"name"`
	Status      string  `json:"status"`
	CPU         int     `json:"cpu"`       // 核心数
	MemTotal    int64   `json:"mem_total"` // 总内存（字节）
	CPUPercent  float64 `json:"cpu_percent"`
	MemUsed     int64   `json:"mem_used"`
	DiskUsed    int64   `json:"disk_used"`
	DiskTotal   int64   `json:"disk_total"`
	LastSeen    string  `json:"last_seen"`
	Online      bool    `json:"online"`
	Instances   int     `json:"instances"` // 实例总数
	Running     int     `json:"running"`   // 运行中的实例数
	Version     string  `json:"version"`   // 预留：Daemon 版本
	StaleAfterS int     `json:"stale_after_s"`
}

// handleMonitorNodes GET /api/monitor/nodes
//
// 节点监控列表，供所有已登录用户查看（只读）。
func (s *Server) handleMonitorNodes(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.Query(`
		SELECT id, name, status, cpu, mem,
		       cpu_percent, mem_used, disk_used, disk_total, last_seen
		FROM nodes ORDER BY id`)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()

	const staleAfter = 90 // 与告警判定保持一致：心跳间隔 10s，90s 未上报视为离线

	list := []nodeMonitorView{}
	for rows.Next() {
		var v nodeMonitorView
		var lastSeen *time.Time
		if err := rows.Scan(&v.ID, &v.Name, &v.Status, &v.CPU, &v.MemTotal,
			&v.CPUPercent, &v.MemUsed, &v.DiskUsed, &v.DiskTotal, &lastSeen); err != nil {
			continue
		}
		v.StaleAfterS = staleAfter
		if lastSeen != nil {
			v.LastSeen = lastSeen.Format(time.RFC3339)
			v.Online = v.Status == "online" && time.Since(*lastSeen) < staleAfter*time.Second
		}
		list = append(list, v)
	}
	rows.Close()

	// 统计每台节点的实例数与运行数
	for i := range list {
		_ = s.db.QueryRow(`SELECT COUNT(*) FROM instances WHERE node_id = ?`, list[i].ID).Scan(&list[i].Instances)
		_ = s.db.QueryRow(`SELECT COUNT(*) FROM instances WHERE node_id = ? AND status = 'running'`, list[i].ID).Scan(&list[i].Running)
	}

	// 汇总
	var totalInst, totalRunning int
	for _, n := range list {
		totalInst += n.Instances
		totalRunning += n.Running
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"nodes":             list,
		"total_nodes":       len(list),
		"online_nodes":      countOnline(list),
		"total_instances":   totalInst,
		"running_instances": totalRunning,
	})
}

func countOnline(list []nodeMonitorView) int {
	n := 0
	for _, v := range list {
		if v.Online {
			n++
		}
	}
	return n
}

// monitorInstancesView 节点监控页上的实例简表（普通用户只看到自己有权限的实例）。
type monitorInstanceView struct {
	InstanceID string `json:"instance_id"`
	Name       string `json:"name"`
	NodeID     int64  `json:"node_id"`
	NodeName   string `json:"node_name"`
	CoreType   string `json:"core_type"`
	Port       int32  `json:"port"`
	MaxMem     string `json:"max_mem"`
	CPUQuota   int    `json:"cpu_quota"`
	Status     string `json:"status"`
	LiveStatus string `json:"live_status"`
}

// handleMonitorInstances GET /api/monitor/instances
//
// 按节点分组展示实例（管理员看全部，普通用户只看被授权的），
// 用于节点监控页了解"哪些实例跑在哪台节点上"。
func (s *Server) handleMonitorInstances(w http.ResponseWriter, r *http.Request) {
	userID := currentUserID(r)
	role, _ := r.Context().Value(ctxKeyRole).(string)

	q := `
		SELECT i.instance_id, i.name, i.node_id, COALESCE(n.name,''), i.core_type,
		       i.port, i.max_mem, i.cpu_quota, i.status
		FROM instances i LEFT JOIN nodes n ON n.id = i.node_id`
	args := []interface{}{}
	if role != "admin" {
		q += ` JOIN instance_assignments a ON a.instance_id = i.instance_id AND a.user_id = ?`
		args = append(args, userID)
	}
	q += ` ORDER BY i.node_id, i.instance_id`

	rows, err := s.db.Query(q, args...)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()

	list := []monitorInstanceView{}
	for rows.Next() {
		var v monitorInstanceView
		if err := rows.Scan(&v.InstanceID, &v.Name, &v.NodeID, &v.NodeName, &v.CoreType,
			&v.Port, &v.MaxMem, &v.CPUQuota, &v.Status); err != nil {
			continue
		}
		list = append(list, v)
	}
	rows.Close()

	writeJSON(w, http.StatusOK, list)
}
