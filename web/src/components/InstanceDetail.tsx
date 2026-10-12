import { useEffect, useState } from 'react'
import ConsoleTab from './ConsoleTab'
import LogShareTab from './LogShareTab'
import FilesTab from './FilesTab'
import ConfigTab from './ConfigTab'
import BackupsTab from './BackupsTab'
import PlayersTab from './PlayersTab'
import GameSettingsTab from './GameSettingsTab'
import InstanceSettingsTab from './InstanceSettingsTab'
import BackupCalendar from './BackupCalendar'
import AccountCard from './AccountCard'
import StatsTab from './StatsTab'
import TasksTab from './TasksTab'
import {
  instanceAction, levelAtLeast, listInstances, currentUser, User, isNodeUser,
  getMetrics, listBackups, listInstancePorts, getSchedule, getInstanceRuntime,
  BackupItem, Metrics, Instance, InstanceRuntime, instanceIconUrl, renameInstance,
} from '../api'
import AppShell, { NavKey, NAV_ITEMS } from './AppShell'
import ThemeToggle from './ThemeToggle'
import Avatar from './Avatar'
import Brand from './Brand'
import './InstanceDetail.css'

type Tab = 'console' | 'loganalysis' | 'players' | 'gamesettings' | 'files' | 'config' | 'backups' | 'settings' | 'stats' | 'tasks'

const TABS: { key: Tab; label: string; icon: string }[] = [
  { key: 'console', label: '控制台', icon: '▸' },
  // 日志分析紧挨着控制台：它处理的就是控制台里的那些日志（崩溃、报错），
  // 排查顺序天然是"先看控制台 → 再看不懂就交给 AI 分析"
  { key: 'loganalysis', label: '日志分析', icon: '⌬' },
  { key: 'players', label: '玩家', icon: '◉' },
  // 「游戏设置」紧跟在玩家后面（用户 2026-10-05 指定位置：玩家 → 游戏设置 → 文件）：
  // 它管的是**世界本身**（世界信息 + 原版 gamerule），与"谁在玩"是同一类对象，
  // 排在文件管理之前更符合"先看内容、再碰文件"的顺路。
  { key: 'gamesettings', label: '游戏设置', icon: '◈' },
  { key: 'files', label: '文件', icon: '▤' },
  { key: 'config', label: '配置', icon: '⚙' },
  { key: 'backups', label: '备份', icon: '▣' },
  // 端口、核心、启动都属于实例运行环境，合并后放在第一组末尾便于集中管理。
  { key: 'settings', label: '实例设置', icon: '⚙' },
]

/** 分隔线之后的低频功能位 */
const TABS_EXTRA: { key: Tab; label: string; icon: string }[] = [
  // 「统计与配额」：历史趋势 + 资源上限（CPU / 内存 / 磁盘）的查看与修改。
  // 配额原先在右栏单独一张卡，与「资源占用」高度重复、还把右栏撑得很长，
  // 2026-10-04 合并到这一页 —— 看数据的地方顺带管配额。
  { key: 'stats', label: '统计与配额', icon: '◔' },
  { key: 'tasks', label: '任务', icon: '▷' },
]

/**
 * 实例页左栏的「全局导航」。
 *
 * **从 NAV_ITEMS 派生**，不再自己写一份 —— 以前这里是一份硬编码的副本，
 * 加了「账户」「公告与帮助」之后没同步，导致在实例页里进不去这两个页面。
 * 唯一的不同是中间插一个「告警」：那一项在实例页走弹窗，而不是页面。
 */
const GLOBAL_NAV: { key: NavKey | 'alerts'; label: string; icon: string; adminOnly?: boolean; providerAdmin?: boolean }[] =
  NAV_ITEMS.flatMap((it) =>
    it.key === 'tunnels'
      ? [it, { key: 'alerts' as const, label: '告警', icon: '◈', adminOnly: true }]
      : [it],
  )

function fmtBytes(n: number): string {
  if (!n) return '—'
  if (n < 1024 * 1024) return (n / 1024).toFixed(0) + ' KB'
  if (n < 1024 * 1024 * 1024) return (n / 1024 / 1024).toFixed(0) + ' MB'
  return (n / 1024 / 1024 / 1024).toFixed(1) + ' GB'
}

/** 网络速率：自动选择 B/s、KB/s、MB/s */
function fmtRate(bps?: number): string {
  if (bps === undefined || bps === null || bps < 0) return '—'
  if (bps < 1024) return `${bps} B/s`
  if (bps < 1024 * 1024) return `${(bps / 1024).toFixed(1)} KB/s`
  return `${(bps / 1024 / 1024).toFixed(2)} MB/s`
}

/** 运行时长：按量级选择 天/小时/分 */
function fmtUptime(sec?: number, status?: string): string {
  if (status && status !== 'running') return '未运行'
  if (!sec || sec <= 0) return '—'
  const d = Math.floor(sec / 86400)
  const h = Math.floor((sec % 86400) / 3600)
  const m = Math.floor((sec % 3600) / 60)
  if (d > 0) return `${d} 天 ${h} 小时`
  if (h > 0) return `${h} 小时 ${m} 分`
  return `${m} 分 ${sec % 60} 秒`
}

