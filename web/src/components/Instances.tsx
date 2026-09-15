import { useEffect, useState, useCallback } from 'react'
import {
  Instance, MyNode, NodeResource, JavaRuntime, MyPortLine,
  listInstances, listMyNodes, listNodeResources, listNodeJava, listMyPorts,
  createInstance, instanceAction, currentUser, levelAtLeast, roleLabel, isNodeUser,
  instanceIconUrl,
} from '../api'
import AssignmentsModal from './AssignmentsModal'
import AlertsModal from './AlertsModal'
import Avatar from './Avatar'
import './Instances.css'

/**
 * 带标签与说明的表单字段。
 *
 * 为什么要统一成组件：创建实例有十几个参数，其中好几个是**看着像但含义不同**的
 * 一对（-Xmx 与 cgroup memory.max、CPU 核数与线程数、实例 ID 与显示名称）。
 * 只靠 placeholder 根本说不清，而 tooltip 在触屏上根本看不到 ——
 * 所以每个字段都配一句常驻说明，把"这是什么 / 怎么填 / 有什么坑"写清楚。
 */
function Field({
  label, hint, required, span, children,
}: {
  label: string
  hint?: React.ReactNode
  required?: boolean
  span?: 2
  children: React.ReactNode
}) {
  return (
    <div className={`cf-field${span === 2 ? ' cf-span2' : ''}`}>
      <label className="cf-label">
        {label}
        {required && <i className="cf-req">*</i>}
      </label>
      {children}
      {hint && <div className="cf-hint">{hint}</div>}
    </div>
  )
}

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

  // 创建表单
  const [nodes, setNodes] = useState<MyNode[]>([])
  // 选中节点上的可用资源与 JDK（随节点切换而重新拉取）
  const [nodeRes, setNodeRes] = useState<NodeResource[]>([])
  const [nodeJava, setNodeJava] = useState<JavaRuntime[]>([])
  const [javaFallback, setJavaFallback] = useState('')
  const [loadingNode, setLoadingNode] = useState(false)
  // jar 选「自定义」时用手输
  const [customJar, setCustomJar] = useState(false)
  // 我的穿透端口配额（按线路），以及本次想开几个
  const [portLines, setPortLines] = useState<MyPortLine[]>([])
  const [portWant, setPortWant] = useState<Record<number, number>>({})
  const [form, setForm] = useState({
    node_id: '',
    instance_id: '',
    name: '',
    core_type: 'paper',
    java_version: '',
    port: '25565',
    max_mem: '3G',
    min_mem: '1G',
    jar_url: '',
    start_command: '',
    cpu_cores: '0',
    mem_limit: '',
    disk_limit_mb: '',
    disk_autostop: false,
  })

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
      .then((list) => {
        const arr = Array.isArray(list) ? list : []
        setNodes(arr)
        setForm((f) => (f.node_id === '' && arr.length > 0 ? { ...f, node_id: String(arr[0].id) } : f))
      })
      .catch((e) => setError(e.message))
  }, [])

  // 切换节点后重新拉取该节点上的共享资源与 JDK —— 这两样都是"节点本地"的，
  // 换节点后旧列表一律失效（jar 路径是绝对路径，换了机器就不存在了）
  useEffect(() => {
    if (!form.node_id) { setNodeRes([]); setNodeJava([]); setJavaFallback(''); return }
    const nid = Number(form.node_id)
    setLoadingNode(true)
    let alive = true
    Promise.all([
      listNodeResources(nid).catch(() => ({ dir: '', files: [], available: false })),
      listNodeJava(nid).catch(() => ({ runtimes: [], fallback: '' })),
    ]).then(([r, j]) => {
      if (!alive) return
      setNodeRes(Array.isArray(r.files) ? r.files : [])
      setNodeJava(Array.isArray(j.runtimes) ? j.runtimes : [])
      setJavaFallback(j.fallback || '')
      // 默认选版本最高的那个 JDK；jar 默认选第一个共享资源
      setForm((f) => ({
        ...f,
        java_version: f.java_version || (j.runtimes?.[0]?.label ?? ''),
        jar_url: f.jar_url || (r.files?.[0]?.path ?? ''),
      }))
    }).finally(() => { if (alive) setLoadingNode(false) })
    return () => { alive = false }
  }, [form.node_id])

  // 告警计数已由 App 层统一轮询（显示在侧边栏），此处不再重复请求

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

  // 实例 ID 的本地校验：与后端同一套规则（后端才是权威，这里只是即时反馈）。
  // 规则来自它的实际用途 —— 它会成为一个目录名，所以必须排除路径分隔符与 .. 。
  const idError = (() => {
    const v = form.instance_id.trim()
    if (!v) return ''
    if (!/^[a-z0-9][a-z0-9_-]*$/.test(v)) return '只能用小写字母、数字、连字符 - 与下划线 _，且必须以字母或数字开头'
    if (v.length > 32) return '最长 32 个字符'
    if (instances.some((i) => i.instance_id === v)) return '该 ID 已被占用'
    return ''
  })()

  const portNum = Number(form.port)
  const portError =
    !form.port.trim() ? ''
      : !Number.isInteger(portNum) || portNum < 1 || portNum > 65535 ? '端口需为 1 ~ 65535 之间的整数'
        : instances.some((i) => String(i.node_id) === form.node_id && i.port === portNum)
          ? '同一节点上已有实例占用该端口'
          : ''

  const submitCreate = async (e: React.FormEvent) => {
    e.preventDefault()
    if (idError || portError) {
      setError(idError || portError)
      return
    }
    setBusy(true)
    setError('')
    try {
      await createInstance({
        node_id: Number(form.node_id),
        instance_id: form.instance_id.trim(),
        name: form.name.trim() || form.instance_id.trim(),
        core_type: form.core_type,
        java_version: form.java_version,
        port: portNum,
        max_mem: form.max_mem,
        min_mem: form.min_mem,
        jar_url: form.jar_url,
        start_command: form.start_command,
        // 界面按"核数"填写，后端按百分比存储（1 核 = 100%）
        cpu_quota: Math.round((Number(form.cpu_cores) || 0) * 100),
        mem_limit: form.mem_limit.trim(),
        disk_limit_mb: Number(form.disk_limit_mb) || 0,
        disk_autostop: form.disk_autostop,
        // 按线路申请的公网端口数；数量为 0 的线路不提交
        tunnels: Object.entries(portWant)
          .map(([fid, count]) => ({ frps_id: Number(fid), count }))
          .filter((t) => t.count > 0),
      })
      setShowCreate(false)
      await refresh()
    } catch (e: any) {
      setError(e.message)
    } finally {
      setBusy(false)
    }
  }

  const set = (k: string) => (e: React.ChangeEvent<HTMLInputElement | HTMLSelectElement>) =>
    setForm({ ...form, [k]: e.target.value })

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
          <button className="primary" onClick={() => setShowCreate(!showCreate)}>
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
        <form className="create-form" onSubmit={submitCreate}>
          <div className="cf-head">
            <h3>创建实例</h3>
            <div className="cf-head-note">
              带 <i className="cf-req">*</i> 的为必填。创建后 <b>实例 ID 与所属节点不可更改</b>，
              其余参数都能在实例页里随时调整。
            </div>
          </div>

          {/* ---------------- 基本信息 ---------------- */}
          <div className="cf-section">
            <div className="cf-section-title">基本信息</div>
            <div className="cf-grid">
              <Field
                label="实例 ID"
                required
                hint={idError
                  ? <span className="cf-bad">{idError}</span>
                  : <>唯一标识，会作为实例目录名（节点的实例目录下 <code>&lt;实例ID&gt;/</code>）。<b>创建后不可修改</b>，只用小写字母、数字、<code>-</code>、<code>_</code>。</>}
              >
                <input value={form.instance_id} onChange={set('instance_id')} placeholder="survival-01" required spellCheck={false} />
              </Field>

              <Field label="显示名称" hint="界面上展示的名字，随时可改，不影响运行。留空则用实例 ID。">
                <input value={form.name} onChange={set('name')} placeholder="生存服一号" />
              </Field>

              <Field
                label="所属节点"
                required
                hint={<>实例跑在哪台机器上。<b>创建后不能迁移</b>，换节点只能重建。<br />
                  这里只列出<strong>你有权创建实例</strong>的节点{isNodeUser(user?.role) && '（由总管理员分配）'}。</>}
              >
                <select value={form.node_id} onChange={set('node_id')} required>
                  <option value="">— 选择节点 —</option>
                  {nodes.map((n) => (
                    <option key={n.id} value={n.id}>
                      {n.name}{n.ip ? `（${n.ip}）` : ''}{n.status !== 'online' ? ' · 离线' : ''} · 已有 {n.instances} 个实例
                    </option>
                  ))}
                </select>
              </Field>

              <Field
                label="服务端核心"
                hint={<>只是<b>分类标签</b>，用于区分与筛选 —— 真正决定跑什么的是下面的 jar 与启动命令。</>}
              >
                <select value={form.core_type} onChange={set('core_type')}>
                  <option value="paper">Paper</option>
                  <option value="folia">Folia</option>
                  <option value="purpur">Purpur</option>
                  <option value="bungeecord">BungeeCord</option>
                  <option value="velocity">Velocity</option>
                  <option value="forge">Forge</option>
                  <option value="fabric">Fabric</option>
                  <option value="custom">自定义</option>
                </select>
              </Field>
            </div>
          </div>

          {/* ---------------- 网络与资源 ---------------- */}
          <div className="cf-section">
            <div className="cf-section-title">网络与资源</div>
            <div className="cf-grid">
              <Field
                label="游戏端口"
                required
                hint={portError
                  ? <span className="cf-bad">{portError}</span>
                  : <>服务端监听的 TCP 端口。<code>server.properties</code> 里的 <code>server-port</code> 要与它一致，否则玩家连不上。</>}
              >
                <input value={form.port} onChange={set('port')} placeholder="25565" inputMode="numeric" />
              </Field>

              <Field
                label="最大内存（-Xmx）"
                required
                hint={<>JVM <b>堆</b>的上限，填 <code>3G</code> / <code>2048M</code> 这类带单位的值。<br />
                  注意堆 ≠ 整个进程：元空间、线程栈、直接内存在堆外另算，别把它当成进程总内存。</>}
              >
                <input value={form.max_mem} onChange={set('max_mem')} placeholder="3G" />
              </Field>

              <Field
                label="最小内存（-Xms）"
                hint={<>JVM 启动时一次性申请的堆大小。与 <code>-Xmx</code> 填相同值可避免运行中反复扩容（略占内存但更稳）。</>}
              >
                <input value={form.min_mem} onChange={set('min_mem')} placeholder="1G" />
              </Field>

              <Field
                label="CPU 核数"
                hint={<>通过 cgroup v2 <code>cpu.max</code> 施加的<b>硬上限</b>，单位是核，可填 <code>1.5</code>。<br />
                  <b>0 = 不限制</b>。它限的是 CPU <b>时间</b>，不是线程数 —— 设 2 核不代表进程只能用 2 个线程。</>}
              >
                <input value={form.cpu_cores} onChange={set('cpu_cores')} placeholder="0" inputMode="decimal" />
              </Field>

              <Field
                label="内存硬上限"
                hint={<>cgroup <code>memory.max</code>，<b>留空 = 不限制</b>。与上面 <code>-Xmx</code> 的区别：这是<b>整个进程</b>的天花板，
                  超了会被内核 OOM 杀掉（但不会连累同节点其它实例）。经验值：<code>-Xmx × 1.3 + 512MB</code>。</>}
              >
                <input value={form.mem_limit} onChange={set('mem_limit')} placeholder="4G（留空不限制）" />
              </Field>

              <Field
                label="磁盘配额（MB）"
                hint={<>实例目录的<b>软配额</b>，<b>0 = 不限制</b>。面板每分钟巡检：到 85% 报「警告」、95% 报「严重」。<br />
                  它不阻止瞬时写入（ext4 没有目录级配额），但能在写满整盘之前介入。</>}
              >
                <input value={form.disk_limit_mb} onChange={set('disk_limit_mb')} placeholder="0" inputMode="numeric" />
              </Field>

              <div className="cf-field cf-span2">
                <label className="cf-check">
                  <input
                    type="checkbox"
                    checked={form.disk_autostop}
                    onChange={(e) => setForm({ ...form, disk_autostop: e.target.checked })}
                  />
                  磁盘超过配额时自动停止实例
                </label>
                <div className="cf-hint">
                  <b>默认关闭</b>。停机有破坏性：玩家会被踢下线，未落盘的进度可能丢失。
                  只在「宁可不跑也不能写满整盘」时才开。
                </div>
              </div>
            </div>
          </div>

          {/* ---------------- 启动配置 ---------------- */}
          <div className="cf-section">
            <div className="cf-section-title">启动配置</div>
            <div className="cf-grid">
              <Field
                label="Java 版本"
                hint={<>选项来自 Daemon 对<b>该节点上实际安装的 JDK</b> 的探测，<b>选中哪个启动就用哪个</b>（不再只是记录）。<br />
                  {loadingNode ? '正在读取节点环境…'
                    : nodeJava.length === 0
                      ? <>节点上未探测到 JDK，将回退到 PATH 上的 <code>java</code>{javaFallback ? `（${javaFallback}）` : ''}。</>
                      : <>共 {nodeJava.length} 个可用；选「自动」则用 PATH 上的 <code>java</code>{javaFallback ? `（${javaFallback}）` : ''}。</>}
                </>}
              >
                <select value={form.java_version} onChange={set('java_version')}>
                  <option value="">自动（PATH 上的 java）</option>
                  {nodeJava.map((r) => (
                    <option key={r.path} value={r.label}>
                      Java {r.label}{r.version ? `（${r.version}）` : ''}
                    </option>
                  ))}
                </select>
              </Field>

              <Field
                label="核心 jar"
                hint={<>优先从该节点已上传的<b>共享资源</b>里选 —— 同一台机器上多个实例共用一份 jar，不必各存一遍。<br />
                  选「自定义」可手填任意路径；文件是否存在<b>要等启动时才会检查</b>。</>}
              >
                <div className="cf-jar">
                  <select
                    value={customJar ? '__custom__' : form.jar_url}
                    onChange={(e) => {
                      if (e.target.value === '__custom__') { setCustomJar(true); return }
                      setCustomJar(false)
                      setForm({ ...form, jar_url: e.target.value })
                    }}
                  >
                    <option value="">（先留空，创建后在「核心」页设置）</option>
                    {nodeRes.map((r) => (
                      <option key={r.path} value={r.path}>
                        {r.name}（{(r.size / 1024 / 1024).toFixed(1)} MB{r.referenced ? ` · ${r.refs.length} 个实例在用` : ''}）
                      </option>
                    ))}
                    <option value="__custom__">自定义路径…</option>
                  </select>
                  {(customJar || (!!form.jar_url && !nodeRes.some((r) => r.path === form.jar_url))) && (
                    <input
                      value={form.jar_url}
                      onChange={set('jar_url')}
                      placeholder="节点上的绝对路径，如 /opt/mcpanel/resources/paper.jar"
                      spellCheck={false}
                    />
                  )}
                </div>
              </Field>

              <Field
                label="自定义启动命令"
                span={2}
                hint={<>留空则按优先级自动选择：实例目录下的 <code>start.sh</code> → 本模板 → 默认
                  <code> java -Xms&#123;min_mem&#125; -Xmx&#123;max_mem&#125; -jar &#123;jar&#125; nogui</code>。<br />
                  可用占位符：<code>&#123;jar&#125;</code> <code>&#123;max_mem&#125;</code> <code>&#123;min_mem&#125;</code> <code>&#123;java&#125;</code> <code>&#123;dir&#125;</code>；
                  命令通过 <code>sh -c</code> 执行，因此也能写管道、环境变量等。</>}
              >
                <input value={form.start_command} onChange={set('start_command')} placeholder="留空 = 自动选择（推荐）" spellCheck={false} />
              </Field>
            </div>
          </div>

          {/* ---------------- 公网端口 ---------------- */}
          {portLines.length > 0 && (
            <div className="cf-section">
              <div className="cf-section-title">公网端口</div>
              <div className="cf-hint" style={{ marginBottom: 10 }}>
                每开一个端口，实例就能多暴露一个对外服务 ——
                多端口模组/插件（BlueMap 网页、Geyser 基岩版、Votifier 等）需要这个。
                默认的本地端口是实例游戏端口，创建后可在实例页的「公网端口」里改成模组实际监听的端口。
                {portLines.some((l) => l.unlimited) && '（你作为总管理员不受配额限制）'}
              </div>
              <div className="cf-portlist">
                {portLines.map((l) => {
                  const want = portWant[l.frps_id] || 0
                  const max = l.unlimited ? 20 : l.available
                  return (
                    <div className={`cf-portline ${max === 0 && !l.unlimited ? 'none' : ''}`} key={l.frps_id}>
                      <div className="cf-portline-name">
                        <b>{l.frps_name}</b>
                        {/* 线路的公网地址只对总管理员显示（后端也只给管理员返回）。
                            普通用户看到的是"我们服务器的 IP"，属于不该暴露的运维信息。 */}
                        {l.frps_host && <span className="mono muted">{l.frps_host}</span>}
                      </div>
                      <div className="cf-portline-quota">
                        {l.unlimited
                          ? '不限'
                          : `剩余 ${l.available} 个（配额 ${l.quota}，已用 ${l.used}）`}
                      </div>
                      <div className="cf-portline-ctl">
                        <button
                          type="button"
                          disabled={want <= 0}
                          onClick={() => setPortWant({ ...portWant, [l.frps_id]: Math.max(0, want - 1) })}
                        >−</button>
                        <span className="cf-portline-num">{want}</span>
                        <button
                          type="button"
                          disabled={!l.unlimited && want >= max}
                          onClick={() => setPortWant({ ...portWant, [l.frps_id]: want + 1 })}
                        >+</button>
                      </div>
                    </div>
                  )
                })}
              </div>
              {Object.values(portWant).some((n) => n > 0) && (
                <div className="cf-hint" style={{ marginTop: 8 }}>
                  本次将开通 <b>{Object.values(portWant).reduce((a, b) => a + b, 0)}</b> 个公网端口。
                  若线路端口段已被占满，创建仍会成功，但会在结果里提示哪几个没开成。
                </div>
              )}
            </div>
          )}

          {nodes.length === 0 && (
            <div className="form-hint">
              尚未登记任何节点，请先到「节点管理」添加并部署 Daemon。
            </div>
          )}

          <div className="form-actions">
            <button className="primary" type="submit" disabled={busy || nodes.length === 0 || !!idError || !!portError}>
              {busy ? '创建中…' : '创建'}
            </button>
            <button type="button" onClick={() => setShowCreate(false)}>取消</button>
          </div>
        </form>
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