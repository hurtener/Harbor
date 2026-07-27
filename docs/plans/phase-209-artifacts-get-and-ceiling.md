# Phase 209 — `artifacts.get`, operator fetch ceiling, byte-offset windowing

## Summary

The Harbor Protocol gains its fifth artifact method, `artifacts.get`, at `POST /v1/control/artifacts.get`. It resolves through `ArtifactStore.Get` — a mandatory interface method — so every registered driver serves it, closing the gap where the only shipped byte path (`artifacts.get_ref`) type-asserts the optional `artifacts.Presigner` capability that exactly one of five drivers implements and the default driver does not. The response is truthful about its own bound (`total_size_bytes` / `returned_bytes` / `truncated`), `artifact_fetch` gains `offset` and answers the same field set, and the fetch ceiling stops being a compile-time constant: `ArtifactsConfig` gains `fetch_default_max_bytes` and `fetch_hard_max_bytes` as additive fields with the current constants as defaults.

## RFC anchor

- RFC §5.2 — the Protocol's advertised artifact surface, which has named `artifacts.get` from the outset.
- RFC §6.5 — the context-window safety net this gives a coherent read side.
- RFC §6.10 — Artifacts: the read side (byte-serving method, byte-offset windows, the operator-owned ceiling).
- RFC §10 — configuration.

## Briefs informing this phase

- brief 05
- brief 07

## Brief findings incorporated

- **brief 05 §3 (Protocol → SessionRegistry, TaskRegistry, StateStore)** lists the artifact surface as `artifacts.list / get / get_ref / delete` (scope-checked). `artifacts.get` has been in the brief's surface list since the beginning and has never existed in code; this phase lands it under exactly that scope-checked posture rather than inventing a shape.
- **brief 05 §1 (Mandatory artifacts policy)**: "A `NoOpArtifactStore` fallback that silently warns and truncates is an anti-pattern… the in-memory driver is the floor." The read side inherits the same rule read forward: a byte path that works only on a presigning driver is the read-side analogue of a floor that does not hold, so this method resolves through the mandatory `Get` and the smoke asserts it on `inmem` specifically.
- **brief 05 §7 (Cross-tenant isolation)**: "Storing an artifact under tenant A and attempting to read under tenant B fails." Pinned twice here — at the handler (`TestArtifactsGetHandler_CrossTenantIsNotFoundOrRefused`) and over the real wire (`TestE2E_ArtifactsGet_IdentityPropagatesAndIsolates`), with the additional property that a foreign id and an unknown id answer IDENTICALLY, because the difference between them is what a prober harvests.
- **brief 05 §7 (Concurrency tests)**: "N concurrent sessions × M concurrent tasks each, asserting no cross-talk in events, memory, artifacts, or task results." The D-025 run here is N=128 concurrent reads against one shared surface, each in its own tenant, asserting content AND the reported bound — because a per-call bound held on the surface instead of read from the request would bleed in exactly the second place.
- **brief 07 §4 (`ObservationRenderer`)**: the renderer "performs LLM-facing redaction (heavy outputs replaced with artifact refs)." That is the write half; `artifact_fetch` is the recovery path the model uses on the ref, and giving it `offset` is what makes a ref the model can actually work through rather than only peek at.

## Findings I'm departing from (if any)

Two, both departures from D-347's stated detail rather than from a brief. Both are recorded in D-353 as well.

1. **The response carries `truncated` and NOT `eof`.** D-347 part 3 names the `artifact_fetch` field set as `{content, offset, returned_bytes, total_size_bytes, eof}` while part 1 names `artifacts.get`'s as `total_size_bytes` / `returned_bytes` / `truncated`. `eof` and `truncated` are exact complements — `eof == !truncated` for every window — so shipping both is two signals for one fact, which is the shape the same entry's own "ONE field set… rather than a signal per source" instruction rules out and which §13 names as two parallel implementations of one conceptual feature. The master plan's Phase 209 detail block names only the three, three times, and it is the more recent artifact. `truncated` is kept over `eof` because it is the field `artifact_fetch` already shipped, so no already-deployed prompt or transcript is invalidated.
2. **The config fields validate as `>= 0`, not "positive".** D-347 part 4 says "each positive". Rejecting zero would make a zero-value `config.ArtifactsConfig` — which is exactly what an operator's existing YAML unmarshals into for a key it does not mention — fail validation, turning an additive field into a breaking one and violating the same part's own "so existing configs are unchanged (§10)". Zero means "unset" and resolves to the documented built-in through `ResolvedFetchDefaultMaxBytes` / `ResolvedFetchHardMaxBytes`, matching the sibling `ProtocolConfig.ResolvedMaxRequestBytes` and `HeavyOutputThresholdBytes` precedents. A NEGATIVE value is refused by name. The "each positive" property holds where it matters — at the consumer, where `NewArtifactsSurface` refuses a non-positive resolved bound outright — and the `default ≤ hard` comparison runs on the resolved values, because a default above a configured ceiling is the same misconfiguration whether the operator wrote the default or inherited it.

