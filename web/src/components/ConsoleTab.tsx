import { useEffect, useRef, useState } from 'react'
import { Terminal } from 'xterm'
import { FitAddon } from '@xterm/addon-fit'
import 'xterm/css/xterm.css'
import { consoleWsUrl } from '../api'
import { onThemeChange } from '../styles/theme'
import './ConsoleTab.css'

/**
 * 读取 CSS 变量当前值。
 *
 * xterm.js 渲染到 canvas，**不读 CSS**，颜色必须由 JS 在构造/更新时传入。
 * 因此这里统一从 token 层取值，保证主题切换时终端与页面同步。
 */
function cssVar(name: string, fallback = ''): string {
  const v = getComputedStyle(document.documentElement).getPropertyValue(name).trim()
  return v || fallback
}

/** 构造 xterm 主题（全部取自 token，不再硬编码颜色） */
function termTheme() {
  const text = cssVar('--term-text', '#c9d1d9')
  const danger = cssVar('--danger', '#f85149')
  const info = cssVar('--term-info', '#7ee787')
  const warn = cssVar('--term-warn', '#d99a12')
  const accent = cssVar('--accent', '#d4799f')
  const termAccent = cssVar('--term-accent', '#f9a8d4')
  const accentLight = cssVar('--accent-light', '#e8a8c4')
  return {
    background: cssVar('--term-bg', '#0f1117'),
    foreground: text,
    cursor: termAccent,
    selectionBackground: cssVar('--accent-solid', '#9c1450'),

    // 常用 ANSI 色跟随语义 token，避免浅色主题下出现刺眼的深色
    black: text,
    red: danger,
    green: info,
    yellow: warn,
    blue: accent,
    magenta: termAccent,
    cyan: accentLight,
    white: text,

    /* bright 系列必须**逐个显式给值**。
       xterm 对未设置的项会落回内置调色板（brightYellow = #ffff00、
       brightRed = #ff0000 这类纯色）：在深色终端上刺眼发糊，
       在浅色终端（淡粉底黑字）上更是几乎看不见 —— 用户反馈的
       "黄色警告有点模糊"正是这一类。
       这里统一回落到对应的语义色：宁可少一点"高亮"，也不要不可读。 */
    brightBlack: cssVar('--term-time', '#6b7280'),
    brightRed: danger,
    brightGreen: info,
    brightYellow: cssVar('--term-warn-bright', warn),
    brightBlue: accent,
    brightMagenta: termAccent,
    brightCyan: accentLight,
    brightWhite: text,
  }
}

/** 日志级别（与后端 internal/consolefmt 的取值一一对应） */
type Level = 'info' | 'warn' | 'error'

/** 级别筛选：all 之外都对应一个具体级别 */
type Filter = 'all' | Level

/** 面板转发过来的控制台消息（见 internal/panel/httpapi/console.go） */
interface ConsoleFrame {
  type: 'line' | 'status' | 'notice'
  level: Level
  data: string
}

/** 一行已收到的输出（含级别，用于切换筛选后重建视图） */
interface BufferedLine {
  level: Level
  text: string
}

/**
 * 本地保留的行数上限。
 *
 * 与 xterm 的 scrollback 取同一个数量级：留得比终端能显示的更多没有意义
 * （终端本来就只记得住这么多），留得太少则切换筛选时会缺内容。
 */
const BUFFER_MAX = 5000

const FILTERS: { key: Filter; label: string }[] = [
  { key: 'all', label: '全部' },
  { key: 'info', label: '信息' },
  { key: 'warn', label: '警告' },
  { key: 'error', label: '错误' },
]

