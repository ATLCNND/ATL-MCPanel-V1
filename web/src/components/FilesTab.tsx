import { useEffect, useMemo, useRef, useState } from 'react'
import {
  FileItem, FileJob,
  listFiles, readFile, writeFile, deleteFile, mkdir,
  renameFile, copyFile, searchFiles, downloadUrl,
  listJobs, createJob, cancelJob,
} from '../api'
import './FilesTab.css'

function formatSize(size: number): string {
  if (size < 1024) return size + ' B'
  if (size < 1024 * 1024) return (size / 1024).toFixed(1) + ' KB'
  if (size < 1024 * 1024 * 1024) return (size / 1024 / 1024).toFixed(2) + ' MB'
  return (size / 1024 / 1024 / 1024).toFixed(2) + ' GB'
}

function formatTime(ts: number): string {
  if (!ts) return '-'
  return new Date(ts * 1000).toLocaleString('zh-CN')
}

// 可在面板内直接编辑的文本类文件。
// 白名单而不是黑名单：误开一个几百 MB 的 .mca 会让浏览器直接卡死。
const TEXT_EXTS = new Set([
  'txt', 'log', 'json', 'yml', 'yaml', 'toml', 'properties', 'conf', 'cfg', 'ini',
  'sh', 'bat', 'md', 'csv', 'xml', 'html', 'js', 'ts', 'tsx', 'py', 'sql', 'env',
])

function extOf(name: string): string {
  const i = name.lastIndexOf('.')
  return i < 0 ? '' : name.slice(i + 1).toLowerCase()
}

function isTextFile(name: string): boolean {
  return TEXT_EXTS.has(extOf(name))
}

// 可解压的压缩包（按扩展名给个提示；真正的格式由服务端按文件内容判定）
const ARCHIVE_EXTS = new Set(['zip', 'gz', 'tgz', 'tar', 'jar'])

function isArchive(name: string): boolean {
  const lower = name.toLowerCase()
  return lower.endsWith('.tar.gz') || ARCHIVE_EXTS.has(extOf(name))
}

function joinPath(dir: string, name: string): string {
  return dir === '/' || dir === '' ? name : dir + '/' + name
}

function parentOf(p: string): string {
  const parts = p.split('/').filter(Boolean)
  parts.pop()
  return '/' + parts.join('/')
}

/** 剪贴板：一次只保存一项，足够覆盖「复制到别处」这个主要用法。 */
interface Clipboard {
  src: string
  name: string
  isDir: boolean
  cut: boolean
}

type DialogKind = 'mkdir' | 'rename' | 'compress' | 'extract'

interface DialogState {
  kind: DialogKind
  /** 输入框内容 */
  value: string
  /** 压缩格式（仅 compress） */
  format?: string
  /** 压缩目标所在目录提示（仅 compress / extract） */
  note?: string
}

const JOB_LABEL: Record<string, string> = { compress: '压缩', extract: '解压' }
const JOB_STATE_LABEL: Record<string, string> = {
  queued: '排队中', running: '执行中', success: '已完成', failed: '失败', canceled: '已取消',
}

