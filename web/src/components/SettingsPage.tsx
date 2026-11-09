import { ReactNode, useEffect, useMemo, useState } from 'react'
import UsersPage from './UsersPage'
import AvatarReview from './AvatarReview'
import AnalysisProviders from './AnalysisProviders'
import TlsSettings from './TlsSettings'
import { canManageProviders } from './AppShell'
import './SettingsPage.css'

/**
 * 面板设置（C1 + C2）。
 *
 * 为什么要有这一页：
 *  - 这些设置原来散在总导航的三个入口里（用户管理 / 头像审核 / 分析平台），
 *    再加上「关于」页里孤零零的 HTTPS 证书卡片。它们都是"面板自己怎么运作"，
 *    与某个实例无关，却混在实例管理、节点管理这些日常入口之间；
 *  - 管理员真正需要的是一个地方把"谁能登录、头像怎么审、日志交给谁、证书用哪张"
 *    一次看完，而不是猜它藏在哪个页面的哪个角落。
 *
 * 用页内左右翻页而不是四个并列区块：
 *  用户管理是一张大表格（含节点授权与端口配额），头像审核是一屏图片网格，
 *  把它们上下堆在一页里会变成一条几米长的滚动条，并且**四个数据源同时加载**。
 *  翻页式一次只挂载一页，进入即是一次干净的加载。
 */

type PageKey = 'users' | 'avatars' | 'providers' | 'tls'

export type SettingsPageKey = PageKey

interface PageDef {
  key: PageKey
  title: string
  icon: string
  /** 页签悬浮提示 */
  hint: string
  /** 页面顶部那句说明：为什么这里有这一页 */
  lead: ReactNode
  /** 该角色是否可见 */
  visible: (role?: string) => boolean
}

const PAGES: PageDef[] = [
  {
    key: 'users',
    title: '用户管理',
    icon: '☷',
    hint: '账号、角色、节点授权与端口配额',
    visible: (r) => r === 'admin',
    lead: (
      <>
        账号、角色，以及两项资源分配 —— <strong>管哪些节点</strong>、
        <strong>在每条线路上有几个穿透端口</strong>。
        只改角色却忘了分配节点，用户点不动任何按钮；只分节点却忘了给端口，
        他建完实例发现开不了公网口。所以这三件事在同一页。
      </>
    ),
  },
  {
    key: 'avatars',
    title: '头像审核',
    icon: '◍',
    hint: '通过后才会对外显示',
    visible: (r) => r === 'admin',
    lead: (
      <>
        用户上传的头像默认为「待审核」，<strong>通过后才会对外显示</strong>；
        未通过时其他人看到的是字母头像。这里能看到待审图片本身，
        驳回时可填写原因，用户会在自己的账号卡上看到。
      </>
    ),
  },
  {
    key: 'providers',
    title: '分析平台',
    icon: '⌘',
    hint: '日志交给谁分析、用什么凭据',
    visible: (r) => canManageProviders(r),
    lead: (
      <>
        日志分析交给谁：LogShare、mclo.gs、自建 OpenAI 兼容网关，或什么都不接、只用内置规则。
        API Key 加密保存、永不回显；平台地址默认禁止内网，自建网关在内网时由管理员显式放行。
      </>
    ),
  },
  {
    key: 'tls',
    title: '域名与证书',
    icon: '⛨',
    hint: 'HTTPS 证书：自签或上传正式证书',
    visible: (r) => r === 'admin',
    lead: (
      <>
        面板自己的 HTTPS 证书。自签证书浏览器会警告，只适合内网；
        对外服务请上传正式证书。<strong>改完要重启面板才生效</strong> ——
        这一步只负责把文件放对位置，不会热加载。
      </>
    ),
  },
]

/** 记住上次看的是哪一页：从实例页返回设置页时不该总被甩回第一页 */
const STORE_KEY = 'atl.settings.page'

