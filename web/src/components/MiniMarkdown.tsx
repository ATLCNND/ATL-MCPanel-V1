import { ReactNode } from 'react'
import './MiniMarkdown.css'

/**
 * MiniMarkdown —— 极小的 Markdown 子集渲染器（帮助文档用）。
 *
 * 为什么自己写而不引依赖：面板对依赖数量敏感（bundle 已经 600KB+），
 * 而帮助文档实际只需要 标题 / 列表 / 粗体 / 行内代码 / 链接 这五样。
 *
 * ⚠️ 为什么**不用** dangerouslySetInnerHTML：
 * 帮助文档是**管理员在界面上编辑**的。用 innerHTML 渲染等于把 XSS 能力
 * 交给管理员账号 —— 一旦那个账号被盗，攻击面就不只是"改个文档"，
 * 而是"对所有访问者执行任意脚本"。这里全部构造成 React 元素，
 * DOM 层面不可能注入标签。
 * 链接也只放行 http/https（正则里就限死了），挡住 `javascript:` 这类伪协议。
 *
 * 支持的语法：
 *   # ## ###      标题
 *   - 或 *        无序列表
 *   1. 2. …       有序列表
 *   空行          分段
 *   **粗体**  `行内代码`  [文字](https://链接)
 */
export default function MiniMarkdown({ text }: { text: string }) {
  const lines = (text || '').replace(/\r\n/g, '\n').split('\n')
  const blocks: ReactNode[] = []

  // 列表/段落都是"攒够一段再输出"，所以要跨行累积
  let ul: string[] | null = null
  let ol: string[] | null = null
  let para: string[] = []
  let k = 0

  const flushList = () => {
    if (ul) {
      blocks.push(<ul key={`ul${k++}`}>{ul.map((it, i) => <li key={i}>{inline(it, `u${k}-${i}`)}</li>)}</ul>)
      ul = null
    }
    if (ol) {
      blocks.push(<ol key={`ol${k++}`}>{ol.map((it, i) => <li key={i}>{inline(it, `o${k}-${i}`)}</li>)}</ol>)
      ol = null
    }
  }
  const flushPara = () => {
    if (para.length) {
      blocks.push(<p key={`p${k++}`}>{inline(para.join(' '), `p${k}`)}</p>)
      para = []
    }
  }

  for (const raw of lines) {
    const line = raw.trimEnd()
    if (!line.trim()) { flushList(); flushPara(); continue }

    const h = /^(#{1,3})\s+(.*)$/.exec(line)
    if (h) {
      flushList(); flushPara()
      const level = h[1].length
      const body = inline(h[2], `h${k}`)
      if (level === 1) blocks.push(<h3 key={`h${k++}`}>{body}</h3>)
      else if (level === 2) blocks.push(<h4 key={`h${k++}`}>{body}</h4>)
      else blocks.push(<h5 key={`h${k++}`}>{body}</h5>)
      continue
    }

    const bullet = /^[-*]\s+(.*)$/.exec(line)
    if (bullet) {
      flushPara()
      if (ol) { flushList() }
      if (!ul) ul = []
      ul.push(bullet[1])
      continue
    }

    const num = /^\d+[.)]\s+(.*)$/.exec(line)
    if (num) {
      flushPara()
      if (ul) { flushList() }
      if (!ol) ol = []
      ol.push(num[1])
      continue
    }

    flushList()
    para.push(line.trim())
  }
  flushList(); flushPara()

  if (!blocks.length) return null
  return <div className="md">{blocks}</div>
}

// 行内语法。注意链接的正则**只认 http/https**，这是安全边界，别放宽。
const INLINE = /(\*\*[^*]+\*\*|`[^`]+`|\[[^\]]+\]\(https?:\/\/[^\s)]+\))/g

/** 把一行文本里的行内语法渲染成 React 节点 */
function inline(text: string, keyPrefix: string): ReactNode[] {
  const out: ReactNode[] = []
  const re = new RegExp(INLINE)
  let last = 0
  let i = 0
  let m: RegExpExecArray | null

  while ((m = re.exec(text)) !== null) {
    if (m.index > last) out.push(text.slice(last, m.index))
    const tok = m[0]
    if (tok.startsWith('**')) {
      out.push(<strong key={`${keyPrefix}-b${i}`}>{tok.slice(2, -2)}</strong>)
    } else if (tok.startsWith('`')) {
      out.push(<code key={`${keyPrefix}-c${i}`}>{tok.slice(1, -1)}</code>)
    } else {
      const mm = /^\[([^\]]+)\]\((https?:\/\/[^\s)]+)\)$/.exec(tok)
      if (mm) {
        out.push(
          <a key={`${keyPrefix}-a${i}`} href={mm[2]} target="_blank" rel="noreferrer noopener">
            {mm[1]}
          </a>,
        )
      } else {
        out.push(tok)
      }
    }
    last = m.index + tok.length
    i++
  }
  if (last < text.length) out.push(text.slice(last))
  return out
}
