# Phase 129 — JWKS max-stale / revocation ceiling

## Summary

Bound how long the production JWKS-backed validator will honor a cached
signing key when refreshes are failing. Today the JWKS `KeySet` prefers
availability over revocation: when a refresh fetch fails (IdP unreachable
or returning garbage) it retains and serves the prior key snapshot
indefinitely, so a key the IdP has **rotated out / revoked** stays
accepted until the next *successful* fetch — with no upper bound. This
phase adds a configurable **max-stale ceiling** (`identity.jwks_max_stale`)
to the `KeySet`: a snapshot older than the ceiling without a successful
refetch is treated as untrustworthy and validation **fails closed** with a
distinct sentinel (`ErrJWKSStale`) and wire reason (`jwks_stale`), so a
revoked key cannot be honored forever during a prolonged IdP outage. The
ceiling (the primitive) lands with its consumer in the same phase — the
`Validator` surfaces the staleness reason distinctly instead of masking it
as a generic unknown-key failure — proven by a controllable-clock test
(never `time.Sleep`).

## RFC anchor

<!-- RFC §5.5 resolves to "### 5.5 Authentication" in RFC-001-Harbor.md. -->
- RFC §5.5 — Authentication. JWT verification, the asymmetric-algorithm
  allowlist, and the rule that *the Protocol rejects any request without a
  verified identity scope*. This phase tightens that edge: when the key
  source has been unreachable past the ceiling, the runtime can no longer
  vouch for the cached keys, so it rejects rather than serving a possibly
  stale (revoked) key.

## Briefs informing this phase

<!-- brief 06 resolves to docs/research/06-events-observability-devx.md via INDEX.md. -->
- brief 06

## Brief findings incorporated

- brief 06 §1: *"Harbor treats events as the canonical projection of
  runtime state … The Runtime owns the bus; everything else is a client …
  there is no privileged 'internal' view."* — a JWKS-stale rejection is a
  security-relevant runtime event a Console / observability vendor must be
  able to see through the **same canonical channel** as every other auth
  rejection. This phase deliberately routes the new fail-closed reason
  through the existing `auth.rejected` audit + bus emit path (the
  `Validator.audit` seam), not a side channel — operators see `jwks_stale`
  as a first-class rejection reason.
- brief 06 §8: *"Audit: auditing is a bus subscriber that persists a
  redacted projection. It is not a parallel system."* — the staleness
  rejection reason flows through the existing redacted audit payload
  (`kid` / `iss` / `sub` / `reason`); no new audit path, no raw-token
  logging.
- brief 06 §6 (Tests required) + §5 (sharp edges): the staleness behavior
  is time-dependent and MUST be tested against a **controllable clock**,
  never `time.Sleep` (CLAUDE.md §11). The existing `KeySet` and `Validator`
  already accept an injectable clock (`WithJWKSClock` / `WithClock`) — this
  phase exercises the ceiling through it.
- Cross-fork synthesis #2 (recorded in `harbor_research_briefs.md`, surfaced
  in brief 06's fail-loud framing and RFC §5 prose): *fail loudly — explicit
  errors over silent degradation.* Serving a possibly-revoked key forever is
  a silent-degradation smell; the ceiling converts it into an explicit,
  observable, fail-closed rejection.

## Findings I'm departing from (if any)

None. The existing `KeySet` godoc explicitly names this gap — *"There is no
max-stale ceiling; an operator who needs revocation to take effect within a
bound must ensure the IdP stays reachable."* — as a known limitation. This
phase closes it; the departure is from the *current code's documented
behavior*, not from any brief finding.

## Goals

- A configurable **max-stale ceiling** on the JWKS `KeySet`: when the
  cached key snapshot's age (time since the last *successful* fetch)
  reaches the ceiling, `KeyByID` fails closed with a distinct sentinel
  rather than serving a possibly-revoked key.
- A new `identity.jwks_max_stale` config field (duration) with a documented
  safe default and fail-loud validation.
- The `Validator` (the consumer, same phase) surfaces the staleness reason
  distinctly — a new `ErrJWKSStale` sentinel and a `jwks_stale` wire reason
  — instead of collapsing it into the generic `ErrUnknownKey` /
  `verification_failed`, so an operator sees *"JWKS too stale"* rather than
  *"unknown key"*.
