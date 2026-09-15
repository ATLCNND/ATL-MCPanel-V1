import { useEffect, useState } from 'react'
import { readFile, writeFile } from '../api'
import './ConfigTab.css'

// server.properties 的关键项说明（用于分组与提示）
const KNOWN_KEYS: Record<string, string> = {
  'motd': '服务器描述（玩家在多人列表看到的名字）',
  'server-port': '服务器端口',
  'max-players': '最大同时在线玩家数',
  'online-mode': '正版验证（true=仅正版可进）',
  'gamemode': '默认游戏模式（survival/creative/adventure/spectator）',
  'difficulty': '难度（peaceful/easy/normal/hard）',
  'hardcore': '极限模式',
  'pvp': '允许玩家互相伤害',
  'level-name': '世界存档目录名',
  'level-seed': '世界种子',
  'level-type': '世界类型',
  'view-distance': '视距（越大越吃性能）',
  'simulation-distance': '模拟距离',
  'spawn-protection': '出生点保护半径',
  'allow-flight': '允许飞行',
  'allow-nether': '允许进入下界',
  'enable-command-block': '启用命令方块',
  'white-list': '启用白名单',
  'enforce-whitelist': '白名单强制模式',
  'spawn-monsters': '生成怪物',
  'spawn-npcs': '生成村民',
  'spawn-animals': '生成动物',
  'generate-structures': '生成结构（村庄等）',
  'max-world-size': '世界边界半径',
  'player-idle-timeout': '挂机踢出时间（分钟，0=不踢）',
  'enable-rcon': '启用 RCON',
  'rcon.port': 'RCON 端口',
  'rcon.password': 'RCON 密码',
}

// 高亮为「常用」的键
const COMMON_KEYS = new Set([
  'motd', 'max-players', 'online-mode', 'gamemode', 'difficulty',
  'pvp', 'view-distance', 'simulation-distance', 'spawn-protection',
  'allow-flight', 'enable-command-block', 'white-list',
])

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
                {KNOWN_KEYS[e.key] && <span className="key-desc">{KNOWN_KEYS[e.key]}</span>}
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
