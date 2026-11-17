import { defineConfig } from 'vitest/config'
import react from '@vitejs/plugin-react'

// The SPA is served by the Go binary in production. During development Vite proxies the
// API, provider webhooks, and health check to a locally running `bin/insteadly`.
export default defineConfig({
  plugins: [react()],
  server: {
    port: 5173,
    proxy: {
      '/api': 'http://127.0.0.1:8080',
      '/webhooks': 'http://127.0.0.1:8080',
      '/healthz': 'http://127.0.0.1:8080',
    },
  },
  build: {
    outDir: 'dist',
    emptyOutDir: true,
  },
  test: {
    environment: 'jsdom',
    setupFiles: ['./src/setupTests.ts'],
    css: false,
    restoreMocks: true,
  },
})
