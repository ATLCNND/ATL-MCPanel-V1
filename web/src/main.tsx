import React from 'react'
import ReactDOM from 'react-dom/client'
import App from './App'
// token 必须先于任何组件样式加载，否则 var() 解析不到值
import './styles/tokens.css'
import './styles/components.css'
import './index.css'
import { initTheme } from './styles/theme'

// 在渲染前应用主题，避免首屏闪烁
initTheme()

ReactDOM.createRoot(document.getElementById('root')!).render(
  <React.StrictMode>
    <App />
  </React.StrictMode>,
)