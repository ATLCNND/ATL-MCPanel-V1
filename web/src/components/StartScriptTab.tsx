import { useEffect, useState } from 'react'
import {
  ContainerState,
  StartScriptState,
  getContainerState,
  getStartScript,
  setContainerEnabled,
  setStartScript,
} from '../api'
import './StartScriptTab.css'

export default function StartScriptTab({ instanceId, canWrite, running, isAdmin }: {
  instanceId: string
  canWrite: boolean
  running: boolean
  isAdmin: boolean
}) {
  const [state, setState] = useState<StartScriptState | null>(null)
  const [draft, setDraft] = useState('')
  const [dirty, setDirty] = useState(false)
  const [error, setError] = useState('')
  const [msg, setMsg] = useState('')
  const [busy, setBusy] = useState(false)
  // 容器化隔离（安全属性，单独一块展示与开关）
  const [ctr, setCtr] = useState<ContainerState | null>(null)
  const [ctrErr, setCtrErr] = useState('')
  const [ctrBusy, setCtrBusy] = useState(false)

  const load = async () => {
    try {
      const st = await getStartScript(instanceId)
      setState(st)
      setDraft(st.script)
      setDirty(false)
      setError('')
    } catch (e: any) {
      setError(e.message)
    }
    try {
      setCtr(await getContainerState(instanceId))
      setCtrErr('')
    } catch (e: any) {
      setCtrErr(e.message)
    }
  }

  useEffect(() => { load() }, [instanceId])

  const toggleContainer = async (next: boolean) => {
    const tip = next
      ? '开启容器化隔离后，实例将在独立容器中运行（独立网络、只读根、只保留必要的权限）。\n\n' +
        '注意：容器内看到的实例目录是 /data；启动脚本里请用 /data/… 这样的路径引用实例内的文件。\n\n' +
        '设置下次启动生效，确定继续吗？'
      : '关闭容器化隔离后，实例将直接运行在节点上（仍以专用系统用户运行，但能看见宿主文件与进程）。\n\n确定继续吗？'
    if (!confirm(tip)) return
    setCtrBusy(true); setCtrErr(''); setMsg('')
    try {
      const r = await setContainerEnabled(instanceId, next)
      setMsg(r.message || '已更新容器化设置')
      await load()
    } catch (e: any) {
      setCtrErr(e.message)
    } finally {
      setCtrBusy(false)
    }
  }

  const run = async (action: 'generate' | 'save' | 'remove', content?: string) => {
    setBusy(true); setError(''); setMsg('')
    try {
      const r = await setStartScript(instanceId, action, content)
      setMsg(r.message || '操作完成')
      await load()
    } catch (e: any) {
      setError(e.message)
    } finally {
      setBusy(false)
    }
  }

  if (!state) {
    return (
      <div className="start-tab">
        {error ? <div className="error-banner">{error}</div> : <div className="empty">加载中…</div>}
      </div>
    )
  }

  return (
    <div className="start-tab">
      <div className="start-head">
        <h3>启动脚本</h3>
        <div className="spacer" />
        <button onClick={load} disabled={busy}>刷新</button>
      </div>

      {running && (
        <div className="warn-banner">
          实例正在运行：修改启动脚本后需<strong>重启实例</strong>才会生效。
        </div>
      )}
      {error && <div className="error-banner">{error}</div>}
      {msg && <div className="success-banner">{msg}</div>}

      {/* 当前生效方式 */}
      <div className="start-status">
        <span className={`mode-badge mode-${state.mode}`}>{state.mode_label}</span>
        <span className="muted">实例将在启动时执行：</span>
      </div>
      <pre className="command-preview">{state.command}</pre>

      <div className="start-meta">
        <span>核心 jar：<span className="mono">{state.jar_path || '（未设置）'}</span></span>
        <span>内存：-Xms{state.min_mem} -Xmx{state.max_mem}</span>
        <span>核心类型：{state.core_type}</span>
      </div>

      {/* 优先级说明 */}
      <div className="priority-box">
        <div className="priority-title">启动方式优先级（从高到低）</div>
        <ol>
          <li className={state.mode === 'start.sh' ? 'current' : ''}>
            <strong>实例目录下的 start.sh</strong> —— 存在即使用，最灵活，可写任意启动逻辑
          </li>
          <li className={state.mode === 'custom' ? 'current' : ''}>
            <strong>自定义启动命令</strong> —— 创建实例时填写，支持
            <code>{'{jar}'}</code> <code>{'{max_mem}'}</code> <code>{'{min_mem}'}</code>
            <code>{'{java}'}</code> <code>{'{dir}'}</code> 占位符
          </li>
          <li className={state.mode === 'default' ? 'current' : ''}>
            <strong>默认 java 命令</strong> —— <code>java -Xms… -Xmx… -jar 核心.jar nogui</code>
          </li>
        </ol>
      </div>

      {/* 容器化隔离：安全属性，值得单独一块、且把"当前到底有没有隔离"说明白 */}
      <div className="container-box">
        <div className="container-head">
          <strong>容器化隔离</strong>
          {ctr?.enabled
            ? <span className="status status-running">已开启</span>
            : <span className="status status-stopped">未开启</span>}
          <div className="spacer" />
          {/* 开关只对总管理员显示：容器化限制的正是实例里的进程，
              而实例 owner 能在启动脚本/控制台里执行任意命令 ——
              被隔离的一方若能自己关掉隔离，隔离就不成立。 */}
          {isAdmin && (
            <label className="ctr-switch">
              <input
                type="checkbox"
                checked={!!ctr?.enabled}
                disabled={ctrBusy || running || !ctr?.enabled && !ctr?.available}
                onChange={(e) => toggleContainer(e.target.checked)}
              />
              <span>{ctr?.enabled ? '关闭容器化' : '开启容器化'}</span>
            </label>
          )}
        </div>

        {ctrErr && <div className="error-banner">{ctrErr}</div>}

        <div className="container-desc">
          {ctr?.enabled ? (
            <>
              {/* container_note 本身就是一句完整的话（"该实例运行在容器 atl-xxx 内…"），
                  不要再套进"实例运行在容器 … 内"，否则界面上会读成
                  "实例运行在容器 该实例运行在容器 … 内"，像出错了。 */}
              {ctr.container_note ? <>{ctr.container_note}：</> : <>实例运行在容器内：</>}
              {' '}独立网络（连不到宿主上的其它服务）、只读根文件系统、丢弃全部 Linux 权限。
              容器内看到的实例目录是 <span className="mono">/data</span>。
            </>
          ) : (
            <>
              未开启时实例直接运行在节点上（仍以专用系统用户运行、目录互相隔离，
              但<strong>能读到宿主上其它可读文件、能访问宿主的本机服务</strong>）。
              开启容器化可以进一步把这部分隔开。
            </>
          )}
        </div>

        {running && <div className="warn-banner">实例正在运行：容器化设置需要<strong>先停止实例</strong>才能修改。</div>}

        {!ctr?.available && (
          <div className="warn-banner">
            当前节点暂不支持容器化：{ctr?.reason || '原因未知'}
          </div>
        )}

        {ctr?.available && (
          <div className="container-meta">
            <span>Docker：<span className="mono">{ctr.docker_version || '—'}</span></span>
            <span>基础镜像：<span className="mono">{ctr.image || '—'}</span></span>
          </div>
        )}

        {!isAdmin && (
          <div className="muted">
            容器化隔离由<strong>总管理员</strong>控制。它限制的是实例内进程能碰到的范围
            （宿主文件、宿主服务、其它实例），因此不能由实例自己开关 ——
            否则被隔离的一方一关就能拿回这些能力。如需开启请联系管理员。
          </div>
        )}
      </div>

      {/* 脚本编辑器 */}
      <div className="script-editor">        <div className="editor-head">
          <strong>start.sh</strong>
          {state.has_script
            ? <span className="status status-running">已存在并生效</span>
            : <span className="muted">尚未创建 —— 当前使用{state.mode_label}</span>}
          <div className="spacer" />
          {canWrite && (
            <>
              {!state.has_script && (
                <button className="primary" disabled={busy}
                  onClick={() => run('generate')}>
                  从当前配置生成脚本
                </button>
              )}
              {state.has_script && (
                <>
                  <button className="primary" disabled={busy || !dirty}
                    onClick={() => run('save', draft)}>
                    保存修改
                  </button>
                  <button disabled={busy} onClick={() => { setDraft(state.script); setDirty(false) }}>
                    撤销修改
                  </button>
                  <button className="danger" disabled={busy}
                    onClick={() => {
                      if (confirm('删除 start.sh？实例将恢复使用面板中配置的启动命令。')) run('remove')
                    }}>
                    删除脚本
                  </button>
                </>
              )}
            </>
          )}
        </div>

        <textarea
          className="script-textarea"
          value={draft}
          spellCheck={false}
          disabled={!canWrite || (!state.has_script && true)}
          placeholder={canWrite
            ? '点击上方「从当前配置生成脚本」，即可基于当前核心与内存生成一份可编辑的启动脚本。'
            : '需要拥有者权限才能编辑启动脚本。'}
          onChange={(e) => { setDraft(e.target.value); setDirty(true) }}
          rows={18}
        />

        {!canWrite && (
          <div className="muted">只读/协作权限：修改启动脚本需要 owner 及以上权限。</div>
        )}
      </div>

      <div className="start-note">
        说明：脚本通过 <code>sh</code> 执行，因此<strong>不需要 chmod +x</strong>，
        直接在文件管理中创建或编辑同样生效。
        脚本中请保留 <code>exec java …</code> 形式，让 java 接管进程 ——
        这样面板的停止、重启与 Daemon 重启后的接管才能正常工作。
        生成脚本使用绝对路径，也可在文件管理中自行调整。
      </div>
    </div>
  )
}
