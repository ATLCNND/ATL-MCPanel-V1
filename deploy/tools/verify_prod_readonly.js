#!/usr/bin/env node
/**
 * 生产机「只读」核验 —— 绝不做任何写操作
 *
 * 用途：在一台**跑着真实 MC 实例**的生产机上核验控制面是否正常。
 * 这个脚本只发 GET 请求（外加一次登录），不启动/停止/备份/回滚任何实例，
 * 不触发存档落盘，因此对玩家没有可感知影响。
 *
 * 用法：
 *   PROD_BASE=https://panel.example.com PROD_USER=admin PROD_PASS=*** \
 *   node verify_prod_readonly.js
 *
 *   # 自签证书的内网面板（跳过 TLS 校验，只用于内网自查）
 *   NODE_TLS_REJECT_UNAUTHORIZED=0 PROD_BASE=https://panel.example.com:8443 ...
 *
 * 退出码：0 = 全绿；1 = 有失败项（每项都会打印原因）。
 */
const BASE = (process.env.PROD_BASE || '').replace(/\/+$/, '');
const USER = process.env.PROD_USER || 'admin';
const PASS = process.env.PROD_PASS || '';
const TIMEOUT = Number(process.env.PROD_TIMEOUT_MS || 15000);

if (!BASE) { console.error('缺少 PROD_BASE（例：PROD_BASE=https://panel.example.com）'); process.exit(2); }
if (!PASS) { console.error('缺少 PROD_PASS'); process.exit(2); }

let token = '';
const fails = [];
const warns = [];
const ok = (m) => console.log('  ✅ ' + m);
const bad = (m) => { fails.push(m); console.log('  ❌ ' + m); };
const warn = (m) => { warns.push(m); console.log('  ⚠️  ' + m); };

async function call(method, path, body) {
  const ctl = new AbortController();
  const t = setTimeout(() => ctl.abort(), TIMEOUT);
  try {
    const r = await fetch(BASE + path, {
      method,
      headers: { 'Content-Type': 'application/json', ...(token ? { Authorization: 'Bearer ' + token } : {}) },
      body: body === undefined ? undefined : JSON.stringify(body),
      signal: ctl.signal,
    });
    const text = await r.text();
    let data; try { data = text ? JSON.parse(text) : null; } catch { data = text; }
    return { status: r.status, data };
  } finally { clearTimeout(t); }
}

