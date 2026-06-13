// Phase 109b — the MCP Apps host sandbox / CSP / origin-validation Playwright
// spec.
//
// This spec is SELF-CONTAINED — it needs neither the `harbor console`
// subcommand nor an LLM, because it tests the security PRIMITIVE directly in a
// real Chromium: a sandboxed iframe hosting a fixture MCP App. It imports the
// EXACT sandbox / CSP / wrapper constants the renderer ships
// (`$lib/chat/renderers/sandbox-policy`), so there is no drift between what is
// asserted here and what reaches the DOM in production.
//
// It proves the four D-173 security invariants jsdom cannot (jsdom does not
// enforce iframe sandboxing or CSP):
//   (a) the iframe sandbox blocks parent-DOM / cookie / localStorage access;
//   (b) the strict CSP is present and `connect-src 'none'` blocks the app
//       opening its own transport (a real `fetch` is blocked);
//   (c) a proxied tool-call message reaches the host ONLY from the trusted
//       iframe source (the host-policy seam);
//   (d) a foreign-origin / foreign-window message is rejected, not executed.

import { expect, test } from '@playwright/test';

import {
  appIframeSandbox,
  buildAppCSP,
  wrapAppDocument,
} from '../src/lib/chat/renderers/sandbox-policy';

// The fixture MCP App. On load it (1) probes every isolation boundary the
// sandbox must block, (2) attempts a network fetch the CSP must block, then
// reports the outcome to the host, and (3) posts a `tools/call` the host
// proxies. All `parent.postMessage` calls use `'*'` because a sandboxed
// iframe's parent is cross-origin from its (opaque) perspective.
const FIXTURE_APP = `<!doctype html><html><head><title>fixture</title></head>
<body>
<script>
  (async function () {
    const out = {};
    try { out.parentDom = window.parent.document ? 'reachable' : 'null'; }
    catch (e) { out.parentDom = 'blocked'; }
    try { out.cookie = 'read:' + document.cookie; }
    catch (e) { out.cookie = 'blocked'; }
    try { localStorage.setItem('x', '1'); out.localStorage = 'ok'; }
    catch (e) { out.localStorage = 'blocked'; }
    out.cspMeta = !!document.querySelector('meta[http-equiv="Content-Security-Policy"]');
    try { await fetch('https://example.com/csp-probe'); out.fetch = 'allowed'; }
    catch (e) { out.fetch = 'blocked'; }
    parent.postMessage({ kind: 'probe', out: out }, '*');
    parent.postMessage({ kind: 'jsonrpc', method: 'tools/call', name: 'srv_echo' }, '*');
  })();
</script>
</body></html>`;

/**
 * Mounts the sandboxed iframe (production sandbox attribute + CSP-wrapped
 * srcdoc) into a blank host page and installs the host's message guard:
 * messages are recorded partitioned by whether they came from the trusted
 * iframe window (the production `isTrustedAppMessage` rule: `event.source ===
 * iframe.contentWindow`). Built via `page.evaluate` — passing `srcdoc` as a JS
 * argument — so the fixture app's own `</script>` cannot terminate the host
 * script. The recorded log is read back through `window.__hostLog`.
 */
async function installHarness(
  page: import('@playwright/test').Page,
  sandbox: string,
  srcdoc: string,
): Promise<void> {
  await page.setContent('<!doctype html><html><head><title>host</title></head><body></body></html>');
  await page.evaluate(
    ({ sandbox, srcdoc }) => {
      (window as unknown as { __hostLog: { accepted: unknown[]; rejected: unknown[] } }).__hostLog = {
        accepted: [],
        rejected: [],
      };
      const frame = document.createElement('iframe');
      frame.id = 'app-frame';
      frame.setAttribute('sandbox', sandbox);
      frame.setAttribute('allow', '');
      frame.setAttribute('referrerpolicy', 'no-referrer');
      frame.srcdoc = srcdoc;
      window.addEventListener('message', (e) => {
        const log = (window as unknown as { __hostLog: { accepted: unknown[]; rejected: { origin: string }[] } })
          .__hostLog;
        // The production host guard: accept ONLY from the expected iframe window.
        if (e.source === frame.contentWindow) {
          log.accepted.push(e.data);
        } else {
          log.rejected.push({ origin: e.origin });
        }
      });
      document.body.appendChild(frame);
    },
    { sandbox, srcdoc },
  );
}

