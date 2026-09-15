import { useEffect, useState } from 'react'
import { ScheduleConfig, BackupPolicy, getSchedule, setSchedule, listBackupPolicies, listBackups, BackupItem } from '../api'
import BackupPolicies from './BackupPolicies'
import './SchedulePanel.css'

function fmtTime(s: string): string {
  if (!s) return '—'
  const d = new Date(s)
  if (isNaN(d.getTime())) return s
  return d.toLocaleString('zh-CN')
}

export default function SchedulePanel({ instanceId, canWrite, isAdmin }: {
  instanceId: string
  canWrite: boolean
  isAdmin?: boolean
}) {
  const [cfg, setCfg] = useState<ScheduleConfig | null>(null)
  const [policies, setPolicies] = useState<BackupPolicy[]>([])
  const [policyID, setPolicyID] = useState('0')
  const [includeConfig, setIncludeConfig] = useState(true)
  const [backupDir, setBackupDir] = useState('')
  const [error, setError] = useState('')
  const [msg, setMsg] = useState('')
  const [busy, setBusy] = useState(false)

  const load = async () => {
    try {
      const c = await getSchedule(instanceId)
      setCfg(c)
      setPolicyID(String(c.policy_id || 0))
      setIncludeConfig(c.include_config)
      setError('')
      // 备份存放位置（管理员可见/可改；普通用户只读展示）
      try {
        const bks = await listBackups(instanceId)
        if (Array.isArray(bks) && bks.length > 0) {
          const anyB = bks[0] as BackupItem & { dir?: string }
          setBackupDir(anyB.dir || '')
        }
      } catch { /* 忽略 */ }
    } catch (e: any) {
      setError(e.message)
    }
  }

  useEffect(() => { load() }, [instanceId])

  // 策略列表仅管理员可拉取
  useEffect(() => {
    if (!isAdmin) return
    listBackupPolicies()
      .then((r) => setPolicies(Array.isArray(r) ? r : []))
      .catch(() => { /* 非管理员会 403，忽略 */ })
  }, [isAdmin])

  const save = async (enabled: boolean) => {
    setBusy(true); setError(''); setMsg('')
    try {
      const r = await setSchedule(instanceId, {
        enabled,
        interval_hours: cfg?.interval_hours || 6,
        keep: cfg?.keep || 0,
        include_config: includeConfig,
        policy_id: Number(policyID) || 0,
      })
      setMsg(r.message || '已保存')
      await load()
    } catch (e: any) {
      setError(e.message)
    } finally {
      setBusy(false)
    }
  }

  const selected = policies.find((p) => String(p.id) === policyID)

  return (
    <div className="schedule-panel">
      <h4>定时备份与保留策略</h4>
      <p className="schedule-hint">
        按策略自动备份世界与数据，并按梯度淘汰旧备份 —— 近期密集、远期稀疏，
        在有限的磁盘占用下保留尽可能长的可回滚范围。
      </p>

      <div className="schedule-form">
        <label>
          保留策略
          {isAdmin ? (
            <select value={policyID} onChange={(e) => setPolicyID(e.target.value)} disabled={!canWrite}>
              <option value="0">使用默认策略{cfg?.policy_id === 0 && cfg?.policy_name ? `（${cfg.policy_name}）` : ''}</option>
              {policies.filter((p) => !p.is_default).map((p) => (
                <option key={p.id} value={p.id}>{p.name}</option>
              ))}
            </select>
          ) : (
            <input value={cfg?.policy_name || '默认策略'} readOnly />
          )}
        </label>

        <label className="switch">
          <input
            type="checkbox"
            checked={includeConfig}
            onChange={(e) => setIncludeConfig(e.target.checked)}
            disabled={!canWrite}
          />
          备份包含配置文件
        </label>

        {canWrite && (
          <>
            <button className="primary" onClick={() => save(true)} disabled={busy}>
              {cfg?.enabled ? '保存' : '启用定时备份'}
            </button>
            {cfg?.enabled && (
              <button onClick={() => save(false)} disabled={busy}>停用</button>
            )}
          </>
        )}
      </div>

      {error && <div className="error-banner">{error}</div>}
      {msg && <div className="success-banner">{msg}</div>}

      {cfg && (
        <div className="schedule-status">
          <span className={`status ${cfg.enabled ? 'status-running' : 'status-stopped'}`}>
            {cfg.enabled ? '已启用' : '未启用'}
          </span>
          <span className="muted">上次执行：{fmtTime(cfg.last_run)}</span>
          {cfg.enabled && <span className="muted">下次执行：{fmtTime(cfg.next_run)}</span>}
          {cfg.last_error && <span className="err-text">上次失败：{cfg.last_error}</span>}
        </div>
      )}

      {/* 生效策略的实际规则 */}
      {cfg && (
        <div className="policy-effective">
          <div className="pe-line">
            <span className="pe-k">生效策略</span>
            <span className="pe-v">{cfg.policy_name || '默认策略'}</span>
          </div>
          <div className="pe-line">
            <span className="pe-k">自动间隔</span>
            <span className="pe-v">每 {cfg.effective_hours || cfg.interval_hours} 小时</span>
          </div>
          <div className="pe-line">
            <span className="pe-k">淘汰规则</span>
            <span className="pe-v pe-desc">{cfg.policy_desc || '—'}</span>
          </div>
          <div className="pe-line">
            <span className="pe-k">手动备份</span>
            <span className="pe-v">保留 {cfg.manual_keep} 份（独立计数，不受自动淘汰影响）</span>
          </div>
          {backupDir && (
            <div className="pe-line">
              <span className="pe-k">存放位置</span>
              <span className="pe-v pe-path">{backupDir}</span>
            </div>
          )}
        </div>
      )}

      {/* 策略管理：仅管理员 */}
      {isAdmin && <BackupPolicies onChanged={load} />}
    </div>
  )
}
