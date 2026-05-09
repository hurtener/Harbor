# Memory + Skills — Phase-Ready Technical Brief

> Internal research note. Phase planning depth, not encyclopedia coverage.
> All concepts re-expressed in Harbor-native vocabulary.
> Source citations point to `~/Repos/Penguiflow/penguiflow/...` for traceability only.

---

## 1. Subsystem overview

Harbor's runtime exposes **two cooperating durable subsystems** that keep the planner cheap and the agent useful across long conversations:

- **Memory** — declared-policy short-term conversational memory. The previous-generation runtime treats memory as the strongest of its seams: configuration (`strategy`, `budget`, `isolation`) is explicit, fail-closed when keys are missing, and pluggable across persistence backends. Harbor inherits this *shape* cleanly and extends the isolation key from `(tenant, user, session)` config-resolved to a first-class `IdentityTriple` carried through `context.Context`.
- **Skills** — token-savvy, DB-backed playbook store with three planner-facing tools (`skill_search`, `skill_get`, `skill_list`) plus a virtual-directory snapshot for cheap browsing. Distinct from Portico's *distribution* role: Portico ships skill packs across tenants; Harbor *consumes* and locally optimizes for retrieval, scoring, capability filtering, and PII/tool-name redaction at injection time.

The single biggest Harbor upgrade in this surface is closing two gaps the predecessor leaves open: (a) no native importer for the open **Skills.md** standard (every external skill needs hand adaptation today), and (b) the in-runtime skill-author tool drafts a skill but cannot persist it (`Do not claim to save or persist anything.` is hardcoded into its prompt — see `skills/tools/skill_propose_tool.py:43-44`).

---

## 2. Key data shapes (Go-flavored sketches)

```go
// internal/runtime/identity/identity.go
type IdentityTriple struct {
    TenantID  string
    UserID    string
    SessionID string
}
func (id IdentityTriple) Composite() string // "{tenant}:{user}:{session}"

// internal/runtime/memory/memory.go
type Strategy string
const (
    StrategyNone           Strategy = "none"
    StrategyTruncation     Strategy = "truncation"
    StrategyRollingSummary Strategy = "rolling_summary"
)

type OverflowPolicy string
const (
    OverflowTruncateOldest  OverflowPolicy = "truncate_oldest"
    OverflowTruncateSummary OverflowPolicy = "truncate_summary"
    OverflowError           OverflowPolicy = "error"
)

type Budget struct {
    FullZoneTurns     int            // recent turns kept verbatim
    SummaryMaxTokens  int
    TotalMaxTokens    int
    OverflowPolicy    OverflowPolicy
}

type IsolationPolicy struct {
    RequireExplicitKey bool // Harbor default: true (fail-closed)
}

type MemoryConfig struct {
    Strategy            Strategy
    Budget              Budget
    Isolation           IsolationPolicy
    SummarizerModel     string
    IncludeTrajectory   bool
    RecoveryBacklogMax  int
    RetryAttempts       int
    RetryBackoffBase    time.Duration
    DegradedRetryEvery  time.Duration
    TokenEstimator      func(string) int // override; default = chars/4 + 1
}

type ConversationTurn struct {
    UserMessage         string
    AssistantResponse   string
    TrajectoryDigest    *TrajectoryDigest
    ArtifactsShown      map[string]any
    ArtifactsHiddenRefs []string
    Timestamp           time.Time
}

type TrajectoryDigest struct {
    ToolsInvoked        []string
    ObservationsSummary string
    ReasoningSummary    string
    ArtifactsRefs       []string
}

type MemoryHealth string // healthy | retry | degraded | recovering

type MemoryStore interface {
    AddTurn(ctx context.Context, id IdentityTriple, turn ConversationTurn) error
    GetLLMContext(ctx context.Context, id IdentityTriple) (LLMContextPatch, error)
    EstimateTokens(ctx context.Context, id IdentityTriple) (int, error)
    Flush(ctx context.Context, id IdentityTriple) error
    Health(ctx context.Context, id IdentityTriple) (MemoryHealth, error)
    Snapshot(ctx context.Context, id IdentityTriple) (MemorySnapshot, error) // protocol export
    Restore(ctx context.Context, id IdentityTriple, snap MemorySnapshot) error
}

// internal/runtime/skills/skills.go
type Skill struct {
    ID             string
    Name           string // unique per scope
    Title          string
    Description    string
    Trigger        string // non-empty; planner-visible match cue
    TaskType       string // browser | api | code | domain | unknown
    Tags           []string
    Steps          []string // non-empty
    Preconditions  []string
    FailureModes   []string
    RequiredTools  []string
    RequiredNS     []string
    RequiredTags   []string
    Origin         Origin // PackImport | Generated
    OriginRef      string // pack name or generator session
    Scope          Scope  // Project | Tenant | Global
    ScopeTenantID  string
    ScopeProjectID string
    ContentHash    string // sha256 of canonical payload
    CreatedAt      time.Time
    UpdatedAt      time.Time
    LastUsed       time.Time
    UseCount       int
    Extra          map[string]any
}

type SkillProvider interface {
    GetRelevant(ctx context.Context, q SkillQuery, cap CapabilityContext) (Retrieval, error)
    Search(ctx context.Context, q SkillSearchQuery, cap CapabilityContext) (SearchResponse, error)
    GetByName(ctx context.Context, names []string, cap CapabilityContext) ([]SkillDetail, error)
    List(ctx context.Context, req ListRequest, cap CapabilityContext) (ListResponse, error)
    Directory(ctx context.Context, cfg DirectoryConfig, cap CapabilityContext) ([]DirectoryEntry, error)
    FormatForInjection(skills []SkillDetail, maxTokens int) (text string, raw int, final int, summarized bool, err error)
}

type SkillProviderFactory func(cfg SkillsConfig) (SkillProvider, error)

// VirtualDir is a *logical namespace*: pinned + recent (or top) entries,
// max_entries-bounded, identity-scoped, with PII + tool-name redaction baked in
// before injection. Backing storage is a driver (LocalDB, Portico, ...).
type VirtualDir struct {
    Pinned          []string
    MaxEntries      int            // default 30, ge=1 le=200
    IncludeFields   []string       // name,title,trigger,task_type
    Selection       string         // pinned_then_recent | pinned_then_top
}
```

