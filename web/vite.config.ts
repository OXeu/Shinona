import { fileURLToPath } from 'node:url'
import vue from '@vitejs/plugin-vue'
import { defineConfig, loadEnv } from 'vite'
import type { Plugin } from 'vite'
import { arrowFrame } from './arrow-frame-plugin.ts'

const root = fileURLToPath(new URL('..', import.meta.url))

function dingStyles(): Plugin {
  return {
    name: 'shinona-ding-styles',
    // Ding embeds its stylesheet in its ES module. Let this transform run in development too.
    config: () => ({ optimizeDeps: { exclude: ['@yusufceng1z/ding-vue', '@yusufceng1z/ding'] } }),
    transform(code, id) {
      const [path] = id.split('?', 1)
      if (!path.endsWith('/node_modules/@yusufceng1z/ding/dist/index.js')) return
      return code.replace(/(?<=[;{])\s*outline-offset\s*:[^;}]+;?/giu, '')
    },
  }
}

export default defineConfig(({ mode }) => {
  const env = loadEnv(mode, root, ['SHINONA_ADDR', 'SHINONA_API_ORIGIN'])
  const api = {
    target: env.SHINONA_API_ORIGIN || `http://${env.SHINONA_ADDR || '127.0.0.1:8780'}`,
    changeOrigin: false,
  }
  return {
    plugins: [vue(), dingStyles(), arrowFrame()],
    server: {
      host: '127.0.0.1',
      strictPort: true,
      proxy: {
        '/auth': api,
        '/graphql': api,
        '/api/session-transfer/v1/recent': { ...api, ws: true },
        '/api/session-transfer/v1/live': { ...api, ws: true },
        '/api/session-transfer/v1/transcript': api,
        '/api/terminal/v1': { ...api, ws: true },
      },
    },
  }
})
