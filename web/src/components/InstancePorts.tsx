import { useEffect, useState } from 'react'
import {
  InstancePort, MyPortLine,
  listInstancePorts, addInstancePort, updateInstancePort, deleteInstancePort,
} from '../api'
import './InstancePorts.css'
import { copyText } from '../clipboard'

/**
 * InstancePorts 实例的公网端口。
 *
 * 面向**所有能看到该实例的人**：多端口模组（BlueMap 网页、Geyser 基岩版、
 * Votifier 等）配好后，用户得把地址填进配置或告诉朋友。
 * 此前这些信息只存在于管理员的「穿透管理」页，用户只能去问管理员。
 *
 * 增 / 改 / 删需要：总管理员、该节点的节点用户，**或本实例的 owner**。
 * owner 是刻意放开的 —— 开哪些端口是"实例自己的事"，
 * 多端口模组本来就该由使用者自己配；而删除实例、改到期仍走更严的节点级权限。
 * collab（协作者）不在内：开端口等于扩大对外暴露面，不该由只被授权启停的人决定。
 *
 * 配额记在**实例归属者**头上（不是操作者），所以表单用的可用线路
 * 直接取自后端返回的 lines，而不是 /api/my/ports。
 */
export default function InstancePorts({
  instanceId, canEdit,
}: {
  instanceId: string
  canEdit: boolean
}) {
  const [ports, setPorts] = useState<InstancePort[]>([])
  const [remaining, setRemaining] = useState(-1)
  const [lines, setLines] = useState<MyPortLine[]>([])
  const [chargingSelf, setChargingSelf] = useState(true)
  const [error, setError] = useState('')
  const [msg, setMsg] = useState('')
  const [busy, setBusy] = useState(false)
  const [copied, setCopied] = useState('')

  // 新增端口
  const [adding, setAdding] = useState(false)
  const [newLine, setNewLine] = useState('')
  const [newLocal, setNewLocal] = useState('')
  // 第三方线路要绑定的隧道（远端端口由服务商分配，是固定值）
  const [newTunnel, setNewTunnel] = useState('')
  const [newProto, setNewProto] = useState('tcp')

  // 就地编辑本地端口
  const [editTunnel, setEditTunnel] = useState<string | null>(null)
  const [editLocal, setEditLocal] = useState('')

  const load = async () => {
    try {
      const r = await listInstancePorts(instanceId)
      setPorts(Array.isArray(r.ports) ? r.ports : [])
      setRemaining(typeof r.remaining === 'number' ? r.remaining : -1)
      // 可用线路由后端按**本实例配额归属者**算好，
      // 与真正开通时扣谁的口径一致 —— 别改用 listMyPorts()
      const ls: MyPortLine[] = Array.isArray(r.lines) ? r.lines : []
      setLines(ls)
      setChargingSelf(r.charging_self !== false)
      setNewLine((cur) => cur || (ls.length > 0 ? String(ls[0].frps_id) : ''))
      setError('')
    } catch (e: any) {
      setError(e.message)
    }
  }

  useEffect(() => {
    load()
  }, [instanceId, canEdit])

  const run = async (fn: () => Promise<any>) => {
    setBusy(true); setError(''); setMsg('')
    try {
      const r = await fn()
      setMsg(r?.message || '操作完成')
      await load()
      return true
    } catch (e: any) {
      setError(e.message)
      return false
    } finally {
      setBusy(false)
    }
  }

  const copy = async (addr: string) => {
    // 走公共实现：http 页面下 navigator.clipboard 不存在，直接调用会静默失效
    // （界面表现就是「点了没反应」）。copyText 内部有 execCommand 兜底并返回真实结果。
    if (await copyText(addr)) {
      setCopied(addr)
      setTimeout(() => setCopied(''), 1500)
    } else {
      setError('复制失败（浏览器限制），请手动选中地址复制')
    }
  }

  const pickedLine = lines.find((l) => String(l.frps_id) === newLine)
  const pickedTunnels = pickedLine?.tunnels || []
  const lineIsThirdParty = pickedLine?.line_type === 'third_party'

  const doAdd = async (e: React.FormEvent) => {
    e.preventDefault()
    const lp = Number(newLocal)
    if (!newLine || !Number.isInteger(lp) || lp < 1 || lp > 65535) {
      setError('请选择线路，并填写 1~65535 的本地端口')
      return
    }
    const picked = lines.find((l) => String(l.frps_id) === newLine)
    if (picked && picked.usable === false) {
      setError(picked.reason || '该线路对本实例不可用')
      return
    }
    // 第三方线路必须明确选一条隧道：后端也要求，不再"自动挑第一条"
    if (picked?.line_type === 'third_party' && !newTunnel) {
      setError('请选择服务商分配的固定端口（第三方线路的端口不能自己指定）')
      return
    }
    const ok = await run(() => addInstancePort(instanceId, {
      frps_id: Number(newLine), local_port: lp, protocol: newProto,
      tunnel_id: picked?.line_type === 'third_party' ? newTunnel : '',
    } as any))
    if (ok) { setAdding(false); setNewLocal(''); setNewTunnel('') }
  }

  const doSaveLocal = async (p: InstancePort) => {
    const lp = Number(editLocal)
    if (!Number.isInteger(lp) || lp < 1 || lp > 65535) {
      setError('本地端口需为 1~65535 的整数')
      return
    }
    const ok = await run(() => updateInstancePort(instanceId, p.tunnel_id, { local_port: lp }))
    if (ok) setEditTunnel(null)
  }

  // 是否生效只看 Daemon 实时上报的 live_status（不要回退到数据库里的 status ——
  // 那是"最后一次下发结果"，节点失联时会把它显示成"已生效"）。
  const liveOf = (p: InstancePort) => p.live_status || 'unknown'
  const okOf = (p: InstancePort) => liveOf(p) === 'running'
  const unknownOf = (p: InstancePort) => liveOf(p) === 'unknown'

  return (
    <div className="iports">
      {error && <div className="error-banner">{error}</div>}
      {msg && <div className="success-banner">{msg}</div>}

      {ports.length === 0 ? (
        <div className="iports-empty">
          该实例还没有对外端口。
          {canEdit && '开一个端口后，BlueMap 网页、Geyser 基岩版这类服务就能从公网访问。'}
        </div>
      ) : (
        <table className="iports-table">
          <thead>
            <tr>
              <th>用途 / 协议</th>
              <th>对外地址</th>
              <th>转发到</th>
              <th>状态</th>
              {canEdit && <th>操作</th>}
            </tr>
          </thead>
          <tbody>
            {ports.map((p) => (
              <tr key={p.tunnel_id}>
                <td>
                  <span className="ip-proto">{p.protocol.toUpperCase()}</span>
                  <div className="ip-sub">{p.line_name || '—'}</div>
                </td>
                <td>
                  <span className="ip-addr mono" title="点击复制">{p.public_address}</span>
                  <button className="ip-copy" onClick={() => copy(p.public_address)}>
                    {copied === p.public_address ? '已复制' : '复制'}
                  </button>
                </td>
                <td>
                  {editTunnel === p.tunnel_id ? (
                    <div className="ip-edit">
                      <input
                        value={editLocal}
                        onChange={(e) => setEditLocal(e.target.value)}
                        inputMode="numeric"
                        autoFocus
                      />
                      <button className="primary" disabled={busy} onClick={() => doSaveLocal(p)}>保存</button>
                      <button disabled={busy} onClick={() => setEditTunnel(null)}>取消</button>
                    </div>
                  ) : (
                    <>
                      <span className="mono">本地 {p.local_port}</span>
                      <div className="ip-sub mono">{p.remote_port} → {p.local_port}</div>
                    </>
                  )}
                </td>
                <td>
                  {/* 三态：已生效 / 未生效 / 未知（问不到节点）。
                      "未知"必须与"未生效"区分开 —— 前者是"不知道"，
                      后者是"确实没在跑"，对排障的含义完全不同。 */}
                  <span className={`status ${unknownOf(p) ? 'status-unknown' : okOf(p) ? 'status-running' : 'status-stopped'}`}>
                    {unknownOf(p) ? '未知（节点不可达）' : okOf(p) ? '已生效' : '未生效'}
                  </span>
                  {p.live_error && <div className="ip-err" title={p.live_error}>{p.live_error}</div>}
                </td>
                {canEdit && (
                  <td>
                    <div className="ip-ops">
                      <button
                        disabled={busy}
                        onClick={() => { setEditTunnel(p.tunnel_id); setEditLocal(String(p.local_port)) }}
                        title="改成模组实际监听的本地端口"
                      >
                        改本地端口
                      </button>
                      <button
                        className="danger"
                        disabled={busy}
                        onClick={() => {
                          if (confirm(`关闭公网端口 ${p.public_address}？`)) {
                            run(() => deleteInstancePort(instanceId, p.tunnel_id))
                          }
                        }}
                      >
                        关闭
                      </button>
                    </div>
                  </td>
                )}
              </tr>
            ))}
          </tbody>
        </table>
      )}

      {canEdit && (
        <div className="iports-add">
          {!adding ? (
            <>
              <button onClick={() => setAdding(true)} disabled={remaining === 0}>
                + 开通端口
              </button>
              <span className="muted">
                {remaining < 0
                  ? '端口配额不限'
                  : remaining === 0
                    ? lines.length === 0
                      ? '还没有分配到端口配额（请管理员在「用户管理 → 穿透端口配额」里分配）'
                      : '端口配额已用尽（可在「用户管理」申请更多）'
                    : `还可开通 ${remaining} 个`}
                {!chargingSelf && ' · 从本实例归属者的配额中扣除'}
              </span>
            </>
          ) : (
            <form className="ip-addform" onSubmit={doAdd}>
              <label className="ip-field">
                <span>线路</span>
                <select value={newLine} onChange={(e) => setNewLine(e.target.value)}>
                  {lines.length === 0 && <option value="">（没有可用线路）</option>}
                  {lines.map((l) => (
                    <option key={l.frps_id} value={l.frps_id} disabled={l.usable === false}>
                      {l.frps_name}（{l.unlimited ? '不限' : `剩 ${l.available}`}）
                      {l.usable === false ? ' — 本节点不支持' : ''}
                    </option>
                  ))}
                </select>
              </label>
              {/* 第三方线路：端口由服务商分配，用户只需**选那条固定端口**。
                  自己填一个端口没有意义 —— 服务端不认（实测会回「配置文件错误」）。 */}
              {lineIsThirdParty && (
                <label className="ip-field">
                  <span>固定端口</span>
                  <select value={newTunnel} onChange={(e) => setNewTunnel(e.target.value)}>
                    <option value="">
                      {pickedTunnels.length ? '— 选择服务商分配的端口 —' : '（这条线路没有可绑定的隧道）'}
                    </option>
                    {pickedTunnels.map((t) => (
                      <option key={t.tunnel_id} value={t.tunnel_id}>
                        {t.remote_port}（{t.name}）
                      </option>
                    ))}
                  </select>
                </label>
              )}
              <label className="ip-field narrow">
                <span>本地端口</span>
                <input
                  value={newLocal}
                  onChange={(e) => setNewLocal(e.target.value)}
                  placeholder={lineIsThirdParty ? '实例监听的端口，如 25565' : '如 8123'}
                  inputMode="numeric"
                />
              </label>
              <label className="ip-field narrow">
                <span>协议</span>
                <select value={newProto} onChange={(e) => setNewProto(e.target.value)}>
                  <option value="tcp">TCP</option>
                  <option value="udp">UDP</option>
                </select>
              </label>
              <button className="primary" type="submit" disabled={busy || lines.length === 0}>开通</button>
              <button type="button" onClick={() => setAdding(false)} disabled={busy}>取消</button>
            </form>
          )}
        </div>
      )}

      <div className="iports-note">
        公网地址由「穿透管理」里的线路提供；「转发到」是这条公网口子在实例内部指向的端口。
        多端口模组要把本地端口改成它实际监听的端口（例如 BlueMap 默认 8123、Geyser 基岩版默认 19132）。
        公网端口本身不可修改 —— 它一旦公布出去就可能已被别人配好，要换就关掉重开。
      </div>
    </div>
  )
}
