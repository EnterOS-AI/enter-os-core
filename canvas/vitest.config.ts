import { defineConfig } from 'vitest/config'
import react from '@vitejs/plugin-react'
import path from 'path'

export default defineConfig({
  plugins: [react()],
  test: {
    environment: 'node',
    exclude: ['e2e/**', 'node_modules/**', '**/dist/**'],
    // CI-conditional test timeout (issue #96).
    //
    // Vitest's 5000ms default is too tight for the first test in any
    // file under our CI shape: `npx vitest run --coverage` on the
    // self-hosted Gitea Actions Docker runner. The cold-start cost
    // (v8 coverage instrumentation init + JSDOM bootstrap + module-
    // graph import for @/components/* and @/lib/* + first React
    // render) consistently consumes 5-7 seconds for the first
    // synchronous test in heavyweight component files
    // (ActivityTab.test.tsx, CreateWorkspaceDialog.test.tsx,
    // ConfigTab.provider.test.tsx) — even though every subsequent
    // test in the same file completes in 100-1500ms.
    //
    // Empirically the worst observed first-test was 6453ms in a
    // single file (CreateWorkspaceDialog). 30000ms gives ~5x
    // headroom over that on CI; we still keep 5000ms locally so
    // genuine waitFor races / hung promises stay sensitive in dev.
    //
    // Same vitest pattern documented at:
    //   https://vitest.dev/config/testtimeout
    //   https://vitest.dev/guide/coverage#profiling-test-performance
    //
    // Per-test duration is still emitted to the CI log; if a test
    // ever silently approaches 25-30s under this raised ceiling that
    // will surface as a duration regression and we revisit.
    testTimeout: process.env.CI ? 30000 : 5000,
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
