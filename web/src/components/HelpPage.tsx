import { useEffect, useState } from 'react'
import {
  Announcement, HelpDoc,
  listAnnouncements, createAnnouncement, updateAnnouncement, deleteAnnouncement,
  getHelp, saveHelp,
} from '../api'
import MiniMarkdown from './MiniMarkdown'
import './HelpPage.css'

function fmtTime(s: string): string {
  if (!s) return '—'
  const d = new Date(s)
  if (isNaN(d.getTime())) return s
  return d.toLocaleString('zh-CN', { hour12: false })
}

const PLACEHOLDER = `# 帮助文档

用简单的 Markdown 写就行：

## 怎么开机
- 进「实例管理」点「打开」
- 控制台里可以直接输指令

## 怎么开公网端口
1. 打开实例 → 「公网端口」页签
2. 点「+ 开通端口」，选线路、填本地端口（BlueMap 是 8123）

**注意**：公网端口本身不能改，要换就关掉重开。
链接可以写成 [面板地址](https://example.com)。`

/**
 * HelpPage 「公告与帮助」页。
 *
 * 公告与帮助放在同一页，因为它们都是"管理员对全体用户说话"的渠道：
 *   公告 —— 现在发生的事（维护、迁移、临时调整），会变
 *   帮助 —— 一直成立的事（怎么用），不常变
 * 分成两个导航项的话，用户想看"怎么用"时还得先猜是哪个。
 *
 * 读：所有登录用户；写：只有总管理员（后端也校验，前端这里只是不显示按钮）。
 *
 * 刻意**不做「已读/未读」**：那需要一张已读表 + 每条公告每人一条记录，
 * 而且很容易变成"红点永远消不掉"的噪音源。先只做展示。
 */
