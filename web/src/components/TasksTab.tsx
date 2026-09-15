import { useEffect, useState } from 'react'
import {
  ScheduleConfig, BackupPolicy, BackupItem, Instance,
  getSchedule, listBackupPolicies, listBackups,
} from '../api'
import ScheduledTasks from './ScheduledTasks'
import ExpiryPanel from './ExpiryPanel'
import './TasksTab.css'

function fmtTime(s: string): string {
  if (!s) return '—'
  const d = new Date(s)
  if (isNaN(d.getTime())) return s
  return d.toLocaleString('zh-CN', { hour12: false })
}
function fmtSize(n: number): string {
  if (!n) return '—'
  if (n < 1024 * 1024 * 1024) return (n / 1024 / 1024).toFixed(1) + ' MB'
  return (n / 1024 / 1024 / 1024).toFixed(2) + ' GB'
}
/** 下次执行还有多久 */
function untilNext(s: string): string {
  if (!s) return '—'
  const d = new Date(s)
  if (isNaN(d.getTime())) return s
  const sec = (d.getTime() - Date.now()) / 1000
  if (sec <= 0) return '即将执行'
  if (sec < 3600) return `${Math.ceil(sec / 60)} 分钟后`
  if (sec < 86400) return `${Math.floor(sec / 3600)} 小时后`
  return `${Math.floor(sec / 86400)} 天后`
}

const isAuto = (b: BackupItem) => (b.name || '').toLowerCase() === 'auto'

/**
 * TasksTab 任务页。
 *
 * 汇总该实例上所有「会自动发生的事」：
 *   1. **到期时间** —— 到点自动停机（管理员设置，与删除同级权限）
 *   2. 定时指令任务 —— 几点开机、几点关机、定时喊话（用户自建，可直接管理）
 *   3. 排队任务 —— 压缩 / 解压等重 IO 操作，走节点公共队列
 *   4. 定时备份与保留策略 —— 数据留存
 * 与「备份」页的区别：备份页管内容（有哪些备份、回滚），这里管调度
 *（什么时候跑、跑得对不对）。
 *
 * 到期面板原先在详情页右栏，本轮搬到这页 —— 它和上面几条是同一类东西，
 * 而且右栏太挤、放不下一整行。
 */
