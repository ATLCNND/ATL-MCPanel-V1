import { useEffect, useMemo, useRef, useState } from 'react'
import {
  PlayerList, PlayerOverview, listPlayers, addPlayer, removePlayer, setWhitelist,
  getPlayerOverview, kickPlayer, setOpLevel,
} from '../api'
import Avatar from './Avatar'
import './PlayersTab.css'

type Kind = 'online' | 'all' | 'whitelist' | 'ops' | 'bans' | 'ipbans'

const KINDS: { key: Kind; label: string; hint: string }[] = [
  { key: 'online', label: '在线玩家', hint: '当前在线的玩家；可直接查看状态并执行踢出、封禁、白名单和 OP 操作。' },
  { key: 'all', label: '全部玩家信息', hint: '服务器见过的所有玩家：累计游戏时长、当前是否在线、白名单 / OP / 封禁状态。数据来自 usercache、世界统计数据与控制台日志。' },
  { key: 'whitelist', label: '白名单', hint: '仅名单内玩家可进入服务器' },
  { key: 'ops', label: '管理员', hint: '拥有 OP 权限的玩家' },
  { key: 'bans', label: '封禁玩家', hint: '被禁止进入的玩家' },
  { key: 'ipbans', label: '封禁 IP', hint: '被禁止的 IP 地址' },
]

function fmtDuration(sec: number): string {
  if (!sec || sec <= 0) return '—'
  const d = Math.floor(sec / 86400)
  const h = Math.floor((sec % 86400) / 3600)
  const m = Math.floor((sec % 3600) / 60)
  if (d > 0) return `${d} 天 ${h} 小时`
  if (h > 0) return `${h} 小时 ${m} 分`
  if (m > 0) return `${m} 分钟`
  return `${sec} 秒`
}

function fmtDate(ts: number): string {
  if (!ts) return '—'
  return new Date(ts * 1000).toLocaleString('zh-CN', { hour12: false })
}

const OP_LEVEL_HINT: Record<number, string> = {
  1: '可绕过出生点保护',
  2: '1 级 + 可用大部分单人指令与命令方块',
  3: '2 级 + 可管理玩家（封禁 / 踢出 / 授权）',
  4: '3 级 + 可管理服务器（stop / save 等），原版默认',
}

/**
 * 玩家头像地址。
 *
 * 用 Minecraft 官方的头像 CDN（`mc-heads.net` / `crafatar`），**不需要后端参与** ——
 * 头像是按玩家名或 UUID 直接取图的公开服务。
 *
 * 为什么按 UUID 优先：同名玩家的皮肤可能不同，而 UUID 唯一。
 * 名字兜底是因为有些玩家（尤其离线模式）在库里可能没有 UUID。
 *
 * ⚠️ 这是**外部网络请求**：玩家列表一屏可能有几十个人 → 几十个图片请求。
 * 因此：
 *   · 用浏览器原生 lazy loading（`Avatar` 里已加 `loading="lazy"`）；
 *   · 取不到图时 `Avatar` 会**自动回退成首字母头像**（已有逻辑），
 *     所以断网/离线部署下不会出现一堆裂图，只是看不到皮肤而已。
 */
function playerHeadUrl(p: { uuid?: string; name?: string }): string {
  if (p.uuid) return `https://mc-heads.net/avatar/${encodeURIComponent(p.uuid)}/64`
  if (p.name) return `https://mc-heads.net/avatar/${encodeURIComponent(p.name)}/64`
  return ''
}

