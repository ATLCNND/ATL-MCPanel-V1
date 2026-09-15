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

export default function ConsoleTab({ instanceId, canSend }: { instanceId: string; canSend: boolean }) {
  const termRef = useRef<HTMLDivElement>(null)
  const term = useRef<Terminal | null>(null)
  const wsRef = useRef<WebSocket | null>(null)
  const [connected, setConnected] = useState(false)
  const [input, setInput] = useState('')

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
        t.writeln('\x1b[90m[控制台已连接]\x1b[0m')
      }
      ws.onmessage = (e) => {
        if (disposed) return
        t.write(e.data)
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
