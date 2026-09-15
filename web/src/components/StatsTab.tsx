import { useEffect, useState } from 'react'
import { InstanceRuntime, getInstanceRuntime, getInstanceStats, StatsResponse } from '../api'
import './StatsTab.css'

function fmtBytes(n: number): string {
  if (!n) return '—'
  if (n < 1024 * 1024) return (n / 1024).toFixed(0) + ' KB'
  if (n < 1024 * 1024 * 1024) return (n / 1024 / 1024).toFixed(0) + ' MB'
  return (n / 1024 / 1024 / 1024).toFixed(2) + ' GB'
}
function fmtUptime(sec?: number, status?: string): string {
  if (status && status !== 'running') return '未运行'
  if (!sec || sec <= 0) return '—'
  const d = Math.floor(sec / 86400)
  const h = Math.floor((sec % 86400) / 3600)
  const m = Math.floor((sec % 3600) / 60)
  if (d > 0) return `${d} 天 ${h} 小时`
  if (h > 0) return `${h} 小时 ${m} 分`
  return `${m} 分`
}

/**
 * 历史趋势图（带 Y 轴刻度与网格）。
 * 与实例页的实时图不同，这里的数据来自后端历史采样，跨度可达 7 天。
 */
function TrendChart({ title, points, color, fmt, unit }: {
  title: string
  points: { v: number; at: string }[]
  color: string
  fmt: (n: number) => string
  unit?: string
}) {
  const max = Math.max(...points.map((p) => p.v), 1)
  const W = 600
  const H = 90
  const step = points.length > 1 ? W / (points.length - 1) : W
  const coords = points.map((p, i) => ({ x: i * step, y: H - (p.v / max) * H }))
  const line = coords.map((c) => `${c.x.toFixed(1)},${c.y.toFixed(1)}`).join(' ')
  const area = `0,${H} ${line} ${W},${H}`

  return (
    <div className="st-chart">
      <div className="st-head">
        <span className="st-title">{title}</span>
        <span className="st-peak">峰值 {fmt(max)}{unit || ''}</span>
      </div>
      <div className="st-plot">
        <div className="st-axis">
          {[0, 25, 50, 75, 100].map((p) => (
            <span key={p} className="st-label" style={{ top: `${p}%` }}>
              {fmt((max * (100 - p)) / 100)}
            </span>
          ))}
        </div>
        <svg viewBox={`0 0 ${W} ${H}`} preserveAspectRatio="none" className="st-svg">
          <g className="st-grid">
            {[0, 25, 50, 75, 100].map((p) => (
              <line key={p} x1="0" y1={(p / 100) * H} x2={W} y2={(p / 100) * H} />
            ))}
            {[25, 50, 75].map((p) => (
              <line key={p} x1={(p / 100) * W} y1="0" x2={(p / 100) * W} y2={H} />
            ))}
          </g>
          {points.length > 1 && (
            <>
              <polygon points={area} fill={color} opacity="0.12" />
              <polyline points={line} fill="none" stroke={color} strokeWidth="1.6" />
            </>
          )}
        </svg>
      </div>
      {points.length > 1 && (
        <div className="st-xaxis">
          <span>{new Date(points[0].at).toLocaleString('zh-CN', { hour12: false, month: 'numeric', day: 'numeric', hour: '2-digit', minute: '2-digit' })}</span>
          <span>{new Date(points[points.length - 1].at).toLocaleString('zh-CN', { hour12: false, month: 'numeric', day: 'numeric', hour: '2-digit', minute: '2-digit' })}</span>
        </div>
      )}
    </div>
  )
}

