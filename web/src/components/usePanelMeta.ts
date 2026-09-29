import { useEffect, useState } from 'react'
import { FALLBACK_META, PanelMeta, getPanelMeta, isTokenExpired, notifyAuthExpired } from '../api'

/**
 * 面板身份信息（名称 / 版本 / 图标）的**全局单例**读取。
 *
 * 为什么做成模块级缓存而不是每个组件各拉一次：
 * 侧边栏、实例页左栏、登录页都要显示它，而这是个随页面加载只需一次的小请求。
 * 各拉一次不仅浪费，更麻烦的是**显示会不一致**（登录页拿到了、侧边栏没有）。
 *
 * 失败时回落到 FALLBACK_META：品牌区宁可少显示版本号，也不能整块消失 ——
 * 那会让"面板是不是坏了"变成新的疑问。
 */
let cache: PanelMeta | null = null
let inflight: Promise<PanelMeta> | null = null
const listeners = new Set<(m: PanelMeta) => void>()

function load(): Promise<PanelMeta> {
  if (cache) return Promise.resolve(cache)
  if (!inflight) {
    inflight = getPanelMeta()
      .then((m) => {
        cache = { ...FALLBACK_META, ...m }
        listeners.forEach((fn) => fn(cache as PanelMeta))
        return cache
      })
      .catch(() => FALLBACK_META)
      .finally(() => { inflight = null })
  }
  return inflight
}

export function usePanelMeta(): PanelMeta {
  const [meta, setMeta] = useState<PanelMeta>(cache || FALLBACK_META)

  useEffect(() => {
    let alive = true
    load().then((m) => { if (alive) setMeta((prev) => (prev === m ? prev : m)) })
    const fn = (m: PanelMeta) => { if (alive) setMeta(m) }
    listeners.add(fn)
    return () => { alive = false; listeners.delete(fn) }
  }, [])

  // 浏览器标题也跟着面板名走：多标签页时，"哪个标签是面板"应当一眼可辨
  useEffect(() => {
    if (meta.name) document.title = meta.name + ' · 多用户实例管理面板'
  }, [meta.name])

  return meta
}

/**
 * 会话看门狗：每 30 秒检查一次本地令牌的到期时间。
 *
 * 为什么需要：用户把面板开着不动时没有任何请求，服务端也就没机会回 401，
 * 界面会一直显示"已登录"，一点操作才被踢出去。这里按 JWT 的 exp 本地判断，
 * 到点就切回登录页并说明原因。
 *
 * 只做本地判断，**不发请求**（也就不消耗服务端资源、不受网络影响）。
 */
export function useSessionWatchdog(enabled: boolean) {
  useEffect(() => {
    if (!enabled) return
    const tick = () => {
      if (isTokenExpired()) notifyAuthExpired('登录已过期，请重新登录')
    }
    tick()
    const t = window.setInterval(tick, 30000)
    return () => window.clearInterval(t)
  }, [enabled])
}
