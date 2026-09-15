/**
 * Avatar 头像：有图片就用图片，否则回退到字母头像。
 *
 * 两个用途：
 *   - 用户头像（有审核通过的图才显示，见下方说明）
 *   - 实例图标（实例目录里的 server-icon.png / icon.png，见第十节第 1 项）
 *
 * 为什么要抽成组件：这段"有图用图、没图用首字母"的判断原先在
 * AppShell / AccountCard / AccountPage 里各写了一遍 ——
 * 结果外壳左下角那张卡片**漏了**，永远只显示字母，
 * 用户传了头像也看不到（这次就是被用户指出来的）。
 *
 * 关于"该不该显示"：后端 `/api/auth/me` 的 `avatar_url` **只在审核通过后才有值**
 * （见 profile.go 的 avatarURLFor：pending / rejected 一律返回空串），
 * 所以这里直接用即可，不需要前端再判一次状态 —— 也就不会再漏。
 *
 * 图片加载失败会自动回退字母头像（见下面 broken 状态）：这些图片都是用户自己
 * 目录里的文件，随时可能被删掉或换掉，浏览器默认会给个裂图 —— 那比没有更难看。
 */
import { useEffect, useState } from 'react'

export default function Avatar({
  url, name, size = 30, radius = '50%', className = '', initialsLength = 2,
}: {
  /** 头像图片地址；空串/undefined 则回退字母头像 */
  url?: string
  /** 用户名，用于取首字母 */
  name?: string
  /** 边长（px） */
  size?: number
  /** 圆角；默认正圆 */
  radius?: string | number
  className?: string
  /**
   * 字母头像取几个字。默认 2（用户名习惯）；实例名习惯只取 1 个
   * （34px 的方框里塞两个字会挤在一起）。
   */
  initialsLength?: number
}) {
  // 图片加载失败时回退字母头像。
  //
  // 为什么必须有：图标/头像都是**用户自己目录里的文件**，随时可能被删掉或换掉
  // （实例图标就放在实例目录里，用户在「文件管理」里删一下而已），
  // 而浏览器对失败图片的处理是显示一个裂图图标 —— 那比没有图标难看得多。
  const [broken, setBroken] = useState(false)

  const style: React.CSSProperties = {
    width: size,
    height: size,
    flex: `0 0 ${size}px`,
    borderRadius: radius,
  }

  // url 变化后要允许重新尝试（例如用户刚换了图标、mtime 变了）
  useEffect(() => { setBroken(false) }, [url])

  if (url && !broken) {
    return (
      <img
        className={`av av-img ${className}`}
        src={url}
        alt={name || '头像'}
        onError={() => setBroken(true)}
        style={{ ...style, objectFit: 'cover', display: 'block' }}
      />
    )
  }

  return (
    <span
      className={`av av-initial ${className}`}
      style={{
        ...style,
        display: 'grid',
        placeItems: 'center',
        background: 'var(--accent-bg)',
        color: 'var(--accent)',
        fontWeight: 700,
        // 字号跟着尺寸走：30px 时 11.5px、64px 时约 24px
        fontSize: Math.max(11, Math.round(size * 0.38)),
      }}
    >
      {(name || '?').slice(0, Math.max(1, initialsLength)).toUpperCase()}
    </span>
  )
}
