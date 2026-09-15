import { useEffect, useState } from 'react'
import {
  UserInfo, MyNode, PortGrant,
  listUsers, createUser, deleteUser, changePassword, roleLabel, isNodeUser, listNodes,
  listNodeUsers, grantNodeUser, revokeNodeUser, NodeUserGrant,
  listFrps, listPortGrants, setPortGrant, deletePortGrant, FrpsServer,
  setUserRole, currentUser,
} from '../api'
import './UsersPage.css'

/**
 * UsersPage 用户管理（仅总管理员）。
 *
 * 承载两件事：
 *   1. 账号本身（创建 / 重置密码 / 删除）
 *   2. 两项资源分配 —— **管哪些节点**（只对节点用户有意义）、
 *      **在每条线路上有几个穿透端口**（节点用户和普通用户都可以有）
 *
 * 为什么把"角色"和这两种分配放在同一页：它们是同一件事的三面。
 * 只改角色却忘了分配节点，用户点不动任何按钮；只分节点却忘了给端口，
 * 他建完实例发现开不了公网口 —— 分到两个页面去设，就一定会有人只设一半。
 *
 * 端口配额为什么对普通用户也开放：普通用户可以是某些实例的 owner
 * （管理员建好实例后把 owner 授权给他），他要给自己的实例加多端口模组的端口。
 * 配额是按**线路**发的，与节点无关，所以它和「管哪些节点」是正交的两件事。
 */
