import { useEffect, useState } from 'react'
import { AlertItem, listAlerts, resolveAlert } from '../api'
import './AlertsModal.css'

const KIND_LABEL: Record<string, string> = {
  instance_crash: '实例异常停止',
  node_offline: '节点离线',
  disk_low: '磁盘空间不足',
  backup_failed: '备份失败',
}

function fmtTime(s: string): string {
  if (!s) return '—'
  const d = new Date(s.replace(' ', 'T') + (s.includes('Z') || s.includes('+') ? '' : 'Z'))
  if (isNaN(d.getTime())) return s
  return d.toLocaleString('zh-CN')
}

export default function AlertsModal({ onClose }: { onClose: () => void }) {
  const [alerts, setAlerts] = useState<AlertItem[]>([])
  const [showResolved, setShowResolved] = useState(false)
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)

  const load = async () => {
    try {
      const r = await listAlerts(!showResolved, 200)
      setAlerts(Array.isArray(r.alerts) ? r.alerts : [])
      setError('')
    } catch (e: any) {
      setError(e.message)
    }
  }

  useEffect(() => { load() }, [showResolved])

  return (
    <div className="modal-mask" onClick={onClose}>
      <div className="alerts-modal" onClick={(e) => e.stopPropagation()}>
        <div className="alerts-head">
          <strong>告警中心</strong>
          <label className="switch">
            <input type="checkbox" checked={showResolved} onChange={(e) => setShowResolved(e.target.checked)} />
            显示已恢复
          </label>
          <div className="spacer" />
          <button onClick={load} disabled={busy}>刷新</button>
          <button onClick={onClose}>关闭</button>
        </div>

        {error && <div className="error-banner">{error}</div>}

        <div className="alerts-body">
          {alerts.map((a) => (
            <div className={`alert-item sev-${a.severity} ${a.active ? '' : 'resolved'}`} key={a.id}>
              <div className="alert-line">
                <span className={`sev-badge sev-${a.severity}`}>{a.severity}</span>
                <strong>{KIND_LABEL[a.kind] || a.kind}</strong>
                <span className="muted">目标：{a.target}</span>
                <div className="spacer" />
                <span className="muted">{fmtTime(a.created_at)}</span>
              </div>
              <div className="alert-msg">{a.message}</div>
              {a.detail && <div className="alert-detail">{a.detail}</div>}
              <div className="alert-actions">
                {a.active ? (
                  <button onClick={async () => {
                    setBusy(true)
                    try { await resolveAlert(a.id); await load() } catch (e: any) { setError(e.message) } finally { setBusy(false) }
                  }} disabled={busy}>标记为已处理</button>
                ) : (
                  <span className="muted">已恢复于 {fmtTime(a.resolved_at)}</span>
                )}
              </div>
            </div>
          ))}
          {alerts.length === 0 && (
            <div className="empty">
              {showResolved ? '暂无告警记录' : '当前没有待处理告警（系统运行正常）'}
            </div>
          )}
        </div>

        <div className="alerts-note">
          告警由面板后台自动检测：实例意外停止（崩溃/OOM）、节点离线、磁盘空间不足、定时备份失败。
          同一问题只保留一条记录，问题恢复后自动关闭。
        </div>
      </div>
    </div>
  )
}
