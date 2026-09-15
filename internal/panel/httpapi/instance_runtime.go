package httpapi

import (
	"context"
	"net/http"
	"time"

	pb "github.com/ATLCNND/ATL-MCPanel/internal/proto/mcpanel"
)

// handleInstanceRuntime GET /api/instances/{id}/runtime
//
// 返回实例运行时长、累计启停次数、实例目录磁盘用量与对外域名。
// 这些数据只有节点 Daemon 才知道（它持有进程与文件系统视角），
// 因此由它上报，面板只做转发与补充。
func (s *Server) handleInstanceRuntime(w http.ResponseWriter, r *http.Request) {
	instanceID := r.PathValue("id")
	if !s.requireInstanceLevel(w, r, instanceID, LevelCollab) {
		return
	}
	nodeID, ok := s.instanceNodeID(instanceID)
	if !ok {
		writeErr(w, http.StatusNotFound, "实例不存在")
		return
	}
	cli, err := s.nodes.GetClient(nodeID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	resp, err := cli.GetInstanceRuntime(ctx, &pb.InstanceRequest{InstanceId: instanceID})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !resp.Success {
		writeErr(w, http.StatusNotFound, resp.Error)
		return
	}

	// 管理员为该实例配置的对外域名（在「穿透管理」里填写）
	var domain string
	_ = s.db.QueryRow(
		"SELECT display_domain FROM tunnels WHERE instance_id = ? AND display_domain != '' ORDER BY id LIMIT 1",
		instanceID,
	).Scan(&domain)

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"instance_id":    resp.InstanceId,
		"status":         resp.Status,
		"uptime_seconds": resp.UptimeSeconds,
		"last_start_at":  resp.LastStartAt,
		"start_count":    resp.StartCount,
		"stop_count":     resp.StopCount,
		"disk_used":      resp.DiskUsed,
		"disk_total":     resp.DiskTotal,
		"disk_free":      resp.DiskFree,
		"net_rx_rate":    resp.NetRxRate,
		"net_tx_rate":    resp.NetTxRate,
		"net_rx_total":   resp.NetRxTotal,
		"net_tx_total":   resp.NetTxTotal,
		"display_domain": domain,
		"net_scope":      resp.NetScope,
		// 运行提示（空 = 正常）：JDK 回退、cgroup 限额没生效。
		// 这两条以前只写进 daemon 内存，界面上看不到 —— 于是"选了 17 却在跑 21"
		// 这种问题只能靠翻日志发现。
		"java_note":     resp.JavaNote,
		"limit_warning": resp.LimitWarning,
	})
}
