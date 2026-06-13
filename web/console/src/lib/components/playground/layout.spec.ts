// Harbor Console — Playground DisplayMode layout state-machine unit tests.
//
// Pins the pure `layout.ts` core (D-062): region routing, the split-ratio
// clamp, fullscreen tab add / remove / activate, and the runtime transitions
// `inline ↔ fullscreen ↔ pip` + teardown. No DOM — the page is the only
// stateful holder; this proves its logic deterministically.

import { describe, it, expect } from 'vitest';

import {
  CHAT_TAB_ID,
  DEFAULT_RATIO,
  INITIAL_LAYOUT,
  RATIO_MAX,
  RATIO_MIN,
  appId,
  clampRatio,
  computeRegion,
  reduceLayout,
  type AppRef,
  type LayoutModel
} from './layout.js';

function ref(id: string): AppRef {
  return {
    id,
    title: `App ${id}`,
    serverID: id.split('::')[0] ?? id,
    resourceUri: `ui://${id}`,
    rawHtmlTrusted: false
  };
}

const A = ref(appId('srvA', 'home'));
const B = ref(appId('srvB', 'panel'));

describe('clampRatio', () => {
  it('clamps below the lower bound', () => {
    expect(clampRatio(0.0)).toBe(RATIO_MIN);
    expect(clampRatio(-1)).toBe(RATIO_MIN);
  });
  it('clamps above the upper bound', () => {
    expect(clampRatio(1.0)).toBe(RATIO_MAX);
    expect(clampRatio(5)).toBe(RATIO_MAX);
  });
  it('passes a value already in range', () => {
    expect(clampRatio(0.5)).toBe(0.5);
    expect(clampRatio(0.33)).toBe(0.33);
  });
  it('collapses NaN to the default', () => {
    expect(clampRatio(Number.NaN)).toBe(DEFAULT_RATIO);
  });
});

describe('computeRegion — default', () => {
  it('inline mode keeps the chat region — no page takeover (109b inline unchanged)', () => {
    // The initial layout (no page-level apps) is the chat + rail default; an
    // `inline` request never promotes an app to a page region.
    const region = computeRegion(INITIAL_LAYOUT);
    expect(region.region).toBe('chat');
    expect(region.tabs).toEqual([]);
    expect(region.railVisible).toBe(true);
    expect(region.fullscreenApp).toBeNull();
    expect(region.pipApp).toBeNull();

    const afterInline = reduceLayout(INITIAL_LAYOUT, {
      type: 'request-display-mode',
      app: A,
      mode: 'inline'
    });
    expect(computeRegion(afterInline).region).toBe('chat');
    expect(afterInline.apps).toEqual([]);
  });
});

describe('reduceLayout — fullscreen', () => {
  it('promotes an app to a fullscreen tab and focuses it', () => {
    const m = reduceLayout(INITIAL_LAYOUT, {
      type: 'request-display-mode',
      app: A,
      mode: 'fullscreen'
    });
    const region = computeRegion(m);
    expect(region.region).toBe('fullscreen');
    // Chat tab + one app tab.
    expect(region.tabs.map((t) => t.id)).toEqual([CHAT_TAB_ID, A.id]);
    expect(region.tabs[0].closable).toBe(false);
    expect(region.tabs[1].closable).toBe(true);
    expect(region.activeTabId).toBe(A.id);
    expect(region.fullscreenApp?.id).toBe(A.id);
  });

  it('multiple fullscreen apps yield multiple tabs', () => {
    let m = reduceLayout(INITIAL_LAYOUT, { type: 'request-display-mode', app: A, mode: 'fullscreen' });
    m = reduceLayout(m, { type: 'request-display-mode', app: B, mode: 'fullscreen' });
    const region = computeRegion(m);
    expect(region.tabs.map((t) => t.id)).toEqual([CHAT_TAB_ID, A.id, B.id]);
    expect(region.activeTabId).toBe(B.id);
  });

  it('activate-tab switches the active fullscreen tab; Chat tab shows chat', () => {
    let m = reduceLayout(INITIAL_LAYOUT, { type: 'request-display-mode', app: A, mode: 'fullscreen' });
    m = reduceLayout(m, { type: 'activate-tab', id: CHAT_TAB_ID });
    const region = computeRegion(m);
    expect(region.activeTabId).toBe(CHAT_TAB_ID);
    expect(region.fullscreenApp).toBeNull();
  });

  it('closing an app removes its tab and returns focus to Chat', () => {
    let m = reduceLayout(INITIAL_LAYOUT, { type: 'request-display-mode', app: A, mode: 'fullscreen' });
    m = reduceLayout(m, { type: 'close-app', id: A.id });
    const region = computeRegion(m);
    expect(region.region).toBe('chat');
    expect(region.railVisible).toBe(true);
  });

  it('closing one of several fullscreen apps keeps the rest', () => {
    let m = reduceLayout(INITIAL_LAYOUT, { type: 'request-display-mode', app: A, mode: 'fullscreen' });
    m = reduceLayout(m, { type: 'request-display-mode', app: B, mode: 'fullscreen' });
    m = reduceLayout(m, { type: 'close-app', id: B.id });
    const region = computeRegion(m);
    expect(region.region).toBe('fullscreen');
    expect(region.tabs.map((t) => t.id)).toEqual([CHAT_TAB_ID, A.id]);
    // Active tab was the closed app → falls back to Chat.
    expect(region.activeTabId).toBe(CHAT_TAB_ID);
  });
});