export default function FilesTab({ instanceId, canWrite }: { instanceId: string; canWrite: boolean }) {
  const [path, setPath] = useState('/')
  const [files, setFiles] = useState<FileItem[]>([])
  const [error, setError] = useState('')
  const [msg, setMsg] = useState('')
  const [loading, setLoading] = useState(false)

  const [selected, setSelected] = useState<FileItem | null>(null)
  const [clip, setClip] = useState<Clipboard | null>(null)
  const [view, setView] = useState<'list' | 'grid'>('list')
  const [dialog, setDialog] = useState<DialogState | null>(null)
  const [busy, setBusy] = useState(false)

  // 搜索
  const [keyword, setKeyword] = useState('')
  const [searching, setSearching] = useState(false)
  const [results, setResults] = useState<FileItem[] | null>(null)

  // 排队任务
  const [jobs, setJobs] = useState<FileJob[]>([])
  const [showJobs, setShowJobs] = useState(false)

  // 编辑状态
  const [editPath, setEditPath] = useState<string | null>(null)
  const [editContent, setEditContent] = useState('')

  const jobsRef = useRef<FileJob[]>([])
  jobsRef.current = jobs

  const refresh = async (p: string) => {
    setLoading(true)
    setError('')
    try {
      const data = await listFiles(instanceId, p)
      setFiles(data.files)
      setPath(data.path || p)
      setSelected(null)
      setResults(null)
    } catch (e: any) {
      setError(e.message)
    } finally {
      setLoading(false)
    }
  }

  const loadJobs = async () => {
    try {
      const list = await listJobs(instanceId)
      const arr = Array.isArray(list) ? list : []
      // 有任务刚刚完成 → 刷新列表，让压缩包/解压结果立刻可见
      const before = jobsRef.current
      const finishedNow = arr.some((j) =>
        (j.state === 'success' || j.state === 'failed')
        && !before.some((b) => b.job_id === j.job_id && (b.state === 'success' || b.state === 'failed')))
      setJobs(arr)
      if (finishedNow) refresh(path)
    } catch {
      /* 任务列表拉取失败不打扰用户：它只是辅助信息 */
    }
  }

  useEffect(() => {
    refresh('/')
    loadJobs()
    // 任务进度需要跟得紧一些；文件列表本身不轮询，避免无谓的目录遍历
    const t = setInterval(loadJobs, 3000)
    return () => clearInterval(t)
  }, [instanceId])

  const openItem = async (f: FileItem) => {
    if (f.is_dir) {
      setKeyword('')
      refresh(f.path)
      return
    }
    if (!isTextFile(f.name)) {
      setError(`「${f.name}」不是文本文件，请在本地下载后查看`)
      return
    }
    try {
      const data = await readFile(instanceId, f.path)
      setEditPath(f.path)
      setEditContent(data.content)
    } catch (e: any) {
      setError(e.message)
    }
  }

  const goUp = () => {
    setKeyword('')
    refresh(parentOf(path))
  }

  const saveFile = async () => {
    if (!editPath) return
    try {
      await writeFile(instanceId, editPath, editContent)
      setEditPath(null)
      await refresh(path)
      setMsg('已保存')
    } catch (e: any) {
      setError(e.message)
    }
  }

  const removeItem = async (f: FileItem) => {
    if (!confirm(`确定删除 ${f.name}？${f.is_dir ? '（目录及其所有内容）' : ''}`)) return
    try {
      await deleteFile(instanceId, f.path)
      if (selected?.path === f.path) setSelected(null)
      await refresh(path)
    } catch (e: any) {
      setError(e.message)
    }
  }

  // ---- 对话框提交 ----

  const submitDialog = async () => {
    if (!dialog) return
    const value = dialog.value.trim()
    if (!value) return
    setBusy(true)
    setError('')
    try {
      if (dialog.kind === 'mkdir') {
        await mkdir(instanceId, joinPath(path, value))
        setMsg('目录已创建')
      } else if (dialog.kind === 'rename' && selected) {
        await renameFile(instanceId, selected.path, value)
        setMsg('已重命名')
      } else if (dialog.kind === 'compress' && selected) {
        const format = dialog.format || 'zip'
        const target = joinPath(path, value)
        const r = await createJob(instanceId, { kind: 'compress', src: selected.path, dst: target, format })
        setMsg(r.message || '压缩任务已提交')
        setShowJobs(true)
      } else if (dialog.kind === 'extract' && selected) {
        const target = value === '/' ? '' : value
        const r = await createJob(instanceId, { kind: 'extract', src: selected.path, dst: target })
        setMsg(r.message || '解压任务已提交')
        setShowJobs(true)
      }
      setDialog(null)
      if (dialog.kind === 'mkdir' || dialog.kind === 'rename') await refresh(path)
      await loadJobs()
    } catch (e: any) {
      setError(e.message)
    } finally {
      setBusy(false)
    }
  }

  const openDialog = (kind: DialogKind) => {
    if (!selected && kind !== 'mkdir') return
    setError('')
    if (kind === 'mkdir') {
      setDialog({ kind, value: '' })
    } else if (kind === 'rename' && selected) {
      setDialog({ kind, value: selected.name })
    } else if (kind === 'compress' && selected) {
      setDialog({ kind, value: selected.name + '.zip', format: 'zip', note: `将打包到 ${path}` })
    } else if (kind === 'extract' && selected) {
      const dir = parentOf(selected.path)
      setDialog({ kind, value: dir, note: '解压到该目录（留 / 表示实例根目录）' })
    }
  }

  // ---- 剪贴板操作 ----

  const doPaste = async () => {
    if (!clip) return
    const dst = joinPath(path, clip.name)
    if (dst === clip.src) {
      setError('目标目录与源目录相同，无需粘贴')
      return
    }
    setBusy(true)
    setError('')
    try {
      const r = await copyFile(instanceId, clip.src, dst, { move: clip.cut, overwrite: false })
      setMsg(r.message || '已粘贴')
      if (clip.cut) setClip(null)
      await refresh(path)
    } catch (e: any) {
      setError(e.message)
    } finally {
      setBusy(false)
    }
  }

  const doDuplicate = async (f: FileItem) => {
    setBusy(true)
    setError('')
    try {
      // 生成一个不冲突的副本名：world → world - 副本 → world - 副本 (2)
      const used = new Set(files.map((x) => x.name))
      const dot = f.is_dir ? -1 : f.name.lastIndexOf('.')
      const stem = dot > 0 ? f.name.slice(0, dot) : f.name
      const ext = dot > 0 ? f.name.slice(dot) : ''
      let name = `${stem} - 副本${ext}`
      let n = 2
      while (used.has(name)) {
        name = `${stem} - 副本 (${n})${ext}`
        n++
      }
      const r = await copyFile(instanceId, f.path, joinPath(path, name), { overwrite: false })
      setMsg(r.message || '已创建副本')
      await refresh(path)
    } catch (e: any) {
      setError(e.message)
    } finally {
      setBusy(false)
    }
  }

  const doSearch = async () => {
    const kw = keyword.trim()
    if (!kw) {
      setResults(null)
      return
    }
    setSearching(true)
    setError('')
    try {
      const data = await searchFiles(instanceId, kw, path, 200)
      setResults(data.files)
      setSelected(null)
    } catch (e: any) {
      setError(e.message)
    } finally {
      setSearching(false)
    }
  }

  const shown = results ?? files
  const activeJobs = useMemo(() => jobs.filter((j) => j.state === 'queued' || j.state === 'running'), [jobs])

  const sel = selected
  const canAct = canWrite && !!sel

  return (
    <div className="files-tab">
      {/* ---- 路径栏 ---- */}
      <div className="files-toolbar">
        <button onClick={goUp} disabled={path === '/' || path === ''} title="返回上级目录">↑</button>
        <span className="path-display mono" title={path}>{path}</span>
        <button onClick={() => refresh(path)} disabled={loading}>{loading ? '…' : '刷新'}</button>

        <div className="spacer" />

        <div className="files-search">
          <input
            placeholder="搜索当前目录及子目录…"
            value={keyword}
            onChange={(e) => setKeyword(e.target.value)}
            onKeyDown={(e) => { if (e.key === 'Enter') doSearch() }}
          />
          <button onClick={doSearch} disabled={searching || !keyword.trim()}>
            {searching ? '搜索中…' : '搜索'}
          </button>
          {results && <button onClick={() => { setResults(null); setKeyword('') }}>返回目录</button>}
        </div>

        <div className="view-toggle">
          <button className={view === 'list' ? 'active' : ''} onClick={() => setView('list')} title="列表视图">☰</button>
          <button className={view === 'grid' ? 'active' : ''} onClick={() => setView('grid')} title="网格视图">▦</button>
        </div>
      </div>

      {/* ---- 操作栏 ---- */}
      <div className="files-actions">
        <button onClick={() => sel && openItem(sel)} disabled={!sel} title="打开目录或编辑文本文件">打开</button>
        <button onClick={() => openDialog('rename')} disabled={!canAct || busy}>重命名</button>
        <button onClick={() => sel && doDuplicate(sel)} disabled={!canAct || busy}>创建副本</button>
        <button
          onClick={() => sel && setClip({ src: sel.path, name: sel.name, isDir: sel.is_dir, cut: true })}
          disabled={!canAct}
          title="剪切到剪贴板，切到目标目录后点「粘贴」"
        >
          剪切
        </button>
        <button
          onClick={() => sel && setClip({ src: sel.path, name: sel.name, isDir: sel.is_dir, cut: false })}
          disabled={!canAct}
        >
          复制
        </button>
        <button className={clip ? 'primary' : ''} onClick={doPaste} disabled={!canWrite || !clip || busy}>
          粘贴
        </button>
        <span className="act-sep" />
        <button onClick={() => sel && !sel.is_dir && (window.location.href = downloadUrl(instanceId, sel.path))}
          disabled={!sel || sel.is_dir} title="下载到本地">下载</button>
        <button onClick={() => openDialog('compress')} disabled={!canAct || busy} title="提交排队任务，由节点统一打包">压缩</button>
        <button
          onClick={() => openDialog('extract')}
          disabled={!canAct || busy || !sel || sel.is_dir || !isArchive(sel.name)}
          title={sel && !sel.is_dir && isArchive(sel.name) ? '解压到指定目录（排队任务）' : '选中一个压缩包后可解压'}
        >
          解压
        </button>
        <span className="act-sep" />
        <button onClick={() => openDialog('mkdir')} disabled={!canWrite || busy}>新建目录</button>
        <button className="danger" onClick={() => sel && removeItem(sel)} disabled={!canAct || busy}>删除</button>

        <div className="spacer" />
        <button className={showJobs ? 'primary' : ''} onClick={() => setShowJobs(!showJobs)}>
          任务{activeJobs.length > 0 ? ` (${activeJobs.length})` : ''}
        </button>
      </div>

      {/* ---- 剪贴板提示 ---- */}
      {clip && (
        <div className="clipboard-bar">
          <span className={`clip-tag ${clip.cut ? 'cut' : 'copy'}`}>{clip.cut ? '剪切' : '复制'}</span>
          <span className="mono">{clip.name}</span>
          <span className="muted">→ 打开目标目录后点「粘贴」</span>
          <div className="spacer" />
          <button onClick={() => setClip(null)}>取消</button>
        </div>
      )}

      {error && <div className="files-error">{error}</div>}
      {msg && <div className="files-msg">{msg}</div>}

      {/* ---- 排队任务面板 ---- */}
      {showJobs && (
        <div className="jobs-panel">
          <div className="jobs-head">
            <strong>排队任务</strong>
            <span className="muted">
              压缩 / 解压由节点统一串行执行，避免多个实例同时抢磁盘 IO
            </span>
            <div className="spacer" />
            <button onClick={loadJobs}>刷新</button>
          </div>
          {jobs.length === 0 ? (
            <div className="jobs-empty">暂无任务</div>
          ) : (
            <div className="jobs-list">
              {jobs.slice(0, 8).map((j) => (
                <div className={`job-row state-${j.state}`} key={j.job_id}>
                  <span className="job-kind">{JOB_LABEL[j.kind] || j.kind}</span>
                  <span className="job-src mono" title={j.src}>{j.src}</span>
                  <span className="job-arrow">→</span>
                  <span className="job-dst mono" title={j.dst}>{j.dst}</span>
                  <div className="job-progress">
                    <div className="bar"><div className="fill" style={{ width: `${j.progress}%` }} /></div>
                    <span className="job-state">
                      {JOB_STATE_LABEL[j.state] || j.state}
                      {j.state === 'running' && ` ${j.progress}%`}
                    </span>
                  </div>
                  <span className="job-msg" title={j.error || j.message}>{j.error || j.message}</span>
                  {(j.state === 'queued' || j.state === 'running') && canWrite && (
                    <button className="danger" onClick={() => cancelJob(j.job_id).then(loadJobs).catch((e) => setError(e.message))}>
                      取消
                    </button>
                  )}
                </div>
              ))}
            </div>
          )}
        </div>
      )}

      {/* ---- 文件列表 ---- */}
      <div className={`files-list ${view}`}>
        {view === 'list' ? (
          <table>
            <thead>
              <tr>
                <th>名称</th>
                <th>大小</th>
                <th>修改时间</th>
              </tr>
            </thead>
            <tbody>
              {shown.map((f) => (
                <tr
                  key={f.path}
                  className={sel?.path === f.path ? 'selected' : ''}
                  onClick={() => setSelected(f)}
                  onDoubleClick={() => openItem(f)}
                >
                  <td>
                    <span className="file-row">
                      <span className="file-icon">{f.is_dir ? '📁' : isArchive(f.name) ? '🗜️' : '📄'}</span>
                      <span className="file-name">{f.name}</span>
                    </span>
                  </td>
                  <td className="mono">{f.is_dir ? '—' : formatSize(f.size)}</td>
                  <td className="mono">{formatTime(f.mod_time)}</td>
                </tr>
              ))}
              {shown.length === 0 && (
                <tr>
                  <td colSpan={3} className="empty">
                    {results ? '没有匹配的文件' : loading ? '加载中…' : '空目录'}
                  </td>
                </tr>
              )}
            </tbody>
          </table>
        ) : (
          <div className="grid-wrap">
            {shown.map((f) => (
              <div
                key={f.path}
                className={`grid-card ${sel?.path === f.path ? 'selected' : ''}`}
                onClick={() => setSelected(f)}
                onDoubleClick={() => openItem(f)}
                title={f.path}
              >
                <div className="grid-icon">{f.is_dir ? '📁' : isArchive(f.name) ? '🗜️' : '📄'}</div>
                <div className="grid-name">{f.name}</div>
                <div className="grid-meta mono">{f.is_dir ? '目录' : formatSize(f.size)}</div>
              </div>
            ))}
            {shown.length === 0 && (
              <div className="empty">{results ? '没有匹配的文件' : loading ? '加载中…' : '空目录'}</div>
            )}
          </div>
        )}
      </div>

      {/* ---- 双击 / 打开 的说明 ---- */}
      <div className="files-tip">
        单击选中，双击打开目录或编辑文本文件。
        压缩 / 解压会提交到<strong>节点任务队列</strong>，可在上方「任务」中查看进度与取消；
        大文件请用「下载」直接取回本地。
      </div>

      {/* ---- 输入对话框 ---- */}
      {dialog && (
        <div className="modal-mask" onClick={() => !busy && setDialog(null)}>
          <div className="files-dialog" onClick={(e) => e.stopPropagation()}>
            <div className="dlg-title">
              {dialog.kind === 'mkdir' && '新建目录'}
              {dialog.kind === 'rename' && `重命名「${sel?.name}」`}
              {dialog.kind === 'compress' && `压缩「${sel?.name}」`}
              {dialog.kind === 'extract' && `解压「${sel?.name}」`}
            </div>

            {dialog.kind === 'compress' && (
              <label className="dlg-field">
                <span>压缩格式</span>
                <select
                  value={dialog.format}
                  onChange={(e) => {
                    const fmt = e.target.value
                    const ext = fmt === 'tar.gz' ? '.tar.gz' : '.' + fmt
                    const stem = (sel?.name || '').replace(/\.(zip|tar|tar\.gz|tgz)$/i, '')
                    setDialog({ ...dialog, format: fmt, value: stem + ext })
                  }}
                >
                  <option value="zip">zip（兼容性最好，Windows 可直接打开）</option>
                  <option value="tar.gz">tar.gz（Linux 原生，压缩率略高）</option>
                  <option value="tar">tar（不压缩，速度最快）</option>
                </select>
              </label>
            )}

            <label className="dlg-field">
              <span>
                {dialog.kind === 'mkdir' && '目录名'}
                {dialog.kind === 'rename' && '新名称'}
                {dialog.kind === 'compress' && '压缩包文件名'}
                {dialog.kind === 'extract' && '解压到目录'}
              </span>
              <input
                autoFocus
                value={dialog.value}
                onChange={(e) => setDialog({ ...dialog, value: e.target.value })}
                onKeyDown={(e) => {
                  if (e.key === 'Enter') submitDialog()
                  if (e.key === 'Escape') setDialog(null)
                }}
                spellCheck={false}
              />
            </label>

            <div className="dlg-note">
              {dialog.note}
              {dialog.kind === 'extract' && '。格式由服务端按文件内容自动识别。'}
              {(dialog.kind === 'compress' || dialog.kind === 'extract') && ' 该操作会作为排队任务执行。'}
            </div>

            <div className="dlg-actions">
              <button onClick={() => setDialog(null)} disabled={busy}>取消</button>
              <button className="primary" onClick={submitDialog} disabled={busy || !dialog.value.trim()}>
                {busy ? '提交中…' : dialog.kind === 'compress' || dialog.kind === 'extract' ? '提交任务' : '确定'}
              </button>
            </div>
          </div>
        </div>
      )}

      {/* ---- 文本编辑器 ---- */}
      {editPath && (
        <div className="editor-overlay">
          <div className="editor">
            <div className="editor-header">
              <span>编辑 - {editPath}</span>
              <div>
                <button className="primary" onClick={saveFile}>保存</button>
                <button onClick={() => setEditPath(null)}>取消</button>
              </div>
            </div>
            <textarea
              value={editContent}
              onChange={(e) => setEditContent(e.target.value)}
              spellCheck={false}
            />
          </div>
        </div>
      )}
    </div>
  )
}
