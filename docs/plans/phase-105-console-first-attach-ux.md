# Phase 105 — Console first-attach UX (zero-clicks-to-attached)

## Summary

Three bugs make the Console's first-attach experience adoption-blocking:

1. **`Settings → Connected Runtimes` card collects only `name` + `baseURL`.** The connection resolver (`web/console/src/lib/connection.ts::resolveConnection`, lines 66-88) requires FIVE fields (baseURL + token + tenant + user + session). The card only sets ONE. `addRuntime` writes the URL via `attachConnection(baseURL)` without the `opts.token / opts.identity / opts.scopes` arguments — leaving the connection resolver to keep returning `null`. The form looks like it works but it silently no-ops.
2. **First load with no connection lands on an inactionable root.** The Console shell renders the page chrome but the inner page surfaces a `<PageState>` Disconnected branch. The operator's next gesture is undefined — there's no breadcrumb to the surface that fixes it (Settings).
3. **`harbor console` advertises itself as zero-config** (`cmd/harbor/cmd_console.go:11-23`) but the Console JS has no mechanism to discover the co-resident Runtime. Even when `harbor console` boots an embedded Runtime on the same port the SPA is served from, the browser still requires the operator to manually paste a token.

This phase wires the three fixes end-to-end: the Settings card widens to collect all five fields with validation, the root layout redirects to Settings when disconnected, and a new loopback-gated `/v1/dev/bootstrap.json` endpoint mints + serves a fresh dev connection envelope so the Settings page can offer a one-click "Attach to local Runtime" button.

## RFC anchor

- RFC §7 — Console as Protocol client (the attach contract).
- RFC §1 — first-five-minutes adoption guarantee.
- RFC §3 — security boundary (the bootstrap endpoint MUST not weaken the JWT auth path).

## Briefs informing this phase

- brief 13 — operator UX (the first-attach flow is the entry to every other surface).
- brief 11 — Console feature surface (Settings is the documented connection-management home).

## Brief findings incorporated

- **brief 13 (operator UX).** "An operator who can't reliably attach in <5 minutes after `harbor console` is the lost adoption signal we are explicitly closing." The Settings card SHOULD be the load-bearing path; today it's a stub.
- **brief 11 (Console feature surface).** CONVENTIONS.md §5 forbids stubbed actions. A form that silently no-ops is the same anti-pattern caught in test-stub-as-default-driver §13 enforcement.

## Findings I'm departing from (if any)

None.

## Goals

- The `Settings → Connected Runtimes → Add Runtime` form collects all six fields the connection resolver requires (`name` + `baseURL` + `token` + `tenant` + `user` + `session`); submitting attaches the Console for real.
- A first load (any URL) that resolves no connection redirects to `/settings`, the only surface where the operator can fix it.
- When `harbor console` is co-resident with a Runtime on `127.0.0.1:18790`, the Settings page detects this and offers a one-click "Attach to local Runtime" button that mints a fresh dev token via a new `/v1/dev/bootstrap.json` endpoint and seeds all five connection fields.
- The bootstrap endpoint is loopback-gated, never registered by `harbor serve`, and ALWAYS off when `HARBOR_DEV_ALLOW_MOCK` is unset AND there is no operator-configured dev identity (fail-loud per CLAUDE.md §13).
- The localStorage-DevTools-seed path stays available for power users / scripts but ceases to be the documented first-attach path; `docs/skills/run-the-dev-loop/SKILL.md` is rewritten accordingly.

## Non-goals

- Removing the existing `attachConnection(baseURL)` partial-attach path. It's used by other surfaces (the existing Settings flow when the operator already has a token cached); the Phase 105 widening is additive.
- Auto-attaching to ANY same-origin Runtime without operator confirmation. The first click is the security boundary — the Console renders the "Attach to local Runtime?" button, never auto-attaches.
- A new identity-issuance flow. `harbor dev` and `harbor console` already mint dev tokens; Phase 105 plumbs them into the first-attach UX.
- Multi-tenant Console first-run. Phase 105 ships the single-operator dev posture.
- A "remember last 5 runtimes" history. The address-book persistence already exists in `runtime_registry`; Phase 105 doesn't extend its surface.

## Acceptance criteria

The bullets below are mutually exclusive — every one must be satisfied:

