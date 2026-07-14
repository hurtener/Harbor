# V1.15 Main-Rebase Delta

This dossier was rebased from `f69ad49` onto `aad8a59a`. The intervening v1.14
fixes materially improve several Protocol projections that the future v1.15
generic TUI will consume. They do not change the client-only architecture or
the non-coding-agent scope.

## 1. Current Wire Inventory

| Surface | Before rebase | Current main |
|---|---:|---:|
| Canonical methods | 113 | 116 |
| Events | 127 | 131 |
| Canonical wire types | 315 | 324 |
| Error codes | 12 | 13 |
| Capabilities | 7 | 8 |

Protocol version remains `0.1.0`. Captured fixtures for a future TUI must use
the current wire digest from the generated manifest rather than hard-code the
value in research prose.

## 2. Closed-Session Reactivation

`start` on a closed, non-erased session now:

- reactivates the same session;
- preserves durable history; and
- emits `session.reopened`.

An erased ID returns HTTP 409 with `session_erased`. There is no separate
reopen method.

TUI behavior:

- show Resume for a closed session;
- await `session.reopened` before presenting it as active;
- treat erased state as a terminal tombstone;
- explain that the prior conversation was deleted; and
- offer Start Fresh with a new ID only after that explanation.

Sources:

- `internal/sessions/events.go:16-24,93-113`
- `docs/site/protocol/events.md`
- `docs/site/protocol/errors.md`

## 3. Session Projection Enrichment

Session rows now project:

- task count;
- event count;
- total cost;
- total tokens;
- pending-intervention state;
- failed-task state; and
- `counters_partial` for bounded lower-bound scans.

Agent binding fields are nullable where the Runtime cannot honestly bind one.
The TUI can use these server-authoritative rows in the session picker/sidebar
instead of reconstructing every count from local history.

This does not provide ordered conversation turns or durable user-message IDs.

Sources:

- `internal/protocol/types/sessions.go:191-259`
- `internal/sessions/protocol/filter.go:38-43`

## 4. Projection Completeness Gate

The new build gate detects never-assigned fields and requires production
wiring coverage for flows, memory, sessions, tasks, and tools.

This narrows false-absence risk and strengthens the decision to consume wire
projections faithfully. It does not cover transcript-part semantics or replace
shared Console/TUI reducer fixtures.

Source: `internal/protocol/projectioncheck/projectioncheck.go`.

## 5. Tool Annotations

The `tool_annotations` capability indicates production annotator wiring. When
present, tool projections can carry real:

- OAuth state;
- approval policy;
- last-used time;
- error-rate metrics;
- offloaded-content statistics;
- approval-policy mutation; and
- OAuth revocation.

`aggregates_partial` distinguishes annotator unavailability. It does not reveal
the annotator's bounded event scan, so metrics and histograms can still
undercount without a wire warning. Label them best-effort until a completeness
marker exists.

Sources:

- `internal/tools/annotate/annotate.go`
- `internal/runtime/serve/mux.go:299-330`
- `internal/protocol/types/version.go:420-455`
- `internal/protocol/types/tools.go`

The per-invocation tool replay gap remains open: no stable generic record yet
combines call ID, redacted typed input, result reference/preview, planner
parent, and parallel grouping.

## 6. Retention And Aggregate Honesty

New signals relevant to diagnostics and reconciliation:

- `runtime.health.retention[].scope`;
- fleet-authorized Runtime-wide task/session horizons;
- stable `events.aggregate.anchor` grids;
- aggregate `truncated`; and
- optional tenant-attributed aggregate counts.

The TUI must display historical partiality rather than equate a live stream
with complete retained history.

Sources:

- `internal/protocol/types/posture.go:119-189`
- `internal/protocol/types/events.go:152-281`

## 7. New Advanced Methods And Events

Methods:

- `agent_config.set_mcp_discovery_origins`
- `agent_config.set_oauth_provider`
- `agent_config.remove_oauth_provider`

Events:

- `mcp.connection.discovery_origins_set`
- `agent_config.oauth_provider.installed`
- `agent_config.oauth_provider.removed`
- `session.reopened`

The first three method/event families belong to advanced administration, not
the initial conversation loop.

## 8. Remaining Honesty Gaps

### Task Cost

The production task enricher still returns an empty cost rollup and no planner
snapshot. `tasks.get` is authoritative for result and terminal state, not yet
for per-task/per-step cost. Live `llm.cost.recorded` events may be displayed as
observed data without claiming durable completeness.

Source: `internal/runtime/serve/enricher.go:110-126`.

### Bounded Tool Analytics

The production annotator reads a bounded event window, while tool metric and
content rows lack a corresponding truncation marker. `aggregates_partial`
means annotator unavailable, not bounded-scan truncation.

Sources:

- `internal/tools/annotate/annotate.go:126-130`
- `internal/tools/annotate/metrics.go:31-53`
- `internal/protocol/types/tools.go`

### Memory Facets

The dead TTL facet and expiring-in-one-hour aggregates are removed. Agent-ID
memory filtering remains unsupported because memory turns carry no producer
agent identity. The TUI must not recreate those controls.

## 9. Unchanged Principal Gap

No canonical ordered conversation/part read model landed. The Console still
joins flat state history and task rows into client-owned turns. The future TUI
still needs:

- a pure reducer;
- shared language-neutral Console/TUI fixtures;
- generation-fenced snapshot reconciliation;
- durable-gap handling; and
- an unknown-event fallback.

A wire-level transcript projection remains a future RFC candidate only after
measured reducer drift.

## 10. V1.15 Planning Implications

1. Rebaseline fixtures to the current method/event/type/error/capability set.
2. Use `start` for both fresh and closed-session turns.
3. Handle `session.reopened` and typed `session_erased` explicitly.
4. Consume `counters_partial`, retention scope, and aggregate truncation.
5. Gate rich tool posture on `tool_annotations`.
6. Label tool analytics best-effort.
7. Do not promise authoritative task cost.
8. Keep the ordered transcript/durable user-turn gap as the primary Protocol
   candidate.
9. Preserve the non-coding-agent boundary: no repository tree, Git/diff,
   shell/PTY, source editing, worktrees, LSP, or coding-specific tool suite.
