import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import { execSync } from 'node:child_process'

// __WEFT_WEB_SHA__ is stamped into the build so the frontend can
// declare itself on every fetch as X-Weft-Client: weft-web/<sha>.
// Tries `git rev-parse --short HEAD`; falls back to "dev" when the
// repo isn't available (e.g. in a tarball install — never expected
// for us, but keeps the build deterministic). Resolved at config
// time, baked into the bundle.
function resolveBuildSHA(): string {
  try {
    return execSync('git rev-parse --short HEAD').toString().trim()
  } catch {
    return 'dev'
  }
}

// https://vite.dev/config/
export default defineConfig({
  plugins: [react()],
  define: {
    __WEFT_WEB_SHA__: JSON.stringify(resolveBuildSHA()),
  },
  server: {
    proxy: {
      '/api': {
        target: process.env.WEFT_API_URL ?? 'http://localhost:8080',
        changeOrigin: true,
      },
    },
  },
})