- [ ] **AC-1** `web/console/src/lib/components/settings/ConnectedRuntimesCard.svelte` form collects **six fields**, NOT two. The new fields below are all `required`; submit is gated until ALL are non-empty AND validate:
  - `draftName` (existing) — non-empty string.
  - `draftURL` (existing) — non-empty + `new URL(value)` does not throw.
  - `draftToken` (NEW) — non-empty + matches a "looks like a JWT" pattern: three base64url segments separated by `.`, total length ≥ 200 chars. Regex: `^[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+$`.
  - `draftTenant` (NEW) — non-empty string, no whitespace.
  - `draftUser` (NEW) — non-empty string, no whitespace.
  - `draftSession` (NEW) — non-empty string, no whitespace.
- [ ] **AC-2** The card's `onadd` callback signature widens to:
  ```ts
  onadd: (
    name: string,
    baseURL: string,
    token: string,
    identity: { tenant: string; user: string; session: string },
    scopes?: string[]
  ) => Promise<void>
  ```
  Old `(name, baseURL)` callers fail TypeScript compilation. `scopes` is optional and defaults to `['admin', 'console:fleet']` at the Settings-page call site (the existing dev-token scope set).
- [ ] **AC-3** `web/console/src/lib/settings/console_db.svelte.ts::addRuntime` widens to match the new signature and calls `attachConnection(baseURL, { token, identity, scopes })` — the existing `attachConnection` already accepts opts (lines 172-188 of `web/console/src/lib/connection.ts`); Phase 105 just uses every parameter.
- [ ] **AC-4** Form-level validation: clicking Submit with any field empty / token failing the regex / URL un-parseable surfaces inline error text and does NOT call `onadd`. No silent no-op.
- [ ] **AC-5** `web/console/src/routes/(console)/+layout.svelte` (the shell layout) gains a first-load redirect: when `resolveConnection() === null` AND the current pathname is NOT `/settings`, the layout calls `goto('/settings')` exactly once on mount (idempotent: subsequent layouts on `/settings` no-op).
- [ ] **AC-6** New endpoint `POST /v1/dev/bootstrap.json` on `harbor dev` AND `harbor console` (and ONLY those two subcommands; `harbor serve` MUST NOT register it). Request body MAY be empty `{}`; response is:
  ```json
  {
    "base_url": "http://127.0.0.1:18080",
    "token":    "eyJ...",
    "identity": { "tenant": "dev", "user": "dev", "session": "dev" },
    "scopes":   ["admin", "console:fleet"],
    "protocol_version": "0.1.0"
  }
  ```
  The token is freshly minted on every call (uses the existing ES256 dev signer in `cmd/harbor/devauth.go`; the EXISTING `harbor dev` token-minting code path is the source of truth — Phase 105 calls it again).
- [ ] **AC-7** Loopback gate: the bootstrap endpoint reads `r.RemoteAddr` (NOT any request header), parses the host, and returns `403` from any non-loopback peer. Loopback netblocks: `127.0.0.0/8`, `::1/128`. Spoofed `X-Forwarded-For` / `Forwarded` headers are ignored. The 403 body carries `{"code":"forbidden","message":"bootstrap endpoint is loopback-only"}`.
- [ ] **AC-8** Fail-loud posture: when both `HARBOR_DEV_ALLOW_MOCK=0` AND no dev identity is wired (the production-shaped `harbor serve` case), the endpoint is NOT registered at all — `404` from any peer. Verified by a smoke assertion that `harbor serve` does not advertise the route.
- [ ] **AC-9** New `web/console/src/lib/components/settings/AttachToLocalCard.svelte` component. When the operator is on Settings AND `resolveConnection() === null`, the card renders. On click it:
  1. `fetch(window.location.origin + '/v1/dev/bootstrap.json', { method: 'POST', credentials: 'omit' })`.
  2. On 200 → `attachConnection(body.base_url, { token: body.token, identity: body.identity, scopes: body.scopes })` + `window.location.reload()`.
  3. On 403 → render a neutral info banner "Local-bootstrap endpoint not available; use the manual form below."
  4. On 404 → same neutral info banner (the endpoint isn't registered on this build).
  5. On network error → red error banner with the error text.
- [ ] **AC-10** The AttachToLocalCard is HIDDEN (not rendered) when `resolveConnection() !== null` — operators who already attached don't see it.
- [ ] **AC-11** `docs/skills/run-the-dev-loop/SKILL.md` is rewritten:
  - The "1. Single-process dev" section leads with: "Open <http://127.0.0.1:18790> → Settings → click 'Attach to local Runtime'." DevTools snippet drops to a "Power-user / scripted attach" appendix.
  - The "2. Multi-process dev" section leads with: "Open Settings → Connected Runtimes → Add Runtime → fill the six fields (name + URL + token + tenant + user + session)." Same DevTools snippet appendix referenced.
- [ ] **AC-12** `scripts/smoke/phase-105.sh` (live-server) asserts every wire surface listed in "Smoke script additions" below.
- [ ] **AC-13** Existing tests in `cmd/harbor/cmd_dev_test.go` + `cmd/harbor/cmd_console_test.go` still pass; Phase 105 ADDS test cases, NEVER deletes existing assertions.

## Files added or changed

### Runtime (Go)

- `internal/server/dev_bootstrap.go` — **NEW**. ~120 LOC. Houses:
  - `BootstrapHandler` struct with deps `(devSigner *devauth.Signer, identity identity.Triple, scopes []string, baseURL string, logger *slog.Logger)`.
  - `func (h *BootstrapHandler) ServeHTTP(w http.ResponseWriter, r *http.Request)`:
    - 1) `loopback := isLoopback(r.RemoteAddr)`; if false → `http.Error(w, ..., 403)`.
    - 2) Mint a fresh token via `h.devSigner.Sign(h.identity)` (24h TTL).
    - 3) Marshal a `BootstrapResponse` struct + `w.Write(json)`.
  - `func isLoopback(remoteAddr string) bool` — parses host (strips `:port`), accepts `127.0.0.0/8` + `::1`. Uses `net.ParseIP` + `IsLoopback()`.
  - `type BootstrapResponse struct { ... }` matching AC-6 schema with json tags.
