import { useState } from 'react'
import { login, setToken, setCurrentUser } from '../api'
import Brand from './Brand'
import './Login.css'

/**
 * 登录页。
 *
 * 名称、图标、版本号一律来自 `/api/meta`（免登录接口）——
 * 这里曾经写死过 `PANEL_VERSION = 'v0.1.0'`（那是 web/package.json 的版本，
 * 与面板版本不是一回事），于是界面显示 v0.1.0、二进制却是 0.9.14。
 * 现在只有一个来源：二进制里由 -ldflags 注入的那份。
 *
 * `notice` 用于"会话失效后回到这里"的场景（面板重启换了密钥、令牌到期）：
 * 直接回到登录页而不说明原因，用户会以为是自己点错了。
 */
export default function Login({ onLogin, notice }: { onLogin: () => void; notice?: string }) {
  const [username, setUsername] = useState('')
  const [password, setPassword] = useState('')
  const [error, setError] = useState('')
  const [loading, setLoading] = useState(false)

  const submit = async (e: React.FormEvent) => {
    e.preventDefault()
    setError('')
    setLoading(true)
    try {
      const data = await login(username, password)
      setToken(data.token)
      // 存下 UID：它是唯一不会变的标识（用户名可以改，见账户页的改名）。
      // 不存的话，改完名之后"这是不是我自己的账号"这类判断就会失效。
      setCurrentUser(data.id, data.username, data.role)
      onLogin()
    } catch (err: any) {
      setError(err.message || '登录失败')
    } finally {
      setLoading(false)
    }
  }

  return (
    <div className="login-wrap">
      <form className="login-box" onSubmit={submit}>
        {/* 品牌区（图标 + 名称 + 版本号）：上下排列，与侧边栏同源同数据 */}
        <div className="login-brand">
          <Brand size={64} layout="stack" />
        </div>

        <p className="subtitle">多用户 Minecraft 实例管理面板</p>

        {notice && <div className="login-notice">{notice}</div>}

        <input
          placeholder="用户名"
          value={username}
          onChange={(e) => setUsername(e.target.value)}
          autoFocus
        />
        <input
          placeholder="密码"
          type="password"
          value={password}
          onChange={(e) => setPassword(e.target.value)}
        />
        {error && <div className="error">{error}</div>}
        <button className="primary" type="submit" disabled={loading}>
          {loading ? '登录中...' : '登录'}
        </button>
      </form>
    </div>
  )
}
