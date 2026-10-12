import { useEffect, useState } from 'react'
import { GameRule, WorldInfo, getWorldInfo, setGameRule } from '../api'
import './GameSettingsTab.css'

/** 游戏模式（原版 GameType 的 0-3）。 */
const GAME_TYPES = ['生存', '创造', '冒险', '旁观']
/** 难度（原版 Difficulty 的 0-3）。 */
const DIFFICULTIES = ['和平', '简单', '普通', '困难']

/**
 * 原版规则的界面分组与中文说明。
 *
 * 为什么要手写这份映射而不是直接把 level.dat 里的 51 个名字列出来：
 * `commandModificationBlockLimit` 这种名字，看不懂的人要么不敢动、要么乱动。
 * 分组 + 一句话说明之后，"keepInventory 死亡不掉落"这种才是可用状态。
 *
 * 只列**常用**的；不在表里的规则仍然会显示（在"其它规则"一组里），
 * 不会被藏起来 —— 藏起来等于"面板说没有这条规则"，那是误导。
 */
const RULE_META: Record<string, { label: string; hint: string; kind: 'bool' | 'number' }> = {
  keepInventory: { label: '死亡不掉落', hint: '开启后玩家死亡不掉落物品与经验', kind: 'bool' },
  doDaylightCycle: { label: '昼夜交替', hint: '关闭后时间停止流动', kind: 'bool' },
  doWeatherCycle: { label: '天气变化', hint: '关闭后不再下雨/放晴', kind: 'bool' },
  doMobSpawning: { label: '生物生成', hint: '关闭后不再自然生成生物', kind: 'bool' },
  mobGriefing: { label: '生物破坏方块', hint: '关闭后苦力怕/末影人不再破坏方块', kind: 'bool' },
  doFireTick: { label: '火焰蔓延', hint: '关闭后火不会蔓延也不会熄灭', kind: 'bool' },
  doMobLoot: { label: '生物掉落物', hint: '关闭后生物死亡不掉落物品', kind: 'bool' },
  doTileDrops: { label: '方块掉落', hint: '关闭后破坏方块不掉落物品', kind: 'bool' },
  doImmediateRespawn: { label: '立即重生', hint: '关闭后死亡需点重生按钮', kind: 'bool' },
  naturalRegeneration: { label: '自然回血', hint: '关闭后饱食度满也不回血', kind: 'bool' },
  fallDamage: { label: '摔落伤害', hint: '', kind: 'bool' },
  fireDamage: { label: '火焰伤害', hint: '', kind: 'bool' },
  drowningDamage: { label: '溺水伤害', hint: '', kind: 'bool' },
  freezeDamage: { label: '冰冻伤害', hint: '', kind: 'bool' },
  showDeathMessages: { label: '死亡消息', hint: '玩家死亡时是否广播', kind: 'bool' },
  announceAdvancements: { label: '进度广播', hint: '', kind: 'bool' },
  doInsomnia: { label: '幻翼生成', hint: '关闭后不睡觉也不会刷幻翼', kind: 'bool' },
  doPatrolSpawning: { label: '灾厄巡逻队', hint: '', kind: 'bool' },
  doTraderSpawning: { label: '流浪商人', hint: '', kind: 'bool' },
  doWardenSpawning: { label: '监守者生成', hint: '', kind: 'bool' },
  disableRaids: { label: '禁用袭击', hint: '', kind: 'bool' },
  sendCommandFeedback: { label: '指令回显', hint: '关闭后执行指令不再提示结果', kind: 'bool' },
  logAdminCommands: { label: '记录管理员指令', hint: '', kind: 'bool' },
  commandBlockOutput: { label: '命令方块输出', hint: '', kind: 'bool' },
  doLimitedCrafting: { label: '限制合成配方', hint: '开启后只能合成已解锁的配方', kind: 'bool' },
  spectatorsGenerateChunks: { label: '旁观者加载区块', hint: '', kind: 'bool' },
  reducedDebugInfo: { label: '精简调试信息', hint: 'F3 隐藏坐标等敏感信息', kind: 'bool' },
  randomTickSpeed: { label: '随机刻速度', hint: '默认 3；调高会显著增加作物生长与卡顿', kind: 'number' },
  spawnRadius: { label: '出生点保护半径', hint: '默认 10 格', kind: 'number' },
  maxEntityCramming: { label: '实体挤压上限', hint: '默认 24；设为 0 关闭挤压伤害', kind: 'number' },
  playersSleepingPercentage: { label: '跳过夜晚所需睡眠比例', hint: '默认 100（全部玩家）；0 = 一人即可', kind: 'number' },
  snowAccumulationHeight: { label: '积雪高度', hint: '默认 1', kind: 'number' },
  spawnChunkRadius: { label: '出生点区块半径', hint: '默认 2', kind: 'number' },
}