- A **loud, rate-bounded** operator signal: when a refresh fails *and* the
  snapshot is already past the ceiling, the keyset escalates its existing
  "serving prior cache" `slog.Warn` to an `slog.Error`; and each rejected
  request flows through the existing `auth.rejected` audit + bus emit with
  the `jwks_stale` reason.
- Documentation that this **bounds, but does not make instantaneous,**
  revocation — and the rotation guidance that makes the bound meaningful
  (overlapping signing keys at the IdP + short token TTLs).

## Non-goals

- **Active revocation / token-introspection (RFC 7662) or a CRL/OCSP-style
  per-token revocation list.** The ceiling bounds *key-set* staleness, not
  per-token revocation. Out of scope.
- **A push/webhook JWKS invalidation channel** (an IdP telling Harbor "key
  X is gone now"). The ceiling is a pull-side time bound; a push channel is
  a separate, larger design.
- **A new Protocol method, wire type, error `Code`, or capability.** This
  phase reuses the existing `auth.Validator` surface, the existing
  `CodeAuthRejected` Protocol code, and the existing free-form wire
  `reason` field. `ProtocolVersion` is untouched (see Risks). A dedicated
  availability-class Protocol error code was considered and deferred (it
  would be a wire-surface change).
- **A "disable the ceiling" opt-out knob.** Harbor's posture is
  fail-closed; the field tunes the ceiling, it does not remove it
  (CLAUDE.md §13 — no identity-downgrading knobs). An operator who wants a
  very loose bound sets a large duration; there is no path to "unbounded."

## Acceptance criteria

<!-- Binding + testable. -->

- [ ] `internal/config.IdentityConfig` gains
      `JWKSMaxStale time.Duration `yaml:"jwks_max_stale,omitempty"``.
      `validateIdentity` rejects a **negative** value
      (`identity.jwks_max_stale must not be negative`) and rejects a
      positive value **below the floor** (a ceiling smaller than the
      minimum possible refresh window can never be satisfied —
      `identity.jwks_max_stale must be >= 1m or 0 for the safe default`).
      A zero value is accepted and means "apply the safe default."
- [ ] `auth.JWKSKeySet` carries an immutable `maxStale time.Duration` set
      once at construction via a new `WithJWKSMaxStale(d time.Duration)`
      option (a non-positive value leaves it at the package default
      `defaultJWKSMaxStale`); it is never mutated after construction.
- [ ] **Primitive — the ceiling fails closed.** When `maxStale > 0` and the
      cached snapshot's age (`now - fetchedAt`) is `>= maxStale` after a
      bounded refresh attempt, `KeyByID` returns a wrapped `ErrJWKSStale`
      **regardless of whether the `kid` resolves** — a possibly-revoked key
      in a too-old snapshot is never served.
- [ ] **Consumer (same phase) — the Validator surfaces it distinctly.** The
      validator's keyfunc propagates `ErrJWKSStale` as `ErrJWKSStale` (not
      masked as `ErrUnknownKey`); `mapParserError` honors `ErrJWKSStale`
      **before** `ErrUnknownKey`; `Validate` returns an error that
      `errors.Is(err, auth.ErrJWKSStale)` is true for.
- [ ] **Wire reason is distinct.** `middleware.reasonForWire` returns
      `"jwks_stale"` for an `ErrJWKSStale` rejection (the existing
      `protocolErrorFor` default arm maps it to `CodeAuthRejected` / HTTP
      401 — unchanged; the *reason* is what distinguishes it for operators).
- [ ] **Controllable-clock consumer test (no `time.Sleep`).** With an
      injected clock and a counting HTTP client whose fetches fail after
      the first success: a token verifies while fresh; once the clock
      advances past the ceiling with refreshes failing, `Validate` rejects
      with `ErrJWKSStale`; a subsequent **successful** refresh (clock still
      advancing, fetch now succeeding) **resets** staleness and the same
      token verifies again. Asserted end-to-end through `Validator.Validate`.
