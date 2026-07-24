// The MCP Apps host↔app handshake regression gate — the REAL vendored App
// client against our REAL AppBridgeHost, in a real sandboxed iframe.
//
// This is the CI gate for the reverted-work break (D-342): the original
// live-theme wiring tore the transport down mid-`ui/initialize`, so the App
// timed out ("iframe received zero messages"). A hand-rolled postMessage stub
// (the sibling `mcp-app-host.spec.ts` security harness) could never catch that
// — it never drives a real `ui/initialize`. This spec does:
//
//   1. It bundles OUR `AppBridgeHost` (+ theme-tokens) with esbuild and runs it
//      in the top window; it bundles the REAL `@modelcontextprotocol/ext-apps`
//      `App` client and runs it inside a real sandboxed iframe.
//   2. It completes `ui/initialize` end-to-end — the App reports `initialized`
//      with the host theme + styles.variables it received (HA-35).
//   3. It delivers the captured tool input then result (D-226) and asserts the
//      App received them, in order.
//   4. It toggles the host theme MID-session (`setHostContext`) and asserts a
//      `host-context-changed` arrives at the App WITHOUT breaking the handshake
//      (the transport survives — the exact reverted break) — twice, to prove
//      repeated live patches keep working.
//
// The App runs at an opaque origin (sandbox `allow-scripts`, no
// `allow-same-origin`), so it reports its state back to the top window over a
// dedicated `__mcpE2E` postMessage channel (the parent cannot read an opaque
// iframe's DOM). CSP-wrapping is intentionally omitted here — the CSP /
// sandbox-escape invariants are the sibling `mcp-app-host.spec.ts`'s job; this
// spec's job is the live handshake + theme relay + delivery.

import fs from 'node:fs';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

import { build, type Plugin } from 'esbuild';
import { expect, test } from '@playwright/test';

import { APP_IFRAME_SANDBOX_BASE } from '../src/lib/chat/renderers/sandbox-policy';

const here = path.dirname(fileURLToPath(import.meta.url));
const renderersDir = path.resolve(here, '../src/lib/chat/renderers');

// esbuild resolves the chat module's `./x.js` imports to their `.ts` sources
// (the module authors internal imports with `.js` per NodeNext). Bare
// specifiers (node_modules) fall through to esbuild's own resolver.
const tsExtPlugin: Plugin = {
  name: 'ts-ext',
  setup(pluginBuild) {
    pluginBuild.onResolve({ filter: /\.js$/ }, (args) => {
      if (!args.path.startsWith('.')) return undefined;
      const abs = path.resolve(args.resolveDir, args.path);
      const tsPath = abs.replace(/\.js$/, '.ts');
      if (fs.existsSync(tsPath)) return { path: tsPath };
      return undefined;
    });
  },
};

async function bundle(contents: string, globalName: string): Promise<string> {
  const result = await build({
    stdin: { contents, resolveDir: renderersDir, loader: 'ts' },
    bundle: true,
    write: false,
    format: 'iife',
    globalName,
    platform: 'browser',
    plugins: [tsExtPlugin],
    logLevel: 'silent',
  });
  return result.outputFiles[0].text;
}

// Escape a bundle so it can be embedded inside an HTML <script> without its own
// content prematurely closing the tag.
function forScriptTag(js: string): string {
  return js.replace(/<\/script/gi, '<\\/script');
}

let hostBundle = '';
let viewBundle = '';

test.beforeAll(async () => {
  // OUR host code (the construct-once + gated-relay + delivery wrapper).
  hostBundle = await bundle(
    `export { AppBridgeHost } from './app-bridge-host.js';`,
    'HarborHost',
  );
  // The REAL vendored ext-apps App client + its theme helpers.
  viewBundle = await bundle(
    `export { App, applyDocumentTheme, applyHostStyleVariables } from '@modelcontextprotocol/ext-apps';`,
    'HarborView',
  );
});

