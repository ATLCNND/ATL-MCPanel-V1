import { useState } from 'react'
import { login, setToken, setCurrentUser } from '../api'
import BrandLogo from './BrandLogo'
import './Login.css'

/**
 * 面板版本号。
 *
 * 与 web/package.json 的 version 保持一致 —— 这里手写常量而不是 import JSON，
 * 是因为把一个含注释的对象打进包里只为读一个字段不划算；
 * 升级版本时两处一起改（package.json 的 version 目前是 0.1.0）。
 */
const PANEL_VERSION = 'v0.1.0'

export default function Login({ onLogin }: { onLogin: () => void }) {
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
      setCurrentUser(data.username, data.role)
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
        <div className="login-brand">
          <BrandLogo size={64} src="/branding/logo.png" radiusRatio={0.24} />
        </div>

        <h1>ATL-MCPanel</h1>
        {/* 描述刻意不写死某个核心：面板是核心无关的
            （Java / 基岩 / 各种服务端与脚本都走同一套实例管理） */}
        <p className="subtitle">多用户 Minecraft 实例管理面板</p>

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

        <p className="ver">{PANEL_VERSION}</p>
      </form>
    </div>
  )
}
