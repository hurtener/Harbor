# Phase 17 — ArtifactStore interface + InMem + Filesystem drivers

## Summary

Land `internal/artifacts/`: Harbor's content-addressed blob store for heavy outputs. Ships the `ArtifactStore` interface, two V1 drivers (in-memory + filesystem), the cross-driver `conformancetest.Run` suite, and the `ScopedArtifacts` facade that auto-stamps the identity quadruple on writes and scope-checks on reads. Heavy outputs (≥ 32 KB by default, runtime-configurable) are mandatorily routed through this store — there is **no** `NoOp` fallback (D-022, D-026, RFC §6.10). Phase 18 (SQLite-blob + Postgres-blob), Phase 19 (S3-style), and the LLM-edge enforcement pass (Phase 32) all consume this surface.

## RFC anchor

- RFC §6.10
- RFC §9
- RFC §3.5
- RFC §4

## Briefs informing this phase

- brief 05

## Brief findings incorporated

- **brief 05 §1 (mandatory artifacts policy).** "The reference implementation ships a `NoOpArtifactStore` fallback that warns and truncates. Harbor removes this fallback. An ArtifactStore is always configured; the in-memory driver is the floor." Phase 17 ships the InMem driver as the floor; production deployments use the FS driver (or future SQLite-blob / Postgres-blob / S3 drivers); no opt-out exists. (D-022, D-026.)
- **brief 05 §2 (ArtifactStore data shapes).** `ArtifactScope` (tenant/user/session/task), `ArtifactRef` with `ID = "{namespace}_{sha256[:12]}"`, content-addressed deduplication. Phase 17 implements these shapes verbatim; the seven-method interface (`PutBytes`, `PutText`, `Get`, `GetRef`, `Exists`, `Delete`, `List`) is the contract.
- **brief 05 §4 (artifact dedup, content addressing, scope-mismatch rejection).** "IDs are `{namespace}_{sha256[:12]}`. Re-uploading identical bytes returns the existing ref. The `ScopedArtifacts` facade is immutable post-construction; access control reads scope fields and rejects on mismatch." Phase 17 enforces all three; the conformance suite tests each.
- **brief 05 §6 (cross-tenant isolation tests, dedup-on-rewrite, scope-mismatch rejection).** Phase 17's conformance suite covers these three plus the standard concurrent-reuse + leak gates (D-025).
- **brief 05 §7 (phase decomposition).** "Artifacts-1: ArtifactStore interface + InMemory + Filesystem drivers. `ScopedArtifacts` facade; mandatory routing above threshold; deletion-by-scope; size-limit enforcement." Phase 17 maps to "Artifacts-1" exactly; "Artifacts-2" maps to Phase 18 (SQLite-blob + Postgres-blob) and Phase 19 (S3-style).

## Findings I'm departing from (if any)

- None.

## Goals