export default function UsersPage() {
  const [users, setUsers] = useState<UserInfo[]>([])
  const [grants, setGrants] = useState<NodeUserGrant[]>([])
  const [portGrants, setPortGrants] = useState<PortGrant[]>([])
  const [nodes, setNodes] = useState<MyNode[]>([])
  const [lines, setLines] = useState<FrpsServer[]>([])
  const [error, setError] = useState('')
  const [msg, setMsg] = useState('')
  const [busy, setBusy] = useState(false)

  // 新建用户
  const [newUser, setNewUser] = useState('')
  const [newPw, setNewPw] = useState('')
  const [newRole, setNewRole] = useState('user')

  // 节点授权
  const [grantUser, setGrantUser] = useState('')
  const [grantNode, setGrantNode] = useState('')

  // 端口配额
  const [quotaUser, setQuotaUser] = useState('')
  const [quotaLine, setQuotaLine] = useState('')
  const [quotaValue, setQuotaValue] = useState('1')

  const load = async () => {
    try {
      const [u, g, p] = await Promise.all([
        listUsers(),
        listNodeUsers().catch(() => []),
        listPortGrants().catch(() => []),
      ])
      setUsers(Array.isArray(u) ? u : [])
      setGrants(Array.isArray(g) ? g : [])
      setPortGrants(Array.isArray(p) ? p : [])
      setError('')
    } catch (e: any) {
      setError(e.message)
    }
  }

  useEffect(() => {
    load()
    listNodes().then((n) => {
      const arr = Array.isArray(n) ? n : []
      setNodes(arr)
      if (arr.length > 0) setGrantNode((cur) => cur || String(arr[0].id))
    }).catch(() => { /* 节点列表拿不到不影响账号管理 */ })
    // frps 线路列表仅管理员可读，失败就当作"还没有线路"
    listFrps().then((f) => {
      const arr = Array.isArray(f) ? f : []
      setLines(arr)
      if (arr.length > 0) setQuotaLine((cur) => cur || String(arr[0].id))
    }).catch(() => setLines([]))
  }, [])

  const run = async (fn: () => Promise<any>) => {
    setBusy(true); setError(''); setMsg('')
    try {
      const r = await fn()
      setMsg(r?.message || '操作完成')
      await load()
    } catch (e: any) {
      setError(e.message)
    } finally {
      setBusy(false)
    }
  }

  const submitCreate = (e: React.FormEvent) => {
    e.preventDefault()
    if (!newUser.trim() || newPw.length < 6) {
      setError('用户名不能为空，密码至少 6 位')
      return
    }
    run(async () => {
      const r = await createUser(newUser.trim(), newPw, newRole)
      setNewUser(''); setNewPw(''); setNewRole('user')
      return r
    })
  }

  const removeUser = (id: number, username: string) => {
    if (!confirm(`确定删除账号「${username}」？该账号的实例授权、节点范围与端口配额会一并移除。`)) return
    run(() => deleteUser(id))
  }

  const resetPw = (username: string) => {
    const pw = prompt(`为「${username}」设置新密码（至少 6 位）：`)
    if (!pw) return
    if (pw.length < 6) { setError('密码至少 6 位'); return }
    run(() => changePassword('', pw, username))
  }

  // 改账号类型。
  //
  // 两个提示都不能省：
  //   - 升/降**总管理员**要二次确认（权限最大的一档，误点代价最大）
  //   - 后端返回的消息里带着"重新登录后生效"，直接展示 —— 角色写在令牌里，
  //     改完对方最长 24 小时仍按旧角色判定，界面不能装作立刻生效
  const changeRole = (u: UserInfo, role: string) => {
    const cur = isNodeUser(u.role) ? 'nodeuser' : u.role
    if (role === cur) return
    if (role === 'admin' && !confirm(`确定把「${u.username}」提升为总管理员？他将拥有所有节点的全部权限。`)) return
    if (cur === 'admin' && !confirm(`确定把「${u.username}」从总管理员降级？`)) return
    run(() => setUserRole(u.id, role))
  }

  const doGrantNode = (e: React.FormEvent) => {
    e.preventDefault()
    if (!grantUser.trim() || !grantNode) { setError('请选择用户与节点'); return }
    run(() => grantNodeUser(grantUser.trim(), Number(grantNode)))
  }

  const doSetQuota = (e: React.FormEvent) => {
    e.preventDefault()
    if (!quotaUser.trim() || !quotaLine) { setError('请选择用户与线路'); return }
    const n = Number(quotaValue)
    if (!Number.isInteger(n) || n < 0) { setError('端口数量需为 0 或正整数'); return }
    run(() => setPortGrant(quotaUser.trim(), Number(quotaLine), n))
  }

  const nodesOf = (username: string) => grants.filter((g) => g.username === username)
  const portsOf = (username: string) => portGrants.filter((g) => g.username === username)

  return (
    <div className="users-page">
      {error && <div className="error-banner">{error}</div>}
      {msg && <div className="success-banner">{msg}</div>}

      {/* ---------------- 新建账号 ---------------- */}
      <div className="up-card">
        <div className="up-card-title">新建账号</div>
        <form className="up-create" onSubmit={submitCreate}>
          <label className="up-field">
            <span>用户名</span>
            <input value={newUser} onChange={(e) => setNewUser(e.target.value)} placeholder="例如 alice" spellCheck={false} />
          </label>
          <label className="up-field">
            <span>密码</span>
            <input type="password" value={newPw} onChange={(e) => setNewPw(e.target.value)} placeholder="至少 6 位" />
          </label>
          <label className="up-field">
            <span>角色</span>
            <select value={newRole} onChange={(e) => setNewRole(e.target.value)}>
              <option value="user">普通用户</option>
              <option value="nodeuser">节点用户</option>
              <option value="admin">总管理员</option>
            </select>
            <div className="up-hint">
              {newRole === 'user' && '只能操作被显式授权的实例。'}
              {newRole === 'nodeuser' && '普通用户权限 + 在指定节点上创建/删除实例，并使用下面分配的穿透端口。两项分配都要在下方设置。'}
              {newRole === 'admin' && '全部权限，包括所有节点与所有实例。请谨慎授予。'}
            </div>
          </label>
          <button className="primary" type="submit" disabled={busy}>创建</button>
        </form>
      </div>

      {/* ---------------- 节点用户：管哪些节点 ---------------- */}
      <div className="up-card">
        <div className="up-card-title">
          节点范围
          <span className="up-card-sub">指定节点用户能在哪些节点上创建 / 删除实例、设置到期时间</span>
        </div>

        <form className="up-create" onSubmit={doGrantNode}>
          <label className="up-field">
            <span>用户</span>
            <input value={grantUser} onChange={(e) => setGrantUser(e.target.value)} placeholder="用户名" list="up-usernames" spellCheck={false} />
            <datalist id="up-usernames">
              {users.map((u) => <option key={u.id} value={u.username} />)}
            </datalist>
          </label>
          <label className="up-field">
            <span>节点</span>
            <select value={grantNode} onChange={(e) => setGrantNode(e.target.value)}>
              {nodes.length === 0 && <option value="">（暂无节点）</option>}
              {nodes.map((n) => <option key={n.id} value={n.id}>{n.name}（{n.ip}）</option>)}
            </select>
          </label>
          <button className="primary" type="submit" disabled={busy || nodes.length === 0}>授权</button>
        </form>

        <div className="up-hint" style={{ marginTop: 8 }}>
          授权时会自动把该用户的角色提升为「节点用户」；收回最后一个节点时角色会降回普通用户 ——
          留着节点用户角色却一个节点都不管，界面上会有一堆点不动的入口。
          <br />
          只想改角色（不必分配节点）就用用户表里的「角色」下拉。注意角色写在登录令牌里，
          <strong>改完要对方重新登录才生效</strong>；降级不会清掉他已分配的节点与端口配额，
          将来升回来配置还在。
        </div>
      </div>

      {/* ---------------- 穿透端口配额（节点用户与普通用户都可以有） ---------------- */}
      <div className="up-card">
        <div className="up-card-title">
          穿透端口配额
          <span className="up-card-sub">按线路分配：该用户能在每条线路上开几个公网端口（普通用户也可以分配）</span>
        </div>

        {lines.length === 0 ? (
          <div className="up-hint">
            还没有任何线路（frps 服务器）。请先到「穿透管理」添加一条线路，再回来分配端口。
          </div>
        ) : (
          <>
            <form className="up-create" onSubmit={doSetQuota}>
              <label className="up-field">
                <span>用户</span>
                <input value={quotaUser} onChange={(e) => setQuotaUser(e.target.value)} placeholder="用户名" list="up-usernames" spellCheck={false} />
              </label>
              <label className="up-field">
                <span>线路</span>
                <select value={quotaLine} onChange={(e) => setQuotaLine(e.target.value)}>
                  {lines.map((f) => <option key={f.id} value={f.id}>{f.name}（{f.host}）</option>)}
                </select>
              </label>
              <label className="up-field narrow">
                <span>端口数量</span>
                <input value={quotaValue} onChange={(e) => setQuotaValue(e.target.value)} inputMode="numeric" />
              </label>
              <button className="primary" type="submit" disabled={busy}>保存配额</button>
            </form>

            <div className="up-hint" style={{ marginTop: 8 }}>
              配额记在<b>实例归属者</b>头上：谁是实例的 owner，开端口就扣谁的配额
              （管理员替别人建的实例，扣的是被授权为 owner 的那个人）。
              用量是<b>从隧道实时推导</b>的，不是单独记的账本 ——
              删除实例或关闭端口会自动释放配额，不会出现"账本说用了 3 个、实际一条都没有"的对不上。
              配额可以调小到低于已用量，此时会提示已超用，由你决定要不要关掉多余的端口。
              普通用户只对这些实例生效（他们不能自己建实例）。
            </div>

            {portGrants.length > 0 && (
              <table className="up-table">
                <thead>
                  <tr><th>用户</th><th>线路</th><th>配额</th><th>已用</th><th>剩余</th><th>操作</th></tr>
                </thead>
                <tbody>
                  {portGrants.map((g) => (
                    <tr key={`${g.user_id}-${g.frps_id}`}>
                      <td><strong>{g.username}</strong></td>
                      <td>{g.frps_name}<div className="up-sub mono">{g.frps_host}</div></td>
                      <td>{g.quota}</td>
                      <td className={g.used > g.quota ? 'up-over' : ''}>{g.used}</td>
                      <td>{g.available}</td>
                      <td>
                        <button
                          className="danger"
                          disabled={busy}
                          onClick={() => run(() => deletePortGrant(g.user_id, g.frps_id))}
                        >
                          移除
                        </button>
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            )}
          </>
        )}
      </div>

      {/* ---------------- 账号列表 ---------------- */}
      <div className="up-card">
        <div className="up-card-title">账号列表</div>
        <table className="up-table">
          <thead>
            <tr><th>用户</th><th>可管理节点</th><th>端口配额</th><th>操作</th></tr>
          </thead>
          <tbody>
            {users.map((u) => {
              const mine = nodesOf(u.username)
              const ports = portsOf(u.username)
              return (
                <tr key={u.id}>
                  <td>
                    <strong>{u.username}</strong>
                    <span className={`up-role role-${isNodeUser(u.role) ? 'nodeuser' : u.role}`}>{roleLabel(u.role)}</span>
                    <div className="up-sub">UID {u.id} · {u.status}</div>
                  </td>
                  <td>
                    {u.role === 'admin' ? (
                      <span className="up-all">全部节点（总管理员）</span>
                    ) : mine.length === 0 ? (
                      <span className="muted">未分配</span>
                    ) : (
                      <div className="up-tags">
                        {mine.map((g) => (
                          <span className="up-node-tag" key={g.node_id}>
                            {g.node_name}
                            <button
                              className="up-tag-x"
                              title="收回该节点的管理权限"
                              disabled={busy}
                              onClick={() => run(() => revokeNodeUser(g.user_id, g.node_id))}
                            >
                              ✕
                            </button>
                          </span>
                        ))}
                      </div>
                    )}
                  </td>
                  <td>
                    {u.role === 'admin' ? (
                      <span className="up-all">不限</span>
                    ) : ports.length === 0 ? (
                      <span className="muted">未分配</span>
                    ) : (
                      <div className="up-tags">
                        {ports.map((p) => (
                          <span
                            className={`up-port-tag ${p.used > p.quota ? 'over' : ''}`}
                            key={p.frps_id}
                            title={`${p.frps_name}：配额 ${p.quota}，已用 ${p.used}`}
                          >
                            {p.frps_name} {p.used}/{p.quota}
                          </span>
                        ))}
                      </div>
                    )}
                  </td>
                  <td>
                    <div className="up-actions">
                      {/* 改账号类型：建号时选错、或事后要升/降，都不必删号重建
                          （删号会连带清掉实例授权与端口配额） */}
                      <label className="up-role-edit">
                        <span>角色</span>
                        <select
                          value={isNodeUser(u.role) ? 'nodeuser' : u.role}
                          disabled={busy || u.username === currentUser()?.username}
                          title={u.username === currentUser()?.username
                            ? '不能修改自己的角色'
                            : '改完该用户重新登录后生效'}
                          onChange={(e) => changeRole(u, e.target.value)}
                        >
                          <option value="user">普通用户</option>
                          <option value="nodeuser">节点用户</option>
                          <option value="admin">总管理员</option>
                        </select>
                      </label>
                      <button onClick={() => resetPw(u.username)} disabled={busy}>重置密码</button>
                      {u.role !== 'admin' && (
                        <button className="danger" onClick={() => removeUser(u.id, u.username)} disabled={busy}>
                          删除
                        </button>
                      )}
                    </div>
                  </td>
                </tr>
              )
            })}
            {users.length === 0 && (
              <tr><td colSpan={4} className="empty">暂无账号</td></tr>
            )}
          </tbody>
        </table>
      </div>
    </div>
  )
}
