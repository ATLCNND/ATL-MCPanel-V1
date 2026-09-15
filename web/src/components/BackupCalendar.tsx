import { useMemo, useState } from 'react'
import { BackupItem } from '../api'
import './BackupCalendar.css'

// 判断备份是自动还是手动：面板创建的自动备份名称为 "auto"
const isAuto = (b: BackupItem) => (b.name || '').toLowerCase() === 'auto'

function dayKey(ts: number): string {
  const d = new Date(ts * 1000)
  return `${d.getFullYear()}-${d.getMonth() + 1}-${d.getDate()}`
}

function fmtSize(bytes: number): string {
  if (!bytes) return '—'
  if (bytes < 1024 * 1024) return (bytes / 1024).toFixed(0) + ' KB'
  if (bytes < 1024 * 1024 * 1024) return (bytes / 1024 / 1024).toFixed(1) + ' MB'
  return (bytes / 1024 / 1024 / 1024).toFixed(2) + ' GB'
}

const DOW = ['一', '二', '三', '四', '五', '六', '日']

/**
 * BackupCalendar 备份日历小组件。
 *
 * 用途：一眼看出"哪天有备份、是自动还是手动"，点某天展开当日明细。
 * 备份是低频事件，用月历比列表更直观地呈现"覆盖了哪些日期"——
 * 这正是回滚时最关心的信息。
 */
export default function BackupCalendar({ backups, policyDesc, manualKeep, onRestore }: {
  backups: BackupItem[]
  policyDesc?: string
  manualKeep?: number
  onRestore?: (b: BackupItem) => void
}) {
  const today = new Date()
  const [year, setYear] = useState(today.getFullYear())
  const [month, setMonth] = useState(today.getMonth()) // 0-based
  const [selected, setSelected] = useState<string>(dayKey(Math.floor(Date.now() / 1000)))

  // 按天分组
  const byDay = useMemo(() => {
    const m = new Map<string, BackupItem[]>()
    for (const b of backups) {
      const k = dayKey(b.created_at)
      const arr = m.get(k) || []
      arr.push(b)
      m.set(k, arr)
    }
    // 每天内按时间倒序
    for (const arr of m.values()) arr.sort((a, b) => b.created_at - a.created_at)
    return m
  }, [backups])

  // 月历格子（周一为第一列）
  const cells = useMemo(() => {
    const first = new Date(year, month, 1)
    // getDay(): 0=周日 → 转成 0=周一
    const lead = (first.getDay() + 6) % 7
    const daysInMonth = new Date(year, month + 1, 0).getDate()
    const daysInPrev = new Date(year, month, 0).getDate()

    const out: { day: number; cur: boolean; key: string; isToday: boolean }[] = []
    for (let i = lead - 1; i >= 0; i--) {
      const d = daysInPrev - i
      const prev = new Date(year, month - 1, d)
      out.push({ day: d, cur: false, key: `${prev.getFullYear()}-${prev.getMonth() + 1}-${d}`, isToday: false })
    }
    for (let d = 1; d <= daysInMonth; d++) {
      const isToday = d === today.getDate() && month === today.getMonth() && year === today.getFullYear()
      out.push({ day: d, cur: true, key: `${year}-${month + 1}-${d}`, isToday })
    }
    while (out.length % 7 !== 0) {
      const d = out.length - (lead + daysInMonth) + 1
      const next = new Date(year, month + 1, d)
      out.push({ day: d, cur: false, key: `${next.getFullYear()}-${next.getMonth() + 1}-${d}`, isToday: false })
    }
    return out
  }, [year, month])

  const shift = (delta: number) => {
    const d = new Date(year, month + delta, 1)
    setYear(d.getFullYear())
    setMonth(d.getMonth())
  }

  const selectedList = byDay.get(selected) || []
  const totalAuto = backups.filter(isAuto).length
  const totalManual = backups.length - totalAuto

  return (
    <div className="backup-calendar">
      <div className="bc-head">
        <span className="bc-title">备份日历</span>
        <div className="spacer" />
        <span className="bc-legend"><i className="dot-auto" />自动</span>
        <span className="bc-legend"><i className="dot-manual" />手动</span>
        <button className="bc-nav" onClick={() => shift(-1)} title="上个月">‹</button>
        <span className="bc-month">{year} 年 {month + 1} 月</span>
        <button className="bc-nav" onClick={() => shift(1)} title="下个月">›</button>
      </div>

      <div className="bc-grid">
        {DOW.map((d) => <div className="bc-dow" key={d}>{d}</div>)}
        {cells.map((c, i) => {
          const list = byDay.get(c.key) || []
          const hasAuto = list.some(isAuto)
          const hasManual = list.some((b) => !isAuto(b))
          const cls = [
            'bc-day',
            !c.cur && 'mute',
            c.isToday && 'today',
            c.key === selected && 'selected',
            hasAuto && 'has-auto',
            hasManual && 'has-manual',
          ].filter(Boolean).join(' ')
          return (
            <div className={cls} key={i} onClick={() => setSelected(c.key)} title={list.length ? `${list.length} 份备份` : ''}>
              {c.day}
              {(hasAuto || hasManual) && <span className="bc-dot" />}
            </div>
          )
        })}
      </div>

      {/* 当日明细 */}
      <div className="bc-detail">
        <div className="bc-detail-head">
          {selected} · {selectedList.length} 份
          {selectedList.length > 0 && <span className="muted"> 共 {fmtSize(selectedList.reduce((s, b) => s + b.size, 0))}</span>}
        </div>
        {selectedList.length === 0 && <div className="bc-empty">当天没有备份</div>}
        {selectedList.map((b) => (
          <div className={`bc-item ${isAuto(b) ? '' : 'manual'}`} key={b.backup_id}>
            <span className="bc-time">
              {new Date(b.created_at * 1000).toLocaleTimeString('zh-CN', { hour: '2-digit', minute: '2-digit' })}
            </span>
            <span className="bc-type">{isAuto(b) ? '自动' : b.name}</span>
            <span className="bc-size">{fmtSize(b.size)}</span>
            {onRestore && (
              <button className="bc-restore" onClick={() => onRestore(b)} title="回滚到此备份">回滚</button>
            )}
          </div>
        ))}
      </div>

      <div className="bc-summary">
        <div>共 <b>{backups.length}</b> 份：自动 <b>{totalAuto}</b> · 手动 <b>{totalManual}</b>
          {manualKeep !== undefined && manualKeep > 0 && <> （手动保留上限 {manualKeep}）</>}
        </div>
        {policyDesc && <div className="bc-policy">策略：{policyDesc}</div>}
      </div>
    </div>
  )
}