(async () => {
  console.log(`目标：${BASE}`);
  console.log('=== 1. 健康检查（不鉴权）===');
  let r = await call('GET', '/api/health');
  r.status === 200 && r.data?.status === 'ok' ? ok('/api/health 200 status=ok') : bad(`/api/health ${r.status} ${JSON.stringify(r.data).slice(0, 120)}`);

  console.log('=== 2. 登录 ===');
  r = await call('POST', '/api/auth/login', { username: USER, password: PASS });
  token = r.data?.token || r.data?.access_token || '';
  token ? ok(`登录成功（用户 ${USER}）`) : bad(`登录失败 ${r.status} ${JSON.stringify(r.data).slice(0, 160)}`);
  if (!token) { finish(); return; }

  console.log('=== 3. 实例清单（只读）===');
  r = await call('GET', '/api/instances');
  const insts = Array.isArray(r.data) ? r.data : [];
  if (r.status !== 200) bad(`/api/instances ${r.status}`);
  else {
    ok(`实例 ${insts.length} 个`);
    const byT = {};
    for (const i of insts) { const k = i.template || '(空)'; byT[k] = (byT[k] || 0) + 1; }
    console.log('    类型分布：' + JSON.stringify(byT));
    const running = insts.filter((i) => (i.live_status || i.status) === 'running');
    console.log(`    运行中：${running.length} 个` + (running.length ? '（' + running.map((i) => i.instance_id).join(', ') + '）' : ''));
    const unknown = insts.filter((i) => (i.live_status || i.status) === 'unknown');
    unknown.length ? warn(`${unknown.length} 个实例状态 unknown（节点不可达或 node_id 指向不存在的节点）：${unknown.map((i) => i.instance_id).join(', ')}`)
                   : ok('没有 unknown 状态的实例');
    const noCap = insts.filter((i) => !Array.isArray(i.capabilities) || i.capabilities.length === 0);
    noCap.length ? bad(`${noCap.length} 个实例没有 capabilities 字段（面板版本过旧？）：${noCap.map((i) => i.instance_id).join(', ')}`)
                 : ok('所有实例都带 capabilities');
  }

  console.log('=== 4. 节点（只读）===');
  r = await call('GET', '/api/nodes');
  const nodes = Array.isArray(r.data) ? r.data : [];
  if (r.status !== 200) bad(`/api/nodes ${r.status}`);
  else {
    for (const n of nodes) {
      const line = `${n.name} id=${n.id} status=${n.status} online=${n.online} grpc_port=${n.daemon_grpc_port} ssh_user='${n.ssh_user || ''}' has_auth=${n.has_auth}`;
      // 单个节点离线只算警告：生产机上可能有刻意下线的节点，也可能是面板里的历史测试节点。
      // 只有「一个在线节点都没有」才是真故障（那样任何实例操作都会失败）。
      n.online ? ok(line) : warn(line + '  ← 这个节点不在线');
      if (n.online && !n.has_auth && !n.ssh_user) ok(`    ${n.name} 没有 SSH 凭据（凭证交换接入的正确形态）`);
    }
    nodes.some((n) => n.online) ? ok(`在线节点 ${nodes.filter((n) => n.online).length}/${nodes.length}`)
                                : bad('没有任何节点在线');
  }

  console.log('=== 5. 运行中实例的资源视图（只读）===');
  const runningIds = insts.filter((i) => (i.live_status || i.status) === 'running').map((i) => i.instance_id);
  for (const id of runningIds.slice(0, 5)) {
    const rt = await call('GET', `/api/instances/${id}/runtime`);
    rt.status === 200 ? ok(`${id} runtime 200`) : warn(`${id} runtime ${rt.status}`);
  }
  if (!runningIds.length) warns.push('没有运行中的实例');

  console.log('=== 6. 控制面其余读接口 ===');
  const probes = [
    ['/api/alerts', '告警'],
    ['/api/tls/status', 'TLS 状态'],
    ['/api/nodes/local', '本机部署能力'],
    ['/api/users', '用户'],
    ['/api/panel-backups', '面板自身备份'],
    ['/api/audit-logs?limit=1', '审计日志'],
  ];
  for (const [p, label] of probes) {
    const rr = await call('GET', p);
    if (rr.status === 200) ok(`${label} ${p} 200`);
    else bad(`${label} ${p} ${rr.status} ${JSON.stringify(rr.data).slice(0, 100)}`);
  }

  console.log('=== 7. 前端静态资源 ===');
  const ui = await call('GET', '/');
  typeof ui.data === 'string' && /<!DOCTYPE html>/i.test(ui.data)
    ? ok('首页 200（HTML 正常返回）')
    : warn(`首页 ${ui.status}（可能启用了安全入口：请用 /<入口>/ 访问）`);

  finish();
})().catch((e) => { console.error('EXCEPTION', e); process.exit(1); });

function finish() {
  console.log('');
  if (warns.length) console.log(`WARN ${warns.length}: ${warns.join(' | ')}`);
  // PROD_STRICT=1：把警告也当失败（做验收留证时用，避免"警告里藏着问题"）
  const strict = process.env.PROD_STRICT === '1';
  const badCount = fails.length + (strict ? warns.length : 0);
  console.log(badCount === 0 ? 'READONLY_ALL_GREEN' : `READONLY_FAILED ${badCount}: ${fails.concat(strict ? warns : []).join(' | ')}`);
  process.exit(badCount === 0 ? 0 : 1);
}