- Single mandatory `ArtifactStore` interface in `internal/artifacts` — seven methods (`PutBytes`, `PutText`, `Get`, `GetRef`, `Exists`, `Delete`, `List`), no `Supports*` ceremony.
- Two V1 drivers: `internal/artifacts/drivers/inmem/` (zero-dependency, the floor) and `internal/artifacts/drivers/fs/` (filesystem, the single-binary production target). Each registers itself via `init()` and is blank-imported by `cmd/harbor`.
- Driver-registry seam (`Register` / `Open` / `OpenDriver` / `RegisteredDrivers`) modeled verbatim on `internal/state/registry.go`. `Open(ctx, cfg config.ArtifactsConfig)` switches on `cfg.Driver` (`inmem` | `fs`); future drivers (Phase 18 SQLite-blob, PG-blob, Phase 19 S3) plug in without runtime changes.
- Cross-package `conformancetest.Run(t, factory)` suite at `internal/artifacts/conformancetest/conformancetest.go` — same shape as Phase 07's `internal/state/conformancetest`. The InMem and FS drivers MUST pass this suite verbatim. Phase 18 / 19 inherit it.
- Content-addressed IDs: `{namespace}_{sha256[:12]}`. Re-uploading identical bytes within the same scope returns the existing ref (no duplicate write).
- `ScopedArtifacts` facade in `internal/artifacts/scoped.go`: immutable post-construction; auto-stamps the quadruple on `PutBytes` / `PutText`; reads scope-check on `Get` / `GetRef` / `Exists` / `Delete` / `List`. Tools and runtime call the facade; they never see raw `ArtifactScope`.
- Heavy-output threshold: 32 KB default (D-022, RFC §6.10), exposed as `config.ArtifactsConfig.HeavyOutputThresholdBytes` (runtime-configurable; per-tool overrides land at Phase 26 via the tool catalog).
- Identity-mandatory at the API boundary: empty tenant/user/session in the scope is rejected with `ErrIdentityRequired`.
- Concurrent-reuse contract enforced by the suite (D-025): N≥100 goroutines putting/getting independent artifacts against a single shared driver instance under `-race`, no data races, no cross-talk, baseline goroutine count restored.
- Cross-tenant / cross-session isolation enforced by the suite: artifact saved under tenant A is not readable / listable / deletable under tenant B; same for session.
- Filesystem driver layout: `<root>/<tenant>/<user>/<session>/<task>/<namespace>/<id>` for blob bytes + a sibling `<id>.meta.json` for the `ArtifactRef`. Path traversal defended via `filepath.Clean` + prefix-check (AGENTS.md §7 #5).

## Non-goals

- No SQLite-blob / Postgres-blob driver (Phase 18) — those inherit this phase's conformance suite.
- No S3 driver (Phase 19).
- No artifact TTL / LRU eviction. The InMem driver is the floor (per-process lifetime); FS driver retains until explicit `Delete`. TTL / LRU lands when a future phase needs it; `ArtifactScope` is the right key to wire it on without changing the interface.
- No presigned-URL `GetRef` path. That's Phase 19 (S3-style).
- No cross-driver migration tooling (e.g. "drain InMem to FS"). Operator concern, not V1's scope.
- No streaming `Put` / `Get` — V1 is byte-slice / text shaped; the heavy-output threshold (32 KB) caps inline payloads to a sane size. Streaming variant lands when LLM-side streaming consumers (Phase 12) need it pull-shaped, which they don't yet.
- No event-bus integration. The runtime fires `artifact.created` / `artifact.deleted` events from the consumer layer (e.g. tool dispatcher Phase 26+); the store is a leaf.
- No PII redaction inside the store. Bytes are opaque; redaction runs upstream (D-020). Stub-shape enforcement (D-026 `ErrContextLeak` / standard `ArtifactStub`) lives at the LLM-edge in Phase 32; the store does not enforce it.
- No GC sweep / quota enforcement at V1. Disk quota / per-tenant byte caps land with their first consumer.

## Acceptance criteria

- [ ] `internal/artifacts/artifacts.go` defines:
  - `ArtifactScope` struct: `TenantID, UserID, SessionID, TaskID string`. Identity helpers: `Validate() error` (rejects empty tenant/user/session), `Equal(other ArtifactScope) bool`.
  - `ArtifactRef` struct: `ID, MimeType string; SizeBytes int64; Filename, SHA256 string; Scope ArtifactScope; Namespace string; Source map[string]any`.
  - `PutOpts` struct: `MimeType, Filename, Namespace string; Source map[string]any`.
  - `ArtifactStore` interface (seven methods, exactly the brief shape).
  - Sentinel errors: `ErrNotFound`, `ErrScopeMismatch`, `ErrIdentityRequired`, `ErrInvalidScope`, `ErrUnknownDriver`, `ErrStoreClosed`.
  - `Validate(scope ArtifactScope) error` exported helper.
- [ ] `internal/artifacts/registry.go` provides `Register(name string, factory Factory)`, `Open(ctx, cfg config.ArtifactsConfig) (ArtifactStore, error)`, `OpenDriver(name string, cfg config.ArtifactsConfig) (ArtifactStore, error)`, `RegisteredDrivers() []string` — modeled verbatim on `internal/state/registry.go`.
- [ ] `internal/artifacts/scoped.go` provides `ScopedArtifacts` — an immutable facade carrying a fixed `ArtifactScope`. Methods: `PutBytes(ctx, data, opts) (ArtifactRef, error)`, `PutText(ctx, text, opts) (ArtifactRef, error)`, `Get(ctx, id) ([]byte, bool, error)`, `GetRef(ctx, id) (*ArtifactRef, bool, error)`, `Exists(ctx, id) (bool, error)`, `Delete(ctx, id) (bool, error)`, `List(ctx) ([]ArtifactRef, error)`. The facade auto-stamps the scope on writes; it scope-checks on reads (returns `ErrScopeMismatch` when the underlying ref's scope doesn't equal the facade's scope).
- [ ] `internal/artifacts/drivers/inmem/inmem.go`:
  - Registers as `"inmem"` via `init()`.
  - Backing storage: `map[string]storedArtifact` keyed by `{scope-path}_{id}` (struct-key for safety) plus a sibling `map[string][]byte` for blob bytes. Single `sync.RWMutex`.
  - `PutBytes` computes `sha256` over data, sets `ID = "{namespace}_{hex[:12]}"`, dedups against same-scope same-ID (returns existing ref).
  - `Get` / `GetRef` / `Exists` / `Delete` / `List` filter by scope (cross-tenant / cross-session leakage is impossible by construction).
  - `Close` is idempotent; no goroutines to join.
- [ ] `internal/artifacts/drivers/fs/fs.go`:
  - Registers as `"fs"` via `init()`.
  - `New(cfg config.ArtifactsConfig)` reads `cfg.FSRoot`; creates the directory if it doesn't exist (`os.MkdirAll(root, 0o755)`).
  - Storage layout: `<root>/<tenant>/<user>/<session>/<task>/<namespace>/<id>` for blob bytes; sibling `<id>.meta.json` for `ArtifactRef`.
  - Path safety: every path component runs through `filepath.Clean`; a `strings.HasPrefix(absPath, allowedRoot)` guard rejects anything outside `cfg.FSRoot` (AGENTS.md §7 #5).
  - `PutBytes` writes blob + meta in one logical operation (write blob to `tmp.<id>`, write meta to `tmp.<id>.meta.json`, then `os.Rename` both atomically). On crash mid-write, only fully-renamed pairs are considered "stored" (the `tmp.*` files are discarded by `Open` startup or by next Put).
  - `Delete` removes both blob + meta; returns the boolean "did anything exist before delete." Idempotent (deleting nonexistent → returns `(false, nil)`).
  - `List` walks `<root>/<tenant>/<user>/<session>/<task>/<namespace>` directories matching the filter.
  - `Close` is idempotent.
- [ ] `internal/artifacts/conformancetest/conformancetest.go` exports `Run(t *testing.T, factory func() (artifacts.ArtifactStore, func()))`. Subtests:
  - `Put_Get_RoundTrip` — basic happy path; `PutBytes` returns a ref; `Get` returns byte-equal data; `GetRef` returns the ref; `Exists` returns `true`.
  - `Put_DedupOnIdenticalBytes` — same `(scope, namespace, bytes)` second-Put returns the existing ref; storage is not duplicated; `List` shows one entry.
  - `Put_DistinguishesByNamespace` — same bytes under different namespaces produce different IDs.
  - `Put_DistinguishesByScope` — same bytes under different scopes are independent (cross-tenant: tenant A's put and tenant B's put yield two rows).
  - `PutText_StoredAsBytes` — `PutText` is a thin wrapper over `PutBytes`; recovered via `Get` as bytes.
  - `Get_NotFound` — returns `(nil, false, nil)` (NOT an error — the consumer pattern is "exists → fetch"). `Exists` returns `false`.
  - `GetRef_NotFound` — same shape.
  - `Delete_Idempotent` — Delete on absent returns `(false, nil)`. Delete on present returns `(true, nil)`; subsequent Get returns `(nil, false, nil)`.
  - `List_FiltersByScope` — `List(scope)` returns refs in that scope only; cross-tenant, cross-session don't leak.
  - `List_NilFieldsAreWildcards` — `ArtifactScope{TenantID: "A"}` with empty user/session/task lists ALL artifacts under tenant A across users/sessions/tasks. (Treats empty fields as wildcards in `List`; documented; tested.)
  - `Put_Identity_Mandatory` — empty tenant / user / session each return `errors.Is(err, ErrIdentityRequired)`. `ScopedArtifacts.New(...)` panics if constructed with an invalid scope (the facade's scope is fixed at construction; rejecting at construction is the simplest, loudest failure).
  - `Get_CrossTenant_Isolation` — saved under tenant A, fetched as tenant B → `ErrScopeMismatch` (or `(nil, false, nil)` from a raw store if no scope check; via `ScopedArtifacts` it's `ErrScopeMismatch`). The conformance suite covers BOTH paths since the raw store + the facade are both production code.
  - `Delete_CrossTenant_Isolation` — same shape: tenant B's `Delete` does not touch tenant A's artifacts.
  - `Concurrent_PutGet_NoRace` (D-025) — N≥128 goroutines doing put/get/list/delete on independent scopes under `-race`. No data races, no cross-talk; baseline goroutine count restored.
  - `Close_Idempotent` — Close called twice returns nil both times.
  - `GoroutineLeak_AfterClose` — `runtime.NumGoroutine` returns to baseline after `Close`.
- [ ] `internal/artifacts/conformancetest/conformancetest_test.go` self-applies `Run` against the InMem driver factory.
- [ ] `internal/artifacts/drivers/inmem/inmem_test.go` runs `conformancetest.Run` against the InMem driver.
- [ ] `internal/artifacts/drivers/fs/fs_test.go` runs `conformancetest.Run` against the FS driver, using `t.TempDir()` for `cfg.FSRoot`.
- [ ] `internal/artifacts/drivers/fs/path_safety_test.go` directly exercises the path-traversal guard: `Put` with `Filename: "../../../etc/passwd"` rejects (the filename is metadata only, not used in path construction — the guard is a defense in depth).
- [ ] `internal/artifacts/scoped_test.go` covers the facade: scope-mismatch on Get, scope auto-stamping on Put, immutability of the facade's scope.
- [ ] `internal/artifacts/artifacts_test.go` covers the registry surface (`Register` / `Open` / unknown driver) and the sentinel-error wiring.
- [ ] `internal/config/config.go` extends `ArtifactsConfig` to the shape:

```go
type ArtifactsConfig struct {
    Driver                     string `yaml:"driver"`
    FSRoot                     string `yaml:"fs_root,omitempty"`
    HeavyOutputThresholdBytes  int    `yaml:"heavy_output_threshold_bytes,omitempty"`
}
```

- [ ] `internal/config/loader.go::Default` populates `ArtifactsConfig` defaults: `Driver: "inmem"`, `FSRoot: ""` (only required when `Driver == "fs"`), `HeavyOutputThresholdBytes: 32 * 1024`.
- [ ] `internal/config/loader.go::Validate` validates: when `Driver == "fs"`, `FSRoot != ""`; when `HeavyOutputThresholdBytes < 0`, reject. Empty `Driver` is rejected (no implicit default at validation; the loader fills the default before validation).
- [ ] `cmd/harbor/main.go` adds `_ "github.com/hurtener/Harbor/internal/artifacts/drivers/inmem"` and `_ "github.com/hurtener/Harbor/internal/artifacts/drivers/fs"` (additive).
- [ ] Coverage on `internal/artifacts` ≥ 85%; `internal/artifacts/drivers/inmem` ≥ 90%; `internal/artifacts/drivers/fs` ≥ 85%.
- [ ] `make drift-audit` and `make preflight` green at commit time.
- [ ] `scripts/smoke/phase-17.sh` present and executable. Reports OK for the `internal/artifacts/...` package tests under preflight; SKIP for the HTTP surface (Phase 60+).
- [ ] `docs/glossary.md` gains entries for `ArtifactStore`, `ArtifactScope`, `ArtifactRef`, `ScopedArtifacts`, `HeavyOutputThreshold`.
- [ ] `docs/plans/README.md` Status column for Phase 17 flips from `Pending` to `Shipped` in the same PR.

## Files added or changed

- `internal/artifacts/artifacts.go` (new) — `ArtifactStore`, `ArtifactScope`, `ArtifactRef`, `PutOpts`, sentinel errors, `Validate`.
- `internal/artifacts/registry.go` (new) — `Register`, `Open`, `OpenDriver`, `RegisteredDrivers`. Modeled on `internal/state/registry.go`.
- `internal/artifacts/scoped.go` (new) — `ScopedArtifacts` immutable facade.
- `internal/artifacts/scoped_test.go` (new).
- `internal/artifacts/artifacts_test.go` (new) — registry tests.
- `internal/artifacts/conformancetest/conformancetest.go` (new) — exported `Run(t, factory)`.
- `internal/artifacts/conformancetest/conformancetest_test.go` (new) — self-applied smoke against InMem.
- `internal/artifacts/drivers/inmem/inmem.go` (new) — InMem driver. `init()` registers under `"inmem"`.
- `internal/artifacts/drivers/inmem/inmem_test.go` (new) — driver-level tests + conformance invocation.
- `internal/artifacts/drivers/fs/fs.go` (new) — FS driver. `init()` registers under `"fs"`.
- `internal/artifacts/drivers/fs/fs_test.go` (new) — driver-level tests + conformance invocation.
- `internal/artifacts/drivers/fs/path_safety_test.go` (new) — explicit path-traversal defense tests.
- `internal/config/config.go` (modified) — `ArtifactsConfig` fields filled.
- `internal/config/loader.go` (modified) — defaults + validation.
- `cmd/harbor/main.go` (modified) — additive blank imports for inmem + fs.
- `scripts/smoke/phase-17.sh` (new) — assertions described under "Smoke script additions".
- `docs/plans/phase-17-artifacts.md` (this file).
- `docs/plans/README.md` (modified) — Phase 17 row Status flip.
- `docs/glossary.md` (modified) — adds `ArtifactStore`, `ArtifactScope`, `ArtifactRef`, `ScopedArtifacts`, `HeavyOutputThreshold` entries.

`internal/artifacts/` is enumerated in AGENTS.md §3 — no top-level directory addition.

## Public API surface

```go
package artifacts

import (
    "context"
    "errors"

    "github.com/hurtener/Harbor/internal/config"
)

// ArtifactScope identifies the (tenant, user, session, task) owner.
// All four are required for store-level writes; empty TaskID is
// acceptable for session-scoped artifacts (parallels state.StateStore's
// session-vs-run rule). List uses empty fields as wildcards.
type ArtifactScope struct {
    TenantID, UserID, SessionID, TaskID string
}

// Validate returns ErrIdentityRequired when tenant/user/session is
// empty. Empty TaskID is acceptable.
func (s ArtifactScope) Validate() error

// Equal compares two scopes structurally.
func (s ArtifactScope) Equal(other ArtifactScope) bool

// ArtifactRef is the canonical reference returned by Put and resolved
// by GetRef. ID is content-addressed: "{namespace}_{sha256_hex[:12]}".
type ArtifactRef struct {
    ID        string
    MimeType  string
    SizeBytes int64
    Filename  string
    SHA256    string  // full hex digest
    Scope     ArtifactScope
    Namespace string
    Source    map[string]any
}

// PutOpts carries optional metadata for Put*.
type PutOpts struct {
    MimeType  string
    Filename  string
    Namespace string         // logical bucket; participates in ID
    Source    map[string]any // tool-name, preview, warnings
}

// ArtifactStore is Harbor's content-addressed blob store.
//
// Concurrent reuse contract (D-025): every method is safe to call from
// N goroutines on a single shared instance.
type ArtifactStore interface {
    PutBytes(ctx context.Context, scope ArtifactScope, data []byte, opts PutOpts) (ArtifactRef, error)
    PutText (ctx context.Context, scope ArtifactScope, text string, opts PutOpts) (ArtifactRef, error)
    Get     (ctx context.Context, scope ArtifactScope, id string) ([]byte, bool, error)
    GetRef  (ctx context.Context, scope ArtifactScope, id string) (*ArtifactRef, bool, error)
    Exists  (ctx context.Context, scope ArtifactScope, id string) (bool, error)
    Delete  (ctx context.Context, scope ArtifactScope, id string) (bool, error)
    List    (ctx context.Context, filter ArtifactScope) ([]ArtifactRef, error)
    Close   (ctx context.Context) error
}

// Sentinel errors. Callers compare via errors.Is.
var (
    ErrNotFound         = errors.New("artifacts: ref not found")
    ErrScopeMismatch    = errors.New("artifacts: scope mismatch")
    ErrIdentityRequired = errors.New("artifacts: identity required (tenant/user/session)")
    ErrInvalidScope     = errors.New("artifacts: invalid scope")
    ErrUnknownDriver    = errors.New("artifacts: unknown driver")
    ErrStoreClosed      = errors.New("artifacts: store is closed")
)

// Factory + registry mirror state.Factory / state.Register.
type Factory func(config.ArtifactsConfig) (ArtifactStore, error)

func Register(name string, factory Factory)
func Open(ctx context.Context, cfg config.ArtifactsConfig) (ArtifactStore, error)
func OpenDriver(name string, cfg config.ArtifactsConfig) (ArtifactStore, error)
func RegisteredDrivers() []string

// ScopedArtifacts wraps an ArtifactStore with a fixed scope. Tools
// and runtime call the facade; they never see raw scopes.
type ScopedArtifacts struct { /* internal */ }

// NewScoped panics if scope.Validate() fails.
func NewScoped(store ArtifactStore, scope ArtifactScope) *ScopedArtifacts

func (s *ScopedArtifacts) PutBytes(ctx context.Context, data []byte, opts PutOpts) (ArtifactRef, error)
func (s *ScopedArtifacts) PutText (ctx context.Context, text string, opts PutOpts) (ArtifactRef, error)
func (s *ScopedArtifacts) Get     (ctx context.Context, id string) ([]byte, bool, error)
func (s *ScopedArtifacts) GetRef  (ctx context.Context, id string) (*ArtifactRef, bool, error)
func (s *ScopedArtifacts) Exists  (ctx context.Context, id string) (bool, error)
func (s *ScopedArtifacts) Delete  (ctx context.Context, id string) (bool, error)
func (s *ScopedArtifacts) List    (ctx context.Context) ([]ArtifactRef, error)
func (s *ScopedArtifacts) Scope   () ArtifactScope
```

## Test plan

- **Unit:** registry — `Register` panics on duplicate / empty / nil; `Open` routes by `cfg.Driver`; unknown driver wraps `ErrUnknownDriver`. `ScopedArtifacts` — auto-stamping, scope-mismatch detection, immutability. `ArtifactScope.Validate` / `Equal`. Path safety on FS driver.
- **Integration:** N/A in-package; the facade + drivers are intra-package. The wave-end smoke (`test/integration/wave5_test.go`, landed alongside Phase 15/16/17 merging) wires sessions + state + artifacts together to prove the seam.
- **Conformance:** `conformancetest.Run` is the load-bearing test. Subtests enumerated under "Acceptance criteria". Self-applied to InMem in `conformancetest_test.go`; re-applied in InMem's `inmem_test.go` and FS's `fs_test.go`. Phase 18 / 19 inherit verbatim.
- **Concurrency / leak (D-025):** `Concurrent_PutGet_NoRace` is the canonical test — N≥128 concurrent puts/gets/lists/deletes on a shared driver instance under `-race`. `GoroutineLeak_AfterClose` covers leaks (InMem has none; FS has none either; future drivers may have pumps and inherit the test). Per AGENTS.md §5 + §11 + RFC §3.5 + D-025.

## Smoke script additions

- `scripts/smoke/phase-17.sh` runs:
  - `go test -race -count=1 -timeout 90s ./internal/artifacts/...` → OK on green, FAIL otherwise.
  - `skip "phase 17: artifacts has no HTTP/Protocol surface yet (lands in Phase 60+)"`.

The smoke is package-test driven (no protocol surface yet, same shape as phase-15 / phase-16).

## Coverage target

- `internal/artifacts`: 85% (registry + facade + sentinel surface).
- `internal/artifacts/drivers/inmem`: 90%.
- `internal/artifacts/drivers/fs`: 85% (some FS-error paths are I/O-failure-only and intentionally excluded).
- `internal/artifacts/conformancetest`: not gated (helper's `t.Errorf` paths only fire on driver failure; precedent: Phase 01 + Phase 07 conformancetest).

## Dependencies

- Phase 01 (identity) — `ArtifactScope`'s tenant/user/session fields mirror `identity.Identity`. Phase 17 does NOT directly import `internal/identity` because `ArtifactScope` is a flat-string struct (the brief shape). The translation between `identity.Quadruple` and `ArtifactScope` lives at the consumer (`tools/dispatcher` Phase 26+), not in the store.
- Phase 02 (config) — `config.ArtifactsConfig` is the open-time argument; Phase 17 fills the field shape.
- Phase 07 (state, by analogy) — Phase 17 mirrors the registry / facade / conformance pattern.

## Risks / open questions

- **`ArtifactScope` vs `identity.Quadruple`.** RFC §6.10's `ArtifactScope` is `(TenantID, UserID, SessionID, TaskID)` — a *task*-scoped 4-tuple, not the runtime's `(Tenant, User, Session, Run)` quadruple. The mapping is `RunID → TaskID` for foreground runs (per D-008: a run is a foreground task); for background tasks, `TaskID` is the bg task's own ID. This translation is the consumer's job (tool dispatcher / runtime) — Phase 17 takes `ArtifactScope` verbatim. Documented on godoc.
- **Heavy-output threshold default 32 KB.** Settled by D-022 + RFC §6.10. Phase 17 only ships the config field + default; **enforcement** of the threshold lives at consumer layers (tool dispatcher Phase 26 — auto-route tool returns above the threshold; LLM-edge Phase 32 — fail loudly with `ErrContextLeak` if raw heavy content slipped through).
- **Path traversal.** FS driver uses `filepath.Clean` + `strings.HasPrefix(absPath, allowedRoot)`. Tested by `path_safety_test.go`. Per AGENTS.md §7 #5, the eventual plan is a shared helper `internal/skills/importer/path_safety.go` (Phase 40) — when that helper lands, Phase 17 can switch to it; until then, the inline guard is sufficient.
- **FS driver atomicity on crash.** `tmp.<id>` + `os.Rename` is the standard atomicity dance; on crash mid-write, only fully-renamed files are visible. Tested? V1 leaves crash-recovery tests to the durable drivers (Phase 18); the FS driver's invariant is documented + enforced by the rename-after-write pattern, but exhaustive crash-recovery tests are out of scope here.
- **Concurrent same-scope same-bytes Put.** Two goroutines `Put`-ing identical bytes at the same scope simultaneously: both should succeed, both should return the same ID. The InMem driver's RWMutex serializes the dedup check; the FS driver's atomic rename means the second writer's rename succeeds (overwriting an identical file is a no-op on POSIX) — both observe the dedup. Tested by `Concurrent_PutGet_NoRace`.
- **`Source map[string]any` shape.** Untyped `map[string]any` is the brief / RFC shape. Encoding to JSON for the FS driver's `.meta.json` requires `map[string]any` to contain JSON-encodable values; non-encodable values fail at `json.Marshal` time. Documented on godoc; the InMem driver retains the raw map (no encoding).
- **No open RFC §11 questions block this phase.** Q-1..Q-6 are unrelated.

## Glossary additions

- **`ArtifactStore`** — Harbor's mandatory content-addressed blob store. Single interface, three V1 driver targets at this phase + Phase 18 + Phase 19; `NoOp` fallback explicitly absent. RFC §6.10, D-022, D-026.
- **`ArtifactScope`** — `(TenantID, UserID, SessionID, TaskID)` ownership tuple for an artifact. Identity-mandatory at the API boundary; empty `TaskID` is acceptable for session-scoped artifacts. RFC §6.10.
- **`ArtifactRef`** — the compact, in-context-safe reference returned by Put and resolved by Get. `ID = "{namespace}_{sha256[:12]}"`. RFC §6.10.
- **`ScopedArtifacts`** — immutable facade carrying a fixed `ArtifactScope`; auto-stamps writes, scope-checks reads. Tools and runtime use the facade exclusively. RFC §6.10.
- **`HeavyOutputThreshold`** — the byte size at which the runtime mandatorily routes a payload through the ArtifactStore. Default 32 KB; runtime-configurable; per-tool overridable at Phase 26. D-022, D-026, RFC §6.10.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated targets (85% `internal/artifacts`, 90% `inmem`, 85% `fs`)
- [ ] If multi-isolation paths changed: cross-session isolation test passes — yes; conformance suite covers `Get_CrossTenant_Isolation`, `Delete_CrossTenant_Isolation`, `List_FiltersByScope`.
- [ ] **Concurrent-reuse test passes** — `Concurrent_PutGet_NoRace` in the conformance suite, N≥128 goroutines under `-race`, no data races, no cross-talk, no leaks (D-025).
- [ ] If new vocabulary: glossary updated (yes — `ArtifactStore`, `ArtifactScope`, `ArtifactRef`, `ScopedArtifacts`, `HeavyOutputThreshold`).
- [ ] If a brief finding was departed from: N/A — none.