/** 按重要性分组显示；未在 RULE_META 里的归入"其它规则"。 */
const GROUPS: { title: string; keys: string[] }[] = [
  {
    title: '常用',
    keys: ['keepInventory', 'doDaylightCycle', 'doWeatherCycle', 'doMobSpawning', 'mobGriefing', 'doFireTick', 'doImmediateRespawn', 'naturalRegeneration'],
  },
  {
    title: '掉落与伤害',
    keys: ['doMobLoot', 'doTileDrops', 'fallDamage', 'fireDamage', 'drowningDamage', 'freezeDamage'],
  },
  {
    title: '生物与事件',
    keys: ['doInsomnia', 'doPatrolSpawning', 'doTraderSpawning', 'doWardenSpawning', 'disableRaids'],
  },
  {
    title: '提示与指令',
    keys: ['showDeathMessages', 'announceAdvancements', 'sendCommandFeedback', 'logAdminCommands', 'commandBlockOutput', 'reducedDebugInfo', 'spectatorsGenerateChunks', 'doLimitedCrafting'],
  },
  {
    title: '数值',
    keys: ['randomTickSpeed', 'spawnRadius', 'maxEntityCramming', 'playersSleepingPercentage', 'snowAccumulationHeight', 'spawnChunkRadius'],
  },
]

function fmtNum(n: number): string {
  // 种子这种大整数用 toLocaleString 分组显示更好读；坐标同理
  return n.toLocaleString('zh-CN')
}

/** 游戏刻换算成"第几天"（24000 刻 = 1 天，且 Minecraft 的第 0 天从 0 刻开始）。 */
function fmtDayTime(ticks: number): string {
  const day = Math.floor(ticks / 24000)
  const t = ((ticks % 24000) + 24000) % 24000
  const hh = Math.floor(t / 1000)
  const mm = Math.floor(((t % 1000) / 1000) * 60)
  return `第 ${day} 天 ${String(hh).padStart(2, '0')}:${String(mm).padStart(2, '0')}`
}

/**
 * 「游戏设置」页：世界基本信息（只读）+ 原版规则管理。
 *
 * 数据来自节点解析 level.dat 的结果（见后端 world.go / worldinfo.go）。
 *
 * ⚠️ 规则**只能改运行中的实例**：后端走控制台 `/gamerule`。
 * 直接改 level.dat 会被运行中的服务器覆写，属于"改了却没变"，所以停止时
 * 后端会拒绝并在界面上如实说明 —— 这里也把输入控件禁用掉，不给出假入口。
 */