export default function PlayersTab({ instanceId, canWrite }: { instanceId: string; canWrite: boolean }) {
  const [kind, setKind] = useState<Kind>('all')
  const [data, setData] = useState<PlayerList | null>(null)
  const [overview, setOverview] = useState<PlayerOverview | null>(null)
  const [opLevels, setOpLevels] = useState<Record<string, number>>({})
  const [filter, setFilter] = useState('')
  const [input, setInput] = useState('')
  const [error, setError] = useState('')
  const [msg, setMsg] = useState('')
  const [busy, setBusy] = useState(false)
  const [loading, setLoading] = useState(false)
  const [menuPlayer, setMenuPlayer] = useState<string | null>(null)
  const menuRef = useRef<HTMLDivElement>(null)

  const isOverview = kind === 'all' || kind === 'online'

  const load = async () => {
    setLoading(true)
    try {
      if (isOverview) {
        setOverview(await getPlayerOverview(instanceId))
        if (canWrite) {
          try {
            const ops = await listPlayers(instanceId, 'ops')
            const m: Record<string, number> = {}
            for (const e of ops.entries || []) if (e.name) m[e.name.toLowerCase()] = e.level ?? 4
            setOpLevels(m)
          } catch { setOpLevels({}) }
        }
      } else setData(await listPlayers(instanceId, kind))
      setError('')
    } catch (e: any) { setError(e.message) } finally { setLoading(false) }
  }

  useEffect(() => { load() }, [instanceId, kind])

  useEffect(() => {
    const close = (e: MouseEvent) => {
      if (menuRef.current && !menuRef.current.contains(e.target as Node)) setMenuPlayer(null)
    }
    const esc = (e: KeyboardEvent) => { if (e.key === 'Escape') setMenuPlayer(null) }
    document.addEventListener('mousedown', close)
    document.addEventListener('keydown', esc)
    return () => { document.removeEventListener('mousedown', close); document.removeEventListener('keydown', esc) }
  }, [])

  const run = async (fn: () => Promise<any>) => {
    setBusy(true); setError(''); setMsg('')
    try {
      const r = await fn(); setMsg(r?.message || '操作完成'); setMenuPlayer(null); setTimeout(load, 1200)
    } catch (e: any) { setError(e.message) } finally { setBusy(false) }
  }

  const submitAdd = (e: React.FormEvent) => {
    e.preventDefault(); const name = input.trim(); if (!name) return
    run(async () => { const r = await addPlayer(instanceId, kind, name); setInput(''); return r })
  }

  const current = KINDS.find((k) => k.key === kind)!
  const levelOf = (p: { name?: string; op: boolean }) => p.name ? (opLevels[p.name.toLowerCase()] ?? (p.op ? 4 : 0)) : 0
  const toggleWhitelist = (p: { name?: string; whitelisted: boolean }) => {
    if (!p.name) return
    run(() => p.whitelisted ? removePlayer(instanceId, 'whitelist', p.name!) : addPlayer(instanceId, 'whitelist', p.name!))
  }
  const toggleBan = (p: { name?: string; banned: boolean }) => {
    if (!p.name) return
    if (p.banned) { run(() => removePlayer(instanceId, 'bans', p.name!)); return }
    // 后端当前只接受名字并固定写入 forever；不发送前端自造的时长字段，避免操作看似成功却不生效。
    if (!confirm(`确定永久封禁「${p.name}」？当前服务器只支持永久封禁，封禁后他将无法进入服务器。`)) return
    run(() => addPlayer(instanceId, 'bans', p.name!))
  }
  const changeOpLevel = (p: { name?: string }, lv: number) => {
    if (!p.name) return
    run(() => lv === 0 ? removePlayer(instanceId, 'ops', p.name!) : setOpLevel(instanceId, p.name!, lv))
  }

  const shownPlayers = useMemo(() => {
    const list = (overview?.players || []).filter((p) => kind !== 'online' || p.online)
    const kw = filter.trim().toLowerCase()
    return kw ? list.filter((p) => (p.name || '').toLowerCase().includes(kw) || (p.uuid || '').toLowerCase().includes(kw)) : list
  }, [overview, filter, kind])

  const playerActions = (p: PlayerOverview['players'][number], named: boolean, lv: number) => (
    // ⚠️ 必须拦住冒泡：卡片的 onClick 是"切换选中"，如果点击弹框里的按钮/下拉
    // 冒泡到卡片，就会**把选中取消掉 → 弹框立刻消失**，用户根本点不到选项。
    // 这是用户实测报的 bug（"展开选项框后点击选项框会消失"）。
    <div
      className="player-popover"
      ref={menuPlayer === (p.uuid || p.name) ? menuRef : undefined}
      role="menu"
      onClick={(e) => e.stopPropagation()}
      onMouseDown={(e) => e.stopPropagation()}
    >
      <button disabled={busy || !named || !p.online} title={p.online ? '把该玩家踢出服务器' : '只有在线玩家才能踢出'} onClick={() => run(() => kickPlayer(instanceId, p.name!))}>踢出</button>
      <button disabled={busy || !named} onClick={() => toggleWhitelist(p)}>{p.whitelisted ? '移出白名单' : '加白名单'}</button>
      <button className={p.banned ? '' : 'danger'} disabled={busy || !named} onClick={() => toggleBan(p)}>{p.banned ? '解封' : '封禁'}</button>
      <label className="op-level" title="管理员（OP）等级：1 级最低，4 级可管理服务器">
        <span>OP</span>
        <select value={String(lv)} disabled={busy || !named} onChange={(e) => changeOpLevel(p, Number(e.target.value))}>
          <option value="0">无</option>
          {[1, 2, 3, 4].map((n) => <option key={n} value={String(n)} title={OP_LEVEL_HINT[n]}>{n} 级</option>)}
        </select>
      </label>
    </div>
  )

  return (
    <div className="players-tab">
      <div className="players-bar">
        <div className="connected-tabs" role="tablist" aria-label="玩家分类">
          {KINDS.map((k) => <button key={k.key} role="tab" aria-selected={kind === k.key} className={kind === k.key ? 'active' : ''} onClick={() => { setKind(k.key); setMsg(''); setError(''); setMenuPlayer(null) }}>
            {k.label}{(k.key === 'all' && overview && overview.total > 0) && <span className="tab-count">{overview.total}</span>}
            {k.key === 'online' && overview && overview.online > 0 && <span className="tab-count">{overview.online}</span>}
          </button>)}
        </div>
        <button onClick={load} disabled={loading}>{loading ? '加载中…' : '刷新'}</button>
      </div>
      <p className="players-hint">{current.hint}</p>

      {isOverview ? <>
        {/* 统计条：**连通的长条形**（用户 2026-10-06 要求）。
            每项的排布按用户指定：
              · 「当前在线」排在「历史玩家」**前面**
              · 标注（label）在数字**上方**，且用正常黑体（不是小字灰色）
              · 每格内容**居中**
              · 各项**均匀分布**（flex:1，不再让最后一格吃掉剩余宽度） */}
        {kind === 'all' && overview && <div className="psum-bar">
          <div className={`psum-item ${overview.online > 0 ? 'on' : ''}`}>
            <span className="psum-label">当前在线</span>
            <span className="psum-num">{overview.online}</span>
          </div>
          <div className="psum-item">
            <span className="psum-label">历史玩家</span>
            <span className="psum-num">{overview.total}</span>
          </div>
          <div className="psum-item">
            <span className="psum-label">OP</span>
            <span className="psum-num">{overview.players.filter((p) => p.op).length}</span>
          </div>
          <div className="psum-item">
            <span className="psum-label">已封禁</span>
            <span className="psum-num">{overview.players.filter((p) => p.banned).length}</span>
          </div>
          <div className="psum-item">
            <span className="psum-label">白名单</span>
            <span className={`psum-num ${overview.whitelist_enabled ? 'on' : ''}`}>
              {overview.whitelist_enabled ? '开' : '关'}
            </span>
          </div>
          <div className="psum-item wide">
            <span className="psum-label">世界目录</span>
            <span className="psum-num mono">{overview.world_name}</span>
          </div>
        </div>}
        <div className="players-filter"><input placeholder="按玩家名或 UUID 过滤…" value={filter} onChange={(e) => setFilter(e.target.value)} />{filter && <button onClick={() => setFilter('')}>清除</button>}<span className="muted">{filter ? `匹配 ${shownPlayers.length} 人` : kind === 'online' ? '当前在线' : '按游戏时长排序'}</span></div>
        {error && <div className="error-banner">{error}</div>}
        <div className="player-grid">
          {shownPlayers.map((p) => {
            const named = !!p.name; const id = p.uuid || p.name || ''; const lv = levelOf(p)
            return <div key={id} className={`player-card ${p.online ? 'online' : ''} ${menuPlayer === id ? 'selected' : ''}`} onClick={() => canWrite && setMenuPlayer(menuPlayer === id ? null : id)} onKeyDown={(e) => { if (canWrite && (e.key === 'Enter' || e.key === ' ')) { e.preventDefault(); setMenuPlayer(menuPlayer === id ? null : id) } }} tabIndex={canWrite ? 0 : -1} role={canWrite ? 'button' : undefined}>
              {/* 版面（2026-10-06 重做，用户反馈"比例奇怪、卡片过高、元素排布要重设"）：
                  原来是一列堆叠（头像行 → 标签行 → 底部行），信息少但卡片很高。
                  现在改成**两行紧凑布局**：
                    左：皮肤头像（点击目标够大）
                    右上：名字 + 状态标签（同行，横向利用宽度）
                    右下：时长 · 最后记录（弱化的一行）
                  UUID 只在悬停时用 title 显示 —— 它很长，常驻会把卡片撑宽。 */}
              <Avatar
                url={playerHeadUrl(p)}
                name={p.name || '?'}
                size={36}
                radius="var(--r)"
                initialsLength={1}
                className="pc-av"
              />
              <div className="pc-main">
                <div className="pc-top">
                  <strong className="pc-name" title={p.name || ''}>{p.name || '（未知名字）'}</strong>
                  <span className="pc-tags">
                    <span className={`tag ${p.online ? 'on' : 'off'}`}>{p.online ? '在线' : '离线'}</span>
                    {p.banned && <span className="tag ban">封禁</span>}
                    {p.op && <span className="tag op">OP{lv ? ` ${lv}` : ''}</span>}
                    {p.whitelisted && <span className="tag wl">白名单</span>}
                    {!p.has_data && <span className="tag muted-tag">无存档</span>}
                  </span>
                </div>
                <div className="pc-sub" title={p.uuid || undefined}>
                  <span>{fmtDuration(p.play_seconds)}</span>
                  <span className="pc-dot">·</span>
                  <span>{p.last_seen ? fmtDate(p.last_seen) : '无记录'}</span>
                </div>
              </div>
              {menuPlayer === id && canWrite && playerActions(p, named, lv)}
            </div>
          })}
          {shownPlayers.length === 0 && <div className="empty player-grid-empty">{loading ? '加载中…' : kind === 'online' ? '当前没有在线玩家' : overview && overview.total === 0 ? '暂无玩家记录（服务器启动并有人进入后才会产生）' : '没有匹配的玩家'}</div>}
        </div>
        {canWrite && kind === 'all' && <div className="players-oplevel-note"><strong>OP 等级</strong>（1 级最低，4 级可管理服务器）：{Object.entries(OP_LEVEL_HINT).map(([k, v]) => <span key={k} className="op-hint-item"><b>{k} 级</b> {v}</span>)}改完等级会写入 <span className="mono">ops.json</span>；运行中的服务器会自动读取名单文件的变化，若游戏内仍是旧等级，执行 <span className="mono">/reload</span> 或重启实例即可。</div>}
        <div className="players-note">说明：<strong>游戏时长</strong>取自世界统计文件（<span className="mono">world/stats/&lt;uuid&gt;.json</span>），<strong>在线状态</strong>由控制台日志的加入 / 退出事件推断。未产生统计数据的玩家会显示「无存档」。</div>
      </> : <>
        {kind === 'whitelist' && data && <div className="whitelist-switch"><label className="switch"><input type="checkbox" checked={data.whitelist_enabled} disabled={!canWrite || busy} onChange={(e) => run(() => setWhitelist(instanceId, e.target.checked))} />启用白名单</label><span className="muted">{data.whitelist_enabled ? '已启用：只有名单内玩家能进入' : '已关闭：任何玩家都能进入'}</span></div>}
        {canWrite && <form className="players-add" onSubmit={submitAdd}><input placeholder={kind === 'ipbans' ? 'IP 地址（如 1.2.3.4）' : '玩家名（区分大小写不敏感）'} value={input} onChange={(e) => setInput(e.target.value)} />{kind === 'bans' && <select className="ban-duration" value="forever" disabled title="后端当前只支持永久封禁"><option value="forever">永久（服务器当前仅支持永久）</option></select>}<button className="primary" type="submit" disabled={busy || !input.trim()}>{kind === 'bans' || kind === 'ipbans' ? '封禁' : '添加'}</button></form>}
        {error && <div className="error-banner">{error}</div>}{msg && <div className="success-banner">{msg}</div>}
        <table className="players-table"><thead><tr><th>{kind === 'ipbans' ? 'IP' : '玩家'}</th>{kind === 'ops' && <th>等级</th>}{kind === 'bans' && <><th>原因</th><th>操作者</th><th>到期</th></>}{kind === 'ipbans' && <><th>原因</th><th>到期</th></>}{kind !== 'ipbans' && <th>UUID</th>}{canWrite && <th>操作</th>}</tr></thead><tbody>
          {(data?.entries || []).map((p, i) => <tr key={p.uuid || p.ip || i}><td><strong>{p.name || p.ip}</strong></td>{kind === 'ops' && <td>{p.level ?? 4}</td>}{kind === 'bans' && <><td>{p.reason}</td><td>{p.source}</td><td>{p.expires}</td></>}{kind === 'ipbans' && <><td>{p.reason}</td><td>{p.expires}</td></>}{kind !== 'ipbans' && <td className="mono uuid">{p.uuid}</td>}{canWrite && <td><button className="danger" disabled={busy} onClick={() => run(() => removePlayer(instanceId, kind, p.name || p.ip || ''))}>{kind === 'bans' || kind === 'ipbans' ? '解除' : '移除'}</button></td>}</tr>)}
          {data && data.entries.length === 0 && <tr><td colSpan={6} className="empty">名单为空{!data.exists && '（文件尚未生成，服务器首次启动后会创建）'}</td></tr>}{!data && !loading && <tr><td colSpan={6} className="empty">暂无数据</td></tr>}
        </tbody></table>
        <div className="players-note">说明：增删操作优先通过<strong>控制台命令</strong>执行，由服务器自行解析玩家名到 UUID；实例停止或接管时改为直接修改名单文件。{data?.exists === false && ' 名单文件尚未生成，请先启动一次服务器。'}</div>
      </>}
    </div>
  )
}
