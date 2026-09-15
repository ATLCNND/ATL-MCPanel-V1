import { useEffect, useMemo, useState } from 'react'
import {
  FileJob, InstanceTask, TaskAction,
  listInstanceTasks, createInstanceTask, updateInstanceTask, deleteInstanceTask, runInstanceTask,
  listJobs, cancelJob,
} from '../api'
import './ScheduledTasks.css'

const ACTIONS: { key: TaskAction; label: string; hint: string; destructive?: boolean }[] = [
  { key: 'start', label: '开机', hint: '启动实例（到点自动拉起服务器）' },
  { key: 'stop', label: '关机', hint: '优雅停止：先让服务器保存存档再退出，建议用它而不是强制关闭' },
  { key: 'restart', label: '重启', hint: '先优雅停止再启动，适合每日例行重启' },
  { key: 'kill', label: '强制关闭', hint: '直接杀进程，可能丢失未落盘的进度' , destructive: true },
  { key: 'command', label: '游戏指令', hint: '向控制台下发一条指令，例如 say 服务器将在 5 分钟后重启' },
]

type Freq = 'daily' | 'weekly' | 'monthly' | 'hourly' | 'custom'

const FREQS: { key: Freq; label: string }[] = [
  { key: 'daily', label: '每天' },
  { key: 'weekly', label: '每周' },
  { key: 'monthly', label: '每月' },
  { key: 'hourly', label: '每隔 N 小时' },
  { key: 'custom', label: '自定义 cron' },
]

const WEEKDAYS = ['周日', '周一', '周二', '周三', '周四', '周五', '周六']

/** 把界面上的选择拼成 5 段 cron。 */
function buildCron(freq: Freq, hh: string, mm: string, dow: string, dom: string, hours: string, raw: string): string {
  const H = String(Math.min(23, Math.max(0, parseInt(hh || '0', 10) || 0)))
  const M = String(Math.min(59, Math.max(0, parseInt(mm || '0', 10) || 0)))
  switch (freq) {
    case 'daily':
      return `${M} ${H} * * *`
    case 'weekly':
      return `${M} ${H} * * ${dow}`
    case 'monthly':
      return `${M} ${H} ${dom} * *`
    case 'hourly': {
      const n = Math.min(23, Math.max(1, parseInt(hours || '2', 10) || 2))
      return `${M} */${n} * * *`
    }
    default:
      return raw.trim()
  }
}

function fmtTime(s: string): string {
  if (!s) return '—'
  const d = new Date(s)
  if (isNaN(d.getTime())) return s
  return d.toLocaleString('zh-CN', { hour12: false })
}

/** 距离某个时刻还有多久。 */
function until(s: string): string {
  if (!s) return ''
  const d = new Date(s)
  if (isNaN(d.getTime())) return ''
  const sec = (d.getTime() - Date.now()) / 1000
  if (sec <= 0) return '即将执行'
  if (sec < 3600) return `${Math.ceil(sec / 60)} 分钟后`
  if (sec < 86400) return `${Math.floor(sec / 3600)} 小时后`
  const days = Math.floor(sec / 86400)
  const hours = Math.floor((sec % 86400) / 3600)
  return hours > 0 ? `${days} 天 ${hours} 小时后` : `${days} 天后`
}

const JOB_LABEL: Record<string, string> = { compress: '压缩', extract: '解压' }
const JOB_STATE: Record<string, string> = {
  queued: '排队中', running: '执行中', success: '已完成', failed: '失败', canceled: '已取消',
}

/**
 * ScheduledTasks 定时指令任务 + 排队任务。
 *
 * 与「定时备份」的区别：备份管的是数据的周期性留存，这里管的是
 * **实例本身的作息**（几点开机、几点关机、定时喊话）。
 * 两者都在「任务」页，因为用户关心的是同一件事：这台服务器会自动做什么。
 */
