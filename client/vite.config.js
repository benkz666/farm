import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'

export default defineConfig({
  plugins: [vue()],
  server: {
    port: 9001,
    strictPort: true,
    proxy: {
      '/api': 'http://127.0.0.1:9002',
      '/ws': { target: 'ws://127.0.0.1:9002', ws: true },
    },
  },
})
