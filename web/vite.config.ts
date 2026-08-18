import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

export default defineConfig({
  plugins: [react()],
  server: {
    port: 5173,
    proxy: {
      '/api': { target: 'http://localhost:8080', ws: true },
      '/healthz': 'http://localhost:8080',
      '/readyz': 'http://localhost:8080',
    },
  },
  build: { outDir: 'dist', sourcemap: true },
})

