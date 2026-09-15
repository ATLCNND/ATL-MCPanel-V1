import { useEffect, useState } from 'react'
import ConsoleTab from './ConsoleTab'
import FilesTab from './FilesTab'
import ConfigTab from './ConfigTab'
import BackupsTab from './BackupsTab'
import PlayersTab from './PlayersTab'
import JarsTab from './JarsTab'
import StartScriptTab from './StartScriptTab'
import InstancePorts from './InstancePorts'
import BackupCalendar from './BackupCalendar'
import AccountCard from './AccountCard'
import StatsTab from './StatsTab'
import TasksTab from './TasksTab'
import {
  instanceAction, levelAtLeast, listInstances, currentUser, User, isNodeUser,
  getMetrics, listBackups, listTunnels, getSchedule, getInstanceRuntime,
  BackupItem, Metrics, Instance, InstanceRuntime, instanceIconUrl,
} from '../api'
import AppShell, { NavKey, NAV_ITEMS } from './AppShell'
import ThemeToggle from './ThemeToggle'
import Avatar from './Avatar'
import './InstanceDetail.css'

type Tab = 'console' | 'players' | 'ports' | 'jars' | 'start' | 'files' | 'config' | 'backups' | 'stats' | 'tasks'

const TABS: { key: Tab; label: string; icon: string }[] = [
  { key: 'console', label: '控制台', icon: '▸' },
  { key: 'players', label: '玩家', icon: '◉' },
  // 公网端口放在靠前的位置：模组/插件配好后，用户最常回来查的就是地址
  { key: 'ports', label: '公网端口', icon: '⇄' },
  { key: 'jars', label: '核心', icon: '◈' },
  { key: 'start', label: '启动', icon: '⌘' },
  { key: 'files', label: '文件', icon: '▤' },
  { key: 'config', label: '配置', icon: '⚙' },
  { key: 'backups', label: '备份', icon: '▣' },
]

/** 分隔线之后的低频功能位 */
const TABS_EXTRA: { key: Tab; label: string; icon: string }[] = [
  { key: 'stats', label: '统计', icon: '◔' },
  { key: 'tasks', label: '任务', icon: '▷' },
]

/**
 * 实例页左栏的「全局导航」。
 *
 * **从 NAV_ITEMS 派生**，不再自己写一份 —— 以前这里是一份硬编码的副本，
 * 加了「账户」「公告与帮助」之后没同步，导致在实例页里进不去这两个页面。
 * 唯一的不同是中间插一个「告警」：那一项在实例页走弹窗，而不是页面。
 */
const GLOBAL_NAV: { key: NavKey | 'alerts'; label: string; icon: string; adminOnly?: boolean }[] =
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

