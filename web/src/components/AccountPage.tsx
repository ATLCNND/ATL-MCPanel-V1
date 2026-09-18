import { useEffect, useMemo, useRef, useState } from 'react'
import {
  MyProfile, MyPermission, MyPortLine, Instance,
  getMyProfile, listMyPermissions, listMyPorts, listInstances, getInstanceRuntime,
  changePassword, uploadAvatar, deleteMyAvatar, roleLabel, isNodeUser, levelAtLeast,
  renameUser, updateCurrentUsername,
} from '../api'
import Avatar from './Avatar'
import './AccountPage.css'

/** 字节 → 易读文本 */
function fmtBytes(n: number): string {
  if (!n || n <= 0) return '0 B'
  const u = ['B', 'KB', 'MB', 'GB', 'TB']
  let i = 0
  let v = n
  while (v >= 1024 && i < u.length - 1) { v /= 1024; i++ }
  return `${v >= 100 ? Math.round(v) : v.toFixed(1)} ${u[i]}`
}

/** 秒 → 「N 天 / N 小时 / N 分钟」 */
function fmtDuration(sec: number): string {
  if (!sec || sec <= 0) return '0 分钟'
  const d = Math.floor(sec / 86400)
  if (d > 0) return `${d} 天`
  const h = Math.floor(sec / 3600)
  if (h > 0) return `${h} 小时`
  return `${Math.floor(sec / 60)} 分钟`
}

const LEVEL_LABEL: Record<string, string> = {
  owner: '拥有者',
  collab: '协作者',
  viewer: '只读',
}

/**
 * AccountPage 账户页。
 *
 * 为什么从弹窗改成页面：弹窗里只放得下"改密码"一件事，
 * 而用户其实想知道的是**我有什么** —— 我有几台实例、能对它们做什么、
 * 我还有几个公网端口配额、占了多少磁盘。这些都要展开看，弹窗撑不住表格。
 *
 * 数据来源（全是已有的接口，没有为此新增后端）：
 *   getMyProfile        —— 账号、角色、注册时间、累计在线、头像
 *   listInstances       —— 我能看到的实例（带 level），据此统计台数与磁盘
 *   listMyPermissions   —— 角色 + 被授权的实例清单
 *   listMyPorts         —— 各线路端口配额与已用
 *
 * 磁盘占用是**逐台问 runtime** 得到的（`listInstances` 不带磁盘字段）。
 * Daemon 侧对每台实例的目录大小有 60 秒缓存，所以这里并发拉取是安全的；
 * 但仍做了并发上限与失败容忍：拿不到的实例只计入"未能统计"，不把总数算错。
 */
