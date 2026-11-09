#!/usr/bin/env node
/**
 * 生产机「安全写操作」演示 —— 只碰它自己创建的那个一次性实例
 *
 * 生产机的红线：**不得干扰正在跑的 MC 实例与玩家**。所以本脚本：
 *   1. 必须显式打开开关才会写：ALLOW_WRITES=1
 *   2. 只操作 instance_id 以 DEMO_PREFIX（默认 pv-demo-）开头的实例，
 *      且该实例必须是**本次运行新建的**（带时间戳，天然不会撞上别人的）
 *   3. 绝不 stop/restart/backup/restore 任何其它实例；不做 daemon 重启
 *   4. 结束时删掉自己建的实例（remove_files=true），不留垃圾
 *   5. 全程打印每条写操作，便于事后审计
 *
 * 用法：
 *   ALLOW_WRITES=1 PROD_BASE=https://panel.example.com PROD_USER=admin PROD_PASS=*** \
 *   DEMO_NODE_ID=3 DEMO_PORT=28301 node demo_prod_safe.js
 *
 *   可选：左括号 DEMO_TEMPLATE=docker 时改测 Docker 类型（需要该节点有容器运行时）
 * 退出码：0 = 全绿并已清理；1 = 有失败（实例会被强制清理后再退出）。
 */
const BASE = (process.env.PROD_BASE || '').replace(/\/+$/, '');
const USER = process.env.PROD_USER || 'admin';
const PASS = process.env.PROD_PASS || '';
const NODE_ID = Number(process.env.DEMO_NODE_ID || 0);
const PORT = Number(process.env.DEMO_PORT || 0);
const PREFIX = process.env.DEMO_PREFIX || 'pv-demo-';
const TEMPLATE = (process.env.DEMO_TEMPLATE || 'console').toLowerCase();

if (!BASE || !PASS) { console.error('缺少 PROD_BASE / PROD_PASS'); process.exit(2); }
if (process.env.ALLOW_WRITES !== '1') {
  console.error('拒绝执行：本脚本会在生产机上创建/停止/删除实例。确认只影响一次性实例后，加 ALLOW_WRITES=1 再跑。');
  process.exit(2);
}
if (!NODE_ID) { console.error('缺少 DEMO_NODE_ID（面板里该节点的 id，看 GET /api/nodes）'); process.exit(2); }
if (!PORT) { console.error('缺少 DEMO_PORT（一段**未被占用**的端口，别用 MC 在用的）'); process.exit(2); }

const INST = PREFIX + Date.now().toString(36);
if (!INST.startsWith(PREFIX)) { console.error('内部错误：实例 ID 前缀不符'); process.exit(2); }
if (!/^[a-z0-9][a-z0-9-]{2,30}$/.test(INST)) { console.error('实例 ID 不合规: ' + INST); process.exit(2); }

let token = '';
const fails = [];
const ok = (m) => console.log('  ✅ ' + m);
const bad = (m) => { fails.push(m); console.log('  ❌ ' + m); };
const write = (m) => console.log('  ✍️  ' + m + '   [写操作]');
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

