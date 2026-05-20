// Harbor Console — Playwright harness baseline (Phase 75 / D-115).
//
// This config is the single source for the Console e2e browser matrix, base
// URL, timeouts, retries, and reporter shape. It deliberately targets the
// `harbor console` subcommand (D-091) — and NOT the local dev-loop
// subcommand. The Harbor Runtime ships headless; the Console static build is
// served exclusively by `harbor console` (CLAUDE.md §4.5 #2). A harness that
// booted the dev-loop subcommand would be testing a surface the Console never
// runs against in production.
//
// The harness boots a per-run Runtime + a `harbor console` instance on an
// ephemeral port (the D-104 preflight pattern). Port allocation and process
// lifecycle live in `tests/fixtures/harbor-runtime.ts`, not here, so the
// `webServer` block is intentionally absent: a single static `webServer`
// entry cannot express the per-run Runtime fixture the harness needs. Each
// spec acquires its Console URL through the typed `runtime` fixture instead.
//
// Adding Firefox / WebKit is a one-line change to the `projects` array; V1
// ships Chromium-only (Phase 75 non-goal: browser matrix beyond Chromium).

import { defineConfig, devices } from "@playwright/test";

export default defineConfig({
  // All specs live under tests/. Per-page specs land alongside their Stage-2
  // page phase (`<slug>-page.spec.ts`); the wave-end aggregator is Phase 75a.
  testDir: "./tests",

  // Deterministic ordering for V1. Parallelism is revisited post-V1 once
  // per-worker fixture flakes are characterised (Phase 75 plan, Test plan).
  workers: 1,
  fullyParallel: false,

  // Fail the CI build if a spec is accidentally left `.only`.
  forbidOnly: !!process.env.CI,

  // No retries — a flaky e2e spec is a bug to fix, not to paper over.
  retries: 0,

  timeout: 30_000,
  expect: { timeout: 5_000 },

  // List reporter keeps CI logs readable; the HTML report is opt-in locally.
  reporter: process.env.CI ? "list" : [["list"], ["html", { open: "never" }]],

  use: {
    // baseURL is supplied per-spec by the `runtime` fixture (the ephemeral
    // `harbor console` URL). It is intentionally NOT pinned here.
    trace: "off",
    screenshot: "only-on-failure",
  },

  projects: [
    {
      name: "chromium",
      use: { ...devices["Desktop Chrome"] },
    },
  ],
});
