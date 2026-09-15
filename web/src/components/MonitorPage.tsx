import { useEffect, useState } from 'react'
import { MonitorSummary, MonitorInstance, monitorNodes, monitorInstances } from '../api'
import './MonitorPage.css'

function fmtGB(bytes: number): string {
  if (!bytes) return '—'
  return (bytes / 1024 / 1024 / 1024).toFixed(1) + ' GB'
}

function fmtPct(part: number, total: number): number {
  if (!total) return 0
  return Math.min(100, Math.round((part / total) * 100))
}

function fmtAgo(s: string): string {
  if (!s) return '—'
  const d = new Date(s)
  if (isNaN(d.getTime())) return s
  const sec = (Date.now() - d.getTime()) / 1000
  if (sec < 60) return `${Math.max(0, Math.floor(sec))} 秒前`
  if (sec < 3600) return `${Math.floor(sec / 60)} 分钟前`
  return d.toLocaleString('zh-CN')
}

function barClass(pct: number): string {
  if (pct >= 90) return 'bar danger'
  if (pct >= 75) return 'bar warn'
  return 'bar'
}

export default function MonitorPage() {
  const [summary, setSummary] = useState<MonitorSummary | null>(null)
  const [instances, setInstances] = useState<MonitorInstance[]>([])
  const [error, setError] = useState('')
  const [loading, setLoading] = useState(false)

  const load = async () => {
    setLoading(true)
    try {
      const [s, ins] = await Promise.all([monitorNodes(), monitorInstances()])
      setSummary(s)
      setInstances(Array.isArray(ins) ? ins : [])
      setError('')
    } catch (e: any) {
      setError(e.message)
    } finally {
      setLoading(false)
    }
  }

  useEffect(() => {
    load()
    // 监控页每 15 秒自动刷新
    const timer = window.setInterval(load, 15000)
    return () => window.clearInterval(timer)
  }, [])

  const nodes = summary?.nodes || []

  return (
    <div className="monitor-page">
      <div className="page-toolbar">
        <span className="muted">每 15 秒自动刷新</span>
        <button onClick={load} disabled={loading}>{loading ? '刷新中…' : '刷新'}</button>
      </div>

      <div className="monitor-body">
        {error && <div className="error-banner">{error}</div>}

        {summary && (
          <div className="monitor-stats">
            <div className="stat">
              <div className="k">节点</div>
              <div className="v">{summary.online_nodes}<span className="u">/ {summary.total_nodes} 在线</span></div>
            </div>
            <div className="stat">
              <div className="k">实例</div>
              <div className="v">{summary.running_instances}<span className="u">/ {summary.total_instances} 运行中</span></div>
            </div>
            <div className="stat">
              <div className="k">整体状态</div>
              <div className="v">
                {summary.online_nodes === summary.total_nodes && summary.total_nodes > 0
                  ? <span className="ok-text">全部正常</span>
                  : <span className="err-text-inline">存在离线节点</span>}
              </div>
            </div>
          </div>
        )}

        <div className="node-cards">
          {nodes.map((n) => {
            const memPct = fmtPct(n.mem_used, n.mem_total)
            const diskPct = fmtPct(n.disk_used, n.disk_total)
            const myInstances = instances.filter((i) => i.node_id === n.id)
            return (
              <div className={`node-card ${n.online ? '' : 'offline'}`} key={n.id}>
                <div className="nc-head">
                  <span className="nc-name">{n.name}</span>
                  <span className={`badge ${n.online ? 'badge-run' : 'badge-off'}`}>
                    {n.online ? '在线' : '离线'}
                  </span>
                  <div className="spacer" />
                  <span className="muted">心跳 {fmtAgo(n.last_seen)}</span>
                </div>

                <div className="nc-metrics">
                  <div className="metric">
                    <div className="mrow">
                      <span className="mk">CPU</span>
                      <span className="mv">{n.cpu_percent ? n.cpu_percent.toFixed(1) + '%' : '—'}</span>
                    </div>
                    <div className={barClass(n.cpu_percent)}><i style={{ width: `${Math.min(100, n.cpu_percent)}%` }} /></div>
                    <div className="msub">{n.cpu} 核</div>
                  </div>

                  <div className="metric">
                    <div className="mrow">
                      <span className="mk">内存</span>
                      <span className="mv">{fmtGB(n.mem_used)} / {fmtGB(n.mem_total)}</span>
                    </div>
                    <div className={barClass(memPct)}><i style={{ width: `${memPct}%` }} /></div>
                    <div className="msub">{memPct}% 已用</div>
                  </div>

                  <div className="metric">
                    <div className="mrow">
                      <span className="mk">磁盘</span>
                      <span className="mv">{fmtGB(n.disk_used)} / {fmtGB(n.disk_total)}</span>
                    </div>
                    <div className={barClass(diskPct)}><i style={{ width: `${diskPct}%` }} /></div>
                    <div className="msub">{diskPct}% 已用</div>
                  </div>
                </div>

                <div className="nc-instances">
                  <div className="nci-title">
                    实例 {n.running} 运行 / {n.instances} 总数
                  </div>
                  {myInstances.length === 0 ? (
                    <div className="muted" style={{ fontSize: 12 }}>（无你有权限查看的实例）</div>
                  ) : (
                    <div className="nci-list">
                      {myInstances.map((i) => (
                        <span className="nci-item" key={i.instance_id}>
                          <span className={`dot-sm ${i.live_status === 'running' ? 'on' : 'off'}`} />
                          {i.name}
                          <span className="muted">{i.core_type} · {i.max_mem}</span>
                        </span>
                      ))}
                    </div>
                  )}
                </div>
              </div>
            )
          })}

          {nodes.length === 0 && !loading && (
            <div className="empty">暂无节点。请联系管理员在「节点管理」中登记并部署 Daemon。</div>
          )}
        </div>

        <div className="monitor-note">
          说明：本页所有登录用户均可查看，用于了解实例所在节点的健康状况。
          CPU / 内存 / 磁盘数据由各节点的 Daemon 每 10 秒上报一次。
          敏感信息（SSH 凭据等）仅在管理员的「节点管理」中可见。
        </div>
      </div>
    </div>
  )
}
