// Harbor Console — Playground DisplayMode layout state machine.
//
// This module is the PURE, DOM-free core of the Playground page-level layout
// that honours an active MCP App's DisplayMode (D-062). It maps the set of
// open page-level apps onto a region routing the page renders:
//
//   - `chat`        — the default: chat + composer in the main column, the
//                     right rail visible. `inline` apps render inside the chat
//                     scroll (109b, unchanged) and never enter this module —
//                     an inline app is NOT a page-level app.
//   - `fullscreen`  — one or more apps replace the chat + composer region; a
//                     tab strip (Chat + one tab per fullscreen app) switches
//                     focus. Closing an app removes its tab and returns focus
//                     to Chat.
//   - `pip`         — ONE app beside chat in a resizable 50/50 split; the right
//                     rail is hidden by default with a toggle to reopen it.
//                     (Distinct from PG-6's two-agent comparison — D-064.)
//
// The page reduces this model on three drivers (all without reloading the
// session): the AppBridge `onrequestdisplaymode` request from a hosted app,
// operator affordances (mode-switch / close), and teardown.
//
// Everything here is a pure function so the region routing, the ratio clamp,
// and the tab add/remove/activate logic are unit-testable without a DOM
// (CLAUDE.md §5 fail-loudly; the page is the only stateful holder).
//
// Invariant the reducer maintains: `apps` holds EITHER N `fullscreen` apps OR
// exactly one `pip` app — never both. `pip` is one app beside chat; promoting
// an app to `fullscreen` drops any `pip` app, and requesting `pip` drops the
// fullscreen tab set. This keeps the page in exactly one region at a time and
// makes `computeRegion` total.

/** An MCP App's declared rendering mode for the Console (D-062, glossary). */
export type DisplayMode = 'inline' | 'fullscreen' | 'pip';

/** The page-level region the layout routes to. `inline` apps never get here. */
export type RegionKind = 'chat' | 'fullscreen' | 'pip';

/** The fixed tab id for the Chat tab in the fullscreen tab strip. */
export const CHAT_TAB_ID = 'chat';

/** Split-ratio bounds (fraction of width given to the chat column in `pip`). */
export const RATIO_MIN = 0.2;
export const RATIO_MAX = 0.8;
export const DEFAULT_RATIO = 0.5;

/**
 * A page-level open app — an MCP App promoted out of the chat scroll into a
 * `fullscreen` tab or the `pip` panel. The `id` is the stable identity the tab
 * strip and the reducer key on (the caller derives it as
 * `${serverID}::${resourceUri}`).
 */
export interface OpenApp {
  /** Stable identity — `${serverID}::${resourceUri}`. */
  id: string;
  /** Human label shown on the fullscreen tab / pip panel header. */
  title: string;
  /** The MCP server (source id) hosting the app. */
  serverID: string;
  /** The `ui://`-scheme URI of the app's UI document. */
  resourceUri: string;
  /** The per-server raw-HTML trust flag — forwarded to the renderer sandbox. */
  rawHtmlTrusted: boolean;
  /**
   * The stable per-invocation id of the tool call that declared the app — the
   * correlation key the renderer passes to `mcp.apps.tool_context` for the
   * after-init Data Delivery push. Absent when the discovery carried none.
   */
  toolCallId?: string;
  /**
   * The server-side tool name that declared the app — projected onto the
   * `ui/initialize` host-context `toolInfo` so the page-level render names the
   * originating call exactly as the inline render does. Absent when the
   * discovery carried none.
   */
  toolName?: string;
  /** The page-level mode: `fullscreen` or `pip` (never `inline` here). */
  mode: Exclude<DisplayMode, 'inline'>;
}

/** A reference to an app that may be opened — `OpenApp` minus its `mode`. */
export type AppRef = Omit<OpenApp, 'mode'>;

/** The full layout state the page holds. Pure data; the page is the holder. */
export interface LayoutModel {
  /** Page-level apps. Invariant: all `fullscreen` OR exactly one `pip`. */
  apps: OpenApp[];
  /** The focused fullscreen tab — `CHAT_TAB_ID` or an app id. */
  activeTabId: string;
  /** Whether the right rail is shown. Hidden by default while in `pip`. */
  railVisible: boolean;
  /** The chat-column width fraction in `pip` (clamped to [MIN, MAX]). */
  splitRatio: number;
}

/** The starting layout: chat + rail, no page-level apps. */
export const INITIAL_LAYOUT: LayoutModel = {
  apps: [],
  activeTabId: CHAT_TAB_ID,
  railVisible: true,
  splitRatio: DEFAULT_RATIO,
};

/** One entry in the fullscreen tab strip. */
export interface LayoutTab {
  id: string;
  label: string;
  /** The Chat tab is never closable; app tabs are. */
  closable: boolean;
}

/**
 * The resolved region the page renders. A total projection of `LayoutModel` —
 * `computeRegion` never throws and always returns one of the three regions.
 */
export interface RegionLayout {
  region: RegionKind;
  /** Fullscreen tab strip (Chat + apps); empty in `chat` / `pip`. */
  tabs: LayoutTab[];
  /** The resolved active tab (falls back to Chat when the active id is gone). */
  activeTabId: string;
  /** The app shown in the fullscreen body, or null when the Chat tab is active. */
  fullscreenApp: OpenApp | null;
  /** The single app shown beside chat in `pip`, or null. */
  pipApp: OpenApp | null;
  /** Whether the right rail is visible in this region. */
  railVisible: boolean;
  /** The clamped chat-column width fraction for the `pip` split. */
  splitRatio: number;
}

