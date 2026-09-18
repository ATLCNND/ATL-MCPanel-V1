/**
 * server.properties 各配置项的中文说明。
 *
 * 用在配置页每一项的**键名下方**（小字灰字），让不熟悉原版配置的用户
 * 不用去查 wiki 就知道这一项在管什么。
 *
 * 两条约定：
 *
 *  1. **键名保持英文**，只有说明是中文。键名就是文件里的真实内容
 *     （`server.properties` 的这一行），翻成中文会让用户对不上文件、
 *     也没法和网上资料/插件文档对照。
 *  2. 说明文字尽量带上**取值含义**（`-1=禁用`、`0=不踢`、`1-4` 这类），
 *     因为用户真正会问的是"填多少合适"，而不是"这一项叫什么"。
 *
 * 文案由面板作者提供（覆盖 1.20.1 的全部 57 个标准项）。将来若要做中英切换，
 * 只需把这个模块换成按语言取值的实现，配置页本身不用改。
 */
export const SERVER_PROPERTY_DESC: Record<string, string> = {
  'allow-flight': '允许飞行',
  'allow-nether': '允许进入下界',
  'broadcast-console-to-ops': '控制台指令的输出广播给 OP',
  'broadcast-rcon-to-ops': 'RCON 指令的输出广播给 OP',
  'debug': '调试模式',
  'difficulty': '难度（peaceful/easy/normal/hard）',
  'enable-command-block': '启用命令方块',
  'enable-jmx-monitoring': '启用 JMX 监控',
  'enable-query': '启用 Query 查询（供第三方工具读取服务器信息）',
  'enable-rcon': '启用 RCON',
  'enable-status': '启用服务器列表状态（false=多人列表里显示离线）',
  'enforce-secure-profile': '强制安全配置（要求玩家账户具备签名公钥）',
  'enforce-whitelist': '白名单强制模式',
  'entity-broadcast-range-percentage': '实体广播范围百分比（默认 100）',
  'force-gamemode': '强制游戏模式（每次进服都用默认模式）',
  'function-permission-level': '函数权限等级（1-4，默认 2）',
  'gamemode': '默认游戏模式（survival/creative/adventure/spectator）',
  'generate-structures': '生成结构（村庄等）',
  'generator-settings': '生成器设置（自定义世界生成参数）',
  'hardcore': '极限模式',
  'hide-online-players': '隐藏在线玩家列表（true=列表里看不到在线玩家）',
  'initial-disabled-packs': '初始禁用的数据包',
  'initial-enabled-packs': '初始启用的数据包',
  'level-name': '世界存档目录名',
  'level-seed': '世界种子',
  'level-type': '世界类型',
  'max-chained-neighbor-updates': '最大连锁方块更新数（防止连锁更新卡顿）',
  'max-players': '最大同时在线玩家数',
  'max-tick-time': '单次 tick 最大耗时（毫秒，超时会被看门狗处理，-1=禁用）',
  'max-world-size': '世界边界半径',
  'motd': '服务器描述（玩家在多人列表看到的名字）',
  'network-compression-threshold': '网络压缩阈值（超过该字节数才压缩，-1=不压缩）',
  'online-mode': '正版验证（true=仅正版可进）',
  'op-permission-level': 'OP 权限等级（1-4，默认 4）',
  'player-idle-timeout': '挂机踢出时间（分钟，0=不踢）',
  'prevent-proxy-connections': '阻止代理连接（拒绝经代理/VPN 连入的玩家）',
  'pvp': '允许玩家互相伤害',
  'query.port': 'Query 查询端口',
  'rate-limit': '数据包速率限制（每秒上限，超出会被踢，0=不限）',
  'rcon.password': 'RCON 密码',
  'rcon.port': 'RCON 端口',
  'require-resource-pack': '强制使用资源包（未加载会被踢出）',
  'resource-pack': '资源包下载地址（URL）',
  'resource-pack-prompt': '资源包提示语（询问下载时显示的文字）',
  'resource-pack-sha1': '资源包 SHA-1 校验值',
  'server-ip': '服务器监听 IP（留空=绑定全部）',
  'server-port': '服务器端口',
  'simulation-distance': '模拟距离（实体更新范围，越大越吃性能）',
  'spawn-animals': '生成动物',
  'spawn-monsters': '生成怪物',
  'spawn-npcs': '生成村民',
  'spawn-protection': '出生点保护半径',
  'sync-chunk-writes': '同步区块写入',
  'text-filtering-config': '文本过滤配置（聊天文本过滤服务）',
  'use-native-transport': '使用原生网络传输（Linux 上为 epoll 优化）',
  'view-distance': '视距（越大越吃性能）',
  'white-list': '启用白名单',
}

/**
 * 配置页「只看常用项」勾选后保留的键。
 *
 * 挑选标准是"开服后大概率要改的"：内网/外网连不上（端口、正版验证）、
 * 服务器叫什么（motd）、多少人能进、卡不卡（视距/模拟距离/玩家上限）、
 * 玩法开关（模式、难度、PvP、飞行、下界、命令方块、白名单）。
 * 其余项保持默认几乎总是对的，收起来能让人一眼找到该改的那几个。
 */
export const COMMON_PROPERTY_KEYS = new Set([
  'motd', 'max-players', 'online-mode', 'gamemode', 'difficulty',
  'pvp', 'view-distance', 'simulation-distance', 'spawn-protection',
  'allow-flight', 'enable-command-block', 'white-list',
  'server-port', 'level-name', 'allow-nether', 'enable-rcon',
])