## Goals

- Land `artifacts.get` as the fifth canonical artifact Protocol method, served by every registered driver.
- Make every artifact read truthful about its own bound through ONE field set shared by the caller's request, the operator's default and the operator's ceiling.
- Give `artifact_fetch` byte-offset windowing so a model can page a large artifact instead of only reading its head.
- Move the fetch ceiling from two compile-time constants to operator policy, additively.
- Retire the four wire-type godoc forward references and the glossary claim that describe `artifacts.get` as already shipped.

## Non-goals

- **No row-, line- or schema-addressed windowing, and no MIME-keyed read behaviour of any kind.** Dropped, not deferred with a design — the two reopening prerequisites are recorded verbatim in D-353 so the next author does not re-derive them.
- **No ranged read on `ArtifactStore`.** `Get` returns whole bytes and every driver materialises the blob, so a window at offset N costs a full materialisation. Making the read cheap is a five-driver conformance change with its own phase; this phase states the cost property rather than implying an incremental one, and puts no capability claim in interface godoc.
- **No third-party byte egress.** No grant URLs, no grant tokens, no externally-reachable base-URL config, no `ServerConfig` change. That arm is deferred on three named blockers that are not this phase's to answer.
- **No `ProtocolVersion` bump.** Additive method + additive wire type; the D-223 and D-209 gates regenerate to a clean diff.
- **No new error code.** The method answers with the existing `not_found` / `scope_mismatch` / `identity_required` / `invalid_request` codes.
- **No `Supports*` capability ceremony.** `Presigner` remains the single documented exception it already is, and `artifacts.get` is mandatory on every driver precisely so no second optional capability is needed.
- **No Console page change.** The typed client gains the method and the wire types (§4.5 item 5); routing a Console read through it is the consuming page's own work.

## Acceptance criteria

- [x] `artifacts.get` is a canonical Protocol method in both closed method sets, routed by the control transport, with `IsArtifactsMethod` covering it.
- [x] `handleGet` resolves through `ArtifactStore.Get` and returns the stored bytes on the default `inmem` driver — a store on which `artifacts.get_ref` answers `CodePresignUnsupported`.
- [x] The response carries the ref metadata plus `content`, `offset`, `returned_bytes`, `total_size_bytes` and `truncated`.
- [x] `truncated` is computed from `offset + returned_bytes < total_size_bytes`, so the LAST window of an artifact reports `false`.
- [x] A request above the effective ceiling is SERVED at the ceiling and reports it through the same fields — never refused, never silent.
- [x] A negative `offset` or `max_bytes` is refused with `invalid_request`.
- [x] A foreign-tenant scope is refused flat with `scope_mismatch`, under both admin-tier claims, before the store is consulted.
- [x] A foreign id and an unknown id answer identically (`not_found`), so the refusal does not reveal existence.
- [x] `artifact_fetch` accepts `offset` and answers `offset` / `returned_bytes` / `total_size_bytes` / `truncated`.
- [x] `ArtifactsConfig` gains `fetch_default_max_bytes` (64 KiB) and `fetch_hard_max_bytes` (1 MiB), validated in the config validator, with an existing config unchanged.
- [x] The tool and the Protocol surface resolve their bounds from the SAME operator configuration.
- [x] `bodyscope` registers the new request type on the flat content-read row; D-349's bidirectional coverage scan is green.
- [x] `make protocol-ts-gen`, `make protocol-docs-gen` and `make protocol-ts-types-gen` regenerate; the Console typed client mirrors the shape by hand; `ProtocolVersion` does not move.
- [x] The four wire-type godoc forward references and the glossary claim are reconciled with what now ships.
- [x] `scripts/smoke/phase-209.sh` passes against the preflight dev server with OK > 0 and FAIL = 0.

