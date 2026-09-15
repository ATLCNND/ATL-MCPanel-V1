import { ReactNode } from 'react'
import { User } from '../api'
import Avatar from './Avatar'
import BrandLogo from './BrandLogo'
import ThemeToggle from './ThemeToggle'
import './AppShell.css'

/** 全局导航项 */
export type NavKey = 'dashboard' | 'instances' | 'monitor' | 'nodes' | 'tunnels' | 'audit' | 'users' | 'avatars' | 'account' | 'help'

/**
 * 全局导航项的**唯一定义处**。
 *
 * 外壳侧边栏与实例详情页左栏都用它 —— 以前两处各写一份，
 * 结果加了「账户」「公告与帮助」之后，实例页那份没跟着加，
 * 于是在实例页里**根本进不去这两个页面**。
 * 同类漂移只会在"同一份清单写两遍"的地方反复发生，所以这里导出共用。
 *
 * 实例页那份会在此基础上插一个「告警」（那一项在实例页走弹窗而不是页面），
 * 见 InstanceDetail 的 GLOBAL_NAV。
 */
export const NAV_ITEMS: { key: NavKey; label: string; icon: string; adminOnly?: boolean }[] = [
  { key: 'dashboard', label: '总览', icon: '▦' },
  { key: 'instances', label: '实例管理', icon: '▤' },
  { key: 'monitor', label: '节点监控', icon: '◔' },
  { key: 'nodes', label: '节点管理', icon: '◇', adminOnly: true },
  { key: 'tunnels', label: '穿透管理', icon: '⇄', adminOnly: true },
  // 用户管理 / 头像审核 / 审计日志：原来挤在「账户设置」弹窗里，
  // 而那是个"改自己密码"的地方 —— 管理功能放在那里既不好找，弹窗也撑不住表格。
  // 现已统一提到总导航。
  { key: 'users', label: '用户管理', icon: '☷', adminOnly: true },
  { key: 'avatars', label: '头像审核', icon: '◍', adminOnly: true },
  { key: 'audit', label: '审计日志', icon: '≡', adminOnly: true },
  // 账户：所有角色都有。原来是弹窗，改成页面后能看到自己的实例、端口配额、磁盘与权限清单。
  { key: 'account', label: '账户', icon: '☺' },
  // 公告与帮助：公告是管理员对全体用户说话的地方，帮助是"面板怎么用"。
  // 两者同页 —— 用户想看"怎么用"时不用先猜是哪个入口。
  { key: 'help', label: '公告与帮助', icon: '※' },
]

export interface NavBadge {
  /** 需要管理员权限才显示 */
  adminOnly?: boolean
  /** 右侧计数徽标（如告警数量） */
  count?: number
}

/**
 * AppShell 应用外壳：左侧固定导航 + 右侧内容区。
 *
 * 设计要点：
 *  - 导航常驻，切换页面不再需要「返回」按钮——这是原来的顶栏方案最主要的痛点
 *  - 页面标题/副标题/操作按钮由各页通过 props 传入，统一排版
 *  - 用户卡片固定在左下角，点击打开账户设置
 */
export default function AppShell({
  active,
  title,
  subtitle,
  actions,
  user,
  alertCount = 0,
  onNavigate,
  onOpenAccount,
  onOpenAlerts,
  bare = false,
  avatarUrl = '',
  children,
}: {
  active: NavKey
  title: string
  subtitle?: string
  actions?: ReactNode
  user: User | null
  alertCount?: number
  onNavigate: (key: NavKey) => void
  onOpenAccount: () => void
  onOpenAlerts?: () => void
  /**
   * bare 模式：不渲染外壳侧边栏与页头，由页面自行组织布局。
   * 用于实例详情页——它自带的左栏同时包含全局导航与实例导航，
   * 若再叠加外壳侧边栏会导致全局导航重复且挤占主内容宽度。
   */
  bare?: boolean
  /**
   * 当前用户**已审核通过**的头像地址（没有则留空 → 回退字母头像）。
   * 由 App 统一取一次再传下来：外壳左下角、账户卡片、账户页三处要用同一个值，
   * 各自去拉会不一致（以前就是漏了这里的头像，只显示字母）。
   */
  avatarUrl?: string
  children: ReactNode
}) {
  const isAdmin = user?.role === 'admin'

  const items = NAV_ITEMS

  if (bare) {
    return <div className="shell-bare">{children}</div>
  }

  return (
    <div className="shell">
      <aside className="shell-nav">
        <div className="shell-logo">
          <BrandLogo size={26} />
          <span>ATL-MCPanel</span>
        </div>

        <nav className="shell-menu">
          {items
            .filter((it) => !it.adminOnly || isAdmin)
            .map((it) => (
              <button
                key={it.key}
                className={`shell-item ${active === it.key ? 'active' : ''}`}
                onClick={() => onNavigate(it.key)}
              >
                <span className="shell-item-icon">{it.icon}</span>
                <span>{it.label}</span>
              </button>
            ))}

          {isAdmin && onOpenAlerts && (
            <button className="shell-item" onClick={onOpenAlerts}>
              <span className="shell-item-icon">◈</span>
              <span>告警</span>
              {alertCount > 0 && <span className="shell-item-count">{alertCount}</span>}
            </button>
          )}
        </nav>

        <div className="shell-nav-bottom">
          {/* 主题切换按钮已移到页头右上角（见下面 .shell-head-actions）。
              实例详情页是 bare 模式、没有页头，它左栏里单独放了一份，
              两处共用 ThemeToggle 组件并订阅同一个主题状态。 */}

          <button className="shell-user" onClick={onOpenAccount} title="账户设置">
            {/* 头像用 Avatar 组件：以前这里写死了首字母，用户传了头像也看不到 */}
            <Avatar url={avatarUrl} name={user?.username} size={44} />
            <span className="shell-user-meta">
              <b>{user?.username || '未登录'}</b>
              <small>{isAdmin ? '管理员' : '用户'}</small>
            </span>
          </button>
        </div>
      </aside>

      <main className="shell-main">
        <header className="shell-head">
          <div className="shell-head-text">
            <h1>{title}</h1>
            {subtitle && <p>{subtitle}</p>}
          </div>
          {/* 这个容器现在总是渲染：即使页面没传 actions，
              主题切换按钮也要在（它是全站唯一的主题入口之一）。 */}
          <div className="shell-head-actions">
            {actions}
            {/* showLabel：把"深色/浅色模式"写出来。只放一个月亮图标时，
                用户根本注意不到这里可以切主题（本轮用户反馈"不够醒目"）。 */}
            <ThemeToggle showLabel />
          </div>
        </header>
        <div className="shell-body">{children}</div>
      </main>
    </div>
  )
}

