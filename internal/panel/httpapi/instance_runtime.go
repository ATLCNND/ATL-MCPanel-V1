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

	// 该实例的对外地址（「穿透管理」里配的域名 + 这条隧道的端口）。
	//
	// 以前这里直接把**隧道级原始值**返回给前端，前端又优先用它 ——
	// 管理员只填域名不填端口时，实例页顶部就显示成 `mc.example.com`，
	// 玩家拿这个地址连不上（端口明明就在旁边一列里）。
	// 现在统一走 publicAddress：域名归一化 + 按需补端口。
	var publicAddr string
	{
		var tunnelDomain, lineDomain, lineHost string
		var remotePort int32
		_ = s.db.QueryRow(`
			SELECT COALESCE(t.display_domain,''), COALESCE(f.display_domain,''), COALESCE(f.host,''), t.remote_port
			FROM tunnels t LEFT JOIN frps_servers f ON f.id = t.frps_id
			WHERE t.instance_id = ? ORDER BY t.remote_port LIMIT 1`,
			instanceID).Scan(&tunnelDomain, &lineDomain, &lineHost, &remotePort)
		if remotePort > 0 {
			publicAddr = publicAddress(tunnelDomain, lineDomain, lineHost, remotePort)
		}
	}

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
		// public_address 才是"该连的地址"；display_domain 保留给需要原始值的界面
		"public_address": publicAddr,
		"net_scope":      resp.NetScope,
		// 运行提示（空 = 正常）：JDK 回退、cgroup 限额没生效。
		// 这两条以前只写进 daemon 内存，界面上看不到 —— 于是"选了 17 却在跑 21"
		// 这种问题只能靠翻日志发现。
		"java_note":     resp.JavaNote,
		"limit_warning": resp.LimitWarning,
	})
}
