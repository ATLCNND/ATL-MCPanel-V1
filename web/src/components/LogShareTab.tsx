import { useEffect, useMemo, useRef, useState } from 'react'
import {
  LogShareFile, AnalysisProvidersResp, AnalysisRecord, AnalysisKind,
  listLogShareFiles, listAnalysisProviders, analyseInstance, deleteAnalysisRecord,
  streamAnalysisAI, listAnalysisHistory, previewHelpText,
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
  // 自配平台那条路没有云端副本，落库时 Expires 是 Go 的零值，
  // 序列化出来就是 0001-01-01 —— 直接显示会变成"约 -74 万天后"，属于典型的描述误区。
  if (d.getFullYear() < 2000) return '—'
  const days = Math.ceil((d.getTime() - Date.now()) / 86400000)
  return `${d.toLocaleDateString('zh-CN')}（约 ${days} 天后）`
}

/** 只取站点域名，用于按钮文案（避免把地址写死成 logshare.cn —— 自建/代理时就不对了） */
function siteLabel(url: string): string {
  try { return new URL(url).hostname || '对方站点' } catch { return '对方站点' }
}

/**
 * 提供方在界面上的显示名：名称与类型说明**只出现一次**。
 *
 * 为什么需要它（用户报的"描述误区"就在这里）：内置提供方的名字叫 `LogShare`，
 * 而 KIND_LABEL 是 `LogShare（AI 分析）` —— 直接拼成 `${name}（${label}）` 会得到
 * **`LogShare（LogShare（AI 分析））`**。
 *
 * 上一版想用"名称里已经含类型说明就跳过"来防重复，方向搞反了：
 * 短名字（LogShare）当然不包含长标签（LogShare（AI 分析）），条件永远不成立，
 * 于是重复照旧 —— 真机上打开下拉一眼就能看到。
 * 正确的判断是**标签里有没有已经包含这个名字**。
 */
function providerLabel(name: string, kind: string): string {
  const label = KIND_LABEL[kind]
  if (!label) return name || kind
  if (!name) return label
  if (label.includes(name)) return label // 「LogShare（AI 分析）」已含名字
  if (name.includes(label)) return name  // 名字里已经写全
  return `${name}（${label}）`
}

/** 类型标签的短文案：挂在名字后面当胶囊用，不含名字本身（见 providerLabel）。 */
const KIND_SHORT: Record<string, string> = {
  logshare: 'AI 分析',
  mclogs: '分享链接',
  openai: '自配平台',
  'builtin-rules': '内置规则',
}

/**
 * 把 LogShare 的 status 事件翻译成人话。
 *
 * 对方在真正开始分析前会发 `{"type":"queued","position":N}` ——
 * 不显示它的话，用户看到的就是一个转了很久的圈，很容易理解成
 * "面板卡住了 / 调用又出问题了"（2026-09-30 用户就是这么报上来的）。
 */
function describeStatus(raw: string): string {
  try {
    const v = JSON.parse(raw)
    const type = typeof v === 'string' ? v : v?.type
    if (type === 'queued') {
      const pos = v?.position
      return typeof pos === 'number'
        ? `已进入 LogShare 的分析队列，前面还有 ${pos} 个任务（免费公益服务，高峰期需要排队）`
        : '已进入 LogShare 的分析队列，正在排队'
    }
    if (type === 'cached') return '' // 回放已缓存的结论，不需要提示
    return type ? `LogShare：${type}` : ''
  } catch {
    return ''
  }
}

