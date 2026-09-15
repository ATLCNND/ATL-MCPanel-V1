import { useEffect, useState } from 'react'
import { getTheme, onThemeChange, toggleTheme, Theme } from '../styles/theme'
import './ThemeToggle.css'

/**
 * 深浅色切换按钮。
 *
 * 为什么抽成组件：它现在有**两处**入口 ——
 *   1. 外壳页头右上角（AppShell 的 .shell-head-actions）
 *   2. 实例详情页左栏（那页是 bare 模式，没有页头，否则就没法切主题了）
 * 两处必须显示同一个状态，所以内部订阅主题变化而不是各自持有 state。
 *
 * 注意：主题是写在 <html data-theme> 上的全局状态，
 * 任一处点击后另一处通过 onThemeChange 的 MutationObserver 自动跟上。
 */
export default function ThemeToggle({ showLabel = false, block = false }: { showLabel?: boolean; block?: boolean }) {
  const [theme, setTheme] = useState<Theme>(getTheme())

  // 订阅主题变化：另一处切换后，本组件要跟着更新图标与提示文字
  useEffect(() => onThemeChange(setTheme), [])

  const next: Theme = theme === 'dark' ? 'light' : 'dark'
  const label = theme === 'dark' ? '深色模式' : '浅色模式'

  return (
    <button
      className={`theme-toggle ${block ? 'block' : ''}`}
      onClick={() => setTheme(toggleTheme())}
      title={`当前：${label}，点击切换到${next === 'dark' ? '深色' : '浅色'}模式`}
      aria-label={`切换到${next === 'dark' ? '深色' : '浅色'}模式`}
    >
      <span className="theme-toggle-icon">{theme === 'dark' ? '☾' : '☀'}</span>
      {showLabel && <span>{label}</span>}
    </button>
  )
}