- `internal/server/dev_bootstrap_test.go` — **NEW**. ~150 LOC. Tests:
  - `TestBootstrap_Loopback_127001_Returns200` — peer `127.0.0.1:12345` returns 200, body has all five fields.
  - `TestBootstrap_Loopback_IPv6_Returns200` — peer `[::1]:12345` returns 200.
  - `TestBootstrap_NonLoopback_Returns403` — peer `192.168.1.5:12345` returns 403 with the documented body.
  - `TestBootstrap_SpoofedXForwardedFor_StillReturns403` — peer `192.168.1.5:12345` with header `X-Forwarded-For: 127.0.0.1` returns 403 (header ignored).
  - `TestBootstrap_TokenIsFreshPerCall` — two consecutive calls return DIFFERENT `token` values.
  - `TestBootstrap_ResponseShape` — JSON parses + every documented field is present and non-empty.
  - `TestBootstrap_ConcurrentReuse_NoCrossTalk` — N=100 concurrent requests under `-race`; each gets a valid 200; no goroutine leak; baseline goroutine count restored.
- `cmd/harbor/cmd_dev.go` — register the bootstrap handler. Find the existing mux setup (it adds the auth-protected `/v1/*` routes); add ONE line registering `/v1/dev/bootstrap.json` BEFORE the auth middleware (the bootstrap endpoint is its OWN auth boundary via the loopback gate; it must not require an existing token).
- `cmd/harbor/cmd_console.go` — same registration as cmd_dev.go (the embedded Runtime + Console share the mux setup). Mirror exactly.
- `cmd/harbor/cmd_serve.go` — VERIFY (no edit unless missing) that `harbor serve` does NOT register the bootstrap handler. Add a comment: `// PHASE 105: bootstrap endpoint is dev-only; harbor serve never mounts it.`

### Console (Svelte)

- `web/console/src/lib/components/settings/ConnectedRuntimesCard.svelte` — widen form. Add 4 `$state` declarations alongside the existing `draftName` + `draftURL`:
  ```ts
  let draftToken = $state('');
  let draftTenant = $state('');
  let draftUser = $state('');
  let draftSession = $state('');
  ```
  Extend `submitAdd()` to:
  - Validate all 6 fields per AC-1.
  - Call the widened `onadd(draftName.trim(), draftURL.trim(), draftToken.trim(), { tenant: draftTenant.trim(), user: draftUser.trim(), session: draftSession.trim() })`.
  - Clear all 6 drafts on success.
  Add 4 new `<input>` elements in the form (HTML order: name → URL → token (textarea) → tenant → user → session). Tag every input with `data-testid` for e2e: `add-runtime-token`, `add-runtime-tenant`, `add-runtime-user`, `add-runtime-session`.
