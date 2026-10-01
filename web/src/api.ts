export interface Instance {
  id: number
  instance_id: string
  node_id: number
  name: string
  core_type: string
  port: number
  max_mem: string
  status: string
  level?: string // 当前用户对该实例的权限级别：owner/collab/viewer
  live_status?: string // Daemon 上报的实时状态（可能领先于数据库中的 status）
  /** 实例图标修改时间（Unix 秒，0/未定义 = 没有图标）；见 instanceIconUrl */
  icon_mtime?: number
  cpu_quota?: number // CPU 配额百分比（100 = 1 核；0 = 不限制）
  mem_limit?: string // cgroup 内存上限（空 = 不限制）
  disk_limit_mb?: number // 磁盘软配额（MB，0 = 不限制）
  disk_autostop?: boolean // 超限自动停机
  /** 创建时按线路申请的穿透端口数 */
  tunnels?: { frps_id: number; count: number }[]
  // 到期控制
  expires_at?: string          // RFC3339；空 = 永不过期
  expiry_notice_days?: number  // 提前多少天告警
  expiry_autostop?: boolean    // 到期是否自动停止
  expiry_state?: 'none' | 'active' | 'soon' | 'expired'
  expiry_days_left?: number
}

// 权限级别判定
export function levelAtLeast(level: string | undefined, need: 'viewer' | 'collab' | 'owner'): boolean {
  const rank = (l?: string) => (l === 'owner' ? 3 : l === 'collab' ? 2 : l === 'viewer' ? 1 : 0)
  return rank(level) >= rank(need)
}

export interface FileItem {
  name: string
  path: string
  is_dir: boolean
  size: number
  mod_time: number
}

export interface Metrics {
  cpu_percent: number
  mem_used: number
  tps: number
  players: number
  players_max: number
  /** 进程当前线程数（JVM 的 GC / Netty / 区块 IO 等线程总和） */
  threads?: number
}

const TOKEN_KEY = 'atlmcpanel_token'
const USER_KEY = 'atlmcpanel_user'

export function getToken(): string | null {
  return localStorage.getItem(TOKEN_KEY)
}

export function setToken(token: string) {
  localStorage.setItem(TOKEN_KEY, token)
}

export function clearToken() {
  localStorage.removeItem(TOKEN_KEY)
  // 登录态里的用户信息一起清掉：留着它会让"已经登出"的界面上仍显示上一个用户的名字
  localStorage.removeItem(USER_KEY)
}

// ---- 会话失效（401）的全局处理 ----
//
// 为什么必须有这一层：面板重启换了 JWT 密钥、令牌到期（默认 24 小时）、
// 或者服务端数据目录被重建，都会让**已发出的令牌失效**。此前这种情况下的表现是：
// 每个页面各自弹一句"令牌无效或已过期"，用户被留在主界面上、点了也没用，
// 只能自己去猜"是不是要重新登录"。
//
// 现在统一处理：任何请求收到 401（登录/改密码这两个接口除外）就清掉本地令牌、
// 广播一个事件，由 App 把界面切回登录页并说明原因。
export const AUTH_EXPIRED_EVENT = 'atlmcpanel:auth-expired'

/** 一次会话里只提示一次，避免并发请求各弹一遍 */
let authExpiredNotified = false

export function notifyAuthExpired(reason: string) {
  if (authExpiredNotified) return
  authExpiredNotified = true
  clearToken()
  window.dispatchEvent(new CustomEvent(AUTH_EXPIRED_EVENT, { detail: { reason } }))
}

/** 登录成功后重置（否则第二次过期不会再提示） */
export function resetAuthExpiredNotice() {
  authExpiredNotified = false
}

/**
 * 这两个接口的 401 与"会话失效"无关，是**凭据本身错**：
 *  · 登录：用户名或密码错误；
 *  · 改密码：旧密码错误。
 * 若不加区分，输错密码会顺手把界面切走并提示"登录已过期" —— 用户会以为自己被踢了。
 */
function isCredentialEndpoint(path: string): boolean {
  return path.startsWith('/api/auth/login') || path.startsWith('/api/auth/change-password')
}

/**
 * 本地判断令牌是否已过期（只解 JWT 的 exp，不验签 —— 验签是服务端的事）。
 *
 * 为什么要本地判：用户把页面开着不动时没有任何请求，服务端也就没机会回 401，
 * 界面会一直显示"已登录"。等下一次点击才跳登录页，体验上就是"点一下被踢出去"。
 * 本地按 exp 判断可以准点把界面切回登录页。
 *
 * 解析失败（不是 JWT / 没有 exp）一律当"未过期"，交给服务端判定 ——
 * 宁可多留一会儿，也不要因为解码失败把正常用户踢下线。
 */
export function isTokenExpired(token: string | null = getToken()): boolean {
  if (!token) return false
  const payload = decodeJWTPayload(token)
  if (!payload || typeof payload.exp !== 'number') return false
  return payload.exp * 1000 <= Date.now()
}

function decodeJWTPayload(token: string): any {
  const part = token.split('.')[1]
  if (!part) return null
  try {
    const b64 = part.replace(/-/g, '+').replace(/_/g, '/')
    const padded = b64 + '='.repeat((4 - (b64.length % 4)) % 4)
    const bin = atob(padded)
    // 用户名可能是中文：必须先按 UTF-8 解码，否则 JSON.parse 会抛
    const bytes = Uint8Array.from(bin, (c) => c.charCodeAt(0))
    return JSON.parse(new TextDecoder().decode(bytes))
  } catch {
    return null
  }
}

/** apiFetch 的额外选项 */
export interface ApiOptions extends RequestInit {
  /** 遇到 401 时**不要**触发"会话失效"处理（给登录/改密码用） */
  skipAuthExpired?: boolean
}

export async function apiFetch(path: string, options: ApiOptions = {}): Promise<any> {
  const headers: Record<string, string> = {
    'Content-Type': 'application/json',
    ...(options.headers as Record<string, string>),
  }
  const token = getToken()
  if (token) headers['Authorization'] = `Bearer ${token}`

  const { skipAuthExpired, ...init } = options
  const resp = await fetch(path, { ...init, headers })
  const data = await resp.json().catch(() => ({}))
  if (!resp.ok) {
    if (resp.status === 401 && !skipAuthExpired && !isCredentialEndpoint(path)) {
      // 服务端的话更准确（"未登录" / "令牌无效或已过期"），原样带给登录页
      notifyAuthExpired(data.error || '登录状态已失效，请重新登录')
    }
    throw new Error(data.error || `HTTP ${resp.status}`)
  }
  return data
}

// ---- 接口封装 ----

export async function login(username: string, password: string) {
  return apiFetch('/api/auth/login', {
    method: 'POST',
    body: JSON.stringify({ username, password }),
    // 密码错也是 401，但那不是"会话失效"：不能因此清令牌、跳登录页（我们本来就在登录页）
    skipAuthExpired: true,
  })
}

// ---- 面板身份信息（名称 / 版本 / 图标）----

export interface PanelMeta {
  /** 面板显示名（config.yaml 的 server.panel_name，默认 ATL-MCPanel） */
  name: string
  /** 面板版本（来自二进制，构建时注入） */
  version: string
  commit: string
  built_at: string
  go_version: string
  logo_url: string
  favicon_url: string
}

/** 前端在拿到 /api/meta 之前的兜底值：宁可显示默认名，也不要空着 */
export const FALLBACK_META: PanelMeta = {
  name: 'ATL-MCPanel',
  version: '',
  commit: '',
  built_at: '',
  go_version: '',
  logo_url: '/branding/logo-128.png',
  favicon_url: '/branding/favicon-64.png',
}

/**
 * 读面板身份信息（**免登录**：登录页要用它显示名称与版本号）。
 *
 * 版本号只有这一个来源：二进制里由 -ldflags 注入的那份。前端此前写死过
 * `PANEL_VERSION = 'v0.1.0'`（那是 package.json 的版本），界面显示 0.1.0
 * 而二进制是 0.9.14 —— 报 bug 时这个号码会把排查方向带偏。
 */
export async function getPanelMeta(): Promise<PanelMeta> {
  return apiFetch('/api/meta', { skipAuthExpired: true })
}

export async function listInstances(): Promise<Instance[]> {
  return apiFetch('/api/instances')
}

export interface CreateInstancePayload {
  node_id: number
  instance_id: string
  name: string
  core_type: string
  java_version: string
  port: number
  max_mem: string
  min_mem: string
  jar_url: string
  start_command: string
  /** CPU 配额百分比（100 = 1 核；0 = 不限制） */
  cpu_quota?: number
  /** cgroup 内存上限（如 "4G"；留空 = 不限制） */
  mem_limit?: string
  /** 实例目录软配额（MB，0 = 不限制） */
  disk_limit_mb?: number
  /** 超限时是否自动停止实例 */
  disk_autostop?: boolean
  /**
   * 创建时是否以容器方式运行（容器化隔离）。
   *
   * **三态**，别用 false 来表达"不表态"：
   *  - 不传该字段（undefined，JSON 里直接没有这个 key）→ 跟随节点默认值，
   *    现在即"节点装了 docker 且导入了镜像就容器化"，也就是**默认方式**；
   *  - 显式 true  → 必须容器化（节点不支持时后端报错，不静默降级）；
   *  - 显式 false → 必须原生进程（排障、或包不接受容器内的 /data 路径语义）。
   *
   * 非总管理员不提交此字段：开关只掌握在管理员手里，用户侧一律走节点默认值。
   */
  container?: boolean
  /** 创建时按线路申请的穿透端口数（多端口模组/插件用） */
  tunnels?: { frps_id: number; count: number }[]
}