export default function AccountPage({ onLogout }: { onLogout: () => void }) {
  const [me, setMe] = useState<MyProfile | null>(null)
  const [instances, setInstances] = useState<Instance[]>([])
  const [perms, setPerms] = useState<MyPermission[]>([])
  const [ports, setPorts] = useState<MyPortLine[]>([])
  const [disk, setDisk] = useState<{ used: number; ok: number; failed: number }>({ used: 0, ok: 0, failed: 0 })
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')
  const [msg, setMsg] = useState('')

  // 头像上传
  const fileRef = useRef<HTMLInputElement>(null)

  // 改密码
  const [oldPw, setOldPw] = useState('')
  const [newPw, setNewPw] = useState('')
  const [confirmPw, setConfirmPw] = useState('')
  const [pwBusy, setPwBusy] = useState(false)

  // 改用户名（就地编辑）
  const [editingName, setEditingName] = useState(false)
  const [nameDraft, setNameDraft] = useState('')
  const [nameBusy, setNameBusy] = useState(false)

  const loadProfile = () => getMyProfile().then(setMe).catch((e) => setError(e.message))

  useEffect(() => {
    let alive = true
    ;(async () => {
      setLoading(true)
      const [p, inst, pr, po] = await Promise.allSettled([
        getMyProfile(), listInstances(), listMyPermissions(), listMyPorts(),
      ])
      if (!alive) return
      if (p.status === 'fulfilled') setMe(p.value)
      else setError(String((p as any).reason?.message || p.reason))
      if (inst.status === 'fulfilled') setInstances(Array.isArray(inst.value) ? inst.value : [])
      if (pr.status === 'fulfilled') setPerms(pr.value?.permissions || [])
      if (po.status === 'fulfilled') setPorts(Array.isArray(po.value) ? po.value : [])
      setLoading(false)

      // 磁盘：只统计"我是拥有者"的实例（别人实例的占用不该算在我头上）。
      // 并发 4 个一批，避免一次性打太多 RPC。
      if (inst.status === 'fulfilled') {
        const mine = (inst.value || []).filter((i) => i.level === 'owner')
        let used = 0, ok = 0, failed = 0
        for (let i = 0; i < mine.length; i += 4) {
          const batch = mine.slice(i, i + 4)
          const rs = await Promise.allSettled(batch.map((x) => getInstanceRuntime(x.instance_id)))
          for (const r of rs) {
            if (r.status === 'fulfilled' && typeof (r.value as any)?.disk_used === 'number') {
              used += (r.value as any).disk_used
              ok++
            } else failed++
          }
          if (!alive) return
          setDisk({ used, ok, failed })
        }
        if (alive) setDisk({ used, ok, failed })
      }
    })()
    return () => { alive = false }
  }, [])

  /** 按级别统计台数 */
  const counts = useMemo(() => {
    const c = { owner: 0, collab: 0, viewer: 0 }
    for (const i of instances) {
      if (i.level === 'owner') c.owner++
      else if (i.level === 'collab') c.collab++
      else if (i.level === 'viewer') c.viewer++
    }
    return c
  }, [instances])

  const portTotal = useMemo(() => {
    let quota = 0, used = 0, available = 0
    for (const l of ports) {
      if (l.unlimited) return { unlimited: true, quota: -1, used: 0, available: -1 }
      quota += l.quota
      used += l.used
      available += Math.max(0, l.available)
    }
    return { unlimited: false, quota, used, available }
  }, [ports])

  const running = instances.filter((i) => (i.live_status || i.status) === 'running').length

  const pick = () => fileRef.current?.click()

  const onFile = async (e: React.ChangeEvent<HTMLInputElement>) => {
    const f = e.target.files?.[0]
    e.target.value = ''
    if (!f) return
    setError(''); setMsg('')
    try {
      const r = await uploadAvatar(f)
      setMsg(r.message || '已提交审核')
      loadProfile()
    } catch (err: any) {
      setError(err.message)
    }
  }

  const removeAvatar = async () => {
    setError(''); setMsg('')
    try {
      await deleteMyAvatar()
      setMsg('头像已移除')
      loadProfile()
    } catch (err: any) {
      setError(err.message)
    }
  }

  const submitPassword = async () => {
    setError(''); setMsg('')
    if (newPw !== confirmPw) { setError('两次输入的新密码不一致'); return }
    if (newPw.length < 6) { setError('新密码至少 6 位'); return }
    setPwBusy(true)
    try {
      await changePassword(oldPw, newPw)
      setMsg('密码修改成功')
      setOldPw(''); setNewPw(''); setConfirmPw('')
    } catch (e: any) {
      setError(e.message)
    } finally {
      setPwBusy(false)
    }
  }

  const startEditName = () => {
    setNameDraft(me?.username || '')
    setEditingName(true)
    setError(''); setMsg('')
  }

  const cancelEditName = () => {
    setEditingName(false)
    setNameDraft('')
  }

  /**
   * 提交改名。
   *
   * 改完要同时更新本地登录态里的用户名 —— 令牌里带的仍是签发时的旧值，
   * 而且**不会**因为改名而失效（后端鉴权只看 UID，不看用户名），
   * 所以这里不需要强制重新登录；只要把界面上显示的名字刷新过来即可。
   */
  const submitName = async () => {
    if (!me) return
    const n = nameDraft.trim()
    if (!n || n === me.username) { setEditingName(false); return }
    setNameBusy(true); setError(''); setMsg('')
    try {
      const r = await renameUser(me.id, n)
      updateCurrentUsername(n)
      setEditingName(false)
      setMsg(r?.message || '用户名已修改')
      loadProfile()
    } catch (e: any) {
      setError(e.message)
    } finally {
      setNameBusy(false)
    }
  }

  // 首字母交给 Avatar 组件；avatar_status 仍要用来显示"审核中/被驳回"提示
  const avatarStatus = me?.avatar_status || 'none'
  const days = me?.registered_at
    ? Math.max(0, Math.floor((Date.now() - new Date(me.registered_at).getTime()) / 86400000))
    : 0

  return (
    <div className="account-page">
      {error && <div className="error-banner">{error}</div>}
      {msg && <div className="success-banner">{msg}</div>}

      {/* ---------------- 账号 ---------------- */}
      <section className="ap-card ap-identity ap-span-2">
        <div className="ap-avatar-box" onClick={pick} title="点击上传头像">
          <Avatar url={me?.avatar_url} name={me?.username} size={96} />
          <span className="ap-avatar-cam" title="上传头像">＋</span>
        </div>
        <input
          ref={fileRef}
          type="file"
          accept="image/png,image/jpeg,image/webp,image/gif"
          style={{ display: 'none' }}
          onChange={onFile}
        />

        <div className="ap-identity-main">
          <div className="ap-name">
            {editingName ? (
              <>
                <input
                  className="ap-name-input"
                  value={nameDraft}
                  maxLength={32}
                  autoFocus
                  spellCheck={false}
                  onChange={(e) => setNameDraft(e.target.value)}
                  onKeyDown={(e) => {
                    if (e.key === 'Enter') { e.preventDefault(); submitName() }
                    if (e.key === 'Escape') { e.preventDefault(); cancelEditName() }
                  }}
                />
                <button className="primary ap-name-btn" onClick={submitName} disabled={nameBusy}>
                  {nameBusy ? '保存中…' : '保存'}
                </button>
                <button className="ghost ap-name-btn" onClick={cancelEditName} disabled={nameBusy}>取消</button>
              </>
            ) : (
              <>
                <span className="ap-name-text">{me?.username || '—'}</span>
                {me && (
                  <button className="ap-name-pen" onClick={startEditName} title="修改用户名">✎</button>
                )}
                <span className={`ap-role role-${me?.role || 'user'}`}>{roleLabel(me?.role)}</span>
              </>
            )}
          </div>
          <div className="ap-meta">
            {me ? `UID ${me.id}` : '—'}
            {me?.registered_at ? ` · 注册 ${days} 天` : ''}
            {me ? ` · 累计在线 ${fmtDuration(me.total_online_seconds)}` : ''}
          </div>
          {editingName && (
            <div className="ap-note">
              用户名可以随时修改。UID {me?.id} 与实例授权、公网端口配额都不受影响；
              改完当前登录状态依然有效，无需重新登录。
            </div>
          )}
          {avatarStatus === 'pending' && <div className="ap-note pending">⏳ 新头像审核中，通过后其他人才能看到</div>}
          {avatarStatus === 'rejected' && (
            <div className="ap-note rejected" title={me?.avatar_note}>
              ✕ 头像未通过{me?.avatar_note ? `：${me.avatar_note}` : ''}
            </div>
          )}
        </div>

        {/* 头像操作贴右：宽卡片时不要挤在名字下面留着右半边空白 */}
        <div className="ap-avatar-ops">
          <button onClick={pick}>{avatarStatus === 'none' ? '上传头像' : '更换头像'}</button>
          {avatarStatus !== 'none' && <button className="ghost" onClick={removeAvatar}>移除头像</button>}
        </div>
      </section>

      {/* ---------------- 我的资源 ---------------- */}
      <section className="ap-card ap-span-2">
        <div className="ap-card-title">
          我的资源
          <span className="ap-card-sub">按「我是谁」统计，不含别人实例的占用</span>
        </div>

        {loading ? (
          <div className="ap-loading">加载中…</div>
        ) : (
          <>
            <div className="ap-stats">
              <div className="ap-stat">
                <div className="k">我的实例</div>
                <div className="v">{counts.owner}<small> 台</small></div>
                <div className="s">拥有者级别</div>
              </div>
              <div className="ap-stat">
                <div className="k">参与协作</div>
                <div className="v">{counts.collab}<small> 台</small></div>
                <div className="s">可启停 / 看控制台</div>
              </div>
              <div className="ap-stat">
                <div className="k">只读可见</div>
                <div className="v">{counts.viewer}<small> 台</small></div>
                <div className="s">仅查看</div>
              </div>
              <div className="ap-stat">
                <div className="k">正在运行</div>
                <div className="v">{running}<small> 台</small></div>
                <div className="s">共 {instances.length} 台可见</div>
              </div>
              <div className="ap-stat">
                <div className="k">磁盘占用</div>
                <div className="v">{fmtBytes(disk.used)}</div>
                <div className="s">
                  统计 {disk.ok} 台
                  {disk.failed > 0 && <span className="ap-warn"> · {disk.failed} 台未取到</span>}
                </div>
              </div>
              <div className="ap-stat">
                <div className="k">公网端口</div>
                <div className="v">
                  {/* 注意这里必须写真正的 JSX，不能用模板字符串 ——
                      写 `\`${used}<small> / ${quota}</small>\`` 的话，
                      <small> 会被当成**纯文本**原样显示出来（踩过）。 */}
                  {portTotal.unlimited ? '不限' : ports.length === 0 ? '—' : (
                    <>
                      {portTotal.used}
                      <small> / {portTotal.quota}</small>
                    </>
                  )}
                </div>
                <div className="s">
                  {/* 文案刻意短：这一格原来写「总管理员不受配额限制」（10 个汉字），
                      在 132px 的格子里会折成两行，比其它格子的副文字高一倍。
                      统一控制在 7 字以内，六格副文字才会齐平。 */}
                  {portTotal.unlimited
                    ? '不受配额限制'
                    : ports.length === 0
                      ? '未分配配额'
                      : `剩余 ${portTotal.available} 个`}
                </div>
              </div>
            </div>

            {ports.length > 0 && (
              <table className="ap-table">
                <thead>
                  <tr><th>线路</th><th>配额</th><th>已用</th><th>剩余</th></tr>
                </thead>
                <tbody>
                  {ports.map((l) => (
                    <tr key={l.frps_id}>
                      <td>
                        {l.frps_name}
                        {/* 线路地址只对总管理员显示（后端对普通用户根本不返回） */}
                        {l.frps_host && <div className="ap-sub mono">{l.frps_host}</div>}
                      </td>
                      <td>{l.unlimited ? '不限' : l.quota}</td>
                      <td>{l.used}</td>
                      <td className={l.available === 0 && !l.unlimited ? 'ap-over' : ''}>
                        {l.unlimited ? '—' : l.available}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            )}
            {!portTotal.unlimited && ports.length === 0 && (
              <div className="ap-hint">
                你还没有被分配公网端口配额 —— 需要开公网端口（BlueMap 网页、Geyser 基岩版等）
                时请联系总管理员分配。
              </div>
            )}
          </>
        )}
      </section>

      {/* ---------------- 我的权限 ---------------- */}
      <section className="ap-card">
        <div className="ap-card-title">
          我的权限
          <span className="ap-card-sub">被授权的实例与级别</span>
        </div>

        <div className="ap-role-note">
          <b>{roleLabel(me?.role)}</b>
          <span>
            {me?.role === 'admin'
              ? '拥有全部权限：所有节点、所有实例、用户管理与审计。'
              : isNodeUser(me?.role)
                ? '可在被授权的节点上创建与删除实例、设置到期时间；其它权限与普通用户相同。'
                : '只能操作被管理员授权的实例；不能自己创建实例。'}
          </span>
        </div>

        {perms.length === 0 ? (
          <div className="ap-hint">还没有被授权任何实例。</div>
        ) : (
          <table className="ap-table">
            <thead>
              <tr><th>实例</th><th>级别</th><th>能做什么</th></tr>
            </thead>
            <tbody>
              {perms.map((p) => (
                <tr key={p.instance_id}>
                  <td className="mono">{p.instance_id}</td>
                  <td>
                    <span className={`ap-level lv-${p.level}`}>{LEVEL_LABEL[p.level] || p.level}</span>
                  </td>
                  <td className="ap-abilities">
                    {levelAtLeast(p.level, 'collab') ? '启停 · 控制台 · 看文件' : '只看状态与日志'}
                    {levelAtLeast(p.level, 'owner') && ' · 改配置 · 开端口 · 删除'}
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </section>

      {/* ---------------- 修改密码 ---------------- */}
      <section className="ap-card">
        <div className="ap-card-title">
          修改密码
          <span className="ap-card-sub">修改后当前会话仍然有效，其它设备需重新登录</span>
        </div>
        <div className="ap-pw-form">
          <input type="password" placeholder="旧密码" value={oldPw}
            onChange={(e) => setOldPw(e.target.value)} autoComplete="current-password" />
          <input type="password" placeholder="新密码（至少 6 位）" value={newPw}
            onChange={(e) => setNewPw(e.target.value)} autoComplete="new-password" />
          <input type="password" placeholder="确认新密码" value={confirmPw}
            onChange={(e) => setConfirmPw(e.target.value)} autoComplete="new-password" />
          <button className="primary" onClick={submitPassword} disabled={pwBusy}>
            {pwBusy ? '提交中…' : '确认修改'}
          </button>
        </div>
      </section>

      <section className="ap-card ap-danger-zone ap-span-2">
        <div className="ap-card-title">会话</div>
        <button className="danger" onClick={onLogout}>退出登录</button>
      </section>
    </div>
  )
}