- `web/console/src/lib/settings/console_db.svelte.ts` — widen `addRuntime` (currently lines 183-214) to:
  ```ts
  async addRuntime(
    name: string,
    baseURL: string,
    token: string,
    identity: { tenant: string; user: string; session: string },
    scopes: string[] = ['admin', 'console:fleet']
  ): Promise<void> {
    this.addWarning = null;
    attachConnection(baseURL, { token, identity, scopes });  // ← was: attachConnection(baseURL)
    // ... rest of method unchanged ...
  }
  ```
- `web/console/src/routes/(console)/settings/+page.svelte` — at the `<ConnectedRuntimesCard onadd={...}>` invocation (around current line 216), update the callback to pass all five fields:
  ```svelte
  onadd={(name, url, token, identity, scopes) => db.addRuntime(name, url, token, identity, scopes)}
  ```
- `web/console/src/routes/(console)/+layout.svelte` — in the existing `onMount` (or equivalent), AFTER the connection resolve, branch on null and redirect:
  ```ts
  import { goto } from '$app/navigation';
  import { page } from '$app/state';
  import { resolveConnection } from '$lib/connection';

  $effect(() => {
    if (resolveConnection() === null && !page.url.pathname.startsWith('/settings')) {
      goto('/settings', { replaceState: true });
    }
  });
  ```
- `web/console/src/lib/components/settings/AttachToLocalCard.svelte` — **NEW**. ~80 LOC. Renders ONLY when `resolveConnection() === null`. Has one button "Attach to local Runtime" + a status region. On click implements AC-9 sequence. Tag the button `data-testid="attach-to-local-runtime"`.
- `web/console/src/routes/(console)/settings/+page.svelte` — render `<AttachToLocalCard />` ABOVE the existing `<ConnectedRuntimesCard />` (it should be the first thing an operator sees in the empty state). The card's own `if connected` guard keeps it from rendering once attached.

### Docs

- `docs/skills/run-the-dev-loop/SKILL.md` — rewrite per AC-11. Keep the existing "Common failure modes" + "See also" sections; replace ONLY the "1. Single-process dev" and "2. Multi-process dev" sections per AC-11. Move the DevTools localStorage snippet into a new "## Power-user / scripted attach" section at the end (before "Common failure modes").

### Smoke