export async function createInstance(payload: CreateInstancePayload) {
  return apiFetch('/api/instances', {
    method: 'POST',
    body: JSON.stringify(payload),
  })
}

export async function instanceAction(
  id: string,
  action: 'start' | 'stop' | 'kill' | 'restart' | 'delete',
  opts: { removeFiles?: boolean } = {},
) {
  const method = action === 'delete' ? 'DELETE' : 'POST'
  let path = action === 'delete' ? `/api/instances/${id}` : `/api/instances/${id}/${action}`
  // remove_files 只在显式要求时带上：默认行为是"只注销实例、保留文件"
  if (action === 'delete' && opts.removeFiles) path += '?remove_files=1'
  return apiFetch(path, { method })
}

// ---- 实例到期时间 ----

export interface ExpiryPayload {
  /** RFC3339 或 datetime-local 字符串；空串 = 清除到期时间 */
  expires_at: string
  /** 提前多少天提醒 */
  notice_days?: number
  /** 到期是否自动停止（false = 仅告警） */
  autostop?: boolean
}

export async function setInstanceExpiry(id: string, payload: ExpiryPayload) {
  return apiFetch(`/api/instances/${id}/expiry`, {
    method: 'POST',
    body: JSON.stringify(payload),
  })
}

// ---- 角色 ----

export type Role = 'admin' | 'nodeuser' | 'user'

export function roleLabel(role?: string): string {
  switch (role) {
    case 'admin': return '总管理员'
    // 兼容改名前的值（角色写在 JWT 里，旧令牌最长 24 小时）
    case 'nodeuser':
    case 'nodeadmin': return '节点用户'
    default: return '普通用户'
  }
}

export function isNodeUser(role?: string): boolean {
  return role === 'nodeuser' || role === 'nodeadmin'
}

// ---- 节点共享资源 ----

export interface NodeResource {
  name: string
  path: string
  size: number
  mod_time: number
  refs: string[]
  referenced: boolean
}

export async function listNodeResources(nodeId: number): Promise<{
  dir: string; files: NodeResource[]; available: boolean; message?: string
}> {
  return apiFetch(`/api/nodes/${nodeId}/resources`)
}

export async function uploadNodeResource(nodeId: number, file: File, overwrite = false): Promise<any> {
  const fd = new FormData()
  fd.append('file', file)
  if (overwrite) fd.append('overwrite', '1')
  const token = getToken()
  const res = await fetch(`/api/nodes/${nodeId}/resources`, {
    method: 'POST',
    headers: token ? { Authorization: `Bearer ${token}` } : {},
    body: fd,
  })
  const text = await res.text()
  let data: any = {}
  try { data = JSON.parse(text) } catch { /* 非 JSON */ }
  if (!res.ok) throw new Error(data.error || `上传失败（HTTP ${res.status}）`)
  return data
}

export async function deleteNodeResource(nodeId: number, name: string, force = false): Promise<any> {
  const q = new URLSearchParams({ name })
  if (force) q.set('force', '1')
  return apiFetch(`/api/nodes/${nodeId}/resources?${q.toString()}`, { method: 'DELETE' })
}

// ---- 节点上实际安装的 JDK ----

export interface JavaRuntime {
  label: string
  path: string
  home: string
  version: string
}

export async function listNodeJava(nodeId: number): Promise<{ runtimes: JavaRuntime[]; fallback: string }> {
  return apiFetch(`/api/nodes/${nodeId}/java`)
}

/**
 * 实例当前用的 JDK + 该节点上装了哪些 JDK（「启动」页换 JDK 的下拉用）。
 *
 * 单独一个实例级接口而不是让前端拼两次请求：实例页只拿得到 instance_id，
 * 而且"当前生效值"与"可选列表"来自两处（面板库 / 节点探测），
 * 分成两次请求就得处理两者不一致的中间态。
 */
export async function getInstanceJava(instanceId: string): Promise<{
  current: string
  runtimes: JavaRuntime[]
  fallback: string
  node_error: string
}> {
  return apiFetch(`/api/instances/${encodeURIComponent(instanceId)}/java`)
}

/** 给**已有实例**换 JDK（空串 = 自动用 PATH 上的 java）。下次启动生效。 */
export async function setInstanceJava(instanceId: string, javaVersion: string): Promise<{ message: string; java_version: string }> {
  return apiFetch(`/api/instances/${encodeURIComponent(instanceId)}/java`, {
    method: 'PUT', body: JSON.stringify({ java_version: javaVersion }),
  })
}

// ---- 节点用户授权（仅总管理员） ----

export interface NodeUserGrant {
  user_id: number
  username: string
  node_id: number
  node_name: string
  granted_by_name: string
  created_at: string
}

export async function listNodeUsers(): Promise<NodeUserGrant[]> {
  return apiFetch('/api/node-users')
}

export async function grantNodeUser(username: string, nodeId: number): Promise<any> {
  return apiFetch('/api/node-users', {
    method: 'POST',
    body: JSON.stringify({ username, node_id: nodeId }),
  })
}

export async function revokeNodeUser(userId: number, nodeId: number): Promise<any> {
  return apiFetch(`/api/node-users?user_id=${userId}&node_id=${nodeId}`, { method: 'DELETE' })
}

// ---- 穿透端口配额（仅总管理员分配） ----

export interface PortGrant {
  user_id: number
  username: string
  frps_id: number
  frps_name: string
  frps_host: string
  quota: number
  used: number
  available: number
  granted_by_name: string
}

export async function listPortGrants(): Promise<PortGrant[]> {
  return apiFetch('/api/node-users/ports')
}

export async function setPortGrant(username: string, frpsId: number, quota: number): Promise<any> {
  return apiFetch('/api/node-users/ports', {
    method: 'POST',
    body: JSON.stringify({ username, frps_id: frpsId, quota }),
  })
}

export async function deletePortGrant(userId: number, frpsId: number): Promise<any> {
  return apiFetch(`/api/node-users/ports?user_id=${userId}&frps_id=${frpsId}`, { method: 'DELETE' })
}

/** 我自己在各线路上的端口配额与已用量（创建实例时用）。 */
export interface MyPortLine {
  frps_id: number
  frps_name: string
  frps_host: string
  /** -1 表示不限（总管理员） */
  quota: number
  used: number
  available: number
  unlimited: boolean
}

export async function listMyPorts(): Promise<MyPortLine[]> {
  return apiFetch('/api/my/ports')
}

// ---- 我的权限（角色 + 被授权的实例）----

export interface MyPermission {
  instance_id: string
  /** owner / collab / viewer */
  level: string
}

/**
 * 我对各实例的权限级别。
 *
 * 总管理员返回**全部实例**（他对每个都是 owner），普通用户只返回
 * instance_assignments 里属于自己的那些 —— 所以这个列表天然就是
 * "我能看到的实例"，可以直接当作账户页的权限清单。
 */
export async function listMyPermissions(): Promise<{ role: string; permissions: MyPermission[] }> {
  return apiFetch('/api/my/instances')
}

// ---- 公告与帮助文档 ----

export interface Announcement {
  id: number
  title: string
  body: string
  pinned: boolean
  /** false = 草稿。普通用户的列表里不会出现草稿，只有管理员看得到 */
  published: boolean
  created_by_name: string
  created_at: string
  updated_at: string
}

export async function listAnnouncements(): Promise<Announcement[]> {
  return apiFetch('/api/announcements')
}

export async function createAnnouncement(body: {
  title: string; body: string; pinned?: boolean; published?: boolean
}): Promise<{ id: number; message: string }> {
  return apiFetch('/api/announcements', { method: 'POST', body: JSON.stringify(body) })
}

export async function updateAnnouncement(id: number, body: {
  title: string; body: string; pinned?: boolean; published?: boolean
}): Promise<{ message: string }> {
  return apiFetch(`/api/announcements/${id}`, { method: 'PUT', body: JSON.stringify(body) })
}

export async function deleteAnnouncement(id: number): Promise<{ message: string }> {
  return apiFetch(`/api/announcements/${id}`, { method: 'DELETE' })
}

export interface HelpDoc {
  content: string
  updated_at: string
  updated_by_name: string
}

export async function getHelp(): Promise<HelpDoc> {
  return apiFetch('/api/help')
}

export async function saveHelp(content: string): Promise<{ message: string }> {
  return apiFetch('/api/help', { method: 'PUT', body: JSON.stringify({ content }) })
}

// ---- 实例公网端口（所有能看到实例的用户都可读） ----