// The App document run inside the sandboxed iframe: it constructs the REAL App,
// registers the tool-input/result + host-context handlers, waits for the parent
// `__mcpStart` signal (so the host is listening before ui/initialize is sent),
// connects, and reports every lifecycle event back to the top window.
function appDocument(view: string): string {
  const script = `
    ${forScriptTag(view)}
    (function () {
      var App = window.HarborView.App;
      var applyDocumentTheme = window.HarborView.applyDocumentTheme;
      var applyHostStyleVariables = window.HarborView.applyHostStyleVariables;
      function report(kind, data) { parent.postMessage({ __mcpE2E: true, kind: kind, data: data }, '*'); }
      var app = new App({ name: 'harbor-e2e-app', version: '1' });
      app.ontoolinput = function (p) { report('tool-input', (p && p.arguments) || {}); };
      app.ontoolresult = function (p) { report('tool-result', p); };
      app.onhostcontextchanged = function (ctx) {
        try { if (ctx && ctx.theme) applyDocumentTheme(ctx.theme); } catch (e) {}
        try { if (ctx && ctx.styles && ctx.styles.variables) applyHostStyleVariables(ctx.styles.variables); } catch (e) {}
        report('host-context-changed', { theme: ctx && ctx.theme, hasStyles: !!(ctx && ctx.styles && ctx.styles.variables) });
      };
      var started = false;
      function start() {
        if (started) return; started = true;
        app.connect().then(function () {
          var ctx = app.getHostContext();
          try { if (ctx && ctx.theme) applyDocumentTheme(ctx.theme); } catch (e) {}
          try { if (ctx && ctx.styles && ctx.styles.variables) applyHostStyleVariables(ctx.styles.variables); } catch (e) {}
          report('initialized', { theme: ctx && ctx.theme, hasStyles: !!(ctx && ctx.styles && ctx.styles.variables) });
        }, function (err) { report('error', String((err && err.message) || err)); });
      }
      window.addEventListener('message', function (e) { if (e.data && e.data.__mcpStart) start(); });
    })();
  `;
  return `<!doctype html><html><head><title>mcp-app</title></head><body><div id="root"></div><script>${script}</script></body></html>`;
}

