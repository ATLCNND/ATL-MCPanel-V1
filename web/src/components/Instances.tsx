import { useEffect, useState, useCallback } from 'react'
import {
  Instance, MyNode, MyPortLine,
  listInstances, listMyNodes, listMyPorts,
  instanceAction, currentUser, levelAtLeast, isNodeUser,
  instanceIconUrl,
} from '../api'
import AssignmentsModal from './AssignmentsModal'
import CreateInstanceDialog from './CreateInstanceDialog'
import Avatar from './Avatar'
import './Instances.css'

/**
 * 实例列表页。
 *
 * 创建实例的表单**不在这里**：它已经抽成 `CreateInstanceDialog`（独立弹窗 + 分页，见 §11.3 A1）。
 * 之所以要拆开，是因为那张表单有十几个字段 + 每个字段一句常驻说明，内联展开会把列表
 * 整个顶下去；而且它自己的状态（节点资源/JDK/容器化能力/端口配额）跟列表页互不相干。
 */
export default function Instances({ onOpen }: {
  onOpen: (id: string, name: string, status: string, level: string) => void
}) {
  const [instances, setInstances] = useState<Instance[]>([])
  const [showCreate, setShowCreate] = useState(false)
  const [assignTarget, setAssignTarget] = useState<string | null>(null)
  const [error, setError] = useState('')
  const [busy, setBusy] = useState(false)
  const [deleteTarget, setDeleteTarget] = useState<Instance | null>(null)
  const [removeFiles, setRemoveFiles] = useState(false)
  const user = currentUser()

  // 可创建实例的节点（弹窗用），以及穿透端口配额（弹窗的「公网端口」页用）
  const [nodes, setNodes] = useState<MyNode[]>([])
  const [portLines, setPortLines] = useState<MyPortLine[]>([])

  const refresh = useCallback(async () => {
    try {
      const list = await listInstances()
      // 防御：仅接受数组，避免异常响应导致列表渲染崩溃（表现为“实例消失”）
      setInstances(Array.isArray(list) ? list : [])
      setError('')
    } catch (e: any) {
      // 失败时保留上一次的列表，只提示错误（不清空，避免误以为实例被删）
      setError(e.message)
    }
  }, [])

  useEffect(() => {
    refresh()
  }, [refresh])

  // 我的穿透端口配额（与节点无关，只跟"我是谁"有关，所以只拉一次）
  useEffect(() => {
    listMyPorts()
      .then((list) => setPortLines(Array.isArray(list) ? list : []))
      .catch(() => setPortLines([]))
  }, [user?.username])

  // 可创建实例的节点：总管理员拿到全部节点，节点用户只拿到被授权的节点，
  // 普通用户拿到空列表（前端据此隐藏「创建实例」入口）。
  // 用 /api/my/nodes 而不是 /api/nodes：后者仅管理员可调，且会带出 SSH 凭据等运维字段。
  useEffect(() => {
    listMyNodes()
      .then((list) => setNodes(Array.isArray(list) ? list : []))
      .catch((e) => setError(e.message))
  }, [])

  const doAction = async (id: string, action: 'start' | 'stop' | 'restart' | 'delete') => {
    setBusy(true)
    setError('')
    try {
      await instanceAction(id, action)
      await refresh()
    } catch (e: any) {
      setError(e.message)
      // 失败时也刷新，确保界面状态与后端一致（例如“实例已在运行”）
      await refresh()
    } finally {
      setBusy(false)
    }
  }

  const doDelete = async () => {
    if (!deleteTarget) return
    setBusy(true)
    setError('')
    try {
      await instanceAction(deleteTarget.instance_id, 'delete', { removeFiles })
      setDeleteTarget(null)
      setRemoveFiles(false)
      await refresh()
    } catch (e: any) {
      setError(e.message)
      await refresh()
    } finally {
      setBusy(false)
    }
  }

  // 能创建实例 = 至少有一个可管理节点（普通用户为空，看不到入口）
  const canCreate = nodes.length > 0

  return (
    <div className="instances">
      <div className="page-toolbar">
        {canCreate && (
          <button className="primary" onClick={() => setShowCreate(true)}>
            + 创建实例
          </button>
        )}
        {!canCreate && isNodeUser(user?.role) && (
          <span className="muted">你还未被分配任何节点，请联系总管理员在「用户管理」里授权。</span>
        )}
        <button onClick={refresh}>刷新</button>
      </div>

      {assignTarget && (
        <AssignmentsModal
          instanceId={assignTarget}
          onClose={() => setAssignTarget(null)}
        />
      )}

      {/* 删除确认：把"删什么"和"留不留文件"分开讲清楚。
          面板与 Daemon 的默认行为是**只注销实例、保留磁盘文件**，
          所以这里默认不勾选；勾选后世界存档、插件、jar、备份全部不可恢复。 */}
      {deleteTarget && (
        <div className="modal-mask" onClick={() => !busy && setDeleteTarget(null)}>
          <div className="del-dialog" onClick={(e) => e.stopPropagation()}>
            <div className="del-title">删除实例「{deleteTarget.name}」</div>
            <div className="del-body">
              <p>
                实例 ID <code className="mono">{deleteTarget.instance_id}</code> 的
                <b>实例记录、授权、定时备份计划、穿透隧道</b>都会被移除。
              </p>
              <label className="del-check">
                <input
                  type="checkbox"
                  checked={removeFiles}
                  onChange={(e) => setRemoveFiles(e.target.checked)}
                />
                <span>
                  同时<strong>彻底删除磁盘文件</strong>
                  <em>（世界存档、插件、jar、备份——不可恢复）</em>
                </span>
              </label>
              <p className="del-note">
                {removeFiles
                  ? '⚠️ 勾选后磁盘上的实例目录会被整目录删除，无法找回。'
                  : '不勾选时，磁盘上的实例目录会**原样保留**（面板不再追踪它，可用文件管理器手动清理）。'}
              </p>
            </div>
            <div className="del-actions">
              <button onClick={() => setDeleteTarget(null)} disabled={busy}>取消</button>
              <button className="danger" onClick={doDelete} disabled={busy}>
                {busy ? '删除中…' : removeFiles ? '彻底删除' : '删除实例'}
              </button>
            </div>
          </div>
        </div>
      )}

      {error && <div className="error-banner">{error}</div>}

      {showCreate && (
        <CreateInstanceDialog
          nodes={nodes}
          portLines={portLines}
          instances={instances}
          onClose={() => setShowCreate(false)}
          onCreated={refresh}
        />
      )}

      {instances.length === 0 ? (
        <div className="empty-state">
          {user?.role === 'admin'
            ? '暂无实例，点击「创建实例」开始'
            : '暂无可用实例（请联系管理员为你分配）'}
        </div>
      ) : (
        <div className="inst-grid">
          {instances.map((inst) => {
            const canOperate = levelAtLeast(inst.level, 'collab')
            // live_status 来自 Daemon，比数据库里的 status 更实时；
            // unknown 表示节点不可达，此时退回数据库记录的状态
            const live =
              inst.live_status && inst.live_status !== 'unknown' ? inst.live_status : inst.status
            const cores = inst.cpu_quota ? inst.cpu_quota / 100 : 0

            return (
              <div className={`inst-card ${live}`} key={inst.instance_id}>
                <div className="ic-head">
                  {/* 实例目录里有 server-icon.png / icon.png 就显示它，否则首字母 */}
                  <Avatar
                    url={instanceIconUrl(inst)}
                    name={inst.name || inst.instance_id}
                    size={34}
                    radius="var(--r-lg)"
                    className="ic-avatar"
                    initialsLength={1}
                  />
                  <div className="ic-title">
                    <div className="ic-name" title={inst.name}>{inst.name}</div>
                    <div className="ic-id mono">{inst.instance_id}</div>
                  </div>
                  <span className={`status status-${live}`}>{live}</span>
                </div>

                <div className="ic-meta">
                  <span className="ic-tag">{inst.core_type}</span>
                  <span className="mono" title="游戏端口">{inst.port}</span>
                  <span title="内存上限">{inst.max_mem}</span>
                  {cores > 0 && <span title="CPU 配额">{cores} 核</span>}
                  {inst.mem_limit && <span title="内存硬上限">内存 {inst.mem_limit}</span>}
                  {inst.level && <span className="ic-level">{inst.level}</span>}
                </div>

                <div className="ic-ops">
                  {canOperate &&
                    (live === 'running' ? (
                      <>
                        <button onClick={() => doAction(inst.instance_id, 'stop')} disabled={busy}>停止</button>
                        <button onClick={() => doAction(inst.instance_id, 'restart')} disabled={busy}>重启</button>
                      </>
                    ) : (
                      <button className="primary" onClick={() => doAction(inst.instance_id, 'start')} disabled={busy}>启动</button>
                    ))}
                  <button
                    className="primary"
                    onClick={() => onOpen(inst.instance_id, inst.name, live, inst.level || 'viewer')}
                  >
                    打开
                  </button>
                  {user?.role === 'admin' && (
                    <button onClick={() => setAssignTarget(inst.instance_id)} title="分配访问权限">授权</button>
                  )}
                  {(user?.role === 'admin' || isNodeUser(user?.role)) && (
                    <button
                      className="danger-outline"
                      onClick={() => { setDeleteTarget(inst); setRemoveFiles(false) }}
                      disabled={busy}
                    >
                      删除
                    </button>
                  )}
                </div>
              </div>
            )
          })}
        </div>
      )}
    </div>
  )
}
