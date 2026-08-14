<script lang="ts">
  // The MCP Apps inline renderer — a sandboxed iframe hosting a `ui://`
  // MCP App, bridged to the Harbor Runtime through the manual-handler
  // AppBridge (D-173). Lives in the shared chat module ($lib/chat/) and
  // imports NOTHING from outside it; the Harbor Protocol surface is the
  // INJECTED `appHostClient` (D-091, CLAUDE.md §4.5 #11).
  //
  // Lifecycle:
  //   1. Resolve the captured tool context (→ mcp.apps.tool_context) BEFORE any
  //      admission-requesting read, so the miss path is decided while there is
  //      still nothing minted: an unresolvable context (evicted / unknown /
  //      another identity — the shape a REPLAYED app hits) renders the honest
  //      "no longer available" placeholder, mints NO render admission, and
  //      mounts no iframe at all.
  //   2. Only a successful replay proceeds to the preload of the app's `ui://`
  //      HTML via the injected client's RENDERER-INTERNAL opt-in read (→
  //      mcp.servers.read_resource with `request_render_admission: true` —
  //      readRenderDocument), minting the bounded render admission for THIS
  //      successful read (HA-56). The admission is host-private: it lives only
  //      on this renderer instance and the bridge's closure, is never
  //      serialized into DOM / storage / logs / replay / history / control
  //      surfaces, and is echoed back on mcp.apps.call_tool (never the legacy
  //      binding when it is in play). A document at/above the heavy-content
  //      threshold (D-026) is offloaded to the ArtifactStore by reference; the
  //      host resolves that stub to a presigned URL and fetches the bytes (→
  //      resolveArtifact → artifacts.get_ref). EITHER content source feeds the
  //      SAME sandboxed `srcdoc` — heavy bytes are NEVER inlined through the
  //      context plane, only fetched at the iframe edge.
  //
  //      On a deployment where the operator left the opt-in surface OFF
  //      (`tools.mcp_app_render_admission.enabled: false` — the DEFAULT), the
  //      opt-in read fails with the Runtime's PRECISE unwired posture
  //      (`runtime_error` naming "render-admission authority is not wired"),
  //      NOT a typed refusal. The renderer restores the pre-admission
  //      behavior then: a LIVE ref still carrying the legacy `binding` falls
  //      back EXACTLY ONCE to the ordinary NON-minting `readResource` and
  //      dispatches callbacks on the binding ALONE (never both authorities);
  //      a durable reopen with no binding shows the explicit admission safe
  //      state — the unwired surface can never mint the fresh admission a
  //      reopened app requires, so its callbacks could never be authorized.
  //   3. Wrap the HTML with the strict CSP and load it into the iframe via
  //      `srcdoc` under a sandbox with NO `allow-same-origin` — the iframe
  //      is forced to an opaque origin (no parent-DOM / cookie / localStorage
  //      access).
  //   4. Construct the AppBridge host (manual-handler mode) and connect it to
  //      the iframe's `contentWindow`, completing the `ui/initialize`
  //      handshake. Every app→host request routes through `appHostClient`.
  import { untrack } from 'svelte';
  import type { McpUiTheme } from '@modelcontextprotocol/ext-apps/app-bridge';
  import type { RendererProps } from './index.js';
  import {
    AppBridgeHost,
    containerDimensionsFromBox,
    DEFAULT_HOST_INFO,
    type MCPAppRenderAdmission,
    type MCPAppRenderDocument,
    type MCPAppToolContext,
    type McpUiDisplayMode
  } from './app-bridge-host.js';
  import { appIframeSandbox, buildAppCSP, wrapAppDocument } from './sandbox-policy.js';
  import { buildHostStyles, hostThemeMediaQuery, resolveHostTheme } from './theme-tokens.js';

  let {
    app,
    serverID,
    appHostClient,
    availableDisplayModes,
    onDisplayModeRequest
  }: RendererProps = $props();

  // `unavailable` is the MISS state: the app declared a captured tool context
  // (it carries a `toolCallId`) but the host could not resolve it — evicted,
  // unknown, or belonging to another identity. It is reachable on a LIVE turn
  // and, far more often, on a REPLAYED one (a reopened transcript re-mounts an
  // app whose backing record may be long gone). It renders a stable, honest
  // placeholder INSTEAD of the iframe: never a blank bubble, never a
  // half-mounted app whose data silently never arrives (CLAUDE.md §13).
  // `closed` is the APP-REQUESTED teardown state: the app sent
  // `ui/notifications/request-teardown`, the host granted it (sent the graceful
  // `ui/resource-teardown` and closed the bridge), and the iframe is unmounted.
  // It renders a short placeholder rather than leaving a dead frame in the
  // transcript, and it is STICKY — a transcript re-render must not resurrect an
  // app that asked to be gone.
  // `admission` is the RENDER-ADMISSION safe state (HA-56): the app itself is
  // CURRENT (its document read and its captured tool context resolved fine —
  // this is deliberately NOT the `unavailable` miss) but the Runtime could not
  // mint / re-mint the render admission that authorizes its callbacks, after
  // the single bounded refresh. Mounting an iframe whose every callback would
  // be refused is a silent lie about what the turn produced; the explicit safe
  // state says so instead. It is ALSO the landing state for a durable reopen
  // on a disabled surface (`tools.mcp_app_render_admission.enabled: false` —
  // the default): the ref carries no legacy binding (the durable view never
  // serializes render authority) and the unwired Runtime can never mint the
  // fresh admission such a reopen requires, so no callback authority exists
  // and none can be obtained — the frame must not mount.
  type LoadState = 'loading' | 'ready' | 'error' | 'empty' | 'unavailable' | 'closed' | 'admission';

  let loadState = $state<LoadState>('loading');
  let errorMessage = $state('');
  // The typed Runtime verdict that landed the app in the `admission` safe
  // state (e.g. `render_admission_unavailable`), shown as debug detail. NEVER
  // a token — the admission itself is host-private and never rendered.
  let admissionFailureCode = $state<string | undefined>(undefined);
  // The tool context resolved BEFORE the iframe mounts, handed to the bridge
  // for after-init delivery. Non-reactive on purpose (the bridge snapshots it
  // untracked), and only ever set on the path that reaches `ready`.
  let resolvedToolContext: MCPAppToolContext | undefined;
  // The FRESH render admission minted by the pre-mount opt-in document read
  // (HA-56). Non-reactive on purpose: it is HOST-PRIVATE, kept only on this
  // live renderer instance and handed to the bridge (which holds its own
  // closure copy); it is NEVER rendered to the DOM, written to storage,
  // logged, or carried on any replay/history/control surface. Only ever set
  // on the path that reaches `ready`, and cleared on every new preload.
  let renderAdmission: MCPAppRenderAdmission | undefined;
  // The bounded re-read budget for a TYPED `unavailable` admission at read
  // time: at most ONE fresh opt-in read (the Runtime's documented "the caller
  // must re-read"), then the explicit safe state. Reset per preload attempt.
  let admissionRefreshBudget = 1;
  let srcdoc = $state('');
  let iframeEl = $state<HTMLIFrameElement | null>(null);
  let host: AppBridgeHost | undefined;
  // The dedup key of the app currently loaded — `resourceUri` PLUS
  // `toolCallId`. The preload effect re-runs whenever the `app` prop's IDENTITY
  // changes — which the transcript does on every re-render of a turn (a failing
  // turn thrashes the message list). Re-fetching the same document each time
  // churns `loadState`, which tears the bridge down and rebuilds it (the
  // lifecycle effect keys on `loadState`), producing a `read_resource` +
  // handshake storm. Dedup: an identity change that resolves to the SAME app is
  // a no-op; a genuinely different one still reloads.
  //
  // The key includes the tool-call id, not just the URI: two tool calls in one
  // turn can declare the SAME `ui://` document with DIFFERENT contexts, and the
  // discovery fold is last-wins. Keying on the URI alone would short-circuit
  // the second one and leave the bridge delivering the FIRST call's data under
  // the second call's app — a silent data mismatch rather than a visible one.
  //
  // Non-reactive on purpose — it must not itself re-trigger the effect.
  let loadedKey: string | undefined;

  /** The dedup identity of an app ref: document, tool context, and agent authority. */
  function appKey(ref: { resourceUri: string; toolCallId?: string; agentId?: string }): string {
    // JSON array encoding stays collision-free even if a configured agent id
    // contains delimiter-like characters. A raw NUL separator would also make
    // this security-relevant source appear binary to git and text-mode guards.
    return JSON.stringify([ref.resourceUri, ref.toolCallId ?? '', ref.agentId ?? '']);
  }

  // Monotonic preload token. `preload` awaits twice (the document, then the
  // tool context), and the `app` prop's identity can change mid-flight, so two
  // preloads can be in flight at once. Only the LATEST may write `loadState`:
  // an older one resolving afterwards would overwrite the newer outcome, and
  // because `loadState` IS a tracked dependency of the bridge lifecycle effect,
  // a stale write of `unavailable` / `error` would fire that effect's cleanup
  // and `host.close()` a bridge mid-`ui/initialize` — the exact teardown shape
  // that got the original MCP-Apps work reverted, reached through a data
  // outcome instead of a theme change. Non-reactive on purpose.
  let preloadToken = 0;

  // Live theme state, isolated from the bridge lifecycle.
  //
  // `hostTheme` tracks the OS `prefers-color-scheme`; a matchMedia `change`
  // event updates it. The bridge is NOT rebuilt when it changes — a SEPARATE
  // effect relays the change onto the LIVE bridge via `setHostContext`. This
  // isolation is the fix for the reverted-work handshake break: the bridge's
  // lifecycle effect must never re-run (and tear the transport down) because
  // the theme changed.
  let hostTheme = $state<McpUiTheme>(resolveHostTheme());
  // Flipped true once the bridge completes `ui/initialize`; GATES the theme
  // relay so it never patches a bridge mid-handshake.
  let bridgeReady = $state(false);
  // The theme most recently baked into / relayed onto the bridge — so the relay
  // skips the redundant echo of the theme the bridge was constructed with, and
  // still fires when the OS toggles back to a previously-applied theme.
  let lastAppliedTheme: McpUiTheme | undefined;

  // The app's self-reported content height, in CSS pixels — `null` until the app
  // reports one. The iframe's inline `height` is driven from it; the CLAMP is
  // pure CSS (`min-height` / `max-height` tokens on `.mcp-app__frame`), so a
  // report of 5 px or 500 000 px lands inside the host's own bounds and a
  // misbehaving app can never seize the viewport. An app that never reports a
  // size leaves this `null`, no inline height is set, and the frame keeps
  // exactly today's fixed `min-height` behaviour.
  //
  // Reactive state the TEMPLATE reads — deliberately NOT read by
  // `connectBridge`, so it is not and cannot become a tracked dependency of the
  // bridge-owning lifecycle effect. A size value that re-ran that effect would
  // tear the transport down on every resize: the reverted-work failure class
  // reached through a new door.
  let appHeightPx = $state<number | null>(null);
  // rAF coalescing: an app that emits a resize storm (a chart animating, a table
  // laying out) must not thrash layout. Only the LAST report in a frame wins.
  // Non-reactive on purpose.
  let pendingHeight: number | null = null;
  let sizeFrame: number | null = null;

  /** Cancel any queued size application (unmount / teardown). */
  function cancelPendingSize(): void {
    if (sizeFrame !== null && typeof cancelAnimationFrame === 'function') {
      cancelAnimationFrame(sizeFrame);
    }
    sizeFrame = null;
    pendingHeight = null;
  }

  /**
   * Consume one `ui/notifications/size-changed` report. Coalesced through
   * `requestAnimationFrame` (falling back to a direct apply where rAF is
   * unavailable, e.g. jsdom). A non-finite or non-positive height is ignored —
   * an app reporting nonsense keeps the previous, valid height.
   */
  function onAppSizeChanged(size: { width?: number; height?: number }): void {
    const h = size.height;
    if (typeof h !== 'number' || !Number.isFinite(h) || h <= 0) return;
    pendingHeight = h;
    if (typeof requestAnimationFrame !== 'function') {
      appHeightPx = pendingHeight;
      pendingHeight = null;
      return;
    }
    if (sizeFrame !== null) return;
    sizeFrame = requestAnimationFrame(() => {
      sizeFrame = null;
      if (pendingHeight !== null) appHeightPx = pendingHeight;
      pendingHeight = null;
    });
  }

  /**
   * Measure the box the app is being handed, for the `ui/initialize`
   * host-context `containerDimensions`. Width comes from the mounted frame's
   * layout box; the height BOUND comes from the frame's resolved `max-height`
   * (the host-owned `--size-app-inline-max` token) — i.e. the host tells the app
   * how tall it is allowed to grow, which is exactly what the app needs to lay
   * itself out without over-reporting.
   *
   * Called only from inside `connectBridge`'s `untrack` block. Pure DOM reads:
   * nothing here is reactive.
   */
  function measureContainer(
    el: HTMLIFrameElement | null
  ): ReturnType<typeof containerDimensionsFromBox> {
    if (!el || typeof el.getBoundingClientRect !== 'function') return undefined;
    const rect = el.getBoundingClientRect();
    let maxHeight: number | undefined;
    if (typeof window !== 'undefined' && typeof window.getComputedStyle === 'function') {
      const px = Number.parseFloat(window.getComputedStyle(el).maxHeight);
      if (Number.isFinite(px) && px > 0) maxHeight = px;
    }
    return containerDimensionsFromBox({ width: rect.width, maxHeight });
  }

  // The sandbox token set + CSP are derived from the per-server raw-HTML
  // trust posture. `appIframeSandbox` GUARANTEES `allow-same-origin` is never
  // present (it throws otherwise), so the iframe can never escape the sandbox.
  const trusted = $derived(app?.rawHtmlTrusted ?? false);
  const sandbox = $derived(appIframeSandbox(trusted));

  // The operator "pop to side-by-side" affordance (host-side, app-independent).
  // The app can ask to grow via the AppBridge `ui/request-display-mode` handshake;
  // this gives the OPERATOR the same reach without the app asking, dispatched
  // through the SAME injected `onDisplayModeRequest` seam (never by reaching into
  // the page — chat-module encapsulation, D-091). The affordance shows ONLY while
  // the app is inline (the page-level fullscreen/pip panels carry their own mode
  // bar) and ONLY for modes the host advertises it can apply (`availableDisplayModes`).
  const isInline = $derived((app?.displayMode ?? 'inline') === 'inline' || app?.displayMode === '');
  const canPip = $derived((availableDisplayModes ?? []).includes('pip'));
  const canFullscreen = $derived((availableDisplayModes ?? []).includes('fullscreen'));
  const showExpand = $derived(
    isInline && onDisplayModeRequest != null && (canPip || canFullscreen)
  );

  // Pop the inline app to a host-applied display mode. The host knows it can
  // apply `mode` (it is in `availableDisplayModes`), so it grants it directly —
  // the same `{requested, granted}` shape the AppBridge handler produces — and
  // the page's layout reducer moves the app to that region (D-062, D-214).
  function popTo(mode: McpUiDisplayMode): void {
    onDisplayModeRequest?.({ requested: mode, granted: mode });
  }

  /**
   * True when the opt-in document read failed with the Runtime's PRECISE
   * unwired posture: `runtime_error` naming that the render-admission
   * authority is not wired — the compatible surface of
   * `tools.mcp_app_render_admission.enabled: false` (the default), where the
   * opt-in read fails through the unwired seam while ordinary reads and the
   * legacy binding stay byte-for-byte unchanged.
   *
   * Deliberately duck-typed: the chat module may not import the Protocol
   * error class (D-091), and the wire gives only `{code, message}`. The
   * message is the discriminator — the SAME `runtime_error` code also covers
   * an enabled surface whose authorization seam failed, and that posture must
   * NOT fall back (it stays fail-closed in the loud error state). Matching
   * the stable "authority is not wired" wording is the ONLY precise marker of
   * the disabled surface; every other denial (a typed current-conditions
   * refusal — which arrives as a RESPONSE, not a throw — tamper, expiry,
   * foreign identity, resource mismatch, a disabled tool/source) is a
   * different shape and fails closed.
   */
  function isUnwiredRenderAdmissionRuntimeError(err: unknown): boolean {
    if (err === null || typeof err !== 'object') return false;
    const e = err as { code?: unknown; message?: unknown };
    return (
      e.code === 'runtime_error' &&
      typeof e.message === 'string' &&
      e.message.includes('render-admission authority is not wired on this runtime')
    );
  }

  async function preload(): Promise<void> {
    if (!app || !serverID || !appHostClient) {
      loadState = 'empty';
      return;
    }
    // Same app already loaded and live — a prop-identity re-run, not a new app.
    // Skip the refetch so `loadState` stays `ready` and the bridge is not torn
    // down + rebuilt (guards the re-render storm; see `loadedKey`).
    //
    // `loadState` is read UNTRACKED and deliberately so: this runs inside the
    // preload `$effect`, so a tracked read would make the effect depend on
    // `loadState` — and the effect WRITES `loadState`, so every write would
    // re-run the preload, re-fetching the document and the tool context in a
    // loop. (The pre-existing form relied on `&&` short-circuit to avoid the
    // read; that made the operand ORDER load-bearing, a trap for the next
    // editor. `untrack` removes the hazard instead of documenting it.)
    // `closed` joins `ready` here so an APP-REQUESTED teardown is sticky: a
    // transcript re-render must not resurrect an app that asked to be gone
    // (and immediately have it ask again — a teardown loop).
    const key = appKey(app);
    if (untrack(() => loadState === 'ready' || loadState === 'closed') && key === loadedKey) {
      return;
    }
    // Claim this preload. Every post-await write below is gated on still being
    // the latest: a prop-identity change mid-flight starts a second preload,
    // and a stale one resolving afterwards must not overwrite the newer
    // outcome — least of all with a terminal state that would close a bridge
    // built by the newer one.
    const token = ++preloadToken;
    const stale = (): boolean => token !== preloadToken;

    loadState = 'loading';
    errorMessage = '';
    admissionFailureCode = undefined;
    // The fresh admission belongs to the preload about to run; a re-preload
    // (new app) must never hand a STALE token to a new bridge. Cleared here
    // and only set again on the path that reaches `ready`.
    renderAdmission = undefined;
    admissionRefreshBudget = 1;
    try {
      // 1. Resolve the captured tool context BEFORE any admission-requesting
      //    read. Doing it here (rather than letting the bridge fetch it after
      //    the handshake) is what makes the miss path honest: a context that
      //    cannot be resolved yields a placeholder instead of a mounted app
      //    whose data never arrives. A `null` is the Runtime's `not_found` —
      //    unknown / evicted / cross-identity — and is the common REPLAY
      //    shape; any other failure throws and lands in the loud error state
      //    below. The miss is decided FIRST (HA-56): a replayed app whose
      //    backing record is gone returns the `unavailable` state here,
      //    before the opt-in read runs, so no render admission is ever minted
      //    for an app whose data cannot be delivered.
      if (app.toolCallId !== undefined && app.toolCallId !== '') {
        const ctx = await appHostClient.toolContext(serverID, app.toolCallId);
        if (stale()) return;
        if (ctx === null) {
          resolvedToolContext = undefined;
          loadedKey = undefined;
          loadState = 'unavailable';
          return;
        }
        resolvedToolContext = ctx;
      } else {
        // The discovery recorded no correlation id, so no context was ever
        // captured for this app — nothing has gone missing. The app mounts and
        // boots without delivered data, exactly as it always has.
        resolvedToolContext = undefined;
      }

      // 2. ONLY a successful replay proceeds to the pre-mount document read —
      //    the OPT-IN admission-requesting read (HA-56): `readRenderDocument`
      //    routes to `mcp.servers.read_resource` with
      //    `request_render_admission: true`, minting the bounded render
      //    admission for THIS successful read. The AppBridge's app-initiated
      //    `resources/read` handler stays on the ordinary NON-minting
      //    `readResource`, so an App can never mint callback authority merely
      //    by requesting another resource.
      //
      //    The opt-in read's PRECISE unwired posture — the Runtime answering
      //    `runtime_error` with "render-admission authority is not wired on
      //    this runtime" — is the compatible surface of
      //    `tools.mcp_app_render_admission.enabled: false` (the DEFAULT):
      //    the Runtime wires it so the opt-in read fails through the unwired
      //    seam while ordinary reads and the legacy binding stay unchanged.
      //    On a LIVE turn whose ref still carries the legacy `binding`,
      //    restore that rendering: retry EXACTLY ONCE with the ordinary
      //    NON-minting `readResource` and dispatch callbacks on the binding
      //    ALONE (never both authorities). A ref WITHOUT a binding (durable
      //    reopen — the durable view never serializes render authority)
      //    cannot authorize any callback and none can be minted while the
      //    surface is unwired; show the explicit admission safe state instead
      //    of a generic failure. Every other error — a typed
      //    current-conditions refusal (handled below), tamper, expiry,
      //    foreign identity, resource mismatch, a disabled tool/source, an
      //    authorization-seam failure — stays fail-closed: it throws and
      //    lands in the loud error state, never a silent downgrade.
      let resource: MCPAppRenderDocument;
      try {
        resource = await appHostClient.readRenderDocument(serverID, app.resourceUri, app.agentId);
      } catch (err) {
        if (stale()) return;
        if (!isUnwiredRenderAdmissionRuntimeError(err)) {
          // NOT the disabled-surface posture: fail closed, loud.
          throw err;
        }
        if (!app.binding) {
          // Durable reopen with no legacy binding: no authority exists to
          // dispatch callbacks with (and the unwired surface can mint none),
          // so mounting the frame would be the silent lie the admission safe
          // state exists to prevent. Show it explicitly — never the generic
          // App load failure, never the tool-context miss.
          loadState = 'admission';
          return;
        }
        // The legacy live binding authorizes this render's callbacks: one
        // ordinary NON-minting read for the document; the bridge dispatches
        // on the binding alone (renderAdmission stays undefined).
        resource = await appHostClient.readResource(serverID, app.resourceUri, app.agentId);
      }
      if (stale()) return;
      // A TYPED `unavailable` admission is the Runtime's closed "no admission
      // minted" answer (empty/unknown current provider/catalog generation, or
      // a current-conditions refusal) — it says the caller must re-read. The
      // app itself is CURRENT: this is deliberately NOT the `unavailable`
      // tool-context miss, and it must never be silently reduced to one. At
      // most ONE bounded fresh opt-in re-read happens here; a second typed
      // refusal is the explicit `admission` safe state, not a retry loop.
      if (resource.renderAdmission && resource.renderAdmission.availability !== 'available') {
        if (admissionRefreshBudget > 0) {
          admissionRefreshBudget -= 1;
          const fresh = await appHostClient.readRenderDocument(serverID, app.resourceUri, app.agentId);
          if (stale()) return;
          resource = fresh;
        }
      }
      if (resource.renderAdmission && resource.renderAdmission.availability !== 'available') {
        admissionFailureCode = resource.renderAdmission.availability;
        loadState = 'admission';
        return;
      }
      // An AVAILABLE admission carries the opaque token; an ABSENT
      // renderAdmission means the runtime answered the old (non-admission)
      // surface — the preserved legacy path (the bridge then sends the
      // legacy `binding`, never both authorities).
      const admission = resource.renderAdmission;
      renderAdmission =
        admission && admission.availability === 'available' && admission.token !== ''
          ? admission
          : undefined;
      let documentHTML: string;
      if (resource.artifactRef) {
        // The `ui://` document is at/above the heavy-content threshold (D-026),
        // so the Runtime offloaded it to the ArtifactStore by reference rather
        // than inlining it. This is the COMMON case, not a server bug — real
        // Svelte/React App bundles routinely exceed the 32 KiB threshold.
        // Resolve the by-reference stub to a presigned URL and fetch the bytes
        // at the iframe edge, exactly as the inline path uses `content`; the
        // offload (never inlining heavy bytes through the context plane) is
        // correct and preserved — only the document SOURCE differs here.
        const url = await appHostClient.resolveArtifact(resource.artifactRef.id);
        const resp = await fetch(url);
        if (!resp.ok) {
          throw new Error(
            `failed to fetch app document artifact ${resource.artifactRef.id}: HTTP ${resp.status}`,
          );
        }
        documentHTML = await resp.text();
        if (stale()) return;
      } else {
        documentHTML = resource.content ?? '';
      }
      if (documentHTML === '') {
        loadState = 'empty';
        return;
      }

      srcdoc = wrapAppDocument(documentHTML, buildAppCSP(trusted));
      loadedKey = key;
      loadState = 'ready';
    } catch (err) {
      if (stale()) return;
      errorMessage = err instanceof Error ? err.message : String(err);
      loadState = 'error';
    }
  }

  // Once the iframe is mounted with the app document, connect the bridge to
  // its contentWindow. The PostMessageTransport only accepts messages from
  // that exact window (origin / source validation), so a foreign frame's
  // messages are rejected.
  async function connectBridge(): Promise<void> {
    if (host) return;
    // Snapshot EVERY reactive input to the bridge — the iframe window, the five
    // renderer props, and the resolved theme/styles — in a single `untrack`, so
    // NONE of them is tracked by the caller (the lifecycle `$effect` below).
    // That effect therefore depends on ONLY `loadState` + `iframeEl` (the two
    // signals it reads directly): a change to any prop's identity, or a theme
    // change, can never re-run it and tear the transport down mid-`ui/initialize`
    // — the exact outage that got the original work reverted. `iframeEl` stays a
    // tracked dep because the effect body reads it directly; reading it here
    // untracked only avoids a redundant second subscription. Theme/styles are
    // plain DOM reads, snapshotted here for the same isolation guarantee.
    const s = untrack(() => ({
      win: iframeEl?.contentWindow ?? null,
      app,
      serverID,
      client: appHostClient,
      availableDisplayModes,
      onDisplayModeRequest,
      theme: resolveHostTheme(),
      styles: buildHostStyles(),
      // The container box the app is being handed, measured ONCE here. A plain
      // DOM read inside the same `untrack` as everything else — it is not a
      // signal, so it can never become a tracked dependency of the lifecycle
      // effect (a size-shaped dependency on the bridge-owning effect is the
      // exact hazard D-342 closes).
      container: measureContainer(iframeEl),
      toolContext: resolvedToolContext,
      // The FRESH render admission minted by the pre-mount opt-in read —
      // host-private, never serialized; the bridge holds its own closure copy
      // and echoes it on `mcp.apps.call_tool` (never the legacy binding when
      // it is in play).
      admission: renderAdmission
    }));
    if (!s.win || !s.app || !s.serverID || !s.client) return;
    lastAppliedTheme = s.theme;
    // Every bridge callback below writes COMPONENT state, and a callback can
    // fire long after the bridge that owns it stopped being the current one:
    // `close()` awaits a teardown round-trip the app itself acks (up to the
    // teardown timeout), and in that window the effect cleanup has already run,
    // the `app` prop can churn, and a NEW bridge can mount. A stale
    // `onRequestTeardown` would then set the sticky `closed` state over a
    // perfectly live successor and the app would never render again; a stale
    // `onSizeChanged` would drive the successor's frame from the dead app's
    // reported height.
    //
    // Phase 204's generation token guards PRELOAD writes only — it is keyed to
    // the preload, not the bridge — so it does not cover this. The guard here
    // is instance identity: each callback captures the bridge it was created
    // for and no-ops unless that instance is still the current `host`.
    // `created` is assigned before `connect()`, and no callback can fire before
    // the transport exists, so the comparison is never racing an unset value.
    const created: AppBridgeHost = new AppBridgeHost({
      client: s.client,
      serverID: s.serverID,
      agentID: s.app.agentId,
      // The preserved LEGACY live-result path — distinct from the fresh
      // render admission below. The bridge sends EXACTLY ONE authority on
      // `mcp.apps.call_tool`: the fresh admission when minted, else this
      // legacy binding (never both — the Runtime refuses
      // `render_authority_ambiguous`).
      binding: s.app.binding,
      resourceURI: s.app.resourceUri,
      // The FRESH render admission from the pre-mount opt-in read. Host-
      // private: it lives only on this renderer instance + the bridge's
      // closure, and is never aliased into `binding`, never serialized into
      // DOM / storage / logs / replay / history / control surfaces.
      renderAdmission: s.admission,
      availableDisplayModes: s.availableDisplayModes,
      onDisplayModeRequest: s.onDisplayModeRequest,
      // Host identity is injected through the seam (not baked into the module).
      hostInfo: DEFAULT_HOST_INFO,
      theme: s.theme,
      styles: s.styles,
      // The originating tool call, named for the app. Host-derived, from the
      // `mcp.app_available` discovery — never anything the app supplied.
      toolInfo: s.app.toolName ? { toolCallId: s.app.toolCallId, toolName: s.app.toolName } : undefined,
      containerDimensions: s.container,
      // The already-resolved context for the after-init Data Delivery push.
      toolContext: s.toolContext,
      // A typed render-admission refusal that the single bounded refresh did
      // not recover: the app's callback authority is genuinely gone. Replace
      // the mounted app with the explicit safe state — never silently keep a
      // frame whose every callback would be refused.
      onAdmissionInvalid: (code) => {
        if (host !== created) return;
        cancelPendingSize();
        appHeightPx = null;
        admissionFailureCode = code;
        loadState = 'admission';
      },
      // The renderer flips its live-theme gate here, AFTER the handshake.
      onInitialized: () => {
        if (host !== created) return;
        bridgeReady = true;
      },
      // The app reports its content height; the frame grows into it (clamped).
      onSizeChanged: (size) => {
        if (host !== created) return;
        onAppSizeChanged(size);
      },
      // The app asked to be torn down and the host granted it. The bridge is
      // already closed by the time this fires; unmount the frame and say so.
      onRequestTeardown: () => {
        if (host !== created) return;
        cancelPendingSize();
        appHeightPx = null;
        loadState = 'closed';
      }
    });
    host = created;
    try {
      await host.connect(s.win);
    } catch (err) {
      errorMessage = err instanceof Error ? err.message : String(err);
      loadState = 'error';
    }
  }

  $effect(() => {
    void preload();
  });

  // The bridge lifecycle effect — depends ONLY on `loadState` + `iframeEl` (the
  // two signals it reads directly). Every other input the bridge needs (theme,
  // styles, and all five renderer props) is snapshotted UNTRACKED inside
  // `connectBridge`, so neither a theme change nor prop-identity churn can
  // re-run this effect and tear the transport down mid-handshake. That teardown
  // was the reverted-work outage (#346); this isolation is the fix (D-342).
  $effect(() => {
    if (loadState === 'ready' && iframeEl) {
      void connectBridge();
    }
    return () => {
      if (host) {
        // `close()` sends the graceful `ui/resource-teardown` before dropping
        // the transport — but ONLY when the app finished `ui/initialize`, so a
        // bridge closed mid-handshake still closes silently and instantly. It
        // is fire-and-forget from here: an effect cleanup must not await a
        // round-trip into a sandboxed iframe, and `close()` bounds it anyway.
        void host.close();
        host = undefined;
      }
      cancelPendingSize();
      appHeightPx = null;
      bridgeReady = false;
    };
  });

  // Track the OS `prefers-color-scheme` into `hostTheme`. Runs once (reads no
  // reactive state); the listener is torn down on unmount. Separate from the
  // bridge lifecycle effect on purpose.
  $effect(() => {
    const mql = hostThemeMediaQuery();
    if (!mql) return;
    const onChange = (e: MediaQueryListEvent): void => {
      hostTheme = e.matches ? 'dark' : 'light';
    };
    mql.addEventListener('change', onChange);
    return () => mql.removeEventListener('change', onChange);
  });

  // Relay a LIVE theme change onto the running bridge — NEVER a teardown. Fires
  // only after the bridge is initialized and only when the theme actually
  // changed from the last-applied value; `AppBridgeHost.setHostContext` gates
  // on init a second time.
  $effect(() => {
    const theme = hostTheme;
    if (!bridgeReady || !host) return;
    if (theme === lastAppliedTheme) return;
    lastAppliedTheme = theme;
    host.setHostContext({ theme, styles: buildHostStyles() });
  });