---

## 3. Public API surface

**Planner-facing tools** (always-loaded; the planner sees them by name; capability + identity context is injected by the runtime, never by the model):

- `skill_search(query, search_type, limit, task_type?) -> SearchResponse` — ranked candidates with `name/title/trigger/task_type/score`. See `skills/tools/skill_search_tool.py`.
- `skill_get(names[], format=injection|raw, max_tokens) -> {skills, formatted_context}` — full content fetch with tiered downsizing (drop optional → cap steps to 3) until the budget fits. See `skills/tools/skill_get_tool.py:1-45` and the injection formatter at `skills/provider.py:248-360`.
- `skill_list(page, page_size, task_type?, origin?) -> ListResponse` — paged enumeration filtered by capability.
- `skill_propose(source_material, hints) -> SkillDraft` (Harbor adds: `&& persist?: bool=false, scope?: Scope`) — the predecessor stops at draft; Harbor extends with **explicit persistence** that respects the identity triple.

**Author/operator surface:**

- File-based skill packs: `*.skill.md` (frontmatter), `*.skill.yaml`, `*.skill.json`, `*.skill.jsonl`. See `skills/pack_loader.py:18-23`.
- Harbor adds: `*.md` Skills.md-standard files (no `.skill` infix) recognized natively.
- Skills config (YAML, embedded in agent config):

```yaml
skills:
  enabled: true
  cache_dir: .harbor
  max_tokens: 2000
  redact_pii: true
  scope_mode: project   # project | tenant | global
  packs:
    - name: ops
      path: ./skills/ops
      scope_mode: project
      prune_missing: true
  directory:
    enabled: true
    max_entries: 30
    selection_strategy: pinned_then_recent
  proposal:
    enabled: true
    persistence: true   # Harbor-only flag
  fts_fallback_to_regex: true
  top_k: 6
  prune_packs_not_in_config: true
```