## Files added or changed

```text
internal/protocol/
├── transports/control/artifacts_body_scope_test.go  # NEW — §17.6: pins all
│                                   #   five methods' body-scope row selection
├── methods/methods.go              # MethodArtifactsGet + both closed sets
├── types/artifacts.go              # ArtifactsGetRequest / ArtifactsGetResponse
│                                   #   + the package godoc's "where bytes travel"
├── artifacts.go                    # handleGet, effectiveMaxBytes, boundedWindow
│                                   #   + the two fetch-bound deps
├── artifacts_get_test.go           # NEW — the handler suite + D-025 N=128
├── bodyscope/coverage.go           # ArtifactsGetRequest -> SurfaceArtifactsRef
├── bodyscope/registry.go           # the flat content-read row's widened reason
├── transports/control/artifacts_handler.go  # decode + surface selection
├── client/client.go                # RuntimeClient.ArtifactsGet
├── conformance/conformance.go      # the method matrix count
├── singlesource/singlesource.go    # the canonical method + the two wire types
└── types/{memory,flows,search,pause}.go  # the four forward references retired

internal/config/
├── config.go     # the two fields + the two Default* constants + Resolved*
├── loader.go     # Defaults() seeds both
└── validate.go   # >= 0 each, resolved default <= resolved hard

internal/tools/builtin/
├── artifact_fetch.go       # offset, the truthful field set, fetchBounds
├── artifact_fetch_test.go  # the windowing + ceiling suite
└── builtin.go              # RegistryContext carries the operator bound

internal/runtime/
├── serve/mux.go            # resolves the bound onto ArtifactsDeps
└── assemble/assemble.go    # resolves the bound onto RegistryContext

cmd/
├── harbor-gen-protocol-docs/{methods,typeindex}.go
├── harbor-protocol-ts-lockstep/typeindex.go
└── harbor-protocol-ts-types/typeindex.go

web/console/src/lib/
├── protocol/artifacts.ts   # the hand-mirrored wire types
├── protocol/client.ts      # artifacts.get on the typed namespace
├── protocol.ts             # the re-export
└── protocol/wire-manifest.gen.json  # REGENERATED

test/integration/artifacts_get_test.go   # NEW — two real drivers over the wire
scripts/smoke/phase-209.sh               # the live gate
docs/{CONFIG.md, glossary.md, decisions.md, plans/README.md}
docs/site/protocol/{methods,types}.md    # REGENERATED
docs/skills/use-the-harbor-protocol/SKILL.md
examples/{harbor,dev,serve}.yaml
examples/protocol-clients/event-viewer-ts/harbor-protocol.gen.ts  # REGENERATED
```

## Public API surface

```go
// internal/protocol/methods
const MethodArtifactsGet Method = "artifacts.get"

// internal/protocol/types
type ArtifactsGetRequest struct {
    Scope    ArtifactScope `json:"scope"`
    ID       string        `json:"id"`
    Offset   int64         `json:"offset,omitempty"`
    MaxBytes int64         `json:"max_bytes,omitempty"`
}

type ArtifactsGetResponse struct {
    Ref             ArtifactRef `json:"ref"`
    Content         []byte      `json:"content,omitempty"`
    Offset          int64       `json:"offset"`
    ReturnedBytes   int64       `json:"returned_bytes"`
    TotalSizeBytes  int64       `json:"total_size_bytes"`
    Truncated       bool        `json:"truncated"`
    ProtocolVersion string      `json:"protocol_version"`
}

// internal/protocol
type ArtifactsDeps struct {
    // ... existing fields ...
    FetchDefaultMaxBytes int // mandatory, positive
    FetchHardMaxBytes    int // mandatory, positive, >= FetchDefaultMaxBytes
}

// internal/protocol/client
type RuntimeClient interface {
    // ... existing methods ...
    ArtifactsGet(context.Context, types.ArtifactsGetRequest) (types.ArtifactsGetResponse, error)
}

// internal/config
const DefaultArtifactFetchMaxBytes     = 64 * 1024
const DefaultArtifactFetchHardMaxBytes = 1 * 1024 * 1024

func (c ArtifactsConfig) ResolvedFetchDefaultMaxBytes() int
func (c ArtifactsConfig) ResolvedFetchHardMaxBytes() int

// internal/tools/builtin
type RegistryContext struct {
    // ... existing fields ...
    ArtifactFetchDefaultMaxBytes int // optional; non-positive takes the built-in
    ArtifactFetchHardMaxBytes    int // optional; non-positive takes the built-in
}
```