- [ ] **Loud, rate-bounded operator signal.** When a refresh fetch fails
      and the snapshot is already past the ceiling, the keyset emits an
      `slog.Error` (escalated from the existing Warn), bounded to at most
      once per minimum-refresh window; each rejected request still flows
      through the existing `auth.rejected` audit + (when wired) bus emit
      with `reason` distinguishable as the staleness sentinel. No raw token
      is logged (CLAUDE.md §7 rule 7).
- [ ] **`from_config` wiring.** `NewJWKSValidator` threads the effective
      ceiling into the keyset: `cfg.JWKSMaxStale` when `> 0`, else
      `defaultJWKSMaxStale`. A `harbor serve` boot with a negative/too-low
      `jwks_max_stale` fails loud at config validation, naming the field.
- [ ] **Concurrent-reuse (reusable artifact).** The existing JWKSKeySet /
      Validator concurrent-reuse test is extended: N≥100 concurrent
      `Validate` calls against one shared validator across the
      fresh → stale → recovered transitions exhibit no data race, no
      context bleed, no goroutine leak (`-race`).
- [ ] No new Protocol method / wire type / error `Code` / capability;
      `internal/protocol/types/version.go` `ProtocolVersion` unchanged; no
      manifest or generated-docs regeneration required (verified by the
      smoke script asserting the manifest and `ProtocolVersion` are
      untouched).

## Files added or changed

```text
internal/protocol/auth/jwks.go          # maxStale field + WithJWKSMaxStale + ErrJWKSStale + KeyByID ceiling gate + escalated Error log
internal/protocol/auth/jwks_test.go     # ceiling fail-closed + reset; controllable-clock; rate-bounded Error log
internal/protocol/auth/auth.go          # keyfunc propagates ErrJWKSStale distinctly; mapParserError honors it before ErrUnknownKey
internal/protocol/auth/auth_test.go     # mapParserError ordering; Validate surfaces ErrJWKSStale (consumer test)
internal/protocol/auth/middleware.go    # reasonForWire arm: ErrJWKSStale -> "jwks_stale"
internal/protocol/auth/middleware_test.go   # wire reason "jwks_stale"; protocolErrorFor still CodeAuthRejected/401
internal/protocol/auth/from_config.go   # thread effective ceiling (cfg or default) into NewJWKSKeySet
internal/protocol/auth/from_config_test.go  # default applied when unset; explicit value honored
internal/config/config.go               # IdentityConfig.JWKSMaxStale field + godoc
internal/config/validate.go             # validateIdentity: negative / below-floor rejection
internal/config/validate_test.go        # field validation table
examples/*.yaml                         # production-auth example gains identity.jwks_max_stale (documented default)
test/integration/jwks_max_stale_test.go # E2E: real validator over the wire edge; stale -> 401/jwks_stale; recovery; -race
scripts/smoke/phase-129.sh
docs/plans/phase-129-jwks-max-stale-ceiling.md
docs/decisions.md                       # D-261
docs/glossary.md                        # JWKS max-stale ceiling
docs/plans/README.md                    # Phase 129 row Pending (V1.7) -> Shipped on land + detail block
README.md                               # Status table Phase 129 row on land
docs/skills/*/SKILL.md                  # §18: any skill quoting identity.* config gains the field (grep gate below)
```

No new top-level directory (AGENTS.md §3 unchanged): every touched package
already exists.

## Public API surface

```go
// internal/protocol/auth (jwks.go)

// ErrJWKSStale — the JWKS key snapshot exceeded the configured max-stale
// ceiling without a successful refresh: the runtime can no longer vouch
// for the cached keys (a key the IdP revoked may still be present), so
// validation fails CLOSED rather than serving a possibly-revoked key.
// Distinct from ErrUnknownKey (the kid never resolved). Mapped onto the
// "jwks_stale" wire reason and CodeAuthRejected (HTTP 401).
var ErrJWKSStale = errors.New("auth: JWKS key set too stale (max-stale ceiling exceeded); failing closed")

// WithJWKSMaxStale sets the maximum age a cached key snapshot may reach,
// without a successful refresh, before KeyByID fails closed with
// ErrJWKSStale. A non-positive value keeps the package default
// (defaultJWKSMaxStale). The ceiling bounds — but does not make
// instantaneous — revocation: a revoked key may still be honored until
// the snapshot ages past the ceiling. Pair a tight ceiling with
// overlapping IdP signing keys and short token TTLs.
func WithJWKSMaxStale(d time.Duration) JWKSOption
```