/** 这条错误是不是"对方 AI 队列满了"（免费公益服务的限流，不是面板坏了）。 */
function isQueueFullError(msg: string): boolean {
  return /429|队列已满|queue is full|rate limit/i.test(msg)
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
  // 选中的提供方：**三态**。内置的 LogShare / mclo.gs 没有数据库行、id 都是 0，
  // 只靠 id 无法与"自动"区分（下拉里会撞值，选了等于没选），所以内置的用 kind 指定。
  const [pick, setPick] = useState<{ kind: string; id: number }>({ kind: '', id: 0 })
  const pickValue = pick.id > 0 ? `id:${pick.id}` : pick.kind ? `kind:${pick.kind}` : 'auto'
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
  // 确认弹窗里的求助文本预览（见 previewHelpText：模板可由管理员改，用户得先看到成品）
  const [helpPreview, setHelpPreview] = useState('')
  const [previewErr, setPreviewErr] = useState('')
  /** LogShare 的排队提示（对方会发 `{"type":"queued","position":N}`） */
  const [queueNote, setQueueNote] = useState('')
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
  /** 选中的提供方（决定确认弹窗里写哪家的条款；自动时取链上第一家） */
  const selected = useMemo(() => {
    if (pick.id > 0) return chain.find((c) => c.id === pick.id) || chain[0] || null
    if (pick.kind) return chain.find((c) => c.kind === pick.kind) || chain[0] || null
    return chain[0] || null
  }, [pick, chain])

  const openConfirm = () => {
    setError(''); setMsg('')
    if (!picked) { setError('请先选择要分析的日志文件'); return }
    setAgree(false)
    setConfirmOpen(true)
  }

  /**
   * 确认"已读 mclo.gs 使用须知"。
   *
   * 它**顺带把上传确认也勾上**：用户刚刚读完须知、点的是"我已阅读并理解"，
   * 那就是同一件事的两半。此前这是两个互不相干的勾选（一个存 localStorage、
   * 一个是弹窗里的复选框），两个都满足按钮才亮 —— 用户只勾了一个，
   * 看到的就是"同意须知了还是灰的点不动"，而且界面**不说为什么**。
   * 那个"这份日志可以上传"的勾选框仍然留着（仍然可以取消），只是不再需要他勾第二次。
   */
  const confirmNotice = () => {
    setNoticeRead(true)
    try { localStorage.setItem(MCLOGS_NOTICE_KEY, '1') } catch { /* 隐私模式下忽略 */ }
    setAgree(true)
    setNoticeOpen(false)
  }

  // 选中 mclo.gs 且还没确认过须知时，**自动把须知弹出来** ——
  // 让"必读"变成流程里的一步，而不是一个用户可能永远没注意到的链接。
  useEffect(() => {
    if (confirmOpen && selected?.kind === 'mclogs' && !noticeRead) setNoticeOpen(true)
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [confirmOpen, selected?.kind, noticeRead])

  // 确认弹窗里预生成的求助文本（只在选中 mclo.gs 时取；改「现象」会重新渲染）。
  // 取不到不影响上传：这只是预览。
  useEffect(() => {
    if (!confirmOpen || selected?.kind !== 'mclogs') { setHelpPreview(''); setPreviewErr(''); return }
    let alive = true
    const t = setTimeout(() => {
      previewHelpText(instanceId, phenomenon)
        .then((r) => { if (alive) { setHelpPreview(r.text); setPreviewErr('') } })
        .catch((e: any) => { if (alive) setPreviewErr(e?.message || String(e)) })
    }, 250) // 打字防抖：改「现象」时不要每敲一个字打一次
    return () => { alive = false; clearTimeout(t) }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [confirmOpen, selected?.kind, phenomenon, instanceId])

  const doAnalyse = async () => {
    if (!agree) { setError('请先勾选同意'); return }
    setBusy(true); setError(''); setMsg('')
    try {
      const r = await analyseInstance(instanceId, {
        path: picked, filterChat, agree, providerId: pick.id, providerKind: pick.kind, phenomenon,
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
    setQueueNote('')
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
      else if (ev.event === 'status') setQueueNote(describeStatus(ev.data))
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
  /** 灰按钮的原因（没有原因就不显示）。空串 = 可以点。 */
  const disabledReason = needsMclogsNotice
    ? '请先阅读《mclo.gs 使用须知》并点「我已阅读并理解」'
    : !agree
      ? '请勾选下方的确认项'
      : ''

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
                  <b>{providerLabel(c.name, c.kind)}</b>
                </span>
              ))}
            。<b>LogShare</b> 给 AI 结论；<b>mclo.gs</b> 不做 AI，只把日志变成可分享的链接（用于去社区求助）。
          </span>
        </div>
        <a className="ls-site-btn" href={meta.siteUrl} target="_blank" rel="noreferrer noopener">
          访问 {siteLabel(meta.siteUrl)} ↗
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
              <select value={pickValue}
                onChange={(e) => {
                  // 值形如 "auto" / "kind:mclogs" / "id:3"（见 pick 的注释）
                  const v = e.target.value
                  if (v === 'auto') { setPick({ kind: '', id: 0 }); return }
                  const [t, raw] = v.split(':')
                  setPick(t === 'kind' ? { kind: raw, id: 0 } : { kind: '', id: Number(raw) })
                }}>
                <option value="auto">自动（按顺序尝试，推荐）</option>
                {chain.map((c) => (
                  <option key={`${c.kind}-${c.id}`} value={c.id > 0 ? `id:${c.id}` : `kind:${c.kind}`}>
                    {providerLabel(c.name, c.kind)}
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

          {/* 「现象」是给求助文本用的：社区看不到你的控制台，但"崩之前发生了什么"往往最关键。
              默认留空（模板里 {phenomenon} 那一行会自动消失）—— 预填一句假的"现象"
              比留空更糟：用户不编辑就会把不属于他的描述贴到社区去。右边会实时预览成品。 */}
          <label className="ls-phenomenon">
            现象（可选，会写进求助文本）：
            <input value={phenomenon} maxLength={400}
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
                    {h.provider_kind === 'mclogs' && <>分享链接保留至 {fmtExpire(h.expires_at)}</>}
                    {h.provider_kind === 'logshare' && <>云端保留至 {fmtExpire(h.expires_at)}</>}
                    {/* 自配平台：日志发给了对方，但对方不替我们存副本，这条记录只在面板里 */}
                    {h.provider_kind === 'openai' && <>日志已发给自配平台，结论仅保存在本面板</>}
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
              {streamErr && (
                <div className="ls-error">
                  分析失败：{streamErr}
                  {/* "AI 队列已满"是**对方**的限流（LogShare 是免费公益服务），
                      不说清楚的话，用户只会认为"面板的 logshare 调用又坏了"。
                      同时给两个出路：重试（它是暂时的），或改走 mclo.gs 拿分享链接。 */}
                  {isQueueFullError(streamErr) && (
                    <div className="ls-error-hint">
                      这是 LogShare 侧的限流（免费公益服务，高峰期 AI 队列会满），与面板无关。
                      等一会儿点「重试分析」通常就好了；也可以改用 mclo.gs 先拿到分享链接去社区求助。
                    </div>
                  )}
                  <div className="ls-error-ops">
                    <button onClick={() => startStream(result.recordId)} disabled={analysing}>重试分析</button>
                    {isQueueFullError(streamErr) && (
                      <button onClick={() => {
                        setPick({ kind: 'mclogs', id: 0 })
                        setNoticeOpen(false)
                        openConfirm()
                      }}>改用 mclo.gs 生成分享链接</button>
                    )}
                  </div>
                </div>
              )}
              {!answer && !analysing && !streamErr && <div className="ls-empty">（没有内容）</div>}
              {answer && (
                // AI 返回 Markdown，用 MiniMarkdown 渲染（纯 React 构造、不走 innerHTML，
                // 第三方返回的文本注入不了标签 —— 这条比"渲染好看"更重要）
                <div className="ls-answer md-body"><MiniMarkdown text={answer} /></div>
              )}
              {analysing && !answer && (
                <div className="ls-empty">{queueNote || '正在等待分析结果…'}</div>
              )}
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
              {/* 胶囊只放**类型**（"AI 分析"/"分享链接"），不重复名字 ——
                  以前这里挂的是 KIND_LABEL，于是标题读作
                  "上传到 LogShareLogShare（AI 分析）"。 */}
              {selected && (
                <span className="ls-prov-tag">{KIND_SHORT[selected.kind] || selected.kind}</span>
              )}
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

              {/* mclo.gs 的"必读须知"单独一行、单独一个按钮 ——
                  以前它是个塞在同意勾选框里的链接，点它既可能顺手把勾选框切掉、
                  又和下面那个勾选框的语义重叠（用户反馈：同意了须知，按钮还是灰的点不动）。 */}
              {selected?.kind === 'mclogs' && (
                <p className="ls-notice-row">
                  <button type="button" className="ls-link-btn" onClick={() => setNoticeOpen(true)}>
                    {noticeRead ? '再看一遍《mclo.gs 使用须知》' : '阅读《mclo.gs 使用须知》（首次必读）'}
                  </button>
                  {noticeRead
                    ? <span className="muted">已确认过（记在这台浏览器上）</span>
                    : <span className="muted">未确认前无法上传</span>}
                </p>
              )}

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
                      我已确认这份日志<b>可以上传到 mclo.gs</b>
                      （分享链接公开可读、最多保留 90 天，玩家名与聊天内容不会被过滤）。
                    </>
                  )}
                  {selected?.kind === 'openai' && (
                    <>我已确认把这份日志发给我自己配置的平台（消耗我的额度），并自行承担其隐私边界。</>
                  )}
                </span>
              </label>

              {/* 上限数字来自 LogShare 自己的 meta 接口，只有真的会走 LogShare 时才显示，
                  否则选 mclo.gs / 自配平台时会看到一个跟自己无关的 16MB。 */}
              {meta.maxBytes > 0 && selected?.kind === 'logshare' && (
                <p className="muted">
                  LogShare 单次上限 {formatSize(meta.maxBytes)}；超出会保留尾部（崩溃现场在后面）并在结果里注明。
                </p>
              )}
              {selected?.kind === 'mclogs' && (
                <p className="muted">
                  mclo.gs 单次上限 10 MB / 25000 行；超出会保留尾部并在结果里注明。
                </p>
              )}

              {/* 走 mclo.gs 时把**渲染后的求助文本**摆出来：模板可以由管理员改，
                  只给模板原文（一堆 {占位符}）用户不知道自己最后会贴出去什么。 */}
              {selected?.kind === 'mclogs' && (
                <div className="ls-help-preview">
                  <div className="ls-help-title">
                    将生成的求助文本
                    <span className="muted">上传后链接会自动填进去，可直接复制去社区提问</span>
                  </div>
                  <pre className="ls-help-text">{helpPreview || '（正在生成预览…）'}</pre>
                  {previewErr && <p className="muted">预览生成失败：{previewErr}（不影响上传）</p>}
                </div>
              )}
            </div>

            <div className="ls-confirm-ops">
              {error && <span className="ls-confirm-err">{error}</span>}
              {/* 灰按钮必须自己说明"为什么不能点"：只把按钮置灰、不给原因，
                  用户只会得出"这个功能坏了"（这正是本轮反馈的由来）。 */}
              {!error && disabledReason && <span className="ls-confirm-hint">{disabledReason}</span>}
              <div className="spacer" />
              <button onClick={() => setConfirmOpen(false)} disabled={busy}>取消</button>
              <button className="primary" onClick={doAnalyse} disabled={busy || !!disabledReason}>
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
              <p className="muted">
                点下面的按钮即表示你已阅读并理解以上内容。
                这份确认只记在**这台浏览器**上（不上传服务端、也不代表其他人同意过）。
              </p>
            </div>
            <div className="ls-confirm-ops">
              <div className="spacer" />
              <button onClick={() => setNoticeOpen(false)}>取消</button>
              <button className="primary" onClick={confirmNotice}>我已阅读并理解</button>
            </div>
          </div>
        </div>
      )}
    </div>
  )
}
