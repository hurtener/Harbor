# Phase 85m — mcp-rc-2026-07-28

<!--
STUB — authored 2026-05-28 alongside D-168 (the 85-band RC re-plan).

This phase absorbs the cross-cutting breaking changes the MCP 2026-07-28
release candidate introduces. The RC was locked 2026-05-21; final spec
drops 2026-07-28; Tier-1 SDKs ship RC support within a ~10-week window.

Status: **Revisit after SDK-RC** (≈ late Jul–Aug 2026). The plan can be
fleshed out NOW against the published SEPs so implementation can dispatch
the day the go-sdk RC lands. Several acceptance criteria below are placeholders
that need to be tightened once the SDK exposes the relevant types — those are
marked `(TBD: SDK)`.

RC blog post (informational, not in `docs/research/`):
  https://blog.modelcontextprotocol.io/posts/2026-07-28-release-candidate/
-->

## Summary

Adopt the cross-cutting breaking changes introduced by the MCP 2026-07-28 release candidate that don't fit any other 85-band phase: remove the `initialize`/`initialized` handshake and `Mcp-Session-Id` dependence, add the new `Mcp-Method` / `Mcp-Name` headers on Streamable HTTP, flip the `-32002` → `-32602` error code, restructure server-to-client requests around `InputRequiredResult`, raise tool / resource-template schema validation to JSON Schema 2020-12, respect the new `ttlMs` / `cacheScope` cache directives on list/resource reads, and propagate W3C Trace Context (`traceparent` / `tracestate` / `baggage`) through `_meta`. These changes touch every MCP transport and every wire shape; bundling them into one phase prevents the band from absorbing the breaking changes piecemeal across 85a / 85b / 85f.

## RFC anchor

- RFC §6.4

## Briefs informing this phase

- brief 14
- brief 03

## Brief findings incorporated

- brief 14 §2 (the capability matrix): the matrix entries this phase touches are protocol-baseline (sessions, headers, errors) rather than capability-specific, but the matrix-discipline approach — "every row in the matrix gets a wired ↔ unwired classification" — extends to the RC's new baseline.
- brief 14 §4 ("biggest gaps"): the RC's session-elimination + error-code flip + transport-header additions are protocol-baseline changes that didn't exist as gaps when brief 14 was written. This phase treats them with the same rigour brief 14 applied to the capability-level gaps — fail loudly when the wire shape is wrong, never silently degrade.
- brief 14 §4 (compliance-statement wording): "never write 'fully MCP compliant' unscoped" — the scoped statement 85j substantiates targets the RC, not 2025-11-25, after this phase lands.
- brief 03 (LLM client): unaffected directly; the trace-context propagation reuses Harbor's existing `internal/telemetry/propagation.go` rather than introducing a parallel trace surface.

## Findings I'm departing from (if any)

- None. The departure that motivates the whole 85-band re-plan (D-168) is from brief 14's prioritisation of sampling / roots / Tasks — those departures live in the cut phases' decisions, not here. This phase implements baseline RC requirements with no brief departures.

## Goals

- The MCP driver speaks the 2026-07-28 RC wire shape end-to-end: no session handshake, no `Mcp-Session-Id` keying, new transport headers, flipped error code, restructured server-to-client requests, JSON Schema 2020-12, cache directives, W3C trace propagation.
- Identity and per-request `_meta` carry the client info that the eliminated session used to carry — without re-introducing session-scoped state.
- Cache directives are honoured per the spec's `ttlMs` / `cacheScope` semantics; an operator-visible knob lets the cache be disabled (`cacheScope: none`) for debugging.
- W3C Trace Context propagates through `_meta` on every MCP request and response; Harbor's existing OTel propagator is the source.
- The phase composes cleanly with 85b (auth) and 85d (elicitation): 85b's token keying picks up the per-request `_meta` shape; 85d's rewrite picks up the new `InputRequiredResult` round-trip mechanic.

## Non-goals

- The deprecated capabilities (sampling, roots, logging) — those are cut per D-168; this phase does not retract their wire-level recognition, only ensures the baseline below them is RC-correct.
- The Tasks extension — Tasks is a separate future band per D-168; this phase does not implement any `tasks/*` method.
- The six auth-hardening SEPs — those land in 85b (scope ↑) not here.
- The `InputRequiredResult` flow for elicitation specifically — the wire mechanic lands here; 85d's rewrite consumes it.

