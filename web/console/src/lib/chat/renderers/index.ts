// The canonical Harbor Console renderer registry (Phase 73l / D-120).
//
// This module is the SHARED dispatch table that maps a MIME type onto a
// `.svelte` renderer component. It lives at the canonical path
// `web/console/src/lib/chat/renderers/` per Brief 12 §"The shared chat /
// playground library" and CLAUDE.md §4.5 #11.
//
// # The encapsulate-first / extend-on-second-consumer contract
//
// Phase 73l (Stage 2.1) is the FIRST in-staging consumer — the Artifacts
// page preview pane. It introduces this dispatch table plus the six MIME
// renderers the Artifacts preview needs: markdown, code, image, pdf,
// audio, json.
//
// Phase 73n (Stage 2.3) Playground is the SECOND consumer. It EXTENDS
// this registry with chat-bubble / tool-call / diff renderers. It does
// so by calling `registerRenderer` from its own module init — it does
// NOT edit this dispatch core. The dispatch table is open for
// registration, closed for modification: a new renderer is a new
// `register*` call, never a new `case` in a hard-coded switch.
//
// # The dispatch contract
//
//   - `RendererDescriptor` pairs a Svelte component with the metadata
//     the host UI needs (a stable `source` id for tests + telemetry).
//   - `registerRenderer(predicate, descriptor)` appends a rule. Rules
//     are evaluated in registration order; the first matching predicate
//     wins. The six built-in MIME renderers register at module load.
//   - `dispatchRenderer(mime)` returns the winning descriptor, or the
//     `fallbackRenderer` when no rule matches (an unrenderable MIME — the
//     host shows a "Preview unavailable — Download" placeholder).
//
// The registry is a process-wide singleton (the renderer set is the same
// for every page); it carries no per-request state.

import type { Component } from 'svelte';

import type { DisplayModeRequest, McpUiDisplayMode, MCPAppHostClient, MCPAppRefView } from './app-bridge-host.js';
import AudioRenderer from './audio.svelte';
import CodeRenderer from './code.svelte';
import FallbackRenderer from './fallback.svelte';
import ImageRenderer from './image.svelte';
import JsonRenderer from './json.svelte';
import MarkdownRenderer from './markdown.svelte';
import McpAppRenderer from './mcp-app.svelte';
import PdfRenderer from './pdf.svelte';

/**
 * The synthetic MIME the chat host dispatches an inline MCP App widget under.
 * An MCP App is not discovered by a real media type — it is triggered by a
 * tool result's `_meta.ui.resourceUri` (projected by the Protocol as an
 * `MCPAppRef`). The chat host maps that onto this MIME so the same
 * `registerRenderer` / `dispatchRenderer` registry serves it.
 */
export const MCP_APP_INLINE_MIME = 'application/vnd.harbor.mcp-app';

/**
 * The props every renderer component accepts. A renderer receives the
 * artifact's MIME type, a resolved source URL (a presigned URL per
 * D-026 — renderers NEVER receive inline heavy bytes), and an optional
 * filename for display.
 */
export interface RendererProps {
  /** The IANA media type of the content being rendered. */
  mime: string;
  /** A resolved URL the renderer fetches the content from (presigned per D-026). */
  src: string;
  /** Optional display filename. */
  filename?: string;
  /**
   * The MCP App reference, set ONLY when dispatching the inline MCP App
   * renderer (MIME {@link MCP_APP_INLINE_MIME}). Carries the `ui://` resource
   * URI, the negotiated display mode, and the per-server raw-HTML trust flag.
   */
  app?: MCPAppRefView;
  /** The MCP server (source id) hosting the app — paired with {@link app}. */
  serverID?: string;
  /**
   * The injected Harbor Protocol surface the MCP App renderer drives every
   * app→host request through (D-173 — never a direct MCP transport). Paired
   * with {@link app}; the caller (the Playground page) provides it.
   */
  appHostClient?: MCPAppHostClient;
  /**
   * The display modes the host can apply. Defaults to inline-only when absent
   * (the chat-scroll host). The Playground App panel passes the full set so the
   * app can request fullscreen / pip. Forwarded to the AppBridge host.
   */
  availableDisplayModes?: McpUiDisplayMode[];
  /**
   * Called when the hosted app requests a display mode (the AppBridge
   * `onrequestdisplaymode` seam). The Playground page reduces this into its
   * page-level layout state machine to switch `inline ↔ fullscreen ↔ pip`
   * without reloading the session. Forwarded to the AppBridge host.
   */
  onDisplayModeRequest?: (req: DisplayModeRequest) => void;
}

