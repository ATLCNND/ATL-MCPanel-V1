import { useEffect, useMemo, useRef, useState } from 'react'
import {
  LogShareFile, AnalysisProvidersResp, AnalysisRecord, AnalysisKind,
  listLogShareFiles, listAnalysisProviders, analyseInstance, deleteAnalysisRecord,
  streamAnalysisAI, listAnalysisHistory,
} from '../api'
import MiniMarkdown from './MiniMarkdown'
import './LogShareTab.css'

/**
 * 「日志分析」页。
 *
 * 2026-09-30 起这里不再只对接 LogShare，而是**提供方链**：
 *
 *   1. **LogShare**（首选，公益合作）—— 给 AI 结论（崩在哪/为什么/怎么修）；
 *   2. **mclo.gs**（保底）—— **不做 AI**，只把日志变成一个可分享的链接并数 ERROR 行；
 *      它真正的价值是"MC 互助社区里贴日志的标准做法"：拿到链接的人直接能看原文。
 *      所以这条路上面板要做的是**把求助文本拼好**（带上社区看不到的环境信息），
 *      并给出第一次使用必须确认的**用户须知**；
 *   3. **自配平台**（可选）—— 用户自己的 OpenAI 兼容平台，消耗自己的额度。
 *
 * 三条路共用同一套界面：选文件 → 确认（告知 + 手动勾选）→ 结果。
 * 结果区按 provider_kind 切换：AI 走 SSE 流式渲染，mclo.gs 走"链接 + 求助"卡片。
 */

const KIND_LABEL: Record<string, string> = {
  logshare: 'LogShare（AI 分析）',
  mclogs: 'mclo.gs（分享链接）',
  openai: '自配平台',
  'builtin-rules': '内置规则',
}

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

const KIND_ICON: Record<string, string> = {
  crash: '崩溃报告',
  latest: '当前日志',
  console: '控制台完整输出',
  rotated: '历史日志',
}

/** mclo.gs 用户须知的确认标记（存在浏览器里：这是"用户已读"的凭据，不需要上服务端） */
const MCLOGS_NOTICE_KEY = 'atlmcpanel_mclogs_notice_v1'