export default function GameSettingsTab({ instanceId, canWrite, running }: {
  instanceId: string
  canWrite: boolean
  running: boolean
}) {
  const [info, setInfo] = useState<WorldInfo | null>(null)
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState('')
  const [msg, setMsg] = useState('')
  /** 正在提交的规则名（用于锁定那一行的控件） */
  const [busyRule, setBusyRule] = useState('')
  /** 数值型规则的编辑草稿：规则名 → 当前输入值 */
  const [numDraft, setNumDraft] = useState<Record<string, string>>({})

  const load = async () => {
    setLoading(true)
    try {
      const w = await getWorldInfo(instanceId)
      setInfo(w)
      // 数值型规则的草稿跟着刷新（不要在刷新后还留着旧草稿，那会让人以为已生效）
      const draft: Record<string, string> = {}
      for (const g of w.gamerules) draft[g.name] = g.value
      setNumDraft(draft)
      setError('')
    } catch (e: any) {
      setError(e.message)
    } finally {
      setLoading(false)
    }
  }

  useEffect(() => { load() }, [instanceId])

  const apply = async (name: string, value: string) => {
    setBusyRule(name); setError(''); setMsg('')
    try {
      const r = await setGameRule(instanceId, name, value)
      setMsg(r.message)
      // ⚠️ **不要**在改完之后立刻 load()。
      //
      // 规则读自存档（level.dat），而 /gamerule 改的是服务器内存里的值，
      // 只有世界保存时才落盘。改完马上重新读，拿到的还是**旧值** ——
      // 界面上就会看到"点了开关又弹回去"（实测踩过）。
      // 所以这里把服务端**确认生效**的值就地写进本地状态，等用户下次
      // 手动刷新 / 重进页面时再与存档对齐（那时通常已经保存过了）。
      setInfo((prev) => prev ? {
        ...prev,
        gamerules: prev.gamerules.map((g) => g.name === name ? { ...g, value } : g),
      } : prev)
      setNumDraft((d) => ({ ...d, [name]: value }))
    } catch (e: any) {
      setError(e.message)
      // 失败时反过来必须重新读：本地状态可能与服务器不一致，
      // 以服务端为准（例如"实例未运行"被拒，值本来就该是旧的）。
      void load()
    } finally {
      setBusyRule('')
    }
  }

  if (loading && !info) return <div className="gs-empty">加载世界信息中…</div>
  if (!info) return <div className="error-banner">{error || '读取世界信息失败'}</div>

  const canEditRules = canWrite && running
  const ruleMap: Record<string, GameRule> = {}
  for (const g of info.gamerules) ruleMap[g.name] = g

  // 未被 GROUPS 覆盖的规则：全部塞进"其它规则"，一条都不藏
  const grouped = new Set(GROUPS.flatMap((g) => g.keys))
  const others = info.gamerules.filter((g) => !grouped.has(g.name)).map((g) => g.name)

  const renderRule = (name: string) => {
    const rule = ruleMap[name]
    if (!rule) return null
    const meta = RULE_META[name]
    const isBool = meta ? meta.kind === 'bool' : rule.value === 'true' || rule.value === 'false'
    const label = meta?.label || name
    const busy = busyRule === name

    return (
      <div className="gs-rule" key={name}>
        <div className="gs-rule-main">
          <div className="gs-rule-label">
            {label}
            {/* 真实规则名一并露出：出问题时要能在控制台/日志里对上 */}
            {meta && <code className="gs-rule-name">{name}</code>}
          </div>
          {meta?.hint && <div className="gs-rule-hint">{meta.hint}</div>}
        </div>
        <div className="gs-rule-ctl">
          {isBool ? (
            <button
              className={`gs-toggle ${rule.value === 'true' ? 'on' : ''}`}
              disabled={!canEditRules || busy}
              onClick={() => apply(name, rule.value === 'true' ? 'false' : 'true')}
              title={canEditRules ? '' : '实例需在运行中才能修改规则'}
            >
              {busy ? '…' : rule.value === 'true' ? '开' : '关'}
            </button>
          ) : (
            <div className="gs-num">
              <input
                value={numDraft[name] ?? rule.value}
                disabled={!canEditRules || busy}
                inputMode="numeric"
                onChange={(e) => setNumDraft({ ...numDraft, [name]: e.target.value })}
                title={canEditRules ? '' : '实例需在运行中才能修改规则'}
              />
              <button
                disabled={!canEditRules || busy || (numDraft[name] ?? rule.value) === rule.value}
                onClick={() => apply(name, (numDraft[name] ?? rule.value).trim())}
              >
                应用
              </button>
            </div>
          )}
        </div>
      </div>
    )
  }

  return (
    <div className="gs-tab">
      {error && <div className="error-banner" onClick={() => setError('')}>{error}</div>}
      {msg && <div className="success-banner" onClick={() => setMsg('')}>{msg}</div>}

      {!info.level_dat_found ? (
        <div className="gs-empty">
          尚未找到世界存档（<code>{info.world_dir}/level.dat</code>）。
          <br />
          <span className="muted">实例启动过一次、世界生成之后这里才会有数据。</span>
        </div>
      ) : (
        <>
          {/* ---------- 世界基本信息 ---------- */}
          <section className="gs-card">
            <div className="gs-card-head">
              <span className="gs-card-title">世界信息</span>
              <span className="gs-badge">{info.world_dir}</span>
            </div>

            <div className="gs-grid">
              <div className="gs-item">
                <span className="k">世界名</span>
                <span className="v">{info.level_name}</span>
                <span className="sub">level.dat 里的 LevelName</span>
              </div>
              <div className="gs-item">
                <span className="k">种子</span>
                {/* 种子用字符串：实测值超过 2^53，用 number 会在前端被改掉末几位 */}
                <span className="v mono">
                  {info.seed_present ? info.seed : '未记录'}
                </span>
                <span className="sub">
                  {info.seed_present
                    ? '可直接用于同种子开新世界'
                    : '老存档可能不写种子'}
                </span>
              </div>
              <div className="gs-item">
                <span className="k">游戏模式</span>
                <span className="v">{GAME_TYPES[info.game_type] ?? `未知(${info.game_type})`}</span>
                <span className="sub">{info.hardcore ? '极限模式（死亡即封禁）' : '普通模式'}</span>
              </div>
              <div className="gs-item">
                <span className="k">难度</span>
                <span className="v">
                  {DIFFICULTIES[info.difficulty] ?? `未知(${info.difficulty})`}
                </span>
                <span className="sub">
                  {info.difficulty_locked ? '已锁定（不能改）' : '未锁定'}
                </span>
              </div>
              <div className="gs-item">
                <span className="k">生成类型</span>
                <span className="v">{info.generator.replace(/^minecraft:/, '') || '—'}</span>
                <span className="sub">
                  {info.generate_features ? '生成结构' : '不生成结构'}
                  {info.bonus_chest ? ' · 含奖励箱' : ''}
                </span>
              </div>
              <div className="gs-item">
                <span className="k">允许作弊指令</span>
                <span className="v">{info.allow_commands ? '是' : '否'}</span>
                <span className="sub">allowCommands</span>
              </div>
              <div className="gs-item">
                <span className="k">出生点</span>
                <span className="v mono">
                  {fmtNum(info.spawn_x)} / {fmtNum(info.spawn_y)} / {fmtNum(info.spawn_z)}
                </span>
                <span className="sub">X / Y / Z</span>
              </div>
              <div className="gs-item">
                <span className="k">世界边界</span>
                <span className="v">
                  {info.border_set ? `${fmtNum(Math.round(info.border_size))} 格` : '未设置'}
                </span>
                <span className="sub">
                  {info.border_set
                    ? `中心 ${fmtNum(Math.round(info.border_center_x))}, ${fmtNum(Math.round(info.border_center_z))}`
                    : '原版在未设置时写入哨兵值 6e7，已识别'}
                </span>
              </div>
            </div>
          </section>

          {/* ---------- 世界状态 ---------- */}
          <section className="gs-card">
            <div className="gs-card-head">
              <span className="gs-card-title">世界状态</span>
            </div>
            <div className="gs-grid">
              <div className="gs-item">
                <span className="k">游戏内时间</span>
                <span className="v">{fmtDayTime(info.day_time)}</span>
                <span className="sub">{fmtNum(info.day_time)} 刻</span>
              </div>
              <div className="gs-item">
                <span className="k">天气</span>
                <span className="v">
                  {info.thundering ? '雷暴' : info.raining ? '下雨' : '晴朗'}
                </span>
                <span className="sub">来自存档当前状态</span>
              </div>
              <div className="gs-item">
                <span className="k">上次游玩</span>
                <span className="v">
                  {/* LastPlayed 是**毫秒**，要 /1000 才是 JS 的时间戳 */}
                  {info.last_played_ms
                    ? new Date(info.last_played_ms).toLocaleString('zh-CN', { hour12: false })
                    : '—'}
                </span>
                <span className="sub">存档里的 LastPlayed</span>
              </div>
              <div className="gs-item">
                <span className="k">服务端</span>
                <span className="v">{info.server_brands.join('、') || '—'}</span>
                <span className="sub">数据版本 {info.data_version}</span>
              </div>
              <div className="gs-item">
                <span className="k">已加载维度</span>
                <span className="v small">
                  {info.dimensions.map((d) => d.replace(/^minecraft:/, '')).join(' · ') || '—'}
                </span>
                <span className="sub">来自 WorldGenSettings</span>
              </div>
              <div className="gs-item">
                <span className="k">数据包</span>
                <span className="v small">
                  启用 {info.datapacks_enabled.length} 个
                  {info.datapacks_disabled.length > 0 ? ` · 停用 ${info.datapacks_disabled.length} 个` : ''}
                </span>
                <span className="sub">
                  {info.datapacks_enabled.map((d) => d.replace(/^(file|mod):\//, '')).join(', ')}
                </span>
              </div>
            </div>
          </section>

          {/* ---------- 原版规则 ---------- */}
          <section className="gs-card">
            <div className="gs-card-head">
              <span className="gs-card-title">原版游戏规则</span>
              <span className="muted">共 {info.gamerules.length} 条</span>
            </div>

            {/* 规则只能在服务器运行时改：不给出假入口，直接说明白 */}
            {!canEditRules && (
              <div className="gs-locked">
                {!running
                  ? '实例未在运行：游戏规则通过原版 /gamerule 修改，需要先启动实例。直接改存档里的规则会在服务器启动时被覆写，所以不做那种事。'
                  : '你没有修改规则的权限（需要能向控制台发送指令的权限）。'}
              </div>
            )}

            {/* 保存时机说明：这是最容易让人以为"没生效"的地方。
                /gamerule 立即改的是服务器内存值，存档（本页读的来源）要等
                世界保存才更新 —— 实测踩过，必须写出来。 */}
            {canEditRules && (
              <div className="gs-timing">
                规则修改<strong>立即生效</strong>（服务器内存中），改完本页会显示已确认的新值；
                本页的初始数据读自存档，会在服务器<strong>保存世界</strong>后与之一致。
              </div>
            )}

            {GROUPS.map((group) => {
              const items = group.keys.filter((k) => ruleMap[k])
              if (items.length === 0) return null
              return (
                <div className="gs-group" key={group.title}>
                  <div className="gs-group-title">{group.title}</div>
                  {items.map(renderRule)}
                </div>
              )
            })}

            {others.length > 0 && (
              <details className="gs-group gs-others">
                <summary className="gs-group-title">
                  其它规则（{others.length} 条，未归类）
                </summary>
                {others.map(renderRule)}
              </details>
            )}
          </section>
        </>
      )}

      <div className="gs-foot">
        <button onClick={load} disabled={loading}>{loading ? '刷新中…' : '刷新'}</button>
        <span className="muted">
          世界信息读自节点上的 <code>level.dat</code>（只读）；规则修改走控制台，立即生效。
        </span>
      </div>
    </div>
  )
}
