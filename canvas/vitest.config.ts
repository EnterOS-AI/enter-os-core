import { defineConfig } from 'vitest/config'
import react from '@vitejs/plugin-react'
import path from 'path'

export default defineConfig({
  plugins: [react()],
  test: {
    environment: 'node',
    exclude: ['e2e/**', 'node_modules/**', '**/dist/**'],
    // Coverage is instrumented but NOT yet a CI gate — first land
    // observability so we can see the baseline, then dial in
    // thresholds + a hard gate in a follow-up PR (#1815). Today's
    // baseline is unknown; turning on a 70% threshold blind would
    // either fail CI immediately or paper over a real gap with an
    // ad-hoc exclude list.
    //
    // Run locally with: `npm run test:coverage`
    // Reports: text (terminal), html (./coverage/index.html),
    // json-summary (machine-readable for tooling).
    coverage: {
      provider: 'v8',
      reporter: ['text', 'html', 'json-summary'],
      include: ['src/**/*.{ts,tsx}'],
      exclude: [
        'src/**/*.test.{ts,tsx}',
        'src/**/__tests__/**',
        'src/**/*.d.ts',
        'src/types/**',
      ],
    },
  },
  resolve: {
    alias: {
      '@': path.resolve(__dirname, 'src'),
    },
  },
})