**Protocol surface (for Console / third-party clients):**
read-only. Snapshots only: `MemorySnapshot`, `SkillIndexHealth`, `RecentSearches`, `DirectoryEntries`. Console never writes; mutation flows back through tools or operator endpoints. Identity flows through the protocol; cross-session reads require an `admin` scope on the principal.

---

## 4. Internal mechanics

### 4.1 Memory strategies

- **`none`** — `AddTurn` is a no-op; `GetLLMContext` returns empty.
- **`truncation`** — append turn → keep last `FullZoneTurns` verbatim → enforce `TotalMaxTokens` per `OverflowPolicy`. Synchronous.
- **`rolling_summary`** — append turn → evict older turns into `pending` → schedule background summarization. Summarizer is an injectable async callable: input `{previous_summary, turns}`, output `{summary: string}`. The runtime spawns one task at a time per memory key and lock-protects all state. See `planner/memory.py:544-606`. Health states (`healthy → retry → degraded → recovering → healthy`) gate behavior: in `degraded`, the memory falls back to truncation-style and queues a recovery loop bounded by `RecoveryBacklogMax`.
- **Failure semantics:** summarizer exceptions never leak. The store falls to `degraded`, drops summarization, keeps the conversation usable from a recent window, and emits a `memory.health_changed` event the Console can render.

### 4.2 Identity-keyed isolation (fail-closed)

The Harbor invariant: **if the identity triple is incomplete, the operation behaves as if memory is disabled and emits an audit event**, never returns data scoped to a default. The predecessor enforces this with `require_explicit_key=True` in `MemoryIsolation`; Harbor makes it the only mode by removing the toggle. See `planner/memory.py:77-94`. Memory keys are `composite = "tenant:user:session"`; persistence stores key it directly. Cross-key reads are impossible by API construction.

### 4.3 Skill store internals

Reference implementation in `skills/local_store.py:26-450`:

- SQLite schema with a `skills` table keyed by `id` (sha256-derived) and unique on `name`. Columns include `scope_mode`, `scope_tenant_id`, `scope_project_id`, `task_type`, `tags` (JSON), `steps` (JSON), `preconditions/failure_modes` (JSON), `origin`, `origin_ref`, `content_hash`, lifecycle timestamps, `use_count`, `extra` (JSON).
- FTS5 virtual table (`skills_fts` over `name|title|trigger|description|tags`, porter unicode61 tokenizer) with INSERT/DELETE/UPDATE triggers maintaining sync. Falls back to regex/exact when FTS5 is unavailable (`_ensure_fts` returns false → `_fts_fallback_to_regex` toggles the path). Harbor: keep the same fallback ladder, ship `modernc.org/sqlite` (CGo-free) and verify FTS5 is compiled in.
- WAL journal mode set on every connection.

### 4.4 Search ranking

- **FTS path** (`skills/local_store.py:670-742`): tokenize via `[A-Za-z0-9]+`, run strict AND first, then OR fallback if no rows. Score is `bm25 → 1/(1+raw) → min-max normalized 0..1`.
- **Regex path** (`skills/local_store.py:830-863`): try compiling the query; for queries with whitespace, fall back to OR-of-tokens regex (NL queries are rarely intentional regex). Scoring: `name fullmatch=0.95`, `name match=0.90`, `name search=0.85`, body `search=0.75`.
- **Exact path** (`skills/local_store.py:810-827`): lowercase equality on `name|title|trigger|tags`.

Harbor port keeps the same three-tier fallback and the same scoring constants — it's calibrated for this corpus and easy to test.

### 4.5 Capability filtering + redaction (injection-time)

