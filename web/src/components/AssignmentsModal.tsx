import { useEffect, useState } from 'react'
import { listAssignments, grantAssignment, revokeAssignment, listUsers } from '../api'
import './AssignmentsModal.css'

export default function AssignmentsModal({ instanceId, onClose }: { instanceId: string; onClose: () => void }) {
  const [list, setList] = useState<any[]>([])
  const [users, setUsers] = useState<any[]>([])
  const [username, setUsername] = useState('')
  const [level, setLevel] = useState('collab')
  const [error, setError] = useState('')
  const [msg, setMsg] = useState('')

  const load = async () => {
    try {
      setList(await listAssignments(instanceId))
    } catch (e: any) {
      setError(e.message)
    }
  }

  const loadUsers = async () => {
    try {
      const all = await listUsers()
      // 管理员默认拥有全部权限，不参与单独授权
      setUsers(all.filter((u: any) => u.role !== 'admin'))
    } catch (e: any) {
      setError(e.message)
    }
  }

  useEffect(() => {
    load()
    loadUsers()
  }, [instanceId])

  const grant = async () => {
    setError(''); setMsg('')
    if (!username) { setError('请选择账号'); return }
    try {
      await grantAssignment(instanceId, username, level)
      setMsg(`已授予 ${username} ${level} 权限`)
      setUsername('')
      await load()
    } catch (e: any) {
      setError(e.message)
    }
  }

  const revoke = async (userId: number, name: string) => {
    if (!confirm(`确定撤销 ${name} 的权限？`)) return
    try {
      await revokeAssignment(instanceId, userId)
      await load()
    } catch (e: any) {
      setError(e.message)
    }
  }

  // 已被授权的用户名集合（用于在下拉中标注）
  const assignedNames = new Set(list.map((a) => a.username))

  // 遮罩统一用 .modal-mask；.modal-mask-top 只是把层叠压到其它弹窗之上
  // （授权弹窗可能与实例列表的其它弹窗同时出现）—— 见 components.css 的说明
  return (
    <div className="modal-mask modal-mask-top" onClick={onClose}>
      <div className="assign-modal" onClick={(e) => e.stopPropagation()}>
        <div className="modal-header">
          <span>实例授权 - {instanceId}</span>
          <button onClick={onClose}>✕</button>
        </div>

        {error && <div className="modal-error">{error}</div>}
        {msg && <div className="modal-success">{msg}</div>}

        <div className="assign-form">
          <h4>从账号列表中选择并授权</h4>
          <div className="row">
            <select value={username} onChange={(e) => setUsername(e.target.value)}>
              <option value="">— 请选择账号 —</option>
              {users.map((u) => (
                <option key={u.id} value={u.username}>
                  {u.username}{assignedNames.has(u.username) ? '（已授权，可修改级别）' : ''}
                </option>
              ))}
            </select>
            <select value={level} onChange={(e) => setLevel(e.target.value)}>
              <option value="viewer">viewer（只读）</option>
              <option value="collab">collab（可启停+控制台）</option>
              <option value="owner">owner（全部权限，不含删除/授权）</option>
            </select>
            <button className="primary" onClick={grant}>授权</button>
          </div>
          {users.length === 0 && (
            <div className="assign-hint-inline">
              暂无可授权的普通账号，请先到「账户设置 → 用户管理」创建账号。
            </div>
          )}
        </div>

        <div className="assign-list">
          <h4>已有授权</h4>
          <table>
            <thead>
              <tr><th>用户名</th><th>级别</th><th>操作</th></tr>
            </thead>
            <tbody>
              {list.map((a) => (
                <tr key={a.id}>
                  <td>{a.username}</td>
                  <td><span className="level-badge">{a.level}</span></td>
                  <td>
                    <button className="danger" onClick={() => revoke(a.user_id, a.username)}>撤销</button>
                  </td>
                </tr>
              ))}
              {list.length === 0 && (
                <tr><td colSpan={3} className="empty">暂无授权（管理员默认拥有全部权限，无需授权）</td></tr>
              )}
            </tbody>
          </table>
        </div>

        <div className="assign-hint">
          说明：授权管理仅限管理员操作；管理员对所有实例默认拥有 owner 权限且不可被撤销。
          owner 可启停、使用控制台、管理文件，但删除实例与授权仍需管理员。
        </div>
      </div>
    </div>
  )
}