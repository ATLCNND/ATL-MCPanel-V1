import { useState } from 'react'
import { login, setToken, setCurrentUser, basePath } from '../api'
import { copyText } from '../clipboard'
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
  const [copied, setCopied] = useState('')

  /**
   * 启用自定义安全入口（`server.security_entry`）时，面板**只在** `/<入口>/` 下提供服务。
   *
   * 为什么要在这里显示完整地址：入口是安装时随机生成的 12 位，用户手上只有
   * 安装脚本打印的那一行。等他关掉终端、过几天再打开面板，只会看到 404
   * （而且那个 404 是**不带面板特征**的，看不出是"入口写错了"还是"服务没起"）。
   * 登录页是他唯一一定会看到的页面，把地址摆在这里最省事。
   */
  const entry = basePath()
  const fullURL = entry ? `${window.location.origin}${entry}/` : ''

  const copyEntry = async () => {
    setCopied(await copyText(fullURL) ? '已复制' : '复制失败，请手动选中')
    setTimeout(() => setCopied(''), 2500)
  }

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

        {/* 安全入口提示：只在启用时出现（普通部署看不到这一块） */}
        {entry && (
          <div className="login-entry">
            <div className="login-entry-title">本站启用了自定义安全入口</div>
            <div className="login-entry-row">
              <code className="login-entry-url">{fullURL}</code>
              <button type="button" className="login-entry-copy" onClick={copyEntry}>
                {copied || '复制'}
              </button>
            </div>
            <div className="login-entry-hint">
              只有这个地址（含 <code>{entry}/</code>）能打开面板，其它路径一律 404。
              建议加入书签；忘记入口可在节点上查看面板配置的 <code>server.security_entry</code>。
            </div>
          </div>
        )}
      </form>
    </div>
  )
}
