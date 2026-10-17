import { defineConfig } from 'vitest/config'
import { weftdVersion } from './weftdVersion'

export default defineConfig({
  define: {
    __WEFTD_VERSION__: JSON.stringify(weftdVersion()),
  },
  test: {
    include: ['src/test/**/*.test.{ts,tsx}'],
    pool: 'forks',
  },
})
