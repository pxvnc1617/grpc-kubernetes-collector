import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'

// 개발 중에는 Vite 개발 서버(5173)에서 Go 서버(8080)로 API 를 프록시한다.
// 빌드하면 dist/ 가 Go 서버에서 그대로 서빙되므로 프록시가 필요 없다.
export default defineConfig({
  plugins: [vue()],
  server: {
    port: 5173,
    proxy: {
      '/api': { target: 'http://localhost:8080', changeOrigin: true },
    },
  },
  build: { outDir: 'dist', emptyOutDir: true },
})
