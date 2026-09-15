import { useEffect, useState } from 'react'
import { BackupPolicy, RetentionTier, listBackupPolicies, saveBackupPolicy, deleteBackupPolicy } from '../api'
import './BackupPolicies.css'

// 把小时数转成易读文本
function fmtHours(h: number): string {
  if (h >= 24 && h % 24 === 0) return `${h / 24} 天`
  if (h > 24) return `${Math.floor(h / 24)} 天 ${h % 24} 小时`
  return `${h} 小时`
}

const PRESETS: { name: string; desc: string; interval: number; manual: number; tiers: RetentionTier[] }[] = [
  {
    name: '标准',
    desc: '自动 6 小时一次，保存 2 天',
    interval: 6,
    manual: 3,
    tiers: [{ within_hours: 24, keep: 4 }, { within_hours: 48, keep: 2 }],
  },
  {
    name: '轻量',
    desc: '自动 12 小时一次，保存 3 天，占用更小',
    interval: 12,
    manual: 3,
    tiers: [{ within_hours: 24, keep: 2 }, { within_hours: 72, keep: 3 }],
  },
  {
    name: '密集',
    desc: '自动 1 小时一次，近期可回滚到任意小时',
    interval: 1,
    manual: 5,
    tiers: [{ within_hours: 6, keep: 6 }, { within_hours: 24, keep: 4 }, { within_hours: 72, keep: 3 }],
  },
  {
    name: '长期归档',
    desc: '自动 24 小时一次，保留 30 天',
    interval: 24,
    manual: 5,
    tiers: [{ within_hours: 24 * 7, keep: 7 }, { within_hours: 24 * 30, keep: 23 }],
  },
]

