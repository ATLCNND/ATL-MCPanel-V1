import { useEffect, useMemo, useRef, useState } from 'react'
import {
  LogShareFile, LogShareRecord,
  listLogShareFiles, analyseLog, deleteLogShare, streamLogShareAI,
} from '../api'
import './LogShareTab.css'

/**
 * LogShareTab 第三方日志分析（https://logshare.cn）。
 *
 * 这个页面做的事只有一件：把实例的崩溃/运行日志交给 LogShare 的 AI 分析，
 * 拿回"崩在哪、为什么、怎么修"。它不是我们自己实现的诊断，所以界面必须
 * **把提供方说清楚**，并且在使用前让人明确同意对方的条款 —— 三条硬约束：
 *
 *  1. 页面上标明"免费日志分析由 LogShare 提供"，并给出跳转入口；
 *  2. 每次上传前弹窗告知"要传什么、传到哪、保留多久、哪些信息不会被过滤"，
 *     并**要求手动勾选**同意《服务协议》与《隐私政策》（不预勾选、不记住）。
 *     服务端也会校验这个勾选，前端藏不住也绕不过。
 *  3. 提供「过滤玩家聊天行」（默认开启）：对方的自动过滤只处理 IP，
 *     玩家名与聊天内容会原样过去。
 */

function formatSize(n: number): string {
  if (!n || n <= 0) return '0 B'
  const u = ['B', 'KB', 'MB', 'GB']
  let i = 0
  let v = n
  while (v >= 1024 && i < u.length - 1) { v /= 1024; i++ }
  return `${v >= 100 ? Math.round(v) : v.toFixed(1)} ${u[i]}`
}

function formatTime(ts: number): string {
  if (!ts) return '—'
  return new Date(ts * 1000).toLocaleString('zh-CN', { hour12: false })
}

function fmtExpire(s: string): string {
  if (!s) return '—'
  const d = new Date(s.replace(' ', 'T'))
  if (isNaN(d.getTime())) return s
  const days = Math.ceil((d.getTime() - Date.now()) / 86400000)
  return `${d.toLocaleDateString('zh-CN')}（约 ${days} 天后）`
}

const KIND_LABEL: Record<string, string> = {
  crash: '崩溃报告',
  latest: '当前日志',
  console: '控制台完整输出',
  rotated: '历史日志',
}

