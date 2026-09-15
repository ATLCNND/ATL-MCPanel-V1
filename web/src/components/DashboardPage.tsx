import { useEffect, useState } from 'react'
import {
  Instance, User, AlertItem, Announcement,
  listInstances, monitorNodes, monitorInstances, listAlerts, listAnnouncements,
  MonitorSummary, MonitorInstance, levelAtLeast,
} from '../api'
import MiniMarkdown from './MiniMarkdown'
import { NavKey } from './AppShell'
import './DashboardPage.css'

function fmtGB(bytes: number): string {
  if (!bytes) return '—'
  return (bytes / 1024 / 1024 / 1024).toFixed(1) + ' GB'
}
function pct(part: number, total: number): number {
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
  if (sec < 86400) return `${Math.floor(sec / 3600)} 小时前`
  return `${Math.floor(sec / 86400)} 天前`
}
function barClass(p: number): string {
  if (p >= 90) return 'bar danger'
  if (p >= 75) return 'bar warn'
  return 'bar'
}

const SEVERITY_TEXT: Record<string, string> = {
  critical: '严重',
  warning: '警告',
  info: '提示',
}

/**
 * DashboardPage 总览。
 *
 * 定位：回答"现在整体是什么状况"—— 有多少实例在跑、节点健康度、
 * 有没有需要立刻处理的告警。因此刻意不放操作按钮，
 * 需要动手时点进对应页面。
 */
