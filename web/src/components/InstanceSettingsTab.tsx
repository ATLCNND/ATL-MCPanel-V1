import { useState } from 'react'
import InstancePorts from './InstancePorts'
import JarsTab from './JarsTab'
import StartScriptTab from './StartScriptTab'
import './InstanceSettingsTab.css'

type SettingsSection = 'ports' | 'jars' | 'start'

/**
 * 实例设置把三个低频但相关的页面收进一个入口，减少实例导航的横向选择成本。
 * 子页面只在选中时挂载，避免端口、核心和启动设置同时请求数据并占用纵向空间。
 */
export default function InstanceSettingsTab({
  instanceId, canManageExpiry, canWriteFiles, running, isAdmin,
}: {
  instanceId: string
  canManageExpiry: boolean
  canWriteFiles: boolean
  running: boolean
  isAdmin: boolean
}) {
  const [section, setSection] = useState<SettingsSection>('ports')
  const sections: { key: SettingsSection; label: string; icon: string }[] = [
    { key: 'ports', label: '公网端口', icon: '⇄' },
    { key: 'jars', label: '核心', icon: '◈' },
    { key: 'start', label: '启动', icon: '⌘' },
  ]

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
            onClick={() => setSection(item.key)}
          >
            <span className="settings-tab-icon">{item.icon}</span>
            {item.label}
          </button>
        ))}
      </nav>
      <div className="settings-panel" role="tabpanel">
        {section === 'ports' && <InstancePorts instanceId={instanceId} canEdit={canManageExpiry} />}
        {section === 'jars' && <JarsTab instanceId={instanceId} canWrite={canWriteFiles} running={running} />}
        {section === 'start' && <StartScriptTab instanceId={instanceId} canWrite={canWriteFiles} running={running} isAdmin={isAdmin} />}
      </div>
    </div>
  )
}