/** The actions the page dispatches against the layout model. */
export type LayoutAction =
  | { type: 'request-display-mode'; app: AppRef; mode: DisplayMode }
  | { type: 'close-app'; id: string }
  | { type: 'activate-tab'; id: string }
  | { type: 'toggle-rail' }
  | { type: 'set-ratio'; ratio: number }
  | { type: 'teardown' };

/** Clamps a split ratio to sane bounds; NaN collapses to the default. */
export function clampRatio(ratio: number): number {
  if (Number.isNaN(ratio)) return DEFAULT_RATIO;
  return Math.min(RATIO_MAX, Math.max(RATIO_MIN, ratio));
}

function upsertApp(apps: OpenApp[], app: AppRef, mode: Exclude<DisplayMode, 'inline'>): OpenApp[] {
  const others = apps.filter((a) => a.id !== app.id);
  return [...others, { ...app, mode }];
}

/**
 * The pure reducer. Returns a NEW model; never mutates the input. Maintains the
 * one-region invariant (all `fullscreen` OR a single `pip`). The page calls
 * this on every driver (app request, operator affordance, teardown).
 */
export function reduceLayout(model: LayoutModel, action: LayoutAction): LayoutModel {
  switch (action.type) {
    case 'request-display-mode': {
      const { app, mode } = action;
      if (mode === 'inline') {
        // The app returned to the chat scroll — drop it from the page layout.
        const apps = model.apps.filter((a) => a.id !== app.id);
        if (apps.length === 0) {
          return { ...model, apps, activeTabId: CHAT_TAB_ID, railVisible: true };
        }
        const activeTabId = model.activeTabId === app.id ? CHAT_TAB_ID : model.activeTabId;
        return { ...model, apps, activeTabId };
      }
      if (mode === 'fullscreen') {
        // Promote to a fullscreen tab. Drop any pip app (pip and fullscreen are
        // mutually exclusive page regions); restore the rail to its default.
        const fullscreenApps = model.apps.filter((a) => a.mode === 'fullscreen');
        const apps = upsertApp(fullscreenApps, app, 'fullscreen');
        return { ...model, apps, activeTabId: app.id, railVisible: true };
      }
      // mode === 'pip' — one app beside chat. Replace the whole set; hide the
      // rail by default (the toggle reopens it). The split ratio is preserved.
      return {
        ...model,
        apps: [{ ...app, mode: 'pip' }],
        activeTabId: CHAT_TAB_ID,
        railVisible: false,
      };
    }

    case 'close-app': {
      const apps = model.apps.filter((a) => a.id !== action.id);
      if (apps.length === 0) {
        // Last app closed — back to the chat + rail default (ratio remembered).
        return { ...model, apps, activeTabId: CHAT_TAB_ID, railVisible: true };
      }
      const activeTabId = model.activeTabId === action.id ? CHAT_TAB_ID : model.activeTabId;
      return { ...model, apps, activeTabId };
    }

    case 'activate-tab':
      return { ...model, activeTabId: action.id };

    case 'toggle-rail':
      return { ...model, railVisible: !model.railVisible };

    case 'set-ratio':
      return { ...model, splitRatio: clampRatio(action.ratio) };

    case 'teardown':
      // Return to the chat + rail default. The split ratio is Console-local
      // view state that may persist (D-061), so it is carried across teardown.
      return { ...INITIAL_LAYOUT, splitRatio: clampRatio(model.splitRatio) };

    default:
      return model;
  }
}

/**
 * The pure projection `(LayoutModel) → RegionLayout`. Total: always returns one
 * of the three regions. `chat` when there are no page-level apps (inline apps
 * live in the chat scroll); `pip` when exactly one app is in pip mode;
 * `fullscreen` when one or more apps are in fullscreen mode.
 */
export function computeRegion(model: LayoutModel): RegionLayout {
  const splitRatio = clampRatio(model.splitRatio);
  const pipApp = model.apps.find((a) => a.mode === 'pip') ?? null;
  const fullscreenApps = model.apps.filter((a) => a.mode === 'fullscreen');

  if (fullscreenApps.length > 0) {
    const tabs: LayoutTab[] = [
      { id: CHAT_TAB_ID, label: 'Chat', closable: false },
      ...fullscreenApps.map((a) => ({ id: a.id, label: a.title, closable: true })),
    ];
    const activeTabId = tabs.some((t) => t.id === model.activeTabId)
      ? model.activeTabId
      : CHAT_TAB_ID;
    const fullscreenApp =
      activeTabId === CHAT_TAB_ID
        ? null
        : (fullscreenApps.find((a) => a.id === activeTabId) ?? null);
    return {
      region: 'fullscreen',
      tabs,
      activeTabId,
      fullscreenApp,
      pipApp: null,
      railVisible: model.railVisible,
      splitRatio,
    };
  }

  if (pipApp) {
    return {
      region: 'pip',
      tabs: [],
      activeTabId: CHAT_TAB_ID,
      fullscreenApp: null,
      pipApp,
      railVisible: model.railVisible,
      splitRatio,
    };
  }

  return {
    region: 'chat',
    tabs: [],
    activeTabId: CHAT_TAB_ID,
    fullscreenApp: null,
    pipApp: null,
    railVisible: true,
    splitRatio,
  };
}

/** Derives the stable page-level app id from a server + resource pair. */
export function appId(serverID: string, resourceUri: string): string {
  return `${serverID}::${resourceUri}`;
}
