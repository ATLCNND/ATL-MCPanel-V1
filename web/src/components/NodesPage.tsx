import { useEffect, useState } from 'react'
import {
  NodeInfo, NodeProbe, PKIInfo, NodeResource,
  listNodes, createNode, deleteNode, probeNode, deployNode,
  restartDaemon, daemonLogs, getPKI, getNodeCert,
  listNodeResources, uploadNodeResource, deleteNodeResource,
} from '../api'
import './NodesPage.css'

function fmtTime(s: string): string {
  if (!s) return '—'
  const d = new Date(s)
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
  const [form, setForm] = useState({ name: '', ip: '', ssh_user: 'root', ssh_auth: '', ssh_port: '22' })

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
      const [n, p] = await Promise.all([listNodes(), getPKI().catch(() => null)])
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

  const submitAdd = async (e: React.FormEvent) => {
    e.preventDefault()
    await wrap(() => createNode({
      name: form.name,
      ip: form.ip,
      ssh_user: form.ssh_user,
      ssh_auth: form.ssh_auth,
      ssh_port: Number(form.ssh_port) || 22,
    }), '节点已登记')
    setForm({ name: '', ip: '', ssh_user: 'root', ssh_auth: '', ssh_port: '22' })
    setShowAdd(false)
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
            <span className="muted">到期：{new Date(pki.not_after).toLocaleDateString('zh-CN')}</span>
            {!pki.grpc_mtls && <span className="err-text">建议在 config.yaml 中设置 server.grpc_mtls: true</span>}
          </div>
        )}

        {showAdd && (
          <form className="node-form" onSubmit={submitAdd}>
            <h3>登记节点</h3>
            <div className="grid">
              <input placeholder="节点名称（唯一，如 node-002）" value={form.name}
                onChange={(e) => setForm({ ...form, name: e.target.value })} required />
              <input placeholder="节点 IP" value={form.ip}
                onChange={(e) => setForm({ ...form, ip: e.target.value })} required />
              <input placeholder="SSH 用户" value={form.ssh_user}
                onChange={(e) => setForm({ ...form, ssh_user: e.target.value })} />
              <input placeholder="SSH 端口" value={form.ssh_port}
                onChange={(e) => setForm({ ...form, ssh_port: e.target.value })} />
              <input placeholder="SSH 密码或私钥内容（用于一键部署）" value={form.ssh_auth}
                onChange={(e) => setForm({ ...form, ssh_auth: e.target.value })}
                style={{ gridColumn: 'span 4' }} />
            </div>
            <div className="form-actions">
              <button className="primary" type="submit" disabled={busy}>登记</button>
              <button type="button" onClick={() => setShowAdd(false)}>取消</button>
            </div>
            <p className="form-note">
              SSH 凭据仅用于一键部署（上传 Daemon、安装服务）。凭据保存在面板数据库中，
              请确保数据库访问安全。
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
                  <div className="spacer" />
                  <span className="muted">实例 {n.instances} 个</span>
                  <span className="muted">最后心跳 {fmtTime(n.last_seen)}</span>
                </div>

                <div className="node-meta">
                  <span>CPU 核心 {n.cpu}</span>
                  <span>内存 {(n.mem / 1024 / 1024 / 1024).toFixed(1)} GB</span>
                  <span>SSH {n.ssh_user || '—'}@{n.ip}:{n.ssh_port}</span>
                  <span>{n.has_auth ? '已配置凭据' : '未配置凭据'}</span>
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