export default function BackupPolicies({ onChanged }: { onChanged?: () => void }) {
  const [list, setList] = useState<BackupPolicy[]>([])
  const [editing, setEditing] = useState<Partial<BackupPolicy> | null>(null)
  const [error, setError] = useState('')
  const [msg, setMsg] = useState('')
  const [busy, setBusy] = useState(false)

  const load = async () => {
    try {
      const r = await listBackupPolicies()
      setList(Array.isArray(r) ? r : [])
      setError('')
    } catch (e: any) {
      setError(e.message)
    }
  }

  useEffect(() => { load() }, [])

  const startNew = (preset?: typeof PRESETS[number]) => {
    setEditing({
      name: preset ? preset.name : '',
      remark: preset ? preset.desc : '',
      auto_interval_hours: preset ? preset.interval : 6,
      manual_keep: preset ? preset.manual : 3,
      tiers: preset ? JSON.parse(JSON.stringify(preset.tiers)) : [{ within_hours: 24, keep: 4 }],
      include_config: true,
      is_default: false,
    })
    setMsg(''); setError('')
  }

  const save = async () => {
    if (!editing) return
    setBusy(true); setError(''); setMsg('')
    try {
      const r = await saveBackupPolicy(editing)
      setMsg(r.message || '已保存')
      setEditing(null)
      await load()
      onChanged?.()
    } catch (e: any) {
      setError(e.message)
    } finally {
      setBusy(false)
    }
  }

  const remove = async (p: BackupPolicy) => {
    setError(''); setMsg('')
    try {
      const r = await deleteBackupPolicy(p.id)
      setMsg(r.message || '已删除')
      await load()
      onChanged?.()
    } catch (e: any) {
      setError(e.message)
    }
  }

  const setTier = (i: number, patch: Partial<RetentionTier>) => {
    if (!editing?.tiers) return
    const tiers = editing.tiers.map((t, idx) => (idx === i ? { ...t, ...patch } : t))
    setEditing({ ...editing, tiers })
  }

  return (
    <div className="policy-panel">
      <div className="policy-head">
        <h4>备份保留策略</h4>
        <div className="spacer" />
        <button onClick={() => startNew()} disabled={busy}>+ 新建策略</button>
      </div>
      <p className="policy-hint">
        策略决定「多久自动备份一次」与「旧备份如何淘汰」。梯度保留的含义是：
        <b>近期密集、远期稀疏</b> —— 例如最近 24 小时留 4 份（每 6 小时一份，可回滚到任意 6 小时前），
        1~2 天只留 2 份（每天一份），更早的自动删除。
        <br />
        <b>手动备份与自动备份分别计数</b>，手动备份不会被自动备份的滚动淘汰挤掉。
      </p>

      {error && <div className="error-banner">{error}</div>}
      {msg && <div className="success-banner">{msg}</div>}

      {/* 预设：一键套用常见配置 */}
      <div className="policy-presets">
        <span className="muted">快速套用：</span>
        {PRESETS.map((p) => (
          <button key={p.name} className="preset-btn" onClick={() => startNew(p)} disabled={busy} title={p.desc}>
            {p.name}
          </button>
        ))}
      </div>

      <div className="policy-list">
        {list.map((p) => (
          <div className={`policy-item ${p.is_default ? 'is-default' : ''}`} key={p.id}>
            <div className="pi-main">
              <div className="pi-name">
                {p.name}
                {p.is_default && <span className="tag-default">默认</span>}
                {p.used_by > 0 && <span className="tag-used">{p.used_by} 个实例在用</span>}
              </div>
              <div className="pi-desc">{p.describe}</div>
              <div className="pi-meta">
                自动间隔 <b>{fmtHours(p.auto_interval_hours)}</b> · 手动备份保留 <b>{p.manual_keep}</b> 份
                {p.remark ? ` · ${p.remark}` : ''}
              </div>
            </div>
            <div className="pi-ops">
              <button onClick={() => { setEditing(JSON.parse(JSON.stringify(p))); setMsg(''); setError('') }}>编辑</button>
              {!p.is_default && (
                <button className="danger-outline" onClick={() => remove(p)} disabled={p.used_by > 0}
                  title={p.used_by > 0 ? '正被实例使用，无法删除' : '删除'}>
                  删除
                </button>
              )}
            </div>
          </div>
        ))}
        {list.length === 0 && <div className="empty">暂无策略</div>}
      </div>

      {editing && (
        <div className="policy-editor">
          <h5>{editing.id ? `编辑策略 #${editing.id}` : '新建策略'}</h5>

          <div className="pe-row">
            <label>
              策略名称
              <input value={editing.name || ''} onChange={(e) => setEditing({ ...editing, name: e.target.value })}
                placeholder="如：标准、高频、长期归档" />
            </label>
            <label>
              自动备份间隔（小时）
              <input type="number" min={1} value={editing.auto_interval_hours ?? 6}
                onChange={(e) => setEditing({ ...editing, auto_interval_hours: Number(e.target.value) })} />
            </label>
            <label>
              手动备份保留份数
              <input type="number" min={0} value={editing.manual_keep ?? 3}
                onChange={(e) => setEditing({ ...editing, manual_keep: Number(e.target.value) })} />
            </label>
          </div>

          <label className="pe-remark">
            备注
            <input value={editing.remark || ''} onChange={(e) => setEditing({ ...editing, remark: e.target.value })}
              placeholder="用途说明（可选）" />
          </label>

          <div className="pe-tiers">
            <div className="pe-tiers-head">
              <span>梯度保留规则</span>
              <div className="spacer" />
              <button className="btn-sm" onClick={() =>
                setEditing({ ...editing, tiers: [...(editing.tiers || []), { within_hours: 24, keep: 1 }] })
              }>+ 增加档位</button>
            </div>
            <div className="tier-table">
              <div className="tier-row tier-header">
                <span>距今时间范围内</span><span>最多保留</span><span></span>
              </div>
              {(editing.tiers || []).map((t, i) => (
                <div className="tier-row" key={i}>
                  <span>
                    最近 <input type="number" min={1} value={t.within_hours}
                      onChange={(e) => setTier(i, { within_hours: Number(e.target.value) })} style={{ width: 70 }} /> 小时
                  </span>
                  <span>
                    <input type="number" min={1} value={t.keep}
                      onChange={(e) => setTier(i, { keep: Number(e.target.value) })} style={{ width: 70 }} /> 份
                  </span>
                  <span>
                    <button className="btn-sm danger-outline" onClick={() =>
                      setEditing({ ...editing, tiers: (editing.tiers || []).filter((_, idx) => idx !== i) })
                    }>移除</button>
                  </span>
                </div>
              ))}
              {(editing.tiers || []).length === 0 && (
                <div className="tier-empty">未配置梯度 → 自动备份不会被淘汰（需自行清理）</div>
              )}
            </div>
            <div className="tier-preview">
              效果预览：{(editing.tiers || []).length === 0
                ? '不自动淘汰'
                : [...(editing.tiers || [])]
                    .sort((a, b) => a.within_hours - b.within_hours)
                    .map((t, i, arr) => {
                      const lo = i > 0 ? arr[i - 1].within_hours : 0
                      const range = lo === 0 ? `最近 ${fmtHours(t.within_hours)}` : `${fmtHours(lo)}~${fmtHours(t.within_hours)}`
                      return `${range} 留 ${t.keep} 份`
                    })
                    .join('；') + '；更早的删除'}
            </div>
          </div>

          <div className="pe-flags">
            <label className="switch">
              <input type="checkbox" checked={editing.include_config ?? true}
                onChange={(e) => setEditing({ ...editing, include_config: e.target.checked })} />
              备份包含配置文件
            </label>
            <label className="switch">
              <input type="checkbox" checked={editing.is_default ?? false}
                onChange={(e) => setEditing({ ...editing, is_default: e.target.checked })} />
              设为默认策略（未指派策略的实例使用它）
            </label>
          </div>

          <div className="pe-ops">
            <button onClick={() => setEditing(null)} disabled={busy}>取消</button>
            <button className="primary" onClick={save} disabled={busy || !editing.name?.trim()}>
              {busy ? '保存中…' : '保存策略'}
            </button>
          </div>
        </div>
      )}
    </div>
  )
}
