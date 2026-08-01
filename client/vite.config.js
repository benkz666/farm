import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'
import { resolve } from 'node:path'

export default defineConfig(() => {
  const gateway = (process.env.FARM_GATEWAY_URL || 'http://127.0.0.1:9002').replace(/\/+$/, '')
  const websocketGateway = gateway.replace(/^http/, 'ws')

  return {
    plugins: [vue()],
    build: {
      rollupOptions: {
        input: {
          app: resolve(import.meta.dirname, 'index.html'),
          modelWorkshop: resolve(import.meta.dirname, 'model-workshop.html'),
        },
      },
    },
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
