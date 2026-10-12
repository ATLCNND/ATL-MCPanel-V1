import { useEffect, useState } from 'react'
import {
  Instance, InstanceRuntime, getInstanceRuntime, getInstanceStats, StatsResponse,
  setInstanceLimits,
} from '../api'
import { parseServerTime } from '../time'
import './StatsTab.css'

function fmtBytes(n: number): string {
  if (!n) return '—'
  if (n < 1024 * 1024) return (n / 1024).toFixed(0) + ' KB'
  if (n < 1024 * 1024 * 1024) return (n / 1024 / 1024).toFixed(0) + ' MB'
  return (n / 1024 / 1024 / 1024).toFixed(2) + ' GB'
}
function fmtUptime(sec?: number, status?: string): string {
  if (status && status !== 'running') return '未运行'
  if (!sec || sec <= 0) return '—'
  const d = Math.floor(sec / 86400)
  const h = Math.floor((sec % 86400) / 3600)
  const m = Math.floor((sec % 3600) / 60)
  if (d > 0) return `${d} 天 ${h} 小时`
  if (h > 0) return `${h} 小时 ${m} 分`
  return `${m} 分`
}
/** 核数显示：允许半核（150% = 1.5 核），所以不能直接取整 */
function fmtCores(n: number): string {
  return Number.isInteger(n) ? String(n) : n.toFixed(1)
}

/**
 * 历史趋势图（带 Y 轴刻度与网格）。
 * 与实例页的实时图不同，这里的数据来自后端历史采样，跨度可达 7 天。
 */
function TrendChart({ title, points, color, fmt, unit }: {
  title: string
  points: { v: number; at: string }[]
  color: string
  fmt: (n: number) => string
  unit?: string
}) {
  const max = Math.max(...points.map((p) => p.v), 1)
  const W = 600
  const H = 90
  const step = points.length > 1 ? W / (points.length - 1) : W
  const coords = points.map((p, i) => ({ x: i * step, y: H - (p.v / max) * H }))
  const line = coords.map((c) => `${c.x.toFixed(1)},${c.y.toFixed(1)}`).join(' ')
  const area = `0,${H} ${line} ${W},${H}`

  return (
    <div className="st-chart">
      <div className="st-head">
        <span className="st-title">{title}</span>
        <span className="st-peak">峰值 {fmt(max)}{unit || ''}</span>
      </div>
      <div className="st-plot">
        <div className="st-axis">
          {[0, 25, 50, 75, 100].map((p) => (
            <span key={p} className="st-label" style={{ top: `${p}%` }}>
              {fmt((max * (100 - p)) / 100)}
            </span>
          ))}
        </div>
        <svg viewBox={`0 0 ${W} ${H}`} preserveAspectRatio="none" className="st-svg">
          <g className="st-grid">
            {[0, 25, 50, 75, 100].map((p) => (
              <line key={p} x1="0" y1={(p / 100) * H} x2={W} y2={(p / 100) * H} />
            ))}
            {[25, 50, 75].map((p) => (
              <line key={p} x1={(p / 100) * W} y1="0" x2={(p / 100) * W} y2={H} />
            ))}
          </g>
          {points.length > 1 && (
            <>
              <polygon points={area} fill={color} opacity="0.12" />
              <polyline points={line} fill="none" stroke={color} strokeWidth="1.6" />
            </>
          )}
        </svg>
      </div>
      {points.length > 1 && (
        <div className="st-xaxis">
          {/* 采样点是服务端给的无时区标记 UTC 串，走 parseServerTime 才不会早 8 小时 */}
          <span>{parseServerTime(points[0].at).toLocaleString('zh-CN', { hour12: false, month: 'numeric', day: 'numeric', hour: '2-digit', minute: '2-digit' })}</span>
          <span>{parseServerTime(points[points.length - 1].at).toLocaleString('zh-CN', { hour12: false, month: 'numeric', day: 'numeric', hour: '2-digit', minute: '2-digit' })}</span>
        </div>
      )}
    </div>
  )
}

