import { useEffect, useState } from 'react'
import {
  NodeInfo, NodeProbe, PKIInfo, NodeResource,
  listNodes, createNode, deleteNode, probeNode, deployNode,
  restartDaemon, daemonLogs, getPKI, getNodeCert,
  listNodeResources, uploadNodeResource, deleteNodeResource, updateNode, testNodeAuth,
  clearNodeCredential, getLocalNode,
} from '../api'
import { parseServerTime } from '../time'
import './NodesPage.css'

function fmtTime(s: string): string {
  if (!s) return '—'
  // 服务端时间是无时区标记的 UTC，`new Date(s)` 会按本地时间解析 → 早一个时区差
  const d = parseServerTime(s)
  if (isNaN(d.getTime())) return s
  const diff = (Date.now() - d.getTime()) / 1000
  if (diff < 60) return `${Math.floor(diff)} 秒前`
  if (diff < 3600) return `${Math.floor(diff / 60)} 分钟前`
  return d.toLocaleString('zh-CN')
}

export default function NodesPage() {
  const [nodes, setNodes] = useState<NodeInfo[]>([])
  const [pki, setPki] = useState<PKIInfo | null>(null)
  const [probes, setProbes] = useState<Record<number, NodeProbe>>({})
  const [logs, setLogs] = useState<{ node: string; logs: string } | null>(null)
  const [error, setError] = useState('')
  const [msg, setMsg] = useState('')
  const [busy, setBusy] = useState(false)

  // 新增节点表单
  const [showAdd, setShowAdd] = useState(false)
  // auth_type：'password' | 'key' —— 显式声明凭据类型，不再靠"猜"
  const [form, setForm] = useState({ name: '', ip: '', public_ip: '', ssh_user: 'root', ssh_auth: '', auth_type: 'password', ssh_port: '22', panel_grpc_port: '', daemon_grpc_port: '' })
  /** 「测试连接」的结果（登记前先验证地址 + 凭证是否可用） */
  const [authTest, setAuthTest] = useState<{ ok: boolean; text: string } | null>(null)

  /**
   * 编辑中的节点（null = 没有在编辑）。
   *
   * 为什么需要它：以前只有"新增节点"，没有"修改节点" —— 于是 IP 填错、公网 IP
   * 没填都只能删了重建（节点上的 daemon 与证书都白配了）。公网直出又恰好依赖
   * 节点公网 IP，这个缺口必须先补上。
   */
  /** 本机节点现状：面板和节点常在同一台机器上，能一键接进来就不该让用户填 SSH */
  const [localNode, setLocalNode] = useState<{ listening: boolean; registered: boolean; node_name?: string; hostname: string; can_auto_deploy: boolean; message?: string; daemon_port: number } | null>(null)

  const [editing, setEditing] = useState<{ id: number; name: string; ip: string; public_ip: string; ssh_user: string; ssh_port: string; ssh_auth: string; auth_type: 'password' | 'key'; panel_grpc_port: string; daemon_grpc_port: string } | null>(null)

  // 节点共享资源（一次只展开一个节点）
  const [resNode, setResNode] = useState<number | null>(null)
  const [resInfo, setResInfo] = useState<{ dir: string; files: NodeResource[]; available: boolean; message?: string } | null>(null)

  const loadResources = async (nodeId: number) => {
    try {
      setResInfo(await listNodeResources(nodeId))
    } catch (e: any) {
      setResInfo({ dir: '', files: [], available: false, message: e.message })
    }
  }

  const doUploadResource = async (nodeId: number, file: File) => {
    setBusy(true); setError(''); setMsg('')
    try {
      const r = await uploadNodeResource(nodeId, file)
      setMsg(r.message || '上传完成')
      await loadResources(nodeId)
    } catch (e: any) {
      // 同名冲突时问一句是否覆盖 —— 静默覆盖会让引用该资源的实例行为突变
      if (/已存在/.test(e.message)) {
        if (confirm(`${e.message}\n\n要覆盖吗？正在使用该 jar 的实例会在下次重启时用到新版本。`)) {
          try {
            const r = await uploadNodeResource(nodeId, file, true)
            setMsg(r.message || '已覆盖')
            await loadResources(nodeId)
          } catch (e2: any) {
            setError(e2.message)
          }
        }
      } else {
        setError(e.message)
      }
    } finally {
      setBusy(false)
    }
  }

  const doDeleteResource = async (nodeId: number, f: NodeResource) => {
    let force = false
    if (f.referenced) {
      if (!confirm(`「${f.name}」正被 ${f.refs.length} 个实例使用（${f.refs.join('、')}）。\n删除后这些实例将无法启动，确定继续？`)) return
      force = true
    } else if (!confirm(`删除共享资源「${f.name}」？`)) {
      return
    }
    setBusy(true); setError(''); setMsg('')
    try {
      const r = await deleteNodeResource(nodeId, f.name, force)
      setMsg(r.message || '已删除')
      await loadResources(nodeId)
    } catch (e: any) {
      setError(e.message)
    } finally {
      setBusy(false)
    }
  }

  const load = async () => {
    try {
      const [n, p, ln] = await Promise.all([listNodes(), getPKI().catch(() => null), getLocalNode().catch(() => null)])
      setLocalNode(ln)
      setNodes(Array.isArray(n) ? n : [])
      setPki(p)
      setError('')
    } catch (e: any) {
      setError(e.message)
    }
  }

  useEffect(() => { load() }, [])

  const wrap = async (fn: () => Promise<any>, okMsg?: string) => {
    setBusy(true); setError(''); setMsg('')
    try {
      const r = await fn()
      if (okMsg || r?.message) setMsg(okMsg || r.message)
      await load()
      return r
    } catch (e: any) {
      setError(e.message)
      return null
    } finally {
      setBusy(false)
    }
  }

  const doProbe = async (n: NodeInfo) => {
    const r = await wrap(() => probeNode(n.id))
    if (r?.probe) setProbes((prev) => ({ ...prev, [n.id]: r.probe }))
  }

  const doDeploy = async (n: NodeInfo) => {
    if (!confirm(`将在节点 ${n.name}（${n.ip}）上安装 Daemon：\n` +
      `  • 上传 Daemon 二进制与 mTLS 证书\n  • 写入配置并安装 systemd 服务\n  • 启动服务并校验\n\n继续？`)) return
    const r = await wrap(() => deployNode(n.id), '部署完成')
    if (r?.steps) setMsg(`部署完成：${r.steps.join(' → ')}`)
  }

  const doLogs = async (n: NodeInfo) => {
    const r = await wrap(() => daemonLogs(n.id, 150))
    if (r) setLogs(r)
  }

  const doCert = async (n: NodeInfo) => {
    const r = await wrap(() => getNodeCert(n.id))
    if (r) {
      // 证书材料较大，用新窗口展示便于复制
      const text = `# 节点 ${r.node_name} 的 mTLS 材料\n# 有效期 ${r.expires_days} 天\n\n` +
        `=== ca.crt ===\n${r.ca_cert}\n=== node.crt ===\n${r.client_cert}\n=== node.key ===\n${r.client_key}`
      const w = window.open('', '_blank')
      if (w) {
        w.document.write(`<pre style="font-family:monospace;font-size:12px;white-space:pre-wrap">${
          text.replace(/[<>&]/g, (c) => ({ '<': '&lt;', '>': '&gt;', '&': '&amp;' }[c] as string))
        }</pre>`)
        w.document.title = `节点证书 - ${r.node_name}`
      }
    }
  }

  /**
   * 用表单里的地址与凭证当场试连一次。
   *
   * 登记节点是唯一"填错要等点部署才发现"的地方，而那时的报错往往只有一句
   * 认证失败。先在这里点一下，通了再保存。
   */
  const testAuth = async () => {
    setAuthTest(null)
    setBusy(true)
    try {
      const r = await testNodeAuth({
        ip: form.ip.trim(),
        ssh_port: Number(form.ssh_port) || 22,
        ssh_user: form.ssh_user.trim() || 'root',
        ssh_auth: form.ssh_auth,
        auth_type: form.auth_type as 'password' | 'key',
      })
      const p = r.probe || {}
      setAuthTest({
        ok: true,
        text: `连接成功：${p.os || '?'} / ${p.arch || '?'}，systemd ${p.has_systemd ? '可用' : '不可用'}，剩余磁盘 ${p.free_disk_mb || 0} MB`,
      })
    } catch (e: any) {
      setAuthTest({ ok: false, text: e.message })
    } finally {
      setBusy(false)
    }
  }

  /** 移除节点上保存的 SSH 凭据（节点本身保留） */
  const clearCred = async (n: NodeInfo) => {
    if (!confirm(`移除节点「${n.name}」保存的 SSH 凭据？\n\n节点会继续用 mTLS/心跳工作，但「探测 / 部署 / 重启 Daemon / 看日志」这些需要 SSH 的功能将不可用。`)) return
    await wrap(() => clearNodeCredential(n.id), 'SSH 凭据已移除')
  }

  const submitAdd = async (e: React.FormEvent) => {
    e.preventDefault()
    await wrap(() => createNode({
      name: form.name,
      ip: form.ip,
      public_ip: form.public_ip.trim(),
      ssh_user: form.ssh_user,
      ssh_auth: form.ssh_auth,
      auth_type: form.auth_type as 'password' | 'key',
      ssh_port: Number(form.ssh_port) || 22,
      panel_grpc_port: Number(form.panel_grpc_port) || 0,
      daemon_grpc_port: Number(form.daemon_grpc_port) || 0,
    }), '节点已登记')
    setForm({ name: '', ip: '', public_ip: '', ssh_user: 'root', ssh_auth: '', auth_type: 'password', ssh_port: '22', panel_grpc_port: '', daemon_grpc_port: '' })
    setAuthTest(null)
    setShowAdd(false)
  }

  /**
   * 保存节点修改。
   *
   * 只提交**改过的**字段：SSH 凭据留空表示"不改"，否则一次编辑就会把
   * 原来存好的密码/私钥清掉（后端对空串是"不更新"语义）。
   */
  const submitEdit = async (e: React.FormEvent) => {
    e.preventDefault()
    if (!editing) return
    const payload: Record<string, unknown> = {
      name: editing.name,
      ip: editing.ip,
      public_ip: editing.public_ip.trim(),
      ssh_user: editing.ssh_user,
      ssh_port: Number(editing.ssh_port) || 22,
      panel_grpc_port: Number(editing.panel_grpc_port) || 0,
      daemon_grpc_port: Number(editing.daemon_grpc_port) || 0,
    }
    if (editing.ssh_auth.trim()) {
      payload.ssh_auth = editing.ssh_auth.trim()
      payload.auth_type = editing.auth_type  // 只在改了凭据时一起提交类型
    }
    const ok = await wrap(() => updateNode(editing.id, payload as any), '节点已更新')
    if (ok) setEditing(null)
  }

  return (
    <div className="nodes-page">
      <div className="page-toolbar">
        <button onClick={load} disabled={busy}>刷新</button>
        <button className="primary" onClick={() => setShowAdd(!showAdd)}>+ 登记节点</button>
      </div>

      <div className="nodes-body">
        {error && <div className="error-banner">{error}</div>}
        {msg && <div className="success-banner">{msg}</div>}

        {pki && (
          <div className="pki-banner">
            <span className={`status ${pki.grpc_mtls ? 'status-running' : 'status-error'}`}>
              {pki.grpc_mtls ? 'mTLS 已启用' : 'mTLS 未启用'}
            </span>
            <span className="muted">CA：{pki.subject}</span>
            <span className="muted">到期：{parseServerTime(pki.not_after).toLocaleDateString('zh-CN')}</span>
            {!pki.grpc_mtls && <span className="err-text">建议在 config.yaml 中设置 server.grpc_mtls: true</span>}
          </div>
        )}

        {localNode && !localNode.registered && (
          <div className="pki-banner">
            <span className="status status-error">本机节点未接入</span>
            <span className="muted">
              面板所在的这台机器（{localNode.hostname}）还没有作为节点登记。
              {localNode.listening
                ? `检测到 Daemon 端口 ${localNode.daemon_port} 已在监听 —— 直接填 127.0.0.1 登记即可，不需要 SSH。`
                : '在同一台机器上跑实例时，可以直接登记为 127.0.0.1 节点（无需 SSH 凭据）。'}
            </span>
            <button className="small primary" onClick={() => {
              setShowAdd(true)
              setForm({ ...form, name: localNode.hostname || 'local', ip: '127.0.0.1', ssh_auth: '' })
            }}>用本机地址登记</button>
            {localNode.message && <span className="err-text">{localNode.message}</span>}
          </div>
        )}

        {showAdd && (
          <form className="node-form" onSubmit={submitAdd}>
            <h3>登记节点</h3>
            <div className="grid">
              <input placeholder="节点名称（唯一，如 node-002）" value={form.name}
                onChange={(e) => setForm({ ...form, name: e.target.value })} required />
              <input placeholder="管理地址（面板连它用的 IP 或域名）" value={form.ip}
                onChange={(e) => setForm({ ...form, ip: e.target.value })} required />
              <input placeholder="公网地址（玩家连它用的，公网直出需要；可留空）" value={form.public_ip}
                onChange={(e) => setForm({ ...form, public_ip: e.target.value })} />
              <input placeholder="SSH 用户（默认 root）" value={form.ssh_user}
                onChange={(e) => setForm({ ...form, ssh_user: e.target.value })} />
              <input placeholder="SSH 端口（默认 22）" value={form.ssh_port}
                onChange={(e) => setForm({ ...form, ssh_port: e.target.value })} />
              {/* 端口可覆盖：面板的 9090 被端口映射改过、或多面板共存时需要 */}
              <input placeholder="面板连接端口（留空=默认，节点连面板用）" value={form.panel_grpc_port}
                onChange={(e) => setForm({ ...form, panel_grpc_port: e.target.value })} />
              <input placeholder="Daemon 端口（留空=默认 9091）" value={form.daemon_grpc_port}
                onChange={(e) => setForm({ ...form, daemon_grpc_port: e.target.value })} />

              {/* 凭证类型必须显式选：以前靠内容猜，私钥少一行就被当成密码，
                  最后只报一句认证失败，用户根本不知道哪里错。 */}
              <label className="node-auth-type" style={{ gridColumn: 'span 4' }}>
                <span className="muted">凭证类型</span>
                <label><input type="radio" name="auth_type" checked={form.auth_type === 'password'}
                  onChange={() => setForm({ ...form, auth_type: 'password' })} /> 密码</label>
                <label><input type="radio" name="auth_type" checked={form.auth_type === 'key'}
                  onChange={() => setForm({ ...form, auth_type: 'key' })} /> 私钥</label>
              </label>

              {form.auth_type === 'key' ? (
                <textarea placeholder="贴私钥全文（-----BEGIN OPENSSH PRIVATE KEY----- 到 END 那一行，别漏末尾）"
                  value={form.ssh_auth}
                  onChange={(e) => setForm({ ...form, ssh_auth: e.target.value })}
                  rows={5} style={{ gridColumn: 'span 4', fontFamily: 'var(--mono, monospace)', fontSize: 12 }} />
              ) : (
                <input type="password" placeholder="SSH 密码" value={form.ssh_auth}
                  onChange={(e) => setForm({ ...form, ssh_auth: e.target.value })}
                  style={{ gridColumn: 'span 4' }} />
              )}
            </div>
            <div className="form-actions">
              <button className="primary" type="submit" disabled={busy}>登记</button>
              <button type="button" onClick={testAuth} disabled={busy || !form.ip.trim() || !form.ssh_auth}>
                测试连接
              </button>
              <button type="button" onClick={() => { setShowAdd(false); setAuthTest(null) }}>取消</button>
            </div>
            {authTest && (
              <p className={authTest.ok ? 'form-note' : 'err-text'} style={{ marginTop: 6 }}>
                {authTest.ok ? '✓ ' : '✗ '}{authTest.text}
              </p>
            )}
            <p className="form-note">
              SSH 凭据仅用于一键部署（上传 Daemon、安装服务）。凭据保存在面板数据库中，
              请确保数据库访问安全。
            </p>
          </form>
        )}

        {editing && (
          <form className="node-form" onSubmit={submitEdit}>
            <h3>编辑节点 #{editing.id}</h3>
            <div className="grid">
              <input placeholder="节点名称" value={editing.name}
                onChange={(e) => setEditing({ ...editing, name: e.target.value })} required />
              <input placeholder="节点 IP（面板连它的地址）" value={editing.ip}
                onChange={(e) => setEditing({ ...editing, ip: e.target.value })} required />
              <input placeholder="公网 IP（玩家连它的地址，公网直出用）" value={editing.public_ip}
                onChange={(e) => setEditing({ ...editing, public_ip: e.target.value })} />
              <input placeholder="SSH 用户" value={editing.ssh_user}
                onChange={(e) => setEditing({ ...editing, ssh_user: e.target.value })} />
              <input placeholder="SSH 端口" value={editing.ssh_port}
                onChange={(e) => setEditing({ ...editing, ssh_port: e.target.value })} />
              <input placeholder="面板连接端口（0=默认）" value={editing.panel_grpc_port}
                onChange={(e) => setEditing({ ...editing, panel_grpc_port: e.target.value })} />
              <input placeholder="Daemon 端口（0=默认 9091）" value={editing.daemon_grpc_port}
                onChange={(e) => setEditing({ ...editing, daemon_grpc_port: e.target.value })} />
              <label className="node-auth-type" style={{ gridColumn: 'span 4' }}>
                <span className="muted">新凭据类型（留空则不修改凭据）</span>
                <label><input type="radio" name="edit_auth_type" checked={editing.auth_type === 'password'}
                  onChange={() => setEditing({ ...editing, auth_type: 'password' })} /> 密码</label>
                <label><input type="radio" name="edit_auth_type" checked={editing.auth_type === 'key'}
                  onChange={() => setEditing({ ...editing, auth_type: 'key' })} /> 私钥</label>
              </label>
              <input placeholder="新的密码/私钥（留空则不修改）" value={editing.ssh_auth}
                onChange={(e) => setEditing({ ...editing, ssh_auth: e.target.value })}
                style={{ gridColumn: 'span 4' }} />
            </div>
            <div className="form-actions">
              <button className="primary" type="submit" disabled={busy}>保存</button>
              <button type="button" onClick={() => setEditing(null)}>取消</button>
            </div>
            <p className="form-note">
              改了 IP / SSH 参数后面板会重建到该节点的连接。公网 IP 只影响「公网直出」线路的判定与展示，
              不会改变面板与 Daemon 之间的连接方式。
            </p>
          </form>
        )}

        <div className="node-list">
          {nodes.map((n) => {
            const p = probes[n.id]
            return (
              <div className="node-card" key={n.id}>
                <div className="node-head">
                  <span className="node-name">{n.name}</span>
                  <span className={`status status-${n.status === 'online' ? 'running' : 'error'}`}>{n.status}</span>
                  <span className="mono muted">{n.ip}</span>
                  {n.public_ip && <span className="mono muted">公网 {n.public_ip}</span>}
                  <div className="spacer" />
                  {n.has_auth && (
                    <button className="small danger" title="清掉保存的 SSH 凭据（节点本身保留）"
                      onClick={() => clearCred(n)}>移除凭据</button>
                  )}
                  <button className="small" onClick={() => setEditing({
                    id: n.id, name: n.name, ip: n.ip, public_ip: n.public_ip || '',
                    ssh_user: n.ssh_user || '', ssh_port: String(n.ssh_port || 22), ssh_auth: '',
                    auth_type: (n.auth_type === 'key' ? 'key' : 'password') as 'password' | 'key',
                    panel_grpc_port: String(n.panel_grpc_port || 0),
                    daemon_grpc_port: String(n.daemon_grpc_port || 0),
                  })}>编辑</button>
                  <span className="muted">实例 {n.instances} 个</span>
                  <span className="muted">最后心跳 {fmtTime(n.last_seen)}</span>
                </div>

                <div className="node-meta">
                  <span>CPU 核心 {n.cpu}</span>
                  <span>内存 {(n.mem / 1024 / 1024 / 1024).toFixed(1)} GB</span>
                  <span>SSH {n.ssh_user || '—'}@{n.ip}:{n.ssh_port}</span>
                  <span>{n.has_auth ? `已配置${n.auth_type === 'key' ? '密钥' : n.auth_type === 'password' ? '密码' : '凭据'}` : '未配置凭据'}</span>
                  {n.panel_grpc_port > 0 && <span>面板端口 {n.panel_grpc_port}</span>}
                  {n.grpc_port > 0 && n.grpc_port !== 9091 && <span>Daemon 端口 {n.grpc_port}</span>}
                </div>

                {p && (
                  <div className="node-probe">
                    <span>系统：{p.os || '未知'}（{p.arch}）</span>
                    <span>systemd：{p.has_systemd ? '支持' : '不支持'}</span>
                    <span>Daemon：{p.installed ? '已安装' : '未安装'}</span>
                    <span>磁盘可用：{p.free_disk_mb > 1024 ? `${(p.free_disk_mb / 1024).toFixed(1)} GB` : `${p.free_disk_mb} MB`}</span>
                  </div>
                )}

                <div className="node-actions">
                  <button onClick={() => { setResNode(resNode === n.id ? null : n.id); if (resNode !== n.id) loadResources(n.id) }} disabled={busy}>
                    {resNode === n.id ? '收起共享资源' : '共享资源'}
                  </button>
                  <button onClick={() => doProbe(n)} disabled={busy || !n.has_auth}>环境探测</button>
                  <button className="primary" onClick={() => doDeploy(n)} disabled={busy || !n.has_auth}>
                    {p?.installed ? '重新部署 / 升级' : '一键部署'}
                  </button>
                  <button onClick={() => restartDaemon(n.id).then(load)} disabled={busy || !n.has_auth}>重启 Daemon</button>
                  <button onClick={() => doLogs(n)} disabled={busy || !n.has_auth}>查看日志</button>
                  <button onClick={() => doCert(n)} disabled={busy}>导出证书</button>
                  <button className="danger" onClick={() => {
                    if (confirm(`删除节点 ${n.name}？该操作不会卸载节点上的服务。`)) {
                      deleteNode(n.id).then(load)
                    }
                  }} disabled={busy}>删除</button>
                </div>

                {/* 节点共享资源：管理员在这一处上传，本节点所有实例复用同一份 jar */}
                {resNode === n.id && (
                  <div className="node-res">
                    <div className="nr-head">
                      <b>共享资源</b>
                      <span className="muted mono">{resInfo?.dir || '（未配置资源目录）'}</span>
                      <div className="spacer" />
                      <label className="nr-upload">
                        <input
                          type="file"
                          accept=".jar,.zip,.tar,.gz,.tgz"
                          disabled={busy}
                          onChange={(e) => {
                            const f = e.target.files?.[0]
                            if (f) doUploadResource(n.id, f)
                            e.target.value = ''
                          }}
                        />
                        <span className="nr-upload-btn">{busy ? '上传中…' : '+ 上传资源'}</span>
                      </label>
                    </div>

                    {resInfo && !resInfo.available && (
                      <div className="nr-hint">{resInfo.message || '该节点尚未开放共享资源目录'}</div>
                    )}

                    {resInfo && resInfo.available && (
                      resInfo.files.length === 0 ? (
                        <div className="nr-hint">
                          还没有共享资源。上传一份服务端 jar 后，创建实例时就能直接从这个列表里选，
                          同节点多个实例共用同一份文件，不必各存一遍。
                        </div>
                      ) : (
                        <table className="nr-table">
                          <thead>
                            <tr><th>文件</th><th>大小</th><th>被引用</th><th>操作</th></tr>
                          </thead>
                          <tbody>
                            {resInfo.files.map((f) => (
                              <tr key={f.name}>
                                <td className="mono">{f.name}</td>
                                <td>{(f.size / 1024 / 1024).toFixed(1)} MB</td>
                                <td>
                                  {f.referenced
                                    ? <span className="nr-refs" title={f.refs.join('、')}>{f.refs.length} 个实例</span>
                                    : <span className="muted">—</span>}
                                </td>
                                <td>
                                  <button
                                    className="danger-outline"
                                    disabled={busy}
                                    onClick={() => doDeleteResource(n.id, f)}
                                  >
                                    删除
                                  </button>
                                </td>
                              </tr>
                            ))}
                          </tbody>
                        </table>
                      )
                    )}
                  </div>
                )}

                {!n.has_auth && (
                  <div className="node-warn">未配置 SSH 凭据，无法探测与一键部署（可先更新节点信息补上凭据）</div>
                )}
              </div>
            )
          })}
          {nodes.length === 0 && <div className="empty">暂无节点</div>}
        </div>
      </div>

      {logs && (
        <div className="modal-mask" onClick={() => setLogs(null)}>
          <div className="log-modal" onClick={(e) => e.stopPropagation()}>
            <div className="log-head">
              <strong>Daemon 日志 — {logs.node}</strong>
              <div className="spacer" />
              <button onClick={() => setLogs(null)}>关闭</button>
            </div>
            <pre className="log-body">{logs.logs || '（无输出）'}</pre>
          </div>
        </div>
      )}
    </div>
  )
}
