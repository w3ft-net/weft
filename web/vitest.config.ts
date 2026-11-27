import { defineConfig } from 'vitest/config'

export default defineConfig({
  test: {
    include: ['src/test/**/*.test.{ts,tsx}'],
    pool: 'forks',
    // The published package is ESM in node_modules, so vitest would hand it to
    // Node, which cannot take named imports from @xterm's CommonJS build. Run it
    // through vite like the rest of the app.
    server: { deps: { inline: [/@kevlarcon\/shellcore-wasm/] } },
  },
})
