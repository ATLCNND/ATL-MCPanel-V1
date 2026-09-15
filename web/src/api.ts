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

export function getToken(): string | null {
  return localStorage.getItem(TOKEN_KEY)
}

export function setToken(token: string) {
  localStorage.setItem(TOKEN_KEY, token)
}

export function clearToken() {
  localStorage.removeItem(TOKEN_KEY)
}

export async function apiFetch(path: string, options: RequestInit = {}): Promise<any> {
  const headers: Record<string, string> = {
    'Content-Type': 'application/json',
    ...(options.headers as Record<string, string>),
  }
  const token = getToken()
  if (token) headers['Authorization'] = `Bearer ${token}`

  const resp = await fetch(path, { ...options, headers })
  const data = await resp.json().catch(() => ({}))
  if (!resp.ok) {
    throw new Error(data.error || `HTTP ${resp.status}`)
  }
  return data
}

// ---- 接口封装 ----

export async function login(username: string, password: string) {
  return apiFetch('/api/auth/login', {
    method: 'POST',
    body: JSON.stringify({ username, password }),
  })
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
  status: string
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
  status: string
  cpu: number
  mem: number
  last_seen: string
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

export async function setWhitelist(instanceId: string, enabled: boolean): Promise<any> {
  return apiFetch(`/api/instances/${instanceId}/whitelist`, {
    method: 'POST',
    body: JSON.stringify({ enabled }),
  })
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

export async function changePassword(oldPassword: string, newPassword: string, targetUser?: string) {
  return apiFetch('/api/auth/change-password', {
    method: 'POST',
    body: JSON.stringify({ old_password: oldPassword, new_password: newPassword, target_user: targetUser }),
  })
}

/** 当前登录用户（同步读取，登录时写入本地存储） */
export interface User {
  username: string
  role: string
}

export function currentUser(): User | null {
  const u = localStorage.getItem('atlmcpanel_user')
  if (!u) return null
  try {
    return JSON.parse(u)
  } catch {
    return null
  }
}

export function setCurrentUser(username: string, role: string) {
  localStorage.setItem('atlmcpanel_user', JSON.stringify({ username, role }))
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
  display_domain: string
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