## Acceptance criteria

- [ ] No code path in `internal/tools/drivers/mcp` issues or expects `initialize` / `initialized` JSON-RPC methods; no code path reads, writes, or matches on `Mcp-Session-Id`. Grep-level test asserts both.
- [ ] Every outbound Streamable HTTP request sets `Mcp-Method` and `Mcp-Name` headers matching the request body; a mock server asserting reject-on-mismatch is part of the integration suite.
- [ ] All `-32002` callsites flip to `-32602`; an error-mapping test asserts a resource-not-found response surfaces as `ErrResourceNotFound` regardless of whether the server emits the old code (back-compat for legacy servers) or the new code (RC-conformant servers).
- [ ] Server-to-client request restructuring: server-initiated requests are only accepted while a client request is actively in flight; an out-of-band server request is rejected loudly. The `InputRequiredResult` payload type + the client-side retry-with-`inputResponses` mechanic are implemented as a reusable primitive 85d's rewrite consumes. (TBD: SDK — the exact Go types depend on what go-sdk RC exposes.)
- [ ] Tool input/output schemas and resource-template schemas validate against JSON Schema **2020-12** (composition, conditionals, `$ref`); a schema using a 2020-12-only feature (e.g. `if`/`then`/`else`) passes validation. The schema validator may be a vendored library or a `gojsonschema`-style dependency; selection documented in the PR.
- [ ] `ttlMs` and `cacheScope` on `*/list` and resource-read responses are respected; a list response with `ttlMs: 30000` does not re-fetch within 30s; `cacheScope: none` disables caching for that response. Cross-isolation: cache is keyed by the identity triple + the server resource, never globally.
- [ ] W3C Trace Context (`traceparent` / `tracestate` / `baggage`) is set on every outbound MCP `_meta` and parsed from every inbound `_meta`; the existing `internal/telemetry/propagation.go` is the source, not a parallel implementation.
- [ ] Identity-scoped: every change above respects the `(tenant, user, session)` triple; a two-identity concurrent test asserts no cross-talk on cache, trace, or client-info propagation.
- [ ] Concurrent-reuse: N≥100 concurrent invokes against a single shared MCP provider under `-race` with no header-construction race, no cache-keying bleed, no trace-context cross-talk.

## Files added or changed

- `internal/tools/drivers/mcp/` — remove handshake plumbing; add `Mcp-Method` / `Mcp-Name` header construction; flip error-code mapping; add the `InputRequiredResult` round-trip primitive; add schema validator upgrade; add cache directive honour; wire W3C trace propagation.
- `internal/tools/drivers/mcp/cache.go` (new) — identity-scoped list/resource response cache honouring `ttlMs` / `cacheScope`.
- `internal/telemetry/propagation.go` — consumed (no change expected); if a small extension is needed for MCP's `_meta` injection shape, that is in-scope.
- `internal/config/config.go` — operator knob for cache (enable/disable, max-ttl ceiling).
- Test files — mock servers for each acceptance criterion; two-identity isolation fixtures; concurrent-reuse stress.
- `examples/harbor.yaml` — document the cache config and any operator-visible RC opt-outs.
- `scripts/smoke/phase-85m.sh`.
- `docs/decisions.md` — entry on schema-validator library selection (filed at implementation time).
- `docs/plans/README.md` — Status flip on merge.

## Public API surface

```go
// internal/tools/drivers/mcp (delta — illustrative; finalised against go-sdk RC)

// InputRequiredResult is the RC-shaped restructured server-to-client request:
// the server returns a payload describing what it needs; the client retries
// the original call with inputResponses populated.
type InputRequiredResult struct {
    InputRequests []InputRequest
    RequestState  string // echoed by the client on retry
}

// ResourceCacheDirective carries ttlMs / cacheScope from RC list/read responses.
type ResourceCacheDirective struct {
    TTL   *time.Duration // nil = no expiry directive
    Scope string         // "session", "user", "tenant", "none"
}
```

The exact Go types are TBD pending go-sdk RC. No exported MCP-driver surface changes are expected beyond these two (and the cache + trace plumbing, which is package-internal).

