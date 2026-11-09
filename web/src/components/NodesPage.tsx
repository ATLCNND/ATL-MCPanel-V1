import { useEffect, useState } from 'react'
import {
  NodeInfo, NodeProbe, PKIInfo, NodeResource,
  listNodes, deleteNode, probeNode, deployNode,
  restartDaemon, daemonLogs, getPKI, getNodeCert,
  listNodeResources, uploadNodeResource, deleteNodeResource, updateNode,
  clearNodeCredential, getLocalNode, adoptLocalNode, installLocalNode,
} from '../api'
import { parseServerTime } from '../time'
import NodeRegisterDialog, { RegisterPrefill } from './NodeRegisterDialog'
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

  /**
   * 登记节点弹窗（null = 没开）。
   *
   * 三种接入口子（面板 SSH 连过去装 / 凭证交换 / 粘贴导入）原来各是一块内联横幅，
   * 互斥关系与先后顺序只能靠读文案猜；现在收进一个弹窗用页签并列（§13 第 7 步 B4）。
   * prefill 用于「本机节点未接入」横幅点「手动填写」时带上 127.0.0.1。
   */
  const [register, setRegister] = useState<{ prefill?: RegisterPrefill } | null>(null)

  /**
   * 编辑中的节点（null = 没有在编辑）。
   *
   * 为什么需要它：以前只有"新增节点"，没有"修改节点" —— 于是 IP 填错、公网 IP
   * 没填都只能删了重建（节点上的 daemon 与证书都白配了）。公网直出又恰好依赖
   * 节点公网 IP，这个缺口必须先补上。
   */
  /** 本机节点现状：面板和节点常在同一台机器上，能一键接进来就不该让用户填 SSH */
  const [localNode, setLocalNode] = useState<{ listening: boolean; registered: boolean; node_name?: string; hostname: string; can_auto_deploy: boolean; message?: string; daemon_port: number } | null>(null)

  const [editing, setEditing] = useState<{ id: number; name: string; ip: string; public_ip: string; ssh_user: string; ssh_port: string; ssh_auth: string; auth_type: 'password' | 'key'; panel_grpc_port: string; daemon_grpc_port: string; backup_root: string } | null>(null)

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
   * 登记弹窗里的「测试连接」已随表单一起搬进 NodeRegisterDialog —— 那里才知道
   * 用户正在填什么，父组件只看结果。
   */

  /** 本机已装好 daemon → 直接登记（不需要 SSH 凭据） */
  const adoptLocal = async () => {
    const r = await wrap(() => adoptLocalNode(), '已接入本机节点')
    if (r) setLocalNode((cur) => (cur ? { ...cur, registered: true } : cur))
  }

  /** 本机只有面板 → 面板自己装 daemon（不走 SSH） */
  const installLocal = async () => {
    if (!confirm('将在这台机器上安装并启动 Daemon（需要面板以 root 运行）。继续？')) return
    setBusy(true); setError(''); setMsg('')
    try {
      const r = await installLocalNode()
      const bad = (r.steps || []).filter((s: any) => !s.ok)
      if (bad.length) {
        setError('部署未完全成功：' + bad.map((s: any) => s.name + '：' + (s.detail || '失败')).join('；'))
      } else {
        setMsg(r.ready ? '本机节点已部署并接入' : '已安装，但 Daemon 端口尚未就绪，请查看服务日志')
      }
      await load()
    } catch (e: any) {
      setError(e.message)
    } finally {
      setBusy(false)
    }
  }

  /** 移除节点上保存的 SSH 凭据（节点本身保留） */
  const clearCred = async (n: NodeInfo) => {
    if (!confirm(`移除节点「${n.name}」保存的 SSH 凭据？\n\n节点会继续用 mTLS/心跳工作，但「探测 / 部署 / 重启 Daemon / 看日志」这些需要 SSH 的功能将不可用。`)) return
    await wrap(() => clearNodeCredential(n.id), 'SSH 凭据已移除')
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
      // 备份根目录：空串表示清空（回到实例目录下的 backups/）
      backup_root: editing.backup_root.trim(),
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
        {/* 三种接入方式都在这个弹窗里（页签切换），不再各占一条工具栏按钮 */}
        <button className="primary" onClick={() => setRegister({})}
          title="SSH 部署 / 凭证交换 / 粘贴导入，三种接入方式">
          + 登记节点
        </button>
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
            {/* 两条路：本机已有 daemon → 直接接入；本机只有面板 → 面板自己装。
                都不需要 SSH 凭据 —— 面板本来就有这台机器的权限。 */}
            {localNode.listening ? (
              <button className="small primary" onClick={adoptLocal} disabled={busy}>接入本机 Daemon</button>
            ) : (
              <button className="small primary" onClick={installLocal} disabled={busy}
                title="在这台机器上安装并启动 Daemon（不走 SSH，需要 root）">
                一键部署本机节点
              </button>
            )}
            <button className="small" onClick={() => setRegister({
              prefill: { name: localNode.hostname || 'local', ip: '127.0.0.1' },
            })}>手动填写</button>
            {localNode.message && <span className="err-text">{localNode.message}</span>}
          </div>
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
                  <button className="small" onClick={() => setEditing(editing?.id === n.id ? null : {
                    id: n.id, name: n.name, ip: n.ip, public_ip: n.public_ip || '',
                    ssh_user: n.ssh_user || '', ssh_port: String(n.ssh_port || 22), ssh_auth: '',
                    auth_type: (n.auth_type === 'key' ? 'key' : 'password') as 'password' | 'key',
                    panel_grpc_port: String(n.panel_grpc_port || 0),
                    daemon_grpc_port: String(n.daemon_grpc_port || 0),
                    backup_root: n.backup_root || '',
                  })}>{editing?.id === n.id ? '收起' : '编辑'}</button>
                  <span className="muted">实例 {n.instances} 个</span>
                  <span className="muted">最后心跳 {fmtTime(n.last_seen)}</span>
                </div>

                {/* 编辑就地展开在**这张卡片里**（以前是页面顶部一个弹窗：
                    改的是列表里的某个节点，眼睛却要跳到页面顶上，也对不上是哪一台）。
                    表单结构与「共享资源」展开块一致，同一个视觉层级。 */}
                {editing?.id === n.id && (
                  <form className="node-edit" onSubmit={submitEdit}>
                    <div className="ne-sec">
                      <div className="ne-sec-title">基本信息</div>
                      <div className="ne-grid">
                        <label className="ne-field">
                          <span>节点名称 <i className="ne-req">*</i></span>
                          <input value={editing.name} required
                            onChange={(e) => setEditing({ ...editing, name: e.target.value })} />
                        </label>
                        <label className="ne-field">
                          <span>节点 IP <i className="ne-req">*</i></span>
                          <input value={editing.ip} required
                            onChange={(e) => setEditing({ ...editing, ip: e.target.value })} />
                          <em>面板连它的地址</em>
                        </label>
                        <label className="ne-field">
                          <span>公网 IP</span>
                          <input value={editing.public_ip}
                            onChange={(e) => setEditing({ ...editing, public_ip: e.target.value })} />
                          <em>玩家连它的地址，公网直出用</em>
                        </label>
                      </div>
                    </div>

                    <div className="ne-sec">
                      <div className="ne-sec-title">连接</div>
                      <div className="ne-grid">
                        <label className="ne-field">
                          <span>SSH 用户</span>
                          <input value={editing.ssh_user}
                            onChange={(e) => setEditing({ ...editing, ssh_user: e.target.value })} />
                        </label>
                        <label className="ne-field">
                          <span>SSH 端口</span>
                          <input value={editing.ssh_port}
                            onChange={(e) => setEditing({ ...editing, ssh_port: e.target.value })} />
                        </label>
                        <label className="ne-field">
                          <span>面板连接端口</span>
                          <input value={editing.panel_grpc_port}
                            onChange={(e) => setEditing({ ...editing, panel_grpc_port: e.target.value })} />
                          <em>0 = 默认</em>
                        </label>
                        <label className="ne-field">
                          <span>Daemon 端口</span>
                          <input value={editing.daemon_grpc_port}
                            onChange={(e) => setEditing({ ...editing, daemon_grpc_port: e.target.value })} />
                          <em>0 = 默认 9091</em>
                        </label>
                      </div>
                    </div>

                    <div className="ne-sec">
                      <div className="ne-sec-title">存储</div>
                      <div className="ne-grid">
                        <label className="ne-field ne-span-all">
                          <span>备份根目录</span>
                          <input value={editing.backup_root}
                            onChange={(e) => setEditing({ ...editing, backup_root: e.target.value })} />
                          <em>留空 = 各实例目录下的 backups/；填绝对路径则落冷存储（如 /mnt/cold-backup）。换目录会让老备份链与新链脱钩</em>
                        </label>
                      </div>
                    </div>

                    <div className="ne-sec">
                      <div className="ne-sec-title">SSH 凭据</div>
                      <div className="ne-radios">
                        <span className="muted">凭据类型</span>
                        <label>
                          <input type="radio" name="edit_auth_type" checked={editing.auth_type === 'password'}
                            onChange={() => setEditing({ ...editing, auth_type: 'password' })} /> 密码
                        </label>
                        <label>
                          <input type="radio" name="edit_auth_type" checked={editing.auth_type === 'key'}
                            onChange={() => setEditing({ ...editing, auth_type: 'key' })} /> 私钥
                        </label>
                        <span className="muted ne-radio-hint">留空下面的输入框则不修改已存凭据</span>
                      </div>
                      <div className="ne-grid">
                        <label className="ne-field ne-span-all">
                          <span>新的密码 / 私钥</span>
                          <input value={editing.ssh_auth}
                            onChange={(e) => setEditing({ ...editing, ssh_auth: e.target.value })} />
                        </label>
                      </div>
                    </div>

                    <div className="ne-foot">
                      <button className="primary" type="submit" disabled={busy}>保存</button>
                      <button type="button" onClick={() => setEditing(null)} disabled={busy}>取消</button>
                      <span className="ne-foot-note">
                        改了 IP / SSH 参数后面板会重建到该节点的连接；公网 IP 只影响「公网直出」线路的判定与展示。
                      </span>
                    </div>
                  </form>
                )}

                <div className="node-meta">
                  <span>CPU 核心 {n.cpu}</span>
                  <span>内存 {(n.mem / 1024 / 1024 / 1024).toFixed(1)} GB</span>
                  {n.enroll_mode === 'local-probe' || n.enroll_mode === 'local-install'
                    ? <span className="muted">本机 · Daemon 端口 {n.grpc_port || '默认'}</span>
                    : <span>SSH {n.ssh_user || '—'}@{n.ip}:{n.ssh_port}</span>}
                  {/* enroll_mode 决定这条节点是怎么进来的：本机两条路径根本不需要凭据，
                      所以不显示"未配置凭据"这种会让人以为还缺东西的文案。 */}
                  {n.enroll_mode === 'local-probe' || n.enroll_mode === 'local-install' ? (
                    <span className="muted">本机节点 · 无需凭据</span>
                  ) : (
                    <span>{n.has_auth ? `已配置${n.auth_type === 'key' ? '密钥' : n.auth_type === 'password' ? '密码' : '凭据'}` : '未配置凭据'}</span>
                  )}
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

      {register && (
        <NodeRegisterDialog
          prefill={register.prefill}
          onClose={() => setRegister(null)}
          onDone={(m) => { setRegister(null); setMsg(m); load() }}
        />
      )}

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
