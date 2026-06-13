// The MCP Apps host security policy — the iframe sandbox token set, the strict
// CSP, the `ui://`-document wrapper, and the postMessage origin/source guard.
//
// This is the single, documented, reviewed home of the sandbox + CSP surface
// (D-173, CLAUDE.md §7). It is deliberately DEPENDENCY-FREE — it imports
// nothing (not even the AppBridge), so the renderer, the host wrapper, and the
// Playwright sandbox-escape spec can all consume the EXACT same constants with
// no risk of drift between what the test asserts and what the renderer ships.

/* ------------------------------------------------------------------ */
/* The sandbox + CSP constants — the documented, reviewed surface      */
/* ------------------------------------------------------------------ */

/**
 * The base iframe `sandbox` token set applied in EVERY state.
 *
 * `allow-scripts` is mandatory (the app bundle runs JavaScript). Crucially
 * `allow-same-origin` is NEVER part of any token set: a sandboxed iframe
 * without `allow-same-origin` is forced into an opaque ("null") origin, which
 * is what blocks all parent-DOM, cookie, and `localStorage` access. Combining
 * `allow-same-origin` with `allow-scripts` would let the framed content remove
 * its own sandbox and is FORBIDDEN in all states (D-173, CLAUDE.md §7). This
 * is the single, reviewed constant — do not assemble sandbox tokens anywhere
 * else.
 */
export const APP_IFRAME_SANDBOX_BASE = 'allow-scripts';

/**
 * Returns the iframe `sandbox` attribute value for the given trust state. A
 * raw-HTML-trusted server (the operator opted in via
 * `mcp.servers.set_raw_html_trust`) additionally gets `allow-forms` +
 * `allow-popups` for richer interaction — but NEVER `allow-same-origin`,
 * because that token together with `allow-scripts` defeats the sandbox in any
 * state. Trust therefore relaxes the CSP (see {@link buildAppCSP}) and the
 * interaction tokens, never the same-origin isolation boundary.
 */
export function appIframeSandbox(rawHtmlTrusted: boolean): string {
  const tokens = rawHtmlTrusted
    ? `${APP_IFRAME_SANDBOX_BASE} allow-forms allow-popups`
    : APP_IFRAME_SANDBOX_BASE;
  assertSandboxTokensSafe(tokens);
  return tokens;
}

/**
 * Hard invariant guard: throws if a sandbox token string carries
 * `allow-same-origin` at all (which, combined with the mandatory
 * `allow-scripts`, is a sandbox escape). Called by {@link appIframeSandbox} so
 * the forbidden token can never reach the DOM; also exported so tests assert
 * the invariant directly.
 */
export function assertSandboxTokensSafe(tokens: string): void {
  const set = new Set(tokens.split(/\s+/).filter(Boolean));
  if (set.has('allow-same-origin')) {
    throw new Error(
      'MCP Apps sandbox: allow-same-origin is forbidden in all states — it ' +
        'would let framed content escape the sandbox (D-173).',
    );
  }
}

/**
 * Builds the strict Content-Security-Policy applied to the `ui://` document.
 *
 * `connect-src 'none'` is set in BOTH trust states — the framed app can never
 * open its own `fetch` / WebSocket / EventSource, so the ONLY path off the
 * iframe is the host `postMessage` bridge → the injected Protocol client. That
 * is the CSP-layer enforcement of D-173's "no direct MCP transport." A
 * raw-HTML-trusted server may load remote images / styles / fonts / media
 * (richer rendering), but still cannot open a transport.
 */
export function buildAppCSP(rawHtmlTrusted: boolean): string {
  const media = rawHtmlTrusted ? 'data: blob: https:' : 'data: blob:';
  const style = rawHtmlTrusted ? "'unsafe-inline' https:" : "'unsafe-inline'";
  const font = rawHtmlTrusted ? 'data: https:' : 'data:';
  return [
    "default-src 'none'",
    "script-src 'unsafe-inline'",
    `style-src ${style}`,
    `img-src ${media}`,
    `font-src ${font}`,
    `media-src ${media}`,
    // The whole point: the app cannot reach the network on its own.
    "connect-src 'none'",
    "form-action 'none'",
    "base-uri 'none'",
    "frame-ancestors 'none'",
  ].join('; ');
}

/**
 * Wraps a `ui://` HTML document so it carries the strict CSP as a
 * `<meta http-equiv>` tag — the document is loaded into the sandboxed iframe
 * via `srcdoc` (an opaque, non-same-origin context), never same-origin. The
 * CSP meta is injected at the very top of `<head>` (or synthesised when the
 * document has no `<head>`), so it governs every subsequent resource the
 * document declares.
 */
export function wrapAppDocument(html: string, csp: string): string {
  const meta = `<meta http-equiv="Content-Security-Policy" content="${csp}">`;
  if (/<head[^>]*>/i.test(html)) {
    return html.replace(/<head[^>]*>/i, (head) => `${head}${meta}`);
  }
  if (/<html[^>]*>/i.test(html)) {
    return html.replace(/<html[^>]*>/i, (tag) => `${tag}<head>${meta}</head>`);
  }
  return `<!doctype html><html><head>${meta}</head><body>${html}</body></html>`;
}

/* ------------------------------------------------------------------ */
/* postMessage origin / source validation                              */
/* ------------------------------------------------------------------ */

/** The expected peer for an inbound app message. */
export interface ExpectedAppPeer {
  /** The iframe's `contentWindow` — the only window the host trusts. */
  source: MessageEventSource | null;
  /**
   * The expected origin. A sandboxed iframe (no `allow-same-origin`) posts
   * from the opaque origin `"null"`; pass `"null"` for that case, or a real
   * origin when one is expected.
   */
  origin: string;
}

/**
 * Decides whether an inbound `postMessage` event is a trusted message from
 * the expected app iframe. BOTH the source window reference AND the origin
 * must match — a foreign-origin or foreign-window message is rejected, never
 * executed (D-173: "a missing origin check is a cross-frame injection
 * vector"). The window-reference check is the primary guard for a sandboxed
 * iframe (whose origin is the opaque `"null"`), the origin check the
 * defence-in-depth second factor.
 */
export function isTrustedAppMessage(
  event: Pick<MessageEvent, 'source' | 'origin'>,
  expected: ExpectedAppPeer,
): boolean {
  if (expected.source === null) {
    return false;
  }
  if (event.source !== expected.source) {
    return false;
  }
  return event.origin === expected.origin;
}
