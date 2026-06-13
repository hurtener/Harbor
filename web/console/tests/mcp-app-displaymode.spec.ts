// Phase 109c — the MCP Apps DisplayMode layout Playwright spec.
//
// SELF-CONTAINED (no `harbor console` subcommand, no LLM) — it mirrors the
// 109b `mcp-app-host.spec.ts` precedent: it imports the EXACT shipped layout
// state machine (`$lib/components/playground/layout`) and drives a faithful
// real-Chromium DOM harness from it, so there is zero drift between what is
// asserted here and the pure logic the Playground page renders. The harness
// renders the computed `RegionLayout` into DOM that mirrors the component
// `data-testid` contract (`app-tabstrip` / `app-tab` / `pip-split` /
// `pip-divider` / `pip-rail-toggle` / `app-panel-body`).
//
// It proves what jsdom + unit tests cannot: the per-DisplayMode page region in
// a real browser, a REAL pointer-drag on the split divider that clamps to the
// shipped bounds, the rail toggle preserving the ratio, the runtime transition
// `inline → pip → fullscreen`, and teardown returning to chat + rail.
//
// The pure state-machine branches (reducer + projection) are pinned
// exhaustively in the DOM-free unit suite
// (`src/lib/components/playground/layout.spec.ts`).

import { expect, test } from '@playwright/test';

import {
  CHAT_TAB_ID,
  INITIAL_LAYOUT,
  RATIO_MAX,
  RATIO_MIN,
  appId,
  clampRatio,
  computeRegion,
  reduceLayout,
  type AppRef,
  type LayoutModel,
  type RegionLayout
} from '../src/lib/components/playground/layout';

function appRef(id: string): AppRef {
  return {
    id,
    title: id.toUpperCase(),
    serverID: id,
    resourceUri: `ui://${id}`,
    rawHtmlTrusted: false
  };
}

// The harness renders a serialized RegionLayout into DOM mirroring the real
// component testids, and wires a REAL draggable divider that clamps with the
// shipped bounds (passed in — no drift). Built via setContent + evaluate so the
// browser holds only inert harness code; Node owns the shipped state machine.
async function mount(
  page: import('@playwright/test').Page,
  region: RegionLayout,
  bounds: { min: number; max: number }
): Promise<void> {
  await page.setContent('<!doctype html><html><head><title>layout</title></head><body></body></html>');
  await page.evaluate(
    ({ region, bounds }) => {
      const root = document.body;
      root.innerHTML = '';
      const layout = document.createElement('div');
      layout.setAttribute('data-testid', 'playground-layout');
      layout.setAttribute('data-region', region.region);
      layout.setAttribute('data-rail', String(region.railVisible));

      const chat = (testid: string) => {
        const el = document.createElement('div');
        el.setAttribute('data-testid', testid);
        el.textContent = 'chat';
        return el;
      };
      const railEl = () => {
        const el = document.createElement('aside');
        el.setAttribute('data-testid', 'detail-rail');
        el.textContent = 'rail';
        return el;
      };
      const panelEl = (id: string) => {
        const el = document.createElement('div');
        el.setAttribute('data-testid', 'app-panel-body');
        el.setAttribute('data-app-id', id);
        el.textContent = 'app';
        return el;
      };

      if (region.region === 'fullscreen') {
        const main = document.createElement('div');
        main.setAttribute('data-testid', 'fullscreen-region');
        const strip = document.createElement('div');
        strip.setAttribute('data-testid', 'app-tabstrip');
        for (const tab of region.tabs) {
          const btn = document.createElement('button');
          btn.setAttribute('data-testid', 'app-tab');
          btn.setAttribute('data-tab-id', tab.id);
          btn.setAttribute('aria-selected', String(tab.id === region.activeTabId));
          btn.textContent = tab.label;
          strip.appendChild(btn);
        }
        main.appendChild(strip);
        const body = document.createElement('div');
        body.setAttribute('data-testid', 'fullscreen-body');
        if (region.fullscreenApp) {
          body.appendChild(panelEl(region.fullscreenApp.id));
        } else {
          body.appendChild(chat('chat-panel'));
        }
        main.appendChild(body);
        layout.appendChild(main);
        layout.appendChild(railEl());
      } else if (region.region === 'pip') {
        const pip = document.createElement('div');
        pip.setAttribute('data-testid', 'pip-region');
        const toggle = document.createElement('button');
        toggle.setAttribute('data-testid', 'pip-rail-toggle');
        toggle.textContent = region.railVisible ? 'Hide rail' : 'Show rail';
        pip.appendChild(toggle);

        const split = document.createElement('div');
        split.setAttribute('data-testid', 'pip-split');
        split.style.display = 'grid';
        split.style.width = '600px';
        split.style.height = '300px';
        const apply = (ratio: number) => {
          split.style.gridTemplateColumns = `${ratio}fr 8px ${1 - ratio}fr`;
          split.setAttribute('data-ratio', String(ratio));
        };
        apply(region.splitRatio);

        const left = chat('pip-pane-chat');
        const divider = document.createElement('div');
        divider.setAttribute('data-testid', 'pip-divider');
        divider.style.cursor = 'col-resize';
        const right = document.createElement('div');
        right.setAttribute('data-testid', 'pip-pane-app');
        right.appendChild(panelEl(region.pipApp ? region.pipApp.id : ''));

        // The real drag: clamp with the shipped bounds (passed in).
        let dragging = false;
        const clamp = (r: number) => Math.min(bounds.max, Math.max(bounds.min, r));
        divider.addEventListener('pointerdown', () => {
          dragging = true;
        });
        window.addEventListener('pointermove', (ev) => {
          if (!dragging) return;
          const rect = split.getBoundingClientRect();
          if (rect.width <= 0) return;
          apply(clamp((ev.clientX - rect.left) / rect.width));
        });
        window.addEventListener('pointerup', () => {
          dragging = false;
        });

        split.appendChild(left);
        split.appendChild(divider);
        split.appendChild(right);
        pip.appendChild(split);
        layout.appendChild(pip);
        if (region.railVisible) layout.appendChild(railEl());
      } else {
        layout.appendChild(chat('chat-panel'));
        layout.appendChild(railEl());
      }

      root.appendChild(layout);
    },
    { region, bounds }
  );
}

