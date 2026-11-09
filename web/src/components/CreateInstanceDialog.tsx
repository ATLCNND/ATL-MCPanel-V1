import { useEffect, useMemo, useState } from 'react'
import {
  Instance, MyNode, NodeResource, JavaRuntime, MyPortLine, InstanceTemplate,
  listNodeResources, listNodeJava, createInstance, currentUser, isNodeUser,
  getNodeContainerCapability,
} from '../api'
// 字段级样式（.cf-*）与实例列表共用同一套定义，见 Instances.css；
// 本文件的两个 import 顺序无所谓，打包后同属一份 CSS。
import './Instances.css'
import './CreateInstanceDialog.css'

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

/**
 * 三种实例类型（与后端 instancekind.Kind 一一对应）。
 *
 * 这里只负责"怎么讲给用户听"：真正的行为差异在 daemon 与能力清单里，
 * 前端不允许按类型写 `if kind === 'docker'` 那种分支来决定页签 —— 那是
 * capabilitiesOf() 的活。
 */
const TEMPLATES: { key: InstanceTemplate; icon: string; name: string; desc: string; caps: string }[] = [
  {
    key: 'minecraft',
    icon: '⛏',
    name: 'Minecraft 服务器',
    desc: '用 Java 跑服务端核心，面板能看玩家、改配置、解析日志、备份世界。',
    caps: '控制台 · 文件 · 备份 · 玩家 · 世界 · RCON',
  },
  {
    key: 'docker',
    icon: '◲',
    name: '通用 Docker 容器',
    desc: '跑任意镜像。填镜像名与端口映射即可，面板负责拉起、看日志、停止。',
    caps: '控制台 · 文件 · 备份 · 镜像 · 端口 · 卷 · 环境变量',
  },
  {
    key: 'console',
    icon: '⌨',
    name: '通用控制台程序',
    desc: '不管它跑什么，面板只提供一个能收发 stdin/stdout 的终端，脚本自己放进实例目录。',
    caps: '控制台 · 文件 · 备份 · 日志',
  },
]

/**
 * 创建实例：**独立弹窗 + 分页**。
 *
 * 为什么从内联展开改成弹窗分页：这一张表单有十几个字段，每个字段还带一句常驻说明，
 * 内联展开时它会把实例列表整个顶下去、页面变成一条很长的滚动条，用户既看不全也
 * 找不到「我现在填到哪一步」。分页后每页只讲一件事（基本信息 / 网络与资源 /
 * 启动配置 / 公网端口），翻页时上一页的填值不会丢（状态都在本组件里）。
 *
 * 注意：本组件**自己持有表单状态**，父组件只负责给"有哪些节点/端口配额"和
 * 「创建成功后刷新列表」。这样翻页、关闭再打开都不会污染实例列表那一层。
 */