- The runtime computes `CapabilityContext{allTools, allowedTools, allowedNamespaces, allowedTags}` from the planner's tool-visibility decisions.
- `_skill_is_applicable` (`skills/provider.py:156-166`) gates each candidate: the skill's `RequiredTools/Namespaces/Tags` must be subsets of the allowed sets.
- Disallowed tool names are scrubbed from skill text before injection; replacement is `"a suitable tool (use tool_search)"` when search is available, else `"a suitable tool"`. PII redaction (email/phone/bearer-tokens/URL query strings) runs over titles/triggers/steps/preconditions/failure_modes when `redact_pii=true`. See `skills/redaction.py` and `skills/provider.py:189-245`.
- **Tiered injection budgeter:** start full → drop optional (preconditions, failure_modes) → cap steps to 3. Stop at the first attempt that fits within `max_tokens`. See `skills/provider.py:248-360`.

### 4.6 Virtual directory

The "virtual directory" (in Harbor terms `VirtualDir`) is a small, identity-scoped, capability-filtered snapshot of the catalog for cheap browsing. The provider blends pinned skills (config-declared) with either *most-recently-used* or *most-frequently-used* up to `max_entries`. Output is `[]DirectoryEntry{name, title?, trigger?, task_type?}`, redacted before injection. See `skills/provider.py:685-751`. The virtual-directory pattern is the right *user-visible namespace* abstraction even when the backing storage swaps (LocalDB ↔ Portico ↔ Git ↔ OCI) — that pluggability is what Harbor preserves.

### 4.7 Skills.md import pipeline (Harbor-only)

Today the predecessor's `.skill.md` reads only YAML frontmatter into a dict and validates against a custom schema (`skills/pack_loader.py:32-87`). The open **Skills.md** standard (Anthropic-style) carries authorial procedure prose in the markdown *body* with frontmatter that is intentionally lean (`name`, `description`, optional `license`, optional `allowed-tools`). Harbor's importer must:

1. **Parse** YAML frontmatter + markdown body via a deterministic CommonMark-only parser.
2. **Normalize** body sections into Harbor's structured fields. Headings like `## Steps`, `## Preconditions`, `## Failure modes` map to lists; absent sections default to empty. The body's narrative becomes `description`. `name` is taken from frontmatter; if missing, derived from filename via slugify (mirroring `pack_loader._slugify`).
3. **Resolve** sibling resource files (e.g. `examples/`, `assets/`) referenced by relative path; record them as `Extra.attachments` for later injection by `skill_get`.
4. **Validate + index** via the same `Skill` validator the loader uses; fail loud on a non-empty `trigger` and non-empty `steps`.
5. **Round-trip** test: any spec-compliant Skills.md imports without source edits and re-exports byte-stable.

This is where the predecessor's per-skill manual-adaptation gap is closed once and tested as an invariant.

### 4.8 Generator with persistence (Harbor-only)

The predecessor's `skill_propose` tool calls the LLM with a JSON-schema-constrained `SkillProposalDraft`, emits a `skill_propose` planner event, and **stops at draft** (the system prompt explicitly forbids claiming persistence — `skills/tools/skill_propose_tool.py:43-44`). Harbor adds a **persist phase**:

