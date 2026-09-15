import { useEffect, useState } from 'react'
import {
  FrpsServer, Tunnel, Instance, PanelTunnelState,
  listFrps, createFrps, deleteFrps, updateFrps,
  listTunnels, createTunnel, deleteTunnel, reapplyTunnel,
  listInstances, getPanelTunnel, setPanelTunnel,
  testFrps, testFrpsSaved, FrpsTestResult,
} from '../api'
import './TunnelsPage.css'

export default function TunnelsPage() {
  const [frpsList, setFrpsList] = useState<FrpsServer[]>([])
  const [tunnels, setTunnels] = useState<Tunnel[]>([])
  const [instances, setInstances] = useState<Instance[]>([])
  const [error, setError] = useState('')
  const [msg, setMsg] = useState('')
  const [busy, setBusy] = useState(false)

  // 面板自身穿透
  const [panel, setPanel] = useState<PanelTunnelState | null>(null)

  // 新建 frps 表单
  const [fName, setFName] = useState('')
  const [fHost, setFHost] = useState('')
  const [fBindPort, setFBindPort] = useState('7000')
  const [fToken, setFToken] = useState('')
  const [fStart, setFStart] = useState('25565')
  const [fEnd, setFEnd] = useState('25600')
  const [fDomain, setFDomain] = useState('')

  // 线路「对外域名」的就地编辑草稿：{ frpsId: 当前输入值 }
  //
  // 为什么不直接改 frpsList 里的值：那样"输入到一半"的状态会污染已保存的数据，
  // 用户改错了想放弃也回不去。草稿与已保存值分开，取消时直接丢掉草稿即可。
  const [domainDraft, setDomainDraft] = useState<Record<number, string>>({})
  const [domainBusy, setDomainBusy] = useState<number | null>(null)

  // 线路自检结果。
  //
  // 用 key 区分来源：'form' = 尚未保存的新线路（测表单里填的值），
  // 数字 = 已保存线路的 id。这样"加线路前先测一次"和"回头复测某条线路"
  // 两种场景共用一套 UI，且结果不会互相覆盖。
  const [testResult, setTestResult] = useState<Record<string, FrpsTestResult>>({})
  const [testing, setTesting] = useState<string | null>(null)

  // 新建隧道表单
  const [tInstance, setTInstance] = useState('')
  const [tFrps, setTFrps] = useState('')
  const [tProtocol, setTProtocol] = useState('tcp')
  const [tLocalPort, setTLocalPort] = useState('')
  const [tRemotePort, setTRemotePort] = useState('')
  const [tName, setTName] = useState('')
  // 对外展示的域名（可含端口）。填了之后实例页的「公网域名」会显示它，
  // 而不是 IP:端口 —— 玩家看到的地址更友好。
  const [tDomain, setTDomain] = useState('')

  const load = async () => {
    try {
      const [f, t, i, p] = await Promise.all([listFrps(), listTunnels(), listInstances(), getPanelTunnel()])
      const frps = Array.isArray(f) ? f : []
      setFrpsList(frps)
      setTunnels(Array.isArray(t) ? t : [])
      setInstances(Array.isArray(i) ? i : [])
      setPanel(p)
      // 重新载入时把草稿重置为已保存值：否则界面上会留着改了一半的内容，
      // 用户以为已经生效了
      setDomainDraft(Object.fromEntries(frps.map((x) => [x.id, x.display_domain || ''])))
    } catch (e: any) {
      setError(e.message)
    }
  }

  useEffect(() => { load() }, [])

  // 面板穿透表单本地状态
  const pc = panel?.config
  const updPanel = (patch: Partial<NonNullable<PanelTunnelState['config']>>) => {
    if (!panel) return
    setPanel({ ...panel, config: { ...panel.config, ...patch } })
  }

  const savePanel = async () => {
    if (!pc) return
    setBusy(true); setError(''); setMsg('')
    try {
      const r = await setPanelTunnel(pc)
      setMsg(r.message || '面板穿透配置已保存')
      await load()
    } catch (e: any) {
      setError(e.message)
    } finally {
      setBusy(false)
    }
  }

  const submitFrps = async () => {
    setBusy(true); setError(''); setMsg('')
    try {
      await createFrps({
        name: fName, host: fHost,
        bind_port: Number(fBindPort) || 7000,
        token: fToken,
        port_start: Number(fStart) || 25565,
        port_end: Number(fEnd) || 25600,
        display_domain: fDomain.trim(),
      })
      setMsg(`frps 服务器「${fName}」已添加`)
      setFName(''); setFHost(''); setFToken(''); setFDomain('')
      await load()
    } catch (e: any) {
      setError(e.message)
    } finally {
      setBusy(false)
    }
  }

  /**
   * 线路自检。
   *
   * key='form' 时测表单里当前填的值（**保存之前**就能查出来地址/token/端口段对不对），
   * 传 frps 对象时测已保存的那条。
   *
   * 注意自检是在**节点**上跑的：真正跑 frpc 的是节点，面板能连通 ≠ 节点能连通。
   */
  const runTest = async (key: string, cfg: Partial<FrpsServer>) => {
    setTesting(key); setError(''); setMsg('')
    try {
      const r = cfg.id
        ? await testFrpsSaved(cfg.id)
        : await testFrps({
            host: (cfg.host || '').trim(),
            bind_port: Number(cfg.bind_port) || 7000,
            token: cfg.token || '',
            port_start: Number(cfg.port_start) || 25565,
            port_end: Number(cfg.port_end) || 25600,
          })
      setTestResult((m) => ({ ...m, [key]: r }))
    } catch (e: any) {
      setError(e.message)
      setTestResult((m) => {
        const next = { ...m }
        delete next[key]
        return next
      })
    } finally {
      setTesting(null)
    }
  }

  /** 自检结果面板：分两层说明，失败时给出 frpc 日志尾部。 */
  const renderTest = (key: string) => {
    const r = testResult[key]
    if (!r) return null
    return (
      <div className={`frps-test ${r.success ? 'ok' : 'bad'}`}>
        <div className="frps-test-head">
          {r.success
            ? `✓ 线路可用（在节点「${r.node_name || r.node_id}」上实测）`
            : `✗ 线路不可用：${r.error || '未知原因'}`}
        </div>
        <ul className="frps-test-steps">
          <li>
            {r.dial_ok ? '✓' : '✗'} TCP 连接 <code className="mono">{r.address}</code>
            {r.dial_ok ? ` 成功（${r.dial_ms} ms）` : ` 失败：${r.dial_error || '不可达'}`}
          </li>
          <li>
            {r.register_ok ? '✓' : '✗'} frpc 注册
            {r.register_ok
              ? `成功，试用了端口 ${r.used_port}（已释放）`
              : `失败：${r.register_error || '未能完成注册'}`}
          </li>
        </ul>
        {r.log_tail && !r.register_ok && (
          <details>
            <summary>frpc 日志尾部</summary>
            <pre className="frps-test-log">{r.log_tail}</pre>
          </details>
        )}
      </div>
    )
  }

  /** 保存某条线路的对外域名 */
  const saveDomain = async (f: FrpsServer) => {
    setDomainBusy(f.id); setError(''); setMsg('')
    try {
      const r = await updateFrps(f.id, { display_domain: domainDraft[f.id] ?? '' })
      setMsg(`线路「${f.name}」的对外域名已保存${r.display_domain ? '：' + r.display_domain : '（已清空）'}`)
      await load()
    } catch (e: any) {
      setError(e.message)
    } finally {
      setDomainBusy(null)
    }
  }

  const removeFrps = async (f: FrpsServer) => {
    if (!confirm(`确定删除 frps「${f.name}」？\n该服务器下的 ${f.used_ports} 条隧道也会一并删除。`)) return
    try {
      await deleteFrps(f.id)
      await load()
    } catch (e: any) {
      setError(e.message)
    }
  }

  const submitTunnel = async () => {
    setBusy(true); setError(''); setMsg('')
    try {
      const r = await createTunnel({
        instance_id: tInstance,
        frps_id: Number(tFrps),
        protocol: tProtocol,
        local_port: Number(tLocalPort) || 0,
        remote_port: Number(tRemotePort) || 0,
        name: tName,
        display_domain: tDomain.trim(),
      })
      if (r.status === 'running') {
        setMsg(`隧道已建立：${r.public_address}`)
      } else {
        setError(`隧道下发未成功：${r.message}`)
      }
      setTName(''); setTRemotePort(''); setTLocalPort(''); setTDomain('')
      await load()
    } catch (e: any) {
      setError(e.message)
    } finally {
      setBusy(false)
    }
  }

  const removeTunnel = async (t: Tunnel) => {
    if (!confirm(`确定删除隧道「${t.name}」？公网端口 ${t.remote_port} 将被释放。`)) return
    try {
      await deleteTunnel(t.id)
      await load()
    } catch (e: any) {
      setError(e.message)
    }
  }

  const reapply = async (t: Tunnel) => {
    setBusy(true); setError(''); setMsg('')
    try {
      const r = await reapplyTunnel(t.id)
      if (r.status === 'running') setMsg(`隧道「${t.name}」已重新下发`)
      else setError(`重新下发失败：${r.message}`)
      await load()
    } catch (e: any) {
      setError(e.message)
    } finally {
      setBusy(false)
    }
  }

  // 状态只看 Daemon 实时上报的 live_status。
  //
  // **不要**回退到 t.status：那是"最后一次下发的结果"，可能是几天前的，
  // 会把"节点不可达/问不到"显示成"运行中"。拿不到就如实显示 unknown。
  const statusOf = (t: Tunnel) => t.live_status || 'unknown'

  return (
    <div className="tunnels-page">
      <div className="page-toolbar">
        <button onClick={load}>刷新</button>
      </div>

      {error && <div className="tunnels-error">{error}</div>}
      {msg && <div className="tunnels-success">{msg}</div>}

      <div className="tunnels-body">
        {/* 面板自身穿透 */}
        <section className="panel">
          <h3>面板访问（面板自身穿透）</h3>
          {pc && (
            <>
              <div className="form-row">
                <label className="switch">
                  <input
                    type="checkbox"
                    checked={pc.enabled}
                    onChange={(e) => updPanel({ enabled: e.target.checked })}
                  />
                  启用
                </label>
                <select value={pc.frps_id || ''} onChange={(e) => updPanel({ frps_id: Number(e.target.value) })}>
                  <option value="">— 选择公网服务器 —</option>
                  {frpsList.map((f) => (
                    <option key={f.id} value={f.id}>{f.name}（{f.host}）</option>
                  ))}
                </select>
                <select value={pc.proxy_type} onChange={(e) => updPanel({ proxy_type: e.target.value })} style={{ width: 150 }}>
                  <option value="tcp">TCP（按端口）</option>
                  <option value="http">HTTP（按域名）</option>
                  <option value="https">HTTPS（按域名）</option>
                </select>

                {pc.proxy_type === 'tcp' ? (
                  <input
                    placeholder="公网端口（如 18080）"
                    value={pc.remote_port || ''}
                    onChange={(e) => updPanel({ remote_port: Number(e.target.value) || 0 })}
                    style={{ width: 160 }}
                  />
                ) : (
                  <input
                    placeholder="自定义域名（如 panel.example.com）"
                    value={pc.custom_domain}
                    onChange={(e) => updPanel({ custom_domain: e.target.value })}
                  />
                )}

                <button className="primary" onClick={savePanel} disabled={busy}>保存并应用</button>
              </div>

              {pc.proxy_type === 'https' && (
                <div className="form-row">
                  <label className="switch">
                    <input
                      type="checkbox"
                      checked={pc.use_tls}
                      onChange={(e) => updPanel({ use_tls: e.target.checked })}
                    />
                    由 frpc 终止 TLS（需证书）
                  </label>
                  {pc.use_tls && (
                    <>
                      <input
                        placeholder="证书文件路径（.crt/.pem）"
                        value={pc.cert_file}
                        onChange={(e) => updPanel({ cert_file: e.target.value })}
                      />
                      <input
                        placeholder="私钥文件路径（.key）"
                        value={pc.key_file}
                        onChange={(e) => updPanel({ key_file: e.target.value })}
                      />
                    </>
                  )}
                </div>
              )}

              <div className="panel-status">
                <span className={`status status-${panel.status}`}>{panel.status}</span>
                {panel.public_address && (
                  <span className="public-addr">
                    公网访问地址：<a href={panel.public_address} target="_blank" rel="noreferrer">{panel.public_address}</a>
                  </span>
                )}
                <span className="muted">
                  转发目标：{panel.tls_port ? `HTTPS ${panel.tls_port}` : `HTTP ${panel.local_port}`}
                </span>
                {panel.tls_port > 0 && <span className="muted tls-on">TLS 已启用</span>}
                {panel.error && <span className="err-text">{panel.error}</span>}
              </div>
              <div className="tunnels-hint">
                说明：把面板暴露到公网以便远程访问。选择 TCP 模式需一个公网端口；
                HTTP/HTTPS 模式需要域名（且 frps 已配置 vhostHTTPPort / vhostHTTPSPort）。
                {panel.tls_port > 0
                  ? '当前面板已启用 HTTPS，穿透会自动转发到 HTTPS 端口，公网访问即为加密连接。'
                  : '当前面板未启用 HTTPS，公网访问为明文；建议在 config.yaml 配置 tls_listen / tls_cert / tls_key（见 docs/CERTIFICATES.md）。'}
              </div>
            </>
          )}
          {!pc && <div className="empty">加载中...</div>}
        </section>

        {/* frps 服务器 */}
        <section className="panel">
          <h3>公网服务器（frps）</h3>
          <div className="form-row">
            <input placeholder="名称（如：香港高防线路）" value={fName} onChange={(e) => setFName(e.target.value)} />
            <input placeholder="公网地址（IP 或域名）" value={fHost} onChange={(e) => setFHost(e.target.value)} />
            <input placeholder="frps 端口" value={fBindPort} onChange={(e) => setFBindPort(e.target.value)} style={{ width: 90 }} />
            <input placeholder="认证 token（可选）" value={fToken} onChange={(e) => setFToken(e.target.value)} />
            <input placeholder="端口起" value={fStart} onChange={(e) => setFStart(e.target.value)} style={{ width: 90 }} />
            <input placeholder="端口止" value={fEnd} onChange={(e) => setFEnd(e.target.value)} style={{ width: 90 }} />
            <input
              placeholder="对外域名（如 mc.example.com）"
              value={fDomain}
              onChange={(e) => setFDomain(e.target.value)}
              style={{ width: 220 }}
              title="该线路上所有实例端口对外展示用这个域名（填纯域名，不用带端口，端口会自动拼上）"
            />
            <button className="primary" onClick={submitFrps} disabled={busy}>添加</button>
            {/* 添加前先测：地址/token/端口段填错时当场就能看出来，
                而不是等实例下发隧道、玩家连不上才发现 */}
            <button
              onClick={() => runTest('form', {
                host: fHost, bind_port: Number(fBindPort) || 7000, token: fToken,
                port_start: Number(fStart) || 25565, port_end: Number(fEnd) || 25600,
              })}
              disabled={testing !== null || !fHost.trim()}
              title="在节点上真的连一次并注册一个临时端口，随后撤销"
            >
              {testing === 'form' ? '测试中…' : '测试连接'}
            </button>
          </div>

          {renderTest('form')}

          <div className="tunnels-hint">
            公网服务器这一行的「公网地址」是 frps 真正监听的地方（可以是内网 IP），
            只有节点和 frps 用得到；<b>「对外域名」才是给用户看的地址</b> ——
            填了之后，这条线路上所有实例的端口都会显示成 <code>域名:端口</code>。
            不填的话实例页只能显示 <code>节点IP:端口</code>，用户一转发就把节点入口地址暴露出去了。
          </div>

          <table>
            <thead>
              <tr>
                <th>名称</th><th>公网地址</th><th>端口范围</th>
                <th>对外域名</th><th>已用</th><th>操作</th>
              </tr>
            </thead>
            <tbody>
              {frpsList.map((f) => {
                const draft = domainDraft[f.id] ?? f.display_domain ?? ''
                const dirty = draft !== (f.display_domain || '')
                return (
                  <tr key={f.id}>
                    <td>{f.name}</td>
                    <td className="mono">{f.host}:{f.bind_port}</td>
                    <td className="mono">{f.port_start}-{f.port_end}</td>
                    <td>
                      <span className="domain-edit">
                        <input
                          value={draft}
                          placeholder="如 mc.example.com"
                          onChange={(e) => setDomainDraft((d) => ({ ...d, [f.id]: e.target.value }))}
                          onKeyDown={(e) => { if (e.key === 'Enter' && dirty) saveDomain(f) }}
                          spellCheck={false}
                        />
                        <button
                          className={dirty ? 'primary' : ''}
                          onClick={() => saveDomain(f)}
                          disabled={domainBusy === f.id || !dirty}
                          title={dirty ? '保存这条线路的对外域名' : '没有改动'}
                        >
                          {domainBusy === f.id ? '…' : '保存'}
                        </button>
                      </span>
                      {f.display_domain
                        ? <div className="mono small">{f.display_domain}:{f.port_start}（示例）</div>
                        : <div className="err-text small">未配置 → 实例页会显示节点 IP</div>}
                    </td>
                    <td>{f.used_ports}</td>
                    <td>
                      <div className="frps-row-actions">
                        <button
                          onClick={() => runTest(String(f.id), f)}
                          disabled={testing !== null}
                          title="在节点上实测这条线路：TCP 可达 + frpc 能否用这个 token 注册"
                        >
                          {testing === String(f.id) ? '测试中…' : '测试'}
                        </button>
                        <button className="danger" onClick={() => removeFrps(f)}>删除</button>
                      </div>
                      {renderTest(String(f.id))}
                    </td>
                  </tr>
                )
              })}
              {frpsList.length === 0 && <tr><td colSpan={6} className="empty">尚未添加公网服务器</td></tr>}
            </tbody>
          </table>
        </section>

        {/* 隧道分配 */}
        <section className="panel">
          <h3>线路分配（隧道）</h3>
          <div className="form-row">
            <select value={tInstance} onChange={(e) => setTInstance(e.target.value)}>
              <option value="">— 选择实例 —</option>
              {instances.map((i) => (
                <option key={i.instance_id} value={i.instance_id}>
                  {i.name}（{i.instance_id}）
                </option>
              ))}
            </select>
            <select value={tFrps} onChange={(e) => setTFrps(e.target.value)}>
              <option value="">— 选择公网服务器 —</option>
              {frpsList.map((f) => (
                <option key={f.id} value={f.id}>{f.name}（{f.host}）</option>
              ))}
            </select>
            <select value={tProtocol} onChange={(e) => setTProtocol(e.target.value)} style={{ width: 90 }}>
              <option value="tcp">TCP</option>
              <option value="udp">UDP</option>
            </select>
            <input placeholder="实例内端口（留空=游戏端口）" value={tLocalPort} onChange={(e) => setTLocalPort(e.target.value)} style={{ width: 180 }} />
            <input placeholder="公网端口（留空=自动分配）" value={tRemotePort} onChange={(e) => setTRemotePort(e.target.value)} style={{ width: 180 }} />
            <input placeholder="线路备注" value={tName} onChange={(e) => setTName(e.target.value)} style={{ width: 140 }} />
            <input
              placeholder="对外域名（如 mc.example.com:25570）"
              value={tDomain}
              onChange={(e) => setTDomain(e.target.value)}
              style={{ width: 240 }}
              title="管理员配置的对外地址。填了之后实例详情页的「公网域名」会显示它，而不是 IP:端口"
            />
            <button className="primary" onClick={submitTunnel} disabled={busy || !tInstance || !tFrps}>分配线路</button>
          </div>

          <table>
            <thead>
              <tr>
                <th>实例</th><th>公网地址</th><th>转发</th><th>协议</th><th>状态</th><th>操作</th>
              </tr>
            </thead>
            <tbody>
              {tunnels.map((t) => {
                const st = statusOf(t)
                return (
                  <tr key={t.id}>
                    <td>{t.instance_name || t.instance_id}<div className="mono small">{t.name}</div></td>
                    <td className="mono">{t.public_address}</td>
                    <td className="mono">{t.remote_port} → {t.local_port}</td>
                    <td>{t.protocol}</td>
                    <td>
                      <span className={`status status-${st}`}>{st}</span>
                      {t.live_error && <div className="err-text">{t.live_error}</div>}
                    </td>
                    <td>
                      <div className="row-actions">
                        <button onClick={() => reapply(t)} disabled={busy}>重新下发</button>
                        <button className="danger" onClick={() => removeTunnel(t)} disabled={busy}>删除</button>
                      </div>
                    </td>
                  </tr>
                )
              })}
              {tunnels.length === 0 && <tr><td colSpan={6} className="empty">尚未分配任何线路</td></tr>}
            </tbody>
          </table>

          <div className="tunnels-hint">
            说明：分配后系统会自动生成 frpc 配置并下发到实例所在节点，公网玩家即可通过「公网地址」连入。
            实例停止时隧道随之断开；实例启动后若需恢复，点击「重新下发」。
          </div>
        </section>
      </div>
    </div>
  )
}