describe('reduceLayout — pip', () => {
  it('renders one app beside chat and hides the rail by default', () => {
    const m = reduceLayout(INITIAL_LAYOUT, { type: 'request-display-mode', app: A, mode: 'pip' });
    const region = computeRegion(m);
    expect(region.region).toBe('pip');
    expect(region.pipApp?.id).toBe(A.id);
    expect(region.railVisible).toBe(false);
    expect(region.splitRatio).toBe(DEFAULT_RATIO);
  });

  it('is ONE app beside chat — a second pip request replaces the first (not PG-6)', () => {
    let m = reduceLayout(INITIAL_LAYOUT, { type: 'request-display-mode', app: A, mode: 'pip' });
    m = reduceLayout(m, { type: 'request-display-mode', app: B, mode: 'pip' });
    expect(m.apps).toHaveLength(1);
    expect(computeRegion(m).pipApp?.id).toBe(B.id);
  });

  it('the rail toggle reopens the rail WITHOUT resetting the split ratio (D-061)', () => {
    let m = reduceLayout(INITIAL_LAYOUT, { type: 'request-display-mode', app: A, mode: 'pip' });
    m = reduceLayout(m, { type: 'set-ratio', ratio: 0.65 });
    m = reduceLayout(m, { type: 'toggle-rail' });
    const region = computeRegion(m);
    expect(region.railVisible).toBe(true);
    expect(region.splitRatio).toBe(0.65);
  });

  it('clamps the split ratio on drag (lower / upper bounds)', () => {
    let m = reduceLayout(INITIAL_LAYOUT, { type: 'request-display-mode', app: A, mode: 'pip' });
    m = reduceLayout(m, { type: 'set-ratio', ratio: 0.95 });
    expect(computeRegion(m).splitRatio).toBe(RATIO_MAX);
    m = reduceLayout(m, { type: 'set-ratio', ratio: 0.05 });
    expect(computeRegion(m).splitRatio).toBe(RATIO_MIN);
  });

  it('closing the pip app returns to chat + rail default', () => {
    let m = reduceLayout(INITIAL_LAYOUT, { type: 'request-display-mode', app: A, mode: 'pip' });
    m = reduceLayout(m, { type: 'close-app', id: A.id });
    const region = computeRegion(m);
    expect(region.region).toBe('chat');
    expect(region.railVisible).toBe(true);
  });
});

describe('reduceLayout — runtime transitions (no session reload)', () => {
  it('transitions inline → pip → fullscreen for one app', () => {
    // inline: no page region.
    let m: LayoutModel = reduceLayout(INITIAL_LAYOUT, {
      type: 'request-display-mode',
      app: A,
      mode: 'inline'
    });
    expect(computeRegion(m).region).toBe('chat');

    // pip: split, rail hidden.
    m = reduceLayout(m, { type: 'request-display-mode', app: A, mode: 'pip' });
    expect(computeRegion(m).region).toBe('pip');
    expect(computeRegion(m).railVisible).toBe(false);

    // fullscreen: tab strip, rail restored to default.
    m = reduceLayout(m, { type: 'request-display-mode', app: A, mode: 'fullscreen' });
    const region = computeRegion(m);
    expect(region.region).toBe('fullscreen');
    expect(region.railVisible).toBe(true);
    expect(region.tabs.map((t) => t.id)).toEqual([CHAT_TAB_ID, A.id]);
  });

  it('pip and fullscreen are mutually exclusive page regions', () => {
    // App A fullscreen, then a pip request collapses to the single pip region.
    let m = reduceLayout(INITIAL_LAYOUT, { type: 'request-display-mode', app: A, mode: 'fullscreen' });
    m = reduceLayout(m, { type: 'request-display-mode', app: B, mode: 'pip' });
    const region = computeRegion(m);
    expect(region.region).toBe('pip');
    expect(region.pipApp?.id).toBe(B.id);
    expect(m.apps).toHaveLength(1);
  });
});

describe('reduceLayout — teardown', () => {
  it('returns the layout to the chat + rail default but remembers the ratio', () => {
    let m = reduceLayout(INITIAL_LAYOUT, { type: 'request-display-mode', app: A, mode: 'pip' });
    m = reduceLayout(m, { type: 'set-ratio', ratio: 0.7 });
    m = reduceLayout(m, { type: 'teardown' });
    const region = computeRegion(m);
    expect(region.region).toBe('chat');
    expect(region.railVisible).toBe(true);
    expect(m.apps).toEqual([]);
    // The split ratio is Console-local view state that persists (D-061).
    expect(m.splitRatio).toBe(0.7);
  });
});

describe('appId', () => {
  it('derives a stable id from server + resource', () => {
    expect(appId('srvA', 'ui://home')).toBe('srvA::ui://home');
  });
});
