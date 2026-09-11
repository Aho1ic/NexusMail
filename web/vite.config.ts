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
  // No sourcemap in the production bundle: the SPA is served unauthenticated, so an
  // emitted .js.map publishes the complete original TypeScript, sourcesContent and
  // all, to anyone who asks for it.
  build: { outDir: 'dist', sourcemap: false },
})

