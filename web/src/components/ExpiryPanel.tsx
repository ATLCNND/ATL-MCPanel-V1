import { useEffect, useState } from 'react'
import { Instance, setInstanceExpiry } from '../api'
import './ExpiryPanel.css'

/**
 * ExpiryPanel 实例到期时间。
 *
 * 为什么从详情页**右栏**搬到「任务」标签页：
 *   1. 语义上本来就是一类东西 —— 任务页汇总"这台实例什么时候会自动做什么"
 *      （几点开机 / 几点关机 / 定时备份 / **到期自动停止**），
 *      到期是其中一条；单拎在右栏反而最难找
 *   2. 右栏本来就挤（实例信息 / 资源占用 / 到期 / 运行时长 / 备份日历）
 *   3. 这是"低频但重要"的设置，值得占一整行，而不是塞在 240px 的窄栏里
 *
 * ⚠️ 权限仍是**与删除同级**（总管理员 / 该节点的节点用户），由父组件算好传进来。
 * 不能因为"搬了个位置"就顺手放宽 —— 让租户自己续期的话，到期就失去意义了。
 */
export default function ExpiryPanel({ instanceId, inst, onChanged }: {
  instanceId: string
  inst: Instance | null
  /** 保存/清除之后通知父组件重新拉取实例（右栏与实例列表也要跟着变） */
  onChanged: () => void | Promise<void>
}) {
  // 本地编辑态（datetime-local 需要 `YYYY-MM-DDTHH:mm` 这种没有时区的格式）
  const [input, setInput] = useState('')
  const [autostop, setAutostop] = useState(true)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const [msg, setMsg] = useState('')

  // 实例数据到达/刷新后同步编辑框，但别覆盖用户正在输入的值
  useEffect(() => {
    if (!inst) return
    setAutostop(inst.expiry_autostop !== false)
    if (!inst.expires_at) { setInput(''); return }
    const d = new Date(inst.expires_at)
    if (isNaN(d.getTime())) { setInput(''); return }
    const pad = (n: number) => String(n).padStart(2, '0')
    setInput(
      `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(d.getMinutes())}`,
    )
  }, [inst?.expires_at, inst?.expiry_autostop, instanceId])

  const save = async () => {
    if (!input) { setError('请先选择到期时间（或点「清除」取消到期）'); return }
    setBusy(true); setError(''); setMsg('')
    try {
      const r = await setInstanceExpiry(instanceId, { expires_at: input, autostop })
      setMsg(r?.message || '已保存')
      await onChanged()
    } catch (e: any) {
      setError(e.message)
    } finally {
      setBusy(false)
    }
  }

  const clear = async () => {
    if (!confirm('清除到期时间？该实例将不再自动停止。')) return
    setBusy(true); setError(''); setMsg('')
    try {
      const r = await setInstanceExpiry(instanceId, { expires_at: '', autostop })
      setMsg(r?.message || '已清除')
      setInput('')
      await onChanged()
    } catch (e: any) {
      setError(e.message)
    } finally {
      setBusy(false)
    }
  }

  const st = inst?.expiry_state || 'none'
  const fmtAt = (s?: string) =>
    s ? new Date(s).toLocaleString('zh-CN', { hour12: false }) : ''

  const stateText =
    st === 'expired' ? `已到期（${fmtAt(inst?.expires_at)}）`
      : st === 'soon' ? `还有 ${inst?.expiry_days_left} 天到期`
        : st === 'active' ? `到期：${fmtAt(inst?.expires_at)}`
          : '未设置到期时间（永不过期）'

  return (
    <div className="task-card">
      <div className="tc-head">
        <span className={`tc-dot ${st === 'none' ? 'off' : 'on'}`} />
        <span className="tc-name">到期时间</span>
        <span className={`expiry-badge st-${st}`}>{stateText}</span>
      </div>

      <div className="expiry-form">
        <input
          type="datetime-local"
          value={input}
          onChange={(e) => setInput(e.target.value)}
          aria-label="到期时间"
        />
        <label className="expiry-auto">
          <input type="checkbox" checked={autostop} onChange={(e) => setAutostop(e.target.checked)} />
          到期自动停止
        </label>
        <div className="expiry-actions">
          <button className="primary" onClick={save} disabled={busy}>
            {busy ? '保存中…' : '保存'}
          </button>
          {inst?.expires_at && (
            <button onClick={clear} disabled={busy}>清除</button>
          )}
        </div>
      </div>

      {error && <div className="tc-err expiry-msg err">{error}</div>}
      {msg && <div className="expiry-msg ok">{msg}</div>}

      <div className="tc-note">
        到期<strong>不会删除数据</strong>，只是停止实例；续期后可重新启动。
        提前 {inst?.expiry_notice_days || 3} 天开始告警。
        到期时间由管理员设置，与删除实例同级权限。
      </div>
    </div>
  )
}