```go
// internal/config (config.go) — IdentityConfig gains one additive field.
type IdentityConfig struct {
    // … existing fields …
    // JWKSMaxStale bounds how long a cached JWKS snapshot is honored
    // without a successful refresh. Past this age the validator fails
    // closed (rejects tokens) rather than serving a possibly-revoked
    // key during a prolonged IdP outage. Zero applies the safe default;
    // a negative or below-floor value is a validation error. This bounds
    // — it does not make instantaneous — revocation.
    JWKSMaxStale time.Duration `yaml:"jwks_max_stale,omitempty"`
}
```

No new Protocol method, error `Code`, wire type, or capability. The
`auth.Validator` interface, the `auth.KeySet` interface, and
`NewJWKSValidator` signatures are unchanged — `ErrJWKSStale` and
`WithJWKSMaxStale` are additive package symbols.

## Test plan

- **Unit (`internal/protocol/auth`):**
  - `jwks_test.go`: with an injected clock + counting HTTP client —
    (a) a fresh snapshot serves the key; (b) refreshes failing + clock past
    the ceiling → `KeyByID` returns `ErrJWKSStale` even for a `kid` present
    in the stale snapshot; (c) a later successful refresh resets
    `fetchedAt` and the key resolves again; (d) the escalated `slog.Error`
    fires at most once per minimum-refresh window (captured via a test
    `slog.Handler`); (e) `maxStale <= 0` (option) keeps the package default
    — the ceiling is never disabled.
  - `auth_test.go`: the **consumer** path — `Validate` returns an error for
    which `errors.Is(err, ErrJWKSStale)` holds (not `ErrUnknownKey`);
    `mapParserError` honors `ErrJWKSStale` ahead of `ErrUnknownKey`.
  - `middleware_test.go`: `reasonForWire(ErrJWKSStale) == "jwks_stale"`;
    `protocolErrorFor(ErrJWKSStale)` is `(CodeAuthRejected, 401)`.
  - `validate_test.go` / `from_config_test.go`: negative and below-floor
    `jwks_max_stale` rejected with the field-naming message; zero applies
    `defaultJWKSMaxStale`; an explicit positive value is threaded through.
- **Integration (`test/integration/jwks_max_stale_test.go`):** a real
  `auth` middleware over the real wire edge (`httptest.Server`) behind a
  JWKS-backed validator with an injected clock + a controllable fetcher.
  A request with a valid token succeeds while fresh; after the clock
  advances past the ceiling with the fetcher failing, the same request is
  rejected `401` with wire `reason: "jwks_stale"`; after the fetcher
  recovers it succeeds again. Identity propagation asserted on the success
  path (verified triple on ctx); failure mode (stale rejection) asserted on
  the wire. Runs under `-race`.
- **Conformance:** N/A — no new Protocol method / error code / event type /
  wire type, so the `internal/protocol/singlesource` checker has nothing
  new to gate (the new symbols are package-internal `auth` errors, not
  Protocol wire codes).
