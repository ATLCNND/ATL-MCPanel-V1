import { useEffect, useState } from 'react'
import { BackupItem, listBackups, createBackup, deleteBackup, restoreBackup } from '../api'
import SchedulePanel from './SchedulePanel'
import BackupCalendar from './BackupCalendar'
import './BackupsTab.css'

function formatSize(bytes: number): string {
  if (bytes < 1024) return bytes + ' B'
  if (bytes < 1024 * 1024) return (bytes / 1024).toFixed(1) + ' KB'
  if (bytes < 1024 * 1024 * 1024) return (bytes / 1024 / 1024).toFixed(1) + ' MB'
  return (bytes / 1024 / 1024 / 1024).toFixed(2) + ' GB'
}

function formatTime(ts: number): string {
  if (!ts) return '-'
  return new Date(ts * 1000).toLocaleString('zh-CN')
}

export default function BackupsTab({ instanceId, canWrite, running, isAdmin }: {
  instanceId: string
  canWrite: boolean
  running: boolean
  /** 仅管理员可管理备份保留策略 */
  isAdmin?: boolean
}) {
  const [backups, setBackups] = useState<BackupItem[]>([])
  const [name, setName] = useState('')
  const [includeConfig, setIncludeConfig] = useState(true)
  const [error, setError] = useState('')
  const [msg, setMsg] = useState('')
  const [busy, setBusy] = useState(false)

  const load = async () => {
    try {
      const list = await listBackups(instanceId)
      setBackups(Array.isArray(list) ? list : [])
    } catch (e: any) {
      setError(e.message)
    }
  }

  useEffect(() => {
    load()
  }, [instanceId])

  const create = async () => {
    setBusy(true); setError(''); setMsg('')
    try {
      const r = await createBackup(instanceId, name, includeConfig)
      setMsg(r.message || '备份完成')
      setName('')
      await load()
    } catch (e: any) {
      setError(e.message)
    } finally {
      setBusy(false)
    }
  }

  const remove = async (b: BackupItem) => {
    if (!confirm(`确定删除备份「${b.name}」？`)) return
    try {
      await deleteBackup(instanceId, b.backup_id)
      await load()
    } catch (e: any) {
      setError(e.message)
    }
  }

  const restore = async (b: BackupItem) => {
    if (running) {
      setError('实例正在运行，请先停止实例再回滚')
      return
    }
    if (!confirm(`确定回滚到「${b.name}」？\n\n当前世界存档将被备份内容覆盖（不可撤销）。\n建议先创建一份当前状态的备份。`)) return
    setBusy(true); setError(''); setMsg('')
    try {
      const r = await restoreBackup(instanceId, b.backup_id)
      setMsg(r.message || '回滚完成')
    } catch (e: any) {
      setError(e.message)
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="backups-tab">
      <SchedulePanel instanceId={instanceId} canWrite={canWrite} isAdmin={isAdmin} />

      <div className="backups-toolbar">
        <input
          placeholder="备份名称（可选，如：开荒前）"
          value={name}
          onChange={(e) => setName(e.target.value)}
          disabled={!canWrite}
        />
        <label className="switch">
          <input type="checkbox" checked={includeConfig} onChange={(e) => setIncludeConfig(e.target.checked)} />
          包含配置文件
        </label>
        <button className="primary" onClick={create} disabled={!canWrite || busy}>
          {busy ? '处理中...' : '立即备份'}
        </button>
        <div className="spacer" />
        <button onClick={load}>刷新</button>
      </div>

      {error && <div className="backups-error">{error}</div>}
      {msg && <div className="backups-success">{msg}</div>}
      {running && <div className="backups-hint">实例运行中：备份前会自动触发存档落盘（save-all flush）以保证一致性；回滚需先停止实例。</div>}
      {!canWrite && <div className="backups-hint">只读/协作权限：创建与回滚需要 owner 及以上权限。</div>}

      <div className="backups-list">
        <table>
          <thead>
            <tr>
              <th>名称</th>
              <th>大小</th>
              <th>创建时间</th>
              <th>操作</th>
            </tr>
          </thead>
          <tbody>
            {backups.map((b) => (
              <tr key={b.backup_id}>
                <td>{b.name}</td>
                <td>{formatSize(b.size)}</td>
                <td>{formatTime(b.created_at)}</td>
                <td>
                  <div className="backup-actions">
                    <button onClick={() => restore(b)} disabled={!canWrite || busy}>回滚</button>
                    <button className="danger" onClick={() => remove(b)} disabled={!canWrite || busy}>删除</button>
                  </div>
                </td>
              </tr>
            ))}
            {backups.length === 0 && (
              <tr><td colSpan={4} className="empty">暂无备份，点击"立即备份"创建</td></tr>
            )}
          </tbody>
        </table>
      </div>

      {/* 备份日历：一眼看出哪天有备份、自动还是手动 */}
      <div className="backup-calendar-wrap">
        <BackupCalendar
          backups={backups}
          onRestore={canWrite ? (b) => restore(b) : undefined}
        />
      </div>
    </div>
  )
}
