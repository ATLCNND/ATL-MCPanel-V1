// 复制文本到剪贴板：**两条路都试，并如实返回结果**。
//
// 为什么不能直接 `navigator.clipboard?.writeText(...)`：
//   · `navigator.clipboard` 只在**安全上下文**（https 或 localhost）存在。
//     面板常以 `http://<公网IP>:端口` 访问 —— 那时它是 undefined，
//     可选链 `?.` 会把整句变成「静默什么都不做」：按钮点了毫无反应，
//     而且没有任何报错（用户 2026-10-24 反馈实例页右上角「复制」就是这样失效的）。
//   · `document.execCommand('copy')` 兜底也有前提：文档要处于可聚焦/可编辑上下文，
//     否则同样静默失败。所以必须**临时造一个 textarea、聚焦并选中**，
//     而不是依赖用户此前点过哪里。
//
// 返回值是真实结果：调用方应当据此给出「已复制 / 复制失败」的反馈，
// 不要把「调用了 API」当成「复制成功」。
export async function copyText(text: string): Promise<boolean> {
  if (!text) return false

  // 路子 1：现代剪贴板 API（安全上下文可用；也可能因缺少用户激活而 reject）
  if (typeof navigator !== 'undefined' && navigator.clipboard?.writeText) {
    try {
      await navigator.clipboard.writeText(text)
      return true
    } catch {
      // 落到下面的兜底
    }
  }

  // 路子 2：execCommand 兜底（http 页面就靠这条）
  try {
    const ta = document.createElement('textarea')
    ta.value = text
    // 放在视口外而不是 display:none —— 部分浏览器拒绝 select() 不可见的元素
    ta.style.position = 'fixed'
    ta.style.top = '-9999px'
    ta.style.left = '-9999px'
    ta.setAttribute('readonly', '')
    document.body.appendChild(ta)
    const prevFocus = document.activeElement as HTMLElement | null
    ta.select()
    ta.setSelectionRange(0, text.length)
    const ok = document.execCommand('copy')
    document.body.removeChild(ta)
    // 把焦点还回去，别打断用户正在输入的内容
    prevFocus?.focus?.()
    return ok
  } catch {
    return false
  }
}

// 剪贴板 API 当前是否可用（仅用于界面上的提示文案，不用于判断能否复制）
export function clipboardApiAvailable(): boolean {
  return typeof navigator !== 'undefined' && !!navigator.clipboard?.writeText
}