export interface InstancePort {
  id: number
  tunnel_id: string
  name: string
  protocol: string
  local_port: number
  remote_port: number
  line_name: string
  public_address: string
  status: string
  live_status: string
  live_error: string
  can_edit: boolean
}

export async function listInstancePorts(instanceId: string): Promise<{
  ports: InstancePort[]
  can_edit: boolean
  /** 还能再开几个；-1 = 不限 */
  remaining: number
  /**
   * 本实例可用的线路及各线路余额。
   *
   * 由后端按**本实例配额归属者**算好，与开通时实际扣费口径一致。
   * 不要改用 listMyPorts()：那返回的是**操作者**的配额，
   * 节点用户替别人的实例开端口时会与实际扣费对不上。
   */
  lines: MyPortLine[]
  /** 扣的是不是操作者自己的配额（false = 扣实例归属者的） */
  charging_self: boolean
}> {
  return apiFetch(`/api/instances/${instanceId}/ports`)
}

export async function addInstancePort(
  instanceId: string,
  payload: { frps_id: number; local_port: number; protocol?: string },
): Promise<any> {
  return apiFetch(`/api/instances/${instanceId}/ports`, {
    method: 'POST',
    body: JSON.stringify(payload),
  })
}

export async function updateInstancePort(
  instanceId: string,
  tunnelId: string,
  payload: { local_port: number; protocol?: string },
): Promise<any> {
  return apiFetch(`/api/instances/${instanceId}/ports/${encodeURIComponent(tunnelId)}`, {
    method: 'POST',
    body: JSON.stringify(payload),
  })
}

export async function deleteInstancePort(instanceId: string, tunnelId: string): Promise<any> {
  return apiFetch(`/api/instances/${instanceId}/ports/${encodeURIComponent(tunnelId)}`, {
    method: 'DELETE',
  })
}

/**
 * 我可以在哪些节点上创建实例。
 *
 * 单独一个类型而不是复用 NodeInfo：这个接口对**普通用户/节点管理员**开放，
 * 因此返回的字段里刻意不含 SSH 用户名、凭据、Daemon 端口等运维信息。
 */
export interface MyNode {
  id: number
  name: string
  ip: string
  /** 原始状态列（Daemon 失联时不会自己变）—— 判断在线请用 online */
  status: string
  /** 按心跳新鲜度算出来的在线状态 */
  online: boolean
  instances: number
}

export async function listMyNodes(): Promise<MyNode[]> {
  return apiFetch('/api/my/nodes')
}

// ---- 节点监控（所有登录用户可见） ----

export interface NodeMonitor {
  id: number
  name: string
  status: string
  cpu: number
  mem_total: number
  cpu_percent: number
  mem_used: number
  disk_used: number
  disk_total: number
  last_seen: string
  online: boolean
  instances: number
  running: number
}

export interface MonitorSummary {
  nodes: NodeMonitor[]
  total_nodes: number
  online_nodes: number
  total_instances: number
  running_instances: number
}

export interface MonitorInstance {
  instance_id: string
  name: string
  node_id: number
  node_name: string
  core_type: string
  port: number
  max_mem: string
  cpu_quota: number
  status: string
  live_status: string
}

export async function monitorNodes(): Promise<MonitorSummary> {
  return apiFetch('/api/monitor/nodes')
}

export async function monitorInstances(): Promise<MonitorInstance[]> {
  return apiFetch('/api/monitor/instances')
}

export function consoleWsUrl(instanceId: string): string {
  const proto = window.location.protocol === 'https:' ? 'wss' : 'ws'
  const token = getToken() || ''
  return `${proto}://${window.location.host}/ws/console/${instanceId}?token=${encodeURIComponent(token)}`
}

// ---- 实例授权 ----

export async function listAssignments(instanceId: string): Promise<any[]> {
  return apiFetch(`/api/instances/${instanceId}/assignments`)
}

export async function grantAssignment(instanceId: string, username: string, level: string) {
  return apiFetch(`/api/instances/${instanceId}/assignments`, {
    method: 'POST',
    body: JSON.stringify({ username, level }),
  })
}

export async function revokeAssignment(instanceId: string, userId: number) {
  return apiFetch(`/api/instances/${instanceId}/assignments?user_id=${userId}`, { method: 'DELETE' })
}

// ---- 审计日志 ----

export async function listAuditLogs(limit = 100): Promise<any[]> {
  return apiFetch(`/api/audit-logs?limit=${limit}`)
}

// ---- 备份/回滚 ----

export interface BackupItem {
  backup_id: string
  name: string
  size: number
  created_at: number
}

export async function listBackups(instanceId: string): Promise<BackupItem[]> {
  return apiFetch(`/api/instances/${instanceId}/backups`)
}

export async function createBackup(instanceId: string, name: string, includeConfig: boolean) {
  return apiFetch(`/api/instances/${instanceId}/backups`, {
    method: 'POST',
    body: JSON.stringify({ name, include_config: includeConfig }),
  })
}

export async function deleteBackup(instanceId: string, backupId: string) {
  return apiFetch(`/api/instances/${instanceId}/backups?backup_id=${encodeURIComponent(backupId)}`, {
    method: 'DELETE',
  })
}

export async function restoreBackup(instanceId: string, backupId: string) {
  return apiFetch(`/api/instances/${instanceId}/restore`, {
    method: 'POST',
    body: JSON.stringify({ backup_id: backupId }),
  })
}

// ---- 穿透管理（frps 服务器 + 隧道） ----

export interface FrpsServer {
  id: number
  name: string
  host: string
  bind_port: number
  token: string
  port_start: number
  port_end: number
  remark: string
  /**
   * 线路级对外域名（纯主机名，不带端口；后端写入时会归一化）。
   *
   * 配了之后，这条线路上的**所有**端口都会显示成 `域名:端口`，
   * 而不是 `节点IP:端口` —— 后者会让用户一转发就把节点真实入口公布出去。
   */
  display_domain: string
  used_ports: number
}

export interface Tunnel {
  id: number
  tunnel_id: string
  instance_id: string
  instance_name: string
  frps_id: number
  frps_name: string
  frps_host: string
  name: string
  protocol: string
  local_port: number
  remote_port: number
  status: string
  live_status: string
  live_error: string
  public_address: string
  display_domain: string // 管理员配置的对外域名（空则展示 IP:端口）
}

export async function listFrps(): Promise<FrpsServer[]> {
  return apiFetch('/api/frps')
}

export async function createFrps(payload: Partial<FrpsServer>): Promise<any> {
  return apiFetch('/api/frps', { method: 'POST', body: JSON.stringify(payload) })
}

/**
 * 改线路。目前只开放 名称 / 备注 / **对外域名** ——
 * host、bind_port、token、端口段不在其中：它们一动，该线路上
 * 已下发的所有 frpc 配置都得重写，属于"删掉重建"更安全的操作。
 */
export async function updateFrps(
  id: number,
  payload: { name?: string; remark?: string; display_domain?: string },
): Promise<{ message: string; display_domain: string }> {
  return apiFetch(`/api/frps/${id}`, { method: 'PUT', body: JSON.stringify(payload) })
}

export async function deleteFrps(id: number) {
  return apiFetch(`/api/frps/${id}`, { method: 'DELETE' })
}

/** 线路自检的结果（分两层：TCP 可达 + frpc 能否注册）。 */
export interface FrpsTestResult {
  success: boolean
  error?: string
  dial_ok: boolean
  dial_ms: number
  dial_error?: string
  register_ok: boolean
  /** 自检时实际占用的远程端口（用完立刻释放） */
  used_port: number
  register_error?: string
  /** frpc 日志尾部：失败时给人看原始原因 */
  log_tail?: string
  node_id: number
  node_name: string
  address: string
}

/**
 * 线路自检（保存之前就能调用）。
 *
 * 自检**在节点上跑**：真正跑 frpc 的是节点，面板能连通不等于节点能连通 ——
 * 它会在节点上起一个临时 frpc，用这个 token 登录并占一个端口，随后撤掉。
 */
export async function testFrps(cfg: {
  host: string
  bind_port?: number
  token?: string
  port_start?: number
  port_end?: number
  node_id?: number
}): Promise<FrpsTestResult> {
  return apiFetch('/api/frps/test', { method: 'POST', body: JSON.stringify(cfg) })
}

/** 用库里已保存的线路配置做自检。 */
export async function testFrpsSaved(id: number, node_id?: number): Promise<FrpsTestResult> {
  return apiFetch(`/api/frps/${id}/test`, {
    method: 'POST',
    body: JSON.stringify({ node_id: node_id || 0 }),
  })
}

export async function listTunnels(): Promise<Tunnel[]> {
  return apiFetch('/api/tunnels')
}

export async function createTunnel(payload: {
  instance_id: string
  frps_id: number
  protocol: string
  local_port?: number
  remote_port?: number
  name?: string
  /** 对外展示域名（可含端口）；留空则展示 IP:端口 */
  display_domain?: string
}): Promise<any> {
  return apiFetch('/api/tunnels', { method: 'POST', body: JSON.stringify(payload) })
}

export async function deleteTunnel(id: number) {
  return apiFetch(`/api/tunnels/${id}`, { method: 'DELETE' })
}