export default function LogShareTab({ instanceId, canWrite }: { instanceId: string; canWrite: boolean }) {
  const [files, setFiles] = useState<LogShareFile[]>([])
  const [history, setHistory] = useState<AnalysisRecord[]>([])
  const [providers, setProviders] = useState<AnalysisProvidersResp | null>(null)
  const [providerId, setProviderId] = useState(0)   // 0 = 自动（按链依次尝试）
  const [meta, setMeta] = useState({ siteUrl: 'https://logshare.cn', termsUrl: '', privacyUrl: '', maxBytes: 0 })
  const [picked, setPicked] = useState('')
  const [filterChat, setFilterChat] = useState(true)
  const [phenomenon, setPhenomenon] = useState('')
  const [error, setError] = useState('')
  const [msg, setMsg] = useState('')
  const [loading, setLoading] = useState(false)
  const [disabled, setDisabled] = useState('')

  // 上传确认弹窗
  const [confirmOpen, setConfirmOpen] = useState(false)
  const [agree, setAgree] = useState(false)
  const [noticeOpen, setNoticeOpen] = useState(false)   // mclo.gs 用户须知
  // 须知确认状态**记在这台浏览器上**（localStorage）：它是"用户已读"的凭据，
  // 不需要也不应该上传服务端（面板不该记录"谁读过哪份第三方条款"）。
  const [noticeRead, setNoticeRead] = useState(() => {
    try { return localStorage.getItem(MCLOGS_NOTICE_KEY) === '1' } catch { return false }
  })
  const [busy, setBusy] = useState(false)

  // 本次结果：AI 结论 or mclo.gs 分享链接
  const [result, setResult] = useState<{
    recordId: number
    kind: AnalysisKind
    providerName: string
    url?: string
    rawUrl?: string
    errors?: number
    lines?: number
    expiresAt?: string
    helpText?: string
    notice?: string
    fallbackNote?: string
    fallbacks?: { provider: string; error: string }[]
  } | null>(null)

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
      setMeta({
        siteUrl: r.site_url || 'https://logshare.cn',
        termsUrl: r.terms_url || 'https://logshare.cn/terms',
        privacyUrl: r.privacy_url || 'https://logshare.cn/privacy',
        maxBytes: r.max_bytes || 0,
      })
      setDisabled('')
      if (!picked) {
        const crash = r.files.find((f) => f.kind === 'crash')
        const latest = r.files.find((f) => f.kind === 'latest')
        setPicked((crash || latest || r.files[0])?.path || '')
      }
    } catch (e: any) {
      const m = String(e?.message || '')
      if (m.includes('未启用')) setDisabled(m)
      else setError(m)
    } finally {
      setLoading(false)
    }
  }

  const loadProviders = async () => {
    try {
      setProviders(await listAnalysisProviders())
    } catch { /* 读不到就走默认链，不影响本页其它功能 */ }
  }

  const loadHistory = async () => {
    try {
      const r = await listAnalysisHistory(instanceId)
      setHistory(Array.isArray(r.history) ? r.history : [])
    } catch { /* 忽略 */ }
  }

  useEffect(() => {
    load()
    loadProviders()
    loadHistory()
    return () => { abortRef.current?.abort() }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [instanceId])

  const pickedFile = useMemo(() => files.find((f) => f.path === picked), [files, picked])
  const chain = providers?.chain || []
  /** 选中的提供方类型（决定确认弹窗里写哪家的条款） */
  const selected = useMemo(() => {
    if (providerId === 0) return chain[0] || null
    return chain.find((c) => c.id === providerId && c.kind !== 'logshare') ||
      chain.find((c) => c.id === providerId) || null
  }, [providerId, chain])

  const openConfirm = () => {
    setError(''); setMsg('')
    if (!picked) { setError('请先选择要分析的日志文件'); return }
    setAgree(false)
    setConfirmOpen(true)
  }

  const doAnalyse = async () => {
    if (!agree) { setError('请先勾选同意'); return }
    setBusy(true); setError(''); setMsg('')
    try {
      const r = await analyseInstance(instanceId, {
        path: picked, filterChat, agree, providerId, phenomenon,
      })
      setConfirmOpen(false)
      setResult({
        recordId: r.record_id,
        kind: r.provider_kind,
        providerName: r.provider?.name || KIND_LABEL[r.provider_kind] || r.provider_kind,
        url: r.url, rawUrl: r.raw_url, errors: r.errors, lines: r.lines,
        expiresAt: r.expires_at, helpText: r.help_text, notice: r.notice,
        fallbackNote: r.fallback_note,
        fallbacks: (r.fallbacks || []).map((f) => ({ provider: f.provider, error: f.error })),
      })
      setThinking(''); setAnswer(''); setStreamErr('')
      await loadHistory()
      if (r.ai_available) startStream(r.record_id)
    } catch (e: any) {
      setError(e.message)
    } finally {
      setBusy(false)
    }
  }

  /** 拉取 AI 流（或回放已缓存的结论） */
  const startStream = (recordId: number) => {
    abortRef.current?.abort()
    const ac = new AbortController()
    abortRef.current = ac
    setAnalysing(true)
    setStreamErr('')
    streamAnalysisAI(recordId, (ev) => {
      const text = (() => {
        try {
          const v = JSON.parse(ev.data)
          return typeof v === 'string' ? v : String(v)
        } catch { return ev.data }
      })()
      if (ev.event === 'content') setAnswer((a) => a + text)
      else if (ev.event === 'thinking') setThinking((t) => t + text)
      else if (ev.event === 'error') setStreamErr(text)
      else if (ev.event === 'done') setAnalysing(false)
    }, ac.signal)
      .catch((e: any) => { if (e.name !== 'AbortError') setStreamErr(e.message) })
      .finally(() => setAnalysing(false))
  }

  const openRecord = (rec: AnalysisRecord) => {
    setError(''); setMsg('')
    setResult({
      recordId: rec.id,
      kind: rec.provider_kind,
      providerName: KIND_LABEL[rec.provider_kind] || rec.provider_kind,
      url: rec.url, rawUrl: rec.raw_url, errors: rec.errors, lines: rec.lines,
      expiresAt: rec.expires_at,
    })
    setThinking(''); setStreamErr('')
    if (rec.provider_kind === 'mclogs') {
      setAnswer('')
      setAnalysing(false)
      return
    }
    if (rec.analysis) {
      setAnswer(rec.analysis)
      setAnalysing(false)
      abortRef.current?.abort()
      return
    }
    setAnswer('')
    startStream(rec.id)
  }

  const removeCloud = async (rec: AnalysisRecord) => {
    if (!confirm(`删除云端副本？\n\n${rec.url || '（自配平台没有云端副本）'}\n\n删除后该链接立即失效（日志里含玩家数据，分析完不再需要时建议删掉）。`)) return
    try {
      const r = await deleteAnalysisRecord(rec.id)
      setMsg(r.message || '云端副本已删除')
      await loadHistory()
    } catch (e: any) {
      setError(e.message)
    }
  }

  const copy = (text: string, what: string) => {
    navigator.clipboard?.writeText(text)
      .then(() => setMsg(`${what}已复制到剪贴板`))
      .catch(() => setError('复制失败：浏览器拒绝了剪贴板访问，请手动选中复制'))
  }

  const needsMclogsNotice = selected?.kind === 'mclogs' && !noticeRead

  if (disabled) {
    return (
      <div className="logshare-tab">
        <div className="ls-disabled">
          <div className="ls-disabled-title">日志分析功能未启用</div>
          <p>{disabled}</p>
          <p className="muted">
            日志分析会按「LogShare → mclo.gs → 自配平台」的顺序尝试；
            总管理员可以在本页开启或关闭它。
          </p>
        </div>
      </div>
    )
  }

  return (
    <div className="logshare-tab">
      {error && <div className="ls-error" onClick={() => setError('')}>{error}</div>}
      {msg && <div className="ls-msg" onClick={() => setMsg('')}>{msg}</div>}

      {/* ---- 提供方与归因 ---- */}
      <div className="ls-provider">
        <div className="ls-provider-main">
          <span className="ls-provider-badge">第三方</span>
          <span>
            分析按顺序尝试：
            {chain.length === 0
              ? <b> 暂无可用的提供方</b>
              : chain.map((c, i) => (
                <span key={`${c.kind}-${c.id}-${i}`}>
                  {i > 0 && ' → '}
                  <b>{c.name}</b>
                  <span className="muted">（{KIND_LABEL[c.kind] || c.kind}）</span>
                </span>
              ))}
            。<b>LogShare</b> 给 AI 结论；<b>mclo.gs</b> 不做 AI，只把日志变成可分享的链接（用于去社区求助）。
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
            <div className="ls-empty">没有找到可分析的日志（实例还没运行过，或 logs/ 目录为空）。</div>
          )}

          <div className="ls-list">
            {files.map((f) => (
              <label key={f.path} className={`ls-file ${picked === f.path ? 'on' : ''}`}>
                <input type="radio" name="ls-file" checked={picked === f.path} onChange={() => setPicked(f.path)} />
                <span className={`ls-kind kind-${f.kind}`}>{KIND_ICON[f.kind] || f.kind}</span>
                <span className="ls-fname mono">{f.name}</span>
                <span className="ls-fmeta">{formatSize(f.size)} · {formatTime(f.mod_time)}</span>
              </label>
            ))}
          </div>

          <div className="ls-actions">
            <label className="ls-pick">
              用哪个平台：
              <select value={providerId} onChange={(e) => setProviderId(Number(e.target.value))}>
                <option value={0}>自动（按顺序尝试，推荐）</option>
                {chain.map((c) => (
                  <option key={`${c.kind}-${c.id}`} value={c.id || 0} disabled={c.id === 0 && providerId !== 0}>
                    {c.name}（{KIND_LABEL[c.kind] || c.kind}）
                  </option>
                ))}
              </select>
            </label>
            <button className="primary" onClick={openConfirm} disabled={!canWrite || !picked || busy}>
              一键分析
            </button>
            <button onClick={() => { load(); loadHistory() }} disabled={loading}>刷新</button>
            {!canWrite && <span className="muted">需要 owner 及以上权限（上传日志到第三方）</span>}
          </div>

          {/* 「现象」是给求助文本用的：社区看不到你的控制台，但"崩之前发生了什么"往往最关键 */}
          <label className="ls-phenomenon">
            现象（可选，会写进求助文本）：
            <input value={phenomenon} maxLength={200}
              placeholder="例如：启动后约 30 秒崩溃 / 玩家一进服就掉线"
              onChange={(e) => setPhenomenon(e.target.value)} />
          </label>
        </div>

        {/* ---- 右：历史 ---- */}
        <div className="ls-card">
          <div className="ls-card-title">
            分析记录
            <span className="ls-card-sub">已有结论不会再消耗额度</span>
          </div>
          {history.length === 0 ? (
            <div className="ls-empty">还没有分析过。</div>
          ) : (
            <div className="ls-history">
              {history.map((h) => (
                <div key={h.id} className={`ls-hist-item ${h.deleted ? 'deleted' : ''}`}>
                  <div className="ls-hist-head">
                    <span className="ls-prov-tag">{KIND_LABEL[h.provider_kind] || h.provider_kind}</span>
                    <span className="mono">{h.source_path}</span>
                    <span className="ls-hist-time">{h.created_at}</span>
                  </div>
                  <div className="ls-hist-meta">
                    {formatSize(h.size)} · {h.lines} 行
                    {h.errors > 0 && ` · ERROR ${h.errors} 行`}
                    {h.filtered_lines > 0 && ` · 过滤聊天 ${h.filtered_lines} 行`}
                    {h.truncated && ' · 已截断'}
                    {h.deleted && ' · 云端已删除'}
                  </div>
                  <div className="ls-hist-meta">
                    {h.provider_kind === 'mclogs' ? '分享链接保留至 ' : '云端保留至 '}
                    {fmtExpire(h.expires_at)}
                    {h.provider_kind !== 'mclogs' && (h.analysis ? ' · 已有分析结论' : ' · 尚未分析')}
                  </div>
                  <div className="ls-hist-ops">
                    <button onClick={() => openRecord(h)}>{h.analysis ? '查看结论' : '查看'}</button>
                    {h.url && <a href={h.url} target="_blank" rel="noreferrer noopener">打开 ↗</a>}
                    {!h.deleted && h.provider_kind !== 'openai' && (
                      <button className="danger" onClick={() => removeCloud(h)}>删除云端副本</button>
                    )}
                  </div>
                </div>
              ))}
            </div>
          )}
        </div>
      </div>

      {/* ---- 结果区 ---- */}
      {result && (
        <div className="ls-card ls-result">
          <div className="ls-card-title">
            {result.kind === 'mclogs' ? '分享链接与求助' : 'AI 分析结果'}
            <span className="ls-prov-tag">{result.providerName}</span>
            {analysing && <span className="ls-running">分析中，可能需要几十秒…</span>}
            <span className="spacer" />
            {result.url && <a href={result.url} target="_blank" rel="noreferrer noopener">打开云端副本 ↗</a>}
          </div>

          {/* 回退说明：换了提供方一定要说清楚，不能静默换一家 */}
          {result.fallbackNote && (
            <div className="ls-fallback">
              ⚠️ {result.fallbackNote}
              {result.fallbacks && result.fallbacks.length > 0 && (
                <details>
                  <summary>为什么？</summary>
                  <ul>
                    {result.fallbacks.map((f, i) => <li key={i}><b>{f.provider}</b>：{f.error}</li>)}
                  </ul>
                </details>
              )}
            </div>
          )}

          {/* ---- mclo.gs：链接 + 求助提示 ---- */}
          {result.kind === 'mclogs' && (
            <div className="ls-help">
              <p className="ls-help-lead">
                已生成分享链接：<a className="mono" href={result.url} target="_blank" rel="noreferrer noopener">{result.url}</a>
                <button onClick={() => copy(result.url || '', '链接')}>⧉ 复制链接</button>
                {typeof result.errors === 'number' && (
                  <span className="ls-help-errors">日志里数出 <b>{result.errors}</b> 行 ERROR</span>
                )}
              </p>
              {result.notice && <p className="ls-warn">{result.notice}</p>}

              <div className="ls-help-box">
                <div className="ls-help-title">
                  接下来去哪儿问？
                  <span className="muted">把链接贴给 MC 社区、模组作者、你的服务器群或我们</span>
                </div>
                <p className="muted">
                  建议一起带上下面这段（已替你填好环境信息 —— 社区看不到你的容器与内存设置，
                  而它们经常正是崩溃原因）：
                </p>
                <pre className="ls-help-text">{result.helpText}</pre>
                <button className="primary" onClick={() => copy(result.helpText || '', '求助文本')}>
                  ⧉ 复制求助文本
                </button>
              </div>

              <p className="muted">
                提示：日志里可能含<b>玩家名、聊天内容、IP</b>，贴之前请先确认自己能接受；
                想删掉这份云端副本，点右侧记录里的「删除云端副本」（链接和删除凭据都保存在这台面板上，
                别人拿到链接也不能删）。
              </p>
            </div>
          )}

          {/* ---- AI 提供方：流式结论 ---- */}
          {result.kind !== 'mclogs' && (
            <>
              {thinking && (
                <details className="ls-thinking">
                  <summary>思考过程（{thinking.length} 字）</summary>
                  <pre>{thinking}</pre>
                </details>
              )}
              {streamErr && <div className="ls-error">分析失败：{streamErr}</div>}
              {!answer && !analysing && !streamErr && <div className="ls-empty">（没有内容）</div>}
              {answer && (
                // AI 返回 Markdown，用 MiniMarkdown 渲染（纯 React 构造、不走 innerHTML，
                // 第三方返回的文本注入不了标签 —— 这条比"渲染好看"更重要）
                <div className="ls-answer md-body"><MiniMarkdown text={answer} /></div>
              )}
              {analysing && !answer && <div className="ls-empty">正在等待分析结果…</div>}
            </>
          )}
        </div>
      )}

      {/* ---- 上传确认（每次都要手动勾选） ---- */}
      {confirmOpen && (
        <div className="modal-mask modal-mask-top" onClick={() => !busy && setConfirmOpen(false)}>
          <div className="ls-confirm" onClick={(e) => e.stopPropagation()}>
            <div className="ls-confirm-title">
              上传到 {selected?.name || '分析平台'}
              <span className="ls-prov-tag">{selected ? KIND_LABEL[selected.kind] || selected.kind : ''}</span>
            </div>

            <div className="ls-confirm-body">
              <p><b>将要上传：</b></p>
              <ul>
                <li><span className="mono">{pickedFile?.name || picked}</span>（{formatSize(pickedFile?.size || 0)}）</li>
                {selected?.kind === 'logshare' && pickedFile?.kind !== 'latest' && (
                  <li>另外会自动附带 <span className="mono">logs/latest.log</span> 与最近的崩溃报告作为上下文</li>
                )}
                {selected?.kind === 'mclogs' && (
                  <li>只上传你选中的这一份（mclo.gs 是保底通道，不做多文件附带）</li>
                )}
              </ul>

              {selected?.kind === 'logshare' && (
                <p><b>上传到哪里：</b>第三方服务 <span className="mono">api.logshare.cn</span>。
                  保留期以对方实际声明为准（界面显示实测值），也可以随时在本页手动删除云端副本。</p>
              )}
              {selected?.kind === 'mclogs' && (
                <p><b>上传到哪里：</b>第三方服务 <span className="mono">api.mclo.gs</span>
                  （由 Aternos GmbH 运营，德国）。它<b>不做 AI 分析</b>，只会给你一个
                  <b>公开可读的分享链接</b>与 ERROR 行计数 —— 用来去社区求助。</p>
              )}
              {selected?.kind === 'openai' && (
                <p><b>上传到哪里：</b>你自己配置的平台（
                  <span className="mono">{(providers?.custom || []).find((p) => p.id === selected.id)?.base_url || '（未配置）'}</span>
                  ）—— 消耗的是<b>你自己的额度</b>；日志直接发给该平台，面板不保留副本。</p>
              )}

              {selected?.kind !== 'openai' && (
                <p className="ls-warn">
                  <b>关于隐私：</b>
                  {selected?.kind === 'logshare'
                    ? '对方会自动给 IP 打码，但玩家名与聊天内容不会被过滤。'
                    : 'mclo.gs 会尽力移除 IP 等信息，但明确写着"无法保证总是有效"；玩家名与聊天内容不在其处理范围内，且链接公开可读。'}
                </p>
              )}

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
                  {selected?.kind === 'logshare' && (
                    <>
                      我已阅读并同意 LogShare 的
                      <a href={meta.termsUrl} target="_blank" rel="noreferrer noopener">《服务协议》</a>与
                      <a href={meta.privacyUrl} target="_blank" rel="noreferrer noopener">《隐私政策》</a>，
                      并确认这份日志可以上传到该第三方服务。
                    </>
                  )}
                  {selected?.kind === 'mclogs' && (
                    <>
                      我已阅读并理解
                      <a href="#" onClick={(e) => { e.preventDefault(); setNoticeOpen(true) }}>《mclo.gs 使用须知》</a>
                      （分享链接公开可读、最多保留 90 天、玩家名与聊天不会被过滤），
                      并确认这份日志可以上传。
                    </>
                  )}
                  {selected?.kind === 'openai' && (
                    <>我已确认把这份日志发给我自己配置的平台（消耗我的额度），并自行承担其隐私边界。</>
                  )}
                </span>
              </label>

              {needsMclogsNotice && (
                <p className="muted">
                  首次使用 mclo.gs 需要先看一遍使用须知（点上面的链接打开）。
                </p>
              )}

              {meta.maxBytes > 0 && (
                <p className="muted">单次上限 {formatSize(meta.maxBytes)}；超出会保留尾部（崩溃现场在后面）并在结果里注明。</p>
              )}
            </div>

            <div className="ls-confirm-ops">
              {error && <span className="ls-confirm-err">{error}</span>}
              <div className="spacer" />
              <button onClick={() => setConfirmOpen(false)} disabled={busy}>取消</button>
              <button className="primary" onClick={doAnalyse} disabled={busy || !agree || needsMclogsNotice}>
                {busy ? '处理中…' : '同意并分析'}
              </button>
            </div>
          </div>
        </div>
      )}

      {/* ---- mclo.gs 用户须知（首次使用必须确认）---- */}
      {noticeOpen && (
        <div className="modal-mask modal-mask-top" onClick={() => setNoticeOpen(false)}>
          <div className="ls-confirm" onClick={(e) => e.stopPropagation()}>
            <div className="ls-confirm-title">mclo.gs 使用须知</div>
            <div className="ls-confirm-body">
              <p className="ls-warn">
                这里是<b>日志分享</b>，不是 AI 分析。它的用途是：把日志变成一个能发出去的链接，
                让社区/模组作者/客服直接看原文 —— 这是 MC 互助沟通里的标准做法。
              </p>
              <ol className="ls-notice-list">
                <li>
                  <b>第三方是谁</b>：mclo.gs，由 <b>Aternos GmbH</b>（德国波恩）运营，
                  适用它自己的<a href="https://aternos.gmbh/en/mclogs/privacy" target="_blank" rel="noreferrer noopener">隐私政策</a>，
                  与面板没有从属关系。
                </li>
                <li>
                  <b>拿到链接的任何人都能看</b>：分享链接与原文接口都无需登录，链接本身就是凭据；
                  对方也没有承诺这些链接不会被索引或转载 —— <b>不要把含敏感信息的日志贴上去</b>。
                </li>
                <li>
                  <b>他们会处理日志，但不能保证干净</b>：对方写明"会尽力在存储前移除 IP 等信息，
                  但<b>无法保证总是有效</b>"；玩家名与聊天内容不在其处理范围内。
                  本面板会在上传前过滤玩家聊天行（默认开启），但无法保证日志里其它内容不含隐私
                  （插件输出、自定义消息、异常栈里的路径等）。
                </li>
                <li>
                  <b>保留期</b>：对方写明日志在<b>最后一次访问后最多 90 天</b>自动删除；
                  单份上限 10MB / 25000 行。
                </li>
                <li>
                  <b>删除</b>：上传时会返回一个删除凭据，面板替你保存，随时可以在本页删除云端副本；
                  凭据丢失就无法删除，只能等它自然过期。
                </li>
                <li>
                  <b>它不会给你结论</b>：只提供链接与 ERROR 行计数。要"为什么崩、怎么修"，
                  请把链接发出去问人，或改用 AI 分析（LogShare / 你自配的平台）。
                </li>
                <li>
                  <b>请求日志</b>：对方会记录网站请求（URL、IP、会话 ID、Cloudflare Ray ID、国家码）
                  最多 24 小时用于防攻击与排障；流量经 <b>Cloudflare</b>（CDN/WAF）与
                  <b>oneCorp GmbH</b>（硬件托管）处理。
                </li>
                <li>数据权利（查询/更正/删除/投诉）依对方政策直接找他们（GDPR，§77 投诉权）。</li>
              </ol>
              <label className="ls-agree">
                <input type="checkbox" checked={noticeRead}
                  onChange={(e) => {
                    setNoticeRead(e.target.checked)
                    try { localStorage.setItem(MCLOGS_NOTICE_KEY, e.target.checked ? '1' : '0') } catch { /* 隐私模式下忽略 */ }
                  }} />
                <span>我已阅读并理解以上内容（这份确认只记在这台浏览器上，不会上传）</span>
              </label>
            </div>
            <div className="ls-confirm-ops">
              <div className="spacer" />
              <button className="primary" onClick={() => setNoticeOpen(false)} disabled={!noticeRead}>
                关闭
              </button>
            </div>
          </div>
        </div>
      )}
    </div>
  )
}
