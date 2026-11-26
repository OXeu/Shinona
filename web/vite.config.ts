import { fileURLToPath } from 'node:url'
import vue from '@vitejs/plugin-vue'
import { defineConfig, loadEnv } from 'vite'
import { arrowFrame } from './arrow-frame-plugin.ts'

const root = fileURLToPath(new URL('..', import.meta.url))

export default defineConfig(({ mode }) => {
  const env = loadEnv(mode, root, ['SHINONA_ADDR', 'SHINONA_API_ORIGIN'])
  const api = {
    target: env.SHINONA_API_ORIGIN || `http://${env.SHINONA_ADDR || '127.0.0.1:8780'}`,
    changeOrigin: false,
  }
  return {
    plugins: [vue(), arrowFrame()],
    server: {
      host: '127.0.0.1',
      strictPort: true,
      proxy: {
        '/auth': api,
        '/api/v1': { ...api, ws: true },
        '/api/terminal/v1': { ...api, ws: true },
      },
    },
  }
})
