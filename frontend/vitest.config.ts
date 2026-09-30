import { mergeConfig } from 'vite'
import { defineConfig } from 'vitest/config'
import base from './vite.config.ts'

export default mergeConfig(base, defineConfig({
  test: { environment: 'jsdom', setupFiles: ['./test/setup.ts'], include: ['test/**/*.test.tsx'] },
}))