export async function reapplyTunnel(id: number): Promise<any> {
  return apiFetch(`/api/tunnels/${id}/reapply`, { method: 'POST' })
}

// ---- 面板自身穿透 ----

export interface PanelTunnelConfig {
  enabled: boolean
  frps_id: number
  proxy_type: string // tcp / http / https
  remote_port: number
  custom_domain: string
  subdomain: string
  use_tls: boolean
  cert_file: string
  key_file: string
}

export interface PanelTunnelState {
  config: PanelTunnelConfig
  status: string
  error: string
  public_address: string
  local_port: number
  tls_port: number
  current_url: string
}

export async function getPanelTunnel(): Promise<PanelTunnelState> {
  return apiFetch('/api/panel-tunnel')
}

export async function setPanelTunnel(cfg: Partial<PanelTunnelConfig>): Promise<any> {
  return apiFetch('/api/panel-tunnel', { method: 'POST', body: JSON.stringify(cfg) })
}

// ---- 节点管理 ----

export interface NodeInfo {
  id: number
  name: string
  ip: string
  ssh_user: string
  ssh_port: number
  has_auth: boolean
  /** 数据库里的原始状态列。Daemon 失联时它不会自己变 —— 判断在线请用 online */
  status: string
  /** 按心跳新鲜度算出来的在线状态（90 秒没有心跳即视为离线） */
  online: boolean
  cpu: number
  mem: number
  last_seen: string
  /** 心跳距今秒数（-1 = 从未上报） */
  last_seen_age_s: number
  instances: number
}

export interface NodeProbe {
  arch: string
  os: string
  has_systemd: boolean
  installed: boolean
  existing_service: boolean
  binary_version: string
  free_disk_mb: number
}

export async function listNodes(): Promise<NodeInfo[]> {
  return apiFetch('/api/nodes')
}

export async function createNode(payload: {
  name: string; ip: string; ssh_user?: string; ssh_auth?: string; ssh_port?: number
}): Promise<any> {
  return apiFetch('/api/nodes', { method: 'POST', body: JSON.stringify(payload) })
}

export async function updateNode(id: number, payload: Partial<NodeInfo> & { ssh_auth?: string }): Promise<any> {
  return apiFetch(`/api/nodes/${id}`, { method: 'PUT', body: JSON.stringify(payload) })
}

export async function deleteNode(id: number): Promise<any> {
  return apiFetch(`/api/nodes/${id}`, { method: 'DELETE' })
}

export async function probeNode(id: number): Promise<{ node: string; probe: NodeProbe; ready: boolean }> {
  return apiFetch(`/api/nodes/${id}/probe`, { method: 'POST' })
}

export async function deployNode(id: number): Promise<{ message: string; steps: string[] }> {
  return apiFetch(`/api/nodes/${id}/deploy`, { method: 'POST' })
}

export async function restartDaemon(id: number): Promise<any> {
  return apiFetch(`/api/nodes/${id}/daemon/restart`, { method: 'POST' })
}

export async function daemonLogs(id: number, lines = 100): Promise<{ node: string; logs: string }> {
  return apiFetch(`/api/nodes/${id}/daemon/logs?lines=${lines}`)
}

// ---- PKI ----

export interface PKIInfo {
  initialized: boolean
  subject: string
  not_after: string
  server_name: string
  grpc_mtls: boolean
  ca_cert_path: string
  ca_cert: string
}

export async function getPKI(): Promise<PKIInfo> {
  return apiFetch('/api/pki')
}

export async function getNodeCert(id: number): Promise<any> {
  return apiFetch(`/api/nodes/${id}/cert`)
}

// ---- 玩家管理 ----

export interface PlayerEntry {
  uuid: string
  name: string
  level?: number
  reason?: string
  source?: string
  expires?: string
  ip?: string
}

export interface PlayerList {
  type: string
  file: string
  exists: boolean
  entries: PlayerEntry[]
  whitelist_enabled: boolean
}

export async function listPlayers(instanceId: string, type: string): Promise<PlayerList> {
  return apiFetch(`/api/instances/${instanceId}/players?type=${type}`)
}

// ---- 全部玩家信息（跨名单文件合并的历史玩家总览） ----

export interface PlayerOverviewEntry {
  uuid: string
  name: string
  play_seconds: number
  online: boolean
  whitelisted: boolean
  op: boolean
  banned: boolean
  has_data: boolean
  last_seen: number
  source: string
}

export interface PlayerOverview {
  players: PlayerOverviewEntry[]
  total: number
  online: number
  whitelist_enabled: boolean
  world_name: string
}

export async function getPlayerOverview(instanceId: string): Promise<PlayerOverview> {
  return apiFetch(`/api/instances/${instanceId}/players/all`)
}

export async function addPlayer(instanceId: string, type: string, name: string): Promise<any> {
  return apiFetch(`/api/instances/${instanceId}/players`, {
    method: 'POST',
    body: JSON.stringify({ type, name }),
  })
}

export async function removePlayer(instanceId: string, type: string, name: string): Promise<any> {
  return apiFetch(`/api/instances/${instanceId}/players?type=${type}&name=${encodeURIComponent(name)}`, {
    method: 'DELETE',
  })
}

/**
 * 踢出在线玩家。
 *
 * 复用"加入名单"那个接口（type=kick）：踢出是一次性动作、没有名单文件，
 * 但它对前端的形状和"加进某个名单"是一样的 —— 都是"对某个玩家做一件事"，
 * 单独开一个接口只会让调用方多写一条分支。后端对 kick 走的是纯控制台路径。
 */
export async function kickPlayer(instanceId: string, name: string): Promise<any> {
  return addPlayer(instanceId, 'kick', name)
}

/**
 * 设置某玩家的 OP 等级（1~4）。
 *
 * 注意这不是"开关 OP"，而是"设成几级"：原版的 /op 命令一律按
 * server.properties 的 op-permission-level 设等级，表达不了具体级别，
 * 所以后端改的是 ops.json 里的 level 字段。
 */
export async function setOpLevel(instanceId: string, name: string, level: number): Promise<any> {
  return apiFetch(`/api/instances/${instanceId}/players/op-level`, {
    method: 'POST',
    body: JSON.stringify({ name, level }),
  })
}

export async function setWhitelist(instanceId: string, enabled: boolean): Promise<any> {
  return apiFetch(`/api/instances/${instanceId}/whitelist`, {
    method: 'POST',
    body: JSON.stringify({ enabled }),
  })
}

// ---- 日志分析的可插拔化：提供方 / 设置 / 统一分析入口 ----
//
// 原来的 `/logshare/*` 接口保留（老版本前端与兼容性），新的界面走这一组：
// 链上现在有四类提供方（LogShare / mclo.gs / 用户自配平台 / V2 的内置规则），
// 而用户看到的应该是同一件事。

/** 提供方类型 */
export type AnalysisKind = 'logshare' | 'mclogs' | 'openai' | 'builtin-rules'

export interface AnalysisProvider {
  ID?: number
  id: number
  name: string
  kind: AnalysisKind
  base_url: string
  model: string
  key_hint?: string
  max_bytes?: number
  timeout_sec?: number
  prompt?: string
  owner_id?: number
  enabled: boolean
  /** 面板给的一句话说明（谁提供、做什么） */
  describe?: string
}

export interface AnalysisChainEntry {
  id: number
  kind: AnalysisKind
  name: string
  reason: string
}

export interface AnalysisProvidersResp {
  builtin: AnalysisProvider[]
  custom: AnalysisProvider[]
  chain: AnalysisChainEntry[]
  can_manage: boolean
  logshare_on: boolean
}

export interface HelpPlaceholder {
  name: string
  desc: string
}

export interface AnalysisSettings {
  order: string[]
  allow_private: boolean
  rate_per_min: number
  rate_per_day: number
  can_manage: boolean
  logshare_on: boolean
  rate_default_min: number
  rate_default_day: number
  /** 当前生效的求助模板原文（含 {占位符}） */
  help_template: string
  /** 内置默认模板（「恢复默认」按钮用它，也用作编辑框的初始内容） */
  help_template_default: string
  /** 是否还是内置默认（没被管理员改过） */
  help_template_is_default: boolean
  /** 占位符图例：由后端给出，避免文档与实现走散 */
  help_placeholders: HelpPlaceholder[]
}

export async function listAnalysisProviders(): Promise<AnalysisProvidersResp> {
  return apiFetch('/api/analysis/providers')
}

export async function createAnalysisProvider(payload: Partial<AnalysisProvider> & { api_key?: string; global?: boolean }) {
  return apiFetch('/api/analysis/providers', { method: 'POST', body: JSON.stringify(payload) })
}

export async function updateAnalysisProvider(id: number, payload: Partial<AnalysisProvider> & { api_key?: string }) {
  return apiFetch(`/api/analysis/providers/${id}`, { method: 'PUT', body: JSON.stringify(payload) })
}

export async function deleteAnalysisProvider(id: number) {
  return apiFetch(`/api/analysis/providers/${id}`, { method: 'DELETE' })
}

