/**
 * 主题管理。
 *
 * 主题通过 <html data-theme="dark|light"> 切换 —— 所有颜色都在
 * styles/tokens.css 中以 CSS 变量定义，切换主题只是换一组变量值，
 * 组件样式无需任何感知。
 *
 * 默认 dark：与重构前的外观保持一致。
 */

export type Theme = 'dark' | 'light'

const STORAGE_KEY = 'atlmcpanel.theme'

/** 读取已保存的主题；未设置时跟随系统偏好，系统未声明则用深色。 */
export function getTheme(): Theme {
  try {
    const saved = localStorage.getItem(STORAGE_KEY)
    if (saved === 'dark' || saved === 'light') return saved
  } catch {
    /* localStorage 不可用（隐私模式等）时忽略 */
  }
  if (typeof window !== 'undefined' && window.matchMedia) {
    return window.matchMedia('(prefers-color-scheme: light)').matches ? 'light' : 'dark'
  }
  return 'dark'
}

/** 应用主题到 DOM。 */
export function applyTheme(theme: Theme): void {
  document.documentElement.setAttribute('data-theme', theme)
}

/**
 * 订阅主题变化。
 *
 * 为什么需要：**xterm.js 渲染到 canvas，不读 CSS 变量**，
 * 其配色必须在主题切换后由 JS 重新写入 theme 选项。
 * 这类"非 CSS 驱动"的组件（xterm、canvas 图表等）都要用这个钩子。
 *
 * 返回取消订阅函数。
 */
export function onThemeChange(fn: (theme: Theme) => void): () => void {
  const observer = new MutationObserver(() => {
    const cur = document.documentElement.getAttribute('data-theme')
    fn(cur === 'light' ? 'light' : 'dark')
  })
  observer.observe(document.documentElement, { attributes: true, attributeFilter: ['data-theme'] })
  return () => observer.disconnect()
}

/** 读取 CSS 变量的当前计算值（供 canvas / 终端等非 CSS 渲染使用）。 */
export function cssVar(name: string, fallback = ''): string {
  const v = getComputedStyle(document.documentElement).getPropertyValue(name).trim()
  return v || fallback
}

/** 保存并应用主题。 */
export function setTheme(theme: Theme): void {
  try {
    localStorage.setItem(STORAGE_KEY, theme)
  } catch {
    /* 忽略写入失败 */
  }
  applyTheme(theme)
}

/** 在 dark / light 之间切换，返回切换后的主题。 */
export function toggleTheme(): Theme {
  const next: Theme = getTheme() === 'dark' ? 'light' : 'dark'
  setTheme(next)
  return next
}

/** 应用启动时调用：把已保存（或系统）的主题写到 DOM 上。 */
export function initTheme(): Theme {
  const t = getTheme()
  applyTheme(t)
  return t
}
