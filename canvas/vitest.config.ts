import { defineConfig } from 'vitest/config'
import react from '@vitejs/plugin-react'
import path from 'path'

export default defineConfig({
  plugins: [react()],
  test: {
    environment: 'node',
    exclude: ['e2e/**', 'node_modules/**', '**/dist/**'],
    // Issue #22 / vitest pool investigation:
    //
    // The forks pool spawns one Node.js worker per concurrent slot.
    // Each jsdom-environment worker bootstraps a full DOM (~30-50 MB resident
    // set) at cold-start.  With the default maxWorkers derived from CPU
    // count, multiple jsdom workers can start simultaneously, exhausting
    // memory on the 2-CPU Gitea Actions runner and causing pool workers to
    // fail to respond with "[vitest-pool]: Timeout starting … runner."
    //
    // Fix: cap maxWorkers at 1 so only one worker is alive at any time.
    // Tests still run in parallel within that single worker's process (via
    // node's EventLoop) — this is the same parallelism as the `threads`
    // pool but without the per-worker jsdom cold-start overhead.  51 test
    // files that previously took 5070 s with 5 failures now run
    // sequentially through one worker, eliminating the memory spike.
    maxWorkers: 1,
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