export default function TasksTab(
  { instanceId, canWrite, isAdmin, inst, canManageExpiry, onExpiryChanged }: {
    instanceId: string
    canWrite: boolean
    isAdmin?: boolean
    /** 实例记录（到期字段在里面），由 InstanceDetail 传入，避免这里重复拉一次 */
    inst: Instance | null
    /** 能否设置到期时间：总管理员 / 该节点的节点用户 */
    canManageExpiry: boolean
    onExpiryChanged: () => void | Promise<void>
  },
) {
  const [sched, setSched] = useState<ScheduleConfig | null>(null)
  const [policy, setPolicy] = useState<BackupPolicy | null>(null)
  const [backups, setBackups] = useState<BackupItem[]>([])
  const [error, setError] = useState('')
  const [loading, setLoading] = useState(false)

  const load = async () => {
    setLoading(true)
    try {
      const [s, b] = await Promise.all([
        getSchedule(instanceId),
        listBackups(instanceId).catch(() => []),
      ])
      setSched(s)
      setBackups(Array.isArray(b) ? b : [])

      // 策略详情仅管理员可读（普通用户只能看到 schedule 里摘要过的描述）
      if (isAdmin && s.policy_id) {
        const list = await listBackupPolicies().catch(() => [])
        setPolicy(Array.isArray(list) ? list.find((p) => p.id === s.policy_id) || null : null)
      }
      setError('')
    } catch (e: any) {
      setError(e.message)
    } finally {
      setLoading(false)
    }
  }

  useEffect(() => { load() }, [instanceId, isAdmin])

  const autos = backups.filter(isAuto).sort((a, b) => b.created_at - a.created_at)
  const lastAuto = autos[0]
  const manuals = backups.filter((b) => !isAuto(b))

  const enabled = !!sched?.enabled

  return (
    <div className="tasks-tab">
      <div className="page-toolbar">
        <span className="muted">任务由面板调度器每分钟检查到期情况</span>
        <button onClick={load} disabled={loading}>{loading ? '加载中…' : '刷新'}</button>
      </div>

      {error && <div className="error-banner">{error}</div>}

      {/* 到期时间：放在最上面 —— 它是"这台实例会不会被自动停掉"的答案，
          比下面几条调度配置更需要一眼看到。只有管理员 / 该节点的节点用户可见。 */}
      {canManageExpiry && (
        <ExpiryPanel instanceId={instanceId} inst={inst} onChanged={onExpiryChanged} />
      )}

      {/* 定时指令任务 + 排队任务：普通用户（实例 owner）即可管理 */}
      <ScheduledTasks instanceId={instanceId} canWrite={canWrite} />

      {/* 定时备份任务 */}
      <div className={`task-card ${enabled ? 'on' : 'off'}`}>
        <div className="tc-head">
          <span className={`tc-dot ${enabled ? 'on' : 'off'}`} />
          <span className="tc-name">定时备份</span>
          <span className={`status ${enabled ? 'status-running' : 'status-stopped'}`}>
            {enabled ? '已启用' : '未启用'}
          </span>
          {sched?.last_error && <span className="tc-err">上次失败：{sched.last_error}</span>}
        </div>

        {sched ? (
          <div className="tc-body">
            <div className="tc-row"><span className="k">执行间隔</span><b>每 {sched.effective_hours || sched.interval_hours} 小时</b></div>
            <div className="tc-row"><span className="k">上次执行</span><b>{fmtTime(sched.last_run)}</b></div>
            <div className="tc-row">
              <span className="k">下次执行</span>
              <b>{enabled ? `${fmtTime(sched.next_run)}（${untilNext(sched.next_run)}）` : '—'}</b>
            </div>
            <div className="tc-row"><span className="k">生效策略</span><b>{sched.policy_name || '默认策略'}</b></div>
            <div className="tc-row"><span className="k">淘汰规则</span><b className="tc-desc">{sched.policy_desc || '—'}</b></div>
            <div className="tc-row"><span className="k">手动备份保留</span><b>{sched.manual_keep || 0} 份（独立计数）</b></div>
            <div className="tc-row"><span className="k">包含配置文件</span><b>{sched.include_config ? '是' : '否'}</b></div>
          </div>
        ) : (
          <div className="tc-empty">尚未配置备份计划</div>
        )}
      </div>

      {/* 策略详情（仅管理员） */}
      {isAdmin && policy && (
        <div className="task-card">
          <div className="tc-head">
            <span className="tc-dot on" />
            <span className="tc-name">保留策略 · {policy.name}</span>
          </div>
          <div className="tc-body">
            <div className="tc-row"><span className="k">自动间隔</span><b>每 {policy.auto_interval_hours} 小时</b></div>
            <div className="tc-row"><span className="k">手动保留</span><b>{policy.manual_keep} 份</b></div>
            <div className="tc-row">
              <span className="k">梯度档位</span>
              <b className="tc-desc">
                {(policy.tiers || []).length === 0
                  ? '未配置（自动备份不淘汰）'
                  : policy.tiers.map((t, i, arr) => {
                      const lo = i > 0 ? arr[i - 1].within_hours : 0
                      const r = lo === 0 ? `最近 ${t.within_hours}h` : `${lo}~${t.within_hours}h`
                      return `${r} 留 ${t.keep} 份`
                    }).join('；')}
              </b>
            </div>
            {policy.used_by > 0 && (
              <div className="tc-row"><span className="k">被引用</span><b>{policy.used_by} 个实例</b></div>
            )}
          </div>
        </div>
      )}

      {/* 最近自动备份 */}
      <div className="task-card">
        <div className="tc-head">
          <span className="tc-dot on" />
          <span className="tc-name">最近自动备份</span>
          <span className="tc-count">共 {autos.length} 份</span>
        </div>
        {autos.length === 0 ? (
          <div className="tc-empty">尚无自动备份记录</div>
        ) : (
          <div className="tc-body">
            {autos.slice(0, 5).map((b) => (
              <div className="tc-row" key={b.backup_id}>
                <span className="k mono">{fmtTime(new Date(b.created_at * 1000).toISOString())}</span>
                <b>{fmtSize(b.size)}</b>
              </div>
            ))}
            {lastAuto && (
              <div className="tc-note">
                最近一次：<b>{fmtTime(new Date(lastAuto.created_at * 1000).toISOString())}</b>
                ，距今 {untilNext(new Date(lastAuto.created_at * 1000).toISOString()).replace('后', '前').replace('即将执行', '刚刚')}
              </div>
            )}
          </div>
        )}
      </div>

      {/* 手动备份概览 */}
      <div className="task-card">
        <div className="tc-head">
          <span className="tc-dot on" />
          <span className="tc-name">手动备份</span>
          <span className="tc-count">共 {manuals.length} 份</span>
        </div>
        <div className="tc-body">
          {manuals.length === 0 ? (
            <div className="tc-empty">尚无手动备份。建议在重大变更（更换核心、开荒）前手动备份一次。</div>
          ) : (
            manuals.slice(0, 5).map((b) => (
              <div className="tc-row" key={b.backup_id}>
                <span className="k">{b.name}</span>
                <b>{fmtSize(b.size)}</b>
              </div>
            ))
          )}
        </div>
      </div>

      {/* 其它自动任务的说明已并入上方「定时指令任务」区块，
          这里不再重复一份（同一页出现两块同样的"健康检查/指标采样"会让人以为有两套） */}
    </div>
  )
}
