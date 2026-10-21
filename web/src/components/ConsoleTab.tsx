import { useEffect, useMemo, useRef, useState } from 'react'
import { Terminal } from 'xterm'
import { FitAddon } from '@xterm/addon-fit'
import 'xterm/css/xterm.css'
import { consoleWsUrl, isTokenExpired, notifyAuthExpired, tokenRejectedByServer } from '../api'
import { clampRetain, needsTrim, trimByUseful, RETAIN_MIN, RETAIN_MAX, REPLAY_MAX } from './consoleBuffer'
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
  // 报错色分两个 token：底色用 --term-error（压得住的品红），
  // 文字用 --term-error-text（要够亮才读得清）。见 tokens.css 的说明。
  const termError = cssVar('--term-error', danger)
  const termErrorText = cssVar('--term-error-text', termError)
  return {
    background: cssVar('--term-bg', '#0f1117'),
    foreground: text,
    cursor: termAccent,
    // 选中区单独一个 token：深色模式是半透明淡粉叠加，浅色模式是深粉实色
    selectionBackground: cssVar('--term-select', cssVar('--accent-solid', '#9c1450')),

    // 常用 ANSI 色跟随语义 token，避免浅色主题下出现刺眼的深色
    black: text,
    red: termError,
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
    brightRed: termErrorText,
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

/** `#rrggbb` → RGB 三元组；不是六位十六进制就返回 null（调用方走兜底）。 */
function hexRgb(hex: string): [number, number, number] | null {
  const m = /^#([0-9a-f]{6})$/i.exec(hex.trim())
  if (!m) return null
  const n = parseInt(m[1], 16)
  return [(n >> 16) & 255, (n >> 8) & 255, n & 255]
}

/** 真彩底色序列（48;2;R;G;B）。取不到颜色时退回深色模式的暗品红。 */
function sgrBg(hex: string): string {
  const c = hexRgb(hex) ?? [163, 40, 95]
  return `\x1b[48;2;${c[0]};${c[1]};${c[2]}m`
}

/** 真彩前景序列（38;2;R;G;B）。取不到颜色时退回深色模式的亮品红。 */
function sgrFg(hex: string): string {
  const c = hexRgb(hex) ?? [255, 127, 182]
  return `\x1b[38;2;${c[0]};${c[1]};${c[2]}m`
}

/**
 * 把 Daemon 的**级别配色**重映射成当前主题的色号。
 *
 * 为什么必须在浏览器侧做（而不是改 Daemon 那个常量）：
 *   Daemon 涂的是 24 位真彩（见 internal/consolefmt.AnsiErrorBand），色号写死在
 *   节点上、所有浏览器共用一份。而"深色用暗品红、浅色用亮品红"是**主题属性**，
 *   只有浏览器知道当前是哪套主题。所以职责这样分：
 *     · Daemon —— 只负责**标记级别**（用哪一种底色带 / 哪一个提示色）；
 *     · 浏览器 —— 负责**选具体色号**，同一份标记在不同主题下渲染成不同颜色。
 *   这样既没有推翻"配色只有一份定义"的既有决策（标记仍然只有一份），
 *   又满足了"深色别刺眼、浅色要清楚"这个只能按主题决定的需求。
 *
 * 只改两处，且**只对 error 级别的行**生效：
 *   · `48;2;R;G;B`（级别底色带）→ --term-error
 *   · `91`（亮红前景，Daemon 的提示色）→ --term-error-text
 * 服务器自己输出的一切颜色、以及 info/warn 行，一个字都不碰。
 */
function paintLine(text: string, level: Level): string {
  if (level !== 'error') return text
  return text
    .replace(/\x1b\[48;2;\d+;\d+;\d+m/g, sgrBg(cssVar('--term-error', '#a3285f')))
    .replace(/\x1b\[91m/g, sgrFg(cssVar('--term-error-text', '#ff7fb6')))
}

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
  /**
   * 这一行是否属于"被屏蔽的周期性刷屏"（判定发生在**收行那一刻**）。
   *
   * 只用于**裁剪计数**：设置里的「保留行数」现在按"非刷屏行"算，被屏蔽的行不占额度
   *（用户 2026-10-17 要求）。重放时的屏蔽判定仍按**当前规则**现算 ——
   * 用户改了规则之后，旧行也要跟着变。
   */
  spam?: boolean
}

/**
 * "尚未收到换行的半行"。
 *
 * `shown` 记的是**已经写进终端**的字符数 —— 没有它就会重复输出，见 onFrame。
 */
interface PendingLine {
  level: Level
  text: string
  shown: number
}

/**
 * 把**当前生效**的刷屏屏蔽规则序列化，交给服务端（见 api.ts 的 consoleWsUrl）。
 *
 * 为什么要给服务端：控制台附加时会回放最近 500 行，而回放窗口现在也按"非刷屏行"计数
 *（用户 2026-10-17 要求）—— 判"什么算刷屏"的规则只有这一份（就下面这张表 + 用户自定义），
 * 服务端不该另写一套，否则两边迟早走偏。所以把规则直传下去。
 *
 * 三种情况不给规则（服务端退回"不过滤"，即改动前的行为）：
 *   ① 总开关关着（用户在界面上选了"显示刷屏"，那就别在回放里偷偷丢掉这些行）；
 *   ② 一条规则都没开；
 *   ③ 序列化失败（理论上不会）。
 *
 * ⚠️ 2026-10-21 起这个 payload 里还带 `history`：**设置里的「保留行数」**。
 * 在此之前服务端回放窗口是写死的 500 行，于是用户把保留行数从 5000 调到 1000
 * 或 50000，控制台能滚的范围都不变（实测：节点日志 43017 行，控制台只拿到 503 行）
 * —— 这就是用户报的"保留行数没作用到控制台上"。现在由浏览器把自己的口径传下去，
 * Daemon 按它生成回放窗口（上下限与理由见 daemon/grpcapi/server.go）。
 */
function attachPayloadFor(
  ruleEnabled: Record<string, boolean>,
  customRules: { id: string; label: string; pattern: string }[],
  hideHeartbeat: boolean,
  retain: number,
): string {
  const rules: { id: string; pattern: string; flags: string; block: boolean }[] = []
  if (hideHeartbeat) {
    for (const r of HEARTBEAT_PRESETS) {
      if (!ruleEnabled[r.id]) continue
      rules.push({ id: r.id, pattern: r.re.source, flags: r.re.flags, block: !!r.block })
    }
    for (const c of customRules) {
      // 自定义规则一律逐行（与前端 regRef 的用法一致），大小写不敏感
      if (ruleEnabled[c.id] === false) continue
      rules.push({ id: c.id, pattern: c.pattern, flags: 'i', block: false })
    }
  }
  // history 必须先于规则存在：即使一条规则都没开（回放不过滤），
  // 「保留行数」也仍然要生效 —— 早期写法是"没规则就直接返回空串"，
  // 那样等于把 history 一起丢了。
  const payload: { spam?: { rules: typeof rules }; history: number } = {
    history: clampRetain(retain),
  }
  if (rules.length > 0) payload.spam = { rules }
  try {
    return JSON.stringify(payload)
  } catch {
    return ''
  }
}

/**
 * 本地缓冲的保留条数**默认值**。
 *
 * ⚠️ 2026-10-17 起，实际保留条数跟着设置里的「保留行数」（xterm scrollback）走，
 * 而且按**非刷屏行**计数 —— 见 consoleBuffer.ts 里的说明与 trimByUseful。
 * 这个常量只作为 opts 读不到时的兜底。
 */
const BUFFER_MAX = 5000

const FILTERS: { key: Filter; label: string }[] = [
  { key: 'all', label: '全部' },
  { key: 'info', label: '信息' },
  { key: 'warn', label: '警告' },
  { key: 'error', label: '错误' },
]

/**
 * 周期性刷屏日志的**预制规则**（用户 2026-10-06 要求"泛化要预制进去"）。
 *
 * 为什么要按核心分类：不同服务端的周期性输出**形式不同** ——
 * 实测（节点上的 Paper / Folia 日志）：
 *   · Paper：`[HH:MM:SS INFO]: There are 0 of a max of 20 players online:`
 *   · Paper：`[HH:MM:SS INFO]: TPS from last 1m, 5m, 15m: 20.0, 20.0, 20.0`
 *   · Folia：只有 players online 那条，**没有** TPS 那条（Folia 的 TPS 报法不同）
 * 所以规则必须写成"一族模式"而不是写死两条，否则换个核心就漏。
 *
 * 每条规则的 `re` 都用**模式**而非字面量：玩家数、TPS 数值每次都在变。
 * `default: true` 的会在首次打开时默认启用（用户可逐条关闭，也可自己加）。
 */
export interface HeartbeatRule {
  id: string
  label: string
  /** 命中即视为"周期性刷屏" */
  re: RegExp
  /** 默认是否启用 */
  byDefault: boolean
  /** 说明这条针对什么 */
  note: string
  /**
   * 是否为**块状规则**：命中后不只是隐藏这一行，而是隐藏从它开始的整个块。
   *
   * 为什么需要（用户 2026-10-06 报）：像 Folia 的 `Server Health Report`
   * 是**十几行的多行块**，而续行（` - Online Players: 0`、`Chunks: 529 ...`）
   * 既没有时间戳也没有唯一特征 —— 逐行规则要么挡不住，要么误伤别的内容。
   * 所以把"块"作为一个整体单位来处理。
   */
  block?: boolean
}

export const HEARTBEAT_PRESETS: HeartbeatRule[] = [
  {
    id: 'players-online',
    label: '在线人数统计',
    re: /There are \d+ of a max of \d+ players online/i,
    byDefault: true,
    note: 'Paper / Folia / Purpur 等：每 5 秒一条。注意 `list` 命令的回显与它同文本，已由"命令回显保护"单独处理。',
  },
  {
    id: 'tps-report',
    label: 'TPS 报告（Paper 系）',
    re: /TPS from last 1m, 5m, 15m:/i,
    byDefault: true,
    note: 'Paper / Spigot：每 15 秒一条。Folia 不输出这条。',
  },
  {
    id: 'tps-report-alt',
    label: 'TPS 报告（其它写法）',
    // 覆盖 "Current TPS"、"TPS: 20.0"、"Server tick ... TPS" 等变体
    re: /(\bTPS[:：]\s*[\d.]|Current TPS|Server tick loop|Average tick time|MSPT from last)/i,
    byDefault: true,
    note: '其它核心 / 插件的 TPS 或 MSPT 周期报告。',
  },
  {
    id: 'health-report',
    label: 'Folia 健康报告（多行块）',
    // ⚠️ 这是**块状**输出：从 "Server Health Report" 开始，后续若干行
    // （- Online Players: / - Total regions: / Region around block ... / Chunks: ...）
    // 都属于同一个块。逐行匹配挡不住它 —— 用 `block` 标记，由 replay/onFrame
    // 按"块"整体判定（见 BlockRule 与 blockSkipRef）。
    re: /Server Health Report/i,
    byDefault: true,
    block: true,
    note: 'Folia 每分钟一次的多行报告（含 region/区块统计）。开启后整块隐藏。',
  },
  {
    id: 'health-report-cont',
    label: '健康报告续行',
    // 续行没有独立的时间戳前缀，靠内容识别：以 " - " 开头、或 "Chunks:"/"util at"
    re: /^(\s*-\s|\s*\d+\.\d+% util at|\s*Chunks:|\s*Region around block|\s*Highest \d+ utilisation)/i,
    byDefault: false,
    block: true,
    note: '健康报告块内的续行。**默认关**：这些形状（如 "Chunks:"）在其它输出里也可能出现，误伤风险较高，需要时再开。',
  },
  {
    id: 'saving-world',
    label: '自动保存提示',
    re: /(Saving (the game|chunks|world)|Saved the game|Saved \d+ .*chunks?)/i,
    byDefault: false,
    note: '自动保存很频繁；默认**不屏蔽**（它是"服务器还活着"的信号），需要时自行开启。',
  },
  {
    id: 'timings-reset',
    label: 'Timings 重置',
    re: /Timings Reset|Timings\b.*reset/i,
    byDefault: true,
    note: 'Paper 的 Timings 周期重置提示。',
  },
]

/** 规则的存储 key（用户自定义的屏蔽规则持久化在 localStorage） */
const RULES_KEY = 'atl-console-hide-rules'
const OPTS_KEY = 'atl-console-opts'

/**
 * 去掉 ANSI 转义序列。
 *
 * 为什么需要：控制台的行里带颜色码（Daemon 会在行首涂色表示级别），
 * 直接复制到剪贴板/粘贴到别处会变成一堆 `\x1b[32m` 之类的乱码。
 * xterm 的 `getSelection()` 返回的是**纯文本**（它自己剥掉了），
 * 但"复制全部"走的是我们自己的行缓冲，必须自己剥。
 *
 * 覆盖范围：CSI（`ESC [ ... 字母`）与两种常见简写（`ESC ] ... BEL`、OSC）。
 * 不追求完备 —— 只要覆盖终端实际会写出来的那些即可。
 */
function stripAnsi(s: string): string {
  // eslint-disable-next-line no-control-regex
  return s.replace(/\x1b\[[0-9;?]*[ -/]*[@-~]/g, '')
          .replace(/\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)/g, '')
}

/**
 * 用户可调的终端显示参数（用户 2026-10-06 要求"字体大小、行间距等"）。
 *
 * 全部存 localStorage：这些是**个人观感偏好**，不该同步到服务端，
 * 也不该因为换台机器就丢（所以不用 sessionStorage）。
 */
export interface ConsoleOpts {
  fontSize: number
  lineHeight: number
  /** 终端保留的最大行数（xterm scrollback） */
  scrollback: number
  /** 是否折行（长行自动换行 vs 横向滚动） */
  wrap: boolean
  /** 光标闪烁 */
  cursorBlink: boolean
}

const DEFAULT_OPTS: ConsoleOpts = {
  fontSize: 13,
  lineHeight: 1.2,
  scrollback: 5000,
  wrap: true,
  cursorBlink: true,
}

function loadOpts(): ConsoleOpts {
  try {
    const raw = localStorage.getItem(OPTS_KEY)
    if (!raw) return { ...DEFAULT_OPTS }
    const o = { ...DEFAULT_OPTS, ...JSON.parse(raw) } as ConsoleOpts
    // 旧版本可能存着超过当前上限的值（上一版滑块到 50000）——
    // 载入时就夹一次，否则界面会显示"50k"而滑块停在顶端，看着自相矛盾。
    o.scrollback = clampRetain(o.scrollback)
    return o
  } catch { return { ...DEFAULT_OPTS } }
}

/** 载入用户的屏蔽规则：预制规则按其 byDefault + 用户覆盖 */
function loadRules(): { enabled: Record<string, boolean>; custom: { id: string; label: string; pattern: string }[] } {
  const enabled: Record<string, boolean> = {}
  for (const r of HEARTBEAT_PRESETS) enabled[r.id] = r.byDefault
  let custom: { id: string; label: string; pattern: string }[] = []
  try {
    const raw = localStorage.getItem(RULES_KEY)
    if (raw) {
      const saved = JSON.parse(raw)
      if (saved && typeof saved === 'object') {
        Object.assign(enabled, saved.enabled || {})
        if (Array.isArray(saved.custom)) custom = saved.custom
      }
    }
  } catch { /* 存储损坏就用默认值，不打断控制台 */ }
  return { enabled, custom }
}

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

  // 屏蔽周期性心跳（玩家数 / TPS）。**默认开**：这两条日志每 5~15 秒刷一次，
  // 会把真正的输出冲到看不见的地方 —— 用户报的就是这个。
  //
  // 与上面的级别筛选**正交**：级别管"信息/警告/错误"，这个只管"周期性刷屏"。
  // 同样是 state + ref 两份（ref 给 WS 闭包读）。
  //
  // 2026-10-06 起**规则可配置**（用户要求）：预制规则（含多核心泛化）+ 用户自定义正则，
  // 均持久化在 localStorage。
  const initialRules = useMemo(() => loadRules(), [])
  /** 显示参数的初值单独存一份：下面要给 spamCfgRef 预置 payload，需要它。 */
  const initialOpts = useMemo(() => loadOpts(), [])
  const [hideHeartbeat, setHideHeartbeat] = useState(true)
  const hideHbRef = useRef(true)
  /** 当前生效的附加参数（JSON：屏蔽规则 + 保留行数）。连接时随 URL 带给服务端，见 attachPayloadFor。 */
  const spamCfgRef = useRef('')
  // ⚠️ 必须在这里预置：真正发起连接的是下面那个"终端初始化"effect，
  // 而同步这份 payload 的 effect 声明在它**之后**（React 按声明顺序执行），
  // 不预置的话第一次连接拿不到 history，用户会看到"改了保留行数、重进控制台还是不生效"。
  if (!spamCfgRef.current) {
    spamCfgRef.current = attachPayloadFor(
      initialRules.enabled, initialRules.custom, true, initialOpts.scrollback,
    )
  }
  /** 被心跳屏蔽掉的行数（显示在开关旁边，让人知道藏了多少） */
  const [hbHidden, setHbHidden] = useState(0)

  const [ruleEnabled, setRuleEnabled] = useState<Record<string, boolean>>(initialRules.enabled)
  const [customRules, setCustomRules] = useState(initialRules.custom)
  /** 编译好的屏蔽正则（预制里启用的 + 自定义里合法的） */
  const regRef = useRef<RegExp[]>([])
  /** 块状规则（命中后整块隐藏，见 HeartbeatRule.block） */
  const blockReRef = useRef<RegExp[]>([])
  /**
   * 实时通道的"当前是否在块内"状态。
   *
   * 与 replay 里的局部变量 `inBlock` 对应：replay 每次全量重扫，用局部变量即可；
   * 实时通道是**流式**的，必须把状态存在 ref 里跨帧保持。
   */
  const liveBlockRef = useRef(false)
  /**
   * 命令回显保护窗口（毫秒级时间戳）。
   *
   * 为什么需要（用户 2026-10-06 报「屏蔽会屏蔽手动指令的输出，比如 list」）：
   *   `list` 命令的回显是 `There are 0 of a max of 20 players online: ` ——
   *   与周期性心跳**文本完全相同**，光看内容无法区分。
   *   实际差别在**来源**：周期性的是服务器自己定时打的，命令回显是紧接着用户
   *   发送命令之后出现的。所以在发送命令时记一个时间窗，窗内命中的行**不屏蔽**。
   *
   * 窗口取 3 秒：服务器处理 `list`/`tps` 这类即时命令通常远快于此，
   * 而周期性心跳间隔是 5 秒以上，3 秒窗口不会把心跳误判成命令回显。
   */
  const cmdWindowRef = useRef(0)

  // 终端显示参数 + 设置面板开关
  const [opts, setOpts] = useState<ConsoleOpts>(initialOpts)
  const [showSettings, setShowSettings] = useState(false)
  const fitRef = useRef<FitAddon | null>(null)

  /**
   * 设置面板的**定位信息**（视口坐标）。
   *
   * ⚠️ 2026-10-06 用户报「设置悬浮卡片不能完整显示」，根因是：
   *   `.console { overflow: hidden }`（为了圆角裁剪）会把**绝对定位的浮层一起裁掉** ——
   *   面板设了 `top: calc(100% + 8px)` 挂在按钮下方，但它超出了控制台容器的高度，
   *   于是下半部分被剪掉。`max-height: 70vh` 更是基于视口算的，与控制台盒子的
   *   高度无关，所以必然溢出。
   *
   * 修法：面板改用 `position: fixed`（脱离那个裁剪上下文），
   * 坐标在**打开时**从按钮的 `getBoundingClientRect()` 现算（见按钮的 onClick）。
   * 这样无论控制台容器多高、页面滚到哪里，面板都能完整显示。
   */
  const settingsBtnRef = useRef<HTMLDivElement>(null)
  const [settingsPos, setSettingsPos] = useState<
    { right: number; top?: number; bottom?: number; maxHeight: number } | null
  >(null)

  /**
   * 算悬浮窗的位置与**可用高度**。
   *
   * ⚠️ 2026-10-17 实测缺陷（用户报"还有BUG以及界面设计不合理"）：原先无论下方有没有
   * 空间都往下展开（`top: r.bottom + 8`），而 CSS 里的 `max-height: calc(100vh - 96px)`
   * 用的是**固定 96px**（注释写的却是"减去面板顶端到顶部的距离"）。窗口高 607px 时：
   * 面板 top=285、高 511 → 底边 796，**超出视口 189px**；而面板自己是 `overflow:auto`、
   * 页面又不可滚动 —— 底部那截（「复制」那一块）**既看不到也滚不到**。
   *
   * 现在按真实剩余空间算：下方够用就向下、不够就改向上，并把 max-height 内联成
   * 那个真实可用高度，保证底边永远在视口内。
   */
  const computeSettingsPos = (r: DOMRect) => {
    const GAP = 8        // 与按钮的间距
    const MARGIN = 12    // 与视口边的留白
    const MIN_USABLE = 240  // 再小就只能靠内部滚动，但至少给这么多
    const below = window.innerHeight - r.bottom - GAP - MARGIN
    const above = r.top - GAP - MARGIN
    const right = Math.max(MARGIN, window.innerWidth - r.right)
    if (below >= MIN_USABLE || below >= above) {
      return { right, top: r.bottom + GAP, maxHeight: Math.max(MIN_USABLE, below) }
    }
    // 下方不够、上方更宽裕：贴着按钮向上展开
    return {
      right,
      bottom: window.innerHeight - r.top + GAP,
      maxHeight: Math.max(MIN_USABLE, above),
    }
  }

  /**
   * 悬浮窗开着时的三个"桌面端该有"的行为：Esc 关闭、点外部关闭、窗口尺寸变化后重算位置。
   *
   * 之前**只有**右上角那个 ✕ 一个关闭入口（代码里确实没有这三样）：
   * 用户在设置里改完字号想关掉，只能去精准点那个小叉；而把窗口缩小之后再打开设置，
   * 位置还是按旧尺寸算的，又会溢出视口 —— 这正是上面那个"底部够不着"的另一半成因。
   */
  useEffect(() => {
    if (!showSettings) return
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape') setShowSettings(false)
    }
    const onDown = (e: MouseEvent) => {
      const t = e.target as HTMLElement | null
      if (!t) return
      if (t.closest('.console-settings') || t.closest('.console-settings-wrap')) return
      setShowSettings(false)
    }
    const onResize = () => {
      const r = settingsBtnRef.current?.getBoundingClientRect()
      if (r) setSettingsPos(computeSettingsPos(r))
    }
    window.addEventListener('keydown', onKey)
    // capture 阶段：避免别处 stopPropagation 把这次点击吃掉
    window.addEventListener('mousedown', onDown, true)
    window.addEventListener('resize', onResize)
    return () => {
      window.removeEventListener('keydown', onKey)
      window.removeEventListener('mousedown', onDown, true)
      window.removeEventListener('resize', onResize)
    }
  }, [showSettings])

  /**
   * "复制全部可见内容"的实现（由终端初始化时挂上，头部按钮调用）。
   *
   * 用 ref 而不是把逻辑写在按钮的 onClick 里，是因为这段实现需要闭包里的
   * `t`（xterm 实例）与缓冲引用；而按钮在 JSX 里，拿不到那些局部变量。
   */
  const copyAllRef = useRef<(() => void) | null>(null)

  /** "全选终端内容"（供按钮调用，避免依赖 Ctrl+A 的焦点判定） */
  const selectAllRef = useRef<(() => void) | null>(null)

  /**
   * "复制当前选中内容"（供右键菜单调用）。
   *
   * 与 copyAllRef 一样用 ref 暴露：终端实例 `t` 是 useEffect 里的局部变量，
   * JSX 里的按钮拿不到它。
   */
  const copySelRef = useRef<(() => Promise<boolean>) | null>(null)

  /**
   * 连接**世代号**：每次 connect() 自增。
   *
   * 用途：把"上一代连接的迟到回调"挡掉。WebSocket 的 close/error/message
   * 都是异步的，旧连接在新连接建立后仍可能投递事件 —— 那些帧会写进**同一个
   * 终端**，表现为整屏重复（用户实测的"控制台重复显示、刷新后消失"）。
   * 回调里比对世代号即可丢弃它们。
   */
  const connGenRef = useRef(0)

  /**
   * 本次连接是否出过错（onerror 置位）。
   *
   * 浏览器对 WS 失败的触发顺序是 onerror → onclose，而两者都会调到。
   * 只用 onclose 做判断就不会重复；这个标记留给需要区分
   * "正常关闭"与"出错关闭"的未来用途，也便于排查时打印。
   */
  const errRef = useRef(false)

  // 新增自定义规则的草稿
  const [newPattern, setNewPattern] = useState('')
  const [newLabel, setNewLabel] = useState('')
  const [newRuleErr, setNewRuleErr] = useState('')

  /** 命中普通（逐行）屏蔽规则 */
  const isHeartbeat = (text: string) => regRef.current.some((re) => re.test(text))
  /** 命中**块开始**规则 */
  const startsBlock = (text: string) => blockReRef.current.some((re) => re.test(text))

  /**
   * 复制成功后的短暂提示。
   *
   * 为什么要有：终端里按 Ctrl+C 是**静默**的（没有浏览器那种选中反馈），
   * 用户无法确认到底复制上没有 —— 尤其剪贴板 API 在 http 下会失败。
   * 给一个一闪而过的"已复制 N 行"提示，能立刻消除这个疑问。
   */
  const [copiedTip, setCopiedTip] = useState('')
  const tipTimerRef = useRef<any>(null)

  /**
   * 自绘右键菜单的位置（null = 不显示）。
   *
   * ⚠️ 2026-10-06：用户反馈"右键在浏览器中存在其他功能" ——
   * 我们挂在终端容器上的 `contextmenu` 阻止默认没生效（xterm 内部也会处理它，
   * 事件到不了我们这里）。改为在容器上用**捕获阶段**拦下来，
   * 并弹出自己的菜单，彻底避开与浏览器/终端自身菜单的冲突。
   */
  const [ctxMenu, setCtxMenu] = useState<{ x: number; y: number } | null>(null)

  /**
   * 当前环境是否是**安全上下文**。
   *
   * 为什么要在界面上区分（这是本轮最重要的一条认知）：
   *   `navigator.clipboard` 只在 https（有效证书）或 localhost 下存在。
   *   面板常以 `http://<IP>` 访问 —— 那时浏览器**根本不提供**写剪贴板的 API，
   *   只能用 `document.execCommand('copy')`，而它**要求文档处于已聚焦上下文**，
   *   于是表现为"必须先点一下终端才能复制"。
   *
   * 这是浏览器的安全策略，**代码绕不过去**。所以界面要如实说明，
   * 而不是让用户以为是自己没操作对。
   */
  const secureCtx = typeof window !== 'undefined' && window.isSecureContext === true
  const flashCopied = () => {
    const sel = term.current?.getSelection() || ''
    const lines = sel ? sel.split('\n').length : 0
    setCopiedTip(`已复制 ${lines} 行`)
    clearTimeout(tipTimerRef.current)
    tipTimerRef.current = setTimeout(() => setCopiedTip(''), 1500)
  }

  /**
   * 一行是否应被屏蔽（考虑：规则命中 + 命令回显保护窗口）。
   *
   * 命令回显保护优先：用户在窗口内发的命令，其输出一律显示，
   * 否则 `list` 这类命令会出现"发了但什么都没看到"的困惑。
   */
  const blockedLine = (text: string) => {
    if (Date.now() - cmdWindowRef.current < 3000) return false // 命令回显保护
    return isHeartbeat(text)
  }

  // 收到的行缓冲 + "尚未收到换行的半行"。
  //
  // 为什么要自己留一份缓冲：xterm 只能"往里写"，没法把已写的行藏起来。
  // 想在切换筛选时**连历史一起**过滤（而不是只影响之后的新行），
  // 就必须能重放 —— 切筛选时清屏，再把符合条件的行重新写一遍。
  const bufRef = useRef<BufferedLine[]>([])
  const pendingRef = useRef<PendingLine | null>(null)

  /**
   * 缓冲的保留条数（=「保留行数」滑块的值）。
   *
   * 用 ref 而不是直接在回调里读 opts：onFrame 是在建立连接的那个 effect 里定义的，
   * 直接闭包 opts 会拿到**建立连接那一刻**的旧值（用户后来改了滑块就不生效了）。
   */
  const retainRef = useRef(clampRetain(BUFFER_MAX))

  /**
   * 收一行进缓冲，并按「保留行数」裁剪 —— **只数非刷屏行**。
   *
   * 用户 2026-10-17 要求：被屏蔽的周期性刷屏不该占用保留行数的计数。
   * 被屏蔽的行本身仍然留在缓冲里（关掉屏蔽要能立刻看到，见上面的说明），
   * 只是**不参与计数**；另外还有一道总行数硬上限防止内存被刷屏撑爆（见 consoleBuffer.ts）。
   *
   * 两个写入点（实时输出、本地命令回显）都走这里，保证口径只有一个。
   */
  const pushBuf = (entry: BufferedLine) => {
    const buf = bufRef.current
    buf.push(entry)
    const retain = retainRef.current
    if (needsTrim(buf.length, retain)) {
      const kept = trimByUseful(buf, retain)
      // 原地裁剪（splice）而不是替换数组：别处可能还持有这个数组的引用
      if (kept.length < buf.length) buf.splice(0, buf.length - kept.length)
    }
  }

  /** 是否显示某级别的行 */
  const visible = (lv: Level, f: Filter) => f === 'all' || f === lv

  /**
   * 一行是否应当显示。
   *
   * 两个条件都要满足：级别筛选通过、且（若开了屏蔽）不是心跳行。
   * 心跳行在缓冲里**保留**（不删），这样关掉开关就能立刻恢复出来 ——
   * 丢掉的话用户想看原始输出只能重连。
   *
   * ⚠️ 注意块状规则（Health Report）**不在这里判**：它只看单行，
   * 无法知道"这一行是不是某个块的一部分"。块由 replay/onFrame 单独处理。
   */
  const shouldShow = (line: BufferedLine) =>
    visible(line.level, filterRef.current) &&
    !(hideHbRef.current && blockedLine(line.text))

  /**
   * 把缓冲里符合条件的行重写到终端（切换筛选/切换屏蔽时用）。
   *
   * ⚠️ 必须重建**块状态**：缓冲是原始行序列，块边界要靠顺序扫描才能确定
   *（`Server Health Report` 之后的十几行都属于同一块）。
   * 所以这里用一个 `inBlock` 标志顺序推进，而不是对每行独立判断。
   */
  const replay = (f: Filter, hideHb: boolean) => {
    const t = term.current
    if (!t) return
    t.reset()
    let hidden = 0
    let inBlock = false

    for (const line of bufRef.current) {
      if (!visible(line.level, f)) continue

      if (hideHb) {
        // 块开始：进入块，并隐藏这一行
        if (startsBlock(line.text)) {
          inBlock = true
          hidden++
          continue
        }
        // 块内续行：一直隐藏到出现"不像续行"的行
        if (inBlock) {
          if (isBlockContinuation(line.text)) {
            hidden++
            continue
          }
          inBlock = false // 块自然结束，这一行照常显示
        }
        if (blockedLine(line.text)) {
          hidden++
          continue
        }
      }

      // 级别配色按当前主题重映射（见 paintLine）
      t.write(paintLine(line.text, line.level))
    }

    if (pendingRef.current) {
      const p = pendingRef.current
      if (visible(p.level, f) && !(hideHb && (blockedLine(p.text) || inBlock || startsBlock(p.text)))) {
        t.write(p.text)
      }
    }
    setHbHidden(hidden)
  }

  /**
   * 一行是否像"多行块的续行"。
   *
   * 判据（针对 Folia Health Report）：以 ` - ` 开头的条目行、缩进的数值行、
   * `Chunks:` / `Region around block` / `Highest N utilisation` 等。
   *
   * ⚠️ 这个判据**故意收紧**：宁可让块提前结束（多显示几行），
   * 也不要把无关内容吞掉 —— 用户已经因为"list 被吃掉"报过一次了。
   */
  const isBlockContinuation = (text: string) =>
    /^(\s*-\s|\s+\d|\s*Region around block|\s*Chunks:|\s*Highest \d+ utilisation|\s*\d+\.\d+% util at)/i.test(text)

  /** 重算各级别行数 */
  const recount = () => {
    const c: Record<Level, number> = { info: 0, warn: 0, error: 0 }
    for (const l of bufRef.current) c[l.level]++
    setCounts(c)
  }

  // 终端初始化 + WebSocket 接入
  useEffect(() => {
    if (!termRef.current) return

    // 用**当前**的显示参数初始化（用户在设置里改过的话要生效）
    const t = new Terminal({
      cursorBlink: opts.cursorBlink,
      fontSize: opts.fontSize,
      fontFamily: cssVar('--mono', "'SFMono-Regular', Consolas, monospace"),
      theme: termTheme(),
      convertEol: true,
      disableStdin: true, // 终端只读，命令走独立输入框
      scrollback: opts.scrollback,
    })
    const fit = new FitAddon()
    t.loadAddon(fit)
    t.open(termRef.current)
    fit.fit()
    term.current = t
    fitRef.current = fit

    /**
     * 剪贴板与键盘交互（用户 2026-10-06 要求"复制能不能做到和键盘快捷键交互"）。
     *
     * 终端是**只读**的（`disableStdin: true`，命令走下面的输入框），
     * 所以这里只需要处理"选中的文本怎么复制出去"，不需要处理粘贴输入。
     *
     * 提供三种方式，覆盖不同的肌肉记忆：
     *   · Ctrl/Cmd+Shift+C —— 终端里的惯例（Linux 终端通用）
     *   · Ctrl/Cmd+C —— **只在有选中文本时**接管，否则放行给浏览器（复制页面其它内容）
     *   · 右键 —— Windows 用户习惯；选中内容时复制，否则粘贴到命令输入框
     *
     * 为什么不直接用 xterm 自带的 selection + 浏览器默认行为：
     *   终端用 canvas 渲染，浏览器的"复制"拿不到选中文本，
     *   必须走 xterm 的 `getSelection()` 手动写剪贴板。
     */
    /**
     * 写文本到剪贴板，返回是否**确实成功**。
     *
     * ⚠️ 2026-10-06 第二次修「Ctrl+C 要点击终端才有用」（用户实测）：
     *
     * 前一次修只把 Ctrl+C 与 Ctrl+Shift+C 做成等价，但用户反馈**仍然要点终端**。
     * 真正的原因在这里 —— 旧实现把"调用 API"当成了"复制成功"：
     *
     *   if (navigator.clipboard?.writeText) {
     *     navigator.clipboard.writeText(sel).catch(...)   // ← 没有 await！
     *     return true                                      // ← 直接当成功
     *   }
     *
     * `navigator.clipboard` **只在安全上下文**（https / localhost）才有。
     * 面板常以 `http://<IP>` 访问，那时它是 undefined，会走 execCommand 分支；
     * 而 `document.execCommand('copy')` **要求文档处于可编辑/已聚焦的上下文**，
     * 否则静默失败 —— 表现就是"必须先点一下终端"（点一下把焦点给了终端，
     * execCommand 才生效）。这完全对应用户的观察。
     *
     * 现在改成：**两条路都试，且如实返回结果**（await + 检查返回值），
     * 失败时上层会给出"复制失败"的提示，而不是假装成功。
     */
    const writeClipboard = async (text: string): Promise<boolean> => {
      // 路子 1：现代 API（需要安全上下文）。它也会因"用户未激活"而 reject。
      if (navigator.clipboard?.writeText) {
        try {
          await navigator.clipboard.writeText(text)
          return true
        } catch { /* 落到下面的兜底 */ }
      }
      // 路子 2：execCommand 兜底。
      // 关键在于先把焦点交给一个**临时 textarea** 并 select() ——
      // 这样即便用户此前点在别处，复制也能成功（不再依赖"点过终端"）。
      try {
        const ta = document.createElement('textarea')
        ta.value = text
        // 放在视口内但不可见：某些浏览器对 display:none / opacity:0 的元素
        // 拒绝 select()，所以用"移出可视区 + 1px"这种更稳妥的写法。
        ta.style.position = 'fixed'
        ta.style.top = '-9999px'
        ta.style.left = '-9999px'
        ta.setAttribute('readonly', '')
        document.body.appendChild(ta)
        const prevFocus = document.activeElement as HTMLElement | null
        ta.select()
        ta.setSelectionRange(0, text.length)
        const ok = document.execCommand('copy')
        document.body.removeChild(ta)
        // 把焦点还回去，避免打断用户正在输入的命令
        prevFocus?.focus?.()
        return ok
      } catch {
        return false
      }
    }

    const copySelection = async (): Promise<boolean> => {
      const sel = t.getSelection()
      if (!sel) return false
      return writeClipboard(sel)
    }

    const onKeyDown = (e: KeyboardEvent) => {
      // ⚠️ 2026-10-06 用户实测两轮：
      //   第一次报"只有 Ctrl+Shift+C 能复制"；
      //   第二次报"Ctrl+C 仍然要先点一下终端"。
      // 根因见 writeClipboard 的注释（execCommand 需要聚焦上下文，
      // 而旧实现没 await、把"调用"当成了"成功"）。
      //
      // 现在的策略：Ctrl+C 与 Ctrl+Shift+C 完全等价，只要有选区就复制；
      // 复制结果**如实反馈**（成功显示"已复制 N 行"，失败显示失败原因），
      // 不再出现"看起来按了但没进剪贴板"的情况。
      const isC = e.key === 'C' || e.key === 'c'
      const mod = e.ctrlKey || e.metaKey
      if (mod && isC) {
        const sel = t.getSelection()
        if (!sel) return // 没有选区：放行给浏览器（用户在别处选了文字要复制）
        e.preventDefault()
        void copySelection().then((ok) => {
          if (ok) flashCopied()
          else setCopiedTip('复制失败：浏览器拒绝了剪贴板访问')
        })
        return
      }
      // Ctrl/Cmd + A：选中终端全部可见内容。
      //
      // ⚠️ 2026-10-06 用户反馈"Ctrl+A 也不正常"。原因是判定条件太脆弱：
      // 终端是 canvas，`document.activeElement` 常常不是它，
      // 于是有时**没生效**（用户以为按了），有时**又抢走**了别处（如命令输入框）的全选。
      //
      // 现在收紧为：**只有当焦点确实在终端容器内、且不在输入框里**才接管。
      // 否则放行给浏览器（输入框里的全选是常规操作，绝不能被抢）。
      // 另外头部还提供「全选」按钮 —— 快捷键判定不准时用它。
      if (mod && !e.shiftKey && (e.key === 'A' || e.key === 'a')) {
        const ae = document.activeElement
        const isTextField = ae instanceof HTMLInputElement ||
          ae instanceof HTMLTextAreaElement ||
          (ae instanceof HTMLElement && ae.isContentEditable)
        const inTerm = ae === termRef.current ||
          (ae instanceof Node && !!termRef.current?.contains(ae))
        if (inTerm && !isTextField) {
          e.preventDefault()
          t.selectAll()
        }
        return
      }
    }
    // 右键菜单：**自绘**，不再与浏览器/终端自带的菜单冲突。
    //
    // ⚠️ 2026-10-06 用户反馈"右键在浏览器中存在其他功能"：
    // 原先绑在 termRef 上的 listener 用冒泡阶段，而 xterm 内部也处理
    // contextmenu，事件可能到不了我们这里 → 浏览器默认菜单照样弹出来。
    // 现在用**捕获阶段**（第三个参数 true）+ 直接挂在容器上，
    // 保证在 xterm 之前拿到事件。
    const onContextMenu = (e: MouseEvent) => {
      e.preventDefault()
      e.stopPropagation()
      // 贴近指针弹出；并做视口边界收束，避免菜单跑出屏幕
      const mw = 180, mh = 132
      const x = Math.min(e.clientX, window.innerWidth - mw - 8)
      const y = Math.min(e.clientY, window.innerHeight - mh - 8)
      setCtxMenu({ x, y })
    }

    // 把"复制全部可见内容"挂到 ref 上，供头部按钮调用。
    //
    // 为什么要有按钮（2026-10-06 用户反馈 Ctrl+C 不好使）：
    //   在非安全上下文（http + IP）下，浏览器不给网页直接写剪贴板的权限，
    //   只能靠 execCommand，而它要求"用户激活 + 聚焦"。
    //   **按钮点击天然满足"用户激活"**，所以按钮是最可靠的路径。
    //   ⚠️ 限制要说清：拷的是**前端缓冲里的行**（受"最近 500 行"回放上限
    //   与屏蔽规则影响），不是完整的 console.log —— 提示文案里要写明。
    copyAllRef.current = async () => {
      const lines = bufRef.current
        .filter((l) => visible(l.level, filterRef.current))
        .filter((l) => !(hideHbRef.current && blockedLine(l.text)))
        .map((l) => stripAnsi(l.text))
        .join('')
      if (!lines) return
      // 走同一个 writeClipboard：它会在 http（非安全上下文）下自动
      // 退到 execCommand，并**如实返回成功与否**。
      const ok = await writeClipboard(lines)
      if (ok) {
        setCopiedTip(`已复制 ${lines.split('\n').length - 1} 行`)
        clearTimeout(tipTimerRef.current)
        tipTimerRef.current = setTimeout(() => setCopiedTip(''), 1800)
      } else {
        // 失败时**明确告诉用户原因与替代办法**，而不是只说"失败"
        setCopiedTip(secureCtx
          ? '复制失败：浏览器拒绝了剪贴板访问'
          : '复制失败：请先点击终端或日志区域，再重试')
        clearTimeout(tipTimerRef.current)
        tipTimerRef.current = setTimeout(() => setCopiedTip(''), 3200)
      }
    }

    // 全选（给按钮用，不依赖 Ctrl+A 的焦点判定）
    selectAllRef.current = () => {
      t.selectAll()
      // 让终端拿到焦点：这样用户紧接着按 Ctrl+C 能成功
      try { t.focus() } catch { /* ignoreReadOnly 时可能不支持 */ }
    }

    // 复制选中（给右键菜单用）
    copySelRef.current = () => copySelection()

    window.addEventListener('keydown', onKeyDown)
    // ⚠️ 捕获阶段（第三参数 true）：必须在 xterm 自己处理 contextmenu 之前拿到它，
    // 否则 xterm / 浏览器的默认菜单会先弹出（用户实测的"右键有其他功能"）。
    termRef.current.addEventListener('contextmenu', onContextMenu, true)

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
     *
     * ⚠️ 2026-10-02 修「输出两遍」：合并之后**只能写新到的那一段**。
     * 原来的写法是把合并结果整行写出去，而半行在前一片到达时已经写过一次，
     * 于是 `partial` + `more\n` 在屏幕上变成 `partialpartialmore`（前缀重复）。
     * 这不是理论问题：Daemon 的 tailLog 会把"攒了一会儿还没换行的残行"先 flush
     * 出来（见 mcprocess.process.go 的 flushPending），容器化的实例输出又是按
     * chunk 到达的，所以长行、进度条、日志尾部回放都会命中。
     * `shown` 就是为此存在的：记住"这个前缀已经写过了"。
     */
    const onFrame = (f: ConsoleFrame) => {
      const data = f.data ?? ''
      if (!data) return
      const prev = pendingRef.current
      const lv = prev ? prev.level : f.level
      const text = (prev?.text ?? '') + data

      // 按换行切分：除最后一段外都是完整行
      const parts = text.split('\n')
      const tail = parts.pop() ?? ''
      let grew = false
      // 已经写出去的字符数，只对**第一段**有意义（后面的段落整段都是新的）
      let written = prev?.shown ?? 0
      for (let i = 0; i < parts.length; i++) {
        const line = parts[i] + '\n'
        const out = i === 0 ? line.slice(written) : line
        // 实时通道也要过**块判定**（与 replay 一致）：
        //   块开始 → 隐藏并进入块；块续行 → 隐藏；否则按逐行规则
        let hideLive = false
        if (hideHbRef.current) {
          if (startsBlock(line)) {
            liveBlockRef.current = true
            hideLive = true
          } else if (liveBlockRef.current) {
            if (isBlockContinuation(line)) hideLive = true
            else liveBlockRef.current = false // 块结束，这行照常显示
          }
          if (!hideLive && blockedLine(line)) hideLive = true
        }
        // spam 标记记在缓冲里：裁剪时按"非刷屏行"计数，被屏蔽的行不占保留额度
        pushBuf({ level: lv, text: line, spam: hideLive })
        if (out && !hideLive) t.write(paintLine(out, lv))
        grew = true
      }
      if (parts.length > 0) written = 0

      // 还有没带换行的残句：只写新到的那一段，并记住已经写到哪
      if (tail) {
        const delta = tail.slice(written)
        const pend: PendingLine = { level: lv, text: tail, shown: tail.length }
        const hidePend = hideHbRef.current &&
          (liveBlockRef.current || startsBlock(pend.text) || blockedLine(pend.text))
        if (delta && visible(pend.level, filterRef.current) && !hidePend) t.write(paintLine(delta, pend.level))
        pendingRef.current = pend
      } else {
        pendingRef.current = null
      }

      if (grew) {
        // ⚠️ 这里**不再**按 BUFFER_MAX 硬裁。
        // 2026-10-21 修：F14 把裁剪改成"按非刷屏行、条数取设置里的保留行数"（见
        // pushBuf / consoleBuffer.ts），而这一段老代码还留着 —— 于是保留行数设成
        // 超过 5000（滑块上限 50000）时，缓冲仍被这里砍到 5000，滑块看起来"没作用"。
        // 裁剪口径只能有一处，就是 pushBuf。
        recount()
      }
    }

    const connect = () => {
      if (disposed) return

      // ⚠️ 2026-10-06 修「控制台重复显示」：
      // 上一代连接如果还没完全关闭（close 是异步的），它的 onmessage 仍会往
      // **同一个终端**写 —— 于是新旧两条流的帧叠加，表现为整屏重复。
      // 这里在新建之前先把旧连接彻底摘掉（回调置空 + close），
      // 再赋新的，保证任何时刻只有一条流在写终端。
      if (ws) {
        ws.onopen = ws.onmessage = ws.onerror = ws.onclose = null
        try { ws.close() } catch { /* 已关闭 */ }
        ws = null
      }

      // ⚠️ 没有令牌就不该发起连接（见 consoleWsUrl 的说明）：
      // 拼出 `?token=` 的请求必然 401，而 WS 握手失败读不到响应体，
      // 用户只能看到笼统的"连接错误"，白白刷一屏报错。
      // 这里直接给出可操作的提示，并交给全局的"会话失效"流程把界面切回登录页。
      const url = consoleWsUrl(instanceId, spamCfgRef.current || undefined)
      if (!url) {
        setConnected(false)
        t.writeln('\x1b[91m[未登录或登录状态已失效，请刷新页面并重新登录]\x1b[0m')
        notifyAuthExpired('登录状态已失效，请重新登录')
        return
      }

      ws = new WebSocket(url)
      wsRef.current = ws

      // 每次连接前先把"本地已写入状态"清干净，并且**用一个世代号**把
      // 上一次连接的迟到帧挡掉（见下面 gen 的说明）。
      const myGen = ++connGenRef.current

      // ⚠️ 下面每个回调都要先判 disposed + 世代：清理函数里有 ws.close()，
      // 而 onclose 是**异步**触发的 —— 它会在 t.dispose() 之后才跑到，
      // 于是往一个已销毁的终端上写日志，抛
      // TypeError: Cannot read properties of undefined (reading 'dimensions')
      //（xterm 内部去读已被释放的 renderService）。切页签时必然触发。
      ws.onopen = () => {
        if (disposed || myGen !== connGenRef.current) return
        setConnected(true)
        // 服务端会回放最近日志，先重置本地缓冲避免重连后重复
        t.reset()
        bufRef.current = []
        pendingRef.current = null
        recount()
        t.writeln('\x1b[90m[控制台已连接]\x1b[0m')
      }
      ws.onmessage = (e) => {
        // 迟到的旧连接帧直接丢弃 —— 这是"重复显示"的主要来源
        if (disposed || myGen !== connGenRef.current) return
        // 面板发的是 JSON（带级别）；解析失败就按原始文本兜底 ——
        // 协议将来若再变，最坏也只是丢掉筛选能力，不会整屏空白。
        try {
          onFrame(JSON.parse(String(e.data)) as ConsoleFrame)
        } catch {
          t.write(String(e.data))
        }
      }
      ws.onerror = () => {
        if (disposed || myGen !== connGenRef.current) return
        setConnected(false)
        // 不再只写一句笼统的"连接错误"：真正的判断放在 onclose 里，
        // 那里能区分"登录态失效"与"网络问题"，并给出可操作的提示。
        // 这里只记一个标记，避免重复刷消息（onerror 总是先于 onclose 触发）。
        errRef.current = true
      }
      ws.onclose = (ev) => {
        if (disposed || myGen !== connGenRef.current) return
        setConnected(false)

        // ⚠️ 2026-10-06：把"连接失败"分成几类，给出**可操作**的提示。
        //
        // 起因（用户实测）：控制台一直显示"连接错误"，浏览器控制台里刷了
        // 281 条 `WebSocket connection to 'wss://…' failed:
        // Error in connection establishment: net::ERR_CERT_AUTHORITY_INVALID`，
        // 但界面上只有一句笼统的"连接错误"，用户完全不知道该怎么办。
        //
        // ⚠️ 局限要写清楚：**WebSocket 握手失败时，浏览器不把 HTTP 状态码或
        // 响应体交给 JS**（onclose 事件里只有 code/reason，且握手失败时
        // code 通常是 1006）。所以这里只能靠"事件里能拿到的信息 + 一次探针请求"
        // 来反推原因，做不到精确。
        //   · ev.code === 1006 表示"异常关闭"（含握手被拒、TLS 失败、网络中断）
        //   · 具体是哪一种，靠下面的探针与 url 协议来判断
        const isAbnormal = ev.code === 1006
        const isHTTPS = window.location.protocol === 'https:'

        const check = async () => {
          // 1) 先看服务端是否认这个令牌（能识别"令牌被撤销但 exp 未到"）
          const rejected = isTokenExpired() || await tokenRejectedByServer()
          if (disposed || myGen !== connGenRef.current) return
          if (rejected) {
            t.writeln('\x1b[91m[登录状态已失效，请刷新页面并重新登录]\x1b[0m')
            notifyAuthExpired('登录状态已失效，请重新登录')
            return
          }

          // 2) 令牌有效却仍连不上：HTTPS 下最常见的原因是**证书不受信任** ——
          //    页面可以点"继续访问"，但 WebSocket 没有这个机会，会被直接拒绝。
          //    这条对使用者很有用，所以明确写出来。
          if (isAbnormal && isHTTPS) {
            t.writeln('\x1b[91m[连接失败]\x1b[0m 可能的原因与处理办法：')
            t.writeln('\x1b[90m  · 服务器证书不受信任（HTTPS 下最常见）：')
            t.writeln('    浏览器允许你"继续访问"页面，但 WebSocket 没有这个选择。')
            t.writeln('    处理：用 HTTP 端口访问面板，或为面板配置受信任的证书')
            t.writeln('    （生产环境建议申请正式证书，如阿里云测试证书 / Let\'s Encrypt）。\x1b[0m')
            t.writeln('\x1b[90m  · 登录态异常：刷新页面并重新登录。\x1b[0m')
          } else {
            t.writeln('\x1b[90m[连接已断开，3 秒后重连…]（若持续失败，请刷新页面并重新登录）\x1b[0m')
          }
          retryTimer = setTimeout(connect, 3000)
        }
        void check()
      }
    }
    connect()

    return () => {
      disposed = true
      clearTimeout(retryTimer)
      window.removeEventListener('resize', onResize)
      window.removeEventListener('keydown', onKeyDown)
      termRef.current?.removeEventListener('contextmenu', onContextMenu, true)
      copyAllRef.current = null
      selectAllRef.current = null
      copySelRef.current = null
      clearTimeout(tipTimerRef.current)
      // 世代号自增：让所有还在路上的旧回调（message/close/error）立刻失效。
      // 只置 disposed 不够 —— 回调闭包读的是外层的 disposed，而它已经是 true，
      // 但**重连定时器里新建的那条连接**持有的是自己的闭包；
      // 自增世代号能一次性废掉全部旧世代，语义更干净。
      connGenRef.current++
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

  // 切换筛选 / 切换心跳屏蔽：重建视图（连历史一起过滤）
  //
  // 两个开关都走这里：它们是正交的两个维度，但都需要"清屏 + 重放"
  // 才能让**已经收到的那部分历史**也按新条件显示。
  useEffect(() => {
    filterRef.current = filter
    hideHbRef.current = hideHeartbeat
    if (!term.current) return
    replay(filter, hideHeartbeat)
    term.current.scrollToBottom()
  }, [filter, hideHeartbeat])

  // 屏蔽规则变化 → 重编译正则、持久化、并**连历史一起重放**
  //（用户改了规则后，已经显示出来的历史也要按新规则过滤，否则得重连才生效）
  useEffect(() => {
    const out: RegExp[] = []
    const blocks: RegExp[] = []
    for (const r of HEARTBEAT_PRESETS) {
      if (!ruleEnabled[r.id]) continue
      // 块状规则进 blockReRef（由 replay/onFrame 按块处理）；
      // 普通规则进 regRef（逐行判定）。
      if (r.block) blocks.push(r.re)
      else out.push(r.re)
    }
    for (const c of customRules) {
      try {
        const re = new RegExp(c.pattern, 'i')
        // 自定义规则一律按**逐行**处理（块语义对用户来说太隐晦）
        if (ruleEnabled[c.id] !== false) out.push(re)
      } catch { /* 非法正则跳过（表单里会标红） */ }
    }
    regRef.current = out
    blockReRef.current = blocks
    try { localStorage.setItem(RULES_KEY, JSON.stringify({ enabled: ruleEnabled, custom: customRules })) } catch { /* 隐私模式忽略 */ }
    replay(filterRef.current, hideHbRef.current)
    term.current?.scrollToBottom()
  }, [ruleEnabled, customRules])

  // 规则或总开关变化 → 更新给服务端的那份规则（**不重连**，只是下次连接时带上）。
  //
  // 说明一处有意的取舍：改完规则后，**当前这条连接**里的回放仍是按旧规则生成的
  //（回放发生在附加那一刻），但前端会就地按新规则重新过滤已有内容，
  // 所以用户看到的立即是对的；新规则对"回放窗口算多少行"生效于下一次连接。
  useEffect(() => {
    spamCfgRef.current = attachPayloadFor(ruleEnabled, customRules, hideHeartbeat, opts.scrollback)
  }, [ruleEnabled, customRules, hideHeartbeat, opts.scrollback])

  // 「保留行数」变化 → 更新缓冲的保留条数，并**立刻按新值裁一次**。
  // 不立刻裁的话要等到攒够两倍才会裁剪，用户会以为"改了没生效"。
  useEffect(() => {
    const r = clampRetain(opts.scrollback)
    retainRef.current = r
    const buf = bufRef.current
    if (buf.length > r) {
      const kept = trimByUseful(buf, r)
      if (kept.length < buf.length) buf.splice(0, buf.length - kept.length)
    }
  }, [opts.scrollback])

  // 显示参数变化 → 写回终端 + 持久化 + 重新 fit（字号变了列数会变）
  useEffect(() => {
    try { localStorage.setItem(OPTS_KEY, JSON.stringify(opts)) } catch { /* 忽略 */ }
    const t = term.current
    if (!t) return
    t.options.fontSize = opts.fontSize
    t.options.lineHeight = opts.lineHeight
    t.options.scrollback = opts.scrollback
    t.options.cursorBlink = opts.cursorBlink
    // 字号/行距变了要重新计算行列，否则终端会留出空白或截断
    requestAnimationFrame(() => { try { fitRef.current?.fit() } catch { /* 未挂载 */ } })
  }, [opts])

  /** 添加一条自定义屏蔽规则（校验正则合法性） */
  const addCustomRule = () => {
    const pat = newPattern.trim()
    if (!pat) { setNewRuleErr('请输入匹配规则（正则）'); return }
    try { new RegExp(pat, 'i') } catch (e: any) { setNewRuleErr('正则不合法：' + e.message); return }
    setCustomRules((prev) => [...prev, {
      id: 'c' + Date.now().toString(36),
      label: newLabel.trim() || pat,
      pattern: pat,
    }])
    setNewPattern(''); setNewLabel(''); setNewRuleErr('')
  }

  /**
   * 实际生效的回放行数 = min(设置里的保留行数, 服务端回放上限)。
   *
   * 界面必须按这个值标注，不能写死：用户改了「保留行数」之后，
   * 徽标要跟着变，否则又会变成"改了没反应"的观感。
   */
  const replayLines = Math.min(clampRetain(opts.scrollback), REPLAY_MAX)

  const total = counts.info + counts.warn + counts.error
  const shown = filter === 'all' ? total : counts[filter as Level]
  const hidden = total - shown

  const countOf = (k: Filter) => (k === 'all' ? total : counts[k as Level])

  const sendCommand = () => {
    const ws = wsRef.current
    const cmd = input.trim()
    if (ws && ws.readyState === WebSocket.OPEN && cmd) {
      // ⚠️ 打开"命令回显保护"窗口：此后 3 秒内到达的行**不套用屏蔽规则**。
      // 原因见 cmdWindowRef 的注释 —— `list` 之类命令的回显与周期性心跳
      // 文本完全相同（都是 "There are N of a max of M players online"），
      // 只能靠时间窗区分"这是用户刚发命令得到的输出"。
      cmdWindowRef.current = Date.now()
      // 命令可能触发多行块（如 health），先把块状态清掉，避免误判
      liveBlockRef.current = false

      ws.send(cmd + '\n')
      // 本地回显：**服务端不会把控制台输入回显到 stdout**（Minecraft 只记录
      // 命令产生的输出，不记录输入本身），所以"发了什么"必须由前端自己写出来 ——
      // 否则用户敲完一条命令，屏幕上什么都没有，会以为面板没收到
      //（2026-10-02 用户反馈"没显示输入的指令"）。
      // 走缓冲一起进缓冲：切换级别筛选时重放也能看到自己发过的命令。
      // 本地回显不是刷屏（spam: false），它要占保留额度 —— 用户自己敲的命令不该被裁掉。
      pushBuf({ level: 'info' as Level, text: '\x1b[90m› ' + cmd + '\x1b[0m\n', spam: false })
      if (filterRef.current === 'all' || filterRef.current === 'info') {
        term.current?.write('\x1b[90m› ' + cmd + '\x1b[0m\n')
      }
      recount()
      term.current?.scrollToBottom()
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

        {/* 屏蔽周期性心跳（玩家数 / TPS）。默认开 —— 这两条每 5~15 秒刷一次，
            会把真正的输出冲到看不见的地方。关掉就能看到原始输出，
            缓冲里一直留着这些行，所以关掉是**立刻**恢复、不需要重连。 */}
        <button
          type="button"
          className={`cf-btn cf-hb${hideHeartbeat ? ' on' : ''}`}
          onClick={() => setHideHeartbeat((v) => !v)}
          title={
            hideHeartbeat
              ? '已屏蔽周期性刷屏日志（玩家数统计、TPS 报告）。点击显示原始输出。'
              : '正在显示全部原始输出（含玩家数统计、TPS 报告）。点击屏蔽它们。'
          }
        >
          {hideHeartbeat ? '已屏蔽刷屏' : '显示刷屏'}
          {hideHeartbeat && hbHidden > 0 && <em>{hbHidden}</em>}
        </button>

        {/* 说明"这里只有最近 N 行"，必须让人知道去哪找完整日志，否则会以为日志被丢了。
            N 不再是写死的 500：它跟着设置里的「保留行数」走（2026-10-21 用户报
            "保留行数没作用到控制台上"之后的修法），但服务端有回放上限（REPLAY_MAX），
            所以这里如实显示**实际生效**的那个值。默认只在悬停时显示详情，不占版面。 */}
        <span
          className="console-scope"
          title={
            `控制台回放最近 ${replayLines} 行（取「保留行数」与回放上限 ${REPLAY_MAX} 的较小值）。\n` +
            '每次切回本页签都会重新附加，回放太多会把屏幕刷满，所以有上限。\n' +
            '完整日志在实例的 logs/console.log，可在「文件」页查看或下载。'
          }
        >
          最近 {replayLines} 行
        </span>

        {/* 复制按钮（2026-10-06 用户反馈 Ctrl+C 不好使）。
            比键盘复制可靠：不依赖选区、不依赖焦点，点一下就是当前可见内容。
            ⚠️ 文案要说清复制的范围 —— 是前端缓冲里的行，不是完整日志文件。 */}
        <button
          type="button"
          className="cf-btn cf-copy"
          onClick={() => copyAllRef.current?.()}
          title={
            '把当前显示的内容复制到剪贴板（不含被屏蔽的行）。\n' +
            `⚠️ 只含前端已缓冲的行（受「最近 ${replayLines} 行」回放上限影响），不是完整日志。\n` +
            '完整日志在实例的 logs/console.log，可在「文件」页下载。\n' +
            (secureCtx
              ? '当前是安全上下文，复制可直接写入剪贴板。'
              : '当前不是安全上下文（http + IP）：浏览器限制了直接写剪贴板，\n' +
                '按钮点击仍可用；用 Ctrl+C 复制选中时需先点一下终端。')
          }
        >
          ⧉ 复制
        </button>

        {/* 全选按钮：Ctrl+A 的焦点判定在 canvas 终端上不可靠，给一个确定的入口 */}
        <button
          type="button"
          className="cf-btn cf-selall"
          onClick={() => selectAllRef.current?.()}
          title="选中终端里的全部内容（然后再点「⧉ 复制」即可）"
        >
          全选
        </button>

        {/* 设置按钮（用户 2026-10-06 要求）：点开是悬浮框，
            里面能调终端显示参数、以及增删屏蔽规则。 */}
        <div className="console-settings-wrap" ref={settingsBtnRef}>
          <button
            type="button"
            className={`cf-btn cf-gear${showSettings ? ' on' : ''}`}
            onClick={() => {
              // 打开前先量按钮位置 —— 面板用 position:fixed 定位（见 CSS 里的说明），
              // 必须拿到视口坐标，不能靠父元素的 absolute 定位。
              const r = settingsBtnRef.current?.getBoundingClientRect()
              if (r) setSettingsPos(computeSettingsPos(r))
              setShowSettings((v) => !v)
            }}
            title="控制台设置：字体 / 行间距 / 屏蔽规则"
            aria-expanded={showSettings}
          >
            ⚙ 设置
          </button>

          {showSettings && (
            <div
              className="console-settings"
              role="dialog"
              aria-label="控制台设置"
              style={settingsPos
                ? {
                    position: 'fixed',
                    right: settingsPos.right,
                    ...(settingsPos.top !== undefined ? { top: settingsPos.top } : {}),
                    ...(settingsPos.bottom !== undefined ? { bottom: settingsPos.bottom } : {}),
                    maxHeight: settingsPos.maxHeight,
                  }
                : undefined}
            >
              <div className="cs-head">
                <span>控制台设置</span>
                <button className="cs-close" onClick={() => setShowSettings(false)} title="关闭">✕</button>
              </div>

              {/* ---- 显示 ---- */}
              <div className="cs-sec">
                <div className="cs-sec-title">显示</div>

                <label className="cs-row">
                  <span>字号</span>
                  <input
                    type="range" min={10} max={20} step={1}
                    value={opts.fontSize}
                    onChange={(e) => setOpts({ ...opts, fontSize: Number(e.target.value) })}
                  />
                  <b>{opts.fontSize}px</b>
                </label>

                <label className="cs-row">
                  <span>行间距</span>
                  <input
                    type="range" min={10} max={20} step={1}
                    value={Math.round(opts.lineHeight * 10)}
                    onChange={(e) => setOpts({ ...opts, lineHeight: Number(e.target.value) / 10 })}
                  />
                  <b>{opts.lineHeight.toFixed(1)}</b>
                </label>

                <label className="cs-row">
                  <span>保留行数</span>
                  {/* 上限必须与 REPLAY_MAX / Daemon 的 consoleHistoryMax 一致：
                      能拖到的每一档都要真的生效，否则就是"拖了没反应"的 bug。 */}
                  <input
                    type="range" min={RETAIN_MIN} max={RETAIN_MAX} step={1000}
                    value={clampRetain(opts.scrollback)}
                    onChange={(e) => setOpts({ ...opts, scrollback: Number(e.target.value) })}
                  />
                  <b>{`${clampRetain(opts.scrollback) / 1000}k`}</b>
                </label>

                <label className="cs-check">
                  <input
                    type="checkbox" checked={opts.cursorBlink}
                    onChange={(e) => setOpts({ ...opts, cursorBlink: e.target.checked })}
                  />
                  <span>光标闪烁</span>
                </label>
              </div>

              {/* ---- 屏蔽规则 ---- */}
              <div className="cs-sec">
                <div className="cs-sec-title">
                  刷屏屏蔽规则
                  <button className="cs-reset" onClick={() => {
                    const d: Record<string, boolean> = {}
                    for (const r of HEARTBEAT_PRESETS) d[r.id] = r.byDefault
                    setRuleEnabled(d)
                  }}>恢复默认</button>
                </div>

                {/* 预制规则（按核心泛化，用户可逐条开关）*/}
                {HEARTBEAT_PRESETS.map((r) => (
                  <label className="cs-check cs-rule" key={r.id} title={r.note}>
                    <input
                      type="checkbox"
                      checked={!!ruleEnabled[r.id]}
                      onChange={(e) => setRuleEnabled({ ...ruleEnabled, [r.id]: e.target.checked })}
                    />
                    <span className="cs-rule-label">{r.label}</span>
                  </label>
                ))}

                {/* 自定义规则 */}
                {customRules.length > 0 && (
                  <div className="cs-custom">
                    {customRules.map((c) => (
                      <div className="cs-custom-item" key={c.id} title={c.pattern}>
                        <input
                          type="checkbox"
                          checked={ruleEnabled[c.id] !== false}
                          onChange={(e) => setRuleEnabled({ ...ruleEnabled, [c.id]: e.target.checked })}
                        />
                        <span className="cs-rule-label">{c.label}</span>
                        <code>{c.pattern}</code>
                        <button
                          className="cs-del"
                          onClick={() => setCustomRules((prev) => prev.filter((x) => x.id !== c.id))}
                          title="删除这条规则"
                        >✕</button>
                      </div>
                    ))}
                  </div>
                )}

                {/* 新增 */}
                <div className="cs-add">
                  <input
                    className="cs-add-pat"
                    placeholder="正则，如：joined the game"
                    value={newPattern}
                    spellCheck={false}
                    onChange={(e) => { setNewPattern(e.target.value); setNewRuleErr('') }}
                  />
                  <input
                    className="cs-add-label"
                    placeholder="名称（可选）"
                    value={newLabel}
                    onChange={(e) => setNewLabel(e.target.value)}
                  />
                  <button onClick={addCustomRule}>添加</button>
                </div>
                {newRuleErr && <div className="cs-err">{newRuleErr}</div>}
                <div className="cs-hint">
                  规则用<strong>正则</strong>匹配整行（不区分大小写）；命中的行只从**显示**里隐藏，
                  日志文件不受影响。已预置 Paper / Folia 等常见核心的周期性输出。
                  <br />
                  <strong>不会误伤你自己的命令</strong>：发送命令后 3 秒内的输出一律显示
                  （`list` 的回显与周期性人数统计文本完全相同，靠时间窗区分）。
                </div>
              </div>

              {/* ---- 复制操作说明 ---- */}
              <div className="cs-sec">
                <div className="cs-sec-title">复制</div>
                <div className="cs-hint" style={{ marginTop: 0 }}>
                  · <strong>「⧉ 复制」按钮</strong>：一键复制当前显示的全部内容（最可靠）<br />
                  · <strong>「全选」按钮</strong>：选中终端全部内容，再点复制<br />
                  · 右键：弹出本面板自己的菜单（复制选中 / 复制全部 / 全选 / 粘贴）<br />
                  · <kbd>Ctrl</kbd>+<kbd>C</kbd> 或 <kbd>Ctrl</kbd>+<kbd>Shift</kbd>+<kbd>C</kbd>：
                  复制选中的文字
                </div>

                {/*
                  ⚠️ 这段说明是必要的（2026-10-06 用户实测两轮才定位）：
                  在非安全上下文下，浏览器**不提供**写剪贴板的权限，
                  只能退到 execCommand，而它要求文档已聚焦 ——
                  于是"拖选后按 Ctrl+C"必须先点一下终端。
                  这是浏览器的安全策略，代码绕不过去，所以要如实告诉用户，
                  并指出"按钮"与"配有效证书"两条更好的路。
                */}
                {!secureCtx && (
                  <div className="cs-note-warn">
                    <strong>当前页面不是安全上下文</strong>（通过 <code>http://</code> + IP 访问）。<br />
                    浏览器在这种页面上<strong>不允许</strong>网页直接写剪贴板，
                    因此用 <kbd>Ctrl</kbd>+<kbd>C</kbd> 复制选中内容时<strong>需要先点击一下终端</strong>。<br />
                    想彻底免掉这一步：使用 <code>https://</code> 且证书受信任（生产环境配正式证书），
                    或改用 <code>http://localhost</code> 访问。
                  </div>
                )}
              </div>
            </div>
          )}
        </div>

        {/* 连接状态。未连接时给出**悬停说明**：终端的滚动区里已经写了
            断开原因（登录失效会明确提示），但用户未必往上翻 —— 这里补一句
            让鼠标停在徽章上就能看到，减少"显示未连接但不知道怎么办"。 */}
        <span
          className={`console-status ${connected ? 'on' : 'off'}`}
          title={
            connected
              ? '控制台已连接，正在实时接收输出'
              : '控制台未连接。常见原因：登录状态已失效（请重新登录）、面板不可达。' +
                '终端里会给出具体原因。'
          }
        >
          {connected ? '已连接' : '未连接'}
        </span>

        {/* 复制提示：终端复制是静默的，给一个一闪而过的确认 */}
        {copiedTip && <span className="console-tip">{copiedTip}</span>}
      </div>

      <div className="console-body" ref={termRef} />

      {/* 自绘右键菜单。
          为什么要自己画（2026-10-06 用户反馈"右键在浏览器中存在其他功能"）：
          浏览器/终端自带的右键菜单在这里既抢事件又语义混乱。
          自绘一个只有"复制/粘贴/全选"的菜单，行为完全可控。
          点击任意处关闭（用一个覆盖全屏的透明层接住点击）。 */}
      {ctxMenu && (
        <>
          <div
            className="ctx-backdrop"
            onClick={() => setCtxMenu(null)}
            onContextMenu={(e) => { e.preventDefault(); setCtxMenu(null) }}
          />
          <div className="ctx-menu" style={{ left: ctxMenu.x, top: ctxMenu.y }} role="menu">
            <button
              role="menuitem"
              onClick={async () => {
                setCtxMenu(null)
                const ok = await copySelRef.current?.()
                if (ok) setCopiedTip('已复制选中内容')
                else setCopiedTip('没有选中内容（先拖选一段文字）')
                clearTimeout(tipTimerRef.current)
                tipTimerRef.current = setTimeout(() => setCopiedTip(''), 2200)
              }}
            >
              复制选中
            </button>
            <button
              role="menuitem"
              onClick={() => { setCtxMenu(null); copyAllRef.current?.() }}
            >
              复制全部
            </button>
            <button
              role="menuitem"
              onClick={() => { setCtxMenu(null); selectAllRef.current?.() }}
            >
              全选
            </button>
            <button
              role="menuitem"
              disabled={!canSend}
              onClick={async () => {
                setCtxMenu(null)
                // 读剪贴板同样受安全上下文限制：http + IP 下 readText 不可用。
                // 如实告知用户，而不是静默什么都不发生。
                try {
                  const txt = await navigator.clipboard?.readText?.()
                  if (txt) setInput((v) => v + txt.trim())
                  else setCopiedTip('无法读取剪贴板（请手动 Ctrl+V 粘贴到输入框）')
                } catch {
                  setCopiedTip('无法读取剪贴板（请手动 Ctrl+V 粘贴到输入框）')
                }
                clearTimeout(tipTimerRef.current)
                tipTimerRef.current = setTimeout(() => setCopiedTip(''), 2600)
              }}
            >
              粘贴到命令框
            </button>
          </div>
        </>
      )}

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