- `scripts/smoke/phase-105.sh` — **NEW** (replaces the existing skeleton from PR #243). Static-only header but exercises the live Protocol surface for AC-6/AC-7/AC-8.

## Public API surface

- New HTTP endpoint `POST /v1/dev/bootstrap.json` (NOT a canonical Protocol method — it's a dev-only convenience endpoint outside `/v1/control` / `/v1/tasks` / etc., gated by loopback). It MUST NOT be added to `internal/protocol/methods/methods.go` (that file is the canonical Protocol surface) — instead live next to the auth middleware as a sibling.
- `ConnectedRuntimesCard.svelte::onadd` signature widens (internal Console API; no Protocol impact).
- `attachConnection` itself is unchanged — Phase 105 just USES every optional arg the function already accepts.

## Test plan

### Unit (Go)

- `internal/server/dev_bootstrap_test.go::TestBootstrap_Loopback_127001_Returns200`
- `internal/server/dev_bootstrap_test.go::TestBootstrap_Loopback_IPv6_Returns200`
- `internal/server/dev_bootstrap_test.go::TestBootstrap_NonLoopback_Returns403`
- `internal/server/dev_bootstrap_test.go::TestBootstrap_SpoofedXForwardedFor_StillReturns403`
- `internal/server/dev_bootstrap_test.go::TestBootstrap_TokenIsFreshPerCall`
- `internal/server/dev_bootstrap_test.go::TestBootstrap_ResponseShape`
- `cmd/harbor/cmd_dev_test.go` — extend to assert the bootstrap route IS registered.
- `cmd/harbor/cmd_console_test.go` — extend to assert the bootstrap route IS registered.
- `cmd/harbor/cmd_serve_test.go` (or wherever serve tests live) — assert the bootstrap route is NOT registered (returns 404).

### Unit (Svelte)

- `web/console/src/lib/components/settings/ConnectedRuntimesCard.test.ts` (new or extend existing):
  - `ConnectedRuntimesCard: empty form blocks submit` — render with the 6 inputs, click Submit, assert `onadd` NOT called.
  - `ConnectedRuntimesCard: malformed token blocks submit` — fill 5 fields validly + a too-short token, assert `onadd` NOT called + error text rendered.
  - `ConnectedRuntimesCard: malformed URL blocks submit` — fill 5 fields validly + URL "not-a-url", assert error + no call.
  - `ConnectedRuntimesCard: all-valid submit calls onadd with widened signature` — assert `onadd` called with `(name, url, token, { tenant, user, session })`.
- `web/console/src/lib/components/settings/AttachToLocalCard.test.ts` (new):
  - `AttachToLocalCard: hidden when connected` — provide a non-null `resolveConnection` mock, assert nothing renders.
  - `AttachToLocalCard: renders button when disconnected`.
  - `AttachToLocalCard: 200 response attaches + reloads` — mock `fetch` returning the bootstrap shape, click button, assert `attachConnection` called with the right args.
  - `AttachToLocalCard: 403 renders info banner` — mock 403, assert no attach, assert banner text.
  - `AttachToLocalCard: 404 renders info banner` — mock 404, assert no attach, assert banner.
  - `AttachToLocalCard: network error renders red banner`.

### Integration (Playwright)

Extend `test/integration/console_e2e_*` (the Phase 75 baseline) or add a new `console_first_attach_e2e_test.go`:

- `TestE2E_FirstLoad_RedirectsToSettings` — open `harbor console` cold (no localStorage), navigate to `/`, assert URL becomes `/settings`.
- `TestE2E_AttachToLocal_OneClickAttach` — open `harbor console` cold, navigate, click "Attach to local Runtime", assert connection footer flips to "Connected", assert subsequent page navigation to `/playground` works.
- `TestE2E_ManualAttach_AllSixFields` — open Settings, fill the 6-field form, submit, assert connection live.

Real `harbor console` co-resident runtime per AGENTS.md §17.3 (no mocks on the seam).

### Concurrency / leak

- `TestBootstrap_ConcurrentReuse_NoCrossTalk` (Go) — listed above; N=100 concurrent under `-race`.

### Conformance

- N/A — bootstrap endpoint is not part of the canonical Protocol surface; the `protocol/conformance/` suite skips it.

## Smoke script additions

`scripts/smoke/phase-105.sh` — `PREFLIGHT_REQUIRES: live-server`. Assertions:

1. `assert_status 200 "$(api_url /v1/dev/bootstrap.json)" POST -d '{}'` — endpoint reachable on `harbor dev` build.
2. `assert_json_truthy '.token' "$(curl ... /v1/dev/bootstrap.json -X POST -d '{}')"` — token field non-empty.
3. `assert_json_path '.identity.tenant' 'dev' "$(curl ... -X POST -d '{}')"` — identity correctly populated.
4. `assert_json_path '.scopes[0]' 'admin' "$(curl ...)"` — admin scope first.
5. **Token freshness**: call twice in a row, capture both `.token` values, assert they differ.
6. **Non-loopback rejection** (only runnable in environments where the test can simulate a non-loopback peer; otherwise SKIP):
   - `curl --resolve` to a non-loopback bind OR via the test harness's mock peer → assert 403.
   - If unable to simulate, SKIP with reason `"non-loopback rejection: requires a non-loopback peer simulation (test gap, manual verification only)"`.
7. **Static**: `grep -q 'AttachToLocalCard' web/console/src/routes/(console)/settings/+page.svelte` — page imports the new component.
8. **Static**: `grep -q 'goto.*settings' web/console/src/routes/(console)/+layout.svelte` — layout has the redirect.

## Coverage target

- `internal/server/`: 80% (existing target). The new `dev_bootstrap.go` is small; the test suite covers every branch.
- `cmd/harbor/`: 80%. Extend the existing dev/console/serve tests.
- `web/console/src/lib/components/settings/`: 80%. Per existing CONVENTIONS.md §10.

## Dependencies

- 73m (Settings page baseline — the `ConnectedRuntimesCard` and address-book infra ship here).
- 73r (the existing `attachConnection` API + the `<PageState>` Disconnected branch).
- 83u (D-163 — `addRuntime` already split into "active connection" + "address-book" effects; Phase 105 keeps the split).
- 85k (operator skills — `run-the-dev-loop` is the doc this phase rewrites).

## Risks / open questions

- **Loopback gate spoofing.** The bootstrap endpoint reads `r.RemoteAddr` (the actual TCP peer); a non-loopback peer cannot fake this. BUT if Harbor ever sits behind a reverse proxy that rewrites `RemoteAddr`, the gate breaks. Mitigation: the endpoint is registered ONLY on `harbor dev` + `harbor console` (the dev subcommands that never run behind a proxy in V1.2 posture). A future production deployment that proxies the Console MUST NOT mount the bootstrap endpoint.
- **Token staleness after `harbor dev` restart.** Bootstrap mints a fresh token; the operator's localStorage now holds it. The Runtime's signing key is in-memory and rotates per `harbor dev` boot, so a subsequent restart invalidates the token. Mitigation: when the Console gets 401 on a Protocol call and the bootstrap endpoint is reachable, the AttachToLocalCard auto-rebootstrap path can run (Phase 105 explicitly defers this auto-rebootstrap; the operator hits the "Attach to local Runtime" button again. Phase 107 / V1.3 can layer the auto-rebootstrap on top.).
- **CSRF on the bootstrap endpoint.** The endpoint mints a token; a malicious local web page could trigger a fetch. Mitigation: `credentials: 'omit'` on the Console's fetch + the loopback gate + the absence of cookies in the response means a malicious site can't steal a token even if it succeeds. The token still goes to localStorage, accessible only to the same-origin page. Same-origin policy is the second gate.
- **Six-field form is tedious.** The "Attach to local Runtime" one-click button covers the dev case; remote-attach requires the six fields. We could shorten remote-attach by accepting a single "share URL" containing all fields query-encoded, but that's an enhancement, not a blocker.

## Glossary additions

- **Bootstrap endpoint** — `/v1/dev/bootstrap.json` on `harbor dev` / `harbor console`. Loopback-gated. Mints a fresh dev token + returns the full connection envelope. Never registered by `harbor serve`. (Glossary entry alphabetised under "B".)
- **First-attach** — the operator-facing flow turning a fresh Console open into a Console with a live Protocol connection. Phase 105 makes this zero-clicks for the co-resident case, six-clear-fields for the remote case. (Alphabetised under "F".)
- **AttachToLocalCard** — the new Settings component (`web/console/src/lib/components/settings/AttachToLocalCard.svelte`) that hosts the one-click attach button when co-resident. (Alphabetised under "A".)

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] Cross-session isolation test — covered (bootstrap endpoint is loopback-only AND mints identity-bound tokens for a single triple; tests verify no cross-tenant leakage)
- [ ] Concurrent-reuse test — `TestBootstrap_ConcurrentReuse_NoCrossTalk` (N=100 under `-race`)
- [ ] Integration test — Playwright e2e per AGENTS.md §17.3 (real `harbor console` on the seam)
- [ ] Glossary updated for `Bootstrap endpoint`, `First-attach`, `AttachToLocalCard`
- [ ] `docs/skills/run-the-dev-loop/SKILL.md` rewrite reviewed against AC-11

## Implementation order (suggested)

A less-context implementing agent can follow these steps top-to-bottom; each step is independently testable:

1. **Add the bootstrap endpoint** (`internal/server/dev_bootstrap.go` + tests). Verify with `go test -race ./internal/server/...`.
2. **Register it on `harbor dev` + `harbor console`** (`cmd/harbor/cmd_dev.go` + `cmd_console.go` edits). Verify with `go test -race ./cmd/harbor/...` + a manual `curl -X POST http://127.0.0.1:18080/v1/dev/bootstrap.json -d '{}'`.
3. **Verify `harbor serve` does NOT register it** (`cmd/harbor/cmd_serve_test.go` extension). Verify `harbor serve` curl returns 404.
4. **Widen `attachConnection` usage in `console_db.svelte.ts`** (mechanical signature change).
5. **Widen `ConnectedRuntimesCard.svelte`** (add 4 inputs + validation + widened `onadd`).
6. **Update `settings/+page.svelte`** to pass the new params.
7. **Add unit tests for `ConnectedRuntimesCard`** (per the named tests above).
8. **Add `AttachToLocalCard.svelte`** with its tests.
9. **Embed `AttachToLocalCard` in `settings/+page.svelte`** above `ConnectedRuntimesCard`.
10. **Add the first-load redirect** to `(console)/+layout.svelte`.
11. **Add the Playwright e2e tests**.
12. **Rewrite `docs/skills/run-the-dev-loop/SKILL.md`**.
13. **Write `scripts/smoke/phase-105.sh`** + run it locally against a live `harbor dev`.
14. **Run `make drift-audit && make preflight`** — both green.
15. **Open PR.**
