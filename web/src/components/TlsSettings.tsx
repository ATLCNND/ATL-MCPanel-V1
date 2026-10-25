import { useEffect, useState } from 'react'
import { getTLSStatus, generateSelfSigned, uploadCert } from '../api'

/**
 * 面板 HTTPS 证书设置（F7）。
 *
 * 三种来源都在这一个卡片里：自签生成 / 上传正式证书 / 查看当前状态。
 *
 * 界面上必须明说两件事，否则用户会踩坑：
 *  1. 改完证书**要重启面板**才生效（本组件只负责把文件放对位置）；
 *  2. 自签证书浏览器会警告，公网请用正式证书。
 */
export default function TlsSettings() {
  const [st, setSt] = useState<any>(null)
  const [hosts, setHosts] = useState('')
  const [cert, setCert] = useState('')
  const [key, setKey] = useState('')
  const [busy, setBusy] = useState(false)
  const [msg, setMsg] = useState('')
  const [err, setErr] = useState('')

  const load = async () => {
    try { setSt(await getTLSStatus()) } catch (e: any) { setErr(e.message) }
  }
  useEffect(() => { void load() }, [])

  const run = async (fn: () => Promise<any>) => {
    setBusy(true); setErr(''); setMsg('')
    try {
      const r = await fn()
      setMsg(r.message || '完成')
      await load()
    } catch (e: any) {
      setErr(e.message)
    } finally { setBusy(false) }
  }

  return (
    <div className="panel">
      <h3>HTTPS 证书</h3>
      {err && <p className="error">{err}</p>}
      {msg && <p className="ok">{msg}</p>}

      {st && (
        <div className="form-note">
          <div>当前协议：<b>{st.https_now ? 'HTTPS' : 'HTTP'}</b></div>
          <div>配置路径：<code className="mono">{st.cert_path || '（未配置）'}</code></div>
          {st.subject && <div>证书主体：{st.subject}</div>}
          {st.dns_names?.length > 0 && <div>域名 SAN：{st.dns_names.join(', ')}</div>}
          {st.ip_names?.length > 0 && <div>IP SAN：{st.ip_names.join(', ')}</div>}
          {st.days_left !== undefined && (
            <div>
              到期：{st.not_after?.slice(0, 10)}（剩 {st.days_left} 天）
              {st.days_left < 15 && <b className="muted"> ← 即将到期，请尽快更换</b>}
              {st.self_signed && <span className="muted"> · 自签证书，浏览器会提示不受信任</span>}
            </div>
          )}
          <div><b>改完证书要重启面板才生效。</b></div>
        </div>
      )}

      <h4>生成自签证书</h4>
      <p className="form-note">输入面板的域名或 IP（逗号分隔）。回环地址与主机名会自动补上。</p>
      <input value={hosts} onChange={(e) => setHosts(e.target.value)} placeholder="mc.example.com, 1.2.3.4" />
      <button className="primary" disabled={busy}
        onClick={() => run(() => generateSelfSigned(hosts.split(',').map((h) => h.trim()).filter(Boolean)))}>
        生成并保存
      </button>

      <h4>上传正式证书</h4>
      <p className="form-note">粘贴 Nginx 格式的证书与私钥（含 -----BEGIN----- 行）。要求：能配对、未过期、含 SAN。</p>
      <textarea rows={5} value={cert} onChange={(e) => setCert(e.target.value)} placeholder="-----BEGIN CERTIFICATE-----" />
      <textarea rows={5} value={key} onChange={(e) => setKey(e.target.value)} placeholder="-----BEGIN PRIVATE KEY-----" />
      <button disabled={busy} onClick={() => run(() => uploadCert(cert, key))}>上传并保存</button>
    </div>
  )
}