function parseMem(s: string): number {
  const m = /^(\d+(?:\.\d+)?)\s*([KMGT]?)B?$/i.exec((s || '').trim())
  if (!m) return 0
  const v = parseFloat(m[1])
  const unit = (m[2] || 'M').toUpperCase()
  const mul: Record<string, number> = { K: 1024, M: 1024 ** 2, G: 1024 ** 3, T: 1024 ** 4 }
  return v * (mul[unit] || 1024 ** 2)
}

/**
 * 趋势折线。
 *
 * 值映射刻意占满整个视口高度（0 → y=40，max → y=0），
 * 这样网格线与外部 HTML 刻度可以共用同一套百分比定位：
 * 值 v 对应 top = (1 - v/max) * 100%。
 * （SVG 用了 preserveAspectRatio="none" 横向拉伸，
 *   在里面直接写 <text> 会被拉变形，所以刻度必须放在 HTML 侧。）
 */
const GRID_LINES = [0, 25, 50, 75, 100] // 百分比刻度

function Spark({ points, points2, max, tone = 'accent' }: {
  points: number[]
  /**
   * 第二条线（可选）。用于"上下行速率合并到一张图"这类场景 ——
   * 两条线**共用同一 Y 轴刻度**：各自单独缩放的话，"哪条更高"会被看反。
   * 只画线、不填充，免得把第一条线的面积盖住。
   */
  points2?: number[]
  max: number
  tone?: 'accent' | 'alt'
}) {
  const grid = (
    <g className="spark-grid">
      {GRID_LINES.map((pct) => (
        <line
          key={`h${pct}`}
          className={pct === 100 ? 'spark-base' : undefined}
          x1="0"
          y1={(pct / 100) * 40}
          x2="200"
          y2={(pct / 100) * 40}
        />
      ))}
      {[25, 50, 75].map((pct) => (
        <line key={`v${pct}`} x1={(pct / 100) * 200} y1="0" x2={(pct / 100) * 200} y2="40" />
      ))}
    </g>
  )

  // 上限要把两条线都算进去，否则第二条会顶出画布
  const hi = max > 0 ? max : Math.max(...points, ...(points2 || []), 1)

  const pathOf = (arr: number[]) => {
    if (arr.length < 2) return ''
    const step = 200 / (arr.length - 1)
    return arr
      .map((v, i) => `${(i * step).toFixed(1)},${(40 - Math.min(1, Math.max(0, v / hi)) * 40).toFixed(1)}`)
      .join(' ')
  }

  const pts = pathOf(points)
  const pts2 = points2 ? pathOf(points2) : ''

  if (!pts) {
    return (
      <svg className={`spark ${tone}`} viewBox="0 0 200 40" preserveAspectRatio="none">
        {grid}
      </svg>
    )
  }

  return (
    <svg className={`spark ${tone}`} viewBox="0 0 200 40" preserveAspectRatio="none">
      {grid}
      <polygon className="spark-area" points={`0,40 ${pts} 200,40`} />
      <polyline points={pts} fill="none" strokeWidth="2" />
      {pts2 && <polyline className="spark-line2" points={pts2} fill="none" strokeWidth="2" />}
    </svg>
  )
}

/**
 * 带 Y 轴刻度的图表。
 *
 * 刻度用 HTML 绝对定位（而非 SVG <text>），避免被 preserveAspectRatio="none" 拉伸变形。
 * 顶部为最大值、底部为 0，与 Spark 的值映射严格对应。
 */
function ChartPlot({ points, points2, max, tone, fmt }: {
  points: number[]
  /** 第二条线（可选），与 points 共用同一刻度 */
  points2?: number[]
  max: number
  tone?: 'accent' | 'alt'
  fmt: (v: number) => string
}) {
  const all = points2 ? [...points, ...points2] : points
  const hi = max > 0 ? max : Math.max(...all, 1)
  return (
    <div className="chart-plot">
      <div className="chart-axis">
        {[...GRID_LINES].reverse().map((pct) => (
          <span key={pct} className="axis-label" style={{ top: `${pct}%` }}>
            {fmt((hi * (100 - pct)) / 100)}
          </span>
        ))}
      </div>
      <Spark points={points} points2={points2} max={hi} tone={tone} />
    </div>
  )
}

