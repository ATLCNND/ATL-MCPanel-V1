import { useEffect, useMemo, useRef, useState } from 'react'
import {
  FileItem, FileJob,
  listFiles, readFile, writeFile, deleteFile, mkdir,
  renameFile, copyFile, searchFiles, downloadUrl, fetchFileBlob,
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

// 可在页面内预览的图片（走 blob 预览，顺带避免把令牌放进 URL）
const IMAGE_EXTS = new Set(['png', 'jpg', 'jpeg', 'gif', 'webp', 'bmp', 'ico'])

function extOf(name: string): string {
  const i = name.lastIndexOf('.')
  return i < 0 ? '' : name.slice(i + 1).toLowerCase()
}

function isTextFile(name: string): boolean {
  return TEXT_EXTS.has(extOf(name))
}

function isImageFile(name: string): boolean {
  return IMAGE_EXTS.has(extOf(name))
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

/**
 * 剪贴板：可以同时拿着多项（多选后剪切/复制）。
 *
 * 早先只能拿一项 —— 批量整理目录（"把这一堆插件挪到 plugins/"）时
 * 必须一个一个来，这正是用户要求优化文件交互的原因之一。
 */
interface ClipItem {
  src: string
  name: string
  isDir: boolean
}

interface Clipboard {
  items: ClipItem[]
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
  // 勾选集合（多选）。与 selected 分开：selected 是"操作焦点"（重命名/压缩/解压
  // 这类只能对一项做的操作用它），checked 是"批量对象"。
  // 点行 → 同时设为焦点与勾选（这样"点一下再点删除"仍然符合直觉）；
  // Ctrl/Cmd 点或勾选框 → 只改勾选，不动焦点。
  const [checked, setChecked] = useState<Set<string>>(new Set())
  const [clip, setClip] = useState<Clipboard | null>(null)
  const [view, setView] = useState<'list' | 'grid'>('list')
  const [dialog, setDialog] = useState<DialogState | null>(null)
  const [busy, setBusy] = useState(false)
  // 图片预览（blob 临时地址；用完必须 revoke）
  const [preview, setPreview] = useState<{ name: string; url: string; size: number } | null>(null)

  // 全选框的 indeterminate（部分选中）只能通过 DOM 属性设置
  const allBoxRef = useRef<HTMLInputElement>(null)
  const listRef = useRef<HTMLDivElement>(null)

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
      // 换目录后清空勾选：留着上一层的勾选会让人以为"选了东西"，
      // 而批量操作此时的目标已经不存在了
      setChecked(new Set())
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

  /**
   * 双击打开（也用于「打开」按钮与回车键）。
   *
   * 按文件类型分派 —— 双击是最高频的动作，逐类给出"最可能想要的结果"：
   *   目录      → 进入
   *   文本文件  → 面板内编辑（白名单，避免误开 .mca 这类大二进制把浏览器卡死）
   *   图片      → 页面内预览（blob，令牌不进 URL）
   *   压缩包    → 问一句是否解压到当前目录（原来要"选中→点解压→填目录"三步）
   *   核心 jar  → 不做任何事并提示去「核心」页（避免手滑换掉核心）
   *   其它      → 直接下载
   */
  const openItem = async (f: FileItem) => {
    if (f.is_dir) {
      setKeyword('')
      refresh(f.path)
      return
    }
    if (isTextFile(f.name)) {
      try {
        const data = await readFile(instanceId, f.path)
        setEditPath(f.path)
        setEditContent(data.content)
      } catch (e: any) {
        setError(e.message)
      }
      return
    }
    if (isImageFile(f.name)) {
      try {
        const blob = await fetchFileBlob(instanceId, f.path)
        // 前一个预览的临时地址要释放，否则每次预览都漏一块内存
        if (preview) URL.revokeObjectURL(preview.url)
        setPreview({ name: f.name, url: URL.createObjectURL(blob), size: blob.size })
      } catch (e: any) {
        setError(e.message)
      }
      return
    }
    if (isArchive(f.name)) {
      // .jar 例外：它多半是服务端核心或库文件，直接解压出来没有意义，
      // 还可能让用户以为"核心被我弄坏了"。给一句准确的指引而不是静默不动。
      if (extOf(f.name) === 'jar') {
        setError(`「${f.name}」是 jar 包，不直接打开。如果它是服务端核心，请到「核心」页切换；` +
          `如果只是想看里面的内容，用工具栏的「解压」（会解压到指定目录）。`)
        return
      }
      if (!canWrite) {
        setError('解压需要 owner 及以上权限')
        return
      }
      if (!confirm(`解压「${f.name}」到当前目录 ${path}？`)) return
      try {
        const r = await createJob(instanceId, { kind: 'extract', src: f.path, dst: path === '/' ? '' : path })
        setMsg(r.message || '解压任务已提交')
        setShowJobs(true)
        await loadJobs()
      } catch (e: any) {
        setError(e.message)
      }
      return
    }
    // 其它二进制：直接下载比弹一句"请在本地下载后查看"更省事
    window.location.href = downloadUrl(instanceId, f.path)
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
      setChecked((prev) => {
        const next = new Set(prev)
        next.delete(f.path)
        return next
      })
      await refresh(path)
    } catch (e: any) {
      setError(e.message)
    }
  }

  // ---- 多选 / 全选 ----

  const toggleCheck = (f: FileItem) => {
    setChecked((prev) => {
      const next = new Set(prev)
      if (next.has(f.path)) next.delete(f.path)
      else next.add(f.path)
      return next
    })
  }

  /**
   * 全选 / 全不选。
   *
   * 范围**只限当前目录的可见条目**，并且**搜索模式下不提供全选**（见 selectable）：
   * 搜索结果是跨目录的，界面上看不全"到底会删掉哪些"，
   * 一个全选 + 删除就可能删到别的目录里的同名文件 —— 这类误操作代价太大。
   */
  const toggleAll = () => {
    if (allChecked) {
      setChecked(new Set())
      return
    }
    setChecked(new Set(selectable.map((f) => f.path)))
  }

  const clipboardItems = (items: FileItem[], cut: boolean) => {
    if (!items.length) return
    setClip({ items: items.map((f) => ({ src: f.path, name: f.name, isDir: f.is_dir })), cut })
    setChecked(new Set())
    setSelected(null)
    setMsg(`已${cut ? '剪切' : '复制'} ${items.length} 项，切到目标目录后点「粘贴」`)
  }

  /** 批量删除：确认框里列出**数量与目录数**，而不是只问一句"确定吗"。 */
  const removeChecked = async () => {
    const targets = checkedFiles
    if (!targets.length) return
    const dirs = targets.filter((f) => f.is_dir).length
    const files = targets.length - dirs
    const sample = targets.slice(0, 5).map((f) => f.name).join('、')
    const more = targets.length > 5 ? ` 等 ${targets.length} 项` : ''
    if (!confirm(
      `确定删除以下 ${targets.length} 项？\n\n${sample}${more}\n\n` +
      `其中目录 ${dirs} 个、文件 ${files} 个${dirs > 0 ? '（目录会连同其全部内容一起删除）' : ''}\n` +
      `此操作不可撤销。`)) return
    setBusy(true); setError(''); setMsg('')
    const failed: string[] = []
    for (const f of targets) {
      try {
        await deleteFile(instanceId, f.path)
      } catch (e: any) {
        failed.push(`${f.name}: ${e.message}`)
      }
    }
    setBusy(false)
    setChecked(new Set())
    setSelected(null)
    await refresh(path)
    if (failed.length) {
      // 逐个失败原因都要说：批量操作里"有一部分没成功"是最容易被忽略的状态
      setError(`${targets.length - failed.length} 项已删除，${failed.length} 项失败：\n${failed.join('\n')}`)
    } else {
      setMsg(`已删除 ${targets.length} 项`)
    }
  }

  /**
   * 批量下载。
   *
   * 浏览器对"一次触发多个下载"会拦（Chrome 会问是否允许），所以这里**顺序触发**
   * 并明确提示可能需要在地址栏允许 —— 与其静默只下到第一个，不如说清楚。
   */
  const downloadChecked = () => {
    const targets = checkedFiles.filter((f) => !f.is_dir)
    const dirs = checkedFiles.length - targets.length
    if (!targets.length) {
      setError(dirs > 0 ? '所选项目都是目录，无法下载（可先压缩）' : '没有可下载的文件')
      return
    }
    targets.forEach((f, i) => {
      // 稍作间隔：连续同步触发多个下载更容易被浏览器判定为滥用
      setTimeout(() => { window.location.href = downloadUrl(instanceId, f.path) }, i * 400)
    })
    setMsg(targets.length === 1
      ? '已开始下载'
      : `已开始下载 ${targets.length} 个文件${dirs > 0 ? `（另有 ${dirs} 个目录已跳过）` : ''}；` +
        '浏览器可能会询问是否允许多个下载')
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
    if (!clip || !clip.items.length) return
    setBusy(true)
    setError('')
    const failed: string[] = []
    let okCount = 0
    let skipped = 0
    for (const it of clip.items) {
      const dst = joinPath(path, it.name)
      if (dst === it.src) { skipped++; continue } // 贴回原目录：跳过而不是报错
      try {
        await copyFile(instanceId, it.src, dst, { move: clip.cut, overwrite: false })
        okCount++
      } catch (e: any) {
        failed.push(`${it.name}: ${e.message}`)
      }
    }
    setBusy(false)
    if (clip.cut) setClip(null)
    await refresh(path)
    // 逐项报告结果：批量移动最怕"看着像成功、其实有一部分没动"
    const parts = [`已${clip.cut ? '移动' : '复制'} ${okCount} 项`]
    if (skipped) parts.push(`${skipped} 项已在目标目录（跳过）`)
    if (failed.length) parts.push(`${failed.length} 项失败：\n${failed.join('\n')}`)
    if (failed.length || skipped) setError(parts.join('；'))
    else setMsg(parts.join('；'))
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

  // ---- 多选派生状态 ----
  // 全选的**范围**：只在目录视图下启用；搜索结果是跨目录的，界面上看不全
  // "到底会选中哪些"，一个全选 + 删除就可能删到别处 —— 所以搜索模式下
  // 只能逐项勾选（选择框照常可用，只是没有"全选"）。
  const selectable = results ? [] : files
  const checkedFiles = shown.filter((f) => checked.has(f.path))
  const allChecked = selectable.length > 0 && checkedFiles.length === selectable.length
  const someChecked = checkedFiles.length > 0 && !allChecked

  useEffect(() => {
    if (allBoxRef.current) allBoxRef.current.indeterminate = someChecked
  }, [someChecked, allChecked])

  /**
   * 键盘快捷键（挂在文件列表容器上，容器可聚焦）。
   *
   * 输入框里打字不触发：`e.target` 是 INPUT/TEXTAREA/SELECT 时直接返回，
   * 否则在重命名输入框里按 Delete 会删文件。
   */
  const onKeyDown = (e: React.KeyboardEvent) => {
    const tag = (e.target as HTMLElement)?.tagName
    if (tag === 'INPUT' || tag === 'TEXTAREA' || tag === 'SELECT') return
    if (e.key === 'a' && (e.ctrlKey || e.metaKey)) {
      e.preventDefault()
      toggleAll()
      return
    }
    if (e.key === 'Delete') {
      e.preventDefault()
      if (checkedFiles.length && canWrite) removeChecked()
      else if (sel && canWrite) removeItem(sel)
      return
    }
    if (e.key === 'Enter') {
      e.preventDefault()
      if (sel) openItem(sel)
      return
    }
    if (e.key === 'Escape') {
      setChecked(new Set())
      setSelected(null)
    }
  }

  /** 点行：普通点击 = 选中并勾选它（符合"点一下再说"的直觉）；Ctrl/Cmd/Shift 点击 = 只切换勾选 */
  const onRowClick = (f: FileItem, e: React.MouseEvent) => {
    if (e.ctrlKey || e.metaKey || e.shiftKey) {
      toggleCheck(f)
      return
    }
    setSelected(f)
    setChecked(new Set([f.path]))
  }

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

      {/* ---- 批量操作条：有勾选时才出现，避免平时占地方 ---- */}
      {checkedFiles.length > 0 && (
        <div className="files-bulk">
          <span className="bulk-count">已选 <b>{checkedFiles.length}</b> 项</span>
          {canWrite && (
            <>
              <button onClick={() => clipboardItems(checkedFiles, false)} disabled={busy}>复制</button>
              <button onClick={() => clipboardItems(checkedFiles, true)} disabled={busy}>剪切</button>
              <button className="danger" onClick={removeChecked} disabled={busy}>删除</button>
            </>
          )}
          <button onClick={downloadChecked} disabled={busy || !checkedFiles.some((f) => !f.is_dir)}>下载</button>
          <div className="spacer" />
          <button onClick={() => setChecked(new Set())}>取消选择</button>
        </div>
      )}

      {/* ---- 操作栏 ---- */}
      <div className="files-actions">
        <button onClick={() => sel && openItem(sel)} disabled={!sel} title="打开目录 / 编辑文本 / 预览图片 / 解压压缩包（也可双击）">打开</button>
        <button onClick={() => openDialog('rename')} disabled={!canAct || busy}>重命名</button>
        <button onClick={() => sel && doDuplicate(sel)} disabled={!canAct || busy}>创建副本</button>
        <button
          onClick={() => sel && clipboardItems([sel], true)}
          disabled={!canAct}
          title="剪切到剪贴板，切到目标目录后点「粘贴」"
        >
          剪切
        </button>
        <button
          onClick={() => sel && clipboardItems([sel], false)}
          disabled={!canAct}
        >
          复制
        </button>
        <button className={clip ? 'primary' : ''} onClick={doPaste} disabled={!canWrite || !clip || busy}
          title={clip ? `粘贴 ${clip.items.length} 项到当前目录` : '先在别处剪切/复制'}>
          粘贴{clip && clip.items.length > 1 ? ` (${clip.items.length})` : ''}
        </button>
        <span className="act-sep" />
        <button onClick={() => sel && !sel.is_dir && (window.location.href = downloadUrl(instanceId, sel.path))}
          disabled={!sel || sel.is_dir} title="下载到本地">下载</button>
        <button onClick={() => openDialog('compress')} disabled={!canAct || busy}
          title="提交排队任务，由节点统一打包（一次只打包一项，多选请用批量条）">压缩</button>
        <button
          onClick={() => openDialog('extract')}
          disabled={!canAct || busy || !sel || sel.is_dir || !isArchive(sel.name)}
          title={sel && !sel.is_dir && isArchive(sel.name) ? '解压到指定目录（排队任务）；双击压缩包可直接解压到当前目录' : '选中一个压缩包后可解压'}
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
      {clip && clip.items.length > 0 && (
        <div className="clipboard-bar">
          <span className={`clip-tag ${clip.cut ? 'cut' : 'copy'}`}>{clip.cut ? '剪切' : '复制'}</span>
          <span className="mono">
            {clip.items.length === 1 ? clip.items[0].name : `${clip.items.length} 项`}
          </span>
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

      {/* ---- 文件列表 ----
           容器可聚焦（tabIndex）并接键盘：Ctrl+A 全选、Delete 删除、
           Enter 打开、Esc 取消选择。焦点在输入框里时不响应（见 onKeyDown）。 */}
      <div className={`files-list ${view}`} ref={listRef} tabIndex={0} onKeyDown={onKeyDown}>
        {view === 'list' ? (
          <table>
            <thead>
              <tr>
                <th className="col-check">
                  {results ? (
                    // 搜索结果是跨目录的，不提供全选（见 selectable 的说明），
                    // 但要说明"为什么这里没有全选框"，否则会被当成功能缺失
                    <span className="check-hint" title="搜索结果是跨目录的，请逐项勾选（避免误删到其它目录的同名文件）">—</span>
                  ) : (
                    <input
                      ref={allBoxRef}
                      type="checkbox"
                      checked={allChecked}
                      onChange={toggleAll}
                      disabled={selectable.length === 0}
                      title={allChecked ? '取消全选' : `全选当前目录的 ${selectable.length} 项`}
                    />
                  )}
                </th>
                <th>名称</th>
                <th>大小</th>
                <th>修改时间</th>
              </tr>
            </thead>
            <tbody>
              {shown.map((f) => (
                <tr
                  key={f.path}
                  className={`${sel?.path === f.path ? 'selected' : ''} ${checked.has(f.path) ? 'checked' : ''}`}
                  onClick={(e) => onRowClick(f, e)}
                  onDoubleClick={() => openItem(f)}
                >
                  <td className="col-check">
                    <input
                      type="checkbox"
                      checked={checked.has(f.path)}
                      onChange={() => toggleCheck(f)}
                      onClick={(e) => e.stopPropagation()} // 别让勾选顺带把行也当成"普通点击"
                      title="勾选后可批量操作（也可 Ctrl/Cmd 点整行）"
                    />
                  </td>
                  <td>
                    <span className="file-row">
                      <span className="file-icon">
                        {f.is_dir ? '📁' : isImageFile(f.name) ? '🖼️' : isArchive(f.name) ? '🗜️' : '📄'}
                      </span>
                      <span className="file-name">{f.name}</span>
                    </span>
                  </td>
                  <td className="mono">{f.is_dir ? '—' : formatSize(f.size)}</td>
                  <td className="mono">{formatTime(f.mod_time)}</td>
                </tr>
              ))}
              {shown.length === 0 && (
                <tr>
                  <td colSpan={4} className="empty">
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
                className={`grid-card ${sel?.path === f.path ? 'selected' : ''} ${checked.has(f.path) ? 'checked' : ''}`}
                onClick={(e) => onRowClick(f, e)}
                onDoubleClick={() => openItem(f)}
                title={f.path}
              >
                {!results && (
                  <input
                    className="grid-check"
                    type="checkbox"
                    checked={checked.has(f.path)}
                    onChange={() => toggleCheck(f)}
                    onClick={(e) => e.stopPropagation()}
                  />
                )}
                <div className="grid-icon">
                  {f.is_dir ? '📁' : isImageFile(f.name) ? '🖼️' : isArchive(f.name) ? '🗜️' : '📄'}
                </div>
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

      {/* ---- 操作说明 ---- */}
      <div className="files-tip">
        <b>单击</b>选中，<b>双击</b>打开：目录进入 / 文本编辑 / 图片预览 / 压缩包解压到当前目录 / 其它文件直接下载。
        <b>Ctrl/Cmd 点</b>多选、<b>Ctrl+A</b> 全选本目录、<b>Delete</b> 删除选中、<b>Esc</b> 取消选择。
        压缩 / 解压会提交到<strong>节点任务队列</strong>，可在上方「任务」中查看进度与取消。
      </div>

      {/* ---- 图片预览 ---- */}
      {preview && (
        <div className="modal-mask" onClick={() => { URL.revokeObjectURL(preview.url); setPreview(null) }}>
          <div className="img-preview" onClick={(e) => e.stopPropagation()}>
            <div className="img-preview-head">
              <span className="mono">{preview.name}</span>
              <span className="muted">{formatSize(preview.size)}</span>
              <div className="spacer" />
              <button onClick={() => window.location.href = downloadUrl(instanceId, joinPath(path, preview.name))}>
                下载
              </button>
              <button onClick={() => { URL.revokeObjectURL(preview.url); setPreview(null) }}>关闭</button>
            </div>
            <img src={preview.url} alt={preview.name} />
          </div>
        </div>
      )}

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