export default function DashboardPage({
  user,
  onOpenInstance,
  onNavigate,
}: {
  user: User | null
  onOpenInstance: (id: string, name: string, status: string, level: string) => void
  onNavigate: (k: NavKey) => void
}) {
  const [instances, setInstances] = useState<Instance[]>([])
  const [summary, setSummary] = useState<MonitorSummary | null>(null)
  const [monitorInst, setMonitorInst] = useState<MonitorInstance[]>([])
  const [alerts, setAlerts] = useState<AlertItem[]>([])
  const [anns, setAnns] = useState<Announcement[]>([])
  const [error, setError] = useState('')
  const [loading, setLoading] = useState(false)

  const isAdmin = user?.role === 'admin'

  const load = async () => {
    setLoading(true)
    try {
      const [ins, mon, mIns] = await Promise.all([
        listInstances(),
        monitorNodes().catch(() => null),
        monitorInstances().catch(() => []),
      ])
      setInstances(Array.isArray(ins) ? ins : [])
      setSummary(mon)
      setMonitorInst(Array.isArray(mIns) ? mIns : [])
      // 公告：只取前 3 条（总览是"看一眼就走"的页面，
      // 完整列表在「公告与帮助」）。失败不影响其它内容。
      const an = await listAnnouncements().catch(() => [])
      setAnns(Array.isArray(an) ? an.slice(0, 3) : [])
      if (isAdmin) {
        const a = await listAlerts(true, 20).catch(() => null)
        setAlerts(a && Array.isArray(a.alerts) ? a.alerts : [])
      }
      setError('')
    } catch (e: any) {
      setError(e.message)
    } finally {
      setLoading(false)
    }
  }

  useEffect(() => {
    load()
    const t = window.setInterval(load, 20000)
    return () => window.clearInterval(t)
  }, [isAdmin])

  const running = instances.filter(
    (i) => (i.live_status && i.live_status !== 'unknown' ? i.live_status : i.status) === 'running'
  ).length
  const nodes = summary?.nodes || []
  const onlineNodes = summary?.online_nodes ?? 0
  const totalNodes = summary?.total_nodes ?? 0

  // 节点资源最紧张的一项，用于「最需要关注」提示
  const worst = nodes.length
    ? nodes.reduce((a, b) => {
        const pa = Math.max(a.cpu_percent, pct(a.disk_used, a.disk_total))
        const pb = Math.max(b.cpu_percent, pct(b.disk_used, b.disk_total))
        return pb > pa ? b : a
      })
    : null

  return (
    <div className="dash">
      <div className="page-toolbar">
        <span className="muted">每 20 秒自动刷新</span>
        <button onClick={load} disabled={loading}>{loading ? '刷新中…' : '刷新'}</button>
      </div>

      {error && <div className="error-banner">{error}</div>}

      {/* ---------- 公告 ----------
          放在最上面：总览是用户登录后落地的第一屏，公告要在这里才看得到。
          只展示，不可编辑（编辑在「公告与帮助」页），点标题跳过去看全文。 */}
      {anns.length > 0 && (
        <div className="dash-anns">
          {anns.map((a) => (
            <div className={`da ${a.pinned ? 'pinned' : ''}`} key={a.id}>
              <span className="da-tag">{a.pinned ? '📌 公告' : '公告'}</span>
              <div className="da-main">
                <div className="da-title">{a.title}</div>
                {a.body && (
                  <div className="da-body">
                    <MiniMarkdown text={a.body} />
                  </div>
                )}
              </div>
              <button className="da-more" onClick={() => onNavigate('help')}>全部</button>
            </div>
          ))}
        </div>
      )}

      {/* ---------- 概览统计 ---------- */}
      <div className="dash-stats">
        <div className="ds" onClick={() => onNavigate('instances')}>
          <div className="ds-k">实例</div>
          <div className="ds-v">
            {running}<span className="ds-u">/ {instances.length} 运行中</span>
          </div>
          {instances.length > 0 && (
            <div className="ds-bar"><i style={{ width: `${pct(running, instances.length)}%` }} /></div>
          )}
        </div>

        <div className="ds" onClick={() => onNavigate('monitor')}>
          <div className="ds-k">节点</div>
          <div className="ds-v">
            {onlineNodes}<span className="ds-u">/ {totalNodes} 在线</span>
          </div>
          <div className={`ds-state ${totalNodes > 0 && onlineNodes === totalNodes ? 'ok' : 'warn'}`}>
            {totalNodes === 0
              ? '尚未登记节点'
              : onlineNodes === totalNodes
                ? '全部正常'
                : `${totalNodes - onlineNodes} 台离线`}
          </div>
        </div>

        <div className="ds">
          <div className="ds-k">在线玩家</div>
          <div className="ds-v">
            {monitorInst.reduce((s, x) => s + (x.live_status === 'running' ? 1 : 0), 0)}
            <span className="ds-u">个实例在运行</span>
          </div>
          <div className="ds-state muted">点进实例查看具体人数</div>
        </div>

        {isAdmin && (
          <div className="ds" onClick={() => onNavigate('audit')}>
            <div className="ds-k">待处理告警</div>
            <div className="ds-v">
              {alerts.length}
              <span className="ds-u">条</span>
            </div>
            <div className={`ds-state ${alerts.length > 0 ? 'err' : 'ok'}`}>
              {alerts.length > 0 ? '需要关注' : '一切正常'}
            </div>
          </div>
        )}
      </div>

      {/* ---------- 需要关注 ---------- */}
      {isAdmin && alerts.length > 0 && (
        <div className="dash-section">
          <div className="dash-title">
            <span>需要关注</span>
            <span className="dt-more" onClick={() => onNavigate('audit')}>查看全部 →</span>
          </div>
          <div className="alert-list">
            {alerts.slice(0, 5).map((a) => (
              <div className={`al-item ${a.severity}`} key={a.id}>
                <span className={`al-sev ${a.severity}`}>{SEVERITY_TEXT[a.severity] || a.severity}</span>
                <span className="al-msg">{a.message}</span>
                <span className="al-time">{fmtAgo(a.created_at)}</span>
              </div>
            ))}
          </div>
        </div>
      )}

      {/* ---------- 实例 ---------- */}
      <div className="dash-section">
        <div className="dash-title">
          <span>我的实例</span>
          <span className="dt-more" onClick={() => onNavigate('instances')}>全部实例 →</span>
        </div>
        {instances.length === 0 ? (
          <div className="dash-empty">暂无实例</div>
        ) : (
          <div className="dash-inst-grid">
            {instances.slice(0, 6).map((inst) => {
              const live =
                inst.live_status && inst.live_status !== 'unknown' ? inst.live_status : inst.status
              return (
                <div
                  className={`dash-inst ${live}`}
                  key={inst.instance_id}
                  onClick={() => onOpenInstance(inst.instance_id, inst.name, live, inst.level || 'viewer')}
                >
                  <div className="di-head">
                    <span className="di-avatar">{(inst.name || inst.instance_id).slice(0, 1)}</span>
                    <div className="di-title">
                      <div className="di-name" title={inst.name}>{inst.name}</div>
                      <div className="di-id mono">{inst.instance_id}</div>
                    </div>
                    <span className={`status status-${live}`}>{live}</span>
                  </div>
                  <div className="di-meta">
                    <span className="di-tag">{inst.core_type}</span>
                    <span>{inst.max_mem}</span>
                    {inst.level && <span className="di-level">{inst.level}</span>}
                  </div>
                </div>
              )
            })}
          </div>
        )}
      </div>

      {/* ---------- 节点 ---------- */}
      <div className="dash-section">
        <div className="dash-title">
          <span>节点资源</span>
          <span className="dt-more" onClick={() => onNavigate('monitor')}>节点监控 →</span>
        </div>
        {nodes.length === 0 ? (
          <div className="dash-empty">暂无节点</div>
        ) : (
          <div className="dash-nodes">
            {nodes.map((n) => {
              const memP = pct(n.mem_used, n.mem_total)
              const diskP = pct(n.disk_used, n.disk_total)
              return (
                <div className={`dash-node ${n.online ? '' : 'offline'}`} key={n.id}>
                  <div className="dn-head">
                    <span className="dn-name">{n.name}</span>
                    <span className={`status ${n.online ? 'status-running' : 'status-stopped'}`}>
                      {n.online ? '在线' : '离线'}
                    </span>
                    <span className="dn-time">{fmtAgo(n.last_seen)}</span>
                  </div>
                  <div className="dn-metrics">
                    <div className="dn-m">
                      <div className="dn-mk">CPU <b>{n.cpu_percent ? n.cpu_percent.toFixed(0) + '%' : '—'}</b></div>
                      <div className={barClass(n.cpu_percent)}><i style={{ width: `${Math.min(100, n.cpu_percent)}%` }} /></div>
                      <div className="dn-sub">{n.cpu} 核</div>
                    </div>
                    <div className="dn-m">
                      <div className="dn-mk">内存 <b>{memP}%</b></div>
                      <div className={barClass(memP)}><i style={{ width: `${memP}%` }} /></div>
                      <div className="dn-sub">{fmtGB(n.mem_used)} / {fmtGB(n.mem_total)}</div>
                    </div>
                    <div className="dn-m">
                      <div className="dn-mk">磁盘 <b>{diskP}%</b></div>
                      <div className={barClass(diskP)}><i style={{ width: `${diskP}%` }} /></div>
                      <div className="dn-sub">{fmtGB(n.disk_used)} / {fmtGB(n.disk_total)}</div>
                    </div>
                  </div>
                  <div className="dn-foot">
                    实例 {n.running} 运行 / {n.instances} 总数
                    {isAdmin && (
                      <button className="dn-link" onClick={() => onNavigate('nodes')}>管理 →</button>
                    )}
                  </div>
                </div>
              )
            })}
          </div>
        )}
      </div>

      {!isAdmin && worst && (
        <div className="dash-note">
          提示：资源占用较高的节点是 <b>{worst.name}</b>，若实例卡顿可联系管理员。
        </div>
      )}
    </div>
  )
}
