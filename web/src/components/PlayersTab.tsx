import { useEffect, useMemo, useState } from 'react'
import {
  PlayerList, PlayerOverview, listPlayers, addPlayer, removePlayer, setWhitelist,
  getPlayerOverview,
} from '../api'
import './PlayersTab.css'

type Kind = 'all' | 'whitelist' | 'ops' | 'bans' | 'ipbans'

const KINDS: { key: Kind; label: string; hint: string }[] = [
  { key: 'all', label: '全部玩家信息', hint: '服务器见过的所有玩家：累计游戏时长、当前是否在线、白名单 / OP / 封禁状态。数据来自 usercache、世界统计数据与控制台日志。' },
  { key: 'whitelist', label: '白名单', hint: '仅名单内玩家可进入服务器' },
  { key: 'ops', label: '管理员', hint: '拥有 OP 权限的玩家' },
  { key: 'bans', label: '封禁玩家', hint: '被禁止进入的玩家' },
  { key: 'ipbans', label: '封禁 IP', hint: '被禁止的 IP 地址' },
]

/** 把秒数格式化成「12 天 3 小时」这类人话。 */
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

export default function PlayersTab({ instanceId, canWrite }: { instanceId: string; canWrite: boolean }) {
  const [kind, setKind] = useState<Kind>('all')
  const [data, setData] = useState<PlayerList | null>(null)
  const [overview, setOverview] = useState<PlayerOverview | null>(null)
  const [filter, setFilter] = useState('')
  const [input, setInput] = useState('')
  const [error, setError] = useState('')
  const [msg, setMsg] = useState('')
  const [busy, setBusy] = useState(false)
  const [loading, setLoading] = useState(false)

  const isOverview = kind === 'all'

  const load = async () => {
    setLoading(true)
    try {
      if (isOverview) {
        setOverview(await getPlayerOverview(instanceId))
      } else {
        setData(await listPlayers(instanceId, kind))
      }
      setError('')
    } catch (e: any) {
      setError(e.message)
    } finally {
      setLoading(false)
    }
  }

  useEffect(() => { load() }, [instanceId, kind])

  const run = async (fn: () => Promise<any>) => {
    setBusy(true); setError(''); setMsg('')
    try {
      const r = await fn()
      setMsg(r?.message || '操作完成')
      // 通过控制台执行时服务器写文件有延迟，稍等再刷新以免看到旧数据
      setTimeout(load, 1200)
    } catch (e: any) {
      setError(e.message)
    } finally {
      setBusy(false)
    }
  }

  const submitAdd = (e: React.FormEvent) => {
    e.preventDefault()
    const name = input.trim()
    if (!name) return
    run(async () => {
      const r = await addPlayer(instanceId, kind, name)
      setInput('')
      return r
    })
  }

  const current = KINDS.find((k) => k.key === kind)!

  // 总览列表的本地过滤：数据已经一次取全，再为搜索往返一次接口没有意义
  const shownPlayers = useMemo(() => {
    const list = overview?.players || []
    const kw = filter.trim().toLowerCase()
    if (!kw) return list
    return list.filter((p) =>
      (p.name || '').toLowerCase().includes(kw) || (p.uuid || '').toLowerCase().includes(kw))
  }, [overview, filter])

  return (
    <div className="players-tab">
      <div className="players-bar">
        <div className="kind-tabs">
          {KINDS.map((k) => (
            <button
              key={k.key}
              className={kind === k.key ? 'active' : ''}
              onClick={() => { setKind(k.key); setMsg(''); setError('') }}
            >
              {k.label}
              {k.key === 'all' && overview && overview.total > 0 && (
                <span className="tab-count">{overview.total}</span>
              )}
            </button>
          ))}
        </div>
        <button onClick={load} disabled={loading}>{loading ? '加载中…' : '刷新'}</button>
      </div>

      <p className="players-hint">{current.hint}</p>

      {isOverview ? (
        <>
          {overview && (
            <div className="players-summary">
              <div className="psum">
                <span className="psum-num">{overview.total}</span>
                <span className="psum-label">历史玩家</span>
              </div>
              <div className={`psum ${overview.online > 0 ? 'on' : ''}`}>
                <span className="psum-num">{overview.online}</span>
                <span className="psum-label">当前在线</span>
              </div>
              <div className="psum">
                <span className="psum-num">{overview.players.filter((p) => p.op).length}</span>
                <span className="psum-label">OP</span>
              </div>
              <div className="psum">
                <span className="psum-num">{overview.players.filter((p) => p.banned).length}</span>
                <span className="psum-label">已封禁</span>
              </div>
              <div className="psum">
                <span className={`psum-num ${overview.whitelist_enabled ? 'on' : ''}`}>
                  {overview.whitelist_enabled ? '开' : '关'}
                </span>
                <span className="psum-label">白名单</span>
              </div>
              <div className="psum wide">
                <span className="psum-num mono">{overview.world_name}</span>
                <span className="psum-label">世界目录</span>
              </div>
            </div>
          )}

          <div className="players-filter">
            <input
              placeholder="按玩家名或 UUID 过滤…"
              value={filter}
              onChange={(e) => setFilter(e.target.value)}
            />
            {filter && <button onClick={() => setFilter('')}>清除</button>}
            <span className="muted">
              {filter ? `匹配 ${shownPlayers.length} / ${overview?.total ?? 0} 人` : '按游戏时长排序'}
            </span>
          </div>

          {error && <div className="error-banner">{error}</div>}

          <table className="players-table">
            <thead>
              <tr>
                <th>玩家</th>
                <th>状态</th>
                <th>游戏时长</th>
                <th>身份</th>
                <th>最后记录</th>
                <th>UUID</th>
              </tr>
            </thead>
            <tbody>
              {shownPlayers.map((p) => (
                <tr key={(p.uuid || '') + p.name} className={p.online ? 'row-online' : ''}>
                  <td>
                    <strong>{p.name || '（未知名字）'}</strong>
                    {!p.has_data && <span className="tag muted-tag" title="名单里有记录，但世界目录中没有该玩家的存档">无存档</span>}
                  </td>
                  <td>
                    <span className={`status ${p.online ? 'status-running' : 'status-stopped'}`}>
                      {p.online ? '在线' : '离线'}
                    </span>
                  </td>
                  <td>{fmtDuration(p.play_seconds)}</td>
                  <td>
                    <div className="tag-list">
                      {p.op && <span className="tag op">OP</span>}
                      {p.whitelisted && <span className="tag wl">白名单</span>}
                      {p.banned && <span className="tag ban">已封禁</span>}
                      {!p.op && !p.whitelisted && !p.banned && <span className="muted">普通玩家</span>}
                    </div>
                  </td>
                  <td>{fmtDate(p.last_seen)}</td>
                  <td className="mono uuid">{p.uuid || '—'}</td>
                </tr>
              ))}
              {shownPlayers.length === 0 && (
                <tr>
                  <td colSpan={6} className="empty">
                    {loading
                      ? '加载中…'
                      : overview && overview.total === 0
                        ? '暂无玩家记录（服务器启动并有人进入后才会产生）'
                        : '没有匹配的玩家'}
                  </td>
                </tr>
              )}
            </tbody>
          </table>

          <div className="players-note">
            说明：<strong>游戏时长</strong>取自世界统计文件（<span className="mono">world/stats/&lt;uuid&gt;.json</span>），
            是服务器自己累计的权威数据；<strong>在线状态</strong>由控制台日志的加入 / 退出事件推断，
            与游戏内 <span className="mono">list</span> 指令的结果一致。
            未产生统计数据的玩家（只连上但没进世界）会显示「无存档」。
          </div>
        </>
      ) : (
        <>
          {kind === 'whitelist' && data && (
            <div className="whitelist-switch">
              <label className="switch">
                <input
                  type="checkbox"
                  checked={data.whitelist_enabled}
                  disabled={!canWrite || busy}
                  onChange={(e) => run(() => setWhitelist(instanceId, e.target.checked))}
                />
                启用白名单
              </label>
              <span className="muted">
                {data.whitelist_enabled ? '已启用：只有名单内玩家能进入' : '已关闭：任何玩家都能进入'}
              </span>
            </div>
          )}

          {canWrite && (
            <form className="players-add" onSubmit={submitAdd}>
              <input
                placeholder={kind === 'ipbans' ? 'IP 地址（如 1.2.3.4）' : '玩家名（区分大小写不敏感）'}
                value={input}
                onChange={(e) => setInput(e.target.value)}
              />
              <button className="primary" type="submit" disabled={busy || !input.trim()}>
                {kind === 'bans' || kind === 'ipbans' ? '封禁' : '添加'}
              </button>
            </form>
          )}

          {error && <div className="error-banner">{error}</div>}
          {msg && <div className="success-banner">{msg}</div>}

          <table className="players-table">
            <thead>
              <tr>
                <th>{kind === 'ipbans' ? 'IP' : '玩家'}</th>
                {kind === 'ops' && <th>等级</th>}
                {kind === 'bans' && <><th>原因</th><th>操作者</th><th>到期</th></>}
                {kind === 'ipbans' && <><th>原因</th><th>到期</th></>}
                {kind !== 'ipbans' && <th>UUID</th>}
                {canWrite && <th>操作</th>}
              </tr>
            </thead>
            <tbody>
              {(data?.entries || []).map((p, i) => (
                <tr key={p.uuid || p.ip || i}>
                  <td><strong>{p.name || p.ip}</strong></td>
                  {kind === 'ops' && <td>{p.level ?? 4}</td>}
                  {kind === 'bans' && <><td>{p.reason}</td><td>{p.source}</td><td>{p.expires}</td></>}
                  {kind === 'ipbans' && <><td>{p.reason}</td><td>{p.expires}</td></>}
                  {kind !== 'ipbans' && <td className="mono uuid">{p.uuid}</td>}
                  {canWrite && (
                    <td>
                      <button
                        className="danger"
                        disabled={busy}
                        onClick={() => run(() => removePlayer(instanceId, kind, p.name || p.ip || ''))}
                      >
                        {kind === 'bans' || kind === 'ipbans' ? '解除' : '移除'}
                      </button>
                    </td>
                  )}
                </tr>
              ))}
              {data && data.entries.length === 0 && (
                <tr>
                  <td colSpan={6} className="empty">
                    名单为空{!data.exists && '（文件尚未生成，服务器首次启动后会创建）'}
                  </td>
                </tr>
              )}
              {!data && !loading && <tr><td colSpan={6} className="empty">暂无数据</td></tr>}
            </tbody>
          </table>

          <div className="players-note">
            说明：增删操作优先通过<strong>控制台命令</strong>执行 —— 由服务器自行解析玩家名到 UUID，
            因此在线模式（正版）与离线模式都能正确处理。
            若实例处于停止或接管状态，则改为直接修改名单文件（按离线模式推导 UUID）。
            {data?.exists === false && ' 名单文件尚未生成，请先启动一次服务器。'}
          </div>
        </>
      )}
    </div>
  )
}