export default function HelpPage({ isAdmin }: { isAdmin: boolean }) {
  const [anns, setAnns] = useState<Announcement[]>([])
  const [doc, setDoc] = useState<HelpDoc | null>(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState('')
  const [msg, setMsg] = useState('')

  // 公告编辑态（editing=null 表示"新建"）
  const [editing, setEditing] = useState<Announcement | null>(null)
  const [showForm, setShowForm] = useState(false)
  const [fTitle, setFTitle] = useState('')
  const [fBody, setFBody] = useState('')
  const [fPinned, setFPinned] = useState(false)
  const [fPublished, setFPublished] = useState(true)
  const [busy, setBusy] = useState(false)

  // 帮助文档编辑态
  const [editingDoc, setEditingDoc] = useState(false)
  const [docDraft, setDocDraft] = useState('')

  const load = async () => {
    setLoading(true)
    const [a, h] = await Promise.allSettled([listAnnouncements(), getHelp()])
    if (a.status === 'fulfilled') setAnns(Array.isArray(a.value) ? a.value : [])
    else setError(String((a as any).reason?.message || a.reason))
    if (h.status === 'fulfilled') setDoc(h.value)
    setLoading(false)
  }

  useEffect(() => { load() }, [])

  const openNew = () => {
    setEditing(null); setFTitle(''); setFBody(''); setFPinned(false); setFPublished(true)
    setShowForm(true); setError(''); setMsg('')
  }

  const openEdit = (a: Announcement) => {
    setEditing(a); setFTitle(a.title); setFBody(a.body)
    setFPinned(a.pinned); setFPublished(a.published)
    setShowForm(true); setError(''); setMsg('')
  }

  const submitAnn = async () => {
    if (!fTitle.trim()) { setError('标题不能为空'); return }
    setBusy(true); setError(''); setMsg('')
    try {
      const payload = { title: fTitle.trim(), body: fBody, pinned: fPinned, published: fPublished }
      const r = editing
        ? await updateAnnouncement(editing.id, payload)
        : await createAnnouncement(payload)
      setMsg(r?.message || '已保存')
      setShowForm(false)
      await load()
    } catch (e: any) {
      setError(e.message)
    } finally {
      setBusy(false)
    }
  }

  const remove = async (a: Announcement) => {
    if (!confirm(`删除公告「${a.title}」？此操作不可恢复。`)) return
    setError(''); setMsg('')
    try {
      const r = await deleteAnnouncement(a.id)
      setMsg(r?.message || '已删除')
      await load()
    } catch (e: any) {
      setError(e.message)
    }
  }

  const togglePin = async (a: Announcement) => {
    setError(''); setMsg('')
    try {
      await updateAnnouncement(a.id, {
        title: a.title, body: a.body, pinned: !a.pinned, published: a.published,
      })
      await load()
    } catch (e: any) {
      setError(e.message)
    }
  }

  const saveDocContent = async () => {
    setBusy(true); setError(''); setMsg('')
    try {
      const r = await saveHelp(docDraft)
      setMsg(r?.message || '帮助文档已保存')
      setEditingDoc(false)
      await load()
    } catch (e: any) {
      setError(e.message)
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="help-page">
      {error && <div className="error-banner" onClick={() => setError('')}>{error}</div>}
      {msg && <div className="success-banner" onClick={() => setMsg('')}>{msg}</div>}

      {/* ---------------- 公告 ---------------- */}
      <section className="hp-card">
        <div className="hp-card-title">
          公告
          <span className="hp-card-sub">管理员发布，所有用户可见</span>
          {isAdmin && !showForm && (
            <button className="primary hp-right" onClick={openNew}>+ 发布公告</button>
          )}
        </div>

        {isAdmin && showForm && (
          <div className="hp-form">
            <input
              placeholder="标题（如：今晚 23:00 停机维护）"
              value={fTitle}
              onChange={(e) => setFTitle(e.target.value)}
            />
            <textarea
              placeholder="正文。可以留空 —— 只有标题的短通知也常见。"
              rows={4}
              value={fBody}
              onChange={(e) => setFBody(e.target.value)}
            />
            <div className="hp-form-row">
              <label className="switch">
                <input type="checkbox" checked={fPinned} onChange={(e) => setFPinned(e.target.checked)} />
                置顶
              </label>
              <label className="switch">
                <input type="checkbox" checked={fPublished} onChange={(e) => setFPublished(e.target.checked)} />
                发布（取消勾选 = 存草稿，普通用户看不到）
              </label>
              <div className="hp-form-actions">
                <button className="primary" onClick={submitAnn} disabled={busy}>
                  {busy ? '保存中…' : editing ? '保存修改' : '发布'}
                </button>
                <button onClick={() => setShowForm(false)} disabled={busy}>取消</button>
              </div>
            </div>
          </div>
        )}

        {loading ? (
          <div className="hp-empty">加载中…</div>
        ) : anns.length === 0 ? (
          <div className="hp-empty">
            还没有公告。
            {isAdmin ? '点右上角「+ 发布公告」写第一条。' : '有维护或调整时管理员会在这里通知。'}
          </div>
        ) : (
          <div className="hp-anns">
            {anns.map((a) => (
              <article className={`hp-ann ${a.pinned ? 'pinned' : ''} ${a.published ? '' : 'draft'}`} key={a.id}>
                <div className="hp-ann-head">
                  {a.pinned && <span className="hp-pin" title="已置顶">📌</span>}
                  <span className="hp-ann-title">{a.title}</span>
                  {!a.published && <span className="hp-draft">草稿</span>}
                  <span className="hp-ann-time">
                    {fmtTime(a.created_at)}
                    {a.updated_at && a.updated_at !== a.created_at ? ` · 改于 ${fmtTime(a.updated_at)}` : ''}
                    {a.created_by_name ? ` · ${a.created_by_name}` : ''}
                  </span>
                  {isAdmin && (
                    <span className="hp-ann-ops">
                      <button onClick={() => togglePin(a)} title={a.pinned ? '取消置顶' : '置顶'}>
                        {a.pinned ? '取消置顶' : '置顶'}
                      </button>
                      <button onClick={() => openEdit(a)}>编辑</button>
                      <button className="danger" onClick={() => remove(a)}>删除</button>
                    </span>
                  )}
                </div>
                {a.body && <div className="hp-ann-body"><MiniMarkdown text={a.body} /></div>}
              </article>
            ))}
          </div>
        )}
      </section>

      {/* ---------------- 帮助文档 ---------------- */}
      <section className="hp-card">
        <div className="hp-card-title">
          帮助文档
          <span className="hp-card-sub">
            {doc?.updated_by_name
              ? `最后更新：${fmtTime(doc.updated_at)} · ${doc.updated_by_name}`
              : '还没有内容'}
          </span>
          {isAdmin && !editingDoc && (
            <button
              className="hp-right"
              onClick={() => { setDocDraft(doc?.content || ''); setEditingDoc(true) }}
            >
              {doc?.content ? '编辑' : '+ 写帮助文档'}
            </button>
          )}
        </div>

        {editingDoc ? (
          <div className="hp-form">
            <textarea
              className="hp-doc-editor"
              rows={18}
              value={docDraft}
              onChange={(e) => setDocDraft(e.target.value)}
              placeholder={PLACEHOLDER}
              spellCheck={false}
            />
            <div className="hp-form-row">
              <span className="hp-hint">
                支持 <code># 标题</code>、<code>- 列表</code>、<code>**粗体**</code>、
                <code>`代码`</code>、<code>[文字](https://链接)</code>。
                原文存储，渲染在服务端之外完成（不执行 HTML，安全）。
              </span>
              <div className="hp-form-actions">
                <button className="primary" onClick={saveDocContent} disabled={busy}>
                  {busy ? '保存中…' : '保存'}
                </button>
                <button onClick={() => setEditingDoc(false)} disabled={busy}>取消</button>
              </div>
            </div>
          </div>
        ) : doc?.content ? (
          <MiniMarkdown text={doc.content} />
        ) : (
          <div className="hp-empty">
            {isAdmin ? '还没有写帮助文档。点右上角开始。' : '管理员还没有填写帮助文档。'}
          </div>
        )}
      </section>
    </div>
  )
}
