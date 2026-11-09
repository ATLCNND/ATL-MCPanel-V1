import { useMemo, useState } from 'react'
import InstancePorts from './InstancePorts'
import JarsTab from './JarsTab'
import StartScriptTab from './StartScriptTab'
import { InstanceCapability, InstanceTemplate } from '../api'
import './InstanceSettingsTab.css'

type SettingsSection = 'ports' | 'spec' | 'jars' | 'start'

/**
 * 实例设置把几个低频但相关的页面收进一个入口，减少实例导航的横向选择成本。
 * 子页面只在选中时挂载，避免端口、核心和启动设置同时请求数据并占用纵向空间。
 *
 * 子分页按**能力**出现，不按类型写 if：
 *  - 公网端口：所有类型都有（隧道与端口是面板的事，与被跑的是什么无关）；
 *  - 容器规格：仅 docker（镜像/端口映射/卷/环境变量，只读展示）；
 *  - 核心：仅 minecraft（只有它有 jar 这件东西）；
 *  - 启动：minecraft（启动命令）与控制台（start.sh 就是它的启动方式）都有。
 */
export default function InstanceSettingsTab({
  instanceId, canManageExpiry, canWriteFiles, running, isAdmin, template, caps, spec,
}: {
  instanceId: string
  canManageExpiry: boolean
  canWriteFiles: boolean
  running: boolean
  isAdmin: boolean
  template: InstanceTemplate
  caps: Set<InstanceCapability>
  /** 非 minecraft 类型的规格 JSON（只读展示） */
  spec?: string
}) {
  const sections = useMemo(() => {
    const list: { key: SettingsSection; label: string; icon: string }[] = [
      { key: 'ports', label: '公网端口', icon: '⇄' },
    ]
    if (caps.has('image') || template === 'docker') {
      list.push({ key: 'spec', label: '容器规格', icon: '◲' })
    }
    if (caps.has('core')) {
      list.push({ key: 'jars', label: '核心', icon: '◈' })
    }
    if (caps.has('java') || caps.has('script')) {
      list.push({ key: 'start', label: '启动', icon: '⌘' })
    }
    return list
  }, [caps, template])

  const [wanted, setWanted] = useState<SettingsSection>('ports')
  // 切换实例类型后可能停在一个已经不存在的分页上（比如从 MC 换到 Docker，
  // "核心"页没了）—— 退回第一个，别留一片空白。
  const section = sections.some((s) => s.key === wanted) ? wanted : sections[0].key

  return (
    <div className="instance-settings">
      {/* 连通式子导航：`.connected-tabs` 是玩家页 / 实例设置页 / 游戏设置页**共用**的
          那一套（深品红描边 + 分页只占内容宽度）。以前这里同时挂 kind-tabs 与
          settings-tabs 两个类，两份定义迟早会漂移，现在统一成一个类。 */}
      <nav className="connected-tabs" role="tablist" aria-label="实例设置分类">
        {sections.map((item) => (
          <button
            key={item.key}
            type="button"
            role="tab"
            aria-selected={section === item.key}
            className={section === item.key ? 'active' : ''}
            onClick={() => setWanted(item.key)}
          >
            <span className="settings-tab-icon">{item.icon}</span>
            {item.label}
          </button>
        ))}
      </nav>
      <div className="settings-panel" role="tabpanel">
        {section === 'ports' && <InstancePorts instanceId={instanceId} canEdit={canManageExpiry} />}
        {section === 'spec' && <ContainerSpecPanel spec={spec} />}
        {section === 'jars' && <JarsTab instanceId={instanceId} canWrite={canWriteFiles} running={running} />}
        {section === 'start' && <StartScriptTab instanceId={instanceId} canWrite={canWriteFiles} running={running} isAdmin={isAdmin} />}
      </div>
    </div>
  )
}

/**
 * 容器规格（只读）。
 *
 * 为什么不做成可编辑：容器是**按启动时的规格**拉起来的，改了元数据而没重建
 * 容器，面板说的和实际跑的就不是一回事了（更糟的是重启后行为变了、而用户
 * 以为自己只是改了个数字）。要换镜像/端口/卷，重建实例是对的路径。
 */
function ContainerSpecPanel({ spec }: { spec?: string }) {
  const pretty = useMemo(() => {
    if (!spec) return ''
    try {
      return JSON.stringify(JSON.parse(spec), null, 2)
    } catch {
      return spec
    }
  }, [spec])

  return (
    <div className="ctr-spec">
      <p className="ctr-spec-note">
        以下是创建这台实例时记录的容器规格。<b>只读</b> —— 镜像、端口映射、卷挂载决定的是
        “容器按什么被拉起来”，改动它们等于换一台实例，请重建实例而不是改这里。
      </p>
      {pretty
        ? <pre className="ctr-spec-json">{pretty}</pre>
        : <div className="muted">这台实例没有记录容器规格。</div>}
    </div>
  )
}