export default function StatsTab({ instanceId, maxMem, diskLimitMB, diskAutostop }: {
  instanceId: string
  maxMem: string
  diskLimitMB?: number
  diskAutostop?: boolean
}) {
  const [hours, setHours] = useState(24)
  const [stats, setStats] = useState<StatsResponse | null>(null)
  const [runtime, setRuntime] = useState<InstanceRuntime | null>(null)
  const [error, setError] = useState('')
  const [loading, setLoading] = useState(false)

  const load = async () => {
    setLoading(true)
    try {
      const [s, rt] = await Promise.all([
        getInstanceStats(instanceId, hours),
        getInstanceRuntime(instanceId).catch(() => null),
      ])
      setStats(s)
      setRuntime(rt)
      setError('')
    } catch (e: any) {
      setError(e.message)
    } finally {
      setLoading(false)
    }
  }

  useEffect(() => { load() }, [instanceId, hours])

  const pts = stats?.points || []
  const pick = (k: 'cpu' | 'mem' | 'players' | 'tps') => pts.map((p) => ({ v: p[k] as number, at: p.at }))

  // 运行率：本次运行时长 / 统计窗口
  const windowSec = hours * 3600
  const runRate = runtime && runtime.uptime_seconds > 0
    ? Math.min(100, Math.round((runtime.uptime_seconds / windowSec) * 100))
    : 0

  return (
    <div className="stats-tab">
      <div className="st-toolbar">
        <div className="st-ranges">
          {[1, 6, 24, 72, 168].map((h) => (
            <button key={h} className={hours === h ? 'active' : ''} onClick={() => setHours(h)}>
              {h < 24 ? `${h} 小时` : `${h / 24} 天`}
            </button>
          ))}
        </div>
        <span className="muted">
          {loading ? '加载中…' : `共 ${stats?.total || 0} 个采样点`}
          {stats && stats.step > 1 ? `（每 ${stats.step} 分钟取一点）` : ''}
        </span>
        <button onClick={load} disabled={loading}>刷新</button>
      </div>

      {error && <div className="error-banner">{error}</div>}

      {/* 汇总 */}
      <div className="st-summary">
        <div className="ss">
          <div className="k">平均 CPU</div>
          <div className="v">{stats?.avg?.cpu ? stats.avg.cpu.toFixed(1) + '%' : '—'}</div>
          <div className="sub">峰值 {stats?.peak?.cpu ? stats.peak.cpu.toFixed(1) + '%' : '—'}</div>
        </div>
        <div className="ss">
          <div className="k">平均内存</div>
          <div className="v">{stats?.avg?.mem ? fmtBytes(stats.avg.mem) : '—'}</div>
          <div className="sub">峰值 {stats?.peak?.mem ? fmtBytes(stats.peak.mem) : '—'} / {maxMem || '—'}</div>
        </div>
        <div className="ss">
          <div className="k">平均在线</div>
          <div className="v">{stats?.avg?.players ? stats.avg.players.toFixed(1) : '—'}</div>
          <div className="sub">峰值 {stats?.peak?.players ?? '—'} 人</div>
        </div>
        <div className="ss">
          <div className="k">平均 TPS</div>
          <div className="v">{stats?.avg?.tps ? stats.avg.tps.toFixed(2) : '—'}</div>
          <div className="sub">满值 20.00</div>
        </div>
      </div>

      {/* 运行统计 */}
      <div className="st-runtime">
        <div className="sr">
          <span className="k">本次运行时长</span>
          <b>{fmtUptime(runtime?.uptime_seconds, runtime?.status)}</b>
        </div>
        <div className="sr">
          <span className="k">累计启停</span>
          <b>开机 {runtime?.start_count ?? '—'} 次 · 关机 {runtime?.stop_count ?? '—'} 次</b>
        </div>
        <div className="sr">
          <span className="k">窗口内运行占比</span>
          <b>{runRate}%</b>
        </div>
        <div className="sr">
          <span className="k">实例目录占用</span>
          <b>{runtime ? fmtBytes(runtime.disk_used) : '—'}
            {runtime?.disk_total ? ` / ${fmtBytes(runtime.disk_total)}` : ''}</b>
        </div>
        <div className="sr">
          <span className="k">磁盘配额</span>
          <b>
            {diskLimitMB && diskLimitMB > 0
              ? `${diskLimitMB} MB（已用 ${runtime ? Math.round((runtime.disk_used / 1024 / 1024 / diskLimitMB) * 100) : 0}%）${diskAutostop ? ' · 超限自动停机' : ''}`
              : '未设置'}
          </b>
        </div>
      </div>

      {/* 磁盘配额水位 */}
      {diskLimitMB && diskLimitMB > 0 && runtime && (
        <div className="st-quota">
          <div className="sq-head">
            <span>磁盘配额水位</span>
            <b>{fmtBytes(runtime.disk_used)} / {diskLimitMB} MB</b>
          </div>
          <div className="bar">
            <i
              className={
                runtime.disk_used / 1024 / 1024 / diskLimitMB >= 0.95 ? 'bad'
                  : runtime.disk_used / 1024 / 1024 / diskLimitMB >= 0.85 ? 'warn' : 'ok'
              }
              style={{ width: `${Math.min(100, Math.round((runtime.disk_used / 1024 / 1024 / diskLimitMB) * 100))}%` }}
            />
          </div>
          <div className="sq-note">
            85% 触发警告，95% 触发严重告警{diskAutostop ? '；已开启超限自动停机' : ''}
          </div>
        </div>
      )}

      {/* 趋势 */}
      {pts.length === 0 ? (
        <div className="st-empty">
          暂无历史数据 —— 面板每分钟采样一次，稍后再来看即可。
          <br />
          <span className="muted">（历史数据保留 7 天）</span>
        </div>
      ) : (
        <div className="st-charts">
          <TrendChart title="CPU 使用率" points={pick('cpu')} color="var(--accent-solid)" fmt={(n) => n.toFixed(0)} unit="%" />
          <TrendChart title="内存占用" points={pick('mem')} color="var(--accent)" fmt={(n) => fmtBytes(n)} />
          <TrendChart title="在线玩家" points={pick('players')} color="var(--success)" fmt={(n) => n.toFixed(0)} unit=" 人" />
          <TrendChart title="TPS" points={pick('tps')} color="var(--warning)" fmt={(n) => n.toFixed(1)} />
        </div>
      )}
    </div>
  )
}