export default function InstanceDetail({ instanceId, name, status, level, user, alertCount, onBack, onNavigate, onOpenAccount, onOpenAlerts, onRenamed }: {
  instanceId: string
  name: string
  status: string
  level: string
  user: User | null
  alertCount: number
  onBack: () => void
  onNavigate: (k: NavKey) => void
  onOpenAccount: () => void
  onOpenAlerts: () => void
  /** 改名成功后通知外壳更新标题（实例名由 App 的 view 状态持有） */
  onRenamed?: (name: string) => void
}) {
  const [tab, setTab] = useState<Tab>('console')
  const [curStatus, setCurStatus] = useState(status)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const [msg, setMsg] = useState('')
  const [showKill, setShowKill] = useState(false)
  // 实例改名（就地编辑标题）
  const [editingName, setEditingName] = useState(false)
  const [nameDraft, setNameDraft] = useState('')
  // 注：资源限制（CPU / 内存 / 磁盘）的就地编辑已搬到「统计与配额」页
  //（见 StatsTab）—— 右栏原先那套 state 与处理函数已随之删除。

  const [inst, setInst] = useState<Instance | null>(null)
  const [metrics, setMetrics] = useState<Metrics | null>(null)
  const [backups, setBackups] = useState<BackupItem[]>([])
  const [policyDesc, setPolicyDesc] = useState('')
  const [manualKeep, setManualKeep] = useState<number | undefined>()
  const [pubAddr, setPubAddr] = useState('')
  const [cpuHist, setCpuHist] = useState<number[]>([])
  const [memHist, setMemHist] = useState<number[]>([])
  // 上下行**分开存**：控制台下方的图要把两条线画在同一张图上
  //（原来只有一条 netHist 存的是两者之和，画不出"哪条是哪条"）
  const [netRxHist, setNetRxHist] = useState<number[]>([])
  const [netTxHist, setNetTxHist] = useState<number[]>([])
  // 在线玩家与 TPS 各自一条曲线（用户要求拆成两个折线图）
  const [playersHist, setPlayersHist] = useState<number[]>([])
  const [tpsHist, setTpsHist] = useState<number[]>([])
  const [runtime, setRuntime] = useState<InstanceRuntime | null>(null)

  const canOperate = levelAtLeast(level, 'collab')
  const canWriteFiles = levelAtLeast(level, 'owner')
  const isAdminUser = user?.role === 'admin'
  // 改名与公网端口同级：拥有者级别的用户改自己实例的名字天经地义，
  // 而后端判据与此一致（见 internal/panel/httpapi/instancename.go）。
  const canRenameInstance = isAdminUser || canWriteFiles || (isNodeUser(user?.role) && level !== '')
  // 到期控制与删除同级：总管理员，或该节点上的节点用户。
  // 具体的编辑面板已搬到「任务」标签页（见 ExpiryPanel），这里只负责算权限并传下去。
  const canManageExpiry = isAdminUser || (isNodeUser(user?.role) && level !== '')
  // 资源限制（CPU / 内存 / 磁盘）的编辑权**与改名、公网端口不同**：
  // 只有总管理员与该节点的节点用户能改。后端判据是 canManageInstance
  // （见 instancelimits.go 的注释）——实例 owner 也不行。
  //
  // 为什么 owner 不行（这里曾经写错，2026-10-03 由用户实测发现）：
  // 资源上限是**运营侧的约束**，租户如果能自己改，运营方收紧之后对方点一下
  // 就能放开，这个接口等于没做。前端必须与后端口径一致，否则给出去的编辑入口
  // 只会带来一个 403。
  const canEditLimits = isAdminUser || (isNodeUser(user?.role) && level !== '')

  // ---- 全局导航折叠 ----
  // 折叠时只保留最常用的几项；其余收进「展开全部」。
  // 这两个 key 是"从实例页最可能想去的地方"：回总览、回实例列表、看节点监控。
  const NAV_ALWAYS = ['dashboard', 'instances', 'monitor']
  const [navExpanded, setNavExpanded] = useState(false)
  const navItems = GLOBAL_NAV.filter((g) =>
    (!g.adminOnly || isAdminUser) &&
    // 「分析平台」是管理员/节点用户的全局设置页，普通用户看不到（与后端权限一致）
    (!g.providerAdmin || isAdminUser || isNodeUser(user?.role)))
  const visibleNav = navExpanded ? navItems : navItems.filter((g) => NAV_ALWAYS.includes(g.key))

  // reloadInst 重新拉取本实例的记录（到期时间等字段改完要立刻反映到界面）
  const reloadInst = async () => {
    try {
      const list = await listInstances()
      const me = Array.isArray(list) ? list.find((i) => i.instance_id === instanceId) : null
      if (me) { setInst(me); setCurStatus(me.live_status || me.status) }
    } catch { /* 忽略：界面已有旧值，报错由调用方处理 */ }
  }

  useEffect(() => {
    reloadInst()
  }, [instanceId])

  // 公网入口
  //
  // ⚠️ 这里必须用**实例级**的 listInstancePorts，不能用 listTunnels：
  // `GET /api/tunnels` 是**仅总管理员**的接口，普通用户（哪怕他是这个实例的 owner）
  // 拿到的是 403，而 catch 里是静默的 —— 于是实例页顶部的"对外地址"对**所有普通用户**
  // 永远显示"未配置"，看起来像"没给他分配域名"。
  // 实测就是这样被用户报上来的（2026-09-17 内测）。
  // `GET /api/instances/{id}/ports` 对 viewer 开放，且返回的 public_address 已经
  // 把"隧道级域名 > 线路级域名 > IP:端口"的优先级算好了，正是这里要的东西。
  useEffect(() => {
    listInstancePorts(instanceId)
      .then((r) => {
        const first = (r?.ports || [])[0]
        // 直接用后端算好的 public_address：它已经按"隧道级域名 > 线路级域名 > IP"
        // 的优先级取好，并且**补上了端口**（见 publicAddress）。
        //
        // 以前这里优先取 display_domain —— 那是管理员在隧道上填的原始值，
        // 多数人只填域名不填端口，于是实例页顶部显示成 `mc.example.com`
        //（玩家拿这个地址是连不上的），而端口明明就在旁边这一行里。
        if (first) setPubAddr(first.public_address || (first as any).display_domain || '')
      })
      .catch(() => { /* 读不到就不显示，别把错误抛到界面上 */ })
  }, [instanceId])

  // 资源采样 + 趋势
  useEffect(() => {
    if (curStatus !== 'running') { setMetrics(null); return }
    let alive = true
    const tick = () =>
      getMetrics(instanceId)
        .then((m) => {
          if (!alive) return
          setMetrics(m)
          setCpuHist((h) => [...h, m.cpu_percent].slice(-24))
          setMemHist((h) => [...h, m.mem_used].slice(-24))
          setPlayersHist((h) => [...h, m.players].slice(-24))
          setTpsHist((h) => [...h, m.tps].slice(-24))
        })
        .catch(() => { /* 忽略 */ })
    tick()
    const t = window.setInterval(tick, 5000)
    return () => { alive = false; window.clearInterval(t) }
  }, [instanceId, curStatus])

  // 运行数据（运行时长 / 启停次数 / 磁盘 / 网络）：Daemon 侧数据，5 秒刷新
  //
  // ⚠️ 这里同时负责**修正实例状态**（2026-10-04 修的"必须刷新浏览器按钮才变"）：
  // 实例**非正常停止**（崩溃、被 kill、Daemon 那边停了）时，面板数据库里的 status
  // 不会自己改，而原先只有挂载时拉一次实例记录 —— 于是顶部徽章、启停按钮全都停在
  // "运行中"，用户点「启动」拿到的是"已经在运行"，必须手动刷新页面才对。
  // runtime 是**直接问 Daemon** 的，它的 status 才是实时真相，所以每次轮询拿它
  // 校准一次 curStatus；发现不一致时再顺带重拉一次实例记录（过期时间、占用这些
  // 派生字段要跟着刷新）。
  useEffect(() => {
    let alive = true
    const tick = () =>
      getInstanceRuntime(instanceId)
        .then((r) => {
          if (!alive) return
          setRuntime(r)
          setNetRxHist((h) => [...h, r.net_rx_rate].slice(-24))
          setNetTxHist((h) => [...h, r.net_tx_rate].slice(-24))
          if (r.status && r.status !== curStatus) {
            setCurStatus(r.status)
            void reloadInst()
          }
        })
        .catch(() => { /* 忽略：节点不可达时保持上一次数据 */ })
    tick()
    const t = window.setInterval(tick, 5000)
    return () => { alive = false; window.clearInterval(t) }
  }, [instanceId, curStatus])

  // 备份列表 + 生效策略
  useEffect(() => {
    listBackups(instanceId)
      .then((r) => setBackups(Array.isArray(r) ? r : []))
      .catch(() => setBackups([]))
    getSchedule(instanceId)
      .then((s) => { setPolicyDesc(s.policy_desc || ''); setManualKeep(s.manual_keep) })
      .catch(() => { /* 忽略 */ })
  }, [instanceId, tab])

  const doAction = async (action: 'start' | 'stop' | 'kill' | 'restart') => {
    setBusy(true); setError('')
    try {
      await instanceAction(instanceId, action)
      setTimeout(async () => {
        try {
          const list = await listInstances()
          const me = Array.isArray(list) ? list.find((i) => i.instance_id === instanceId) : null
          if (me) setCurStatus(me.live_status || me.status)
        } catch { /* 忽略 */ }
      }, action === 'restart' ? 8000 : 1500)
    } catch (e: any) {
      setError(e.message || '操作失败')
      setCurStatus(action === 'start' ? 'stopped' : 'running')
    } finally {
      setBusy(false)
    }
  }

  const confirmKill = async () => { setShowKill(false); await doAction('kill') }

  /**
   * 提交实例改名。
   *
   * 改的是显示名（面板数据库的 name），**不是实例 ID** —— ID 是节点上的目录名，
   * 也是公网端口、授权、启动记录的钥匙，改名一律不动它，所以改完不会掉端口。
   * 改完还要通知外壳（实例名由 App 的 view 状态持有），否则左侧标题栏、
   * 返回列表后的名字都还是旧的。
   */
  const submitRename = async () => {
    const n = nameDraft.trim()
    if (!n || n === name) { setEditingName(false); return }
    setBusy(true); setError(''); setMsg('')
    try {
      const r = await renameInstance(instanceId, n)
      setMsg(r?.message || '名称已修改')
      setEditingName(false)
      onRenamed?.(n)
    } catch (e: any) {
      setError(e.message || '改名失败')
    } finally {
      setBusy(false)
    }
  }

  // 注：资源限制的编辑（openLimits / submitLimits）已搬到「统计与配额」页
  //（StatsTab）—— 那一页既有历史曲线又有配额，放在一起比散在右栏更好找。

  const memLimit = parseMem(inst?.max_mem || '')
  const memPct = metrics && memLimit ? Math.min(100, Math.round((metrics.mem_used / memLimit) * 100)) : 0
  // 磁盘占比：**优先按配额算**（配额才是"上限"，也是用户要看的东西）；
  // 没设配额时退回"占所在分区的比例"，只是给个量级参考。
  const diskPct = (() => {
    if (!runtime) return 0
    const usedMB = runtime.disk_used / 1024 / 1024
    const limitMB = inst?.disk_limit_mb || 0
    if (limitMB > 0) return Math.min(100, Math.round((usedMB / limitMB) * 100))
    if (runtime.disk_total > 0) return Math.round((runtime.disk_used / runtime.disk_total) * 100)
    return 0
  })()
  // CPU 配额以百分比表示（100 = 1 核）；0 表示不限。
  // 核数的换算与显示已搬到「统计与配额」页（那边是唯一展示配额的地方）。

  const handleNav = (key: string) => {
    if (key === 'alerts') { onOpenAlerts(); return }
    if (key === 'audit') return // 审计日志在 AppShell 侧边栏中进入
    onNavigate(key as NavKey)
  }

  return (
    <AppShell
      bare
      active="instances"
      title={name}
      user={user}
      alertCount={alertCount}
      onNavigate={onNavigate}
      onOpenAccount={onOpenAccount}
      onOpenAlerts={onOpenAlerts}
    >
      <div className="detail-shell">
        {/* ========================= 左栏 ========================= */}
        <aside className="detail-nav">
          {/* 品牌区：实例页是 bare 模式（没有全局外壳侧边栏），
              所以名称与版本号必须在这里也出现一次，否则这一页"看不出是哪个面板、哪个版本" */}
          <div className="detail-brand">
            <Brand size={24} />
          </div>
          <button className="back-link" onClick={onBack}>← 返回实例列表</button>

          <div className="side-card">
            {/* 全局导航默认**只露出一部分**。
                为什么：这页的主角是「实例导航」（控制台/文件/配置…十来个页签），
                而全局导航有 11 项 + 告警 —— 全展开会把实例导航挤到屏幕外，
                每次切页签都要先滚一下。这里保留最常用的几项（总览/实例列表/节点监控），
                其余折叠起来，需要时点「展开全部」。
                当前所在项如果在折叠区里，会自动展开（否则用户看不出自己在哪）。 */}
            <div className="side-card-title">
              <span>全局导航</span>
              <span
                className="side-card-more nav-toggle"
                onClick={() => setNavExpanded((v) => !v)}
                title={navExpanded ? '收起（只留常用项）' : '展开全部'}
              >
                {navExpanded ? '收起 ▲' : `展开全部 (${navItems.length}) ▼`}
              </span>
            </div>
            <nav className="vnav">
              {visibleNav.map((g) => (
                <button
                  key={g.key}
                  className={`vnav-item ${g.key === 'audit' ? 'dim' : ''}`}
                  onClick={() => handleNav(g.key)}
                  disabled={g.key === 'audit'}
                  title={g.key === 'audit' ? '请从左侧主菜单进入' : undefined}
                >
                  <span className="vnav-icon">{g.icon}</span>
                  {g.label}
                  {g.key === 'alerts' && alertCount > 0 && <span className="vnav-count">{alertCount}</span>}
                </button>
              ))}
            </nav>

            {/* 主题切换：本页是 bare 模式，不经过外壳页头（那上面的按钮在这里不存在），
                所以左栏单独放一份 —— 两处共用 ThemeToggle，状态自动同步。 */}
            <ThemeToggle block showLabel />
          </div>

          <div className="side-card">
            <div className="side-card-title"><span>实例导航</span><span className="hint2">{name}</span></div>
            <nav className="vnav">
              {TABS.map((t) => (
                <button
                  key={t.key}
                  className={`vnav-item ${tab === t.key ? 'active' : ''}`}
                  onClick={() => setTab(t.key)}
                >
                  <span className="vnav-icon">{t.icon}</span>
                  {t.label}
                  {t.key === 'players' && metrics && <span className="vnav-count">{metrics.players}</span>}
                  {t.key === 'backups' && backups.length > 0 && <span className="vnav-count">{backups.length}</span>}
                </button>
              ))}
              <div className="vnav-sep" />
              {TABS_EXTRA.map((t) => (
                <button
                  key={t.key}
                  className={`vnav-item ${tab === t.key ? 'active' : ''}`}
                  onClick={() => setTab(t.key)}
                >
                  <span className="vnav-icon">{t.icon}</span>
                  {t.label}
                </button>
              ))}
            </nav>
          </div>

          <AccountCard onOpenSettings={onOpenAccount} />
        </aside>

        {/* ========================= 中栏 ========================= */}
        <main className="detail-center">
          <div className="center-head">
            <div>
              <h2>
                {editingName ? (
                  <>
                    <input
                      className="inst-name-input"
                      value={nameDraft}
                      maxLength={32}
                      autoFocus
                      spellCheck={false}
                      disabled={busy}
                      onChange={(e) => setNameDraft(e.target.value)}
                      onKeyDown={(e) => {
                        if (e.key === 'Enter') { e.preventDefault(); submitRename() }
                        if (e.key === 'Escape') { e.preventDefault(); setEditingName(false) }
                      }}
                    />
                    <button className="primary" onClick={submitRename} disabled={busy}>保存</button>
                    <button onClick={() => setEditingName(false)} disabled={busy}>取消</button>
                  </>
                ) : (
                  <>
                    {name}
                    {canRenameInstance && (
                      <button
                        className="inst-name-pen"
                        title="修改实例名称（只改显示名，实例 ID 与公网端口不变）"
                        onClick={() => { setNameDraft(name); setEditingName(true) }}
                      >
                        ✎
                      </button>
                    )}
                  </>
                )}
                <span className={`badge badge-${curStatus === 'running' ? 'run' : 'stop'}`}>{curStatus}</span>
              </h2>
              <p>
                {inst?.core_type || '—'} · 端口 {inst?.port || '—'}
                {metrics ? ` · 在线 ${metrics.players}/${metrics.players_max || '—'}` : ''}
              </p>
            </div>
            <div className="center-ops">
              {busy && <span className="muted">处理中…</span>}
              {canOperate ? (
                curStatus === 'running' ? (
                  <>
                    <button onClick={() => doAction('restart')} disabled={busy}>重启</button>
                    <button className="danger-outline" onClick={() => doAction('stop')} disabled={busy}>停止</button>
                    <button className="danger" onClick={() => setShowKill(true)} disabled={busy} title="立即结束进程，未保存的数据会丢失">
                      强制关闭
                    </button>
                  </>
                ) : (
                  <button className="primary" onClick={() => doAction('start')} disabled={busy}>启动</button>
                )
              ) : (
                <span className="readonly-hint">只读权限</span>
              )}
            </div>
          </div>

          {error && <div className="error-banner" onClick={() => setError('')}>{error}</div>}
          {msg && <div className="success-banner" onClick={() => setMsg('')}>{msg}</div>}

          {/* 趋势图（控制台上方） */}
          <div className="charts">
            <div className="chart">
              <div className="k">CPU 使用量</div>
              <div className="v">
                {metrics ? metrics.cpu_percent.toFixed(2) + '%' : '—'}
                {inst?.cpu_quota ? <em> / {inst.cpu_quota}%</em> : null}
              </div>
              <ChartPlot
                points={cpuHist}
                max={Math.max(100, inst?.cpu_quota || 100)}
                fmt={(v) => `${Math.round(v)}%`}
              />
            </div>
            <div className="chart">
              <div className="k">内存使用量</div>
              <div className="v">
                {metrics ? fmtBytes(metrics.mem_used) : '—'}
                {inst?.max_mem ? <em> / {inst.max_mem}</em> : null}
              </div>
              <ChartPlot
                points={memHist}
                max={memLimit || 1}
                tone="alt"
                fmt={(v) => fmtBytes(v)}
              />
            </div>
            {/* 这一格原来是「网络流量」曲线，现改为**运行时长**：
                网络那块信息在控制台下方的模块里已经有了（上下行 + 累计），
                而"已经跑了多久"是进来第一眼最想知道的；放右栏又太低、要滚动才看到。
                正好补上这里空出来的一格。 */}
            <div className="chart">
              <div className="k">运行时长</div>
              <div className="v">{fmtUptime(runtime?.uptime_seconds, curStatus)}</div>
              <div className="chart-note">
                {runtime?.last_start_at
                  ? `启动于 ${new Date(runtime.last_start_at * 1000).toLocaleString('zh-CN', { hour12: false })}`
                  : '尚未记录启动时间'}
              </div>
              <div className="chart-note">
                开机 {runtime?.start_count ?? '—'} 次 · 关机 {runtime?.stop_count ?? '—'} 次
              </div>
              {/* 两条运行提示：JDK 回退 / cgroup 限额没生效。
                  都是"最近一次启动"的结果，正常时为空 —— 只有真出问题才显示，
                  免得常驻一行废话。放在运行时长这一格里是因为它就在首屏。 */}
              {runtime?.java_note ? (
                <div className="chart-note warn-line">{runtime.java_note}</div>
              ) : null}
              {runtime?.limit_warning ? (
                <div className="chart-note warn-line">资源限制未生效：{runtime.limit_warning}</div>
              ) : null}
            </div>
          </div>

          {/* 标签内容区。统一容器契约见 InstanceDetail.css 的 .tab-body：
              内边距与滚动由它负责，标签页自己不加 —— 否则切换时内边距变化，
              整块内容会横向位移（用户报的"切页时布局有差异"）。

              控制台用 --flush：它自带头部工具栏与终端边框，需要贴边、
              且要有确定高度（终端要占满剩余空间）。 */}
          <div className={`tab-body${tab === 'console' ? ' tab-body--flush' : ''}`}>
            {/* key={tab}：切标签时强制重挂载。
                两个理由：① 各标签页的内部状态（滚动位置、展开项、草稿）不该跨页残留；
                ② 切换动效需要元素"真的换了一个"，否则 React 复用同一个 DOM 节点、
                   动画不会重放。 */}
            <div className="tab-pane" key={tab}>
            {tab === 'console' && <ConsoleTab instanceId={instanceId} canSend={canOperate} />}
            {tab === 'loganalysis' && <LogShareTab instanceId={instanceId} canWrite={canWriteFiles} />}
            {tab === 'players' && <PlayersTab instanceId={instanceId} canWrite={canWriteFiles} />}
            {tab === 'gamesettings' && (
              <GameSettingsTab instanceId={instanceId} canWrite={canWriteFiles} running={curStatus === 'running'} />
            )}
            {tab === 'settings' && (
              <InstanceSettingsTab
                instanceId={instanceId}
                canManageExpiry={canManageExpiry}
                canWriteFiles={canWriteFiles}
                running={curStatus === 'running'}
                isAdmin={isAdminUser}
              />
            )}
            {tab === 'files' && <FilesTab instanceId={instanceId} canWrite={canWriteFiles} />}
            {tab === 'config' && <ConfigTab instanceId={instanceId} canWrite={canWriteFiles} />}
            {tab === 'backups' && (
              <BackupsTab instanceId={instanceId} canWrite={canWriteFiles} running={curStatus === 'running'} isAdmin={isAdminUser} />
            )}
            {tab === 'stats' && (
              <StatsTab
                instanceId={instanceId}
                inst={inst}
                canEditLimits={canEditLimits}
                onLimitsChanged={reloadInst}
              />
            )}
            {tab === 'tasks' && (
              <TasksTab
                instanceId={instanceId}
                canWrite={canWriteFiles}
                isAdmin={isAdminUser}
                inst={inst}
                canManageExpiry={canManageExpiry}
                onExpiryChanged={reloadInst}
              />
            )}
            </div>
          </div>

          {/* 控制台下方信息模块：填充原先的空白区。
              四个格子：上下行速率（**合并成一张图**，两条线共用刻度）、
              累计流量、在线玩家、TPS（后两个各自一条折线 —— 原来是挤在一个格子里
              用文字带出 TPS，看不出趋势）。 */}
          {tab === 'console' && (
            <div className="console-modules">
              <div className="mod">
                <div className="mod-k">
                  上下行速率
                  <span className="k-scope" title={
                    runtime?.net_scope === 'instance'
                      ? '该实例隧道的精确流量（来自 frps 管理 API）'
                      : '节点整机的网络吞吐 —— 未配置 frps 管理 API 时无法区分单实例'
                  }>
                    {runtime?.net_scope === 'instance' ? '实例' : '整机'}
                  </span>
                </div>
                {/* 两个数值 + 图例色块：图上是两条线，必须让人一眼分清哪条是哪条 */}
                <div className="mod-v net-legend">
                  <span className="nl nl-rx">↓ {fmtRate(runtime?.net_rx_rate)}</span>
                  <span className="nl nl-tx">↑ {fmtRate(runtime?.net_tx_rate)}</span>
                </div>
                <ChartPlot
                  points={netRxHist}
                  points2={netTxHist}
                  max={Math.max(...netRxHist, ...netTxHist, 1024)}
                  fmt={(v) => fmtRate(v)}
                />
              </div>
              <div className="mod">
                <div className="mod-k">累计流量</div>
                <div className="mod-v">{fmtBytes((runtime?.net_rx_total || 0) + (runtime?.net_tx_total || 0))}</div>
                <div className="mod-sub">收 {fmtBytes(runtime?.net_rx_total || 0)} · 发 {fmtBytes(runtime?.net_tx_total || 0)}</div>
              </div>
              <div className="mod">
                <div className="mod-k">在线玩家</div>
                <div className="mod-v">
                  {metrics ? metrics.players : '—'}
                  {metrics?.players_max ? <em> / {metrics.players_max}</em> : null}
                </div>
                <ChartPlot
                  points={playersHist}
                  max={Math.max(...playersHist, metrics?.players_max || 0, 1)}
                  tone="alt"
                  fmt={(v) => String(Math.round(v))}
                />
              </div>
              <div className="mod">
                <div className="mod-k">TPS</div>
                <div className="mod-v">
                  {metrics?.tps ? metrics.tps.toFixed(2) : '—'}
                  <em> / 20.00</em>
                </div>
                <ChartPlot
                  points={tpsHist}
                  max={20}
                  fmt={(v) => v.toFixed(1)}
                />
              </div>
            </div>
          )}
        </main>

        {/* ========================= 右栏 ========================= */}
        <aside className="detail-side">
          {/* 实例信息 + 公网域名 */}
          <div className="w">
            <div className="inst-head-l">
              <Avatar
                url={inst ? instanceIconUrl(inst) : ''}
                name={name}
                size={34}
                radius="var(--r-lg)"
                className="inst-avatar"
                initialsLength={1}
              />
              <div>
                <div className="inst-name">{name}</div>
                <div className="inst-sub">{instanceId} · {inst?.core_type || '—'}</div>
              </div>
            </div>
            <div><span className={`badge badge-${curStatus === 'running' ? 'run' : 'stop'}`}>{curStatus}</span></div>

            <div className="domain">
              <div className="lbl">公网域名 · 管理员设置</div>
              <div className="val">
                <span>{runtime?.public_address || pubAddr || '未配置'}</span>
                {(runtime?.public_address || pubAddr) && (
                  <span className="copy" onClick={() => navigator.clipboard?.writeText(runtime?.public_address || pubAddr)}>复制</span>
                )}
              </div>
              <div className="sync">⇄ 与「穿透管理」中的域名同步（含端口）</div>
            </div>
          </div>

          {/* 资源占用 */}
          <div className="w">
            <div className="w-title"><span>资源占用</span><span className="w-ico">◔</span></div>

            {/* 「分配核心」原先在最上面一行，2026-10-04 去掉：它与「统计与配额」页
                的 CPU 配额是同一件事，而静态配置放在"实时占用"卡片里也容易让人
                以为会随负载变化。核数现在只在统计页出现一次。 */}

            {curStatus !== 'running' ? (
              <div className="widget-empty">实例未运行</div>
            ) : !metrics ? (
              <div className="widget-empty">采集中…</div>
            ) : (
              <>
                <div className="res">
                  <div className="res-top">
                    <span className="k">CPU</span>
                    <span className="v">
                      {metrics.cpu_percent.toFixed(1)}%
                      {inst?.cpu_quota ? <em> / {inst.cpu_quota}%</em> : null}
                    </span>
                  </div>
                  <div className="bar"><i style={{ width: `${Math.min(100, metrics.cpu_percent)}%` }} /></div>
                  {/* 线程数取自 /proc/<pid>/status，是 JVM 真实开的线程；
                      不要用"核数 × 2"去猜超线程 —— 宿主没开 SMT 时那个估算就是错的 */}
                  {metrics.threads ? (
                    <div className="res-note">◧ 进程线程 {metrics.threads} 条（GC / 网络 / 区块 IO 等）</div>
                  ) : null}
                </div>
                <div className="res">
                  <div className="res-top">
                    <span className="k">内存</span>
                    <span className="v">{fmtBytes(metrics.mem_used)}<em> / {inst?.max_mem || '—'}</em></span>
                  </div>
                  <div className="bar"><i className="ok" style={{ width: `${memPct}%` }} /></div>
                </div>
                {/* 磁盘：分母是**配额**（设了配额时），不是整块盘的容量 ——
                    用户要的就是"离上限还剩多少"；没设配额才回落显示分区容量，
                    且写明"所在分区"，免得被当成配额（用户报过这个混淆）。 */}
                <div className="res">
                  <div className="res-top">
                    <span className="k">磁盘</span>
                    <span className="v">
                      {runtime ? fmtBytes(runtime.disk_used) : '—'}
                      <em>
                        {inst?.disk_limit_mb && inst.disk_limit_mb > 0
                          ? ` / ${inst.disk_limit_mb} MB`
                          : runtime && runtime.disk_total ? ` / 分区 ${fmtBytes(runtime.disk_total)}` : ''}
                      </em>
                    </span>
                  </div>
                  <div className="bar">
                    <i style={{ width: `${diskPct}%` }} />
                  </div>
                  <div className="res-note">
                    {inst?.disk_limit_mb && inst.disk_limit_mb > 0
                      ? `配额 ${inst.disk_limit_mb} MB${inst.disk_autostop ? ' · 超限自动停机' : ''}`
                      : runtime && runtime.disk_free ? `未设配额 · 分区剩余 ${fmtBytes(runtime.disk_free)}` : '未设配额'}
                  </div>
                </div>
                <div className="res">
                  <div className="res-top">
                    <span className="k">TPS</span>
                    <span className="v">{metrics.tps ? metrics.tps.toFixed(2) : '—'}<em> / 20.00</em></span>
                  </div>
                  <div className="bar"><i className="ok" style={{ width: `${Math.min(100, (metrics.tps / 20) * 100)}%` }} /></div>
                </div>
              </>
            )}
          </div>


          {/* 到期时间已搬到「任务」标签页 —— 语义上它和定时开关机、定时备份是一类
              （都是"这台实例什么时候会自动做什么"），原来挤在右栏最难找。
              权限判断仍在上面算好（canManageExpiry），一并传给 TasksTab。 */}

          {/* 运行时长原先在这里，已搬到**控制台上方那一排的右侧**
              （替换掉原来的「网络流量」曲线）—— 右栏位置太靠下，
              要滚动才看得到，而"已经跑了多久"是进来第一眼就想知道的。
              网络那块信息在控制台下方的模块里有，不会丢。 */}

          {/* 备份日历 */}
          <BackupCalendar backups={backups} policyDesc={policyDesc} manualKeep={manualKeep} />
        </aside>
      </div>

      {showKill && (
        <div className="confirm-mask" onClick={() => setShowKill(false)}>
          <div className="confirm-box" onClick={(e) => e.stopPropagation()}>
            <div className="confirm-head"><span className="confirm-title">强制关闭实例？</span></div>
            <div className="confirm-body">
              <p>即将对 <b>{name}（{instanceId}）</b> 执行 <b>强制关闭</b>（直接结束进程）。</p>
              <div className="confirm-warn">
                ⚠️ 与「停止」不同，强制关闭<strong>不会等待服务器保存存档</strong>，
                未落盘的世界数据与玩家进度<strong>可能丢失</strong>，甚至造成存档损坏。
              </div>
              <p className="confirm-hint">
                仅在实例已卡死、控制台无响应、正常停止无效时才使用。<br />
                建议优先尝试「停止」；若已卡死，强杀后请检查最新备份。
              </p>
            </div>
            <div className="confirm-foot">
              <button onClick={() => setShowKill(false)}>取消</button>
              <button className="danger" onClick={confirmKill}>确认强制关闭</button>
            </div>
          </div>
        </div>
      )}
    </AppShell>
  )
}