/** 连通性自检：返回 {ok, error?, took_ms?, message?}（失败也是 200，看 ok 字段） */
export async function testAnalysisProvider(id: number): Promise<{ ok: boolean; error?: string; took_ms?: number; message?: string }> {
  return apiFetch(`/api/analysis/providers/${id}/test`, { method: 'POST' })
}

export async function getAnalysisSettings(): Promise<AnalysisSettings> {
  return apiFetch('/api/analysis/settings')
}

export async function setAnalysisSettings(payload: Partial<Pick<AnalysisSettings, 'order' | 'allow_private' | 'rate_per_min' | 'rate_per_day' | 'help_template'>>) {
  return apiFetch('/api/analysis/settings', { method: 'PUT', body: JSON.stringify(payload) })
}

export interface HelpPreview {
  text: string
  template: string
  is_default: boolean
  pending_fields: string[]
  note: string
}

/**
 * 预览「将生成的求助文本」。
 *
 * 求助模板现在是管理员可改的：只把模板原文摆给用户看（一堆 {占位符}），
 * 他并不知道自己最后会贴出去什么。这里按真实实例渲染一遍，纯本地、不出网。
 */
export async function previewHelpText(instanceId: string, phenomenon: string): Promise<HelpPreview> {
  return apiFetch(`/api/instances/${encodeURIComponent(instanceId)}/analysis/help-preview`, {
    method: 'POST', body: JSON.stringify({ phenomenon }),
  })
}

export interface AnalyseOpts {
  path: string
  filterChat: boolean
  agree: boolean
  providerId?: number
  /** 按**类型**指定内置提供方（logshare / mclogs）：它们没有数据库行、id 都是 0 */
  providerKind?: string
  /** 可选：把"现象"也带上（用于生成求助文本） */
  phenomenon?: string
}

export interface AnalyseResult {
  record_id: number
  provider_kind: AnalysisKind
  provider?: { id: number; kind: AnalysisKind; name: string; is_ai: boolean; describe: string }
  url?: string
  raw_url?: string
  size?: number
  lines?: number
  errors?: number
  filtered_lines?: number
  truncated?: number
  attached?: number
  expires_at?: string
  ai_available: boolean
  /** mclo.gs 的求助文本（面板已把环境信息填好，一键复制即可） */
  help_text?: string
  /** mclo.gs 的说明（这不是 AI 分析） */
  notice?: string
  /** 发生过回退时：每一家的失败原因 */
  fallbacks?: { provider: string; kind: string; error: string; reason: string }[]
  fallback_note?: string
}

export async function analyseInstance(instanceId: string, opts: AnalyseOpts): Promise<AnalyseResult> {
  return apiFetch(`/api/instances/${instanceId}/analyse`, {
    method: 'POST',
    body: JSON.stringify({
      path: opts.path,
      filter_chat: opts.filterChat,
      agree: opts.agree,
      // 内置提供方（LogShare / mclo.gs）没有数据库行，id 都是 0 ——
      // 只传 id 的话"自动"与"指定内置"撞成同一个值，所以内置的用 kind 指定
      provider_id: opts.providerId || 0,
      provider_kind: opts.providerKind || '',
      phenomenon: opts.phenomenon || '',
    }),
  })
}

export interface AnalysisRecord {
  id: number
  instance_id: string
  logshare_id: string
  url: string
  raw_url: string
  source_path: string
  size: number
  lines: number
  errors: number
  filtered_lines: number
  truncated: boolean
  created_at: string
  expires_at: string
  deleted: boolean
  provider_kind: AnalysisKind
  provider_id: number
  analysis: string
  cache_key: string
}

export async function listAnalysisHistory(instanceId: string): Promise<{ history: AnalysisRecord[]; limits: { per_min: number; per_day: number } }> {
  return apiFetch(`/api/instances/${instanceId}/analysis`)
}

export async function deleteAnalysisRecord(recordId: number) {
  return apiFetch(`/api/analysis/${recordId}`, { method: 'DELETE' })
}

/**
 * 统一的 AI 分析流（SSE）。
 *
 * 与原来那条 `streamLogShareAI` 的关系：那条按"对方的日志 id"取流；
 * 这条按**面板的记录 id**，于是自配平台（没有远端 id）也能走同一套渲染。
 */
export async function streamAnalysisAI(
  recordId: number,
  onEvent: (ev: { event: string; data: string }) => void,
  signal?: AbortSignal,
): Promise<void> {
  const headers: Record<string, string> = { Accept: 'text/event-stream' }
  const token = getToken()
  if (token) headers['Authorization'] = `Bearer ${token}`
  const resp = await fetch(`/api/analysis/ai/${recordId}`, { headers, signal })
  if (!resp.ok) {
    const data = await resp.json().catch(() => ({} as any))
    throw new Error(data.error || `HTTP ${resp.status}`)
  }
  if (!resp.body) throw new Error('响应没有可读流')

  const reader = resp.body.getReader()
  const decoder = new TextDecoder()
  let buf = ''
  for (;;) {
    const { done, value } = await reader.read()
    if (done) break
    buf += decoder.decode(value, { stream: true })
    let idx: number
    while ((idx = buf.indexOf('\n\n')) >= 0) {
      const raw = buf.slice(0, idx)
      buf = buf.slice(idx + 2)
      let event = 'message'
      const dataLines: string[] = []
      for (const line of raw.split('\n')) {
        if (line.startsWith('event:')) event = line.slice(6).trim()
        else if (line.startsWith('data:')) dataLines.push(line.slice(5).replace(/^ /, ''))
      }
      onEvent({ event, data: dataLines.join('\n') })
    }
  }
}

// ---- 第三方日志分析（LogShare.CN）----

export interface LogShareFile {
  path: string
  name: string
  size: number
  mod_time: number
  kind: 'crash' | 'latest' | 'console' | 'rotated' | string
}

export interface LogShareRecord {
  id: number
  logshare_id: string
  url: string
  source_path: string
  size: number
  lines: number
  filtered_lines: number
  truncated: boolean
  created_at: string
  expires_at: string
  deleted: boolean
  /** 已缓存的 AI 结论（Markdown）；为空表示还没分析过 */
  analysis: string
}

export interface LogShareFilesResp {
  files: LogShareFile[]
  site_url: string
  terms_url: string
  privacy_url: string
  max_bytes: number
  history: LogShareRecord[]
}

export async function listLogShareFiles(instanceId: string): Promise<LogShareFilesResp> {
  return apiFetch(`/api/instances/${instanceId}/logshare/files`)
}

export async function listLogShareHistory(instanceId: string): Promise<{ enabled: boolean; history: LogShareRecord[] }> {
  return apiFetch(`/api/instances/${instanceId}/logshare`)
}

export interface LogShareSettings {
  enabled: boolean
  /** 当前登录者是不是总管理员（只有他能开关这个功能） */
  can_manage: boolean
  /** 对方的实际保留期（实测自 LogShare /limits，会变，所以不写死） */
  retention_days: number
  site_url: string
}

/** 读取第三方日志分析的运行时开关与对方的保留期 */
export async function getLogShareSettings(): Promise<LogShareSettings> {
  return apiFetch('/api/logshare/settings')
}

/**
 * 总管理员开关第三方日志分析。
 *
 * 这类"把租户日志发到外部"的开关必须能一键关掉：所以走接口 + 立即生效，
 * 而不是让人去改 config.yaml 再重启服务（现实中那等于"没人关"）。
 * 服务端会校验角色，前端藏不住也绕不过；开关动作会写进审计日志。
 */
export async function setLogShareEnabled(enabled: boolean): Promise<{ message: string; enabled: boolean }> {
  return apiFetch('/api/logshare/settings', {
    method: 'PUT',
    body: JSON.stringify({ enabled }),
  })
}

export interface NodeContainerCapability {
  available: boolean
  docker_present: boolean
  image_present: boolean
  docker_version: string
  image: string
  reason: string
}

/**
 * 节点是否具备容器化能力（装了 docker 且已导入基础镜像）。
 *
 * 建实例表单靠它决定"容器化"复选框的默认值与可用性 —— 否则用户勾了，
 * 直到启动时才在节点上发现跑不起来。
 */
export async function getNodeContainerCapability(nodeId: number): Promise<NodeContainerCapability> {
  return apiFetch(`/api/nodes/${nodeId}/container`)
}

/**
 * 上传日志并请求分析。
 *
 * `agree` 必须是用户**手动勾选**的结果：这是把日志（含玩家名与聊天内容）
 * 交给第三方的唯一合法性依据，服务端也会再校验一次。
 */
export async function analyseLog(
  instanceId: string, path: string, opts: { filterChat: boolean; agree: boolean },
): Promise<{
  id: string; url: string; size: number; lines: number
  filtered_lines: number; truncated: number; attached: number; expires_at: string
}> {
  return apiFetch(`/api/instances/${instanceId}/logshare/analyse`, {
    method: 'POST',
    body: JSON.stringify({ path, filter_chat: opts.filterChat, agree: opts.agree }),
  })
}

export async function deleteLogShare(instanceId: string, logshareId: string) {
  return apiFetch(`/api/instances/${instanceId}/logshare/${encodeURIComponent(logshareId)}`, { method: 'DELETE' })
}

