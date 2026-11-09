import { useEffect, useRef, useState } from 'react'
import { getTLSStatus, generateSelfSigned, uploadCert, uploadCertArchive } from '../api'
import './TlsSettings.css'

/**
 * 面板 HTTPS 证书设置（F7）。
 *
 * 三种来源都在这一页：查看当前证书 / 上传证书 / 生成自签证书。
 *
 * 界面上必须明说两件事，否则用户会踩坑：
 *  1. 改完证书**要重启面板**才生效（本组件只负责把文件放对位置）；
 *  2. 自签证书浏览器会警告，公网请用正式证书。
 *
 * 为什么优先给「上传压缩包」：从 IDC 或证书商后台下载下来就是一个 zip，
 * 里面叫什么名字各家都不一样（`cert.crt` / `fullchain.pem` / `privkey.key` …）。
 * 让用户自己开包、挑文件、再分别粘贴两段文本，是最容易出错的一步 ——
 * 而且粘错了界面只会说「公私钥不匹配」，等于把人卡死在这里。
 * 服务端按**内容**识别，所以文件名随便；手工粘贴那条路保留在折叠区里。
 */
export default function TlsSettings() {
  const [st, setSt] = useState<any>(null)
  const [hosts, setHosts] = useState('')
  const [cert, setCert] = useState('')
  const [key, setKey] = useState('')
  const [file, setFile] = useState<File | null>(null)
  const [busy, setBusy] = useState(false)
  const [msg, setMsg] = useState('')
  const [err, setErr] = useState('')
  const [used, setUsed] = useState('')
  const [drag, setDrag] = useState(false)
  const inputRef = useRef<HTMLInputElement | null>(null)

  const load = async () => {
    try { setSt(await getTLSStatus()) } catch (e: any) { setErr(e.message) }
  }
  useEffect(() => { void load() }, [])

  const run = async (fn: () => Promise<any>, pickUsed?: (r: any) => string) => {
    setBusy(true); setErr(''); setMsg(''); setUsed('')
    try {
      const r = await fn()
      setMsg(r.message || '完成')
      if (pickUsed) setUsed(pickUsed(r))
      await load()
    } catch (e: any) {
      setErr(e.message)
    } finally { setBusy(false) }
  }

  const pickFile = (f: File | null) => {
    setErr(''); setMsg(''); setUsed('')
    if (!f) return
    if (f.size > 2 * 1024 * 1024) {
      setErr(`文件 ${(f.size / 1024 / 1024).toFixed(1)} MB，超过 2 MB 上限。证书文件通常只有几 KB，请确认选对了文件。`)
      return
    }
    setFile(f)
  }

  const submitArchive = () => {
    if (!file) return
    void run(
      () => uploadCertArchive(file),
      (r) => (r.used ? `包内使用：${r.used}` : ''),
    )
  }

  return (
    <div className="tls-page">
      {err && <p className="error-banner">{err}</p>}
      {msg && <p className="success-banner">{msg}{used ? ` · ${used}` : ''}</p>}

      {/* ---- 当前证书 ---- */}
      <section className="tls-card">
        <header className="tls-head">
          <div>
            <h3>当前证书</h3>
            <p className="tls-sub">面板对外提供 HTTPS 用的就是这张证书。</p>
          </div>
          <span className={`tls-badge ${st?.https_now ? 'on' : 'off'}`}>
            {st?.https_now ? '● HTTPS 已启用' : '○ HTTP（未启用 HTTPS）'}
          </span>
        </header>

        {st ? (
          <dl className="tls-kv">
            <div><dt>证书文件</dt><dd className="mono">{st.cert_path || '（未配置）'}</dd></div>
            {st.key_path && <div><dt>私钥文件</dt><dd className="mono">{st.key_path}</dd></div>}
            {st.subject && <div><dt>证书主体</dt><dd>{st.subject}</dd></div>}
            {st.dns_names?.length > 0 && <div><dt>域名 SAN</dt><dd className="mono">{st.dns_names.join(', ')}</dd></div>}
            {st.ip_names?.length > 0 && <div><dt>IP SAN</dt><dd className="mono">{st.ip_names.join(', ')}</dd></div>}
            {st.days_left !== undefined && (
              <div>
                <dt>有效期</dt>
                <dd>
                  到 {st.not_after?.slice(0, 10)}（剩 {st.days_left} 天）
                  {st.days_left < 15 && <span className="tls-tag warn">即将到期</span>}
                  {st.self_signed && <span className="tls-tag">自签 · 浏览器会提示不受信任</span>}
                </dd>
              </div>
            )}
          </dl>
        ) : (
          <p className="tls-sub">读取中…</p>
        )}

        <p className="tls-note">
          换证书只是替换上面两个文件，<b>改完要重启面板才生效</b>：
          <code>systemctl restart atlmcpanel-panel</code>
          <span className="tls-sub">（重启面板不影响正在运行的实例 —— 实例归节点上的 Daemon 管。）</span>
        </p>
      </section>

      {/* ---- 上传证书 ---- */}
      <section className="tls-card">
        <header className="tls-head">
          <div>
            <h3>上传证书</h3>
            <p className="tls-sub">
              把证书商给的压缩包直接拖进来即可。包里的文件名不用改，面板按<b>内容</b>识别证书与私钥；
              要它可用，证书与私钥必须能配对、未过期、且带 SAN。
            </p>
          </div>
        </header>

        <label
          className={`tls-drop ${drag ? 'drag' : ''} ${file ? 'has-file' : ''}`}
          onDragOver={(e) => { e.preventDefault(); setDrag(true) }}
          onDragLeave={() => setDrag(false)}
          onDrop={(e) => {
            e.preventDefault(); setDrag(false)
            pickFile(e.dataTransfer.files?.[0] || null)
          }}
        >
          <input
            ref={inputRef}
            type="file"
            accept=".zip,.pem,.crt,.cer,.key,.txt,application/zip"
            onChange={(e) => pickFile(e.target.files?.[0] || null)}
          />
          <span className="tls-drop-icon">{file ? '📦' : '⬆'}</span>
          <span className="tls-drop-text">
            {file ? (
              <><b>{file.name}</b><small>{(file.size / 1024).toFixed(1)} KB · 点击可换一个文件</small></>
            ) : (
              <><b>拖入 zip，或点击选择文件</b><small>支持 zip / pem / crt / key，单个文件不超过 2 MB</small></>
            )}
          </span>
        </label>

        <div className="tls-actions">
          <button className="primary" disabled={!file || busy} onClick={submitArchive}>
            {busy ? '上传中…' : '上传并保存'}
          </button>
          {file && !busy && (
            <button onClick={() => { setFile(null); if (inputRef.current) inputRef.current.value = '' }}>
              清除
            </button>
          )}
          <span className="tls-sub">保存到 certs/ 目录，重启面板后生效。</span>
        </div>

        <details className="tls-more">
          <summary>没有压缩包，手工粘贴证书与私钥</summary>
          <div className="tls-paste">
            <label>
              <span>证书链（含中间证书）</span>
              <textarea
                className="tls-ta"
                rows={6}
                spellCheck={false}
                value={cert}
                onChange={(e) => setCert(e.target.value)}
                placeholder={'-----BEGIN CERTIFICATE-----\n…\n-----END CERTIFICATE-----'}
              />
            </label>
            <label>
              <span>私钥</span>
              <textarea
                className="tls-ta"
                rows={6}
                spellCheck={false}
                value={key}
                onChange={(e) => setKey(e.target.value)}
                placeholder={'-----BEGIN PRIVATE KEY-----\n…\n-----END PRIVATE KEY-----'}
              />
            </label>
          </div>
          <div className="tls-actions">
            <button
              disabled={busy || !cert.trim() || !key.trim()}
              onClick={() => run(() => uploadCert(cert, key))}
            >
              粘贴并保存
            </button>
            <span className="tls-sub">私钥只会写到本机 certs/ 目录，不会外发。</span>
          </div>
        </details>
      </section>

      {/* ---- 自签证书 ---- */}
      <section className="tls-card">
        <header className="tls-head">
          <div>
            <h3>生成自签证书</h3>
            <p className="tls-sub">
              内网或临时使用可以自签；公网请用正式证书，否则每个访客都会看到「连接不安全」。
              回环地址与主机名会自动补进 SAN。
            </p>
          </div>
        </header>
        <div className="tls-actions">
          <input
            className="tls-hosts"
            value={hosts}
            onChange={(e) => setHosts(e.target.value)}
            placeholder="mc.example.com, 1.2.3.4"
          />
          <button
            className="primary"
            disabled={busy}
            onClick={() => run(() => generateSelfSigned(hosts.split(',').map((h) => h.trim()).filter(Boolean)))}
          >
            生成并保存
          </button>
        </div>
      </section>
    </div>
  )
}
