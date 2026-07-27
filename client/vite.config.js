import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'

export default defineConfig(() => {
  const gateway = (process.env.FARM_GATEWAY_URL || 'http://127.0.0.1:9002').replace(/\/+$/, '')
  const websocketGateway = gateway.replace(/^http/, 'ws')

  return {
    plugins: [vue()],
    server: {
      port: 9001,
      strictPort: true,
      proxy: {
        '/api': gateway,
        '/ws': { target: websocketGateway, ws: true },
      },
    },
  }
})