## Test plan

- **Unit** (`internal/protocol/artifacts_get_test.go`): the round trip on the default driver with the get_ref refusal asserted alongside it; the truthful bound; the seven-case offset window table including the last window and both past-the-end shapes; a paging loop that reassembles the artifact from its own reported offsets; the ceiling served-not-refused; the operator default applied; a caller bound below the ceiling honoured; the malformed-request table (three identity shapes, missing id, negative offset, negative max_bytes, nil request, wrong request type); the constructor's refusal of an incoherent bound; and the no-aliasing property (mutating a response must not corrupt the store).
- **Unit** (`internal/tools/builtin/artifact_fetch_test.go`): the same offset table and paging loop against the builtin; the ceiling and operator-default behaviour; the negative-offset soft error; the `resolveFetchBounds` normalisation table including the clamp-down case; and a by-value assertion that the tool's fallbacks ARE the config package's documented defaults.
- **Integration** (`test/integration/artifacts_get_test.go`): the real control transport over `httptest`, against TWO real drivers (`inmem` and `fs`) NEITHER of which presigns — the round trip, the get_ref refusal on the same store, the bounded read, the offset window and the last window. Identity propagation and isolation: a foreign id answers not-found, an unknown id answers identically, a foreign-tenant body is refused 403 under both admin-tier claims. **Failure mode:** a request that establishes no identity at all is refused 401 rather than falling back to the body.
- **§17.6 fix, bundled not deferred** (`internal/protocol/transports/control/artifacts_body_scope_test.go`): the mutation sweep found that dropping ANY arm of `reconcileArtifactsIdentity`'s method → `bodyscope` row switch broke ZERO tests. A content read that lost its arm fell through to the admin-elevatable default, silently widening it, and the surface's own tenant check covered for the transport so a live smoke stayed green — the inert-guard shape §4.2 item 5 names. The new test pins all FIVE arms by driving a cross-tenant body under the admin claim: the three admin-scoped rows must grant, the two content rows must refuse `scope_mismatch` anyway. Verified to fail on three separate mutations (`artifacts.get`, `artifacts.get_ref`, `artifacts.put` each losing their row), each in exactly the right sub-test. This closes the gap for the whole cluster, not only for the method this phase adds.
- **Conformance:** the Protocol method matrix (`internal/protocol/conformance`) covers the new method by construction — it iterates `methods.Methods()` and fails on an uncovered entry.
- **Concurrency / leak:** `TestArtifactsGetHandler_ConcurrentReuse_NoCrossTalk` — N=128 concurrent reads against ONE shared surface under `-race`, each goroutine in its own tenant, with a per-goroutine offset and bound, asserting no content bleed AND no bound bleed. `TestArtifactsGetHandler_CancellationDoesNotCrossTalk` for the cancellation guarantee. `TestE2E_ArtifactsGet_ConcurrentAcrossTenantsOverTheWire` — N=32 across the transport boundary, all 32 tenants against ONE shared server and handler. The surface holds no goroutines of its own, so the leak guarantee is the existing `internal/protocol/goroutine_settle_test.go`'s.

  **The wire stress shares one server, and that is the design rather than a saving.** The first draft stood up one `httptest.Server` per tenant. Two things were wrong with it. It made the stress WEAKER — N tenants against N handler instances proves nothing about isolation, whereas N tenants sharing ONE handler and ONE surface is the shape a cross-talk bug actually appears in. And it made the package flaky: 32 simultaneous listeners and accept loops starved a sibling PTY test (`TestE2E_TUIConversationPTY_KeyDrivenAuthenticatedWorkflow`, whose own godoc documents a 20s render budget chosen for loaded runners) past its budget. **Measured, not assumed:** the base commit ran the package green 3/3; the first draft failed 2 of 3; the shared-server version is green 3/3. Each request now seats its own carrier as its verified identity through `withCarrierVerifiedIdentity`, so one mount serves every tenant.

