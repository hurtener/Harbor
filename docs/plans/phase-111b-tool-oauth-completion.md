# Phase 111b — Tool-OAuth completion leg

## Summary

Tool-side OAuth is half a choreography. The pause **producer** is live: the
catalog OAuth wrapper routes a token-missing tool through
`Provider.buildAuthRequired` (`internal/tools/auth/provider.go:332`) →
`coordinator.Request`, emitting `tool.auth_required` and parking the run.
`Provider.InitiateFlow` (`provider.go:424`) mints the authorize URL. But the
**resume half is unreachable**: `Provider.CompleteFlow` (`provider.go:520` —
the (state, code) exchange that persists the token and calls
`coordinator.Resume` with the typed `DecisionResume` marker at
`provider.go:615-625`) has **zero callers**, and no HTTP route anywhere in the
tree exchanges `(state, code)`. The godocs reference a "Harbor Protocol
callback handler" that does not exist (`internal/tools/auth/auth.go:189`,
`RedirectURI`). The in-flight Wave A chore (PR #278)
makes those godocs honest; **this phase ships the real thing.**

Phase 111b ships `auth.CallbackHandler` — an exported `http.Handler` that maps
the OAuth redirect back onto `PendingFlow` lookup → `CompleteFlow` → typed
error responses — mounted by `harbor dev` and mountable by headless consumers
on their own mux. The full choreography is proven end-to-end against a test
OAuth server: gated tool → `pause.requested` (AuthRequired) → `InitiateFlow`
URL → simulated callback → `CompleteFlow` → coordinator resume → the run
re-enters and the token is used. This closes the §13
primitive-without-consumer pair (`InitiateFlow`/`CompleteFlow` are the
`SpawnTask`/`AwaitTask` of OAuth — half a pair is orphaned work).

## RFC anchor

- RFC §6.4 — Tool catalog and transports ("Tool-side OAuth + HITL uses the
  unified pause/resume primitive … the user completes OAuth out-of-band, the
  callback handler resumes the run with the token. (Settled.)" — the callback
  handler this phase makes real).
- RFC §3.3 — the unified pause/resume primitive (tool-side OAuth is one of
  the four canonical pause reasons; the typed `Decision` marker D-096).
- RFC §6.3 — Steering and pause/resume (the Coordinator contract
  `CompleteFlow` resumes through; the non-gate direct-Resume path D-097
  preserves).

## Briefs informing this phase

- brief 09 — MCP OAuth, lessons from bifrost (the
  `InitiateUserOAuthFlow`/`CompleteUserOAuthFlow` pair Harbor's provider
  mirrors; user-bound vs agent-bound token scoping)
- brief 02 — planner + steering + HITL (the pause/resume choreography the
  completion leg re-enters)

## Brief findings incorporated

- **brief 09 §OAuth2Provider interface.** The reference machinery is an
  explicit pair — `InitiateUserOAuthFlow(...)` AND
  `CompleteUserOAuthFlow(ctx, state, code)`. Harbor transcribed the pair onto
  `Provider` but only ever wired the initiate half. This phase restores the
  pair's unit-of-value: a flow you can start but never complete is
  indistinguishable from a hang to the operator.
- **brief 09 §two binding scopes.** User-bound vs agent-bound tokens are
  per-attachment policy; the callback handler must NOT widen scope — it
  resolves the pending flow by opaque `state` and lets `CompleteFlow`'s
  existing record carry the identity binding. The handler adds zero identity
  logic of its own.
- **brief 02 §pause taxonomy.** Tool OAuth is one of the four canonical pause
  reasons riding ONE primitive — the completion leg goes through
  `coordinator.Resume` (already inside `CompleteFlow`), never a parallel
  resume path (§13 "bypassing the unified pause/resume primitive" is
  reject-on-sight).

## Findings I'm departing from (if any)

None.

## Goals

- **Exported callback handler:**
  `auth.CallbackHandler(providers map[string]OAuthProvider, opts ...CallbackOption) http.Handler`
  in `internal/tools/auth/` (new `callback.go`):
  - Parses `state`, `code`, and the OAuth error params (`error`,
    `error_description`) from the redirect query.
  - Locates the owning provider via the existing
    `Provider.PendingFlow(state)` surface (`provider.go:683`) — no new
    state-tracking, no second source of truth.
  - Calls `CompleteFlow(ctx, state, code)`; maps the existing typed sentinels
    onto HTTP statuses: `ErrFlowNotFound` → 404, `ErrFlowExpired` → 410,
    `ErrStateMismatch` → 400, provider-upstream exchange failure → 502;
    success → a minimal operator-facing HTML "authorization complete — return
    to your session" page (no token material in the response, ever).
  - Upstream `error` param (user denied, etc.) → the handler surfaces the
    denial loudly (400 + the audit-safe reason); whether a denied flow also
    resumes-with-rejection is an implementor verification against the pause
    record's lifecycle (see Risks).
  - Logs through the injected `*slog.Logger`; never logs `code` or token
    bytes (§7 — secrets).
- **Mounted by `harbor dev`:** the dev server mounts the handler at
  `GET /v1/tools/oauth/callback` (the path becomes the documented default
  `RedirectURI` shape); the OAuth provider assembly already constructed by
  the 110d assembly's catalog band (`auth.BuildProviders` inside
  `assemble.Assemble` — D-197) passes its provider map through via
  `assemble.Stack.OAuthProviders`. Devstack reads the same `Stack` field —
  no D-094 hand-mirror needed since 110d collapsed the wiring to thin
  callers.