export default function LogShareTab({ instanceId, canWrite }: { instanceId: string; canWrite: boolean }) {
  const [files, setFiles] = useState<LogShareFile[]>([])
  const [history, setHistory] = useState<LogShareRecord[]>([])
  const [meta, setMeta] = useState({ siteUrl: 'https://logshare.cn', termsUrl: '', privacyUrl: '', maxBytes: 0 })
  const [picked, setPicked] = useState('')
  const [filterChat, setFilterChat] = useState(true)
  const [error, setError] = useState('')
  const [msg, setMsg] = useState('')
  const [loading, setLoading] = useState(false)
  const [disabled, setDisabled] = useState('')

  // 上传确认弹窗
  const [confirmOpen, setConfirmOpen] = useState(false)
  const [agree, setAgree] = useState(false)
  const [busy, setBusy] = useState(false)

  // 分析结果
  const [activeId, setActiveId] = useState('')      // 正在看的 logshare_id
  const [activeUrl, setActiveUrl] = useState('')
  const [thinking, setThinking] = useState('')
  const [answer, setAnswer] = useState('')
  const [analysing, setAnalysing] = useState(false)
  const [streamErr, setStreamErr] = useState('')
  const abortRef = useRef<AbortController | null>(null)

  const load = async () => {
    setLoading(true)
    setError('')
    try {
      const r = await listLogShareFiles(instanceId)
      setFiles(Array.isArray(r.files) ? r.files : [])
      setHistory(Array.isArray(r.history) ? r.history : [])
      setMeta({
        siteUrl: r.site_url || 'https://logshare.cn',
        termsUrl: r.terms_url || 'https://logshare.cn/terms',
        privacyUrl: r.privacy_url || 'https://logshare.cn/privacy',
        maxBytes: r.max_bytes || 0,
      })
      setDisabled('')
      // 默认选最新的崩溃报告；没有就选 latest.log
      if (!picked) {
        const crash = r.files.find((f) => f.kind === 'crash')
        const latest = r.files.find((f) => f.kind === 'latest')
        setPicked((crash || latest || r.files[0])?.path || '')
      }
    } catch (e: any) {
      // 功能未启用时后端返回 503 —— 这不是错误，是"管理员没开"
      const m = String(e?.message || '')
      if (m.includes('未启用')) setDisabled(m)
      else setError(m)
    } finally {
      setLoading(false)
    }
  }

  useEffect(() => {
    load()
    return () => { abortRef.current?.abort() }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [instanceId])

  const pickedFile = useMemo(() => files.find((f) => f.path === picked), [files, picked])

  /** 打开确认弹窗（每次都要求重新勾选：不预勾选、不记住） */
  const openConfirm = () => {
    setError(''); setMsg('')
    if (!picked) { setError('请先选择要分析的日志文件'); return }
    setAgree(false)
    setConfirmOpen(true)
  }

  const doAnalyse = async () => {
    if (!agree) { setError('请先勾选同意 LogShare 的《服务协议》与《隐私政策》'); return }
    setBusy(true); setError(''); setMsg('')
    try {
      const r = await analyseLog(instanceId, picked, { filterChat, agree })
      setConfirmOpen(false)
      setMsg(`已上传并提交分析（${formatSize(r.size)}，${r.lines} 行` +
        `${r.filtered_lines > 0 ? `，已过滤 ${r.filtered_lines} 行聊天` : ''}` +
        `${r.truncated > 0 ? `，截断了前 ${formatSize(r.truncated)}` : ''}）`)
      setActiveId(r.id)
      setActiveUrl(r.url)
      setThinking(''); setAnswer(''); setStreamErr('')
      await load()
      startStream(r.id)
    } catch (e: any) {
      setError(e.message)
    } finally {
      setBusy(false)
    }
  }

  /** 拉取 AI 流（或回放已缓存的结论） */
  const startStream = (logshareId: string) => {
    abortRef.current?.abort()
    const ac = new AbortController()
    abortRef.current = ac
    setAnalysing(true); setStreamErr('')

    streamLogShareAI(instanceId, logshareId, (ev) => {
      if (ev.event === 'error') {
        try { setStreamErr(JSON.parse(ev.data)) } catch { setStreamErr(ev.data) }
        return
      }
      let j: any = null
      try { j = JSON.parse(ev.data) } catch { return }
      // 对方的字段形态：{type:"thinking"|"content", delta} 或 OpenAI 风格 {choices:[{delta:{content}}]}
      const delta = j?.delta || j?.choices?.[0]?.delta?.content || ''
      if (!delta) return
      if (j.type === 'thinking') setThinking((t) => t + delta)
      else setAnswer((a) => a + delta)
    }, ac.signal)
      .catch((e) => { if (!ac.signal.aborted) setStreamErr(String(e?.message || e)) })
      .finally(() => { if (!ac.signal.aborted) setAnalysing(false) })
  }

  const openRecord = (rec: LogShareRecord) => {
    setActiveId(rec.logshare_id)
    setActiveUrl(rec.url)
    setThinking('')
    setStreamErr('')
    if (rec.analysis) {
      // 已经有结论就直接展示，不再消耗对方的 AI 资源
      setAnswer(rec.analysis)
      setAnalysing(false)
      abortRef.current?.abort()
      return
    }
    setAnswer('')
    startStream(rec.logshare_id)
  }

  const removeCloud = async (rec: LogShareRecord) => {
    if (!confirm(`删除云端副本？\n\n${rec.url}\n\n删除后该链接立即失效（日志里含玩家数据，\n分析完不再需要时建议删掉）。`)) return
    try {
      const r = await deleteLogShare(instanceId, rec.logshare_id)
      setMsg(r.message || '云端副本已删除')
      await load()
    } catch (e: any) {
      setError(e.message)
    }
  }

  if (disabled) {
    return (
      <div className="logshare-tab">
        <div className="ls-disabled">
          <div className="ls-disabled-title">日志分析功能未启用</div>
          <p>{disabled}</p>
          <p className="muted">
            这是<b>第三方免费服务</b>（LogShare.CN）。因为它会把实例日志上传到外部，
            面板默认关闭；需要管理员在配置里显式打开 <span className="mono">logshare.enabled</span>。
          </p>
        </div>
      </div>
    )
  }

  return (
    <div className="logshare-tab">
      {error && <div className="ls-error">{error}</div>}
      {msg && <div className="ls-msg">{msg}</div>}

      {/* ---- 提供方与归因 ---- */}
      <div className="ls-provider">
        <div className="ls-provider-main">
          <span className="ls-provider-badge">第三方</span>
          <span>
            本页的<b>免费日志分析由 LogShare 提供</b>，面板本身不做 AI 诊断。
            日志会上传到对方服务器，由其 AI 给出崩溃原因与修复建议。
          </span>
        </div>
        <a className="ls-site-btn" href={meta.siteUrl} target="_blank" rel="noreferrer noopener">
          访问 logshare.cn ↗
        </a>
      </div>

      <div className="ls-grid">
        {/* ---- 左：文件选择 ---- */}
        <div className="ls-card">
          <div className="ls-card-title">
            选择要分析的日志
            <span className="ls-card-sub">崩溃报告优先；也可以分析当前运行日志</span>
          </div>

          {loading && <div className="ls-empty">加载中…</div>}
          {!loading && files.length === 0 && (
            <div className="ls-empty">
              没有找到可分析的日志（实例还没运行过，或 logs/ 目录为空）。
            </div>
          )}

          <div className="ls-list">
            {files.map((f) => (
              <label key={f.path} className={`ls-file ${picked === f.path ? 'on' : ''}`}>
                <input
                  type="radio"
                  name="ls-file"
                  checked={picked === f.path}
                  onChange={() => setPicked(f.path)}
                />
                <span className={`ls-kind kind-${f.kind}`}>{KIND_LABEL[f.kind] || f.kind}</span>
                <span className="ls-fname mono">{f.name}</span>
                <span className="ls-fmeta">
                  {formatSize(f.size)} · {formatTime(f.mod_time)}
                </span>
              </label>
            ))}
          </div>

          <div className="ls-actions">
            <button className="primary" onClick={openConfirm} disabled={!canWrite || !picked || busy}>
              一键分析
            </button>
            <button onClick={load} disabled={loading}>刷新</button>
            {!canWrite && <span className="muted">需要 owner 及以上权限（上传日志到第三方）</span>}
          </div>
        </div>

        {/* ---- 右：历史 ---- */}
        <div className="ls-card">
          <div className="ls-card-title">
            分析记录
            <span className="ls-card-sub">同一份日志不会重复消耗对方的 AI 资源</span>
          </div>
          {history.length === 0 ? (
            <div className="ls-empty">还没有分析过。</div>
          ) : (
            <div className="ls-history">
              {history.map((h) => (
                <div key={h.id} className={`ls-hist-item ${h.deleted ? 'deleted' : ''}`}>
                  <div className="ls-hist-head">
                    <span className="mono">{h.source_path}</span>
                    <span className="ls-hist-time">{h.created_at}</span>
                  </div>
                  <div className="ls-hist-meta">
                    {formatSize(h.size)} · {h.lines} 行
                    {h.filtered_lines > 0 && ` · 过滤聊天 ${h.filtered_lines} 行`}
                    {h.truncated && ' · 已截断'}
                    {h.deleted && ' · 云端已删除'}
                  </div>
                  <div className="ls-hist-meta">
                    云端保留至 {fmtExpire(h.expires_at)}
                    {h.analysis ? ' · 已有分析结论' : ' · 尚未分析'}
                  </div>
                  <div className="ls-hist-ops">
                    <button onClick={() => openRecord(h)} disabled={h.deleted && !h.analysis}>
                      {h.analysis ? '查看结论' : '查看/开始分析'}
                    </button>
                    <a href={h.url} target="_blank" rel="noreferrer noopener">原日志 ↗</a>
                    {!h.deleted && (
                      <button className="danger" onClick={() => removeCloud(h)}>删除云端副本</button>
                    )}
                  </div>
                </div>
              ))}
            </div>
          )}
        </div>
      </div>

      {/* ---- 分析结果 ---- */}
      {activeId && (
        <div className="ls-card ls-result">
          <div className="ls-card-title">
            AI 分析结果
            {analysing && <span className="ls-running">分析中，可能需要几十秒…</span>}
            <span className="spacer" />
            <a href={activeUrl} target="_blank" rel="noreferrer noopener">在 logshare.cn 打开 ↗</a>
          </div>

          {thinking && (
            <details className="ls-thinking">
              <summary>思考过程（{thinking.length} 字）</summary>
              <pre>{thinking}</pre>
            </details>
          )}

          {streamErr && <div className="ls-error">分析失败：{streamErr}</div>}
          {!answer && !analysing && !streamErr && <div className="ls-empty">（没有内容）</div>}
          {answer && <pre className="ls-answer">{answer}</pre>}
          {analysing && !answer && <div className="ls-empty">正在等待对方的分析结果…</div>}
        </div>
      )}

      {/* ---- 上传确认（每次都要手动勾选） ---- */}
      {confirmOpen && (
        <div className="modal-mask modal-mask-top" onClick={() => !busy && setConfirmOpen(false)}>
          <div className="ls-confirm" onClick={(e) => e.stopPropagation()}>
            <div className="ls-confirm-title">上传到 LogShare 分析</div>

            <div className="ls-confirm-body">
              <p><b>将要上传：</b></p>
              <ul>
                <li><span className="mono">{pickedFile?.name || picked}</span>（{formatSize(pickedFile?.size || 0)}）</li>
                {pickedFile?.kind !== 'latest' && (
                  <li>另外会自动附带 <span className="mono">logs/latest.log</span> 作为上下文（便于判断崩溃前发生了什么）</li>
                )}
              </ul>

              <p><b>上传到哪里：</b>第三方服务 <span className="mono">api.logshare.cn</span>
                ，返回的链接形如 <span className="mono">https://logshare.cn/&lt;id&gt;</span>
                ，<b>默认保留 15 天</b>后自动删除（也可以随时在本页手动删除云端副本）。</p>

              <p className="ls-warn">
                <b>关于隐私：</b>对方会自动给 IP 打码，但<b>玩家名与聊天内容不会被过滤</b>。
                清理实例、开服前后排查问题时，请注意日志里是否含有不希望外传的内容。
              </p>

              <label className="ls-opt">
                <input type="checkbox" checked={filterChat} onChange={(e) => setFilterChat(e.target.checked)} />
                <span>
                  过滤玩家聊天行（推荐）
                  <em>上传前在本地删掉「&lt;玩家名&gt; 内容」形态的聊天行，并在文件头注明过滤了多少行</em>
                </span>
              </label>

              <label className="ls-agree">
                <input type="checkbox" checked={agree} onChange={(e) => setAgree(e.target.checked)} />
                <span>
                  我已阅读并同意 LogShare 的
                  <a href={meta.termsUrl} target="_blank" rel="noreferrer noopener">《服务协议》</a>
                  与
                  <a href={meta.privacyUrl} target="_blank" rel="noreferrer noopener">《隐私政策》</a>
                  ，并确认这份日志可以上传到该第三方服务。
                </span>
              </label>

              {meta.maxBytes > 0 && (
                <p className="muted">单次上传上限 {formatSize(meta.maxBytes)}；超出会保留尾部（崩溃现场在日志后面）并在结果里注明。</p>
              )}
            </div>

            <div className="ls-confirm-ops">
              {error && <span className="ls-confirm-err">{error}</span>}
              <div className="spacer" />
              <button onClick={() => setConfirmOpen(false)} disabled={busy}>取消</button>
              <button className="primary" onClick={doAnalyse} disabled={busy || !agree}>
                {busy ? '上传中…' : '同意并上传分析'}
              </button>
            </div>
          </div>
        </div>
      )}
    </div>
  )
}