## Smoke script additions

`scripts/smoke/phase-209.sh` (live-server), every assertion against the DEFAULT `inmem` driver:

1. `put` → `get` round trip returning the stored bytes.
2. A bounded read reporting `truncated: true` with `total_size_bytes` > `returned_bytes`, and the head window's content.
3. An offset window returning the requested byte range (the payload is four distinguishable 32-byte blocks, so the content proves the window started where it was asked to), with the last window reporting `truncated: false`.
4. A `max_bytes` above the ceiling served at the ceiling with HTTP 200 — a 4xx here is a FAIL.
5. A cross-tenant id answering `not_found`; a foreign-tenant scope refused `403 scope_mismatch` (404 here is a FAIL — it would mean the request reached the store).
6. A negative offset refused `400 invalid_request`.
7. Static guards that the two config keys exist, are validated by name, are documented in the reference config, and that the builtin single-sources its bounds on `internal/config`.
8. `go test -race` gates on the builtin suite, the handler suite, the transport body-scope row pin (the §17.6 fix) and the integration test.

## Coverage target

- `internal/protocol`: 85%
- `internal/tools/builtin`: 85%
- `internal/config`: 85% (unchanged; the additions are covered by the validator and resolver tests)

## Dependencies

- **208** — the reconciled artifact read key this serves on. Without it the byte read would reproduce the enumerate-then-fail divergence on a new surface.
- **205** — the `bodyscope` gate it registers with.
- **133** — which reserved `artifacts.get` for its first consumer.

## Risks / open questions

- **A window costs a full materialisation.** `ArtifactStore.Get` has no range parameter, so paging a large artifact in small windows is model-triggerable IO amplification. Accepted here and stated in godoc rather than hidden: the ranged read is a five-driver conformance phase, and ordering semantics before cost is deliberate. The operator ceiling bounds one read, not a sequence — that limit is stated in the config godoc, `docs/CONFIG.md` and D-353 rather than being implied away, and aggregate consumption stays the governance layer's concern.
- **The surface key `artifacts.ref` now governs two methods.** A refusal on `artifacts.get` reports the wire name `artifacts.ref`. Accepted: a surface key is a POSTURE key rather than a method name (the registry's own godoc says so, and two existing keys deliberately avoid method names for exactly this reason), and a per-method key here could not be spelled at all — `"artifacts.get"` as a Go string literal outside `internal/protocol/methods` is a `method-literal` violation the single-source scan rejects.
- **No open RFC question gates this phase.**

## Glossary additions

No new terms. Two existing entries are reconciled with what now ships: **`artifacts.get`** (its "not a parallel implementation" reading is now the shipped code's, and the D-347-forward-looking phrasing becomes a description) and **`memory.get`** (whose "resolves the bytes via `artifacts.get`" claim becomes true).

## Pre-merge checklist

- [x] `make drift-audit` passes
- [x] `make preflight` passes
- [x] `make check-mirror` passes
- [x] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [x] Coverage on touched packages ≥ stated target
- [x] If multi-isolation paths changed: cross-session isolation test passes
- [x] **If this phase builds a reusable artifact: concurrent-reuse test passes** — `TestArtifactsGetHandler_ConcurrentReuse_NoCrossTalk` runs N=128 against one shared `ArtifactsSurface` under `-race`, asserting no data races, no context bleed (content AND bound checked per goroutine), no cancellation cross-talk (`TestArtifactsGetHandler_CancellationDoesNotCrossTalk`), and no goroutine leak (the surface starts none; `goroutine_settle_test.go` holds the package baseline).
- [x] **If this phase consumes a shipped subsystem's surface OR closes a cross-subsystem seam: an integration test exists** — `test/integration/artifacts_get_test.go`, two real drivers on the seam, identity propagation asserted, three failure modes covered, under `-race`.
- [x] If new vocabulary: glossary updated
- [x] If a brief finding was departed from: justified above + decisions.md entry filed (D-353)