export default function ConsoleTab({ instanceId, canSend }: { instanceId: string; canSend: boolean }) {
  const termRef = useRef<HTMLDivElement>(null)
  const term = useRef<Terminal | null>(null)
  const wsRef = useRef<WebSocket | null>(null)
  const [connected, setConnected] = useState(false)
  const [input, setInput] = useState('')

  // 筛选状态：用 ref 存一份给 WS 回调读（回调是闭包，拿不到最新的 state），
  // 用 state 存一份驱动界面与计数。
  const [filter, setFilter] = useState<Filter>('all')
  const filterRef = useRef<Filter>('all')
  const [counts, setCounts] = useState<Record<Level, number>>({ info: 0, warn: 0, error: 0 })

  // 收到的行缓冲 + "尚未收到换行的半行"。
  //
  // 为什么要自己留一份缓冲：xterm 只能"往里写"，没法把已写的行藏起来。
  // 想在切换筛选时**连历史一起**过滤（而不是只影响之后的新行），
  // 就必须能重放 —— 切筛选时清屏，再把符合条件的行重新写一遍。
  const bufRef = useRef<BufferedLine[]>([])
  const pendingRef = useRef<BufferedLine | null>(null)

  /** 是否显示某级别的行 */
  const visible = (lv: Level, f: Filter) => f === 'all' || f === lv

  /** 把缓冲里符合条件的行重写到终端（切换筛选时用） */
  const replay = (f: Filter) => {
    const t = term.current
    if (!t) return
    t.reset()
    for (const line of bufRef.current) {
      if (visible(line.level, f)) t.write(line.text)
    }
    if (pendingRef.current && visible(pendingRef.current.level, f)) {
      t.write(pendingRef.current.text)
    }
  }

  /** 重算各级别行数 */
  const recount = () => {
    const c: Record<Level, number> = { info: 0, warn: 0, error: 0 }
    for (const l of bufRef.current) c[l.level]++
    setCounts(c)
  }

  // 终端初始化 + WebSocket 接入
  useEffect(() => {
    if (!termRef.current) return

    const t = new Terminal({
      cursorBlink: true,
      fontSize: 13,
      fontFamily: cssVar('--mono', "'SFMono-Regular', Consolas, monospace"),
      theme: termTheme(),
      convertEol: true,
      disableStdin: true, // 终端只读，命令走独立输入框
      scrollback: 5000,
    })
    const fit = new FitAddon()
    t.loadAddon(fit)
    t.open(termRef.current)
    fit.fit()
    term.current = t

    const onResize = () => fit.fit()
    window.addEventListener('resize', onResize)

    let ws: WebSocket | null = null
    let retryTimer: any = null
    let disposed = false

    /**
     * 处理一条控制台消息。
     *
     * 半行（没有换行的分片）要合并：Daemon 是按行推送的，但服务端偶尔会写下
     * 不带换行的内容（进度条、交互式提示），那一行会分几片到达。合并时**沿用
     * 第一片的级别** —— 级别标记（行首的 [WARN]/[ERROR] 配色）出现在第一片，
     * 后续分片本来就没有标记，若按分片各自的级别算，同一行会被归到两个级别。
     */
    const onFrame = (f: ConsoleFrame) => {
      const data = f.data ?? ''
      if (!data) return
      const lv = pendingRef.current ? pendingRef.current.level : f.level
      const text = (pendingRef.current?.text ?? '') + data

      // 按换行切分：除最后一段外都是完整行
      const parts = text.split('\n')
      const tail = parts.pop() ?? ''
      let grew = false
      for (let i = 0; i < parts.length; i++) {
        const line = parts[i] + '\n'
        bufRef.current.push({ level: lv, text: line })
        if (visible(lv, filterRef.current)) t.write(line)
        grew = true
      }
      // 还有没带换行的残句：留在 pending 里继续等它的后续分片
      pendingRef.current = tail ? { level: lv, text: tail } : null
      if (pendingRef.current && visible(lv, filterRef.current)) t.write(tail)

      if (grew) {
        if (bufRef.current.length > BUFFER_MAX) {
          bufRef.current.splice(0, bufRef.current.length - BUFFER_MAX)
        }
        recount()
      }
    }

    const connect = () => {
      if (disposed) return
      ws = new WebSocket(consoleWsUrl(instanceId))
      wsRef.current = ws

      // ⚠️ 下面每个回调都要先判 disposed：清理函数里有 ws.close()，
      // 而 onclose 是**异步**触发的 —— 它会在 t.dispose() 之后才跑到，
      // 于是往一个已销毁的终端上写日志，抛
      // TypeError: Cannot read properties of undefined (reading 'dimensions')
      //（xterm 内部去读已被释放的 renderService）。切页签时必然触发。
      ws.onopen = () => {
        if (disposed) return
        setConnected(true)
        // 服务端会回放最近日志，先重置本地缓冲避免重连后重复
        t.reset()
        bufRef.current = []
        pendingRef.current = null
        recount()
        t.writeln('\x1b[90m[控制台已连接]\x1b[0m')
      }
      ws.onmessage = (e) => {
        if (disposed) return
        // 面板发的是 JSON（带级别）；解析失败就按原始文本兜底 ——
        // 协议将来若再变，最坏也只是丢掉筛选能力，不会整屏空白。
        try {
          onFrame(JSON.parse(String(e.data)) as ConsoleFrame)
        } catch {
          t.write(String(e.data))
        }
      }
      ws.onerror = () => {
        if (disposed) return
        setConnected(false)
        t.writeln('\x1b[91m[连接错误]\x1b[0m')
      }
      ws.onclose = () => {
        if (disposed) return
        setConnected(false)
        t.writeln('\x1b[90m[连接已断开，3 秒后重连…]\x1b[0m')
        retryTimer = setTimeout(connect, 3000)
      }
    }
    connect()

    return () => {
      disposed = true
      clearTimeout(retryTimer)
      window.removeEventListener('resize', onResize)
      // 先把回调摘掉再 close：否则 onclose 仍会跑到写终端的逻辑上
      if (ws) {
        ws.onopen = ws.onmessage = ws.onerror = ws.onclose = null
        ws.close()
      }
      term.current = null
      wsRef.current = null
      // ⚠️ 销毁**推迟一帧**。
      // xterm 的 Viewport 内部会排一个 requestAnimationFrame 去做重绘刷新
      //（Viewport._innerRefresh，里面读 renderService.dimensions）。
      // 如果这一帧里立刻 dispose，那个已排队的回调就会在一个已销毁的终端上跑，
      // 抛 TypeError: Cannot read properties of undefined (reading 'dimensions')。
      // 切页签（离开控制台）时必然触发，控制台里能看到这条报错。
      // 排到下一帧：先让 xterm 自己排的回调落地，再销毁。
      requestAnimationFrame(() => {
        try { t.dispose() } catch { /* 已经销毁过就算了 */ }
      })
    }
  }, [instanceId])

  // 主题切换时同步终端配色
  // （xterm 不读 CSS，必须在 data-theme 变化后重新写入 theme 选项）
  useEffect(() => {
    return onThemeChange(() => {
      const t = term.current
      if (!t) return
      t.options.theme = termTheme()
      t.refresh(0, t.rows - 1)
    })
  }, [])

  // 切换筛选：重建视图（连历史一起过滤）
  useEffect(() => {
    filterRef.current = filter
    if (!term.current) return
    replay(filter)
    term.current.scrollToBottom()
  }, [filter])

  const total = counts.info + counts.warn + counts.error
  const shown = filter === 'all' ? total : counts[filter as Level]
  const hidden = total - shown

  const countOf = (k: Filter) => (k === 'all' ? total : counts[k as Level])

  const sendCommand = () => {
    const ws = wsRef.current
    if (ws && ws.readyState === WebSocket.OPEN && input.trim()) {
      ws.send(input + '\n')
      setInput('')
    }
  }

  const onInputKey = (e: React.KeyboardEvent) => {
    if (e.key === 'Enter') {
      e.preventDefault()
      sendCommand()
    }
  }

  return (
    <div className="console">
      <div className="console-head">
        <span className="t">控制台 · {instanceId}</span>

        <div
          className="console-filters"
          title='筛选日志级别（点击后连已收到的历史一起过滤，可随时切回"全部"）'
        >
          {FILTERS.map((f) => (
            <button
              key={f.key}
              type="button"
              className={`cf-btn cf-${f.key}${filter === f.key ? ' on' : ''}`}
              onClick={() => setFilter(f.key)}
            >
              {f.label}
              <em>{countOf(f.key)}</em>
            </button>
          ))}
          {filter !== 'all' && hidden > 0 && (
            <span className="cf-hidden">已隐藏 {hidden} 行</span>
          )}
        </div>

        <span className={`console-status ${connected ? 'on' : 'off'}`}>
          {connected ? '已连接' : '未连接'}
        </span>
      </div>

      <div className="console-body" ref={termRef} />

      <div className="console-input">
        <span className="p">›</span>
        <input
          value={input}
          onChange={(e) => setInput(e.target.value)}
          onKeyDown={onInputKey}
          placeholder={canSend ? '输入命令后回车执行…' : '只读权限：无法发送命令'}
          spellCheck={false}
          disabled={!canSend}
        />
      </div>
    </div>
  )
}
