import { useEffect, useRef, useState } from 'react'
import { MyProfile, getMyProfile, uploadAvatar, deleteMyAvatar } from '../api'
import Avatar from './Avatar'
import './AccountCard.css'

/** 秒 → 「N 天」「N 小时」这类易读文本 */
function fmtDuration(sec: number): { v: string; u: string } {
  if (!sec || sec <= 0) return { v: '0', u: ' 分钟' }
  const d = Math.floor(sec / 86400)
  if (d > 0) return { v: String(d), u: ' 天' }
  const h = Math.floor(sec / 3600)
  if (h > 0) return { v: String(h), u: ' h' }
  return { v: String(Math.floor(sec / 60)), u: ' 分钟' }
}

/**
 * AccountCard 左栏的「我的账号」卡片。
 *
 * 头像需管理员审核后才对外可见：
 *   none      → 字母头像
 *   pending   → 仍显示字母头像，但给出「审核中」提示
 *   rejected  → 显示驳回原因，可重新上传
 *   approved  → 显示上传的图片
 */
export default function AccountCard({ onOpenSettings }: { onOpenSettings?: () => void }) {
  const [me, setMe] = useState<MyProfile | null>(null)
  const [error, setError] = useState('')
  const [msg, setMsg] = useState('')
  const [busy, setBusy] = useState(false)
  const fileRef = useRef<HTMLInputElement>(null)

  const load = () => {
    getMyProfile()
      .then(setMe)
      .catch((e) => setError(e.message))
  }

  useEffect(() => { load() }, [])

  const pick = () => fileRef.current?.click()

  const onFile = async (e: React.ChangeEvent<HTMLInputElement>) => {
    const f = e.target.files?.[0]
    e.target.value = '' // 允许连续选择同一文件
    if (!f) return
    setBusy(true); setError(''); setMsg('')
    try {
      const r = await uploadAvatar(f)
      setMsg(r.message || '已提交审核')
      load()
    } catch (err: any) {
      setError(err.message)
    } finally {
      setBusy(false)
    }
  }

  const remove = async () => {
    setBusy(true); setError(''); setMsg('')
    try {
      await deleteMyAvatar()
      setMsg('头像已移除')
      load()
    } catch (err: any) {
      setError(err.message)
    } finally {
      setBusy(false)
    }
  }

  // 首字母与"有没有图"的判断都交给 Avatar 组件了，这里不再自己算
  const online = fmtDuration(me?.total_online_seconds || 0)
  const days = me?.registered_at
    ? Math.max(0, Math.floor((Date.now() - new Date(me.registered_at).getTime()) / 86400000))
    : 0

  const status = me?.avatar_status || 'none'

  return (
    <div className="acct-card">
      <div className="side-card-title">
        <span>我的账号</span>
        {onOpenSettings && <span className="side-card-more" onClick={onOpenSettings} title="账户设置">⋯</span>}
      </div>

      <div className="acct">
        <div className="acct-avatar-box" onClick={pick} title="点击上传头像">
          {/* 用共用的 Avatar 组件：avatar_url 只在审核通过后才有值，
              这里不必再判一次状态，也就不会再出现"某处漏判/漏用"的问题 */}
          <Avatar url={me?.avatar_url} name={me?.username} size={64} />
          <span className="acct-avatar-cam" title="上传头像">＋</span>
        </div>
        <input
          ref={fileRef}
          type="file"
          accept="image/png,image/jpeg,image/webp,image/gif"
          style={{ display: 'none' }}
          onChange={onFile}
        />

        <div className="acct-name">{me?.username || '—'}</div>
        <div className="acct-role">
          {me?.role === 'admin' ? '管理员' : '用户'}
          {me ? ` · UID ${me.id}` : ''}
        </div>

        {status === 'pending' && <div className="acct-review">⏳ 新头像审核中</div>}
        {status === 'rejected' && (
          <div className="acct-rejected" title={me?.avatar_note}>
            ✕ 头像未通过{me?.avatar_note ? `：${me.avatar_note}` : ''}
          </div>
        )}

        <div className="acct-stats">
          <div className="as">
            <div className="k">入站时长</div>
            <div className="v">{days}<small> 天</small></div>
          </div>
          <div className="as">
            <div className="k">累计在线</div>
            <div className="v">{online.v}<small>{online.u}</small></div>
          </div>
        </div>

        <div className="acct-ops">
          <button className="acct-btn" onClick={pick} disabled={busy}>
            {status === 'none' ? '上传头像' : '更换头像'}
          </button>
          {status !== 'none' && (
            <button className="acct-btn ghost" onClick={remove} disabled={busy} title="移除已上传的头像">移除</button>
          )}
        </div>

        {error && <div className="acct-msg err">{error}</div>}
        {msg && <div className="acct-msg ok">{msg}</div>}
      </div>
    </div>
  )
}