- **Mountable headless:** the handler is a plain `http.Handler` — a headless
  consumer mounts it on their own mux at whatever path matches their
  configured `RedirectURI`. No dependency on the Protocol server, the dev
  server, or `cmd/harbor`.
- **Run re-entry proven:** the E2E asserts the parked run actually re-enters
  after resume and the tool invocation succeeds using the freshly-persisted
  token (the audit's "a bare Resume re-parks immediately" trap is the exact
  regression this guards).
- **Recipe:** the OAuth completion choreography ships as a section of a NEW
  `docs/recipes/steer-and-resume-a-run.md` (the audit-§7 recipe family
  name). Decision: one recipe, not a dedicated OAuth recipe — HITL approval
  and tool OAuth ride the SAME primitive (RFC §3.3), and the recipe's value
  is showing the one choreography with two triggers; fragmenting per-reason
  would re-teach the four-parallel-implementations mistake the primitive
  exists to close. The recipe documents what is and is NOT yet supported
  (honesty per audit §7).
- **Godoc repair:** `auth.go:189`'s `RedirectURI` doc (and any sibling the
  Wave A chore re-worded) is re-pointed at the now-real
  `auth.CallbackHandler` + the dev-server mount path.

## Non-goals

- No new OAuth grant types, token-exchange semantics, or driver changes —
  the `drivers/oauth2` exchange machinery is consumed as shipped (Phase 30).
- No Console "complete your sign-in" UI — the Console already renders the
  pause + auth URL from the canonical events; the redirect lands on the
  runtime's handler, not a Console route. (A richer Console completion
  surface is a follow-up if operator feedback asks for it.)
- No MCP HTTP OAuth (RFC 9728 discovery / 401 step-up) — that is 85b's
  scope; this phase completes the Phase-30 tool-side leg only.
- No multi-process callback routing (the pause handle registry is
  process-local at V1 — RFC §6.3; the callback must land on the same process
  that parked the run, which is true for `harbor dev` and single-process
  embedders by construction).

## SDK-consumer reachability

- **Binary path:** `harbor dev` mounts the handler automatically; the
  operator's `RedirectURI` points at
  `http://<bind>/v1/tools/oauth/callback`. Zero ceremony.
