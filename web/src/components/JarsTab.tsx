import { useEffect, useRef, useState } from 'react'
import { JarItem, listJars, uploadJar, setJar } from '../api'
import './JarsTab.css'

function fmtSize(n: number): string {
  if (n >= 1024 * 1024) return `${(n / 1024 / 1024).toFixed(1)} MB`
  if (n >= 1024) return `${(n / 1024).toFixed(1)} KB`
  return `${n} B`
}

export default function JarsTab({ instanceId, canWrite, running }: {
  instanceId: string
  canWrite: boolean
  running: boolean
}) {
  const [jars, setJars] = useState<JarItem[]>([])
  const [active, setActive] = useState('')
  const [error, setError] = useState('')
  const [msg, setMsg] = useState('')
  const [busy, setBusy] = useState(false)
  const [progress, setProgress] = useState('')
  const fileRef = useRef<HTMLInputElement>(null)

  const load = async () => {
    try {
      const r = await listJars(instanceId)
      setJars(Array.isArray(r.jars) ? r.jars : [])
      setActive(r.active_jar || '')
      setError('')
    } catch (e: any) {
      setError(e.message)
    }
  }

  useEffect(() => { load() }, [instanceId])

  const onUpload = async (e: React.ChangeEvent<HTMLInputElement>) => {
    const f = e.target.files?.[0]
    if (!f) return
    if (!f.name.toLowerCase().endsWith('.jar')) {
      setError('仅支持 .jar 文件')
      return
    }
    setBusy(true); setError(''); setMsg('')
    setProgress(`正在上传 ${f.name}（${fmtSize(f.size)}）…`)
    try {
      const r = await uploadJar(instanceId, f)
      setMsg(r.message || '上传成功')
      await load()
    } catch (err: any) {
      setError(err.message)
    } finally {
      setBusy(false)
      setProgress('')
      if (fileRef.current) fileRef.current.value = ''
    }
  }

  const doSwitch = async (j: JarItem) => {
    if (running && !confirm('实例正在运行，切换核心需先停止并重启才会生效。\n是否继续？')) return
    setBusy(true); setError(''); setMsg('')
    try {
      const r = await setJar(instanceId, j.path)
      setMsg(r.message || '已切换')
      await load()
    } catch (e: any) {
      setError(e.message)
    } finally {
      setBusy(false)
    }
  }

  return (
    <div className="jars-tab">
      <div className="jars-bar">
        <h3>核心 jar 管理</h3>
        <div className="spacer" />
        <button onClick={load} disabled={busy}>刷新</button>
        {canWrite && (
          <>
            <input
              ref={fileRef}
              type="file"
              accept=".jar"
              style={{ display: 'none' }}
              onChange={onUpload}
            />
            <button className="primary" disabled={busy} onClick={() => fileRef.current?.click()}>
              上传核心 jar
            </button>
          </>
        )}
      </div>

      {running && (
        <div className="warn-banner">
          实例正在运行：切换核心后需<strong>重启实例</strong>才会生效。
        </div>
      )}
      {error && <div className="error-banner">{error}</div>}
      {msg && <div className="success-banner">{msg}</div>}
      {progress && <div className="progress-banner">{progress}</div>}

      <div className="active-jar">
        当前核心：<span className="mono">{active || '（未设置）'}</span>
      </div>

      <table className="jars-table">
        <thead>
          <tr><th>文件名</th><th>大小</th><th>状态</th><th>操作</th></tr>
        </thead>
        <tbody>
          {jars.map((j) => (
            <tr key={j.filename} className={j.active ? 'is-active' : ''}>
              <td className="mono">{j.filename}</td>
              <td>{fmtSize(j.size)}</td>
              <td>{j.active ? <span className="status status-running">使用中</span> : <span className="muted">未使用</span>}</td>
              <td>
                {canWrite && !j.active && (
                  <button onClick={() => doSwitch(j)} disabled={busy}>设为当前核心</button>
                )}
                {j.active && <span className="muted">—</span>}
              </td>
            </tr>
          ))}
          {jars.length === 0 && (
            <tr>
              <td colSpan={4} className="empty">
                实例目录中没有 jar 文件。当前核心可能位于共享目录
                （路径见上方「当前核心」），可上传新核心或切换到共享目录中的核心。
              </td>
            </tr>
          )}
        </tbody>
      </table>

      <div className="jars-note">
        说明：上传的 jar 会保存到实例目录。切换核心需要实例处于停止状态；
        正在使用的核心不会被删除（避免实例下次启动失败）。
        若要从共享目录切换核心，可在文件管理中将其复制到实例目录后再操作。
      </div>
    </div>
  )
}