test.describe('MCP Apps DisplayMode layout (D-062)', () => {
  const bounds = { min: RATIO_MIN, max: RATIO_MAX };

  test('fullscreen: the app replaces chat and a tab strip switches Chat ↔ apps', async ({ page }) => {
    const a = appRef('alpha');
    const b = appRef('beta');
    let model: LayoutModel = reduceLayout(INITIAL_LAYOUT, {
      type: 'request-display-mode',
      app: a,
      mode: 'fullscreen'
    });
    model = reduceLayout(model, { type: 'request-display-mode', app: b, mode: 'fullscreen' });

    // Two fullscreen apps → Chat + 2 app tabs; the most-recent app is active.
    await mount(page, computeRegion(model), bounds);
    await expect(page.getByTestId('playground-layout')).toHaveAttribute('data-region', 'fullscreen');
    await expect(page.getByTestId('app-tab')).toHaveCount(3);
    // The active app (beta) replaces chat — the app panel is shown, no chat panel.
    await expect(page.getByTestId('app-panel-body')).toHaveAttribute('data-app-id', b.id);
    await expect(page.getByTestId('chat-panel')).toHaveCount(0);

    // Switch to the Chat tab → chat returns, app panel gone.
    model = reduceLayout(model, { type: 'activate-tab', id: CHAT_TAB_ID });
    await mount(page, computeRegion(model), bounds);
    await expect(page.getByTestId('chat-panel')).toBeVisible();
    await expect(page.getByTestId('app-panel-body')).toHaveCount(0);

    // Close alpha → its tab is gone (Chat + beta remain).
    model = reduceLayout(model, { type: 'close-app', id: a.id });
    await mount(page, computeRegion(model), bounds);
    const tabIds = await page.getByTestId('app-tab').evaluateAll((els) =>
      els.map((e) => e.getAttribute('data-tab-id'))
    );
    expect(tabIds).toEqual([CHAT_TAB_ID, b.id]);
  });

  test('pip: a 50/50 split with the rail hidden by default; the toggle reopens it', async ({
    page
  }) => {
    const a = appRef('alpha');
    const model = reduceLayout(INITIAL_LAYOUT, { type: 'request-display-mode', app: a, mode: 'pip' });
    const region = computeRegion(model);
    await mount(page, region, bounds);

    await expect(page.getByTestId('playground-layout')).toHaveAttribute('data-region', 'pip');
    // 50/50 split.
    await expect(page.getByTestId('pip-split')).toHaveAttribute('data-ratio', '0.5');
    await expect(page.getByTestId('pip-pane-chat')).toBeVisible();
    await expect(page.getByTestId('pip-pane-app')).toBeVisible();
    // Rail hidden by default in pip.
    await expect(page.getByTestId('detail-rail')).toHaveCount(0);
    await expect(page.getByTestId('pip-rail-toggle')).toBeVisible();

    // Toggling the rail reopens it WITHOUT resetting the split ratio (D-061).
    const toggled = reduceLayout(model, { type: 'toggle-rail' });
    await mount(page, computeRegion(toggled), bounds);
    await expect(page.getByTestId('detail-rail')).toBeVisible();
    await expect(page.getByTestId('pip-split')).toHaveAttribute('data-ratio', '0.5');
  });

  test('pip: dragging the divider clamps the split ratio to sane bounds', async ({ page }) => {
    const a = appRef('alpha');
    const model = reduceLayout(INITIAL_LAYOUT, { type: 'request-display-mode', app: a, mode: 'pip' });
    await mount(page, computeRegion(model), bounds);

    const split = page.getByTestId('pip-split');
    const divider = page.getByTestId('pip-divider');
    const box = await split.boundingBox();
    expect(box).not.toBeNull();
    const b = box!;

    // Drag far past the right edge → the ratio clamps at RATIO_MAX, never 1.0.
    await divider.hover();
    await page.mouse.down();
    await page.mouse.move(b.x + b.width * 2, b.y + b.height / 2, { steps: 5 });
    await page.mouse.up();
    const high = Number(await split.getAttribute('data-ratio'));
    expect(high).toBeLessThanOrEqual(RATIO_MAX);
    expect(high).toBe(clampRatio(2));

    // Drag far past the left edge → the ratio clamps at RATIO_MIN, never 0.0.
    await divider.hover();
    await page.mouse.down();
    await page.mouse.move(b.x - b.width, b.y + b.height / 2, { steps: 5 });
    await page.mouse.up();
    const low = Number(await split.getAttribute('data-ratio'));
    expect(low).toBeGreaterThanOrEqual(RATIO_MIN);
    expect(low).toBe(clampRatio(-1));
  });

  test('inline is unchanged: no page region takeover (109b regression guard)', async ({ page }) => {
    // An inline app stays in the chat scroll — the page stays in the chat
    // region with the rail visible; there is no tab strip and no split.
    const a = appRef('alpha');
    const model = reduceLayout(INITIAL_LAYOUT, {
      type: 'request-display-mode',
      app: a,
      mode: 'inline'
    });
    await mount(page, computeRegion(model), bounds);
    await expect(page.getByTestId('playground-layout')).toHaveAttribute('data-region', 'chat');
    await expect(page.getByTestId('chat-panel')).toBeVisible();
    await expect(page.getByTestId('detail-rail')).toBeVisible();
    await expect(page.getByTestId('app-tabstrip')).toHaveCount(0);
    await expect(page.getByTestId('pip-split')).toHaveCount(0);
  });

  test('runtime transition inline → pip → fullscreen, then teardown to chat', async ({ page }) => {
    const a = appRef(appId('srv', 'home'));

    // inline — chat region, chat surface present.
    let model = reduceLayout(INITIAL_LAYOUT, { type: 'request-display-mode', app: a, mode: 'inline' });
    await mount(page, computeRegion(model), bounds);
    await expect(page.getByTestId('playground-layout')).toHaveAttribute('data-region', 'chat');
    await expect(page.getByTestId('chat-panel')).toBeVisible();

    // → pip — the same chat surface persists in the split's left pane (the
    // session is not reloaded; only the layout changes).
    model = reduceLayout(model, { type: 'request-display-mode', app: a, mode: 'pip' });
    await mount(page, computeRegion(model), bounds);
    await expect(page.getByTestId('playground-layout')).toHaveAttribute('data-region', 'pip');
    await expect(page.getByTestId('pip-pane-chat')).toBeVisible();
    await expect(page.getByTestId('detail-rail')).toHaveCount(0);

    // → fullscreen — tab strip, rail restored.
    model = reduceLayout(model, { type: 'request-display-mode', app: a, mode: 'fullscreen' });
    await mount(page, computeRegion(model), bounds);
    await expect(page.getByTestId('playground-layout')).toHaveAttribute('data-region', 'fullscreen');
    await expect(page.getByTestId('detail-rail')).toBeVisible();

    // teardown — back to the chat + rail default.
    model = reduceLayout(model, { type: 'teardown' });
    await mount(page, computeRegion(model), bounds);
    await expect(page.getByTestId('playground-layout')).toHaveAttribute('data-region', 'chat');
    await expect(page.getByTestId('chat-panel')).toBeVisible();
    await expect(page.getByTestId('detail-rail')).toBeVisible();
  });
});