- **Concurrency / leak:** extend the existing JWKSKeySet / Validator
  concurrent-reuse test — N≥100 concurrent `Validate` goroutines against a
  single shared validator driven across fresh → stale → recovered clock
  transitions; assert no data race (the `snapMu` / `refreshMu` discipline
  holds with the added age read), no context bleed (each goroutine's result
  matches the clock state it observed), and goroutine baseline restored
  (no background goroutine is added — the ceiling is checked on the
  caller's path).

## Smoke script additions

`scripts/smoke/phase-129.sh` (uses `scripts/smoke/common.sh` helpers; no
new curl wrappers). Each maps 1:1 to an acceptance criterion:

```bash
# Static: the config field exists with the documented yaml tag.
assert_grep_present 'JWKSMaxStale' "internal/config/config.go" \
  "phase 129: IdentityConfig declares JWKSMaxStale"
assert_grep_present 'jwks_max_stale' "internal/config/config.go" \
  "phase 129: jwks_max_stale yaml tag present"

# Static: the primitive — sentinel + option + ceiling.
assert_grep_present 'ErrJWKSStale' "internal/protocol/auth/jwks.go" \
  "phase 129: ErrJWKSStale sentinel declared"
assert_grep_present 'func WithJWKSMaxStale' "internal/protocol/auth/jwks.go" \
  "phase 129: WithJWKSMaxStale option declared"

# Static: the consumer — Validator surfaces it distinctly + distinct wire reason.
assert_grep_present 'ErrJWKSStale' "internal/protocol/auth/auth.go" \
  "phase 129: keyfunc / mapParserError handle ErrJWKSStale"
assert_grep_present 'jwks_stale' "internal/protocol/auth/middleware.go" \
  "phase 129: reasonForWire emits the jwks_stale wire reason"

# Static: NO wire change — ProtocolVersion untouched, no new error Code.
assert_grep_absent 'ErrJWKSStale' "internal/protocol/errors/errors.go" \
  "phase 129: no new Protocol error Code added (single-source preserved)"

# Build/test gates.
if go test -race ./internal/protocol/auth/... ./internal/config/... >/dev/null 2>&1; then
  ok "phase 129: auth + config unit/clock/concurrency tests pass under -race"
else
  fail "phase 129: go test -race ./internal/protocol/auth/... ./internal/config/... failed"
fi
if go test -race -run TestE2E_JWKSMaxStale ./test/integration/... >/dev/null 2>&1; then
  ok "phase 129: JWKS max-stale E2E passes under -race (stale -> jwks_stale; recovery)"
else
  fail "phase 129: JWKS max-stale E2E failed"
fi

# Live (skips per 404/405/501): a harbor serve boot with an invalid ceiling
# fails non-zero, naming the field. Degrade cleanly on builds without serve.
if ./bin/harbor serve --help >/dev/null 2>&1; then
  if ./bin/harbor serve --config "$BAD_MAXSTALE_CONFIG" >/dev/null 2>&1; then
    fail "phase 129: harbor serve booted with a negative jwks_max_stale (should fail loud)"
  else
    ok "phase 129: harbor serve fails loud on an invalid jwks_max_stale"
  fi
else
  skip "phase 129: harbor serve subcommand absent on this build"
fi
```

The live arm follows the §4.2 degradation convention (skip when the
subcommand is absent); the static + go-test arms are the load-bearing
checks since this phase ships no new endpoint or Protocol method.

## Coverage target

- `internal/protocol/auth`: ≥ 85% (no regression from the Phase 115 floor).
- `internal/config`: no regression below the package's existing target for
  the touched `validateIdentity` path.

## Dependencies

- Phase 115 — production JWT verification (JWKS) + `harbor serve`: ships the
  `JWKSKeySet`, `NewJWKSValidator`, and the `from_config` projection this
  phase extends.
- Phase 55 / 56 — JWT validation core, the `auth.Validator` / `auth.KeySet`
  interfaces, the sentinel-error + `errors.Is` discipline, and
  `identity.Validate` this phase reuses.
- Phase 61 — the Protocol auth middleware (`protocolErrorFor` /
  `reasonForWire`) that maps validator sentinels onto the wire.

## Risks / open questions

- **Behavior change for existing deployments (security improvement,
  documented).** Before this phase, JWKS staleness was unbounded — during a
  prolonged IdP outage tokens kept verifying forever. After, the safe
  default ceiling (`defaultJWKSMaxStale`) means a sufficiently long outage
  starts failing closed. This is a deliberate fail-closed posture
  improvement, not a regression; the default is set **generously** (see
  below) to minimize surprise, and the change is called out in the decision
  + the README/changelog note. There is intentionally no opt-out
  (CLAUDE.md §13 — no identity-downgrading knobs); an operator tunes the
  ceiling, they do not remove it.
- **Default value choice.** `defaultJWKSMaxStale` is proposed at **1 hour**
  — ~12× the 5-minute refresh TTL: tight enough to bound a revoked key to
  ~1h of continued acceptance, loose enough to ride out a transient IdP
  outage without auth flapping. The validation **floor** is the minimum
  refresh interval (1 minute): a ceiling below the smallest possible
  refresh window can never be satisfied. Both values are documented in
  godoc + the example config + the glossary; revisit if operator feedback
  wants a tighter shipped default.
- **Bounds, not instant, revocation — and the rotation guidance.** The
  ceiling caps the *window* in which a revoked key is honored; it does not
  evict a key the instant the IdP revokes it. The plan, the godoc, and the
  glossary all state the operational guidance that makes the bound
  meaningful: the IdP should publish **overlapping** signing keys across a
  rotation (so a normal rotation never trips the ceiling), and deployments
  should pair the ceiling with **short token TTLs** (the token's own `exp`
  is the first-line revocation bound; the ceiling is the backstop for the
  key material).
- **No new Protocol error code — RESOLVED to reuse `CodeAuthRejected`.** A
  dedicated availability-class code (e.g. a 503-flavored
  `key_source_unavailable`) was considered: it would let a client branch on
  "retry later, the runtime's key source is down" vs "your token is bad."
  Rejected for this phase because a new `Code` is a **wire-surface change**
  (single-source `internal/protocol/errors`, manifest regen, generated
  `errors.md` regen, and a deprecation-window consideration per RFC §8) —
  out of scope for a fail-closed hardening phase that must stay
  ProtocolVersion-neutral. The operator-facing distinctness is delivered
  through the Go sentinel (`ErrJWKSStale`), the distinct wire `reason`
  string (`jwks_stale`, a value of an existing free-form field — not a
  manifest change), the escalated `slog.Error`, and the existing
  `auth.rejected` bus event. Recorded in D-261; a future phase may promote
  it to a dedicated code if a client needs to branch on it.
- **Single-flight + ceiling interaction.** The ceiling check sits *after*
  `maybeRefresh()` on the `KeyByID` path, so a request that arrives just as
  the snapshot ages out still gets one bounded refresh attempt before being
  rejected — the ceiling never short-circuits a recovery fetch. The Error
  log is gated to the rate-limited `maybeRefresh` failure path so a flood
  of rejected requests does not flood the logs.
- **`ProtocolVersion` impact: none.** No method, wire type, error code, or
  capability changes; `internal/protocol/types/version.go` is untouched; no
  `make protocol-ts-gen` / `make protocol-docs-gen` regeneration is
  required. The smoke script asserts the manifest + version are unchanged.
- Full §16 brief pass (re-read brief 06 + RFC §5.5) when dispatched.

## Glossary additions

- **JWKS max-stale ceiling** — the maximum age a cached JWKS key snapshot
  may reach, without a successful refresh, before the production validator
  fails closed (rejects tokens) instead of serving a possibly-revoked key.
  Configured via `identity.jwks_max_stale`; enforced by `auth.JWKSKeySet`
  (`ErrJWKSStale` / wire reason `jwks_stale`). Bounds — but does not make
  instantaneous — key revocation; pair with overlapping IdP signing keys
  and short token TTLs. Add to `docs/glossary.md` in the same PR.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes (no AGENTS.md/CLAUDE.md edits — N/A)
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: cross-session isolation test passes
      — N/A (the ceiling is identity-agnostic key-material lifecycle; it
      gates BEFORE a verified identity exists and adds no identity-scoped
      storage path; the integration test still asserts identity propagation
      on the success path)
- [ ] **Concurrent-reuse test passes — N≥100 against a single shared
      validator under `-race`** (extended JWKSKeySet / Validator test
      across fresh → stale → recovered transitions)
- [ ] **Integration test exists, real validator over the wire edge,
      identity propagation on success, ≥1 failure mode (stale rejection),
      `-race`** (`test/integration/jwks_max_stale_test.go`)
- [ ] §18 skill check: `grep -rl 'identity\.' docs/skills/` — any skill
      quoting the `identity.*` config block gains `jwks_max_stale` in this
      PR (or record exemption if none quotes the block)
- [ ] If new vocabulary: glossary updated (`JWKS max-stale ceiling`)
- [ ] No new Protocol method / type / error code / capability;
      `ProtocolVersion` unchanged; no manifest / generated-docs regen
- [ ] If a brief finding was departed from: justified above + decisions.md
      entry — N/A, no brief departure; D-261 records the design decisions

---

## Implementation handoff

Turnkey artifacts for the implementing agent. Operate only inside your
worktree (`pwd` first; STOP if a path resolves outside it). Run
`markdownlint-cli2` repo-wide before committing (blank lines around `---`
and `## D-NNN` headings in `docs/decisions.md`).

### (a) Master-plan `docs/plans/README.md` index row

Append (the table is sorted by phase number; this row sorts after 128):

```text
|129 | JWKS max-stale / revocation ceiling (bound how long the production JWKS validator honors a cached key when refreshes fail; `identity.jwks_max_stale` + a fail-closed ceiling in `auth.JWKSKeySet` returning a distinct `ErrJWKSStale` / `jwks_stale` reason instead of serving a possibly-revoked key indefinitely; the Validator is the same-phase consumer, proven by a controllable-clock test; NO new method/type/code, ProtocolVersion untouched, D-261) | internal/protocol/auth + internal/config | §5.5 | 115, 55, 56, 61 | 85% | Pending (V1.7) |
```

### (b) `docs/decisions.md` entry (markdownlint-clean — note the blank lines)

Append at end of file:

```markdown

---

## D-261 — JWKS max-stale / revocation ceiling: fail closed instead of serving a possibly-revoked key forever

**Date:** 2026-06-25

**Status:** Accepted (planning)

**Context.** The production JWKS-backed `auth.JWKSKeySet` (Phase 115)
prefers availability over revocation: when a refresh fetch fails (IdP
unreachable or returning malformed data) it retains and serves the prior
key snapshot, and there is no upper bound on how long it does so. A key the
IdP has rotated out / revoked therefore stays accepted until the next
*successful* fetch — indefinitely during a prolonged outage. The keyset's
own godoc names this as a known gap. For any external-issuer deployment this
is a real security weakness: revocation never takes effect while the IdP is
unreachable.

**Decision.**

1. **A configurable max-stale ceiling on the keyset (the primitive).**
   `auth.JWKSKeySet` gains an immutable `maxStale` (set via
   `WithJWKSMaxStale`, defaulting to `defaultJWKSMaxStale` = 1h). When the
   cached snapshot's age (time since the last *successful* fetch) reaches
   the ceiling after a bounded refresh attempt, `KeyByID` fails closed with
   a wrapped `ErrJWKSStale` regardless of whether the `kid` resolves — a
   possibly-revoked key in a too-old snapshot is never served. The ceiling
   check sits after `maybeRefresh()` so a recovery fetch is always attempted
   first.
2. **The Validator is the same-phase consumer (§13).** A primitive with no
   consumer bit-rots; the consumer here is the `Validator` keyfunc, which
   propagates `ErrJWKSStale` distinctly (not masked as `ErrUnknownKey`);
   `mapParserError` honors it ahead of `ErrUnknownKey`; and
   `middleware.reasonForWire` emits `"jwks_stale"`. Proven end-to-end by a
   controllable-clock test (no `time.Sleep`): a key served past the ceiling
   with failing refreshes is rejected, and a successful refresh resets
   staleness so the same token verifies again.
3. **Fail loud, distinctly.** Operators see *"JWKS too stale"*, not a
   generic auth failure: the Go sentinel `ErrJWKSStale`, the wire `reason`
   `jwks_stale`, an escalated `slog.Error` on the rate-bounded refresh
   failure path, and the existing `auth.rejected` audit + bus emit carrying
   the reason (brief 06 §1/§8 — rejections surface on the canonical bus, not
   a side channel). No raw token is logged.
4. **A new config field, fail-closed by default.** `IdentityConfig` gains
   `JWKSMaxStale time.Duration` (`identity.jwks_max_stale`). Validation
   rejects negative and below-floor (< 1m) values; zero applies the safe
   default. There is intentionally **no opt-out** — the field tunes the
   ceiling, it does not remove it (CLAUDE.md §13: no identity-downgrading
   knobs). Existing deployments thus gain a bounded default where they
   previously had unbounded staleness: a deliberate security-posture
   improvement, documented in the README/changelog.
5. **No wire change, no version bump.** A dedicated availability-class
   Protocol error code (e.g. `key_source_unavailable`) was considered and
   **deferred** — a new `Code` is a wire-surface change (single-source
   errors, manifest + generated-docs regen, RFC §8 deprecation window).
   This phase reuses `CodeAuthRejected` (HTTP 401) and delivers operator
   distinctness through the sentinel + the free-form wire `reason` value +
   logs/events. `internal/protocol/types/version.go` `ProtocolVersion` is
   untouched; no `make protocol-ts-gen` / `make protocol-docs-gen` regen.

**Operational note (binding on the docs).** The ceiling **bounds** — it
does not make instantaneous — revocation. The IdP should publish
overlapping signing keys across a rotation (so a normal rotation never trips
the ceiling), and deployments should pair the ceiling with short token TTLs
(the token's `exp` is the first-line revocation bound; the ceiling is the
key-material backstop).

**§4.3 deviations.** Departs from the *current code's* documented "no
max-stale ceiling" behavior (a deliberate gap-closure, not a brief
departure). The dedicated availability error code is the only deferred
sub-decision.

**Cross-references.** Phase 115 (the JWKS keyset + `from_config` projection
this extends), Phase 55/56 (JWT core + sentinel/`errors.Is` discipline),
Phase 61 (Protocol auth middleware mapping), D-025 (concurrent-reuse
contract for the shared keyset/validator). RFC §5.5. brief 06. Plan:
`docs/plans/phase-129-jwks-max-stale-ceiling.md`.
```

### (c) `scripts/smoke/phase-129.sh` assertions to add

See the "Smoke script additions" section above — the script copies
`scripts/smoke/_template.sh`, sources `common.sh`, and adds the static
(`assert_grep_present` / `assert_grep_absent`), build/test (`go test -race`
on `internal/protocol/auth`, `internal/config`, and the integration
`TestE2E_JWKSMaxStale`), and the degradation-gated live `harbor serve`
boot-fail assertions. FAIL must be 0; the live arm SKIPs on builds without
`harbor serve`.

### (d) Master-plan per-phase detail-block stub

Add under the detail section of `docs/plans/README.md` (house format —
mirror the 122 / 127 blocks):

```markdown
### Phase 129 — JWKS max-stale / revocation ceiling

- **Subsystem:** internal/protocol/auth (the JWKSKeySet ceiling + the
  Validator consumer + the wire-reason arm) + internal/config (the
  `identity.jwks_max_stale` field + validation).
- **RFC:** §5.5 (Authentication — the verified-identity edge; the ceiling
  is the fail-closed backstop when the key source is unreachable past the
  bound).
- **Deps:** 115 (production JWKS keyset + `harbor serve` + from_config), 55
  / 56 (JWT core + Validator/KeySet interfaces), 61 (Protocol auth
  middleware mapping).
- **What it delivers:** a configurable max-stale ceiling on
  `auth.JWKSKeySet` — past the ceiling, with refreshes failing, `KeyByID`
  fails closed with a distinct `ErrJWKSStale` instead of serving a
  possibly-revoked key indefinitely; the Validator (same-phase consumer)
  surfaces it as the `jwks_stale` wire reason; a new `identity.jwks_max_stale`
  config field with a documented safe default (1h) + fail-loud validation;
  loud, rate-bounded operator signal via an escalated `slog.Error` + the
  existing `auth.rejected` audit/bus emit. Proven by a controllable-clock
  test (stale → rejected; successful refresh → reset). Bounds — not makes
  instantaneous — revocation; rotation guidance documented.
- **Wire impact:** NONE — no new method / type / error code / capability;
  reuses `CodeAuthRejected`; `ProtocolVersion` untouched; no manifest /
  generated-docs regen.
- **Decision:** D-261.
- **Status:** Pending (V1.7).
```
