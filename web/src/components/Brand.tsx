import BrandLogo from './BrandLogo'
import { usePanelMeta } from './usePanelMeta'
import './Brand.css'

/**
 * 面板品牌区：图标 + 名称 + 版本号。
 *
 * 为什么抽成组件：侧边栏、实例详情页左栏、登录页三处都要它，而"名称与版本"
 * 必须**处处一致** —— 分别写三遍的结果就是某处忘了跟着配置改，
 * 或者版本号写死成前端 package.json 的版本（本项目真发生过：
 * 界面显示 v0.1.0，二进制其实是 0.9.14，报 bug 时这个号码把人带偏）。
 *
 * 版本号来自 /api/meta（免登录接口），只有二进制里那一个来源。
 */
export default function Brand({
  size = 26,
  layout = 'row',
  showVersion = true,
  className = '',
}: {
  /** 图标边长（px） */
  size?: number
  /** row：图标与文字并排（侧边栏）；stack：上下排列（登录页） */
  layout?: 'row' | 'stack'
  showVersion?: boolean
  className?: string
}) {
  const meta = usePanelMeta()
  const versionText = meta.version ? `v${meta.version}` : ''

  return (
    <div className={`brand brand-${layout} ${className}`.trim()}>
      {/* 图标渲染复用 BrandLogo（圆角/裁剪那套讲究写在它自己的注释里，只此一处） */}
      <BrandLogo size={size} src={meta.logo_url} alt={meta.name} />
      <div className="brand-text">
        <div className="brand-name" title={meta.name}>{meta.name}</div>
        {showVersion && (
          // commit 放 title：界面上只显示版本号，需要精确到构建时鼠标一悬停就有
          <div className="brand-version" title={meta.commit ? `commit ${meta.commit}${meta.built_at ? ' · built ' + meta.built_at : ''}` : ''}>
            {versionText || '版本未知'}
          </div>
        )}
      </div>
    </div>
  )
}