</script>

<div class="mcp-app" data-renderer-source="mcp-app" data-display-mode={app?.displayMode || 'inline'}>
  {#if loadState === 'loading'}
    <div class="mcp-app__state" data-state="loading" role="status">
      <span class="mcp-app__spinner" aria-hidden="true"></span>
      <span>Loading app…</span>
    </div>
  {:else if loadState === 'error'}
    <div class="mcp-app__state" data-state="error" role="alert">
      <span class="mcp-app__state-title">App failed to load</span>
      <span class="mcp-app__state-detail">{errorMessage}</span>
    </div>
  {:else if loadState === 'unavailable'}
    <!-- The MISS placeholder. The app's captured tool context could not be
         resolved (evicted / unknown / another identity), so the iframe is
         never mounted — a dataless app would be a silent lie about what the
         turn produced. Names WHAT is missing and WHY the rest of the turn is
         still trustworthy. -->
    <div class="mcp-app__state" data-state="unavailable" data-testid="mcp-app-unavailable" role="status">
      <span class="mcp-app__state-title">This view is no longer available</span>
      <span class="mcp-app__state-detail">
        The data behind this app is no longer retained, so it cannot be re-rendered. The
        rest of the conversation is unchanged.
      </span>
    </div>
  {:else if loadState === 'closed'}
    <!-- The app asked to be torn down (`ui/notifications/request-teardown`) and
         the host granted it: the graceful `ui/resource-teardown` was sent, the
         bridge closed, and the frame unmounted. Say so plainly rather than
         leaving a dead iframe or silently blanking the bubble. -->
    <div class="mcp-app__state" data-state="closed" data-testid="mcp-app-closed" role="status">
      <span class="mcp-app__state-title">This app closed itself</span>
      <span class="mcp-app__state-detail">
        The app asked to be shut down and the host released it. The rest of the conversation
        is unchanged.
      </span>
    </div>
  {:else if loadState === 'admission'}
    <!-- The render-admission safe state (HA-56). The app itself is CURRENT —
         its document read and captured tool context resolved fine; this is NOT
         the tool-context miss. But the Runtime could not mint / re-mint the
         render admission that authorizes its callbacks (after the single
         bounded refresh), so mounting the iframe would produce an app whose
         every callback is refused. Explicit and stable; never a retry loop,
         and never silently downgraded to the "no longer available" miss. The
         detail carries the typed verdict code only — never a token. -->
    <div class="mcp-app__state" data-state="admission" data-testid="mcp-app-admission" role="status">
      <span class="mcp-app__state-title">This app's interactive session could not be authorized</span>
      <span class="mcp-app__state-detail">
        The Runtime could not issue the render admission this app needs to run, so its
        controls are not available. The rest of the conversation is unchanged.
        {#if admissionFailureCode}
          <span class="mcp-app__state-code">({admissionFailureCode})</span>
        {/if}
      </span>
    </div>
  {:else if loadState === 'empty'}
    <div class="mcp-app__state" data-state="empty">
      <span>No app content.</span>
    </div>
  {:else}
    {#if showExpand}
      <!-- Host-side operator "pop to side-by-side" affordance. Dispatches the
           display-mode request through the injected callback (D-091) — it never
           reaches into the page. The app does not have to ask. -->
      <div class="mcp-app__toolbar" data-testid="mcp-app-expand-bar">
        {#if canPip}
          <button
            type="button"
            class="mcp-app__expand"
            data-testid="mcp-app-expand-pip"
            aria-label="Pop app to side-by-side"
            title="Pop to side-by-side"
            onclick={() => popTo('pip')}
          >
            <span aria-hidden="true">⤢</span>
          </button>
        {/if}
        {#if canFullscreen}
          <button
            type="button"
            class="mcp-app__expand"
            data-testid="mcp-app-expand-fullscreen"
            aria-label="Pop app to fullscreen"
            title="Pop to fullscreen"
            onclick={() => popTo('fullscreen')}
          >
            <span aria-hidden="true">⛶</span>
          </button>
        {/if}
      </div>
    {/if}
    <!-- The frame's height tracks the app's self-reported content height when
         it reports one — INLINE ONLY. The CLAMP is CSS, not JS: the
         `mcp-app__frame--inline` modifier carries
         `min-height: var(--size-app-inline-min)` and
         `max-height: var(--size-app-inline-max)`, so any reported value lands
         inside the host's own bounds — an app cannot seize the viewport, and an
         app that never reports keeps exactly the previous fixed height.

         Both the modifier and the reported-height style are gated on
         `isInline`. In fullscreen / pip the app is NOT in the chat scroll: the
         page-level AppPanel reuses this same `.mcp-app__frame` class and sizes
         it to fill the panel (`flex: 1; height: 100%`), so an inline growth
         bound applied there would CAP the panel — a 900px fullscreen frame
         clamped to the inline maximum. The envelope belongs to the surface that
         owns the layout, and inline is the only surface this component owns. -->
    <iframe
      bind:this={iframeEl}
      class="mcp-app__frame"
      title="MCP App"
      {sandbox}
      {srcdoc}
      allow=""
      referrerpolicy="no-referrer"
      class:mcp-app__frame--inline={isInline}
      data-trusted={trusted}
      data-app-height={isInline ? (appHeightPx ?? undefined) : undefined}
      style:height={isInline && appHeightPx !== null ? `${appHeightPx}px` : undefined}
    ></iframe>
  {/if}
</div>

<style>
  .mcp-app {
    position: relative;
    width: 100%;
    border: var(--border-hairline);
    border-radius: var(--radius-md);
    overflow: hidden;
    background: var(--color-surface-raised);
  }

  .mcp-app__toolbar {
    position: absolute;
    top: var(--space-2);
    right: var(--space-2);
    z-index: 1;
    display: flex;
    gap: var(--space-1);
  }

  .mcp-app__expand {
    display: inline-flex;
    align-items: center;
    justify-content: center;
    width: var(--space-6);
    height: var(--space-6);
    padding: var(--space-0);
    border: var(--border-hairline);
    border-radius: var(--radius-sm);
    background: var(--color-surface-raised);
    color: var(--color-text-muted);
    font-size: var(--text-sm);
    line-height: 1;
    cursor: pointer;
  }

  .mcp-app__expand:hover {
    color: var(--color-text);
    border-color: var(--color-accent);
  }

  .mcp-app__expand:focus-visible {
    outline: var(--border-hairline-width) solid var(--color-accent);
    outline-offset: var(--space-1);
  }

  .mcp-app__frame {
    width: 100%;
    border: none;
    display: block;
  }

  /* The host-owned size envelope for a self-sizing INLINE app. `min-height` is
     the floor an app that never reports a size keeps; `max-height` is the
     ceiling a self-reported height is clamped to, so a misbehaving (or hostile)
     app cannot grow the frame past the host's allowance — it scrolls inside its
     own box instead. Both are tokens; the inline `height` the app drives sits
     between them.

     Scoped to the `--inline` modifier on purpose. The page-level fullscreen /
     pip panel reuses the base `.mcp-app__frame` class and supplies its own
     sizing; putting the inline envelope on the base class capped that panel to
     the inline maximum. An envelope is only meaningful to the surface that owns
     the layout. */
  .mcp-app__frame--inline {
    min-height: var(--size-app-inline-min);
    max-height: var(--size-app-inline-max);
  }

  .mcp-app__state {
    display: flex;
    flex-direction: column;
    gap: var(--space-2);
    align-items: flex-start;
    padding: var(--space-4);
    color: var(--color-text-muted);
    font-size: var(--text-sm);
  }

  .mcp-app__state[data-state='loading'] {
    flex-direction: row;
    align-items: center;
  }

  .mcp-app__state[data-state='error'] {
    color: var(--color-danger);
  }

  /* The miss placeholder is informational, not an error: the turn succeeded,
     only its interactive view can no longer be rebuilt. Muted, not red. */
  .mcp-app__state[data-state='unavailable'] {
    color: var(--color-text-muted);
  }

  /* Same reasoning as `unavailable`: an app that shut itself down cleanly is a
     lifecycle outcome, not a failure. Muted, not red. */
  .mcp-app__state[data-state='closed'] {
    color: var(--color-text-muted);
  }

  /* Same reasoning: a refused render admission is an authorization outcome,
     not a transport failure. Muted, not red. The verdict code is quiet
     debug detail — never a token. */
  .mcp-app__state[data-state='admission'] {
    color: var(--color-text-muted);
  }

  .mcp-app__state-code {
    font-family: var(--font-mono);
    font-size: var(--text-xs);
  }

  .mcp-app__state-title {
    font-weight: 600;
  }

  .mcp-app__state-detail {
    color: var(--color-text-muted);
    font-size: var(--text-xs);
  }

  .mcp-app__spinner {
    width: var(--space-4);
    height: var(--space-4);
    border: var(--border-hairline);
    border-top-color: var(--color-accent);
    border-radius: var(--radius-pill);
    animation: mcp-app-spin var(--motion-slow) linear infinite;
  }

  @keyframes mcp-app-spin {
    to {
      transform: rotate(360deg);
    }
  }
</style>
