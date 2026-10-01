package httpapi

import (
	"database/sql"
	"time"
)

// 节点"是否在线"的**唯一判定口径**。
//
// 2026-10-01 的断网测试暴露过一次不一致：把 Daemon → 面板的链路掐断后，
// 「节点监控」已经按心跳变旧显示了离线、告警也起来了，
// 但 `/api/nodes` 仍然返回 `status: "online"`（那是数据库里的原始列），
// 于是**同一个页面里两处说法自相矛盾**——节点选择器说"在线"，监控页说"离线"。
//
// 根因是判定逻辑散在三处（monitor.go / scheduler.go / 各自的 SQL），
// 各写各的阈值。现在收成一个函数 + 一个常量，三处都从这里取：
//   · 心跳间隔 10 秒，**90 秒**没有上报即视为离线（留了 9 个心跳的余量，
//     避免单次网络抖动就把节点判死）。
//
// status 列本身仍然保留（它记录"最后一次显式上报的状态"），
// 但**界面上要用的"在线/离线"一律用这个函数算**。
const nodeStaleAfter = 90 * time.Second

// nodeOnline 按"状态 + 心跳新鲜度"判定节点是否在线。
func nodeOnline(status string, lastSeen *time.Time) bool {
	return status == "online" && lastSeen != nil && time.Since(*lastSeen) < nodeStaleAfter
}

// nodeOnlineFromDB 读库判定的便捷包装（内部查 state 与 last_seen 两列）。
func nodeOnlineFromDB(db *sql.DB, nodeID int64) bool {
	var status string
	var lastSeen *time.Time
	if err := db.QueryRow(`SELECT status, last_seen FROM nodes WHERE id = ?`, nodeID).
		Scan(&status, &lastSeen); err != nil {
		return false
	}
	return nodeOnline(status, lastSeen)
}
