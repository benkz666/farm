import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'
import { resolve } from 'node:path'

function preserveBrowserHost(proxy) {
  const setHost = (proxyReq, req) => {
    if (req.headers.host) proxyReq.setHeader('Host', req.headers.host)
  }
  proxy.on('proxyReq', setHost)
  proxy.on('proxyReqWs', setHost)
}

export default defineConfig(() => {
  const gateway = (process.env.FARM_GATEWAY_URL || 'http://127.0.0.1:9002').replace(/\/+$/, '')
  const websocketGateway = gateway.replace(/^http/, 'ws')
  const toGateway = { target: gateway, changeOrigin: false, configure: preserveBrowserHost }

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
        // 与 deploy/nginx.conf 一致：透传浏览器 Host，让 Gateway 返回同源 ws_url，
        // 并通过 CheckOrigin（Origin.host == request.Host）。默认字符串代理会把
        // Host 改成 127.0.0.1:9002，登录后 WebSocket 会被 403。
        '/api': toGateway,
        '/docs': toGateway,
        '/ws': { target: websocketGateway, ws: true, changeOrigin: false, configure: preserveBrowserHost },
      },
    },
  }
})
