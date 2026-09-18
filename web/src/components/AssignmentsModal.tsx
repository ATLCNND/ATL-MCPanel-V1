import { useEffect, useState } from 'react'
import { listAssignments, grantAssignment, revokeAssignment, listUsers, currentUser } from '../api'
import './AssignmentsModal.css'

/**
 * AssignmentsModal 实例授权（协作者管理）。
 *
 * 两种身份两种输入方式，这是刻意的：
 *
 *   - **总管理员**：从下拉里挑账号 —— 他能读全站用户列表（GET /api/users）。
 *   - **实例拥有者 / 该节点的节点用户**：手填用户名。
 *     他们**不该**拿到全站账号清单（那是管理员信息），
 *     但"把实例分享给谁"本来就是拥有者的权力（级别定义里 owner 含授权）。
 *     以前这里只按管理员做，前端却给 owner 也显示了授权按钮，
 *     于是非管理员点进来必然 403。现在后端按同一口径判权，
 *     前端则按身份换输入方式 —— 不因为"要显示下拉"就把用户表暴露出去。
 */
export default function AssignmentsModal({ instanceId, onClose }: { instanceId: string; onClose: () => void }) {
  const [list, setList] = useState<any[]>([])
  const [users, setUsers] = useState<any[]>([])
  const [username, setUsername] = useState('')
  const [level, setLevel] = useState('collab')
  const [error, setError] = useState('')
  const [msg, setMsg] = useState('')

  const isAdmin = currentUser()?.role === 'admin'

  const load = async () => {
    try {
      setList(await listAssignments(instanceId))
    } catch (e: any) {
      setError(e.message)
    }
  }

  const loadUsers = async () => {
    // 非管理员拿不到用户列表（403 是预期的，不是错误）—— 静默跳过，
    // 让输入框退化成手填，而不是在弹窗顶部糊一条红色错误。
    if (!isAdmin) return
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
    const name = username.trim()
    if (!name) { setError(isAdmin ? '请选择账号' : '请填写要授权的用户名'); return }
    try {
      await grantAssignment(instanceId, name, level)
      setMsg(`已授予 ${name} ${level} 权限`)
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
          <h4>{isAdmin ? '从账号列表中选择并授权' : '填写用户名并授权'}</h4>
          <div className="row">
            {isAdmin ? (
              <select value={username} onChange={(e) => setUsername(e.target.value)}>
                <option value="">— 请选择账号 —</option>
                {users.map((u) => (
                  <option key={u.id} value={u.username}>
                    {u.username}{assignedNames.has(u.username) ? '（已授权，可修改级别）' : ''}
                  </option>
                ))}
              </select>
            ) : (
              <input
                value={username}
                onChange={(e) => setUsername(e.target.value)}
                placeholder="对方的登录用户名"
                spellCheck={false}
                list="assign-known-users"
              />
            )}
            <select value={level} onChange={(e) => setLevel(e.target.value)}>
              <option value="viewer">viewer（只读）</option>
              <option value="collab">collab（可启停+控制台）</option>
              <option value="owner">owner（全部权限，不含删除/授权）</option>
            </select>
            <button className="primary" onClick={grant}>授权</button>
          </div>
          {/* 已授权的人名做候选：这不是"全站用户列表"，
              只是这台实例上已经存在的关系，不构成信息泄露 */}
          <datalist id="assign-known-users">
            {list.map((a) => <option key={a.user_id} value={a.username} />)}
          </datalist>
          {isAdmin && users.length === 0 && (
            <div className="assign-hint-inline">
              暂无可授权的普通账号，请先到「账户设置 → 用户管理」创建账号。
            </div>
          )}
          {!isAdmin && (
            <div className="assign-hint-inline">
              你只能管理本实例的授权。用户名需要填对方的**登录名**（不是昵称）。
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
          说明：<b>总管理员</b>对所有实例默认拥有 owner 权限且不可被撤销；
          <b>实例拥有者</b>（owner）可以管理本实例的协作者。
          owner 可启停、使用控制台、管理文件与授权，但**删除实例**仍需管理员。
        </div>
      </div>
    </div>
  )
}