import { Fragment, useEffect, useState } from 'react'
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

/** 一条自动备份链：1 份基线 + 若干增量。 */
interface Chain {
  key: string
  /** 链上各环，按时间从旧到新（成员 0 是基线）。 */
  members: BackupItem[]
  /** 这条链最后一次捕获数据的时间。 */
  latest: number
  /** 整条链占的空间（各包之和）。 */
  totalSize: number
}

/**
 * 把备份列表分成「自动链」与「独立的备份」两类。
 *
 * 为什么要分组：自动备份是增量的 —— 一条链里只有第一份是完整快照，后面每份只含
 * 变化的文件。平铺显示会让人以为"这些都是一份完整备份"，于是删掉中间一份、
 * 或者以为删了 inc 只是少一个时间点（实际上它后面的环全都恢复不回去）。
 *
 * 判定：名字是 auto 且有 base_id 的属于链；手动备份、以及增量功能之前的老备份
 * （没有清单 → kind/base_id 为空）照旧按独立的一份显示。
 */
function splitBackups(backups: BackupItem[]): { chains: Chain[]; standalone: BackupItem[] } {
  const byChain = new Map<string, BackupItem[]>()
  const standalone: BackupItem[] = []
  for (const b of backups) {
    const isAuto = (b.name || '').toLowerCase() === 'auto'
    if (!isAuto || !b.base_id) {
      standalone.push(b)
      continue
    }
    const list = byChain.get(b.base_id) || []
    list.push(b)
    byChain.set(b.base_id, list)
  }
  const chains: Chain[] = []
  byChain.forEach((members, key) => {
    members.sort((a, b) => a.created_at - b.created_at)
    chains.push({
      key,
      members,
      latest: members[members.length - 1].created_at,
      totalSize: members.reduce((sum, m) => sum + m.size, 0),
    })
  })
  chains.sort((a, b) => b.latest - a.latest)
  standalone.sort((a, b) => b.created_at - a.created_at)
  return { chains, standalone }
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
  // 默认展开最近一条链，其余收起：链多的时候一屏放不下
  const [openChains, setOpenChains] = useState<Record<string, boolean>>({})
  const [touchedChains, setTouchedChains] = useState(false)

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

  const { chains, standalone } = splitBackups(backups)
  // 第一次拿到数据时把最新的链展开
  useEffect(() => {
    if (!touchedChains && chains.length > 0) {
      setOpenChains({ [chains[0].key]: true })
    }
  }, [chains.length, touchedChains])

  const toggleChain = (key: string) => {
    setTouchedChains(true)
    setOpenChains((prev) => ({ ...prev, [key]: !prev[key] }))
  }

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

  const remove = async (b: BackupItem, chain?: Chain) => {
    let tip = `确定删除备份「${b.name}」？`
    if (chain && chain.members.length > 1) {
      // 删中间一环 = 它后面那些增量都恢复不回去。必须在动手前说清楚。
      const idx = chain.members.findIndex((m) => m.backup_id === b.backup_id)
      const after = chain.members.length - idx - 1
      tip += after > 0
        ? `\n\n⚠️ 这是一条 ${chain.members.length} 环的增量链，删掉第 ${idx + 1} 环后，` +
          `它之后的 ${after} 环将无法恢复。\n要腾空间请删整条链（从最新一环往旧删）。`
        : `\n\n这是这条 ${chain.members.length} 环增量链的最新一环，删掉它不影响更早的环。`
    }
    if (!confirm(tip)) return
    try {
      await deleteBackup(instanceId, b.backup_id)
      await load()
    } catch (e: any) {
      setError(e.message)
    }
  }

  const restore = async (b: BackupItem, chain?: Chain) => {
    if (running) {
      setError('实例正在运行，请先停止实例再回滚')
      return
    }
    let tip = `确定回滚到「${b.name}」？\n\n当前世界存档将被备份内容覆盖（不可撤销）。\n建议先创建一份当前状态的备份。`
    if (chain && chain.members.length > 1) {
      const idx = chain.members.findIndex((m) => m.backup_id === b.backup_id)
      tip = `确定回滚到 ${formatTime(b.created_at)}？\n\n` +
        `这是一条 ${chain.members.length} 环的增量链，会按「基线 + 到这一环为止的增量」还原，` +
        `效果等同于当时（第 ${idx + 1}/${chain.members.length} 环）的完整状态。\n\n` +
        `当前存档会被覆盖（不可撤销）。建议先创建一份当前状态的备份。`
    }
    if (!confirm(tip)) return
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
          // 名称是**可选**的：留空时由面板生成"手动-<时间>"。
          // 这里必须说清"留空也安全" —— 否则用户会以为不填名字等于随便存一份，
          // 而早先的 bug 正是"不填名字"导致手动备份被当成自动备份滚动淘汰。
          placeholder="备份名称（可选，如：开荒前；留空自动命名）"
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
      {/* 手动/自动的区别直接写在界面上：这决定了备份会不会被自动清理 */}
      <div className="backups-hint">
        「立即备份」创建的是<b>手动备份</b>（完整快照），不会被自动清理的梯度规则淘汰
        （只受保留策略里的「手动保留份数」限制）；<b>自动备份是增量的</b>：一条链 = 1 份完整基线 +
        若干只含变化文件的增量，回滚任意一环都会按整条链还原成当时的完整状态。
        保留策略以<b>整条链</b>为单位淘汰 —— 不会出现「留了增量、删了基线」那种恢复不回去的组合。
      </div>
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
            {chains.map((c) => {
              const open = !!openChains[c.key]
              const head = c.members[c.members.length - 1]
              return (
                <Fragment key={c.key}>
                  <tr className="chain-row">
                    <td>
                      <button
                        className="chain-toggle"
                        onClick={() => toggleChain(c.key)}
                        title={open ? '收起这条链' : '展开这条链的每一环'}
                      >
                        {open ? '▾' : '▸'}
                      </button>
                      <span className="chain-title">自动备份链</span>
                      <span className="chain-badge">{c.members.length} 环</span>
                      <span className="chain-sub">基线 1 份 + 增量 {c.members.length - 1} 份</span>
                    </td>
                    {/* 大小列给的是**整条链**占的空间：这才是用户关心的"腾地方要删多少" */}
                    <td>{formatSize(c.totalSize)}<span className="chain-sub">（合计）</span></td>
                    <td>{formatTime(head.created_at)}</td>
                    <td>
                      <div className="backup-actions">
                        <button onClick={() => restore(head, c)} disabled={!canWrite || busy}>回滚到最新</button>
                      </div>
                    </td>
                  </tr>
                  {open && c.members.map((m, i) => (
                    <tr key={m.backup_id} className="chain-member-row">
                      <td>
                        <span className="chain-member-name">
                          {i === 0 ? '基线（完整快照）' : `增量 ${i}`}
                        </span>
                        <span className="chain-member-id">{m.backup_id}</span>
                      </td>
                      <td>{formatSize(m.size)}</td>
                      <td>{formatTime(m.created_at)}</td>
                      <td>
                        <div className="backup-actions">
                          <button onClick={() => restore(m, c)} disabled={!canWrite || busy}>回滚</button>
                          <button className="danger" onClick={() => remove(m, c)} disabled={!canWrite || busy}>删除</button>
                        </div>
                      </td>
                    </tr>
                  ))}
                </Fragment>
              )
            })}
            {standalone.map((b) => (
              <tr key={b.backup_id}>
                <td>
                  {b.name}
                  {b.kind === 'base' && <span className="chain-badge soft">完整快照</span>}
                </td>
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
