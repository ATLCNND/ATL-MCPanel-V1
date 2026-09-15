import { useEffect, useState } from 'react'
import { listAuditLogs } from '../api'
import './AuditPage.css'

/** 操作类型 → 中文说明与分组色彩 */
const ACTION_LABEL: Record<string, { text: string; tone: string }> = {
  login: { text: '登录', tone: 'info' },
  create_user: { text: '创建用户', tone: 'info' },
  delete_user: { text: '删除用户', tone: 'danger' },
  change_password: { text: '修改密码', tone: 'warn' },
  upload_avatar: { text: '上传头像', tone: 'info' },
  delete_avatar: { text: '移除头像', tone: 'warn' },
  review_avatar: { text: '审核头像', tone: 'info' },
  create_instance: { text: '创建实例', tone: 'info' },
  start_instance: { text: '启动实例', tone: 'success' },
  stop_instance: { text: '停止实例', tone: 'warn' },
  restart_instance: { text: '重启实例', tone: 'warn' },
  kill_instance: { text: '强制关闭实例', tone: 'danger' },
  delete_instance: { text: '删除实例', tone: 'danger' },
  create_backup: { text: '创建备份', tone: 'info' },
  delete_backup: { text: '删除备份', tone: 'warn' },
  restore_backup: { text: '回滚备份', tone: 'danger' },
  create_node: { text: '登记节点', tone: 'info' },
  delete_node: { text: '删除节点', tone: 'danger' },
  deploy_node: { text: '部署节点', tone: 'info' },
  create_tunnel: { text: '创建隧道', tone: 'info' },
  delete_tunnel: { text: '删除隧道', tone: 'warn' },
  set_backup_schedule: { text: '修改备份计划', tone: 'warn' },
  save_backup_policy: { text: '保存备份策略', tone: 'warn' },
  delete_backup_policy: { text: '删除备份策略', tone: 'danger' },
  grant_assignment: { text: '授予权限', tone: 'info' },
  revoke_assignment: { text: '撤销权限', tone: 'warn' },
}

function labelOf(action: string) {
  return ACTION_LABEL[action] || { text: action, tone: 'plain' }
}

/** 相对时间：审计场景下"多久之前"比绝对时间更好判断 */
function fmtTime(s: string): string {
  if (!s) return '—'
  const d = new Date(s)
  if (isNaN(d.getTime())) return s
  const diff = (Date.now() - d.getTime()) / 1000
  if (diff < 60) return `${Math.max(0, Math.floor(diff))} 秒前`
  if (diff < 3600) return `${Math.floor(diff / 60)} 分钟前`
  if (diff < 86400) return `${Math.floor(diff / 3600)} 小时前`
  if (diff < 86400 * 7) return `${Math.floor(diff / 86400)} 天前`
  return d.toLocaleString('zh-CN', { hour12: false })
}

export default function AuditPage() {
  const [logs, setLogs] = useState<any[]>([])
  const [action, setAction] = useState('')
  const [username, setUsername] = useState('')
  const [limit, setLimit] = useState(200)
  const [error, setError] = useState('')
  const [loading, setLoading] = useState(false)

  const load = async () => {
    setLoading(true)
    try {
      const qs = new URLSearchParams()
      qs.set('limit', String(limit))
      if (action.trim()) qs.set('action', action.trim())
      if (username.trim()) qs.set('username', username.trim())
      const r = await fetch(`/api/audit-logs?${qs}`, {
        headers: { Authorization: `Bearer ${localStorage.getItem('atlmcpanel_token') || ''}` },
      }).then((res) => res.json())
      setLogs(Array.isArray(r) ? r : [])
      setError('')
    } catch (e: any) {
      setError(e.message)
    } finally {
      setLoading(false)
    }
  }

  useEffect(() => { load() }, [])
  useEffect(() => { void 0 }, [logs])

  // 按操作类型做客户端聚合，便于一眼看出"谁在频繁做什么"
  const byUser = new Map<string, number>()
  for (const l of logs) byUser.set(l.username, (byUser.get(l.username) || 0) + 1)

  return (
    <div className="audit-page">
      <div className="page-toolbar">
        <input
          className="audit-search"
          placeholder="操作类型（如 delete / instance）"
          value={action}
          onChange={(e) => setAction(e.target.value)}
          onKeyDown={(e) => e.key === 'Enter' && load()}
        />
        <input
          className="audit-search"
          placeholder="用户名"
          value={username}
          onChange={(e) => setUsername(e.target.value)}
          onKeyDown={(e) => e.key === 'Enter' && load()}
        />
        <select value={limit} onChange={(e) => setLimit(Number(e.target.value))}>
          <option value={100}>最近 100 条</option>
          <option value={200}>最近 200 条</option>
          <option value={500}>最近 500 条</option>
          <option value={1000}>最近 1000 条</option>
        </select>
        <button className="primary" onClick={load} disabled={loading}>
          {loading ? '查询中…' : '查询'}
        </button>
        <button onClick={() => { setAction(''); setUsername(''); setLimit(200); setTimeout(load, 0) }}>重置</button>
      </div>

      {error && <div className="error-banner">{error}</div>}

      <div className="audit-summary">
        <span>共 <b>{logs.length}</b> 条记录</span>
        {byUser.size > 0 && (
          <span className="audit-users">
            涉及用户：
            {[...byUser.entries()]
              .sort((a, b) => b[1] - a[1])
              .slice(0, 6)
              .map(([u, n]) => (
                <span className="au-chip" key={u}>{u} <b>{n}</b></span>
              ))}
          </span>
        )}
      </div>

      <div className="audit-table-wrap">
        <table className="audit-table">
          <thead>
            <tr>
              <th style={{ width: 90 }}>时间</th>
              <th style={{ width: 110 }}>用户</th>
              <th style={{ width: 130 }}>操作</th>
              <th style={{ width: 150 }}>对象</th>
              <th>详情</th>
              <th style={{ width: 130 }}>来源 IP</th>
            </tr>
          </thead>
          <tbody>
            {logs.map((l) => {
              const lb = labelOf(l.action)
              return (
                <tr key={l.id}>
                  <td className="au-time" title={l.created_at}>{fmtTime(l.created_at)}</td>
                  <td>{l.username}</td>
                  <td><span className={`au-action ${lb.tone}`}>{lb.text}</span></td>
                  <td className="mono au-target">{l.target || '—'}</td>
                  <td className="au-detail" title={l.detail}>{l.detail || '—'}</td>
                  <td className="mono au-ip">{l.ip || '—'}</td>
                </tr>
              )
            })}
            {logs.length === 0 && !loading && (
              <tr><td colSpan={6} className="empty">没有符合条件的记录</td></tr>
            )}
          </tbody>
        </table>
      </div>
    </div>
  )
}
