/**
 * 服务端时间的解析与格式化 —— **所有来自服务端的时间串都必须经过这里**。
 *
 * 为什么需要它（2026-10-02 用户反馈"面板的时间似乎有问题"）：
 *
 * 面板的库里，时间列是 SQLite 的 `CURRENT_TIMESTAMP` 写进去的，而它**是 UTC**
 *（`datetime('now')` 也一样）。后端把它当字符串原样返回，形如
 * `2026-10-01 09:30:46` —— **没有任何时区标记**。
 * 而浏览器的 `new Date("2026-10-01 09:30:46")` 会把它按**本地时间**解析
 *（这是 ECMAScript 对非 ISO 格式的历史行为，且各家实现一致）。
 * 于是 UTC+8 的用户看到的每一个时间**都早了 8 小时**：上午 9:30 上传的分析记录
 * 显示成 9:30，而实际是下午 17:30。
 *
 * 这个偏差不会报错、也不会让任何功能失效，只是"时间看着不对"，所以特别容易
 * 被当成"就是那时候"而放过 —— 分析记录、审计日志、备份时间全都受影响。
 *
 * 两种正确的源头修法都行：后端序列化成带 `Z` 的 RFC3339，或前端在这里补上。
 * 现在选前端补：后端有十几处返回时间串的地方，逐个改要动 proto/JSON 兼容性；
 * 而前端只需要一个函数，且**新增页面时不容易再写错**（只要用这个函数）。
 */
export function parseServerTime(s: string): Date {
  if (!s) return new Date(NaN)
  let v = s.trim()
  if (!v) return new Date(NaN)
  // 已经是 ISO 形态（有 T）或已带时区标记（Z / +08:00）就原样交给 Date
  const hasZone = /[zZ]$|[+-]\d{2}:?\d{2}$/.test(v)
  if (!hasZone) {
    // `2026-10-01 09:30:46` → `2026-10-01T09:30:46Z`
    v = v.replace(' ', 'T') + 'Z'
  } else if (!v.includes('T')) {
    v = v.replace(' ', 'T')
  }
  return new Date(v)
}

/** 服务端时间 → 本地可读字符串；无效或空值返回 fallback（默认 "—"）。 */
export function formatServerTime(s: string, fallback = '—'): string {
  const d = parseServerTime(s)
  if (isNaN(d.getTime())) return s ? s : fallback
  return d.toLocaleString('zh-CN', { hour12: false })
}

/**
 * 距今多久（"3 分钟前"）—— 用服务端时间算，别用 `Date.now()` 直接减字符串。
 */
export function serverTimeAgo(s: string): string {
  const d = parseServerTime(s)
  if (isNaN(d.getTime())) return ''
  const sec = Math.floor((Date.now() - d.getTime()) / 1000)
  if (sec < 0) return '刚刚'
  if (sec < 60) return `${sec} 秒前`
  if (sec < 3600) return `${Math.floor(sec / 60)} 分钟前`
  if (sec < 86400) return `${Math.floor(sec / 3600)} 小时前`
  return `${Math.floor(sec / 86400)} 天前`
}
