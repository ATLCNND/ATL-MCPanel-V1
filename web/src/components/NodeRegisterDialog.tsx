import { useEffect, useState } from 'react'
import { createNode, testNodeAuth, importJoinRequest, issueJoinToken } from '../api'
import { basePath } from '../api'
import { copyText } from '../clipboard'
import './NodesPage.css'
import './NodeRegisterDialog.css'

/**
 * 登记节点弹窗。
 *
 * 为什么要有这个弹窗：原来「登记节点」是页面上的一块内联表单，而接入一台节点其实有
 * 三条完全不同的路（面板 SSH 连过去装 / 节点上跑一条命令 / 节点生成请求贴回来）。
 * 三条路各自在页面上摊开成横幅，谁跟谁是互斥的、该先做哪一步全靠读文案猜；而且
 * 表单填到一半往下滚，顶上的节点列表还在，容易点错卡片。
 *
 * 现在收进一个弹窗，用页签把三条路并列摆出来：选一条、填完、登记完就关。
 */
type Mode = 'ssh' | 'join' | 'import'

export interface RegisterPrefill {
  /** 从「本机节点未接入」横幅点「手动填写」时带进来的默认值 */
  name?: string
  ip?: string
  ssh_auth?: string
}

export default function NodeRegisterDialog({ prefill, onClose, onDone }: {
  prefill?: RegisterPrefill
  onClose: () => void
  /** 登记成功后由父组件刷新列表并提示 */
  onDone: (msg: string) => void
}) {
  const [mode, setMode] = useState<Mode>('ssh')
  const [busy, setBusy] = useState(false)
  const [err, setErr] = useState('')

  // —— 方式一：面板用 SSH 连过去部署 ——
  const [form, setForm] = useState({
    name: prefill?.name || '',
    ip: prefill?.ip || '',
    public_ip: '',
    ssh_user: 'root',
    ssh_auth: prefill?.ssh_auth || '',
    auth_type: 'password' as 'password' | 'key',
    ssh_port: '22',
    panel_grpc_port: '',
    daemon_grpc_port: '',
  })
  const [authTest, setAuthTest] = useState<{ ok: boolean; text: string } | null>(null)

  // —— 方式二：凭证交换（面板签一次性 token） ——
  const [joinCmd, setJoinCmd] = useState<{ cmd: string; expires: string } | null>(null)

  // —— 方式三：节点发起（粘贴 enroll --init 打印的那一行） ——
  const [importText, setImportText] = useState('')

  const set = (k: keyof typeof form, v: string) => setForm({ ...form, [k]: v })

  // Esc 关闭：弹窗盖住了节点列表，留一条键盘退路
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => { if (e.key === 'Escape') onClose() }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [onClose])

  const testAuth = async () => {
    setAuthTest(null); setErr(''); setBusy(true)
    try {
      const r = await testNodeAuth({
        ip: form.ip.trim(),
        ssh_port: Number(form.ssh_port) || 22,
        ssh_user: form.ssh_user.trim() || 'root',
        ssh_auth: form.ssh_auth,
        auth_type: form.auth_type,
      })
      const p = r.probe || {}
      setAuthTest({
        ok: true,
        text: `连接成功：${p.os || '?'} / ${p.arch || '?'}，systemd ${p.has_systemd ? '可用' : '不可用'}，剩余磁盘 ${p.free_disk_mb || 0} MB`,
      })
    } catch (e: any) {
      setAuthTest({ ok: false, text: e.message })
    } finally { setBusy(false) }
  }

  const submitSsh = async (e: React.FormEvent) => {
    e.preventDefault()
    setErr('')
    if (!form.name.trim()) { setErr('请填写节点名称'); return }
    if (!form.ip.trim()) { setErr('请填写管理地址'); return }
    setBusy(true)
    try {
      await createNode({
        name: form.name.trim(),
        ip: form.ip.trim(),
        public_ip: form.public_ip.trim(),
        ssh_user: form.ssh_user.trim() || 'root',
        ssh_auth: form.ssh_auth,
        auth_type: form.auth_type,
        ssh_port: Number(form.ssh_port) || 22,
        panel_grpc_port: Number(form.panel_grpc_port) || 0,
        daemon_grpc_port: Number(form.daemon_grpc_port) || 0,
      })
      onDone(`节点 ${form.name.trim()} 已登记`)
    } catch (e: any) {
      setErr(e.message)
    } finally { setBusy(false) }
  }

  const genToken = async () => {
    setErr(''); setBusy(true); setJoinCmd(null)
    try {
      const r = await issueJoinToken('panel', '')
      const panelURL = window.location.origin + basePath()
      setJoinCmd({
        cmd: `atl-daemon enroll --panel ${panelURL} --token ${r.token}`,
        expires: r.expires_at,
      })
    } catch (e: any) {
      setErr(e.message)
    } finally { setBusy(false) }
  }

  const doImport = async () => {
    if (!importText.trim()) return
    setErr(''); setBusy(true)
    try {
      const r = await importJoinRequest(importText.trim())
      onDone('已导入 ' + (r.node_name || r.node_uid) + '，证书已签发')
    } catch (e: any) {
      setErr(e.message)
    } finally { setBusy(false) }
  }

  return (
    <div className="modal-mask" onMouseDown={(e) => { if (e.target === e.currentTarget) onClose() }}>
      <div className="panel nreg-dialog" role="dialog" aria-modal="true" aria-label="登记节点">
        <div className="modal-header">
          <h2>登记节点</h2>
          <button className="nreg-close" onClick={onClose} title="关闭（Esc）" aria-label="关闭">×</button>
        </div>

        <div className="nreg-tabs" role="tablist">
          <button role="tab" aria-selected={mode === 'ssh'} className={`nreg-tab${mode === 'ssh' ? ' active' : ''}`}
            onClick={() => { setMode('ssh'); setErr('') }}>
            <b>面板连过去装</b>
            <span>给 SSH 凭据，面板自己把 Daemon 推上去</span>
          </button>
          <button role="tab" aria-selected={mode === 'join'} className={`nreg-tab${mode === 'join' ? ' active' : ''}`}
            onClick={() => { setMode('join'); setErr('') }}>
            <b>凭证交换</b>
            <span>面板签一次性 token，节点上跑一条命令（不交出 SSH 凭据）</span>
          </button>
          <button role="tab" aria-selected={mode === 'import'} className={`nreg-tab${mode === 'import' ? ' active' : ''}`}
            onClick={() => { setMode('import'); setErr('') }}>
            <b>粘贴导入</b>
            <span>节点上 <span className="mono">enroll --init</span> 打印的那一行贴进来</span>
          </button>
        </div>

        {err && <div className="error-banner">{err}</div>}

        {mode === 'ssh' && (
          <form className="nreg-body" onSubmit={submitSsh}>
            <div className="ne-sec">
              <div className="ne-sec-title">基本信息</div>
              <div className="ne-grid">
                <label className="ne-field">
                  <span>节点名称<i className="ne-req">*</i></span>
                  <input value={form.name} onChange={(e) => set('name', e.target.value)} placeholder="唯一，如 node-002" autoFocus />
                  <em>列表与下拉里显示的名字</em>
                </label>
                <label className="ne-field">
                  <span>管理地址<i className="ne-req">*</i></span>
                  <input value={form.ip} onChange={(e) => set('ip', e.target.value)} placeholder="面板连它用的 IP 或域名" />
                  <em>面板 ↔ Daemon 走这个地址</em>
                </label>
                <label className="ne-field">
                  <span>公网地址</span>
                  <input value={form.public_ip} onChange={(e) => set('public_ip', e.target.value)} placeholder="玩家连它用的，可留空" />
                  <em>公网直出需要；NAT 后填映射后的地址</em>
                </label>
              </div>
            </div>

            <div className="ne-sec">
              <div className="ne-sec-title">SSH 连接</div>
              <div className="ne-grid">
                <label className="ne-field">
                  <span>SSH 用户</span>
                  <input value={form.ssh_user} onChange={(e) => set('ssh_user', e.target.value)} placeholder="root" />
                  <em>需要能 sudo 装服务</em>
                </label>
                <label className="ne-field">
                  <span>SSH 端口</span>
                  <input value={form.ssh_port} onChange={(e) => set('ssh_port', e.target.value)} placeholder="22" />
                  <em>默认 22</em>
                </label>
                <label className="ne-field">
                  <span>面板连接端口</span>
                  <input value={form.panel_grpc_port} onChange={(e) => set('panel_grpc_port', e.target.value)} placeholder="留空=默认" />
                  <em>节点连面板用；端口映射改过才需要填</em>
                </label>
                <label className="ne-field">
                  <span>Daemon 端口</span>
                  <input value={form.daemon_grpc_port} onChange={(e) => set('daemon_grpc_port', e.target.value)} placeholder="留空=9091" />
                  <em>面板连节点用；多面板共存时才改</em>
                </label>
              </div>
            </div>

            <div className="ne-sec">
              <div className="ne-sec-title">SSH 凭据</div>
              <div className="ne-radios">
                <label><input type="radio" name="nr-auth-type" checked={form.auth_type === 'password'}
                  onChange={() => setForm({ ...form, auth_type: 'password', ssh_auth: '' })} /> 密码</label>
                <label><input type="radio" name="nr-auth-type" checked={form.auth_type === 'key'}
                  onChange={() => setForm({ ...form, auth_type: 'key', ssh_auth: '' })} /> 私钥</label>
                <span className="nreg-radios-hint">必须显式选：以前靠内容猜，私钥少一行就被当成密码，最后只报一句认证失败</span>
              </div>
              {form.auth_type === 'key' ? (
                <label className="ne-field ne-span-all">
                  <textarea value={form.ssh_auth} rows={5} onChange={(e) => set('ssh_auth', e.target.value)}
                    placeholder="贴私钥全文（-----BEGIN OPENSSH PRIVATE KEY----- 到 END 那一行，别漏末尾）"
                    className="mono" style={{ fontSize: 12 }} />
                </label>
              ) : (
                <label className="ne-field ne-span-all">
                  <input type="password" value={form.ssh_auth} onChange={(e) => set('ssh_auth', e.target.value)} placeholder="SSH 密码" />
                </label>
              )}
              <p className="form-note">
                凭据会明文存进面板数据库，请确保数据库本身的访问安全。不想交出 SSH 凭据就换上面两个页签。
              </p>
              {authTest && (
                <p className={authTest.ok ? 'form-note' : 'err-text'}>
                  {authTest.ok ? '✓ ' : '✗ '}{authTest.text}
                </p>
              )}
            </div>

            <div className="nreg-foot">
              <button type="button" onClick={onClose}>取消</button>
              <div className="nreg-foot-nav">
                <button type="button" onClick={testAuth} disabled={busy || !form.ip.trim() || !form.ssh_auth}>
                  测试连接
                </button>
                <button className="primary" type="submit" disabled={busy}>登记</button>
              </div>
            </div>
          </form>
        )}

        {mode === 'join' && (
          <div className="nreg-body">
            <p className="nreg-lead">
              面板签一个<b>一次性 token</b>（用后即焚、限时失效），在目标节点上执行下面这条命令即可接入 ——
              面板不保存任何 SSH 凭据。
            </p>
            {joinCmd ? (
              <>
                <div className="nreg-cmd">
                  <code className="mono">{joinCmd.cmd}</code>
                </div>
                <p className="form-note">
                  token 于 <b>{joinCmd.expires}</b> 过期；请在<b>目标节点</b>上以 root 执行。
                </p>
              </>
            ) : (
              <p className="form-note">点「生成命令」，然后把命令复制到目标节点执行。</p>
            )}
            <div className="nreg-foot">
              <button type="button" onClick={onClose}>关闭</button>
              <div className="nreg-foot-nav">
                {joinCmd && <button type="button" onClick={() => { copyText(joinCmd.cmd); }}>复制命令</button>}
                <button className="primary" type="button" onClick={genToken} disabled={busy}>
                  {joinCmd ? '重新生成' : '生成命令'}
                </button>
              </div>
            </div>
          </div>
        )}

        {mode === 'import' && (
          <div className="nreg-body">
            <p className="nreg-lead">
              在节点上执行 <code className="mono">atl-daemon enroll --init</code>，把打印出的那一行贴到下面。
              请求自带节点签名（用其中 CSR 的公钥验签），30 分钟内有效。
            </p>
            <label className="ne-field ne-span-all">
              <span>接入请求</span>
              <textarea rows={4} className="mono" value={importText} onChange={(e) => setImportText(e.target.value)}
                placeholder="atl-join-v1.…" style={{ fontSize: 12 }} />
              <em>整行粘贴，别截断</em>
            </label>
            <div className="nreg-foot">
              <button type="button" onClick={onClose}>取消</button>
              <div className="nreg-foot-nav">
                <button className="primary" type="button" onClick={doImport} disabled={busy || !importText.trim()}>
                  导入并签发证书
                </button>
              </div>
            </div>
          </div>
        )}
      </div>
    </div>
  )
}
