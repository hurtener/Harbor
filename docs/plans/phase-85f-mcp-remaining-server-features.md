# Phase 85f — mcp-remaining-server-features

## Summary

Consume the four remaining MCP server-side features Harbor's client driver currently ignores: **completions** (`completion/complete` — autocomplete for prompt and resource-template arguments), **logging** (`logging/setLevel` + `notifications/message` — structured server log ingestion), **resource templates** (`resources/templates/list` — parameterised dynamic resources), and **progress** (`_meta.progressToken` on outbound requests + `notifications/progress` ingestion). Individually minor; collectively a meaningful slice of the "full core MCP client" bar.

## RFC anchor

- RFC §6.4

## Briefs informing this phase

- brief 14
- brief 03
- brief 06

## Brief findings incorporated

- brief 14 §2 (#24, #25, #21, #13): completions, logging, resource templates, and progress are each classified **Absent** — this phase closes all four.
- brief 14 §4 (biggest gaps #8): "No `Unsubscribe`, completions, logging, resource templates — each individually minor, collectively a meaningful slice of consumable server features." — the rationale for bundling.
- brief 14 §4: these four are between Harbor and the "Full core MCP client" claim once 85a–e land.
- brief 06 (events / observability): MCP server logs (`notifications/message`) route into Harbor's telemetry surface, not a parallel log sink — they become Harbor events filterable by the standard attribute set.

## Findings I'm departing from (if any)

- None.

## Goals

- **Completions:** `completion/complete` is callable for `ref/prompt` and `ref/resource` arguments; the Console can offer dropdown-style autocomplete for prompt / resource-template argument entry.
- **Logging:** Harbor sets a server log level via `logging/setLevel`, ingests `notifications/message`, and routes server log records into Harbor's telemetry as `mcp.server_log` events — filterable, redacted via `audit.Redactor`, persistable.
- **Resource templates:** `resources/templates/list` is consumed; URI-template arguments are collected (using `completion/complete` where the server supports it).
- **Progress:** outbound long-running requests carry a `_meta.progressToken`; inbound `notifications/progress` are ingested, rate-limited, and surfaced (as events) for UX.

## Non-goals

- A polished Console autocomplete UX — this phase ships the Protocol/driver surface; the dropdown component rides with the Console wave.
- Progress on Tasks specifically — task progress is covered by Phase 85i (which reuses this phase's progress ingestion).
- Server-log-driven alerting — log ingestion lands as events; alerting policy is operator territory, out of scope.

## Acceptance criteria

- [ ] `completion/complete` is callable; a test against a mock server returns suggestions for a `ref/prompt` argument and a `ref/resource` argument.
- [ ] `logging/setLevel` sets the negotiated server's log level; `notifications/message` records are ingested and emitted as `mcp.server_log` events carrying the standard attribute set + the server identity; payloads pass through `audit.Redactor`.
- [ ] `resources/templates/list` is consumed; a templated resource's arguments are collectable; where the server advertises `completions`, argument autocomplete is wired.
- [ ] Outbound requests for operations a server may run long carry a `_meta.progressToken`; `notifications/progress` for that token are ingested and emitted as `mcp.progress` events.
- [ ] Progress notifications are rate-limited (a documented coalescing window) so a chatty server cannot flood the event bus.
- [ ] Logging respects the negotiated capability — Harbor only calls `logging/setLevel` when the server declared `logging`.
- [ ] All four features are no-ops (not errors) against servers that do not advertise the corresponding capability.

## Files added or changed

- `internal/tools/drivers/mcp/` — new `completions.go`, `logging.go`, `templates.go`, `progress.go` (or a consolidated `server_features.go` — implementer's call).
- `internal/tools/drivers/mcp/mcp.go` — register the `notifications/message` + `notifications/progress` handlers; call `logging/setLevel` post-init for logging-capable servers.
- `internal/tools/drivers/mcp/events.go` — `mcp.server_log` + `mcp.progress` event types.
- `internal/events/` taxonomy — register the two new event types.
- `internal/protocol/` — a read surface so the Console can request completions (finalised against the Console wave).
- Test files — mock server advertising completions / logging / templates / progress.
- `examples/harbor.yaml` — document the server-log level config knob.
- `scripts/smoke/phase-85f.sh`.
- `docs/plans/README.md` — Status flip on merge.

## Public API surface

No new exported MCP-driver types. Two new event types (`mcp.server_log`, `mcp.progress`) in the events taxonomy. A Protocol method for Console-side completion requests (shape finalised with the Console wave; single-source rule applies).

## Test plan

- **Unit:** completion request/response mapping; log-level mapping; URI-template argument collection; progress-token generation + notification matching; rate-limit coalescing.
- **Integration:** mock server advertising all four capabilities; real `audit.Redactor` on the log path; real event bus on the log + progress paths; identity propagation; `-race`.
- **Conformance:** N/A — Phase 85j.
- **Concurrency / leak:** progress-notification flood (assert rate-limit holds, bus not overrun); goroutine-leak baseline.
- **Failure modes:** server lacks each capability (no-op, no error); malformed progress token.

## Smoke script additions

- `scripts/smoke/phase-85f.sh` (classification: `static-only`):
  - Assert the four server-feature source files exist.
  - Assert the events taxonomy contains `mcp.server_log` and `mcp.progress`.

## Coverage target

- `internal/tools/drivers/mcp`: 85%.

## Dependencies

- 28 (MCP driver).
- 85a (the foundation — list-changed + pagination must be sound before adding more consumed surface).

## Risks / open questions

- **Log volume.** A verbose MCP server at `debug` level could flood the event bus. The default negotiated level is `info`; `debug` requires an explicit operator opt-in. Documented.
- **Progress-token lifecycle.** A `progressToken` must remain valid for the request's lifetime (and, post-85i, for a task's lifetime). The token registry must not leak tokens for completed requests — a Close-time sweep is a test case.
- **Redaction on server logs.** Server log payloads are untrusted and may contain secrets; every record goes through `audit.Redactor` before emission. This is a hard criterion, not a nicety.

## Glossary additions

- **MCP completions** — the `completion/complete` capability: a server offers autocomplete suggestions for prompt or resource-template arguments.
- **MCP server logging** — the `logging/setLevel` + `notifications/message` pair: Harbor sets a server's log verbosity and ingests its structured log records as `mcp.server_log` events.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references resolve
- [ ] Coverage ≥ target
- [ ] Cross-isolation test — server logs / progress events are identity-stamped; isolation assertion included.
- [ ] **Concurrent-reuse test passes** — progress-flood + concurrent invokes under `-race`.
- [ ] **Integration test passes** — mock server (four capabilities), real redactor + event bus.
- [ ] Glossary updated.
- [ ] No brief departures.
