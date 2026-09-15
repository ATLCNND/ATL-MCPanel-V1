/**
 * 面板品牌标记（logo）。
 *
 * 图片放在 web/public/branding/ 下 —— Vite 会把 public/ 原样发布到站点根，
 * 所以这里是站点绝对路径，**不经过打包器**：
 * 以后换 logo 只要替换文件，不用改代码、不用重新构建。
 *
 * 关于圆角（这段踩过坑，别删）：
 * 源图是「白色留白 + 自带圆角的粉色徽标」，而且**完全不透明**
 * （虽然 PNG 有 alpha 通道，但实测 alpha 全是 255）。
 * 试过用边界洪泛把白底扣成透明 —— 不行：这个 logo 的白色负空间
 * （"A" 的 counter、"T" 的横杠）与外部白底是**连通的**，
 * 扣背景会把笔画一起抠掉，徽标碎成拼图（已视觉复核确认）。
 *
 * 现在的做法：离线紧裁到粉色主体包围盒（去掉四周白色留白），
 * 圆角交给这里的 border-radius 去盖 —— 徽标自带的圆角正好落在裁剪区四角，
 * 被 CSS 圆角裁掉，于是深色/浅色主题下都不会露出白边。
 * 半径取边长的 27%，与源图自带圆角（约 18~20%）相近，略大以确保盖干净。
 */
export default function BrandLogo({
  size = 26,
  src = '/branding/logo-128.png',
  radiusRatio = 0.27,
  alt = 'ATL-MCPanel',
}: {
  /** 显示边长（px） */
  size?: number
  /** 图片地址；大尺寸用 /branding/logo.png，小尺寸用 logo-128.png */
  src?: string
  /** 圆角占边长的比例 */
  radiusRatio?: number
  alt?: string
}) {
  return (
    <img
      className="brand-logo"
      src={src}
      alt={alt}
      width={size}
      height={size}
      style={{
        width: size,
        height: size,
        borderRadius: Math.round(size * radiusRatio),
        objectFit: 'cover',
        display: 'block',
        flex: `0 0 ${size}px`,
      }}
    />
  )
}