export default function ScheduledTasks({ instanceId, canWrite }: { instanceId: string; canWrite: boolean }) {
  const [tasks, setTasks] = useState<InstanceTask[]>([])
  const [jobs, setJobs] = useState<FileJob[]>([])
  const [error, setError] = useState('')
  const [msg, setMsg] = useState('')
  const [busy, setBusy] = useState(false)
  const [editing, setEditing] = useState(false)

  // 表单状态
  const [name, setName] = useState('')
  const [action, setAction] = useState<TaskAction>('stop')
  const [command, setCommand] = useState('say 服务器将在 5 分钟后重启')
  const [freq, setFreq] = useState<Freq>('daily')
  const [hh, setHh] = useState('23')
  const [mm, setMm] = useState('30')
  const [dow, setDow] = useState('1')
  const [dom, setDom] = useState('1')
  const [hours, setHours] = useState('6')
  const [raw, setRaw] = useState('0 4 * * *')

  const load = async () => {
    try {
      const [t, j] = await Promise.all([
        listInstanceTasks(instanceId).catch(() => []),
        listJobs(instanceId).catch(() => []),
      ])
      setTasks(Array.isArray(t) ? t : [])
      setJobs(Array.isArray(j) ? j : [])
    } catch (e: any) {
      setError(e.message)
    }
  }

  useEffect(() => {
    load()
    const t = setInterval(() => {
      // 只有存在进行中的任务时才轮询，空闲时完全不动（省掉无意义的请求）
      listJobs(instanceId).then((j) => setJobs(Array.isArray(j) ? j : [])).catch(() => {})
    }, 3000)
    return () => clearInterval(t)
  }, [instanceId])

  const cron = useMemo(
    () => buildCron(freq, hh, mm, dow, dom, hours, raw),
    [freq, hh, mm, dow, dom, hours, raw],
  )

  const activeJobs = jobs.filter((j) => j.state === 'queued' || j.state === 'running')

  const run = async (fn: () => Promise<any>, okMsg?: string) => {
    setBusy(true); setError(''); setMsg('')
    try {
      const r = await fn()
      setMsg(okMsg || r?.message || '操作完成')
      await load()
      return true
    } catch (e: any) {
      setError(e.message)
      return false
    } finally {
      setBusy(false)
    }
  }

  const submit = async () => {
    const ok = await run(() => createInstanceTask(instanceId, {
      name: name.trim() || undefined,
      action,
      command: action === 'command' ? command.trim() : '',
      cron,
      enabled: true,
    }))
    if (ok) {
      setEditing(false)
      setName('')
    }
  }

  const applyPreset = (a: TaskAction, f: Freq, h: string, m: string, label: string) => {
    setEditing(true)
    setAction(a)
    setFreq(f)
    setHh(h)
    setMm(m)
    setName(label)
  }

  return (
    <div className="sched-tasks">
      {error && <div className="error-banner">{error}</div>}
      {msg && <div className="success-banner">{msg}</div>}

      {/* ---------------- 定时指令任务 ---------------- */}
      <div className="task-card">
        <div className="tc-head">
          <span className={`tc-dot ${tasks.some((t) => t.enabled) ? 'on' : ''}`} />
          <span className="tc-name">定时指令任务</span>
          <span className="tc-count">共 {tasks.length} 条</span>
          <div className="spacer" />
          <button onClick={load} disabled={busy}>刷新</button>
          <button className="primary" onClick={() => setEditing(!editing)} disabled={!canWrite || busy}>
            {editing ? '收起' : '新建任务'}
          </button>
        </div>

        {!canWrite && (
          <div className="tc-note">
            定时任务会在无人值守时自动开关机与下发指令，因此需要实例<strong>归属者</strong>权限才能管理。
            你当前是该实例的协作者，可以查看但无法修改。
          </div>
        )}

        {/* 常用预设：把最常见的两种作息做成一键，省掉手填 cron */}
        {canWrite && !editing && (
          <div className="preset-row">
            <span className="muted">常用：</span>
            <button onClick={() => applyPreset('start', 'daily', '06', '00', '每天 06:00 开机')}>每天 06:00 开机</button>
            <button onClick={() => applyPreset('stop', 'daily', '23', '30', '每天 23:30 关机')}>每天 23:30 关机</button>
            <button onClick={() => applyPreset('restart', 'daily', '04', '00', '每天 04:00 重启')}>每天 04:00 重启</button>
            <button onClick={() => applyPreset('command', 'daily', '22', '00', '每日提醒')}>每日定时喊话</button>
          </div>
        )}

        {canWrite && editing && (
          <div className="task-form">
            <div className="tf-row">
              <label className="tf-field">
                <span>动作</span>
                <select value={action} onChange={(e) => setAction(e.target.value as TaskAction)}>
                  {ACTIONS.map((a) => (
                    <option key={a.key} value={a.key}>{a.label}{a.destructive ? '（有风险）' : ''}</option>
                  ))}
                </select>
              </label>
              <label className="tf-field grow">
                <span>任务名称（可留空自动生成）</span>
                <input value={name} onChange={(e) => setName(e.target.value)} placeholder="例如：每天深夜关机" />
              </label>
            </div>

            {action === 'command' && (
              <label className="tf-field">
                <span>要下发的指令（不含前导斜杠）</span>
                <input
                  value={command}
                  onChange={(e) => setCommand(e.target.value)}
                  placeholder="say 服务器将在 5 分钟后重启"
                  spellCheck={false}
                />
              </label>
            )}

            <div className="tf-row">
              <label className="tf-field">
                <span>重复方式</span>
                <select value={freq} onChange={(e) => setFreq(e.target.value as Freq)}>
                  {FREQS.map((f) => <option key={f.key} value={f.key}>{f.label}</option>)}
                </select>
              </label>

              {freq === 'hourly' ? (
                <label className="tf-field narrow">
                  <span>间隔</span>
                  <div className="inline-unit">
                    <input type="number" min={1} max={23} value={hours} onChange={(e) => setHours(e.target.value)} />
                    <em>小时</em>
                  </div>
                </label>
              ) : freq === 'custom' ? (
                <label className="tf-field grow">
                  <span>cron 表达式（分 时 日 月 周）</span>
                  <input value={raw} onChange={(e) => setRaw(e.target.value)} spellCheck={false} className="mono" />
                </label>
              ) : (
                <>
                  <label className="tf-field narrow">
                    <span>时间</span>
                    <div className="inline-unit">
                      <input type="number" min={0} max={23} value={hh} onChange={(e) => setHh(e.target.value)} />
                      <em>时</em>
                      <input type="number" min={0} max={59} value={mm} onChange={(e) => setMm(e.target.value)} />
                      <em>分</em>
                    </div>
                  </label>
                  {freq === 'weekly' && (
                    <label className="tf-field narrow">
                      <span>星期</span>
                      <select value={dow} onChange={(e) => setDow(e.target.value)}>
                        {WEEKDAYS.map((w, i) => <option key={w} value={String(i)}>{w}</option>)}
                      </select>
                    </label>
                  )}
                  {freq === 'monthly' && (
                    <label className="tf-field narrow">
                      <span>日期</span>
                      <select value={dom} onChange={(e) => setDom(e.target.value)}>
                        {Array.from({ length: 28 }, (_, i) => i + 1).map((d) => (
                          <option key={d} value={String(d)}>{d} 日</option>
                        ))}
                      </select>
                    </label>
                  )}
                </>
              )}
            </div>

            <div className="tf-preview">
              cron：<code className="mono">{cron || '（空）'}</code>
              <span className="tf-arrow">→</span>
              实际执行：<strong>{previewText(freq, action, hh, mm, dow, dom, hours, cron)}</strong>
            </div>

            <div className="tf-note">
              {ACTIONS.find((a) => a.key === action)?.hint}
              。面板停机期间错过的任务<strong>不会补跑</strong> —— 深夜的关机任务在早上补跑只会造成意外。
            </div>

            <div className="tf-actions">
              <button onClick={() => setEditing(false)} disabled={busy}>取消</button>
              <button className="primary" onClick={submit} disabled={busy || !cron.trim()}>
                {busy ? '提交中…' : '创建任务'}
              </button>
            </div>
          </div>
        )}

        {tasks.length === 0 ? (
          <div className="tc-empty">
            尚无定时任务。可以为实例配置「每天定时开机 / 关机」，或定时下发游戏内公告。
          </div>
        ) : (
          <table className="task-table">
            <thead>
              <tr>
                <th>任务</th>
                <th>动作</th>
                <th>计划</th>
                <th>下次执行</th>
                <th>上次执行</th>
                <th>状态</th>
                {canWrite && <th>操作</th>}
              </tr>
            </thead>
            <tbody>
              {tasks.map((t) => (
                <tr key={t.id} className={t.enabled ? '' : 'off'}>
                  <td>
                    <div className="t-name">{t.name}</div>
                    {t.creator && <div className="t-sub">由 {t.creator} 创建</div>}
                  </td>
                  <td>
                    <span className={`tag act-${t.action}`}>{t.action_label}</span>
                    {t.action === 'command' && <div className="t-sub mono">{t.command}</div>}
                  </td>
                  <td>
                    <div>{t.describe}</div>
                    <div className="t-sub mono">{t.cron}</div>
                  </td>
                  <td>{t.enabled ? <>{fmtTime(t.next_run)}<div className="t-sub">{until(t.next_run)}</div></> : '已停用'}</td>
                  <td>{fmtTime(t.last_run)}</td>
                  <td>
                    {t.last_state === 'failed'
                      ? <span className="status status-error" title={t.last_error}>失败</span>
                      : t.last_state === 'success'
                        ? <span className="status status-running">成功</span>
                        : <span className="muted">未执行</span>}
                    <div className="t-sub">成功 {t.run_count - t.fail_count} / 失败 {t.fail_count}</div>
                  </td>
                  {canWrite && (
                    <td>
                      <div className="row-actions">
                        <button
                          onClick={() => run(() => updateInstanceTask(t.id, { enabled: !t.enabled }), t.enabled ? '已停用' : '已启用')}
                          disabled={busy}
                        >
                          {t.enabled ? '停用' : '启用'}
                        </button>
                        <button onClick={() => run(() => runInstanceTask(t.id), `已执行：${t.name}`)} disabled={busy}>
                          立即执行
                        </button>
                        <button
                          className="danger"
                          onClick={() => confirm(`删除任务「${t.name}」？`) && run(() => deleteInstanceTask(t.id), '任务已删除')}
                          disabled={busy}
                        >
                          删除
                        </button>
                      </div>
                    </td>
                  )}
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </div>

      {/* ---------------- 排队任务（压缩 / 解压） ---------------- */}
      <div className="task-card">
        <div className="tc-head">
          <span className={`tc-dot ${activeJobs.length > 0 ? 'on' : ''}`} />
          <span className="tc-name">排队任务</span>
          <span className="tc-count">
            {activeJobs.length > 0 ? `${activeJobs.length} 个进行中` : `共 ${jobs.length} 条记录`}
          </span>
          <div className="spacer" />
          <span className="muted">由节点公共队列串行执行（压缩 / 解压）</span>
        </div>
        {jobs.length === 0 ? (
          <div className="tc-empty">
            暂无排队任务。在「文件」页对文件或目录点「压缩」「解压」即可提交。
          </div>
        ) : (
          <table className="task-table">
            <thead>
              <tr>
                <th>类型</th>
                <th>源</th>
                <th>目标</th>
                <th>进度</th>
                <th>状态</th>
                {canWrite && <th>操作</th>}
              </tr>
            </thead>
            <tbody>
              {jobs.slice(0, 10).map((j) => (
                <tr key={j.job_id}>
                  <td><span className="tag act-command">{JOB_LABEL[j.kind] || j.kind}</span></td>
                  <td className="mono small">{j.src}</td>
                  <td className="mono small">{j.dst}</td>
                  <td>
                    <div className="mini-bar"><div className="fill" style={{ width: `${j.progress}%` }} /></div>
                  </td>
                  <td>
                    <span className={`job-state ${j.state}`}>{JOB_STATE[j.state] || j.state}</span>
                    {(j.error || j.message) && <div className="t-sub" title={j.error || j.message}>{j.error || j.message}</div>}
                  </td>
                  {canWrite && (
                    <td>
                      {(j.state === 'queued' || j.state === 'running') && (
                        <button className="danger" onClick={() => run(() => cancelJob(j.job_id), '已取消')} disabled={busy}>
                          取消
                        </button>
                      )}
                    </td>
                  )}
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </div>

      {/* ---------------- 其他自动任务 ---------------- */}
      <div className="task-card muted-card">
        <div className="tc-head">
          <span className="tc-dot" />
          <span className="tc-name">其他自动任务</span>
        </div>
        <div className="tc-body">
          <div className="tc-row">
            <span className="k">健康检查</span>
            <b>每分钟检测实例存活，异常时产生告警</b>
          </div>
          <div className="tc-row">
            <span className="k">指标采样</span>
            <b>每分钟记录 CPU / 内存 / 玩家 / TPS，保留 7 天</b>
          </div>
          <div className="tc-row">
            <span className="k">调度检查</span>
            <b>面板每 60 秒检查一次到期的定时任务；停机期间错过的任务不补跑</b>
          </div>
        </div>
      </div>
    </div>
  )
}

/** 预览：把界面上的选择翻译成一句人话。 */
function previewText(
  freq: Freq, action: TaskAction, hh: string, mm: string, dow: string, dom: string, hours: string, cron: string,
): string {
  const pad = (s: string) => String(s).padStart(2, '0')
  const clock = `${pad(hh)}:${pad(mm)}`
  const label = ACTIONS.find((a) => a.key === action)?.label || action
  switch (freq) {
    case 'daily':
      return `每天 ${clock} ${label}`
    case 'weekly':
      return `每${WEEKDAYS[Number(dow)] || ''} ${clock} ${label}`
    case 'monthly':
      return `每月 ${dom} 日 ${clock} ${label}`
    case 'hourly':
      return `每隔 ${hours} 小时（第 ${Number(mm)} 分）${label}`
    default:
      return `按 cron「${cron}」${label}`
  }
}