test.describe('MCP Apps host↔app handshake (D-342 regression gate)', () => {
  test('ui/initialize completes, theme + styles delivered, live theme relay survives, tool data delivered', async ({
    page,
  }) => {
    await page.setContent(
      '<!doctype html><html><head><title>host</title></head><body></body></html>',
    );

    // Install the top-window listener + OUR host bundle.
    await page.evaluate(() => {
      (window as unknown as { __appLog: Array<{ kind: string; data: unknown }> }).__appLog = [];
      window.addEventListener('message', (e) => {
        const d = e.data as { __mcpE2E?: boolean; kind: string; data: unknown };
        if (d && d.__mcpE2E) {
          (window as unknown as { __appLog: Array<{ kind: string; data: unknown }> }).__appLog.push({
            kind: d.kind,
            data: d.data,
          });
        }
      });
    });
    await page.addScriptTag({ content: hostBundle });

    // Mount the sandboxed iframe running the REAL App, construct OUR host, and
    // connect it; then signal the App to start so the host is listening first.
    await page.evaluate(
      ({ srcdoc, sandbox }) => {
        const frame = document.createElement('iframe');
        frame.id = 'app-frame';
        frame.setAttribute('sandbox', sandbox);
        frame.setAttribute('allow', '');
        frame.setAttribute('referrerpolicy', 'no-referrer');
        frame.srcdoc = srcdoc;
        document.body.appendChild(frame);

        const w = window as unknown as {
          __host: unknown;
          __hostInit: boolean;
          HarborHost: { AppBridgeHost: new (opts: unknown) => { connect: (win: Window) => Promise<void>; setHostContext: (p: unknown) => void; isInitialized: boolean } };
        };
        w.__hostInit = false;
        const client = {
          readResource: async (_s: string, u: string) => ({ resourceUri: u, mimeType: 'text/html', content: '' }),
          callTool: async (t: string) => ({ tool: t, content: {}, isError: false }),
          listResources: async () => [],
          listTools: async () => [],
          resolveArtifact: async (id: string) => `blob:${id}`,
          toolContext: async () => ({
            tool: 'srv_report',
            input: { content: { region: 'emea' } },
            result: { content: { revenue: 1234 } },
            isError: false,
          }),
          fetchArtifactText: async () => '{}',
        };
        const host = new w.HarborHost.AppBridgeHost({
          client,
          serverID: 'srv',
          toolCallId: 'tc_e2e_1',
          theme: 'dark',
          styles: {
            variables: {
              '--color-background-primary': '#0d1117',
              '--color-text-primary': '#e6edf3',
            },
          },
          availableDisplayModes: ['inline', 'fullscreen', 'pip'],
          onInitialized: () => {
            w.__hostInit = true;
          },
        });
        w.__host = host;

        function go(): void {
          const cw = frame.contentWindow;
          if (!cw) {
            window.setTimeout(go, 10);
            return;
          }
          void host.connect(cw).then(() => {
            // The host is now listening — release the App's ui/initialize. The
            // App's `__mcpStart` listener may not be registered yet (its script
            // is still parsing), and postMessage is not queued, so re-send until
            // the App reports `initialized`. `start()` is idempotent.
            let ticks = 0;
            const pump = window.setInterval(() => {
              ticks += 1;
              cw.postMessage({ __mcpStart: true }, '*');
              const w2 = window as unknown as { __appLog: Array<{ kind: string }> };
              if (ticks > 60 || w2.__appLog.some((m) => m.kind === 'initialized')) {
                window.clearInterval(pump);
              }
            }, 50);
          });
        }
        go();
      },
      { srcdoc: appDocument(viewBundle), sandbox: APP_IFRAME_SANDBOX_BASE },
    );

    // 1. Handshake completes — no `ui/initialize` timeout — and the App reports
    //    the host theme + styles it received (HA-35).
    await page.waitForFunction(
      () => {
        const log = (window as unknown as { __appLog: Array<{ kind: string }> }).__appLog;
        const w = window as unknown as { __hostInit: boolean };
        return w.__hostInit === true && log.some((m) => m.kind === 'initialized');
      },
      undefined,
      { timeout: 20_000 },
    );

    const initEntry = await page.evaluate(
      () =>
        (window as unknown as { __appLog: Array<{ kind: string; data: { theme?: string; hasStyles?: boolean } }> }).__appLog.find(
          (m) => m.kind === 'initialized',
        )?.data,
    );
    expect(initEntry?.theme, 'App received the host dark theme').toBe('dark');
    expect(initEntry?.hasStyles, 'App received styles.variables').toBe(true);

    // 2. Data Delivery — the App received input THEN result (D-226).
    await page.waitForFunction(
      () => {
        const log = (window as unknown as { __appLog: Array<{ kind: string }> }).__appLog;
        return log.some((m) => m.kind === 'tool-input') && log.some((m) => m.kind === 'tool-result');
      },
      undefined,
      { timeout: 20_000 },
    );
    const delivery = await page.evaluate(() => {
      const log = (window as unknown as { __appLog: Array<{ kind: string; data: unknown }> }).__appLog;
      const inputIdx = log.findIndex((m) => m.kind === 'tool-input');
      const resultIdx = log.findIndex((m) => m.kind === 'tool-result');
      return { input: log[inputIdx]?.data, result: log[resultIdx]?.data, inputIdx, resultIdx };
    });
    expect(delivery.input).toEqual({ region: 'emea' });
    expect((delivery.result as { structuredContent?: unknown }).structuredContent).toEqual({ revenue: 1234 });
    // Input strictly precedes result (the SDK lifecycle order).
    expect(delivery.inputIdx).toBeLessThan(delivery.resultIdx);

    // 3. Live theme relay — patch the LIVE bridge to light; the App receives a
    //    host-context-changed WITHOUT the handshake breaking.
    await page.evaluate(() => {
      const w = window as unknown as { __host: { setHostContext: (p: unknown) => void } };
      w.__host.setHostContext({ theme: 'light', styles: { variables: { '--color-background-primary': '#ffffff' } } });
    });
    await page.waitForFunction(
      () =>
        (window as unknown as { __appLog: Array<{ kind: string; data: { theme?: string } }> }).__appLog.some(
          (m) => m.kind === 'host-context-changed' && m.data.theme === 'light',
        ),
      undefined,
      { timeout: 20_000 },
    );

    // 4. And again back to dark — repeated live patches keep arriving, proving
    //    the transport was never town down (the reverted break would have
    //    silently killed the channel after the first patch).
    await page.evaluate(() => {
      const w = window as unknown as { __host: { setHostContext: (p: unknown) => void } };
      w.__host.setHostContext({ theme: 'dark', styles: { variables: { '--color-background-primary': '#0d1117' } } });
    });
    await page.waitForFunction(
      () =>
        (window as unknown as { __appLog: Array<{ kind: string; data: { theme?: string } }> }).__appLog.filter(
          (m) => m.kind === 'host-context-changed',
        ).length >= 2,
      undefined,
      { timeout: 20_000 },
    );

    // No error was reported by the App, and the host stayed initialized.
    const summary = await page.evaluate(() => {
      const log = (window as unknown as { __appLog: Array<{ kind: string; data: unknown }> }).__appLog;
      const w = window as unknown as { __host: { isInitialized: boolean } };
      return {
        errors: log.filter((m) => m.kind === 'error').map((m) => m.data),
        ctxChanges: log.filter((m) => m.kind === 'host-context-changed').map((m) => (m.data as { theme?: string }).theme),
        hostInitialized: w.__host.isInitialized,
      };
    });
    expect(summary.errors, 'no App-side handshake/render errors').toEqual([]);
    expect(summary.hostInitialized, 'host bridge stayed initialized across theme toggles').toBe(true);
    expect(summary.ctxChanges).toEqual(['light', 'dark']);
  });
});
