package httpapi

import (
	"net/http"
	"strconv"
	"time"
)

// metricsPoint 单个采样点
type metricsPoint struct {
	CPU     float64 `json:"cpu"`
	Mem     int64   `json:"mem"`
	Players int     `json:"players"`
	TPS     float64 `json:"tps"`
	At      string  `json:"at"`
}

// handleInstanceStats GET /api/instances/{id}/stats?hours=24
//
// 返回指标历史（由调度器每分钟采样落库）。用于「统计」页绘制趋势。
//
// 数据量与查询时间的关系（每实例每分钟一行）：
//
//	1 小时 ≈ 60 行   6 小时 ≈ 360 行   24 小时 ≈ 1440 行   7 天 ≈ 1 万行
//
// 为控制响应体大小，超过 6 小时的数据按 5 分钟降采样返回。
func (s *Server) handleInstanceStats(w http.ResponseWriter, r *http.Request) {
	instanceID := r.PathValue("id")
	if !s.requireInstanceLevel(w, r, instanceID, LevelViewer) {
		return
	}

	hours := 24
	if v := r.URL.Query().Get("hours"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 168 {
			hours = n
		}
	}

	rows, err := s.db.Query(
		`SELECT cpu_percent, mem_used, players, tps, sampled_at
		 FROM instance_metrics
		 WHERE instance_id = ? AND sampled_at >= datetime('now', ?)
		 ORDER BY sampled_at ASC`,
		instanceID, "-"+strconv.Itoa(hours)+" hours")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer rows.Close()

	all := []metricsPoint{}
	for rows.Next() {
		var pt metricsPoint
		var at time.Time
		if err := rows.Scan(&pt.CPU, &pt.Mem, &pt.Players, &pt.TPS, &at); err != nil {
			continue
		}
		pt.At = at.Format(time.RFC3339)
		all = append(all, pt)
	}

	// 降采样：长时间跨度按 5 分钟取一个点，避免返回上千个点拖慢前端
	step := 1
	if hours > 6 {
		step = 5
	}
	points := make([]metricsPoint, 0, len(all)/step+1)
	for i := 0; i < len(all); i += step {
		points = append(points, all[i])
	}

	// 汇总：用于展示峰值与均值，比只看曲线更有信息量
	var sumCPU, sumMem, sumTPS, sumPlayers float64
	var peakCPU, peakMem float64
	var peakPlayers int
	for _, pt := range all {
		sumCPU += pt.CPU
		sumMem += float64(pt.Mem)
		sumTPS += pt.TPS
		sumPlayers += float64(pt.Players)
		if pt.CPU > peakCPU {
			peakCPU = pt.CPU
		}
		if float64(pt.Mem) > peakMem {
			peakMem = float64(pt.Mem)
		}
		if pt.Players > peakPlayers {
			peakPlayers = pt.Players
		}
	}
	n := float64(len(all))
	avg := map[string]float64{}
	if n > 0 {
		avg = map[string]float64{
			"cpu":     sumCPU / n,
			"mem":     sumMem / n,
			"tps":     sumTPS / n,
			"players": sumPlayers / n,
		}
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"instance_id": instanceID,
		"hours":       hours,
		"step":        step,
		"points":      points,
		"total":       len(all),
		"avg":         avg,
		"peak": map[string]interface{}{
			"cpu":     peakCPU,
			"mem":     int64(peakMem),
			"players": peakPlayers,
		},
	})
}
