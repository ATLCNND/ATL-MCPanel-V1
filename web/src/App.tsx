import { useCallback, useEffect, useState } from 'react'
import { getToken, clearToken, listAlerts, currentUser, getMyProfile, User } from './api'
import AppShell, { NavKey } from './components/AppShell'
import Login from './components/Login'
import Instances from './components/Instances'
import InstanceDetail from './components/InstanceDetail'
import TunnelsPage from './components/TunnelsPage'
import NodesPage from './components/NodesPage'
import MonitorPage from './components/MonitorPage'
import AuditPage from './components/AuditPage'
import UsersPage from './components/UsersPage'
import AvatarReview from './components/AvatarReview'
import DashboardPage from './components/DashboardPage'
import AccountPage from './components/AccountPage'
import HelpPage from './components/HelpPage'
import AlertsModal from './components/AlertsModal'

/** 当前视图：全局页面，或某个实例的详情 */
type View = NavKey | { id: string; name: string; status: string; level: string }

/**
 * 各全局页面的标题与副标题。
 *
 * 原来是一大串嵌套三元表达式（每加一个页面就要再嵌一层，非常容易改错），
 * 改成查表：加页面时**只在这里加一行**，漏了会被 TypeScript 挡住
 * （Record<NavKey, …> 要求覆盖全部 key）。
 */
const PAGE_META: Record<NavKey, { title: string; subtitle: string }> = {
  dashboard: { title: '控制台总览', subtitle: '从这里开始管理您的全部实例与资源' },
  instances: { title: '实例列表', subtitle: '查看并管理你的所有实例' },
  monitor: { title: '节点监控', subtitle: '查看各节点的运行状况与资源占用' },
  nodes: { title: '节点管理', subtitle: '登记与部署节点上的 Daemon' },
  tunnels: { title: '穿透管理', subtitle: '管理实例的公网穿透线路' },
  users: { title: '用户管理', subtitle: '账号、角色，以及节点用户的节点授权' },
  avatars: { title: '头像审核', subtitle: '审核用户上传的头像（通过后才会对外显示）' },
  audit: { title: '审计日志', subtitle: '查看所有管理操作记录' },
  account: { title: '账户', subtitle: '我的资源、权限与账号设置' },
  help: { title: '公告与帮助', subtitle: '管理员公告，以及面板使用说明' },
}

/**
 * 头像审核页。
 *
 * AvatarReview 组件本身已经有标题与说明，这里只补一句"为什么需要人工审核"
 * 的上下文 —— 单独成一个页面后，它不再有弹窗里的语境了。
 */
function AvatarReviewPage() {
  return (
    <div className="page">
      <p className="page-lead">
        用户上传的头像默认为「待审核」，<strong>通过后才会对外显示</strong>；
        未通过时其他人看到的是字母头像。这里可以看到待审图片本身，
        驳回时可填写原因，用户会在自己的账号卡上看到。
      </p>
      <AvatarReview />
    </div>
  )
}

export default function App() {
  const [authed, setAuthed] = useState<boolean>(!!getToken())
  const [view, setView] = useState<View>('dashboard')
  const [user, setUser] = useState<User | null>(null)
  const [avatarUrl, setAvatarUrl] = useState('')
  const [alertCount, setAlertCount] = useState(0)
  const [showAlerts, setShowAlerts] = useState(false)

  const isInstanceDetail = typeof view === 'object'

  // 用户信息来自本地存储（登录时写入），读取是同步的
  const loadUser = useCallback(() => {
    setUser(currentUser())
  }, [])

  useEffect(() => {
    if (!authed) return
    loadUser()
  }, [authed, loadUser])

  // 头像地址：外壳左下角、实例页的账户卡片、账户页三处都要用同一个值，
  // 所以统一在这里取一次再往下传（以前外壳那张卡片写死了首字母，
  // 用户传了头像也看不到）。
  //
  // 跟着 view 一起刷新：在账户页换了头像之后，切页回来就能看到新的；
  // 这是个很小的 JSON 请求，划算。avatar_url 只在审核通过后才有值，
  // 所以这里不需要再判 avatar_status。
  useEffect(() => {
    if (!authed) return
    getMyProfile()
      .then((p) => setAvatarUrl(p?.avatar_url || ''))
      .catch(() => setAvatarUrl(''))
  }, [authed, view])

  // 告警数量（仅管理员）
  useEffect(() => {
    if (!authed || user?.role !== 'admin') return
    const load = () =>
      listAlerts()
        .then((r) => setAlertCount(Array.isArray(r) ? r.filter((a: any) => a.active).length : 0))
        .catch(() => { /* 忽略 */ })
    load()
    const t = window.setInterval(load, 60000)
    return () => window.clearInterval(t)
  }, [authed, user?.role])

  if (!authed) {
    return (
      <Login
        onLogin={() => {
          setAuthed(true)
          loadUser()
        }}
      />
    )
  }

  const handleLogout = () => {
    clearToken()
    setAuthed(false)
    setUser(null)
    setView('dashboard')
  }

  const navigate = (key: NavKey) => setView(key)

  // 实例详情页：仍在外壳内，但自带页头（含实例名与开关机操作）
  if (isInstanceDetail) {
    return (
      <>
        <InstanceDetail
          instanceId={view.id}
          name={view.name}
          status={view.status}
          level={view.level}
          user={user}
          alertCount={alertCount}
          onBack={() => setView('instances')}
          onNavigate={navigate}
          onOpenAccount={() => setView('account')}
          onOpenAlerts={() => setShowAlerts(true)}
        />
        {showAlerts && (
          <AlertsModal
            onClose={() => {
              setShowAlerts(false)
              setAlertCount(0)
            }}
          />
        )}
      </>
    )
  }

  const page = (() => {
    switch (view) {
      case 'dashboard':
        return (
          <DashboardPage
            user={user}
            onOpenInstance={(id, n, st, lv) => setView({ id, name: n, status: st, level: lv })}
            onNavigate={(k) => setView(k)}
          />
        )
      case 'monitor':
        return <MonitorPage />
      case 'nodes':
        return <NodesPage />
      case 'tunnels':
        return <TunnelsPage />
      case 'users':
        return <UsersPage />
      case 'avatars':
        return <AvatarReviewPage />
      case 'audit':
        return <AuditPage />
      case 'account':
        return <AccountPage onLogout={handleLogout} />
      case 'help':
        return <HelpPage isAdmin={user?.role === 'admin'} />
      default:
        return <Instances onOpen={(id, name, status, level) => setView({ id, name, status, level })} />
    }
  })()

  return (
    <>
      <AppShell
        active={view as NavKey}
        title={PAGE_META[view as NavKey].title}
        subtitle={PAGE_META[view as NavKey].subtitle}
        user={user}
        alertCount={alertCount}
        avatarUrl={avatarUrl}
        onNavigate={navigate}
        onOpenAccount={() => setView('account')}
        onOpenAlerts={() => setShowAlerts(true)}
      >
        {page}
      </AppShell>

      {showAlerts && (
        <AlertsModal
          onClose={() => {
            setShowAlerts(false)
            setAlertCount(0)
          }}
        />
      )}
    </>
  )
}