function readStored(): PageKey | '' {
  try {
    return (sessionStorage.getItem(STORE_KEY) as PageKey | null) || ''
  } catch {
    return ''
  }
}

export default function SettingsPage({ role, initial }: { role?: string; initial?: SettingsPageKey }) {
  const pages = useMemo(() => PAGES.filter((p) => p.visible(role)), [role])

  const [idx, setIdx] = useState<number>(() => {
    // 旧入口（导航里已经没有的 用户管理 / 头像审核 / 分析平台）带着 initial 进来时，
    // 直接停在他点的那一页，不要被上次记住的页顶掉。
    const want = initial || readStored()
    const i = pages.findIndex((p) => p.key === want)
    return i >= 0 ? i : 0
  })

  // 角色变了（比如被降权）导致页数变少时，别停在一个空页上
  const safeIdx = Math.min(idx, Math.max(pages.length - 1, 0))
  const cur = pages[safeIdx]

  useEffect(() => {
    if (safeIdx !== idx) setIdx(safeIdx)
  }, [safeIdx, idx])

  useEffect(() => {
    if (!cur) return
    try {
      sessionStorage.setItem(STORE_KEY, cur.key)
    } catch {
      /* 隐私模式下写不进去，无所谓 */
    }
  }, [cur])

  const goto = (n: number) => setIdx(Math.max(0, Math.min(pages.length - 1, n)))

  // 左右方向键翻页。刻意避开输入框：这一页里全是文本框与文本域，
  // 在输入框里按方向键必须仍然是移动光标。
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      if (e.altKey || e.ctrlKey || e.metaKey || e.shiftKey) return
      if (e.key !== 'ArrowLeft' && e.key !== 'ArrowRight') return
      const t = e.target as HTMLElement | null
      if (t) {
        const tag = t.tagName
        if (tag === 'INPUT' || tag === 'TEXTAREA' || tag === 'SELECT' || t.isContentEditable) return
      }
      // 有弹窗（比如头像驳回填原因）时不要顺手翻页
      if (document.querySelector('.modal-mask')) return
      e.preventDefault()
      setIdx((i) => Math.max(0, Math.min(pages.length - 1, i + (e.key === 'ArrowRight' ? 1 : -1))))
    }
    window.addEventListener('keydown', onKey)
    return () => window.removeEventListener('keydown', onKey)
  }, [pages.length])

  if (!cur) {
    return (
      <div className="page set-page">
        <p className="page-lead">当前角色没有可配置的面板设置。</p>
      </div>
    )
  }

  return (
    <div className="page set-page">
      <div className="set-bar">
        <div className="set-tabs" role="tablist" aria-label="面板设置分页">
          {pages.map((p, i) => (
            <button
              key={p.key}
              role="tab"
              aria-selected={i === safeIdx}
              title={p.hint}
              className={`set-tab${i === safeIdx ? ' active' : ''}`}
              onClick={() => goto(i)}
            >
              <span className="set-tab-icon" aria-hidden="true">{p.icon}</span>
              {p.title}
            </button>
          ))}
        </div>

        <div className="set-nav">
          <button disabled={safeIdx === 0} onClick={() => goto(safeIdx - 1)} title="上一页（←）">
            ← 上一页
          </button>
          <span className="set-count">
            第 {safeIdx + 1} / {pages.length} 页
          </span>
          <button
            disabled={safeIdx >= pages.length - 1}
            onClick={() => goto(safeIdx + 1)}
            title="下一页（→）"
          >
            下一页 →
          </button>
        </div>
      </div>

      <div className="set-body">
        <p className="set-lead">{cur.lead}</p>
        {cur.key === 'users' && <UsersPage />}
        {cur.key === 'avatars' && <AvatarReview />}
        {cur.key === 'providers' && <AnalysisProviders role={role} />}
        {cur.key === 'tls' && <TlsSettings />}
      </div>
    </div>
  )
}
