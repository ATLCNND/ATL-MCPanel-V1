import { useEffect, useState } from 'react'
import { AvatarReviewItem, listAvatars, reviewAvatar, avatarRawUrl } from '../api'
import './AvatarReview.css'

const STATUS_TEXT: Record<string, string> = {
  pending: '待审核',
  approved: '已通过',
  rejected: '已驳回',
}

/**
 * AvatarReview 头像审核（仅管理员）。
 *
 * 审核必须能看到**待审图片本身** —— 因此走 /api/admin/avatars/{id}/raw
 * 这个管理员专用地址（对外接口只提供已通过的头像）。
 */
export default function AvatarReview() {
  const [tab, setTab] = useState<'pending' | 'all'>('pending')
  const [list, setList] = useState<AvatarReviewItem[]>([])
  const [error, setError] = useState('')
  const [msg, setMsg] = useState('')
  const [busy, setBusy] = useState(false)
  const [rejecting, setRejecting] = useState<number | null>(null)
  const [reason, setReason] = useState('')
  // 记录加载失败的图片（按用户 ID）：裂图既不美观也不说明问题，
  // 换成一句"图片加载失败"能让管理员知道是取不到图而不是自己看漏了
  const [imgFailed, setImgFailed] = useState<Record<number, boolean>>({})

  const load = async () => {
    try {
      const r = await listAvatars(tab === 'pending' ? 'pending' : 'all')
      setList(Array.isArray(r) ? r : [])
      setError('')
    } catch (e: any) {
      setError(e.message)
    }
  }

  useEffect(() => { load() }, [tab])

  const review = async (id: number, approve: boolean, note = '') => {
    setBusy(true); setError(''); setMsg('')
    try {
      const r = await reviewAvatar(id, approve, note)
      setMsg(r.message || '已处理')
      setRejecting(null)
      setReason('')
      await load()
    } catch (e: any) {
      setError(e.message)
    } finally {
      setBusy(false)
    }
  }

  const pendingCount = list.filter((x) => x.avatar_status === 'pending').length

  return (
    <div className="avatar-review">
      <div className="ar-head">
        <h4>头像审核</h4>
        <div className="spacer" />
        <div className="ar-tabs">
          <button className={tab === 'pending' ? 'active' : ''} onClick={() => setTab('pending')}>
            待审核{pendingCount > 0 && tab === 'pending' ? `（${pendingCount}）` : ''}
          </button>
          <button className={tab === 'all' ? 'active' : ''} onClick={() => setTab('all')}>全部</button>
        </div>
      </div>

      <p className="ar-hint">
        用户上传的头像需管理员通过后才会对外显示（未通过的显示字母头像）。
        驳回时可填写原因，用户会在自己的账号卡上看到。
      </p>

      {error && <div className="error-banner">{error}</div>}
      {msg && <div className="success-banner">{msg}</div>}

      {list.length === 0 && <div className="ar-empty">没有需要审核的头像</div>}

      <div className="ar-list">
        {list.map((it) => (
          <div className={`ar-item ${it.avatar_status}`} key={it.user_id}>
            {/* 走 avatarRawUrl：图片请求带不上 Authorization 头，必须用 ?token= */}
            {imgFailed[it.user_id] ? (
              <div className="ar-img ar-img-failed" title="图片加载失败（可能是令牌过期，刷新页面重试）">
                图片
                <br />
                加载失败
              </div>
            ) : (
              <img
                className="ar-img"
                src={avatarRawUrl(it.user_id)}
                alt={`${it.username} 的头像`}
                loading="lazy"
                onError={() => setImgFailed((m) => ({ ...m, [it.user_id]: true }))}
              />
            )}

            <div className="ar-meta">
              <div className="ar-name">
                {it.username}
                {it.role === 'admin' && <span className="ar-tag">管理员</span>}
                <span className={`ar-status ${it.avatar_status}`}>
                  {STATUS_TEXT[it.avatar_status] || it.avatar_status}
                </span>
              </div>
              <div className="ar-sub">
                UID {it.user_id}
                {it.reviewed_by_name && ` · 由 ${it.reviewed_by_name} 审核`}
                {it.avatar_note && ` · ${it.avatar_note}`}
              </div>
            </div>

            <div className="ar-ops">
              {it.avatar_status !== 'approved' && (
                <button className="primary" onClick={() => review(it.user_id, true)} disabled={busy}>
                  通过
                </button>
              )}
              {it.avatar_status !== 'rejected' && (
                <button
                  className="danger-outline"
                  onClick={() => { setRejecting(it.user_id); setReason('') }}
                  disabled={busy}
                >
                  驳回
                </button>
              )}
            </div>

            {rejecting === it.user_id && (
              <div className="ar-reject">
                <input
                  value={reason}
                  onChange={(e) => setReason(e.target.value)}
                  placeholder="驳回原因（会展示给用户，可留空）"
                  autoFocus
                />
                <button className="danger" onClick={() => review(it.user_id, false, reason)} disabled={busy}>
                  确认驳回
                </button>
                <button onClick={() => setRejecting(null)} disabled={busy}>取消</button>
              </div>
            )}
          </div>
        ))}
      </div>
    </div>
  )
}