- **Headless path:** `auth.CallbackHandler(providers)` is a plain
  `http.Handler` over the already-exported `OAuthProvider` interface — a Go
  consumer who assembled providers (via 110-band's promoted
  `auth.BuildProviders` or by hand) mounts it on any mux. The recipe's
  headless section shows exactly this, including the
  what-port-must-match-RedirectURI gotcha.

## Acceptance criteria

- [x] `auth.CallbackHandler` exported; state→`PendingFlow` lookup across the
      provider map; `CompleteFlow` invocation; the four error mappings
      (404/410/400/502) + success page; no secret material in any response
      or log line (asserted by test).
- [x] **§13 primitive-with-consumer:** `CompleteFlow` gains its first
      production caller (the handler), and the handler gains its production
      mount (`harbor dev`) in the SAME phase — the
      `InitiateFlow`/`CompleteFlow` pair is whole.
- [x] `harbor dev` mounts the handler at `GET /v1/tools/oauth/callback`;
      devstack mirrors (D-094); both wired from the existing OAuth provider
      assembly output.
- [x] E2E (the full choreography, test OAuth server via `httptest`): planner
      dispatches a gated tool → `tool.auth_required` + `pause.requested`
      observed on the bus → `InitiateFlow` URL fetched → simulated user
      authorize → redirect hits the handler → `CompleteFlow` persists the
      token → `pause.resumed` carries `Decision: resume` (D-096) → the run
      re-enters and the tool invocation succeeds using the token. Identity
      asserted end-to-end.
      (`test/integration/phase111b_oauth_completion_test.go` — the run-level
      re-entry rides the existing steering RESUME surface; see Risks
      resolution + D-199 §5.)
- [x] Failure modes covered: expired flow (410 + pause still parked),
      mismatched state (400), replayed callback (second GET with the same
      state → 404 `ErrFlowNotFound`, idempotency by consumption).
- [x] `docs/recipes/steer-and-resume-a-run.md` ships with the OAuth section
      (+ the HITL-approval section stub if not already present), including
      the headless mount snippet and the process-local-resume constraint.
- [x] `auth.go` godocs re-pointed at the real handler (the Wave A honesty
      edit is superseded by truth).
- [x] `scripts/smoke/phase-111b.sh` exercises the callback route (see Smoke
      script additions) — new REST endpoint ⇒ same-PR smoke check (§4.2).
- [x] D-199 records the handler shape, the mount path, the one-recipe
      decision, and the §4.3 interface refinements (PendingFlow signature,
      DenyFlow, the steered run-re-entry leg).

## Files added or changed

- `internal/tools/auth/callback.go` — **NEW** `CallbackHandler` +
  `CallbackOption` (logger injection, success-page override).
- `internal/tools/auth/callback_test.go` — **NEW** handler unit tests
  (mappings, no-secret assertions, replay).
- `internal/tools/auth/auth.go` — godoc repair (`RedirectURI` + the
  interface docs naming the callback); `PendingFlowInfo`; the
  `OAuthProvider` interface gains `PendingFlow` + `DenyFlow` (§4.3
  refinement — see Public API surface).
- `internal/tools/auth/provider.go` — `PendingFlow` returns the info
  projection; `DenyFlow`; `CompleteFlow` godoc re-pointed at the real
  handler.
- `internal/tools/auth/drivers/oauth2/oauth2.go` — `PendingFlow` /
  `DenyFlow` passthroughs; `ErrMissingRedirectURL` message names the
  mounted handler.
- `cmd/harbor/cmd_dev.go` — mount the handler on the dev server mux; thread
  the provider map from `assemble.Stack.OAuthProviders` (the 110d assembly's
  catalog band output — D-197).
- `harbortest/devstack/devstack.go` — handler reachable on the devstack's
  test server (same `Stack.OAuthProviders` source; thin-caller parity per
  110d).
- `test/integration/phase111b_oauth_completion_test.go` — the full
  choreography E2E.
