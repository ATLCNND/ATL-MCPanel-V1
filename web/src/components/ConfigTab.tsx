import { useEffect, useState } from 'react'
import { readFile, writeFile } from '../api'
import { SERVER_PROPERTY_DESC, COMMON_PROPERTY_KEYS } from '../configDescriptions'
import './ConfigTab.css'

// 说明文案与「常用项」清单统一放在 configDescriptions.ts：
// 那里是**唯一**一处集中放配置文案的地方，将来做中英切换只改那个模块。
const KNOWN_KEYS = SERVER_PROPERTY_DESC
const COMMON_KEYS = COMMON_PROPERTY_KEYS

interface Entry {
  key: string
  value: string
  comment?: string
}

export default function ConfigTab({ instanceId, canWrite }: { instanceId: string; canWrite: boolean }) {
  const [entries, setEntries] = useState<Entry[]>([])
  const [original, setOriginal] = useState<string>('')
  const [raw, setRaw] = useState(false)
  const [rawText, setRawText] = useState('')
  const [error, setError] = useState('')
  const [msg, setMsg] = useState('')
  const [loading, setLoading] = useState(false)
  const [onlyCommon, setOnlyCommon] = useState(false)

  const load = async () => {
    setLoading(true)
    setError('')
    try {
      const data = await readFile(instanceId, 'server.properties')
      setOriginal(data.content)
      setRawText(data.content)
      setEntries(parseProps(data.content))
    } catch (e: any) {
      setError(e.message || '读取 server.properties 失败（实例可能尚未启动过，首次启动后会自动生成）')
    } finally {
      setLoading(false)
    }
  }

  useEffect(() => {
    load()
  }, [instanceId])

  const save = async () => {
    setError('')
    setMsg('')
    try {
      const content = raw ? rawText : serializeProps(entries, original)
      await writeFile(instanceId, 'server.properties', content)
      setMsg('已保存（重启实例后生效）')
      await load()
    } catch (e: any) {
      setError(e.message)
    }
  }

  const setValue = (key: string, value: string) => {
    setEntries((prev) => prev.map((e) => (e.key === key ? { ...e, value } : e)))
  }

  const shown = onlyCommon ? entries.filter((e) => COMMON_KEYS.has(e.key)) : entries

  return (
    <div className="config-tab">
      <div className="config-toolbar">
        <label className="switch">
          <input type="checkbox" checked={onlyCommon} onChange={(e) => setOnlyCommon(e.target.checked)} />
          只看常用项
        </label>
        <label className="switch">
          <input type="checkbox" checked={raw} onChange={(e) => setRaw(e.target.checked)} />
          原始文本模式
        </label>
        <div className="spacer" />
        <button onClick={load} disabled={loading}>重新加载</button>
        <button className="primary" onClick={save} disabled={!canWrite}>保存</button>
      </div>

      {error && <div className="config-error">{error}</div>}
      {msg && <div className="config-success">{msg}</div>}
      {!canWrite && <div className="config-hint">只读权限：不可修改配置</div>}

      {raw ? (
        <textarea
          className="config-raw"
          value={rawText}
          onChange={(e) => setRawText(e.target.value)}
          spellCheck={false}
          readOnly={!canWrite}
        />
      ) : (
        <div className="config-form">
          {shown.map((e) => (
            <div className="config-row" key={e.key}>
              <div className="config-key">
                <span className="key-name">{e.key}</span>
                {KNOWN_KEYS[e.key] ? (
                  <span className="key-desc">{KNOWN_KEYS[e.key]}</span>
                ) : (
                  // 不在原版 57 项里的键：多半是核心（Paper/Leaves 等）或插件追加的。
                  // 明确说一句，比"这一项没有说明"更好 —— 用户至少知道不必去查原版文档。
                  <span className="key-desc unknown">非原版标准项（可能是服务端核心或插件追加）</span>
                )}
              </div>
              <div className="config-value">
                {isBoolean(e.value) ? (
                  <select value={e.value} onChange={(ev) => setValue(e.key, ev.target.value)} disabled={!canWrite}>
                    <option value="true">true</option>
                    <option value="false">false</option>
                  </select>
                ) : (
                  <input
                    value={e.value}
                    onChange={(ev) => setValue(e.key, ev.target.value)}
                    disabled={!canWrite}
                    spellCheck={false}
                  />
                )}
              </div>
            </div>
          ))}
          {shown.length === 0 && !error && (
            <div className="empty">暂无配置项</div>
          )}
        </div>
      )}
    </div>
  )
}

function isBoolean(v: string) {
  return v === 'true' || v === 'false'
}

// parseProps 解析 server.properties（保留注释与顺序）
function parseProps(text: string): Entry[] {
  const out: Entry[] = []
  for (const line of text.split('\n')) {
    const trimmed = line.trim()
    if (trimmed === '' || trimmed.startsWith('#')) continue
    const idx = trimmed.indexOf('=')
    if (idx < 0) continue
    out.push({
      key: trimmed.slice(0, idx).trim(),
      value: trimmed.slice(idx + 1).trim(),
    })
  }
  return out
}

// serializeProps 以原文件为模板写回（保留注释/顺序/未知项）
function serializeProps(entries: Entry[], template: string): string {
  const map = new Map(entries.map((e) => [e.key, e.value]))
  const lines = template.split('\n')
  const written = new Set<string>()

  const out = lines.map((line) => {
    const trimmed = line.trim()
    if (trimmed === '' || trimmed.startsWith('#')) return line
    const idx = trimmed.indexOf('=')
    if (idx < 0) return line
    const key = trimmed.slice(0, idx).trim()
    if (!map.has(key)) return line
    written.add(key)
    return `${key}=${map.get(key)}`
  })

  // 追加模板中不存在的新键
  for (const [k, v] of map) {
    if (!written.has(k)) out.push(`${k}=${v}`)
  }
  return out.join('\n')
}