export default function InstanceDetail({ instanceId, name, status, level, user, alertCount, onBack, onNavigate, onOpenAccount, onOpenAlerts }: {
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
}) {
  const [tab, setTab] = useState<Tab>('console')
  const [curStatus, setCurStatus] = useState(status)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const [msg, setMsg] = useState('')
  const [showKill, setShowKill] = useState(false)

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
  // 到期控制与删除同级：总管理员，或该节点上的节点用户。
  // 具体的编辑面板已搬到「任务」标签页（见 ExpiryPanel），这里只负责算权限并传下去。
  const canManageExpiry = isAdminUser || (isNodeUser(user?.role) && level !== '')

  // ---- 全局导航折叠 ----
  // 折叠时只保留最常用的几项；其余收进「展开全部」。
  // 这两个 key 是"从实例页最可能想去的地方"：回总览、回实例列表、看节点监控。
  const NAV_ALWAYS = ['dashboard', 'instances', 'monitor']
  const [navExpanded, setNavExpanded] = useState(false)
  const navItems = GLOBAL_NAV.filter((g) => !g.adminOnly || isAdminUser)
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
  useEffect(() => {
    listTunnels()
      .then((list) => {
        const t = Array.isArray(list) ? list.find((x: any) => x.instance_id === instanceId) : null
        if (t) setPubAddr((t as any).display_domain || (t as any).public_address || '')
      })
      .catch(() => { /* 非管理员 403，静默 */ })
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
  useEffect(() => {
    let alive = true
    const tick = () =>
      getInstanceRuntime(instanceId)
        .then((r) => {
          if (!alive) return
          setRuntime(r)
          setNetRxHist((h) => [...h, r.net_rx_rate].slice(-24))
          setNetTxHist((h) => [...h, r.net_tx_rate].slice(-24))
        })
        .catch(() => { /* 忽略：节点不可达时保持上一次数据 */ })
    tick()
    const t = window.setInterval(tick, 5000)
    return () => { alive = false; window.clearInterval(t) }
  }, [instanceId])

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

  const memLimit = parseMem(inst?.max_mem || '')
  const memPct = metrics && memLimit ? Math.min(100, Math.round((metrics.mem_used / memLimit) * 100)) : 0
  // CPU 配额以百分比表示（100 = 1 核）；0 表示不限。
  // 配额可能是 150% 这种半核，所以不能直接取整。
  const cores = inst?.cpu_quota ? inst.cpu_quota / 100 : 0
  const fmtCores = (c: number) => (Number.isInteger(c) ? String(c) : c.toFixed(1))

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
                {name} <span className={`badge badge-${curStatus === 'running' ? 'run' : 'stop'}`}>{curStatus}</span>
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

          <div className="tab-body">
            {tab === 'console' && <ConsoleTab instanceId={instanceId} canSend={canOperate} />}
            {tab === 'players' && <PlayersTab instanceId={instanceId} canWrite={canWriteFiles} />}
            {tab === 'ports' && <InstancePorts instanceId={instanceId} canEdit={canManageExpiry} />}
            {tab === 'jars' && <JarsTab instanceId={instanceId} canWrite={canWriteFiles} running={curStatus === 'running'} />}
            {tab === 'start' && <StartScriptTab instanceId={instanceId} canWrite={canWriteFiles} running={curStatus === 'running'} />}
            {tab === 'files' && <FilesTab instanceId={instanceId} canWrite={canWriteFiles} />}
            {tab === 'config' && <ConfigTab instanceId={instanceId} canWrite={canWriteFiles} />}
            {tab === 'backups' && (
              <BackupsTab instanceId={instanceId} canWrite={canWriteFiles} running={curStatus === 'running'} isAdmin={isAdminUser} />
            )}
            {tab === 'stats' && (
              <StatsTab
                instanceId={instanceId}
                maxMem={inst?.max_mem || ''}
                diskLimitMB={inst?.disk_limit_mb}
                diskAutostop={inst?.disk_autostop}
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
                <span>{runtime?.display_domain || pubAddr || '未配置'}</span>
                {(runtime?.display_domain || pubAddr) && (
                  <span className="copy" onClick={() => navigator.clipboard?.writeText(runtime?.display_domain || pubAddr)}>复制</span>
                )}
              </div>
              <div className="sync">⇄ 与「穿透管理」中的域名同步（含端口）</div>
            </div>
          </div>

          {/* 资源占用 */}
          <div className="w">
            <div className="w-title"><span>资源占用</span><span className="w-ico">◔</span></div>

            {/* 资源分配：这是实例的**静态配置**，与是否在运行无关，
                所以放在运行状态判断之外 —— 实例停着的时候同样需要看到
                "这台分了多少核、多少内存"，否则只能去创建页翻配置。 */}
            <div className="res alloc-row">
              <div className="res-top">
                <span className="k">分配核心</span>
                <span className="v">
                  {cores > 0
                    ? <>{fmtCores(cores)}<em> 核</em></>
                    : <>不限制<em> 配额</em></>}
                </span>
              </div>
              <div className="res-note">
                {inst?.cpu_quota
                  ? `cgroup v2 cpu.max 硬上限 · 配额 ${inst.cpu_quota}%（100% = 1 核）`
                  : '未设置 CPU 配额，会与同节点其它实例争抢 CPU'}
              </div>
            </div>

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
                <div className="res">
                  <div className="res-top">
                    <span className="k">磁盘</span>
                    <span className="v">
                      {runtime ? fmtBytes(runtime.disk_used) : '—'}
                      <em>{runtime && runtime.disk_total ? ` / ${fmtBytes(runtime.disk_total)}` : ''}</em>
                    </span>
                  </div>
                  <div className="bar">
                    <i style={{ width: `${runtime && runtime.disk_total ? Math.round((runtime.disk_used / runtime.disk_total) * 100) : 0}%` }} />
                  </div>
                  {runtime && runtime.disk_free ? (
                    <div className="res-note">剩余 {fmtBytes(runtime.disk_free)}</div>
                  ) : null}
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
