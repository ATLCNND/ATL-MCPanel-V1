import Brand from './Brand'
import { usePanelMeta } from './usePanelMeta'
import { formatServerTime } from '../time'
import './AboutPage.css'

/** 关于页直接复用全局元数据缓存，确保品牌、版本和构建信息来自同一个接口。 */
export default function AboutPage() {
  const meta = usePanelMeta()
  const rows = [
    ['面板名称', meta.name],
    ['版本', meta.version ? `v${meta.version}` : '—'],
    ['Commit', meta.commit || '—'],
    ['构建时间', formatServerTime(meta.built_at)],
    ['Go 版本', meta.go_version || '—'],
  ]

  return (
    <div className="about-page">
      <section className="about-card about-brand-card">
        <Brand size={48} layout="stack" />
        <p className="about-lead">Minecraft 多用户实例管理面板</p>
      </section>

      <section className="about-card">
        <div className="about-card-title">运行信息</div>
        <dl className="about-meta">
          {rows.map(([label, value]) => (
            <div className="about-meta-row" key={label}>
              <dt>{label}</dt>
              <dd className={label === 'Commit' ? 'mono' : ''}>{value}</dd>
            </div>
          ))}
        </dl>
      </section>

      <section className="about-card">
        <div className="about-card-title">项目链接</div>
        <div className="about-links">
          <a href="https://github.com/ATLCNND/ATL-MCPanel" target="_blank" rel="noreferrer noopener">GitHub 仓库 ↗</a>
          <a href="https://github.com/ATLCNND/ATL-MCPanel/blob/main/README.md" target="_blank" rel="noreferrer noopener">项目文档 ↗</a>
        </div>
      </section>
    </div>
  )
}