test.describe('MCP Apps host — sandbox / CSP / origin validation (D-173)', () => {
  test('sandbox blocks parent-DOM / cookie / localStorage and CSP blocks the network', async ({
    page,
  }) => {
    const sandbox = appIframeSandbox(false);
    expect(sandbox).not.toContain('allow-same-origin');
    const srcdoc = wrapAppDocument(FIXTURE_APP, buildAppCSP(false));

    await installHarness(page, sandbox, srcdoc);

    // Wait for the fixture app's probe to arrive at the host.
    await page.waitForFunction(
      () =>
        (window as unknown as { __hostLog: { accepted: Array<{ kind?: string }> } }).__hostLog.accepted.some(
          (m) => m.kind === 'probe',
        ),
      undefined,
      { timeout: 30_000 },
    );

    const probe = await page.evaluate(() => {
      const log = (window as unknown as { __hostLog: { accepted: Array<{ kind?: string; out?: Record<string, string> }> } })
        .__hostLog;
      return log.accepted.find((m) => m.kind === 'probe')?.out ?? {};
    });

    expect(probe.parentDom, 'parent DOM is blocked').toBe('blocked');
    expect(probe.cookie, 'cookie access is blocked').toBe('blocked');
    expect(probe.localStorage, 'localStorage is blocked').toBe('blocked');
    expect(probe.cspMeta, 'CSP meta is present in the app document').toBe(true);
    expect(probe.fetch, 'CSP connect-src none blocks the app fetch').toBe('blocked');
  });

  test('a proxied tool call reaches the host only from the trusted iframe source', async ({
    page,
  }) => {
    const srcdoc = wrapAppDocument(FIXTURE_APP, buildAppCSP(false));
    await installHarness(page, appIframeSandbox(false), srcdoc);

    await page.waitForFunction(
      () =>
        (window as unknown as { __hostLog: { accepted: Array<{ method?: string }> } }).__hostLog.accepted.some(
          (m) => m.method === 'tools/call',
        ),
      undefined,
      { timeout: 30_000 },
    );

    const toolCalls = await page.evaluate(
      () =>
        (window as unknown as { __hostLog: { accepted: Array<{ method?: string; name?: string }> } }).__hostLog.accepted.filter(
          (m) => m.method === 'tools/call',
        ),
    );
    expect(toolCalls.length).toBeGreaterThan(0);
    expect(toolCalls[0].name).toBe('srv_echo');
  });

  test('a foreign-window message is rejected, not executed', async ({ page }) => {
    const srcdoc = wrapAppDocument(FIXTURE_APP, buildAppCSP(false));
    await installHarness(page, appIframeSandbox(false), srcdoc);

    // Wait for the legitimate app messages first so the harness is live.
    await page.waitForFunction(
      () => (window as unknown as { __hostLog: { accepted: unknown[] } }).__hostLog.accepted.length > 0,
      undefined,
      { timeout: 30_000 },
    );

    // Post a forged tool call from the TOP window (source !== iframe).
    await page.evaluate(() => {
      window.postMessage({ kind: 'jsonrpc', method: 'tools/call', name: 'evil', forged: true }, '*');
    });

    await page.waitForFunction(
      () => (window as unknown as { __hostLog: { rejected: unknown[] } }).__hostLog.rejected.length > 0,
      undefined,
      { timeout: 30_000 },
    );

    const log = await page.evaluate(
      () =>
        (window as unknown as {
          __hostLog: { accepted: Array<{ forged?: boolean }>; rejected: unknown[] };
        }).__hostLog,
    );
    // The forged message landed in `rejected`, never in `accepted`.
    expect(log.rejected.length).toBeGreaterThan(0);
    expect(log.accepted.some((m) => m.forged === true)).toBe(false);
  });
});