/**
 * 流式读取 AI 分析（SSE）。
 *
 * 用 fetch + ReadableStream 而不是 EventSource：EventSource **不能带请求头**，
 * 而我们的接口要 Authorization —— 用 EventSource 就只能把令牌放进 URL
 *（正是我们一直在避免的做法）。
 *
 * 返回的每个事件形如 {event, data}；data 是对方原样的 JSON 字符串。
 */
export async function streamLogShareAI(
  instanceId: string, logshareId: string,
  onEvent: (ev: { event: string; data: string }) => void,
  signal?: AbortSignal,
): Promise<void> {
  const headers: Record<string, string> = { Accept: 'text/event-stream' }
  const token = getToken()
  if (token) headers['Authorization'] = `Bearer ${token}`
  const resp = await fetch(`/api/instances/${instanceId}/logshare/ai/${encodeURIComponent(logshareId)}`, { headers, signal })
  if (!resp.ok) {
    const data = await resp.json().catch(() => ({} as any))
    throw new Error(data.error || `HTTP ${resp.status}`)
  }
  if (!resp.body) throw new Error('响应没有可读流')

  const reader = resp.body.getReader()
  const decoder = new TextDecoder()
  let buf = ''
  for (;;) {
    const { done, value } = await reader.read()
    if (done) break
    buf += decoder.decode(value, { stream: true })
    // SSE 以空行分隔事件
    let idx: number
    while ((idx = buf.indexOf('\n\n')) >= 0) {
      const raw = buf.slice(0, idx)
      buf = buf.slice(idx + 2)
      let event = 'message'
      const datas: string[] = []
      for (const line of raw.split('\n')) {
        if (line.startsWith('event:')) event = line.slice(6).trim()
        else if (line.startsWith('data:')) datas.push(line.slice(5).trim())
      }
      if (datas.length) onEvent({ event, data: datas.join('\n') })
    }
  }
}

// ---- 核心 jar 管理 ----

export interface JarItem {
  filename: string
  path: string
  size: number
  active: boolean
}

export async function listJars(instanceId: string): Promise<{ jars: JarItem[]; active_jar: string }> {
  return apiFetch(`/api/instances/${instanceId}/jars`)
}

export async function uploadJar(instanceId: string, file: File): Promise<any> {
  const fd = new FormData()
  fd.append('file', file)
  // 不要设置 Content-Type，浏览器会自动带 boundary
  const token = getToken()
  const res = await fetch(`/api/instances/${instanceId}/jars`, {
    method: 'POST',
    headers: token ? { Authorization: `Bearer ${token}` } : {},
    body: fd,
  })
  const text = await res.text()
  let data: any = {}
  try { data = JSON.parse(text) } catch { /* 非 JSON 响应 */ }
  if (!res.ok) throw new Error(data.error || `上传失败（HTTP ${res.status}）`)
  return data
}

export async function setJar(instanceId: string, jarPath: string): Promise<any> {
  return apiFetch(`/api/instances/${instanceId}/jar`, {
    method: 'POST',
    body: JSON.stringify({ jar_path: jarPath }),
  })
}

// ---- 启动脚本 ----

export interface StartScriptState {
  instance_id: string
  mode: string        // start.sh / custom / default
  mode_label: string
  command: string     // 实际会执行的命令
  template: string    // 自定义命令模板
  has_script: boolean
  script: string
  jar_path: string
  max_mem: string
  min_mem: string
  core_type: string
}

export async function getStartScript(instanceId: string): Promise<StartScriptState> {
  return apiFetch(`/api/instances/${instanceId}/start-script`)
}

// ---- 容器化隔离 ----

/** 实例容器化状态：是否已启用、节点是否具备条件、不可用时的原因。 */
export interface ContainerState {
  enabled: boolean
  available: boolean
  running: boolean
  reason: string
  docker_present?: boolean
  image_present?: boolean
  docker_version?: string
  image?: string
  container_note?: string
  status?: string
}

export async function getContainerState(instanceId: string): Promise<ContainerState> {
  return apiFetch(`/api/instances/${instanceId}/container`)
}

export async function setContainerEnabled(instanceId: string, enabled: boolean): Promise<any> {
  return apiFetch(`/api/instances/${instanceId}/container`, {
    method: 'PUT',
    body: JSON.stringify({ enabled }),
  })
}

export async function setStartScript(
  instanceId: string,
  action: 'generate' | 'save' | 'remove',
  content?: string,
): Promise<any> {
  return apiFetch(`/api/instances/${instanceId}/start-script`, {
    method: 'POST',
    body: JSON.stringify({ action, content }),
  })
}

// ---- 定时备份计划 ----

export interface ScheduleConfig {
  instance_id: string
  enabled: boolean
  interval_hours: number
  keep: number
  include_config: boolean
  last_run: string
  last_error: string
  next_run: string
  // 生效的保留策略
  policy_id: number
  policy_name: string
  policy_desc: string
  manual_keep: number
  effective_hours: number
}

// ---- 备份保留策略（管理员配置的「备份组」） ----

export interface RetentionTier {
  within_hours: number
  keep: number
}

export interface BackupPolicy {
  id: number
  name: string
  remark: string
  auto_interval_hours: number
  manual_keep: number
  tiers: RetentionTier[]
  include_config: boolean
  is_default: boolean
  used_by: number
  describe: string
}

export async function listBackupPolicies(): Promise<BackupPolicy[]> {
  return apiFetch('/api/backup-policies')
}

export async function saveBackupPolicy(p: Partial<BackupPolicy>): Promise<any> {
  return apiFetch('/api/backup-policies', { method: 'POST', body: JSON.stringify(p) })
}

export async function deleteBackupPolicy(id: number): Promise<any> {
  return apiFetch(`/api/backup-policies/${id}`, { method: 'DELETE' })
}

export async function getSchedule(instanceId: string): Promise<ScheduleConfig> {
  return apiFetch(`/api/instances/${instanceId}/schedule`)
}

export async function setSchedule(instanceId: string, cfg: {
  enabled: boolean
  interval_hours: number
  keep: number
  include_config: boolean
  /** 保留策略 ID；0 或省略表示使用默认策略 */
  policy_id?: number
}): Promise<any> {
  return apiFetch(`/api/instances/${instanceId}/schedule`, { method: 'POST', body: JSON.stringify(cfg) })
}

// ---- 告警 ----

export interface AlertItem {
  id: number
  kind: string
  severity: string
  target: string
  message: string
  detail: string
  active: boolean
  created_at: string
  resolved_at: string
}

export async function listAlerts(activeOnly = true, limit = 100): Promise<{ alerts: AlertItem[]; active_count: number }> {
  return apiFetch(`/api/alerts?active=${activeOnly ? 1 : 0}&limit=${limit}`)
}

export async function resolveAlert(id: number): Promise<any> {
  return apiFetch(`/api/alerts/${id}/resolve`, { method: 'POST' })
}

// ---- 面板数据库备份 ----

export interface PanelBackup {
  name: string
  path: string
  size: number
  mod_time: string
}

export async function listPanelBackups(): Promise<{
  enabled: boolean; dir: string; keep: number; last_run: string; backups: PanelBackup[]
}> {
  return apiFetch('/api/panel-backups')
}

export async function createPanelBackup(): Promise<any> {
  return apiFetch('/api/panel-backups', { method: 'POST' })
}

// ---- 文件管理 ----

export async function listFiles(instanceId: string, path: string): Promise<{ path: string; files: FileItem[] }> {
  return apiFetch(`/api/instances/${instanceId}/files?path=${encodeURIComponent(path)}`)
}

export async function readFile(instanceId: string, path: string): Promise<{ content: string; size: number }> {
  return apiFetch(`/api/instances/${instanceId}/file?path=${encodeURIComponent(path)}`)
}

/**
 * checkUpload 上传前预检：单文件上限 / 实例磁盘配额 / 节点剩余空间。
 *
 * 为什么界面上传前要先问一次：如果直接发大文件、服务端在中途拒绝并关连接，
 * 客户端还在往外写数据，得到的是 **connection reset** —— 用户看到"网络被重置"，
 * 而不是"磁盘配额不足：配额 2 GB，已用 1.9 GB"。实测如此（20MB 请求被拒时
 * wget 与 python 都拿到 RST 而非响应体）。
 */
export async function checkUpload(
  instanceId: string,
  size: number,
): Promise<{ ok: boolean; error?: string }> {
  const q = new URLSearchParams({ size: String(Math.max(0, Math.floor(size))) })
  return apiFetch(`/api/instances/${instanceId}/upload-check?${q.toString()}`)
}

/**
 * uploadFile 上传一个文件到实例目录（流式，支持进度）。
 *
 * 为什么用 XHR 而不是 fetch：**fetch 至今没有上传进度回调**（只有下载能用
 * ReadableStream 读进度）。拖放上传一个大模组包时，界面上必须有百分比，
 * 否则用户不知道是在传还是卡住了。XHR 的 upload.onprogress 正好给这个。
 *
 * 为什么 body 直接是文件（不是 multipart）：服务端是流式转发的，
 * multipart 会要求它先解析再转发，多一次完整缓冲。
 */