- `docs/recipes/steer-and-resume-a-run.md` — **NEW** (OAuth completion
  section + pause/steer framing; honest not-yet-supported notes).
- `docs/recipes/README.md` — index entry.
- `scripts/smoke/phase-111b.sh` — real assertions.
- `docs/decisions.md` — D-199 (reserved; logged when the phase ships).
- `docs/plans/README.md` — status flip on ship.

## Public API surface

- `auth.CallbackHandler(providers map[string]OAuthProvider, opts ...CallbackOption) http.Handler`.
- `auth.CallbackOption` — `WithCallbackLogger(*slog.Logger)`,
  `WithSuccessPage(string)`.
- `auth.CallbackPath` (`/v1/tools/oauth/callback`) +
  `auth.CallbackRoutePattern` (`GET /v1/tools/oauth/callback`) — the
  operator-visible vocabulary for `RedirectURI` construction.
- **§4.3 refinements shipped with the phase (recorded in D-199):**
  `OAuthProvider.PendingFlow(state)` returns `(PendingFlowInfo, bool)`
  (the prior `bool` could neither locate an owner across the provider map
  nor rebuild the completing identity; `PendingFlowInfo` exposes
  Source / BindingScope / Identity / ExpiresAt and deliberately NOT the
  PKCE verifier or the pause Token), and the interface gains
  `DenyFlow(ctx, state, reason)` (upstream `error=access_denied` →
  consume the flow + resume the pause with `DecisionReject`). Both land
  ON `OAuthProvider` — no optional-capability ceremony (§4.4); the
  `oauth2` driver passes through.

> Scope note: "public" here is module-internal — `internal/` packages are not
> importable by external modules (the recorded reason `harbortest/` lives at
> the top level). This surface is stable for in-module consumers (cmd,
> harbortest, examples); external-team embedding needs the future
> facade/export RFC (audit §5 / Wave D), out of scope for this phase.

## Test plan

- **Unit:** handler param parsing; provider lookup across a multi-provider
  map; the four error→status mappings; success page contains no token bytes;
  upstream `error` param path; logger receives redaction-safe fields only.
- **Integration:** `test/integration/phase111b_oauth_completion_test.go` —
  real drivers everywhere (catalog + inproc tool + OAuth wrapper, pauseresume
  with bus, state inmem, `httptest` OAuth authorization+token server), the
  full produce→park→initiate→callback→resume→re-enter→invoke choreography;
  identity propagation asserted on `tool.auth_required` / `pause.resumed`
  payloads; failure modes: expired flow + replayed callback.
- **Conformance:** N/A — no driver seam added; the `OAuthProvider` interface
  is unchanged.
- **Concurrency / leak:** N≥100 concurrent callback GETs (mixed valid /
  invalid states) against one handler instance under `-race` — the handler
  is a compiled artifact (D-025): no data races, no cross-flow bleed (flow A's
  completion never resumes flow B's pause), goroutine baseline restored.

## Smoke script additions

`scripts/smoke/phase-111b.sh` (PREFLIGHT_REQUIRES: live-server once shipped):

- `GET /v1/tools/oauth/callback` with no params → 400 (handler mounted,
  fails loud on garbage); 404/405/501 → SKIP pre-phase (the sacred
  convention distinguishes "not mounted yet" from "mounted, bad request").
- `GET /v1/tools/oauth/callback?state=bogus&code=x` → 404 (flow-not-found
  mapping live).
- `go test ./internal/tools/auth/... -run Callback` green (unit-tests leg).

## Coverage target

- `internal/tools/auth`: 85% (package target maintained; `callback.go` fully
  covered — it is small and pure-ish).

## Dependencies

- 30 (tool-side OAuth provider + TokenStore), 50 (unified pause/resume
  primitive), 31 (approval-gate machinery shares the catalog wrapper seam).
- The D-192 steering-dispatch fix (Wave A — the B1 HITL/run-loop dispatch
  repair): run re-entry after resume must be reachable while a tool blocks
  in-flight; the E2E's re-entry assertion depends on it.

## Risks / open questions

> **Resolutions (shipped).** (1) *Re-entry mechanics:* the run-level
> re-entry rides the EXISTING steering surface — a `RESUME` control on
> the run's inbox (Protocol `resume` / Console intervention queue / an
> in-process watcher), the same path HITL approval uses; the OAuth
> pause's own resolution is fully automatic via the callback. The E2E
> proves the observable contract (run completes; the tool fetches and
> uses the minted token); an automatic completion→run-resume bridge is
> a recorded candidate follow-up (D-199 §5). (2) *Denied-authorization
> semantics:* resume-with-rejection, per the plan's recommendation —
> `Provider.DenyFlow` consumes the flow and resumes the pause with
> `DecisionReject` (D-199 §4). (3) *Mount-path collision:* none — the
> exact `GET /v1/tools/oauth/callback` pattern is registered before the
> `/v1/` catch-all; Go's ServeMux precedence resolves it first.