/**
 * 「统计与配额」页。
 *
 * 2026-10-04：资源上限（CPU / 内存 / 磁盘）的**查看与修改**从实例页右栏搬到这里。
 * 原先右栏有「资源占用」和「资源限制」两张卡并排，内容高度重复（一个看当前值、
 * 一个看上限），右栏被撑得很长、整页跟着滚动，而两侧其实都能一屏放下。
 * 现在按"看数据的地方顺带管配额"合并到本页，右栏只留「信息 / 资源占用 / 备份日历」。
 *
 * 权限：编辑入口只给运营侧（总管理员 / 该节点的节点用户）——上层算好传进来。
 * 租户自己能改的话，运营方收紧之后对方点一下就能放开，这个接口等于没做；
 * 没有权限的人只看得到当前值。
 */
export default function StatsTab({ instanceId, inst, canEditLimits, onLimitsChanged }: {
  instanceId: string
  /** 实例记录：取 max_mem / cpu_quota / mem_limit / disk_limit_mb / disk_autostop / status */
  inst: Instance | null
  canEditLimits: boolean
  /** 保存成功后通知父组件重拉实例记录（右栏的占用/限制显示要跟着变） */
  onLimitsChanged?: () => void
}) {
  const [hours, setHours] = useState(24)
  const [stats, setStats] = useState<StatsResponse | null>(null)
  const [runtime, setRuntime] = useState<InstanceRuntime | null>(null)
  const [error, setError] = useState('')
  const [loading, setLoading] = useState(false)

  // 资源上限的就地编辑（从实例页右栏搬过来的那套）
  const [editLimits, setEditLimits] = useState(false)
  const [limCores, setLimCores] = useState('')
  const [limMem, setLimMem] = useState('')
  const [limDisk, setLimDisk] = useState('')
  // JVM 堆（-Xmx / -Xms）：与上面的 cgroup 内存上限是两个层面，
  // 所以表单里也要分成两个输入框（用户反馈"配额修改没法改 JVM 内存"）。
  const [limMaxMem, setLimMaxMem] = useState('')
  const [limMinMem, setLimMinMem] = useState('')
  const [limBusy, setLimBusy] = useState(false)
  const [limErr, setLimErr] = useState('')
  const [limNote, setLimNote] = useState('')

  const maxMem = inst?.max_mem || ''
  const diskLimitMB = inst?.disk_limit_mb
  const diskAutostop = inst?.disk_autostop
  const cpuQuota = inst?.cpu_quota || 0
  const cores = cpuQuota / 100
  const running = inst?.status === 'running'

  const load = async () => {
    setLoading(true)
    try {
      const [s, rt] = await Promise.all([
        getInstanceStats(instanceId, hours),
        getInstanceRuntime(instanceId).catch(() => null),
      ])
      setStats(s)
      setRuntime(rt)
      setError('')
    } catch (e: any) {
      setError(e.message)
    } finally {
      setLoading(false)
    }
  }

  useEffect(() => { load() }, [instanceId, hours])

  /** 把当前值填进草稿并进入编辑态 */
  const openLimits = () => {
    setLimCores(cpuQuota ? String(cores) : '')
    setLimMem(inst?.mem_limit || '')
    setLimDisk(diskLimitMB ? String(diskLimitMB) : '')
    setLimMaxMem(inst?.max_mem || '')
    // -Xms **留空**：面板库不保存这个字段（只有 max_mem 一列，见 db/migrate.go 的
    // 建表语句），列表接口拿不到它。留空提交表示"不指定 -Xms"，节点会保留原值 ——
    // 这是诚实的做法；假装回填一个值会让用户以为看的是真实配置。
    setLimMinMem('')
    setLimErr('')
    setLimNote('')
    setEditLimits(true)
  }

  const submitLimits = async () => {
    setLimBusy(true); setLimErr(''); setLimNote('')
    try {
      const coresNum = limCores.trim() === '' ? 0 : Number(limCores)
      if (!Number.isFinite(coresNum) || coresNum < 0) {
        throw new Error('CPU 配额需为不小于 0 的数字（留空或 0 = 不限制）')
      }
      const diskNum = limDisk.trim() === '' ? 0 : Number(limDisk)
      if (!Number.isFinite(diskNum) || diskNum < 0) {
        throw new Error('磁盘配额需为不小于 0 的整数 MB（留空或 0 = 不限制）')
      }
      // JVM 堆在本地先挡一道：服务端也会校验，但"填了空却得到 1G"这种
      // 反直觉的结果不值得让用户跑一趟网络才知道。
      const maxMem = limMaxMem.trim()
      if (!maxMem) {
        throw new Error('JVM 堆上限（-Xmx）不能为空：请填一个值，如 4G。要表达"内存不限制"请把上面的 cgroup 内存上限留空')
      }
      const r = await setInstanceLimits(instanceId, {
        cpu_quota: Math.round(coresNum * 100),
        mem_limit: limMem.trim(),
        disk_limit_mb: Math.round(diskNum),
        max_mem: maxMem,
        min_mem: limMinMem.trim(),
      })
      setEditLimits(false)
      setLimNote(r.message + (r.note ? '；' + r.note : ''))
      onLimitsChanged?.()
      // 配额变了，本页的"配额水位"要立刻按新值算
      void load()
    } catch (e: any) {
      setLimErr(e.message)
    } finally {
      setLimBusy(false)
    }
  }

  const pts = stats?.points || []
  const pick = (k: 'cpu' | 'mem' | 'players' | 'tps') => pts.map((p) => ({ v: p[k] as number, at: p.at }))

  // 运行率：本次运行时长 / 统计窗口
  const windowSec = hours * 3600
  const runRate = runtime && runtime.uptime_seconds > 0
    ? Math.min(100, Math.round((runtime.uptime_seconds / windowSec) * 100))
    : 0

  // 磁盘配额水位（配额为 0/未设置时不算百分比，避免除零）
  const quotaPct = diskLimitMB && diskLimitMB > 0 && runtime
    ? Math.round((runtime.disk_used / 1024 / 1024 / diskLimitMB) * 100)
    : 0
  const quotaCls = quotaPct >= 95 ? 'bad' : quotaPct >= 85 ? 'warn' : 'ok'

  return (
    <div className="stats-tab">
      {/* ---- 资源上限（配额）：查看 + 就地编辑 ----
          放在最前面：配额决定"这台被允许用多少"，是理解下面所有曲线的前提。 */}
      <div className="st-quota-card">
        <div className="sqc-head">
          <span className="sqc-title">资源上限（配额）</span>
          {canEditLimits && !editLimits && (
            <button className="sqc-edit" onClick={openLimits} disabled={limBusy}>修改限制</button>
          )}
        </div>

        {limNote && <div className="warn-banner">{limNote}</div>}

        {!editLimits ? (
          <>
            <div className="sqc-grid">
              <div className="sqc-item">
                <span className="k">CPU 配额</span>
                <span className="v">
                  {cores > 0 ? <>{fmtCores(cores)}<em> 核</em></> : <>不限制</>}
                </span>
                <span className="sub">
                  {cpuQuota ? `cgroup cpu.max 硬上限 · ${cpuQuota}%` : '未设置，会与同节点其它实例争抢 CPU'}
                </span>
              </div>
              <div className="sqc-item">
                <span className="k">cgroup 内存上限</span>
                <span className="v">{inst?.mem_limit ? inst.mem_limit : <em>不限制</em>}</span>
                <span className="sub">memory.max：整个进程的天花板（含堆外）</span>
              </div>
              <div className="sqc-item">
                <span className="k">JVM 堆上限</span>
                <span className="v">{maxMem ? <>{maxMem}<em> -Xmx</em></> : '—'}</span>
                <span className="sub">启动命令里的堆大小；建议比 cgroup 上限小（约 ×1.3 + 512MB）</span>
              </div>
              <div className="sqc-item">
                <span className="k">磁盘配额</span>
                <span className="v">
                  {diskLimitMB && diskLimitMB > 0 ? <>{diskLimitMB}<em> MB</em></> : <>不限制</>}
                </span>
                <span className="sub">
                  {diskLimitMB && diskLimitMB > 0
                    ? `面板每分钟巡检${diskAutostop ? ' · 超限自动停机' : ' · 仅告警'}`
                    : '软配额：不拦写入，只在接近时告警'}
                </span>
              </div>
            </div>
            {canEditLimits ? (
              <div className="sqc-note">
                磁盘配额改完即生效（面板巡检）；CPU 与两种内存在实例<strong>启动时</strong>才生效
                （cgroup 写 memory.max、JVM 堆渲染进启动命令），运行中修改需重启实例。
              </div>
            ) : (
              <div className="sqc-note">
                资源上限由运营方设置：需要总管理员或该节点的节点管理员权限才能修改。如需调整请找管理员。
              </div>
            )}
          </>
        ) : (
          <div className="sqc-form">
            {limErr && <div className="error-banner">{limErr}</div>}
            {running && (
              <div className="warn-banner">
                实例正在运行：CPU、cgroup 内存上限与 <strong>JVM 堆</strong>都要
                <strong>重启实例</strong>后生效（当前进程仍按启动时的那些值运行）；
                磁盘配额改完即生效。
              </div>
            )}
            <label className="sqc-field">
              CPU 配额（核，100% = 1 核；0 或留空 = 不限制）
              <input
                value={limCores} disabled={limBusy} inputMode="decimal" placeholder="0"
                onChange={(e) => setLimCores(e.target.value)}
              />
            </label>
            <label className="sqc-field">
              cgroup 内存上限（如 4G / 1536m；留空 = 不限制）
              <input
                value={limMem} disabled={limBusy} spellCheck={false} placeholder="4G（留空不限制）"
                onChange={(e) => setLimMem(e.target.value)}
              />
            </label>
            {/* JVM 堆：与 cgroup 上限分成两个输入框 —— 它们是两个层面的"内存"，
                合成一个会让人以为改一个就够了（这正是用户报"改不了 JVM 内存"的由来）。 */}
            <label className="sqc-field">
              JVM 堆上限 -Xmx（如 4G / 1536m；<strong>不能留空</strong>）
              <input
                value={limMaxMem} disabled={limBusy} spellCheck={false} placeholder="4G"
                onChange={(e) => setLimMaxMem(e.target.value)}
              />
            </label>
            <label className="sqc-field">
              JVM 初始堆 -Xms（如 1G / 512m；可留空 = 不指定 -Xms）
              <input
                value={limMinMem} disabled={limBusy} spellCheck={false} placeholder="1G（可留空）"
                onChange={(e) => setLimMinMem(e.target.value)}
              />
            </label>
            <div className="sqc-hint">
              建议 <code>-Xmx</code> 比 cgroup 上限小一些（约 <code>-Xmx×1.3 + 512MB</code>）：
              JVM 除堆之外还要用元空间、直接内存、线程栈，堆顶到天花板就容易被内核 OOM 杀掉。
            </div>
            <label className="sqc-field">
              磁盘配额（MB；0 或留空 = 不限制）
              <input
                value={limDisk} disabled={limBusy} inputMode="numeric" placeholder="0"
                onChange={(e) => setLimDisk(e.target.value)}
              />
            </label>
            <div className="sqc-actions">
              <button className="primary" onClick={submitLimits} disabled={limBusy}>
                {limBusy ? '保存中…' : '保存'}
              </button>
              <button onClick={() => { setEditLimits(false); setLimErr('') }} disabled={limBusy}>取消</button>
            </div>
          </div>
        )}
      </div>

      <div className="st-toolbar">
        <div className="st-ranges">
          {[1, 6, 24, 72, 168].map((h) => (
            <button key={h} className={hours === h ? 'active' : ''} onClick={() => setHours(h)}>
              {h < 24 ? `${h} 小时` : `${h / 24} 天`}
            </button>
          ))}
        </div>
        <span className="muted">
          {loading ? '加载中…' : `共 ${stats?.total || 0} 个采样点`}
          {stats && stats.step > 1 ? `（每 ${stats.step} 分钟取一点）` : ''}
        </span>
        <button onClick={load} disabled={loading}>刷新</button>
      </div>

      {error && <div className="error-banner">{error}</div>}

      {/* 汇总 */}
      <div className="st-summary">
        <div className="ss">
          <div className="k">平均 CPU</div>
          <div className="v">{stats?.avg?.cpu ? stats.avg.cpu.toFixed(1) + '%' : '—'}</div>
          <div className="sub">峰值 {stats?.peak?.cpu ? stats.peak.cpu.toFixed(1) + '%' : '—'}</div>
        </div>
        <div className="ss">
          <div className="k">平均内存</div>
          <div className="v">{stats?.avg?.mem ? fmtBytes(stats.avg.mem) : '—'}</div>
          <div className="sub">峰值 {stats?.peak?.mem ? fmtBytes(stats.peak.mem) : '—'} / {maxMem || '—'}</div>
        </div>
        <div className="ss">
          <div className="k">平均在线</div>
          <div className="v">{stats?.avg?.players ? stats.avg.players.toFixed(1) : '—'}</div>
          <div className="sub">峰值 {stats?.peak?.players ?? '—'} 人</div>
        </div>
        <div className="ss">
          <div className="k">平均 TPS</div>
          <div className="v">{stats?.avg?.tps ? stats.avg.tps.toFixed(2) : '—'}</div>
          <div className="sub">满值 20.00</div>
        </div>
      </div>

      {/* 运行统计 */}
      <div className="st-runtime">
        <div className="sr">
          <span className="k">本次运行时长</span>
          <b>{fmtUptime(runtime?.uptime_seconds, runtime?.status)}</b>
        </div>
        <div className="sr">
          <span className="k">累计启停</span>
          <b>开机 {runtime?.start_count ?? '—'} 次 · 关机 {runtime?.stop_count ?? '—'} 次</b>
        </div>
        <div className="sr">
          <span className="k">窗口内运行占比</span>
          <b>{runRate}%</b>
        </div>
        <div className="sr">
          {/* 分母是**配额**（有配额时）——用户要的就是这个：一眼看出"离上限还剩多少"。
              没设配额时才回落显示所在分区的容量，并且**写明是分区**，
              免得把"整块盘"当成配额（这个混淆被用户报过）。 */}
          <span className="k">实例目录占用</span>
          <b>
            {runtime ? fmtBytes(runtime.disk_used) : '—'}
            {diskLimitMB && diskLimitMB > 0 ? (
              <em className="sr-dim"> / 配额 {diskLimitMB} MB</em>
            ) : runtime?.disk_total ? (
              <em className="sr-dim"> / 所在分区 {fmtBytes(runtime.disk_total)}</em>
            ) : null}
          </b>
        </div>
      </div>

      {/* 磁盘配额水位 */}
      {diskLimitMB && diskLimitMB > 0 && runtime && (
        <div className="st-quota">
          <div className="sq-head">
            <span>磁盘配额水位</span>
            <b>{fmtBytes(runtime.disk_used)} / {diskLimitMB} MB</b>
          </div>
          <div className="bar">
            <i className={quotaCls} style={{ width: `${Math.min(100, quotaPct)}%` }} />
          </div>
          <div className="sq-note">
            85% 触发警告，95% 触发严重告警{diskAutostop ? '；已开启超限自动停机' : ''}
          </div>
        </div>
      )}

      {/* 趋势 */}
      {pts.length === 0 ? (
        <div className="st-empty">
          暂无历史数据 —— 面板每分钟采样一次，稍后再来看即可。
          <br />
          <span className="muted">（历史数据保留 7 天）</span>
        </div>
      ) : (
        <div className="st-charts">
          <TrendChart title="CPU 使用率" points={pick('cpu')} color="var(--accent-solid)" fmt={(n) => n.toFixed(0)} unit="%" />
          <TrendChart title="内存占用" points={pick('mem')} color="var(--accent)" fmt={(n) => fmtBytes(n)} />
          <TrendChart title="在线玩家" points={pick('players')} color="var(--success)" fmt={(n) => n.toFixed(0)} unit=" 人" />
          <TrendChart title="TPS" points={pick('tps')} color="var(--warning)" fmt={(n) => n.toFixed(1)} />
        </div>
      )}
    </div>
  )
}
