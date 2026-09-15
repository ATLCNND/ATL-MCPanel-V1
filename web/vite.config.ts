import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

// 后端地址从环境变量取，**不要写死内网地址**：
//   开发者各自的 Panel 地址不同，写死会让人 clone 下来就得改代码（还容易把内网拓扑带进公开仓库）
//   用法：VITE_API_TARGET=http://10.0.0.10:8080 npm run dev
const API_TARGET = process.env.VITE_API_TARGET || 'http://127.0.0.1:8080'
const WS_TARGET = process.env.VITE_WS_TARGET || API_TARGET.replace(/^http/, 'ws')

export default defineConfig({
  plugins: [react()],
  server: {
    port: 5173,
    // 开发时代理后端 API 和 WS 到 Panel
    proxy: {
      '/api': {
        target: API_TARGET,
        changeOrigin: true,
      },
      '/ws': {
        target: WS_TARGET,
        ws: true,
        changeOrigin: true,
      },
    },
  },
})