- New tool (or `skill_propose` with `persist=true`): validates the draft via the same `SkillDefinition` validator the importer uses, stamps `Origin=Generated`, stamps `OriginRef = "gen:{session_id}:{trace_id}"`, scopes by the operator-provided `Scope` (default to current project), and inserts via the `LocalSkillStore.upsert_pack_skill` equivalent. Conflict policy: refuse to overwrite a `Origin=PackImport` skill with the same `name` (matches the predecessor's `existing_origin != "pack"` guard at `local_store.py:124`). For `Origin=Generated → Origin=Generated`, last-write-wins gated by `content_hash` change.
- Audit: record `(actor=identity_triple, action="skill.created", skill_id, content_hash, source_excerpt_hash)`. Console renders this in the activity timeline.

---

## 5. Sharp edges from the source

- **Memory page moved.** `docs/MEMORY_GUIDE.md` (top-level) is a stub redirect to `docs/planner/memory.md`. Indicates a redesign mid-life. Harbor lesson: get this right in the RFC and don't ship the redirect.
- **Migration document is real.** `docs/migration/MEMORY_ADOPTION.md` walks adopters from "stateless" → truncation → rolling summary → persistence. Each step has explicit safeguards. Harbor inherits the *staging* idea as the V1 default rollout for any production agent.
- **`llm_context` vs `tool_context` separation is the load-bearing decision.** Identifiers (tenant/user/session) live in `tool_context` (LLM-invisible); `conversation_memory` lives in `llm_context`. Harbor must keep this split — the Go analogue is "identity flows via `context.Context`, never through prompt-visible state."
- **Per-skill adaptation is the real gap.** No code in `skills/pack_loader.py` knows the open Skills.md format; every external skill must be reshaped by hand into the predecessor's frontmatter dialect. Closing this is a Harbor-defining feature.
- **Generator is intentionally crippled today.** The system prompt at `skills/tools/skill_propose_tool.py:43-44` prevents the LLM from claiming persistence because the runtime cannot back the claim. Harbor inverts this — runtime ships persistence, prompt is updated, audit is mandatory.
- **`require_explicit_key=True` is *configurable* in the source** (`planner/memory.py:85`). Harbor removes the knob: identity is mandatory, period.
- **FTS5 is conditionally available.** Skill search must work even on SQLite builds without FTS5. Test the regex/exact fallback ladder with `FTS5=off` builds in CI.

---

## 6. Tests required

**Unit:**
- Memory strategy matrix: `none|truncation|rolling_summary` × `(empty|short|over-budget|degraded)`. Property test: turns added in arbitrary order produce identical `GetLLMContext` outputs given the same final state.
- Budget enforcement: `OverflowPolicy` semantics for each policy, including `error` raising loudly.
- Token estimator override is honored.
- Skill upsert: insert / update via `content_hash` change / no-op when hash equal / refuse cross-origin overwrite.
- Search ranking: golden tests with frozen scoring constants for FTS / regex / exact paths, including FTS-off fallback.
- Skills.md round-trip: parse → normalize → re-emit byte-stable. Negative tests for missing `trigger` / empty `steps` / non-CommonMark input.
- Capability filter: skill filtered out when `required_tools` not subset of `allowed`; redacted text strips disallowed names; `tool_search`-aware replacement string.
- Virtual directory: pinned-then-recent vs pinned-then-top selection; respects `max_entries`; identity-scoped.

**Integration:**
- Memory persistence round-trip via `Snapshot`/`Restore` against in-memory, SQLite, and Postgres drivers.
- Generator end-to-end: draft → validate → persist → re-discover via `skill_search`.
- Skills.md import golden corpus: ship N spec-compliant fixtures (with attachments) and assert byte-stable normalization.

**Isolation conformance (Harbor mandate):**
- Two concurrent goroutines on the same `MemoryStore` with different `IdentityTriple`s never observe each other's turns, summary, pending, or backlog.
- Same user across two sessions sees zero cross-session content.
- Generator output scoped to session A is not discoverable from session B unless promoted to project/tenant scope by an explicit operator action.
- Fail-closed: `MemoryStore` operation with a missing `SessionID` returns no data and emits an audit event.

**Cross-session no-leak (race):**
- 100 concurrent sessions × random AddTurn / GetLLMContext / Snapshot for 30s under `-race`. Final invariant: every `GetLLMContext` output's identity matches the caller's identity exactly.

---

## 7. Phase decomposition suggestion (8 phases)

Each phase is one shippable subsystem with tests + smoke + protocol surface + docs.

- **M-1. Memory store interface + in-memory driver + identity scoping.** Defines `MemoryStore`, `IdentityTriple`, `MemoryConfig`, fail-closed behavior. Strategy = `none` only. Conformance harness lands here so later drivers reuse it.
- **M-2. Memory strategies: `truncation` and `rolling_summary` with health/recovery.** Including injectable summarizer interface (LLM call lives in the LLM-client subsystem; memory only consumes a `Summarizer` callable).
- **M-3. SQLite + Postgres memory drivers.** Single conformance suite green on all three.
- **S-1. Skill store interface + LocalDB driver + provider/factory.** Schema, upsert, get-by-name, paged list, identity-scoped query helper. SQLite-only first to keep the surface small; FTS5 + regex + exact ladder; capability filter; PII + tool-name redaction at injection time.
- **S-2. Planner-facing tools.** `skill_search`, `skill_get`, `skill_list` with capability + identity context wired through. Includes the tiered injection budgeter.
- **S-3. Virtual-directory subsystem.** `Directory(cfg)` API + `pinned_then_recent` / `pinned_then_top` selectors, with a config schema and Console projection.
- **S-4. Skills.md importer (the gap-closer).** Spec-compliant parser, normalizer, attachment resolver, golden round-trip suite.
- **S-5. In-runtime skill generator with persistence.** Draft → validate → upsert with `Origin=Generated` and audit events; conflict policy with PackImport; `skill_propose(persist=true)` flag.
- **S-6. Portico provider driver.** `SkillProvider` driver consuming a Portico-distributed pack via MCP; reuses the same factory + capability + redaction pipeline.

(8 phases as committed: M-1, M-2, M-3, S-1, S-2, S-3, S-4, S-5. S-6 is post-V1 unless Portico's MCP surface lands in the same window — flag in the RFC.)

---

## 8. Cross-subsystem dependencies

- **Persistence layer (research note 05).** `MemoryStore` and `SkillProvider` LocalDB driver depend on the conformance-tested triad (in-memory / SQLite / Postgres). Coordinate column types and migration tooling there.
- **LLM client (research note 03).** `rolling_summary` requires an injectable `Summarizer`; the runtime does not own the prompt or the model — it receives a callable. Generator persistence is independent of the LLM choice.
- **Planner (research note 02).** Consumes both subsystems via runtime contracts, never via direct struct access. The planner provides tool-visibility decisions; the skills subsystem applies them as `CapabilityContext`. Memory is injected into the planner's `llm_context` patch; identity stays in `tool_context`.
- **Tool catalog (research note 03).** `skill_search` / `skill_get` / `skill_list` / `skill_propose` are tools; they register through the catalog like any other tool — there is no special skills-only path.
- **Protocol layer (research note 06).** Console reads memory health, virtual-directory snapshots, skill index size, recent searches, generator audit events. Read-only. Mutations go through tools or operator endpoints; Console never bypasses identity.
- **Audit (cross-cutting).** Every generator persist + every fail-closed memory denial emits an audit event; the audit subsystem is the consumer.

---

## 9. Open questions for the user

1. **Skill versioning model.** Today the predecessor uses `content_hash` for change detection but has no explicit version field; cross-pack name collisions are origin-gated. For Harbor: do we ship semver-style explicit versions on each skill, or stick with content-hash-as-version and rely on `OriginRef` for lineage? Versioning matters once Portico distribution lands (rolling forward across tenants).
2. **Conflict policy when a Portico-distributed skill collides with a locally generated skill of the same `name`.** Options: (a) refuse to import (Portico cannot overwrite Generated), (b) refuse to generate (Generated cannot overwrite Portico), (c) namespace by source so collisions are impossible, (d) operator-resolved with a Console diff. The predecessor's `existing_origin != "pack"` short-circuit suggests (a) by default — confirm.
3. **Memory budget at very long sessions.** `rolling_summary` is fine for hours; what about days or weeks? Do we need a tier above (e.g. *episodic memory* — durable summaries promoted from session to user scope) at V1, or is that explicitly a post-V1 satellite (Harbor Memory in the ecosystem map)?
4. **Generator scope default.** When an agent calls `skill_propose(persist=true)` mid-session, should the default scope be `project` (matches predecessor `scope_mode`), `session-only` (most isolated, requires explicit promotion), or *no default* (operator config required)?
5. **Skills.md attachment policy.** Skills.md often references sibling files (examples, assets). Do we (a) inline at import (simple, blows up the row), (b) store as artifact references (clean but couples to artifact subsystem), or (c) keep filesystem-backed and re-resolve at injection (fast but breaks once skills move between machines)? Recommend (b); confirm.