## Test plan

- **Unit:** header-construction (`Mcp-Method` / `Mcp-Name`) round-trip; error-code mapping (both legacy `-32002` and RC `-32602` → `ErrResourceNotFound`); cache directive honour (TTL expiry, cacheScope=none disables, identity keying); schema validator on 2020-12-only constructs; W3C trace propagation injection + extraction.
- **Integration:** mock MCP server speaking the RC wire shape (no handshake, new headers, RC errors, `InputRequiredResult` for elicitation); identity propagation; real `internal/telemetry/propagation.go`; `-race`.
- **Conformance:** N/A — 85j is the conformance harness this phase feeds.
- **Concurrency / leak:** N≥100 concurrent invokes against a shared provider; two-identity isolation on cache, trace, client-info; goroutine-leak baseline after Close.
- **Failure modes:** server rejects on `Mcp-Method` mismatch; out-of-band server-initiated request (rejected); cache TTL boundary; trace-context malformed inbound.

## Smoke script additions

- `scripts/smoke/phase-85m.sh` (classification: `static-only`):
  - Assert `internal/tools/drivers/mcp/` no longer references `initialize` / `Mcp-Session-Id` (grep both must return zero hits — back-compat for legacy `-32002` recognition is fine and does not violate this).
  - Assert the package references `Mcp-Method` / `Mcp-Name` (transport headers).
  - Assert the package references `traceparent` (W3C trace propagation).
  - Assert `internal/tools/drivers/mcp/cache.go` exists.

## Coverage target

- `internal/tools/drivers/mcp`: 85% (band-wide target; this phase must not drop it).

## Dependencies

- 28 (MCP southbound driver — the code this phase rewrites).
- 85a (the foundation: pagination + list-changed handlers + unsubscribe must be sound before the wire-shape rewrite).
- go-sdk RC support (external dependency) — implementation cannot start until the SDK exposes the RC types.

## Risks / open questions

- **SDK timing.** Implementation gated on go-sdk RC support landing (≈ late Jul–Aug 2026). The plan can be authored against the published SEPs now so dispatch is one-step the day the SDK lands; until then, the AC list above is best-effort against the RC blog + SEP texts and will be tightened against the SDK's actual Go types.
- **Schema validator selection.** JSON Schema 2020-12 support in Go is fragmented — `gojsonschema` covers older drafts, `santhosh-tekuri/jsonschema` is the leading 2020-12 implementation. Selection lands in the PR with a one-line rationale and a `docs/decisions.md` entry.
- **Legacy server back-compat.** Harbor will encounter 2025-11-25-conformant servers for years; the error-code mapping must accept both `-32002` and `-32602` for resource-not-found, and the absence of `Mcp-Method` / `Mcp-Name` on a legacy server's response is not an error (only the request side ships them). The integration suite exercises both eras.
- **Cache invalidation.** The RC's `ttlMs` is a server hint, not a guarantee. A `list_changed` notification (Phase 85a) must invalidate the corresponding cache entry; the wiring is tested explicitly.
- **Trace propagation under server-initiated requests.** When a server initiates a request mid-client-request, the trace context's parent span is the client's in-flight request, not a new root. The trace plumbing handles this; the integration test asserts the span tree.

## Glossary additions

- **MCP per-request `_meta`** — the RC replaces the eliminated session handshake's per-connection client info with per-request `_meta` fields. Harbor reads/writes `_meta` on every MCP request and response.
- **MCP cache directive** — `ttlMs` + `cacheScope` fields on RC list/resource-read responses; the client respects them for the directive's lifetime, identity-scoped.
- **`InputRequiredResult`** — the RC's restructured server-to-client request payload. The server returns it instead of a streamed request; the client retries the original call with `inputResponses` populated.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] Cross-isolation test passes — identity-scoped cache, trace, client-info
- [ ] **Concurrent-reuse test passes** — N≥100 concurrent invokes against a single shared MCP provider under `-race`
- [ ] **Integration test passes** — mock RC server, two-identity isolation, real telemetry propagator, ≥1 failure mode
- [ ] Glossary updated (`MCP per-request _meta`, `MCP cache directive`, `InputRequiredResult`)
- [ ] No brief departures
- [ ] D-NNN entry filed for the schema-validator library selection