/**
 * A registry entry: the Svelte component plus its stable `source` id.
 * The `source` id is exposed by the host as a `data-renderer-source`
 * DOM attribute so a Playwright spec can assert which renderer handled a
 * preview without reaching into component internals.
 */
export interface RendererDescriptor {
  /** A stable identifier for the renderer — e.g. `"markdown"`, `"image"`. */
  source: string;
  /** The Svelte component that renders the content. */
  component: Component<RendererProps>;
}

/** A predicate deciding whether a renderer rule applies to a MIME type. */
export type RendererPredicate = (mime: string) => boolean;

interface RendererRule {
  predicate: RendererPredicate;
  descriptor: RendererDescriptor;
}

/** The ordered rule list. First-match-wins on dispatch. */
const rules: RendererRule[] = [];

/**
 * The descriptor returned when no rule matches — an unrenderable MIME.
 * The host renders a "Preview unavailable — Download to view"
 * placeholder. Distinct from a registered renderer so the dispatch
 * result is never `undefined`.
 */
export const fallbackRenderer: RendererDescriptor = {
  source: 'fallback',
  component: FallbackRenderer
};

/**
 * Appends a renderer rule. Rules are evaluated in registration order;
 * the first matching predicate wins. This is the seam Phase 73n
 * Playground uses to add chat-bubble / tool-call / diff renderers — it
 * never edits the dispatch core.
 */
export function registerRenderer(predicate: RendererPredicate, descriptor: RendererDescriptor): void {
  rules.push({ predicate, descriptor });
}

/**
 * Returns the `RendererDescriptor` for the given MIME type — the first
 * registered rule whose predicate matches, or `fallbackRenderer` when
 * none match. Never returns `undefined`.
 */
export function dispatchRenderer(mime: string): RendererDescriptor {
  const normalised = (mime ?? '').trim().toLowerCase();
  for (const rule of rules) {
    if (rule.predicate(normalised)) {
      return rule.descriptor;
    }
  }
  return fallbackRenderer;
}

/** A snapshot of every registered renderer `source` id, for tests / telemetry. */
export function registeredSources(): string[] {
  return rules.map((r) => r.descriptor.source);
}

/** Convenience predicate: exact MIME match (case-insensitive). */
export function mimeIs(...mimes: string[]): RendererPredicate {
  const set = new Set(mimes.map((m) => m.toLowerCase()));
  return (mime) => set.has(mime);
}

/** Convenience predicate: MIME type-prefix match (e.g. `image/`). */
export function mimePrefix(...prefixes: string[]): RendererPredicate {
  const lowered = prefixes.map((p) => p.toLowerCase());
  return (mime) => lowered.some((p) => mime.startsWith(p));
}

/* ------------------------------------------------------------------ */
/* The six built-in MIME renderers (Phase 73l first-consumer set).      */
/* Registration order is the dispatch priority — most-specific first.   */
/* ------------------------------------------------------------------ */

registerRenderer(mimePrefix('image/'), { source: 'image', component: ImageRenderer });
registerRenderer(mimeIs('application/pdf'), { source: 'pdf', component: PdfRenderer });
registerRenderer(mimePrefix('audio/'), { source: 'audio', component: AudioRenderer });
registerRenderer(
  mimeIs('application/json', 'application/ld+json', 'text/json'),
  { source: 'json', component: JsonRenderer }
);
registerRenderer(mimeIs('text/markdown', 'text/x-markdown'), {
  source: 'markdown',
  component: MarkdownRenderer
});
// Code is the broadest text rule — it registers last so the more
// specific markdown / json text rules win first.
registerRenderer(
  (mime) =>
    mime.startsWith('text/') ||
    mime === 'application/javascript' ||
    mime === 'application/xml' ||
    mime === 'application/x-yaml' ||
    mime === 'application/x-sh',
  { source: 'code', component: CodeRenderer }
);

// The inline MCP App renderer (Phase 109b) — dispatched under the synthetic
// MCP-App MIME, never a real media type. Registered after the code rule so it
// only matches its own exact MIME.
registerRenderer(mimeIs(MCP_APP_INLINE_MIME), {
  source: 'mcp-app',
  component: McpAppRenderer
});
