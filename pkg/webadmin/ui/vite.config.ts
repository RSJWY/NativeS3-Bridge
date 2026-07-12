import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'
import { writeFileSync } from 'node:fs'

const appVersion = process.env.APP_VERSION?.trim() || 'dev'

export default defineConfig({
  define: {
    __APP_VERSION__: JSON.stringify(appVersion)
  },
  plugins: [
    vue(),
    {
      name: 'keep-dist-directory',
      closeBundle() {
        writeFileSync('dist/.gitkeep', '')
      }
    }
  ],
  server: {
    proxy: {
      '/api': 'http://localhost:9001'
    }
  },
  build: {
    outDir: 'dist',
    emptyOutDir: true
  }
})