export function uploadFile(
  instanceId: string,
  path: string,
  file: File,
  overwrite: boolean,
  onProgress?: (loaded: number, total: number) => void,
): Promise<{ path: string; size: number }> {
  return new Promise((resolve, reject) => {
    const q = new URLSearchParams({ path })
    if (overwrite) q.set('overwrite', '1')
    const xhr = new XMLHttpRequest()
    xhr.open('POST', `/api/instances/${instanceId}/upload?${q.toString()}`)
    const token = getToken()
    if (token) xhr.setRequestHeader('Authorization', `Bearer ${token}`)
    xhr.setRequestHeader('Content-Type', 'application/octet-stream')

    xhr.upload.onprogress = (e) => {
      if (e.lengthComputable) onProgress?.(e.loaded, e.total)
    }
    xhr.onload = () => {
      let data: any = {}
      try { data = JSON.parse(xhr.responseText) } catch { /* 非 JSON 响应走下面的兜底 */ }
      if (xhr.status >= 200 && xhr.status < 300) {
        resolve({ path: data.path ?? path, size: data.size ?? file.size })
      } else {
        // 上传走的是 XHR（为了进度条），绕开了 apiFetch —— 所以 401 要在这里单独接一下，
        // 否则令牌失效时上传只会报一句"上传失败（HTTP 401）"，界面不会回到登录页。
        if (xhr.status === 401) notifyAuthExpired(data.error || '登录状态已失效，请重新登录')
        reject(new Error(data.error || `上传失败（HTTP ${xhr.status}）`))
      }
    }
    xhr.onerror = () => reject(new Error('上传失败：网络错误（连接中断？）'))
    xhr.onabort = () => reject(new Error('已取消'))
    xhr.send(file)
  })
}

export async function writeFile(instanceId: string, path: string, content: string) {
  return apiFetch(`/api/instances/${instanceId}/file`, {
    method: 'POST',
    body: JSON.stringify({ path, content }),
  })
}

export async function deleteFile(instanceId: string, path: string) {
  return apiFetch(`/api/instances/${instanceId}/file?path=${encodeURIComponent(path)}`, { method: 'DELETE' })
}

export async function mkdir(instanceId: string, path: string) {
  return apiFetch(`/api/instances/${instanceId}/mkdir`, {
    method: 'POST',
    body: JSON.stringify({ path }),
  })
}

/** 重命名（仅文件名，不含目录） */
export async function renameFile(instanceId: string, path: string, newName: string) {
  return apiFetch(`/api/instances/${instanceId}/file/rename`, {
    method: 'POST',
    body: JSON.stringify({ path, new_name: newName }),
  })
}

/** 复制或移动（move = true 即「剪切 + 粘贴」） */
export async function copyFile(
  instanceId: string,
  src: string,
  dst: string,
  opts: { move?: boolean; overwrite?: boolean } = {},
) {
  return apiFetch(`/api/instances/${instanceId}/file/copy`, {
    method: 'POST',
    body: JSON.stringify({ src, dst, move: !!opts.move, overwrite: !!opts.overwrite }),
  })
}

/** 按名称递归搜索文件 */
export async function searchFiles(
  instanceId: string,
  keyword: string,
  path = '',
  limit = 200,
): Promise<{ keyword: string; files: FileItem[]; limit: number }> {
  const q = new URLSearchParams({ keyword, limit: String(limit) })
  if (path) q.set('path', path)
  return apiFetch(`/api/instances/${instanceId}/files/search?${q.toString()}`)
}

/**
 * 下载地址。
 *
 * 走查询参数传令牌：下载由浏览器直接发起（<a href> / window.open），
 * 无法附带 Authorization 头。这是面板里唯一允许这样传令牌的接口。
 */
export function downloadUrl(instanceId: string, path: string): string {
  const q = new URLSearchParams({ path, token: getToken() || '' })
  return `/api/instances/${instanceId}/download?${q.toString()}`
}

/**
 * 取文件内容为 Blob（用于页面内预览，例如图片）。
 *
 * 为什么不直接用 `downloadUrl` 放进 `<img src>`：那条路要**把令牌写进 URL**
 *（浏览器发起 <img> 时加不了请求头），而且下载接口返回的是
 * `Content-Disposition: attachment` + `application/octet-stream` + `nosniff`，
 * 浏览器会按"未知二进制"处理、拒绝当图片渲染（nosniff 明确禁止嗅探）。
 *
 * 这条走的是带 Authorization 头的正常请求，取回字节后由调用方
 * `URL.createObjectURL` 成临时地址 —— 令牌不进 URL、不落浏览器历史与日志。
 * 用完记得 `URL.revokeObjectURL`，否则这块内存会一直挂着。
 */
export async function fetchFileBlob(instanceId: string, path: string): Promise<Blob> {
  const headers: Record<string, string> = {}
  const token = getToken()
  if (token) headers['Authorization'] = `Bearer ${token}`
  const q = new URLSearchParams({ path })
  const resp = await fetch(`/api/instances/${instanceId}/download?${q.toString()}`, { headers })
  if (!resp.ok) {
    // 错误响应是 JSON（{error: "..."}），解析出来给人看
    const data = await resp.json().catch(() => ({} as any))
    throw new Error(data.error || `HTTP ${resp.status}`)
  }
  return resp.blob()
}

// ---- 排队任务（压缩 / 解压，走节点公共资源） ----

export interface FileJob {
  job_id: string
  instance_id: string
  kind: 'compress' | 'extract'
  src: string
  dst: string
  format: string
  state: 'queued' | 'running' | 'success' | 'failed' | 'canceled'
  progress: number
  message: string
  error: string
  total_bytes: number
  done_bytes: number
  created_by: string
  created_at: string
  started_at: string
  finished_at: string
}

export async function listJobs(instanceId: string): Promise<FileJob[]> {
  return apiFetch(`/api/instances/${instanceId}/jobs`)
}

export async function createJob(
  instanceId: string,
  payload: { kind: 'compress' | 'extract'; src: string; dst: string; format?: string },
): Promise<{ job_id: string; message: string }> {
  return apiFetch(`/api/instances/${instanceId}/jobs`, {
    method: 'POST',
    body: JSON.stringify(payload),
  })
}

export async function cancelJob(jobId: string): Promise<any> {
  return apiFetch(`/api/jobs/${encodeURIComponent(jobId)}`, { method: 'DELETE' })
}

// ---- 定时指令任务（开机 / 关机 / 重启 / 游戏指令） ----

export type TaskAction = 'start' | 'stop' | 'restart' | 'kill' | 'command'

export interface InstanceTask {
  id: number
  instance_id: string
  name: string
  action: TaskAction
  action_label: string
  command: string
  cron: string
  describe: string
  enabled: boolean
  next_run: string
  last_run: string
  last_state: string
  last_error: string
  run_count: number
  fail_count: number
  created_by: number
  creator: string
}

export async function listInstanceTasks(instanceId: string): Promise<InstanceTask[]> {
  return apiFetch(`/api/instances/${instanceId}/tasks`)
}

export async function createInstanceTask(
  instanceId: string,
  payload: { name?: string; action: TaskAction; command?: string; cron: string; enabled?: boolean },
): Promise<{ id: number; message: string }> {
  return apiFetch(`/api/instances/${instanceId}/tasks`, {
    method: 'POST',
    body: JSON.stringify(payload),
  })
}

export async function updateInstanceTask(
  id: number,
  payload: Partial<{ name: string; action: TaskAction; command: string; cron: string; enabled: boolean }>,
): Promise<any> {
  return apiFetch(`/api/tasks/${id}`, { method: 'PUT', body: JSON.stringify(payload) })
}

export async function deleteInstanceTask(id: number): Promise<any> {
  return apiFetch(`/api/tasks/${id}`, { method: 'DELETE' })
}

export async function runInstanceTask(id: number): Promise<any> {
  return apiFetch(`/api/tasks/${id}/run`, { method: 'POST' })
}

// ---- 监控 ----

export async function getMetrics(instanceId: string): Promise<Metrics> {
  return apiFetch(`/api/instances/${instanceId}/metrics`)
}

// ---- 账号管理 ----

export async function listUsers(): Promise<UserInfo[]> {
  return apiFetch('/api/users')
}

export interface UserInfo {
  id: number
  username: string
  role: string
  status: string
  created_at: string
}

export async function createUser(username: string, password: string, role: string) {
  return apiFetch('/api/users', {
    method: 'POST',
    body: JSON.stringify({ username, password, role }),
  })
}

export async function deleteUser(id: number) {
  return apiFetch(`/api/users/${id}`, { method: 'DELETE' })
}

/**
 * 改账号类型（仅总管理员）。
 *
 * 注意：角色写在令牌里，改完**不会立刻生效** —— 对方手上的旧令牌最长还能按
 * 旧角色用 24 小时。后端返回的 message 里已经写明这点，界面照原样展示即可，
 * 别自己拼一句"已生效"。
 */
export async function setUserRole(id: number, role: string) {
  return apiFetch(`/api/users/${id}`, {
    method: 'PUT',
    body: JSON.stringify({ role }),
  })
}

