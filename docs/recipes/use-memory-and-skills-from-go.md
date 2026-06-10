# Use memory and skills from Go

How a headless Go consumer drives Harbor's skills surface end-to-end —
ingest, retrieve, inject — using the SAME implementations the `harbor`
binary wires (Phase 111d, D-201: there is exactly one skills surface;
the CLI verbs and the dev binary are thin callers over the functions
below).

All snippets assume the module `github.com/hurtener/Harbor` and the
imports they name. Pair this with
[Embed Harbor headless](embed-harbor-headless.md) for the full-stack
assembly; this recipe stays on the skills seam.

## 1. Open the store

The `SkillStore` is a §4.4 driver seam. The production driver is
`localdb` (SQLite + FTS5, CGo-free); register it via the production
aggregator and open through the factory:

```go
import (
    _ "github.com/hurtener/Harbor/internal/drivers/prod" // driver registrations

    "github.com/hurtener/Harbor/internal/skills"
)

store, err := skills.Open(ctx, skills.ConfigSnapshot{
    Driver: "localdb",
    DSN:    "/var/lib/harbor/skills.sqlite",
}, skills.Deps{Bus: bus}) // bus: your events.EventBus — identity rejections + audit emits land there
if err != nil { /* fail loud */ }
defer store.Close(ctx)
```

Every store method takes an `identity.Quadruple` — skills are scoped
to `(tenant, user, session)` and the driver fails closed on a missing
component.

## 2. Ingest — `importer.ImportAndStore`

The one-call ingest path `harbor skill import` wraps:

```go
import "github.com/hurtener/Harbor/internal/skills/importer"

report, err := importer.ImportAndStore(ctx,
    identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"},
    store,
    importer.Deps{Store: artifactStore}, // attachments upload here
    "./skills/triage-incident.skill.md",
    // importer.WithOverwrite(), // opt-in: replace an existing same-name skill
)
if err != nil { /* duplicate name, invalid frontmatter, path escape — all loud */ }
fmt.Printf("imported %s (%d steps)\n", report.Name, report.Steps)
```

Conflict policy: duplicate names reject with
`importer.ErrDuplicateSkillName` unless `WithOverwrite()` is passed;
pack-origin rows are protected from non-pack overwrites at the store.

## 3. Retrieve — the Phase-38 handlers

The planner-facing retrieval handlers (`SearchHandler` / `GetHandler` /
`ListHandler` in `internal/skills/tools`) are the SAME functions the
binary registers behind the `skill_search` / `skill_get` / `skill_list`
built-ins — capability filter, tool-name redaction, and the `skill_get`
token budgeter included:

```go
import skilltools "github.com/hurtener/Harbor/internal/skills/tools"

res, err := skilltools.SearchHandler(ctx, store, bus, skilltools.SearchArgs{
    Query: "triage ticket",
    Capability: skilltools.CapabilityContext{
        AllowedTools: tools.VisibleNames(catalog, tools.CatalogFilter{
            TenantID: "t", UserID: "u", SessionID: "s",
        }),
    },
})
```

The `Capability` envelope is default-deny: a skill whose
`required_tools` are not a subset of `AllowedTools` is invisible.
`tools.VisibleNames` is the canonical producer of that set — the same
helper the binary's built-ins and run loop use. If you register the
handlers on your own `ToolCatalog` instead of calling them directly,
use `skilltools.Register(catalog, store, skilltools.Deps{Bus: bus})`
(and `generator.Register` for `skill_propose`).

## 4. Inject — the skills directory

The per-turn `<skills_context>` prompt block is produced by the
Phase-39 virtual directory — a bounded, pinned-then-recent,
capability-filtered, redacted browse window:

```go
dir, err := skills.NewDirectory(store, skills.Deps{Bus: bus}, skills.DirectoryConfig{
    Pinned:     []string{"triage-incident"}, // anchored first, declaration order
    MaxEntries: 10,
    Selection:  skills.SelectionPinnedThenRecent,
})
// per run:
views, err := dir.View(runCtx, skills.DirectoryCapability{AllowedTools: visibleNames})
rc.SkillsContext = runctx.ProjectSkillsDirectory(views) // → planner.RunContext
```

(`skills.DirectoryFromConfig(cfg.Skills, fallbackMax)` projects the
`skills.directory` yaml block onto `DirectoryConfig` when you load
operator config.) The view is compact — name / title / trigger /
task type / pinned — full bodies stay behind `skill_get`. If you run a
custom retrieval strategy instead, `runctx.ProjectSkillsContext`
projects raw `[]skills.RankedSkill` search results onto the same
`RunContext.SkillsContext` field.

## 5. Memory (the adjacent seam)

Memory follows the same shape: `memory.Open(ctx, memory.SnapshotFromConfig(cfg.Memory), memory.Deps{State: stateStore, Bus: bus, Summarizer: s})`,
then per turn `GetLLMContext` → `runctx.ProjectMemoryBlocks` →
`RunContext.MemoryBlocks`, and `AddTurn` on completion. See
[Embed Harbor headless](embed-harbor-headless.md) for the wiring in
context.

## See also

- `docs/skills/configure-memory-and-skills/SKILL.md` — the operator
  playbook (CLI verbs, yaml blocks, failure modes).
- RFC §6.7 — the skills subsystem design.
- `docs/CONFIG.md` — `skills.*` reference.
