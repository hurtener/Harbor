// MCP Apps DisplayMode layout — real-route Playwright spec.
//
// This spec REPLACES the prior synthetic-DOM harness (which hand-built
// `document.createElement` fixtures and re-implemented the split-ratio clamp).
// It now drives the REAL built Playground route inside the `harbor console`
// subcommand — so the assertions exercise the SHIPPED Svelte components and
// the SHIPPED layout state machine, not a re-implementation. The deterministic
// inline-discovery → renderer-mount regression guard (which mounts the real
// `MessageBubble` / `McpAppRenderer` / `AppPanel` and fails if the wiring is
// reverted) lives in the always-on Vitest component suite
// `src/lib/chat/mcp-app-discovery.spec.ts`; this Playwright spec is the
// real-browser companion that proves the same surface ships in the bundle.
//
// SKIP semantics mirror every other page spec (`playground-page.spec.ts`):
// without the `harbor console` subcommand the `runtime` fixture is
// unavailable, so the describe block SKIPs at collection time.

import { test, expect, consoleSubcommandAvailable } from './fixtures/page';

const CONSOLE_AVAILABLE = consoleSubcommandAvailable();

// Seeds the Console's runtime-connection storage so the page resolves a live
// connection rather than the Disconnected `PageState` (mirrors
// playground-page.spec.ts).
async function seedConnection(
  page: import('@playwright/test').Page,
  baseURL: string,
  token: string,
): Promise<void> {
  await page.addInitScript(
    ([b, t]) => {
      window.localStorage.setItem('harbor.runtime.base_url', b);
      window.localStorage.setItem('harbor.runtime.token', t);
      window.localStorage.setItem('harbor.runtime.tenant', 'dev');
      window.localStorage.setItem('harbor.runtime.user', 'dev');
      window.localStorage.setItem('harbor.runtime.session', 'dev');
    },
    [baseURL, token] as const,
  );
}

test.describe('MCP Apps DisplayMode layout — real Playground route (D-062)', () => {
  test.skip(
    !CONSOLE_AVAILABLE,
    'harbor console subcommand absent or bin/harbor not built',
  );

  test('the real Playground page mounts the DisplayMode layout region machine', async ({
    page,
    runtime,
    helpers,
  }) => {
    await helpers.seedAuth(runtime.token);
    await seedConnection(page, runtime.baseURL, runtime.token);
    await helpers.gotoPage('playground');

    await expect(
      page.locator("[data-testid='console-hydrated']"),
      'the Console app hydrated',
    ).toBeAttached();
    await expect(
      page.locator("[data-testid='playground-page']"),
      'the Playground page route served',
    ).toBeAttached();

    // The SHIPPED region machine renders its `playground-layout` host with a
    // resolved region. In the default (no active app) state it is the `chat`
    // region — the same projection `computeRegion` returns. This proves the
    // real layout subsystem (109c) is wired into the bundle, not a stub.
    const layout = page.locator("[data-testid='playground-layout']");
    await expect(layout).toBeAttached();
    await expect(layout).toHaveAttribute('data-region', /chat|fullscreen|pip/);
  });
});