async function call(method, path, body) {
  const r = await fetch(BASE + path, {
    method,
    headers: { 'Content-Type': 'application/json', ...(token ? { Authorization: 'Bearer ' + token } : {}) },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  const text = await r.text();
  let data; try { data = text ? JSON.parse(text) : null; } catch { data = text; }
  return { status: r.status, data };
}

function guard(id) {
  if (!String(id).startsWith(PREFIX)) throw new Error(`拒绝操作非本脚本创建的实例: ${id}`);
  return id;
}

async function findInst() {
  const r = await call('GET', '/api/instances');
  return (Array.isArray(r.data) ? r.data : []).find((i) => i.instance_id === INST);
}

let created = false;
let started = false;
async function cleanup() {
  try {
    if (started) { write(`停止 ${INST}（清理）`); await call('POST', `/api/instances/${guard(INST)}/stop`); await sleep(1500); }
    if (created) { write(`删除 ${INST}（清理，含文件）`); await call('DELETE', `/api/instances/${guard(INST)}?remove_files=true`); await sleep(1000); }
    const left = await findInst();
    left ? bad('清理后实例仍在列表里: ' + INST) : ok('已清理干净');
  } catch (e) { console.error('清理异常', e); }
}

(async () => {
  console.log(`面板：${BASE}`);
  console.log(`演示实例：${INST}（节点 ${NODE_ID}，类型 ${TEMPLATE}，端口 ${PORT}）`);
  console.log('红线：只动这个实例，不重启 daemon、不碰任何 MC 实例\n');

  let r = await call('POST', '/api/auth/login', { username: USER, password: PASS });
  token = r.data?.token || r.data?.access_token || '';
  if (!token) { console.error('登录失败 ' + r.status); process.exit(1); }
  ok('登录成功');

  console.log('=== 1. 前置核对（这条节点必须在线）===');
  r = await call('GET', '/api/nodes');
  const node = (Array.isArray(r.data) ? r.data : []).find((n) => Number(n.id) === NODE_ID);
  if (!node) { console.error(`节点 ${NODE_ID} 不存在`); process.exit(1); }
  if (!node.online) { bad(`节点 ${node.name} 不在线`); process.exit(1); }
  ok(`节点 ${node.name} online`);
  r = await call('GET', '/api/instances');
  const busy = (Array.isArray(r.data) ? r.data : []).filter((i) => Number(i.port) === PORT);
  if (busy.length) { bad(`端口 ${PORT} 已被实例 ${busy[0].instance_id} 占用，换一个 DEMO_PORT`); process.exit(1); }
  ok(`端口 ${PORT} 未被占用`);

  console.log('=== 2. 建演示实例 ===');
  const body = { node_id: NODE_ID, instance_id: INST, name: '运维核验·临时', template: TEMPLATE, port: PORT };
  if (TEMPLATE === 'console') { body.max_mem = '256M'; body.min_mem = '128M'; body.cpu_quota = 50; }
  write(`创建 ${INST}`);
  r = await call('POST', '/api/instances', body);
  if (r.status !== 200 && r.status !== 201) { bad(`创建失败 ${r.status} ${JSON.stringify(r.data)}`); await cleanup(); finish(); return; }
  created = true;
  ok('创建成功');

  let inst = await findInst();
  inst?.template === TEMPLATE ? ok(`template=${inst.template} capabilities=${(inst.capabilities || []).length}`)
                              : bad(`template=${inst?.template}`);

  console.log('=== 3. 写 start.sh 并启动 ===');
  const sh = '#!/bin/sh\necho "prod demo ready"\nwhile read -r line; do echo "got: $line"; done\n';
  write('写 start.sh');
  r = await call('POST', `/api/instances/${guard(INST)}/file`, { path: 'start.sh', content: sh });
  (r.status === 200 || r.status === 201) ? ok('start.sh 已写入') : bad(`写 start.sh ${r.status}`);

  write(`启动 ${INST}`);
  r = await call('POST', `/api/instances/${guard(INST)}/start`);
  started = r.status === 200;
  started ? ok('start 200') : bad(`start ${r.status} ${JSON.stringify(r.data)}`);
  for (let i = 0; i < 20 && started; i++) {
    await sleep(1000);
    const cur = await findInst();
    if ((cur?.live_status || cur?.status) === 'running') { ok('实例 running'); break; }
  }

  console.log('=== 4. 控制台（只连自己这个实例）===');
  const WS = BASE.replace(/^http/, 'ws');
  const buf = await new Promise((resolve) => {
    let acc = '';
    let ws;
    try { ws = new WebSocket(`${WS}/ws/console/${INST}?token=${encodeURIComponent(token)}`); } catch { resolve(''); return; }
    const t = setTimeout(() => { try { ws.close(); } catch {} resolve(acc); }, 6000);
    ws.onopen = () => { try { ws.send('echo prod-demo-ok\n'); } catch {} };
    ws.onmessage = (ev) => { try { const f = JSON.parse(ev.data); if (f.type === 'line' && f.data) acc += f.data; } catch { acc += String(ev.data); } };
    void t;
  });
  /prod demo ready/.test(buf) ? ok('收到启动输出') : bad('控制台无启动输出');
  /got: echo prod-demo-ok/.test(buf) ? ok('stdin 回显正常') : bad('stdin 无回显');

  console.log('=== 5. 文件接口 ===');
  r = await call('GET', `/api/instances/${guard(INST)}/files?path=/`);
  const fl = (r.data && r.data.files) || [];
  fl.length ? ok(`文件 ${fl.length} 项`) : bad('文件列表为空');

  console.log('=== 6. 备份 + 回滚（只对演示实例）===');
  write('创建备份');
  r = await call('POST', `/api/instances/${guard(INST)}/backups`, { name: '运维核验', include_config: false });
  (r.status === 200 || r.status === 201) ? ok('备份已创建') : bad(`备份失败 ${r.status} ${JSON.stringify(r.data)}`);
  await sleep(1500);
  r = await call('GET', `/api/instances/${guard(INST)}/backups`);
  const list = Array.isArray(r.data) ? r.data : [];
  list.length ? ok(`备份清单 ${list.length} 条（kind=${list.map((b) => b.kind).join(',')}）`) : bad('备份清单为空');

  if (list.length) {
    write('改 note.txt 为 v2');
    await call('POST', `/api/instances/${guard(INST)}/file`, { path: 'note.txt', content: 'v1\n' });
    write('再次备份');
    await call('POST', `/api/instances/${guard(INST)}/backups`, { name: '运维核验2', include_config: false });
    await sleep(1500);
    await call('POST', `/api/instances/${guard(INST)}/file`, { path: 'note.txt', content: 'v2\n' });

    write(`停止 ${INST}（回滚要求停机）`);
    await call('POST', `/api/instances/${guard(INST)}/stop`);
    started = false;
    for (let i = 0; i < 15; i++) { await sleep(1000); const cur = await findInst(); if ((cur?.status || '') === 'stopped') break; }

    const target = list[0];
    write(`从 ${target.backup_id} 回滚`);
    r = await call('POST', `/api/instances/${guard(INST)}/restore`, { backup_id: target.backup_id });
    r.status === 200 ? ok('回滚 200: ' + (r.data?.message || '')) : bad(`回滚 ${r.status} ${JSON.stringify(r.data)}`);
  }

  console.log('=== 7. 清理（删掉演示实例）===');
  await cleanup();

  console.log('');
  console.log(fails.length === 0 ? 'PROD_DEMO_ALL_GREEN' : `PROD_DEMO_FAILED ${fails.length}: ${fails.join(' | ')}`);
  process.exit(fails.length === 0 ? 0 : 1);
})().catch(async (e) => { console.error('EXCEPTION', e); await cleanup(); process.exit(1); });

function finish() {
  console.log('');
  console.log(fails.length === 0 ? 'PROD_DEMO_ALL_GREEN' : `PROD_DEMO_FAILED ${fails.length}: ${fails.join(' | ')}`);
  process.exit(fails.length === 0 ? 0 : 1);
}