- **Staging note (Wave C):** the 111 band parallelizes freely once Wave B
  Stage 1 (110a + 110c) merges; 111b has no 110-band dependency (the
  provider assembly exists in cmd today; if Wave B's P7 `auth.BuildProviders`
  promotion lands first, the mount consumes it — either order works); all
  six 111-band phases are mutually independent.
- **Re-entry mechanics.** How the parked run resumes execution (re-invoke of
  the blocked wrapper vs. step-boundary re-entry) is owned by the
  pauseresume/steering layer post-D-192; this plan asserts the OBSERVABLE
  contract (run completes, token used) and deliberately does not pin the
  mechanism. If the implementor finds re-entry genuinely unreachable even
  after D-192, that is a pause-and-ask checkpoint, not a silent scope cut.
- **Denied-authorization semantics.** Upstream `error=access_denied` — park
  forever vs. resume-with-rejection (terminal per D-071's REJECT shape)? The
  plan recommends resume-with-rejection so the run fails loud instead of
  hanging to TTL; the implementor verifies against `CompleteFlow`'s record
  lifecycle and records the choice in D-199. (Composes with 111c's sweeper —
  even a parked denial eventually times out once 111c lands; the two phases
  are independent but mutually reinforcing.)
- **Mount-path collision.** `/v1/tools/oauth/callback` must not collide with
  Protocol-method routing; the dev mux already serves non-Protocol routes
  (healthz, control) — implementor confirms the router layering.

## Glossary additions

- **OAuth callback handler** — the exported `http.Handler`
  (`auth.CallbackHandler`) that receives the provider redirect, exchanges
  `(state, code)` via `Provider.CompleteFlow`, and resumes the parked run
  through the unified pause/resume primitive. Default dev mount:
  `/v1/tools/oauth/callback`. Add to `docs/glossary.md`.

## Pre-merge checklist

- [x] `make drift-audit` passes
- [x] `make preflight` passes
- [x] `make check-mirror` passes
- [x] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [x] Coverage on touched packages ≥ stated target
- [x] Cross-session isolation: flow records + tokens stay bound to the
      parking identity (asserted in the E2E)
- [x] **Primitive + consumer in the same wave (§13):**
      `InitiateFlow`/`CompleteFlow` pair is whole — `CompleteFlow`'s first
      production caller (the handler) + the handler's production mount land
      in this phase, exercised end-to-end with a test — checked.
- [x] Concurrent-reuse test passes (handler, N≥100, `-race`)
- [x] Integration test wires real drivers end-to-end, asserts identity
      propagation, covers ≥1 failure mode, runs under `-race`
- [x] New endpoint covered by `scripts/smoke/phase-111b.sh` (§4.2)
- [x] Glossary updated
- [x] D-199 filed when the phase ships
