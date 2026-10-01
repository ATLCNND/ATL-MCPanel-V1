package httpapi

import (
	"net/http"
	"runtime"
	"time"
)

// handleDebugDBPool GET /api/debug/dbpool（仅总管理员）
//
// 为什么值得开这么一个小口子：2026-10-01 面板整体卡死那次，现象是
// "登录挂起、健康检查却 1ms"，靠一路猜（iptables？frp？节点？）才定位到
// **数据库连接池被占满**。当时最想要的就是 `db.Stats()` 里的那三个数：
// 打开了几个连接、几个在用、有多少请求在排队等连接。
//
// 现在它就在这个接口里（外加 goroutine 数与内存），排查同类问题时**一眼定性**：
//   · InUse 长期等于 MaxOpen 且 WaitCount 一直涨 → 有连接被借走没还；
//   · Open 上不去 → 连接建不起来（权限/磁盘/文件锁）；
//   · 都没有异常但接口慢 → 问题不在池上，别往这边查。
//
// 权限只要总管理员：它暴露的是进程内部计数，对普通用户没有意义。
func (s *Server) handleDebugDBPool(w http.ResponseWriter, r *http.Request) {
	if !requireAdminIn(w, r) {
		return
	}
	st := s.db.Stats()
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"db": map[string]interface{}{
			"open":          st.OpenConnections,
			"in_use":        st.InUse,
			"idle":          st.Idle,
			"max_open":      st.MaxOpenConnections,
			"wait_count":    st.WaitCount,
			"wait_seconds":  time.Duration(st.WaitDuration).Seconds(),
			"max_idle_clsd": st.MaxIdleClosed,
			"life_closed":   st.MaxLifetimeClosed,
		},
		"runtime": map[string]interface{}{
			"goroutines": runtime.NumGoroutine(),
			"heap_mb":    float64(mem.HeapAlloc) / 1024 / 1024,
			"sys_mb":     float64(mem.Sys) / 1024 / 1024,
			"gc_runs":    mem.NumGC,
		},
		// 顺手把两个"容易看错"的口径写进来，免得看的人凭直觉解读
		"note": "in_use 长期等于 max_open 且 wait_count 持续增长 = 有连接被借走没还；" +
			"open 上不去 = 连接建不起来。两者都没有却接口慢 → 不在池上，别往这边查。",
	})
}