/**
 * 改用户名（本人可改自己，总管理员可改任何人）。
 *
 * 用户名不再是身份标识（身份是 UID），所以改名不影响实例授权、端口配额
 * 等任何关联 —— 后端返回的 message 里写明了这一点，界面照原样展示。
 */
export async function renameUser(id: number, username: string) {
  return apiFetch(`/api/users/${id}/username`, {
    method: 'PUT',
    body: JSON.stringify({ username }),
  })
}

/**
 * 改实例的显示名。
 *
 * 改的是面板数据库里的 name，**不是实例 ID**：ID 是目录名与所有关联的钥匙，
 * 改名不会动它，所以公网端口、授权、启动记录都不受影响。
 */
export async function renameInstance(instanceId: string, name: string) {
  return apiFetch(`/api/instances/${encodeURIComponent(instanceId)}`, {
    method: 'PUT',
    body: JSON.stringify({ name }),
  })
}

export async function changePassword(oldPassword: string, newPassword: string, targetUser?: string) {
  return apiFetch('/api/auth/change-password', {
    method: 'POST',
    body: JSON.stringify({ old_password: oldPassword, new_password: newPassword, target_user: targetUser }),
    // 旧密码错也是 401 —— 但那是"这次输入不对"，不是会话失效
    skipAuthExpired: true,
  })
}

/**
 * 当前登录用户（同步读取，登录时写入本地存储）。
 *
 * `id` 是 UID，也是**唯一不会变**的标识。
 *
 * 为什么必须有它：用户名可以改（PUT /api/users/{id}/username），一旦
 * 前端还拿用户名当身份用（"这行是不是我自己"、按用户名匹配授权），
 * 用户改完名就会出现"认不出自己"这类怪现象 —— 比如把自己显示成普通用户、
 * 或者在对自己的账号点删除时不再拦住。凡是"记住这个人是谁"的地方，
 * 一律比 id，不要比 username。
 *
 * 老版本浏览器里可能存着没有 id 的登录态（登录时后端还没返回 id），
 * 所以它是可选的：读不到就回退到按用户名比较，等下次登录自然补齐。
 */
export interface User {
  id?: number
  username: string
  role: string
}

export function currentUser(): User | null {
  const u = localStorage.getItem(USER_KEY)
  if (!u) return null
  try {
    return JSON.parse(u)
  } catch {
    return null
  }
}

/**
 * 登录态变化事件。
 *
 * 为什么要广播：改自己的用户名之后，右上角/左下角显示的名字来自本地登录态，
 * 而那条链路（App → AppShell）是另一棵组件树，够不着账户页里的这次修改。
 * 用一个 window 事件把"登录态变了"这件事说出去，比把 setState 回调
 * 从 App 一路传进账户页更省事，也不会因为将来多一处入口（比如管理员
 * 在用户页改了自己）就漏掉刷新。
 */
export const USER_EVENT = 'atlmcpanel:user-changed'

export function setCurrentUser(id: number | undefined, username: string, role: string) {
  localStorage.setItem(USER_KEY, JSON.stringify({ id, username, role }))
  resetAuthExpiredNotice()   // 新会话：把"已提示过过期"的标记重置
  window.dispatchEvent(new Event(USER_EVENT))
}

/**
 * 只更新本地登录态里的用户名（改完自己的名字后调用）。
 *
 * 令牌里带的用户名是签发时的旧值且**不会**随之改变 —— 但这不影响任何权限
 * （后端鉴权只用 uid），所以这里只需把界面上的显示名刷新过来即可，
 * 不必强制重新登录。
 */
export function updateCurrentUsername(username: string) {
  const u = currentUser()
  if (!u) return
  setCurrentUser(u.id, username, u.role)
}

/**
 * 当前用户是不是指定的那个账号。
 *
 * 优先比 UID：用户名可以改，UID 改不了。只有当本地登录态里没有 id
 * （旧版本存下来的）时才退回按用户名比较。
 */
export function isCurrentUser(u: { id?: number; username: string }): boolean {
  const me = currentUser()
  if (!me) return false
  if (me.id != null && u.id != null) return me.id === u.id
  return me.username === u.username
}
// ---- 实例运行数据（运行时长 / 启停次数 / 磁盘 / 网络） ----

export interface InstanceRuntime {
  instance_id: string
  status: string
  uptime_seconds: number
  last_start_at: number
  start_count: number
  stop_count: number
  disk_used: number
  disk_total: number
  disk_free: number
  net_rx_rate: number
  net_tx_rate: number
  net_rx_total: number
  net_tx_total: number
  /**
   * 该连的对外地址（域名 + 端口）。端口由后端按这条隧道实际用的 remote_port 自动拼上，
   * 管理员只填域名即可 —— 见 publicAddress。
   */
  public_address: string
  /** node = 节点整机聚合；instance = 该实例隧道精确值 */
  net_scope?: string
  /** JDK 回退说明（空 = 正常）：选了 17、节点上却没有，实际用了 PATH 上的哪个 */
  java_note?: string
  /** cgroup CPU 配额 / 内存上限应用失败的原因（空 = 正常） */
  limit_warning?: string
}

export async function getInstanceRuntime(instanceId: string): Promise<InstanceRuntime> {
  return apiFetch(`/api/instances/${instanceId}/runtime`)
}

// ---- 用户资料与头像 ----

export interface MyProfile {
  id: number
  username: string
  role: string
  status: string
  registered_at: string
  total_online_seconds: number
  avatar_status: 'none' | 'pending' | 'approved' | 'rejected'
  avatar_note: string
  avatar_url: string
  avatar_reviewed_at: string
}

export async function getMyProfile(): Promise<MyProfile> {
  return apiFetch('/api/auth/me')
}

/** 上传头像（multipart）。上传后进入待审核状态。 */
export async function uploadAvatar(file: File): Promise<any> {
  const fd = new FormData()
  fd.append('avatar', file)
  const token = getToken()
  const res = await fetch('/api/my/avatar', {
    method: 'POST',
    headers: token ? { Authorization: `Bearer ${token}` } : {},
    body: fd,
  })
  const text = await res.text()
  let data: any = {}
  try { data = JSON.parse(text) } catch { /* 非 JSON */ }
  if (!res.ok) throw new Error(data.error || `上传失败（${res.status}）`)
  return data
}

export async function deleteMyAvatar(): Promise<any> {
  return apiFetch('/api/my/avatar', { method: 'DELETE' })
}

export interface AvatarReviewItem {
  user_id: number
  username: string
  role: string
  avatar_status: string
  avatar_note: string
  preview_url: string
  reviewed_at: string
  reviewed_by_name: string
}

export async function listAvatars(status = 'pending'): Promise<AvatarReviewItem[]> {
  return apiFetch(`/api/admin/avatars?status=${status}`)
}

/**
 * 待审头像的图片地址。
 *
 * 必须带 ?token=：这是由 `<img src>` 直接发起的请求，浏览器**不会**附加
 * Authorization 头，不带令牌就会被鉴权中间件挡成 401 —— 表现就是管理员
 * 审核页上永远是裂图。这也是面板里仅有的两个允许用查询参数传令牌的接口之一
 *（另一个是实例文件下载）。
 */
export function avatarRawUrl(userId: number): string {
  return `/api/admin/avatars/${userId}/raw?token=${encodeURIComponent(getToken() || '')}`
}

export async function reviewAvatar(userID: number, approve: boolean, note = ''): Promise<any> {  return apiFetch(`/api/admin/avatars/${userID}/review`, {
    method: 'POST',
    body: JSON.stringify({ approve, note }),
  })
}

/**
 * 实例图标的图片地址（实例目录里的 server-icon.png / icon.png）。
 *
 * - 必须带 ?token=：同头像预览，`<img src>` 带不上 Authorization 头
 * - `?v=<icon_mtime>` 做缓存击穿：后端给的是 `max-age`，图标换了 mtime 就变，
 *   URL 一变浏览器自然会重新取
 * - `icon_mtime` 为 0（没有图标）时返回空串 —— 调用方据此跳过请求、
 *   直接显示首字母，免得列表里每个实例都发一次注定 404 的请求
 */
export function instanceIconUrl(inst: { instance_id: string; icon_mtime?: number }): string {
  if (!inst.icon_mtime) return ''
  const q = new URLSearchParams({
    v: String(inst.icon_mtime),
    token: getToken() || '',
  })
  return `/api/instances/${inst.instance_id}/icon?${q.toString()}`
}

// ---- 指标历史（统计页） ----

export interface MetricsPoint {
  cpu: number
  mem: number
  players: number
  tps: number
  at: string
}

export interface StatsResponse {
  instance_id: string
  hours: number
  step: number
  points: MetricsPoint[]
  total: number
  avg: { cpu?: number; mem?: number; tps?: number; players?: number }
  peak: { cpu?: number; mem?: number; players?: number }
}

export async function getInstanceStats(instanceId: string, hours = 24): Promise<StatsResponse> {
  return apiFetch(`/api/instances/${instanceId}/stats?hours=${hours}`)
}
