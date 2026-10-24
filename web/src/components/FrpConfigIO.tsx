import { useEffect, useState } from 'react'
import { applyFrpcImport, exportFrps, previewFrpcImport, type ImportPreview } from '../api'
import { copyText } from '../clipboard'
import './FrpConfigIO.css'

// 穿透配置的导入/导出弹窗（底账 F15）。
//
// 两个弹窗的共同原则：**先让用户看清内容，再谈写操作**。
//   · 导入这边只做"解析预览"（服务端不落库）：用户能看到解析出的服务端、
//     token（打码）、每条隧道与被跳过的部分，确认无误再走下一步；
//   · 导出这边默认**不含 token**：默认导出的是"结构"，要看得到凭据得自己勾选，
//     勾了会记审计 —— 凭据一旦贴到聊天/工单里就收不回来了。

/**
 * 导入弹窗：粘贴 → 解析预览 → **确认后落库**。
 *
 * 为什么是两步而不是一步：落库会新增线路与隧道（写操作），而"贴进去就建"
 * 正是这个功能最该避免的形态 —— 用户根本没机会看清解析出了什么。
 * 所以预览永远先跑，落库按钮只出现在预览成功之后。
 */
export function ImportConfigDialog({ onClose, onApplied }: { onClose: () => void; onApplied?: () => void }) {
  const [text, setText] = useState('')
  const [busy, setBusy] = useState(false)
  const [res, setRes] = useState<ImportPreview | null>(null)
  const [lineName, setLineName] = useState('')
  const [applied, setApplied] = useState<{ line_name: string; message: string } | null>(null)

  const apply = async () => {
    setBusy(true)
    try {
      const r = await applyFrpcImport(text, lineName.trim())
      setApplied({ line_name: r.line_name, message: r.message })
      onApplied?.()
    } catch (e: any) {
      setRes({ ok: false, error: e?.message || '建线路失败' })
    } finally {
      setBusy(false)
    }
  }

  const parse = async () => {
    if (!text.trim()) return
    setBusy(true)
    setRes(null)
    setApplied(null)
    try {
      setRes(await previewFrpcImport(text))
    } catch (e: any) {
      setRes({ ok: false, error: e?.message || '解析请求失败' })
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="modal-mask" onClick={onClose}>
      <div className="panel frpio-dialog" onClick={(e) => e.stopPropagation()}>
        <div className="frpio-head">
          <h3>导入第三方 frpc 配置</h3>
          <span className="muted">先解析看结果，确认无误再建线路</span>
        </div>
        <div className="tunnels-hint">
          把服务商给的 <code>frpc.toml</code> 整段粘进来（带不带标语行都行），
          先解析看结果。<b>这一步不会创建任何线路或隧道</b>。
        </div>

        <textarea
          value={text}
          onChange={(e) => setText(e.target.value)}
          placeholder={"serverAddr = \"frp.example.com\"\nserverPort = 7000\nauth.token = \"...\"\n\n[[proxies]]\nname = \"mc\"\ntype = \"tcp\"\nlocalPort = 25565\nremotePort = 25570"}
          spellCheck={false}
          className="frpio-textarea"
        />

        <div className="frpio-actions">
          <button className="primary" onClick={parse} disabled={busy || !text.trim()}>
            {busy ? '解析中…' : '解析预览'}
          </button>
          <button onClick={() => { setText(''); setRes(null) }} disabled={busy}>清空</button>
          <span className="spacer" />
          <button onClick={onClose}>关闭</button>
        </div>

        {res && !res.ok && (
          <div className="tunnels-error" style={{ marginTop: 12 }}>
            <b>解析失败：</b>{res.error}
          </div>
        )}

        {res && res.ok && res.line && (
          <>
            <div className="frpio-section">
              <div className="frpio-section-title">线路</div>
              <dl className="frpio-kv">
                <dt>服务端</dt>
                <dd className="mono">{res.line.server_addr}:{res.line.server_port}</dd>
                <dt>user</dt>
                <dd className="mono">{res.line.user || '（未提供）'}</dd>
                <dt>token</dt>
                <dd className="mono">
                  {res.line.has_token
                    ? <>{res.line.token_masked} <span className="muted">（已打码，明文不会回传）</span></>
                    : <span className="err-text">未提供</span>}
                </dd>
                <dt>传输层 TLS</dt>
                <dd>{res.line.tls_enable ? '服务商要求开启' : '关闭 / 未声明'}</dd>
              </dl>
            </div>

            <div className="frpio-section">
              <div className="frpio-section-title">隧道（{res.proxies?.length || 0} 条）</div>
              <table className="frpio-table">
                <thead>
                  <tr><th>名称</th><th>协议</th><th>本地</th><th>远端</th><th>域名</th><th>限速</th></tr>
                </thead>
                <tbody>
                  {(res.proxies || []).map((p, i) => (
                    <tr key={i}>
                      <td className="mono">{p.name || '—'}</td>
                      <td><span className="frpio-proto">{p.type}</span></td>
                      <td className="mono">{(p.local_ip || '127.0.0.1') + ':' + p.local_port}</td>
                      <td className="mono">{p.remote_port || '—'}</td>
                      <td className="mono">
                        {(p.custom_domains && p.custom_domains.length)
                          ? p.custom_domains.join(', ')
                          : (p.subdomain || '—')}
                      </td>
                      <td className="mono">{p.bandwidth_limit || '—'}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>

            {!!(res.warnings && res.warnings.length) && (
              <div style={{ marginTop: 12 }}>
                <h4 style={{ margin: '0 0 6px' }}>被跳过的内容（不影响导入）</h4>
                <ul className="tunnels-hint" style={{ margin: 0, paddingLeft: 18 }}>
                  {res.warnings!.map((w, i) => <li key={i}><code className="mono small">{w}</code></li>)}
                </ul>
              </div>
            )}

            {applied ? (
              <div className="tunnels-success" style={{ marginTop: 14 }}>
                <b>已建线路「{applied.line_name}」</b>：{applied.message}
                <div className="tunnels-hint" style={{ margin: '6px 0 0' }}>
                  隧道现在还没有绑定实例 —— 到「线路分配」里给它们指定实例后才会真正生效。
                </div>
              </div>
            ) : (
              <div className="frpio-section">
                <div className="frpio-section-title">确认导入</div>
                <div className="form-row">
                  <input
                    placeholder={'线路名称（留空用 ' + (res.line?.server_addr || '服务端地址') + '）'}
                    value={lineName}
                    onChange={(e) => setLineName(e.target.value)}
                    style={{ width: 280 }}
                  />
                  <button className="primary" onClick={apply} disabled={busy}>
                    {busy ? '创建中…' : '建线路与 ' + (res.proxies?.length || 0) + ' 条隧道'}
                  </button>
                </div>
                <div className="tunnels-hint">
                  会建一条<b>第三方单隧道</b>线路（不分配端口段，远端端口/域名按配置原样记录）。
                  同名线路已存在时会被拒绝。隧道建好后需在「线路分配」里绑定实例。
                </div>
              </div>
            )}
          </>
        )}
      </div>
    </div>
  )
}

/** 导出弹窗：把一条线路渲染成标准 frpc TOML 并复制。 */
export function ExportConfigDialog({
  lineId, lineName, onClose,
}: { lineId: number; lineName: string; onClose: () => void }) {
  const [withToken, setWithToken] = useState(false)
  const [loliaStyle, setLoliaStyle] = useState(false)
  const [text, setText] = useState('')
  const [includesToken, setIncludesToken] = useState(false)
  const [err, setErr] = useState('')
  const [busy, setBusy] = useState(false)
  const [copied, setCopied] = useState('')

  useEffect(() => {
    let alive = true
    setBusy(true)
    setErr('')
    exportFrps(lineId, { withToken, loliaStyle })
      .then((r) => { if (alive) { setText(r.text); setIncludesToken(r.includes_token) } })
      .catch((e: any) => { if (alive) { setText(''); setErr(e?.message || '导出失败') } })
      .finally(() => { if (alive) setBusy(false) })
    return () => { alive = false }
  }, [lineId, withToken, loliaStyle])

  const copy = async () => {
    // 同上：http 下 clipboard 不可用，必须走带兜底的公共实现
    setCopied((await copyText(text)) ? '已复制到剪贴板' : '复制失败：请手动全选文本')
    setTimeout(() => setCopied(''), 2500)
  }

  return (
    <div className="modal-mask" onClick={onClose}>
      <div
        className="panel"
        onClick={(e) => e.stopPropagation()}
        style={{ width: 'min(820px, 94vw)', maxHeight: '88vh', overflowY: 'auto' }}
      >
        <h3>导出配置 · {lineName}</h3>
        <label className="switch" style={{ marginRight: 16 }}>
          <input type="checkbox" checked={withToken} onChange={(e) => setWithToken(e.target.checked)} />
          <span>包含 token</span>
        </label>
        <label className="switch">
          <input type="checkbox" checked={loliaStyle} onChange={(e) => setLoliaStyle(e.target.checked)} />
          <span>用 [metadatas] 写法</span>
        </label>

        {includesToken && (
          <div className="tunnels-error" style={{ marginTop: 10 }}>
            <b>这份文本含凭据（token）。</b>只贴给可信的人；本次导出已记入审计日志。
          </div>
        )}
        {err && <div className="tunnels-error" style={{ marginTop: 10 }}>{err}</div>}

        <pre
          className="mono"
          style={{
            marginTop: 12, padding: 12, maxHeight: '46vh', overflow: 'auto',
            background: 'var(--bg-console, #0f1117)', borderRadius: 6, fontSize: 12,
          }}
        >{busy ? '生成中…' : text}</pre>

        <div className="form-row" style={{ marginTop: 10 }}>
          <button className="primary" onClick={copy} disabled={busy || !text}>{copied || '复制配置'}</button>
          <button onClick={onClose}>关闭</button>
        </div>
      </div>
    </div>
  )
}

