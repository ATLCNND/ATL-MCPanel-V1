import Brand from './Brand'
import TlsSettings from './TlsSettings'
import { usePanelMeta } from './usePanelMeta'
import { formatServerTime } from '../time'
import './AboutPage.css'

/** 关于页直接复用全局元数据缓存，确保品牌、版本和构建信息来自同一个接口。 */
export default function AboutPage() {
  const meta = usePanelMeta()
  const rows: [string, string][] = [
    ['面板名称', meta.name],
    ['版本', meta.version ? `v${meta.version}` : '—'],
    ['Commit', meta.commit || '—'],
    ['构建时间', formatServerTime(meta.built_at)],
    ['Go 版本', meta.go_version || '—'],
    // 启用安全入口时把访问路径显示出来：这段随机串否则只能从安装输出里找
    ...((meta as any).security_entry ? [["访问入口", '/' + (meta as any).security_entry]] as [string, string][] : []),
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

      <TlsSettings />

      <section className="about-card">
        <div className="about-card-title">项目链接</div>
        <div className="about-links">
          {/* 指向**公开**仓库：私人仓库外人打不开，指过去只会看到 404。
              文档也指公开仓那一套（docs/ 是面向公众重写过的版本）。 */}
          <a href="https://github.com/ATLCNND/ATL-MCPanel-V1" target="_blank" rel="noreferrer noopener">GitHub 仓库 ↗</a>
          <a href="https://github.com/ATLCNND/ATL-MCPanel-V1/tree/main/docs" target="_blank" rel="noreferrer noopener">项目文档 ↗</a>
        </div>
      </section>
    </div>
  )
}