export default function CreateInstanceDialog({
  nodes, portLines, instances, onClose, onCreated,
}: {
  nodes: MyNode[]
  portLines: MyPortLine[]
  instances: Instance[]
  onClose: () => void
  onCreated: () => void | Promise<void>
}) {
  const user = currentUser()

  const [form, setForm] = useState({
    node_id: nodes.length > 0 ? String(nodes[0].id) : '',
    instance_id: '',
    name: '',
    // 实例类型：决定这台实例"是什么"，比"用哪个核心"更靠上。
    // 下面 core_type/java_version/jar_url 这些字段只对 minecraft 有意义。
    template: 'minecraft' as InstanceTemplate,
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
    // ---- 通用 Docker 类型的规格（template=docker 时才用）----
    //
    // 这些值最终会拼进 docker 命令行，因此每一项都由后端 instancekind.Spec
    // 再校验一遍（镜像名、端口范围、env 键名、卷路径不许越出实例目录）。
    image: '',
    dcmd: '',
    ctr_port: '80',
    denv: '',
    dmounts: '',
    // 建实例时即可选择容器化（仅管理员可见/可改）。
    //
    // 容器化现在是**默认方式**：勾选状态由所选节点的实测能力决定（见下面的
    // 节点能力查询），节点支持就默认勾上。用户仍可取消勾选 —— 排障或跑
    // 不接受容器路径语义（容器内实例目录是 /data）的包时会需要原生进程。
    container: true,
  })
  // 选中节点上的可用资源与 JDK（随节点切换而重新拉取）
  const [nodeRes, setNodeRes] = useState<NodeResource[]>([])
  const [nodeJava, setNodeJava] = useState<JavaRuntime[]>([])
  const [javaFallback, setJavaFallback] = useState('')
  const [loadingNode, setLoadingNode] = useState(false)
  // jar 选「自定义」时用手输
  const [customJar, setCustomJar] = useState(false)
  // 本次想开几个公网端口（按线路）
  const [portWant, setPortWant] = useState<Record<number, number>>({})
  // 所选节点的容器化能力：null = 还没查到（此时不谎报"支持"）。
  // 表单里只显示事实（docker 在不在、镜像在不在），不靠猜。
  const [ctrCap, setCtrCap] = useState<{ available: boolean; reason: string } | null>(null)
  const [busy, setBusy] = useState(false)
  const [error, setError] = useState('')
  const [page, setPage] = useState(0)

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

  // 节点具备容器化能力时，创建表单默认走容器化（容器化是默认方式）。
  //
  // 只有总管理员能查这个接口（要连节点，属运维信息），所以非管理员不请求 ——
  // 他们看不到开关，字段会整个不提交，由服务端按节点默认值决定。
  useEffect(() => {
    if (user?.role !== 'admin' || !form.node_id) { setCtrCap(null); return }
    let alive = true
    getNodeContainerCapability(Number(form.node_id))
      .then((c) => {
        if (!alive) return
        setCtrCap({ available: !!c.available, reason: c.reason || '' })
        // 查不到能力的节点（没装 docker / 没导镜像）不要默认勾上：
        // 勾了会在启动时才发现跑不起来。
        setForm((f) => ({ ...f, container: !!c.available }))
      })
      .catch(() => { if (alive) setCtrCap({ available: false, reason: '查询节点容器化能力失败' }) })
    return () => { alive = false }
  }, [form.node_id, user?.role])

  // Esc 关闭：弹窗遮住了整个列表，没有键盘退出会很难受。
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.key === 'Escape' && !busy) onClose()
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [busy, onClose])

  const set = (k: string) => (e: React.ChangeEvent<HTMLInputElement | HTMLSelectElement>) =>
    setForm({ ...form, [k]: e.target.value })

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

  // 分页：只有真的能开公网端口时才出现「公网端口」这一页。
  // 硬编码 4 页会让没有穿透线路的用户看到一张空页，反而更乱。
  const isMc = form.template === 'minecraft'
  const isDocker = form.template === 'docker'

  const pages = useMemo(() => {
    const t = form.template
    const list: { key: string; title: string; hint: string }[] = [
      { key: 'kind', title: '实例类型', hint: '这台实例要跑什么' },
      { key: 'basic', title: '基本信息', hint: '实例身份与它跑在哪台机器上' },
      { key: 'res', title: '网络与资源', hint: '端口、内存、CPU 与磁盘的边界' },
    ]
    // 三种类型在这一页分道扬镳：MC 讲 jar 与 Java，Docker 讲镜像与端口映射，
    // 控制台什么都不用配（它只提供 IO，脚本放进实例目录就行）。
    if (t === 'minecraft') {
      list.push({ key: 'start', title: '启动配置', hint: '用哪个 Java、哪个 jar、怎么启动' })
    } else if (t === 'docker') {
      list.push({ key: 'spec', title: '容器规格', hint: '镜像、命令、端口映射与环境变量' })
    } else {
      list.push({ key: 'script', title: '控制台脚本', hint: '往里兑换 stdin/stdout 的那个进程' })
    }
    if (portLines.length > 0) {
      list.push({ key: 'ports', title: '公网端口', hint: '要不要顺手把端口开到公网' })
    }
    return list
  }, [portLines.length, form.template])

  const cur = pages[Math.min(page, pages.length - 1)]

  // 当前页的必填门槛。分页最容易犯的错是「翻到最后才发现前面没填」，
  // 所以每页在「下一步」时就把这一页的硬约束拦住。
  const pageError = (k: string): string => {
    if (k === 'kind') {
      // 类型有默认值（minecraft），这一页永远不会有"没选"的错。
      //
      // 这里**刻意不校验镜像**：镜像字段在后面的「容器规格」页，若在这一页
      // 就拦住"请填写镜像"，用户会卡在第 1 页对着一个看不见的字段反复点下一步
      // （真机验收时就是这么卡住的）。缺什么就在它自己那一页说。
      return ''
    }
    if (k === 'spec') {
      if (isDocker && !form.image.trim()) return '请填写要运行的镜像（如 nginx:alpine）'
      if (isDocker) {
        const cp = Number(form.ctr_port)
        if (!form.ctr_port.trim()) return '请填写容器内端口'
        if (!Number.isInteger(cp) || cp < 1 || cp > 65535) return '容器内端口需为 1 ~ 65535 之间的整数'
      }
    }
    if (k === 'basic') {
      if (!form.node_id) return '请先选择所属节点'
      if (!form.instance_id.trim()) return '请填写实例 ID'
      if (idError) return idError
    }
    if (k === 'res') {
      if (!form.port.trim()) return isMc ? '请填写游戏端口' : '请填写端口'
      if (portError) return portError
      if (isMc && !form.max_mem.trim()) return '请填写最大内存'
    }
    return ''
  }

  const goto = (next: number) => {
    const e = pageError(cur.key)
    if (next > page && e) { setError(e); return }
    setError('')
    setPage(Math.max(0, Math.min(pages.length - 1, next)))
  }

  const submitCreate = async (e: React.FormEvent) => {
    e.preventDefault()
    if (idError || portError) {
      setError(idError || portError)
      return
    }
    // 提交前把**每一页**的硬约束再走一遍：可以点页签直接跳到最后一页，
    // 于是「第一阶段没填的字段」只有在提交时才暴露。此时不要报个看不懂的错，
    // 而是把用户送回缺字段的那一页。
    for (let i = 0; i < pages.length; i++) {
      const pe = pageError(pages[i].key)
      if (pe) {
        setPage(i)
        setError(pe)
        return
      }
    }
    setBusy(true)
    setError('')
    // 类型与规格：非 minecraft 的实例没有"核心/jar/Java"这套概念，
    // 提交时把 spec 一起带上；后端会再校验一遍（这里是即时反馈，那边才作数）。
    const template = form.template
    const spec = template === 'docker'
      ? JSON.stringify({
        image: form.image.trim(),
        // 命令按空格切开 —— 够用且可预期；需要带空格的参数请用单引号或引号包住整体
        cmd: form.dcmd.trim() ? form.dcmd.trim().split(/\s+/) : undefined,
        ports: [{ host: portNum, container: Number(form.ctr_port) || 80 }],
        env: form.denv.split('\n').map((l) => l.trim()).filter(Boolean),
        mounts: form.dmounts.split('\n').map((l) => l.trim()).filter(Boolean).map((l) => {
          const [host, target] = l.split(':')
          return { host, target }
        }),
      })
      : ''
    try {
      await createInstance({
        node_id: Number(form.node_id),
        instance_id: form.instance_id.trim(),
        name: form.name.trim() || form.instance_id.trim(),
        template,
        spec,
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
        // 容器化：**容器化是默认方式**。
        //
        // 管理员提交他自己的勾选结果（勾选框的初始值来自节点实测能力）；
        // 非管理员不提交这个字段 —— 服务端按节点默认值决定（支持则容器化），
        // 这样"默认容器化"对所有用户一致，而开关仍只掌握在管理员手里。
        //
        // 这个字段曾经**没有被后端转发**，勾了也白勾（实例跑在容器里而元数据
        // 是 false），所以两边都得钉住。
        ...(user?.role === 'admin' ? { container: form.container } : {}),
        // 按线路申请的公网端口数；数量为 0 的线路不提交
        tunnels: Object.entries(portWant)
          .map(([fid, count]) => ({ frps_id: Number(fid), count }))
          .filter((t) => t.count > 0),
      })
      await onCreated()
      onClose()
    } catch (e: any) {
      setError(e.message)
    } finally {
      setBusy(false)
    }
  }

  const isLast = page >= pages.length - 1

  return (
    <div
      className="modal-mask"
      onClick={() => { if (!busy) onClose() }}
    >
      <div
        className="panel ci-dialog"
        onClick={(e) => e.stopPropagation()}
        role="dialog"
        aria-label="创建实例"
      >
        <div className="modal-header">
          <div className="ci-title">
            创建实例
            <span className="ci-step">第 {page + 1} / {pages.length} 页 · {cur.title}</span>
          </div>
          <button className="ci-close" onClick={() => !busy && onClose()} disabled={busy} title="关闭">×</button>
        </div>

        {/* 页签：允许直接跳页（已填过的页可以回头看），但跳「往后」仍走同一套校验 */}
        <div className="ci-tabs">
          {pages.map((p, i) => (
            <button
              key={p.key}
              type="button"
              className={`ci-tab${i === page ? ' active' : ''}${i < page ? ' done' : ''}`}
              onClick={() => goto(i)}
            >
              {/* 已填过的页打勾而不是留数字：四步里"当前页"和"已过页"如果只有色相
                  之差（粉 vs 绿），缩小或者色弱就分不出来了。 */}
              <span className="ci-tab-no">{i < page ? '✓' : i + 1}</span>
              <span className="ci-tab-name">{p.title}</span>
            </button>
          ))}
        </div>

        <form className="create-form ci-form" onSubmit={submitCreate}>
          <div className="cf-head-note ci-pagehint">
            {cur.hint}。带 <i className="cf-req">*</i> 的为必填；创建后 <b>实例 ID 与所属节点不可更改</b>，
            其余参数都能在实例页里随时调整。
          </div>

          {error && <div className="error-banner">{error}</div>}

          {/* ---------------- 第 1 页：实例类型 ---------------- */}
          {cur.key === 'kind' && (
            <div className="ci-kinds">
              {TEMPLATES.map((t) => {
                const on = form.template === t.key
                return (
                  <button
                    key={t.key}
                    type="button"
                    className={`ci-kind${on ? ' active' : ''}`}
                    aria-pressed={on}
                    onClick={() => {
                      // 换类型时把"另一类才有"的字段降回默认值：
                      // 带着 docker 镜像名去建 MC 实例没有任何意义，
                      // 后端也会因为 spec 里有镜像而拒绝（见 handleCreateInstance）。
                      setForm({
                        ...form,
                        template: t.key,
                        port: t.key === 'minecraft' ? (form.port || '25565') : form.port,
                        max_mem: form.max_mem || '3G',
                        min_mem: form.min_mem || '1G',
                      })
                    }}
                  >
                    <span className="ci-kind-icon" aria-hidden>{t.icon}</span>
                    <b className="ci-kind-name">{t.name}</b>
                    <span className="ci-kind-desc">{t.desc}</span>
                    <span className="ci-kind-caps">{t.caps}</span>
                  </button>
                )
              })}
            </div>
          )}

          {/* ---------------- 第 2 页：基本信息 ---------------- */}
          {cur.key === 'basic' && (
            <div className="cf-grid">
              <Field
                label="实例 ID"
                required
                hint={idError
                  ? <span className="cf-bad">{idError}</span>
                  : <>唯一标识，会作为实例目录名（节点的实例目录下 <code>&lt;实例ID&gt;/</code>）。<b>创建后不可修改</b>，只用小写字母、数字、<code>-</code>、<code>_</code>。</>}
              >
                <input value={form.instance_id} onChange={set('instance_id')} placeholder="survival-01" spellCheck={false} />
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
                <select value={form.node_id} onChange={set('node_id')}>
                  <option value="">— 选择节点 —</option>
                  {nodes.map((n) => (
                    <option key={n.id} value={n.id}>
                      {n.name}{n.ip ? `（${n.ip}）` : ''}{!n.online ? ' · 离线' : ''} · 已有 {n.instances} 个实例
                    </option>
                  ))}
                </select>
              </Field>

              {/* 核心只是 MC 的分类标签；Docker/控制台实例没有"服务端核心"。 */}
              {isMc && (
                <Field
                  label="服务端核心"
                  hint={<>只是<b>分类标签</b>，用于区分与筛选 —— 真正决定跑什么的是后面的 jar 与启动命令。</>}
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
              )}
            </div>
          )}

          {/* ---------------- 第 2 页：网络与资源 ---------------- */}
          {cur.key === 'res' && (
            <div className="cf-grid">
              <Field
                label={isMc ? '游戏端口' : isDocker ? '宿主端口' : '端口'}
                required
                hint={portError
                  ? <span className="cf-bad">{portError}</span>
                  : isMc
                    ? <>服务端监听的 TCP 端口。<code>server.properties</code> 里的 <code>server-port</code> 要与它一致，否则玩家连不上。</>
                    : isDocker
                      ? <>这个端口开在<b>节点主机</b>上，容器内的端口在下一页填。隧道与端口冲突检查都以它为准。</>
                      : <>脚本自己监听哪个端口就填哪个（面板只用它做记录、隧道与冲突检查）。</>}
              >
                <input value={form.port} onChange={set('port')} placeholder="25565" inputMode="numeric" />
              </Field>

              {isMc && (
                <Field
                  label="最大内存（-Xmx）"
                  required
                  hint={<>JVM <b>堆</b>的上限，填 <code>3G</code> / <code>2048M</code> 这类带单位的值。<br />
                    注意堆 ≠ 整个进程：元空间、线程栈、直接内存在堆外另算，别把它当成进程总内存。</>}
                >
                  <input value={form.max_mem} onChange={set('max_mem')} placeholder="3G" />
                </Field>
              )}

              {isMc && (
                <Field
                  label="最小内存（-Xms）"
                  hint={<>JVM 启动时一次性申请的堆大小。与 <code>-Xmx</code> 填相同值可避免运行中反复扩容（略占内存但更稳）。</>}
                >
                  <input value={form.min_mem} onChange={set('min_mem')} placeholder="1G" />
                </Field>
              )}

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

              {/* 容器化：仅总管理员可见（可改）。
                  与「启动」页那个开关是同一件事，只是建实例时就能选，省得建完再回去开。
                  **默认勾上**（节点实测支持才勾），因为容器化已经是默认运行方式：
                  独立网络、只读根文件系统、丢弃全部权限。取消勾选即回到原生进程。 */}
              {user?.role === 'admin' && (
                <div className="cf-field cf-span2">
                  <label className="cf-check">
                    <input
                      type="checkbox"
                      checked={form.container}
                      disabled={ctrCap !== null && !ctrCap.available}
                      onChange={(e) => setForm({ ...form, container: e.target.checked })}
                    />
                    以容器方式运行（容器化隔离，推荐）
                  </label>
                  <div className="cf-hint">
                    开启后实例跑在独立容器里：独立网络（连不到节点上的其它服务与别的实例）、
                    只读根文件系统、丢弃全部 Linux 权限，端口只发布到本机回环。
                    <br />
                    容器内看到的实例目录是 <code>/data</code>，启动脚本里请用该路径。
                    建好之后也能在「启动」页随时开关（仅管理员）。
                    <br />
                    {ctrCap === null
                      ? <>正在检查该节点的容器化能力…</>
                      : ctrCap.available
                        ? <><b>该节点支持容器化</b>，已默认勾选。</>
                        : <><b>该节点暂不支持容器化</b>（{ctrCap.reason || '未安装 docker 或未导入基础镜像'}），
                            此次将以原生进程创建。</>}
                  </div>
                </div>
              )}
            </div>
          )}

          {/* ---------------- Docker：容器规格 ---------------- */}
          {cur.key === 'spec' && (
            <div className="cf-grid">
              <Field
                label="镜像"
                required
                span={2}
                hint={<>写完整的镜像引用，如 <code>nginx:alpine</code>、<code>ghcr.io/owner/app:1.2</code>。<br />
                  节点上必须<b>已存在</b>这个镜像（面板不会替你拉 —— 拉取要访问外网，属于运维决定）。</>}
              >
                <input value={form.image} onChange={set('image')} placeholder="nginx:alpine" spellCheck={false} />
              </Field>

              <Field
                label="容器内端口"
                required
                hint={<>容器里服务实际监听的端口。上面填的宿主端口会映射到它（<code>-p 宿主:容器</code>）。<br />
                  <b>不填就用镜像默认</b>是猜的 —— 这里必须写实际值。</>}
              >
                <input value={form.ctr_port} onChange={set('ctr_port')} placeholder="80" inputMode="numeric" />
              </Field>

              <Field
                label="启动命令"
                hint={<>留空则用镜像自带的 <code>ENTRYPOINT</code>/<code>CMD</code>。填了会覆盖镜像默认命令，按空格切分。<br />
                  <b>引擎不跑 shell</b>，所以这里写不了管道与重定向 —— 要那类玩法请把脚本放进实例目录并在命令里调它。</>}
              >
                <input value={form.dcmd} onChange={set('dcmd')} placeholder="留空 = 镜像默认" spellCheck={false} />
              </Field>

              <Field
                label="环境变量"
                span={2}
                hint={<>每行一条 <code>KEY=VALUE</code>。键只允许字母/数字/下划线且不以数字开头。</>}
              >
                <textarea rows={4} value={form.denv} onChange={(e) => setForm({ ...form, denv: e.target.value })} placeholder={'TZ=Asia/Shanghai\nMODE=production'} spellCheck={false} />
              </Field>

              <Field
                label="卷挂载"
                span={2}
                hint={<>每行一条 <code>实例内路径:容器内路径</code>，前者是<b>相对实例目录</b>的子路径（<code>.</code> 代表实例目录本身）。<br />
                  例：<code>data:/var/lib/data</code>。绝对路径与 <code>..</code> 会被拒绝 —— 容器不该能碰到实例目录以外的东西。</>}
              >
                <textarea rows={3} value={form.dmounts} onChange={(e) => setForm({ ...form, dmounts: e.target.value })} placeholder={'data:/var/lib/data'} spellCheck={false} />
              </Field>
            </div>
          )}

          {/* ---------------- 控制台：只需要说明它是什么 ---------------- */}
          {cur.key === 'script' && (
            <div className="ci-note">
              <p>
                控制台实例<b>不启动任何东西</b>，只提供一个能收发 stdin/stdout 的终端。
                面板不会替你猜该怎么跑 —— 进程怎么起由你决定：
              </p>
              <ul>
                <li>在实例目录放一个可执行的 <code>start.sh</code>，面板启动实例时执行它；</li>
                <li>或者不放假本时，面板给你一个交互式 <code>sh</code>，你在控制台里手动敲命令（适合临时跑点什么）；</li>
                <li>程序往 stdout 写的东西会进「控制台」页，控制台里输入的内容会进它的 stdin。</li>
              </ul>
              <p className="ci-note-dim">
                文件、备份、日志三页在这三种类型下的行为完全一致 —— 它们只跟"实例目录"有关，与跑什么无关。
              </p>
            </div>
          )}

          {/* ---------------- 第 3 页：启动配置 ---------------- */}
          {cur.key === 'start' && (
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
          )}

          {/* ---------------- 第 4 页：公网端口（仅在有线路时出现） ---------------- */}
          {cur.key === 'ports' && (
            <div>
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
                            普通用户看到的是"我们服务器的 IP"，属于不该暴露的运维信息。
                            地址与线路名相同时（自建线路常用域名当名字）不再重复一遍。 */}
                        {l.frps_host && l.frps_host !== l.frps_name && <span className="mono muted">{l.frps_host}</span>}
                      </div>
                      <div className="cf-portline-quota">
                        {l.unlimited
                          ? '配额不限'
                          : <>剩余 <b>{l.available}</b> 个<span className="cf-portline-used">（配额 {l.quota} · 已用 {l.used}）</span></>}
                      </div>
                      <div className="cf-portline-ctl" title={`开通 ${want} 个公网端口`}>
                        <button
                          type="button"
                          aria-label="减少一个端口"
                          disabled={want <= 0}
                          onClick={() => setPortWant({ ...portWant, [l.frps_id]: Math.max(0, want - 1) })}
                        >−</button>
                        <span className="cf-portline-num">{want}</span>
                        <button
                          type="button"
                          aria-label="增加一个端口"
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

          <div className="ci-foot">
            <button
              type="button"
              onClick={() => goto(page - 1)}
              disabled={page === 0 || busy}
            >上一步</button>
            <div className="ci-foot-nav">
              <button type="button" onClick={() => !busy && onClose()} disabled={busy}>取消</button>
              {!isLast && (
                <button type="button" className="primary" onClick={() => goto(page + 1)} disabled={busy}>下一步</button>
              )}
              {isLast && (
                <button
                  className="primary"
                  type="submit"
                  disabled={busy || nodes.length === 0 || !!idError || !!portError}
                >
                  {busy ? '创建中…' : '创建'}
                </button>
              )}
            </div>
          </div>
        </form>
      </div>
    </div>
  )
}
