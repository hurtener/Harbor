# Phase 37 — Skill store + LocalDB driver + FTS5 ladder

## Summary

Phase 37 lands Harbor's token-savvy skill subsystem: the `SkillStore` interface (the §4.4 seam every later phase consumes), a CGo-free SQLite-backed `localdb` driver, and the FTS5 → regex → exact search ranking ladder calibrated to the constants documented in brief 04 §4.4. The driver owns its own schema (D-034 analog), persists `Origin / OriginRef / Scope / ContentHash`, and refuses pack-overwrite-by-generated at the upsert path. Per `cfg.Skills.Driver`-routed wiring this is the first leg of an in-mem / SQLite / Portico-provider triad (Phase 49 / Phase post-V1) under a single conformance suite.

## RFC anchor

- RFC §6.7

## Briefs informing this phase

- brief 04

## Brief findings incorporated

- brief 04 §4.3: SQLite schema with `Origin / OriginRef / ContentHash` lifecycle columns, FTS5 virtual table over `name | title | trigger | description | tags` with porter unicode61 tokenizer + INSERT/DELETE/UPDATE triggers, WAL journal mode pinned on every connection. Harbor ships `modernc.org/sqlite` (CGo-free) and verifies FTS5 is compiled in at open.
- brief 04 §4.4: three-tier ranking ladder — **FTS** path tokenises via `[A-Za-z0-9]+`, runs strict-AND first then OR fallback, scores `bm25 → 1/(1+raw) → min-max normalised 0..1`; **regex** path scores `name fullmatch=0.95`, `name match=0.90`, `name search=0.85`, body `search=0.75`; **exact** path is lowercase equality on `name | title | trigger | tags`. Constants are calibrated for this corpus and gated by the golden-ranking test.
- brief 04 §5.7: FTS5 is conditionally available — the regex/exact fallback must be tested with an FTS-off run in CI. The driver detects FTS5 at open via `SELECT fts5(?)` and toggles the path; the ranking ladder still ranks results deterministically when FTS5 is absent.
- brief 04 §4.8: conflict policy — refuse to overwrite an `Origin=PackImport` skill with the same name (the predecessor's `existing_origin != "pack"` guard). Generated→Generated is last-write-wins gated by content-hash change.
- brief 04 §6: golden ranking tests with frozen scoring constants for FTS / regex / exact paths, including the FTS-off fallback. Plus identity-mandatory and cross-session no-leak conformance.

## Findings I'm departing from (if any)

- None. The phase ports the brief's settled mechanics verbatim. The §4.4-vs-§3 layout mismatch (AGENTS.md §3 lists `skills/providers/*`, §4.4 binds `drivers/<driver>/`) is reconciled in the same PR by updating §3 to `drivers/` so every subsystem follows the canonical seam shape — recorded as a NIT in the §3 layout block.

## Goals

- Define the mandatory `SkillStore` interface (RFC §6.7's `SkillProvider`, renamed `SkillStore` to match the storage-layer vocabulary the rest of the codebase uses; the planner-facing tools in Phase 38 wrap it as the legacy `SkillProvider` surface). Identity-mandatory (D-001), concurrent-reuse-safe (D-025).
- Ship the `localdb` SQLite-backed driver under the §4.4 seam: self-registers under `"localdb"` via `init()`, owns its own `skills` + `skills_fts` tables (D-034 analog), forward-only migrations, WAL + `busy_timeout(5000)` + `SetMaxOpenConns(1)` matching Phase 25.
- Implement the FTS5 → regex → exact ranking ladder with the brief 04 §4.4 scoring constants; detect FTS5-availability at open and fall back deterministically when absent.
- Enforce the pack-overwrite refusal at the upsert path with `ErrPackOverwriteRefused` + a `skill.pack_overwrite_refused` audit event.
- Emit the `skill.*` event taxonomy: `skill.upserted`, `skill.deleted`, `skill.pack_overwrite_refused`, `skill.search_executed`. Payloads are SafeSealed (RFC §6.7-driven, brief 06).

## Non-goals

- Planner-facing tools (`skill_search`, `skill_get`, `skill_list`, capability filter, redactor, tiered budgeter) — owned by Phase 38.
- Virtual-directory subsystem (`Directory(cfg)`, `pinned_then_recent` / `pinned_then_top`) — owned by Phase 39.
- Skills.md importer (parser, normaliser, attachment resolver, round-trip) — owned by Phase 40.
- In-runtime generator with persistence (`skill_propose(persist=true)`, generator audit) — owned by Phase 41.
- Portico SkillStore driver (`internal/skills/drivers/portico/`) — post-V1 unless Portico's MCP surface lands in the same window (RFC §6.7).
- Postgres SkillStore driver — not required at Phase 37. The §4.4 seam keeps the door open for a follow-up phase if cross-tenant rolling-forward warrants it.

## Acceptance criteria

- [ ] `SkillStore` interface exposed at `internal/skills/skills.go`; sentinels (`ErrSkillNotFound`, `ErrPackOverwriteRefused`, `ErrStoreClosed`, `ErrInvalidSkill`, `ErrUnknownDriver`, `ErrIdentityRequired`) compare via `errors.Is`.
- [ ] `localdb` driver self-registers under `"localdb"` via `init()` and is blank-imported from `cmd/harbor/main.go`.
- [ ] CGo-free build: `CGO_ENABLED=0 go build ./...` succeeds; the driver uses `modernc.org/sqlite`.
- [ ] Schema applied via forward-only embedded migrations; clean DB + restart both produce identical schema. SQLite WAL journal mode pinned; `busy_timeout(5000)`; `SetMaxOpenConns(1)`.
- [ ] FTS5 detected at open: on FTS5-available builds the FTS path executes; on FTS-off builds the driver falls through to regex → exact without erroring. CI exercises both paths.
- [ ] Golden-ranking test passes with the brief 04 §4.4 scoring constants: FTS bm25 → `1/(1+raw)` → min-max; regex `name fullmatch=0.95 / match=0.90 / search=0.85 / body search=0.75`; exact lowercase equality. Rankings are stable across runs (deterministic ordering on ties).
- [ ] `Upsert` refuses to overwrite a row with `existing_origin = "pack"` and incoming `origin != "pack"`: returns `ErrPackOverwriteRefused`, leaves the row untouched, emits `skill.pack_overwrite_refused`. Generated → Generated short-circuits when content-hash matches (idempotent no-op); differing content-hash applies last-write-wins.
- [ ] Identity-mandatory: every method validates the `identity.Quadruple` triple at the boundary; missing tenant/user/session returns wrapped `ErrIdentityRequired` AND emits `skill.identity_rejected` on the bus.
- [ ] Concurrent-reuse contract (D-025) holds: N≥100 goroutines call `Upsert` / `Get` / `Search` / `List` / `Delete` on a single shared store under `-race`; no data races, no context bleed, no cancellation cross-talk, no goroutine leaks (baseline-restored after teardown).
- [ ] Restart survival: open, write N skills, close, reopen against the same DSN, observe identical results from `Search` / `List` / `Get`.
- [ ] `internal/skills` coverage ≥ 85%.

## Files added or changed

```text
internal/skills/
├── skills.go                              # SkillStore iface + types + sentinels + factory + registry
├── events.go                              # skill.* event types + SafeSealed payloads
├── wire.go                                # Skill wire envelope (cross-driver byte-stable hash)
├── reject.go                              # EmitIdentityRejected helper (mirrors memory/reject.go)
├── conformancetest/
│   └── conformancetest.go                 # Harness — shared suite for localdb + future drivers
├── drivers/
│   └── localdb/
│       ├── localdb.go                     # *driver: Open, init() registration, SetMaxOpenConns(1)
│       ├── search.go                      # FTS5 → regex → exact ladder, scoring constants
│       ├── migrations.go                  # embedded migrations runner (mirrors memory/sqlite)
│       ├── migrations/0001_init.sql       # skills + skills_fts + triggers + schema_migrations
│       ├── localdb_test.go                # Unit + golden ranking + FTS-off fallback + LWW
│       └── concurrent_test.go             # D-025 N≥100 stress under -race
internal/config/config.go                  # SkillsConfig: Driver, DSN
internal/config/validate.go                # validateSkills + driver allowlist
cmd/harbor/main.go                         # blank-import internal/skills/drivers/localdb
examples/harbor.yaml                       # skills: driver/dsn block
docs/plans/phase-37-skills-store.md        # this file
docs/plans/README.md                       # flip Phase 37 row to Shipped
README.md                                  # flip Phase 37 row to Shipped
docs/glossary.md                           # SkillStore, Origin, OriginRef, Scope, ContentHash, FTS5Ladder, RankingScore
docs/decisions.md                          # D-NNN entries: D-034-skills (own-table), D-045-fts5-detect, D-046-conflict-policy
scripts/smoke/phase-37.sh                  # smoke skeleton; OK once cli surface lands (placeholder skip OK at Phase 37)
AGENTS.md / CLAUDE.md                      # §3 layout: skills/providers → skills/drivers
```

## Public API surface

```go
// internal/skills/skills.go

type Origin string
const (
    OriginPack      Origin = "pack"       // imported from a Skills.md pack
    OriginGenerated Origin = "generated"  // produced by skill_propose(persist=true)
)

type Scope string
const (
    ScopeProject Scope = "project"
    ScopeTenant  Scope = "tenant"
    ScopeGlobal  Scope = "global"
)

type Skill struct {
    Name            string     // primary key within (identity, scope)
    Title           string
    Description     string
    Trigger         string     // non-empty; planner-visible match cue
    TaskType        string     // browser | api | code | domain | unknown
    Tags            []string
    Steps           []string   // non-empty
    Preconditions   []string
    FailureModes    []string
    RequiredTools   []string
    RequiredNS      []string
    RequiredTags    []string
    Origin          Origin
    OriginRef       string     // pack-name@version OR gen:{session}:{run}
    Scope           Scope
    ScopeTenantID   string
    ScopeProjectID  string
    ContentHash     string     // canonical sha256 of normalised fields
    CreatedAt       time.Time
    UpdatedAt       time.Time
    LastUsed        time.Time
    UseCount        int
    Extra           map[string]any
}

type ListFilter struct {
    Scope     Scope    // optional; empty matches any
    TaskType  string   // optional
    Tags      []string // any-of match
    Limit     int      // 0 = driver default (100); capped at 1000
    Offset    int
}

type RankedSkill struct {
    Skill Skill
    Score float64  // 0.0–1.0 normalised; brief 04 §4.4 constants
    Path  string   // "fts5" | "regex" | "exact" — observability/debug
}

type SkillStore interface {
    Upsert(ctx context.Context, id identity.Quadruple, skill Skill) error
    Get   (ctx context.Context, id identity.Quadruple, name string) (Skill, error)
    List  (ctx context.Context, id identity.Quadruple, filter ListFilter) ([]Skill, error)
    Search(ctx context.Context, id identity.Quadruple, query string, limit int) ([]RankedSkill, error)
    Delete(ctx context.Context, id identity.Quadruple, name string) error
    Close (ctx context.Context) error
}

type ConfigSnapshot struct {
    Driver string  // "localdb"; future: "portico"
    DSN    string  // SQLite file path or :memory: for tests
}

type Deps struct {
    Bus events.EventBus  // mandatory
    // No State — localdb owns its own database (D-034 analog).
}

type Factory func(cfg ConfigSnapshot, deps Deps) (SkillStore, error)

func Register(name string, f Factory)
func Open(ctx context.Context, cfg ConfigSnapshot, deps Deps) (SkillStore, error)

const DefaultDriver = "localdb"

var (
    ErrSkillNotFound        = errors.New("skills: skill not found")
    ErrPackOverwriteRefused = errors.New("skills: refuse to overwrite pack-origin skill")
    ErrStoreClosed          = errors.New("skills: store is closed")
    ErrInvalidSkill         = errors.New("skills: invalid skill (validation failed)")
    ErrUnknownDriver        = errors.New("skills: unknown driver")
    ErrIdentityRequired     = errors.New("skills: identity triple incomplete")
)
```

## Test plan

- **Unit:**
  - `Upsert` happy path: new skill round-trips through `Get`. Content-hash is recomputed from canonical bytes.
  - `Upsert` conflict policies: pack→pack-same-name allowed, pack→generated REFUSED, generated→generated-same-hash idempotent, generated→generated-different-hash LWW.
  - `Get` missing returns `ErrSkillNotFound`.
  - `List` filters by scope / task_type / tags; respects limit + offset; deterministic ordering on `(updated_at DESC, name ASC)`.
  - `Delete` removes the row and decrements FTS triggers; subsequent `Get` returns `ErrSkillNotFound`.
  - `Skill` validator: empty `Trigger` / empty `Steps` / empty `Origin` / empty `Name` → `ErrInvalidSkill`.
  - **Golden ranking (FTS5 path):** seed 10 skills with predictable token frequencies; assert exact ordering + scores within ε=1e-9 against frozen expected values calibrated from brief 04 §4.4.
  - **Golden ranking (regex path):** same corpus, FTS5 disabled at the path level; assert exact ordering + scores for `name fullmatch=0.95`, `name match=0.90`, `name search=0.85`, `body search=0.75`.
  - **Golden ranking (exact path):** lowercase equality on `name | title | trigger | tags`; score=1.0.
  - **FTS-off fallback:** driver opened on a DB where the FTS5 virtual table failed to create (simulated by forcing the detection path to false); `Search` returns regex/exact-ranked results, never errors.

- **Integration:**
  - Restart survival: write skills, close store, reopen against the same DSN, observe identical `Search` + `List` + `Get` results.
  - Identity rejection: missing tenant / user / session in any method returns `ErrIdentityRequired` AND emits `skill.identity_rejected` on the bus. Bus subscriber asserts the emit landed.
  - `skill.pack_overwrite_refused` event emits with `Origin=Generated → existing=Pack` payload.

- **Conformance:**
  - `internal/skills/conformancetest/conformancetest.go` is the harness Phase 49 (Portico) + any future drivers will reuse. Localdb is the first leg.

- **Concurrency / leak:**
  - **D-025 stress:** N=128 goroutines on a single shared `*driver` for 30s; mix Upsert / Get / List / Search / Delete; under `-race`. Asserts: no data races (race detector), no context bleed (per-goroutine identity scope assertions), no cross-cancellation (cancelling one goroutine's `ctx` doesn't affect siblings), no goroutine leak (`runtime.NumGoroutine()` returns to baseline after teardown).

## Smoke script additions

`scripts/smoke/phase-37.sh` is the skeleton until Phase 38 lands the planner-tool surface (which is the smoke-observable boundary). At Phase 37 the script `skip`s with the canonical "phase 37: persistence-only, no smoke-observable surface yet — Phase 38 will land assertions" message; the preflight gate stays green. The script ships from `_template.sh` so Phase 38 can flip the skip to a real assertion in the same PR.

## Coverage target

- `internal/skills`: 85%
- `internal/skills/drivers/localdb`: 85%

## Dependencies

- Phase 01 (events bus surface — `events.EventBus`, `RegisterEventType`, `SafeSealed`).
- Phase 07 (identity quadruple + `ValidateIdentity` analog).
- Phase 15 (SQLite-backed driver shape: WAL + busy_timeout + SetMaxOpenConns(1) + embedded migrations).

## Risks / open questions

- **FTS5 detection.** `modernc.org/sqlite` ships with FTS5 by default, but CI runs need to verify both paths. The driver detects at open via `SELECT * FROM pragma_compile_options WHERE compile_options = 'ENABLE_FTS5'` (or attempts to create the virtual table inside a savepoint and rolls back on failure). A `Skills.ForceRegexFallback bool` config knob is **NOT** introduced — the §4.4 "no parallel implementations" forbidden practice applies; the path branches inside the driver based on detected capability only.
- **Content-hash canonicalisation.** Hash is computed over `(name | title | description | trigger | task_type | sorted(tags) | steps | preconditions | failure_modes | required_tools | required_ns | required_tags)`. Origin / OriginRef / Scope are EXCLUDED so the same skill imported via different paths hashes identically and the LWW gate works on actual content drift. Recorded as D-046.
- **Tags storage.** Tags are stored as JSON array in the `tags` column and ALSO denormalised into a `tags_text` column (whitespace-joined) so the FTS5 virtual table can index them via the porter tokenizer without a separate join. Documented in the migration's comment block.
- **Indexer triggers.** SQLite FTS5 `content='skills' content_rowid='rowid'` external-content model with explicit `AFTER INSERT / DELETE / UPDATE` triggers — `INSERT INTO skills_fts(skills_fts, rowid, ...) VALUES('delete', ...)` for the delete leg so FTS doesn't drift from the parent table.

## Glossary additions

- **SkillStore** — Harbor's identity-scoped, capability-filterable persistence interface for skills. RFC §6.7. The `SkillProvider` name reserved for the planner-facing wrapper Phase 38 ships.
- **Origin** — provenance of a skill: `PackImport` (Skills.md importer) or `Generated` (in-runtime generator).
- **OriginRef** — lineage pointer: `<pack-name>@<version>` for `PackImport`; `gen:{session_id}:{run_id}` for `Generated`.
- **Scope** — skill visibility: `Project | Tenant | Global`. Default for generator-output is `project`.
- **ContentHash** — sha256 over canonicalised skill fields (excludes Origin / OriginRef / Scope). Used for LWW and idempotency.
- **FTS5Ladder** — the three-tier search ranking: FTS5 → regex → exact. Calibrated constants live in brief 04 §4.4; the ladder picks the first path that returns rows.
- **RankingScore** — normalised 0.0–1.0 score; FTS = bm25 → `1/(1+raw)` → min-max; regex = path-specific constants; exact = 1.0.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §6.7`, `brief 04`) resolve
- [ ] Coverage on `internal/skills` + `internal/skills/drivers/localdb` ≥ 85%
- [ ] If multi-isolation paths changed: cross-session isolation test passes (yes — `localdb_test.go` exercises N concurrent identities against one store)
- [ ] Concurrent-reuse test: N≥128 invocations against a single shared instance under `-race` — `internal/skills/drivers/localdb/concurrent_test.go`
- [ ] Integration test: real `events.EventBus` driver wired through the seam, identity propagation asserted, ≥1 failure mode (pack-overwrite refusal) covered, `-race` enabled
- [ ] Glossary updated
- [ ] Brief-finding departures (none) documented above
