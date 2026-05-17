# Phase 69 — `harbor inspect-events` + `harbor inspect-runs`

## Summary

Graduates the two Phase 63 `inspect-*` CLI stubs into real implementations. `harbor inspect-events` tails or snapshots the Phase 60 SSE event stream against a running Runtime with identity / type / run filters; `harbor inspect-runs` derives a per-run summary (list mode) or step trajectory (single-run mode) from the same SSE replay surface. Both subcommands authenticate via a Bearer JWT discovered from `HARBOR_TOKEN` env or `~/.harbor/token` and fail loud when neither is present.

## RFC anchor

- RFC §8 — `harbor` CLI surface (`inspect-events`, `inspect-runs` lines in the settled subcommand table).
- RFC §5.4 — Protocol wire binding (the SSE+REST surface both subcommands consume).
- RFC §5.5 — identity at the Protocol edge (the CLI's mandatory `--tenant/--user/--session` flags).

## Briefs informing this phase

- brief 06 — events surface design + the "one bus, no parallel observability channel" rule. Phase 69 consumes the same SSE stream the Console will read; it does NOT add a new observability channel.
- brief 07 — the runtime owns the Protocol it speaks; CLI is just another Protocol client. The §13 forbidden-practice "Console reads internal Runtime objects" applies symmetrically to the CLI.

## Brief findings incorporated

- **brief 06 §1 ("one bus, no parallel channel")** — `inspect-events` reuses the existing `/v1/events` SSE handler verbatim. No new event-fan-out path, no new internal subscription, no debug endpoint that bypasses redaction.
- **brief 06 §4 (replay-cursor failure modes)** — the CLI honours the SSE `Last-Event-ID` reconnect cursor via `--since`. When the server emits a `: stream.replay_unavailable …` comment (the Phase 60 surface's "I cannot replay" signal), the CLI surfaces it under `--json` as a `{"comment":"…"}` sentinel so scripting consumers see the gap.
- **brief 07 §2 ("the Console is a Protocol client; so is everything else")** — the CLI imports NOTHING from `internal/protocol/transports/stream` beyond what an external Go client would see; the wire `wireEvent` struct is hand-rolled in `cmd/harbor/inspect_common.go` so a transport-shape drift fails a golden test, not a compile.

## Findings I'm departing from (if any)

- The phase 69 acceptance criterion in `docs/plans/README.md` line 733 reads "`harbor inspect-events --session SID --type tool.completed` filters server-side". Phase 69 emits `tool.*` events through the same Phase 60 carrier header (`X-Harbor-Event-Type`), but `tool.completed` is **not yet a registered EventType** in the V1 codebase (it is reserved for the planner-runtime phases that ship tool-dispatch events — RFC §6.4 / Phase 26+). The CLI accepts ANY string for `--type`; the server-side `events.Filter.Types` selector is open-ended (no type whitelist enforced at the bus level). The acceptance criterion is satisfied for any *registered* event type today, and will be satisfied verbatim once Phase 26+ ships `tool.completed`. No design departure — only a clarification of which event vocabulary is live in this phase.

## Goals

- Operator-grade event inspection against a running `harbor dev`: snapshot the replay window or follow live.
- Identity-scoped, JWT-authenticated: no anonymous probes, no silent fallbacks.
- Stable `--json` wire shape for both subcommands so the Console (future) and scripting consumers can read the same output.
- Per-run aggregation derived from the canonical event stream — no new Protocol method, no new persistence index.

## Non-goals

- Bidirectional control: `inspect-runs` does NOT mint, cancel, or replay runs — it READS. Operators use `harbor dev` + the REST control surface for control.
- New persistence: the CLI does not build its own run / trajectory cache. Whatever the bus's retention window holds is what the CLI can see.
- A `runs.list` / `runs.trajectory` Protocol method: deliberately deferred until Phase 72+ (Console subscription protocol surface) introduces a richer per-run query method whose first consumer is the Console's Sessions page.

## Acceptance criteria

- [x] `harbor inspect-events --bind H:P --tenant T --user U --session S --type X --since C --follow=false` snapshots the SSE replay and exits non-zero when the stream rejects (e.g. 401).
- [x] `harbor inspect-events ... --json` emits one canonical `wireEvent` JSON object per line (NDJSON).
- [x] `harbor inspect-runs --bind H:P --tenant T --user U --session S [--json]` aggregates per-run rows from the SSE replay and emits either a human table or a single-line JSON array.
- [x] `harbor inspect-runs <run-id> ... [--json]` filters the replay to one run and emits a trajectory (one row per event) or a `{run_id, steps[]}` JSON object.
- [x] Bearer JWT discovery: `HARBOR_TOKEN` env preferred, `~/.harbor/token` fallback. Missing token at BOTH sources fails loud with `auth_required`.
- [x] Identity triple (`--tenant/--user/--session`) is mandatory at the CLI edge — failing CLI-side with `identity_incomplete` so the error message names the missing flag.
- [x] Golden coverage pins both human and `--json` shapes for each subcommand (4 goldens total).
- [x] Integration test exec's the built binary against an httptest Protocol stack and asserts the wire round-trip — `task.spawned` published over REST → visible in `inspect-events` stdout.

## Files added or changed

- `cmd/harbor/inspect_common.go` — shared token discovery, endpoint composition, SSE frame parser, identity-header injection, fail-loud CLIError codes.
- `cmd/harbor/cmd_inspect_events.go` — graduates the Phase 63 stub; SSE consumer with human + `--json` output modes.
- `cmd/harbor/cmd_inspect_runs.go` — graduates the Phase 63 stub; per-run aggregator (list mode) + trajectory replay (single-run mode).
- `cmd/harbor/inspect_common_test.go` — unit tests for the shared helpers (token resolution, endpoint composition, identity validation, SSE parser, snapshot-on-401).
- `cmd/harbor/cmd_inspect_events_test.go` — golden tests for human + `--json` output; httptest-driven SSE server; type-filter header propagation.
- `cmd/harbor/cmd_inspect_runs_test.go` — golden tests for list + trajectory modes; `CodeRunNotFound` fail-loud; flag-surface parity assertion.
- `cmd/harbor/testdata/golden/inspect-events-human.txt`, `inspect-events-json.txt`, `inspect-runs-list-human.txt`, `inspect-runs-list-json.txt`, `inspect-runs-trajectory-human.txt`, `inspect-runs-trajectory-json.txt` — seeded goldens.
- `cmd/harbor/testdata/golden/help.txt` — refreshed (the stub `(Phase 69)` suffix dropped from the Short descriptions).
- `cmd/harbor/cmd_stub_test.go` — `inspect-events` / `inspect-runs` removed from the stub list (Phase 69 graduation).
- `scripts/smoke/phase-69.sh` — new smoke; runs cmd/harbor tests then drives a live `start` → `inspect-events --follow=false` → `inspect-runs --json` against the preflight server.
- `scripts/smoke/phase-63.sh` — `inspect-events` / `inspect-runs` removed from the stubs array (`§17.6` cross-phase fix).
- `test/integration/phase69_inspect_cli_test.go` — wire round-trip tests; builds the harbor binary, drives it against an httptest Protocol mux that re-uses `phase60Deps`.
- `docs/decisions.md` — appends `## D-101` recording the CLI auth-discovery shape + the "no new Protocol method" choice.
- `docs/plans/README.md` — flips Phase 69 row to `Shipped`.
- `README.md` — Phase 69 status flip + a one-line pointer to the new subcommands.

## Public API surface

CLI subcommands (operator-facing — no Go API change):

```text
harbor inspect-events [--bind H:P] --tenant T --user U --session S
                       [--run R] [--type X ...] [--since CURSOR]
                       [--follow / --follow=false] [--json]

harbor inspect-runs [run-id]
                       [--bind H:P] --tenant T --user U --session S
                       [--since CURSOR] [--json]
```

Internal helpers exported within `cmd/harbor` only (no `internal/` API change):

- `resolveToken(getenv, homedir, readFile) (inspectAuth, error)` — testable token resolver.
- `inspectEndpoint(bind) (string, error)` — bare-host or full-URL accepted.
- `runIDFromEvent(wireEvent) string` — projection that the Console will mirror (RFC §7 Live Runtime page).

## Test plan

- **Unit:** `cmd/harbor/inspect_common_test.go` — token resolver (env-pref, file-fallback, fail-loud-both-empty, empty-file), endpoint composition (host:port, full URL, empty), filter validation (incomplete identity), `applyHeaders` (Bearer + X-Harbor-* + Last-Event-ID + multiple X-Harbor-Event-Type), SSE parser (basic frame, comment frame, EOF), `inspectSSE` (ctx-cancellation cleanup, non-2xx fail-loud).
- **Unit:** `cmd/harbor/cmd_inspect_events_test.go` + `cmd_inspect_runs_test.go` — golden coverage for both output modes of both subcommands (4 goldens), type-filter header propagation, run-not-found path, flag-surface parity.
- **Integration:** `test/integration/phase69_inspect_cli_test.go` — builds `bin/harbor`, stands up `transports.NewMux(..., WithoutValidator())` over real `events`, `state`, `tasks` drivers (re-uses `newPhase60Deps`), drives a real `start`, exec's the binary, asserts the canonical event is in stdout. Includes a fail-loud-no-token assertion.
- **Conformance:** N/A — no new Protocol method shipped (deliberate; see Non-goals).
- **Concurrency / leak:** the CLI is process-lifetime-scoped (one invocation = one stream); not a reusable artifact in the §11 D-025 sense. The `inspectSSE` ctx-cancellation test asserts the read loop exits cleanly.

## Smoke script additions

`scripts/smoke/phase-69.sh` runs:

1. `go test -race ./cmd/harbor/...` (unit + golden coverage gate).
2. Live `POST /v1/control/start` against the preflight `harbor dev` server.
3. `bin/harbor inspect-events --follow=false --json` against the same bind — asserts the `task.spawned` line lands in stdout.
4. `bin/harbor inspect-runs --json` — asserts a non-empty array.
5. `bin/harbor inspect-runs <task-id> --json` — asserts ≥1 step.

`scripts/smoke/phase-63.sh` updated (`§17.6` cross-phase fix): `inspect-events` and `inspect-runs` removed from the stubs array — those two subcommands no longer emit `CodeNotImplemented`.

## Coverage target

- `cmd/harbor`: ≥ 70% (per the Phase 63 / 64 cadence).

## Dependencies

- Phase 63 (CLI skeleton) — the cobra root command and CLIError surface the new bodies plug into.
- Phase 60 (Protocol transports) — the SSE event stream the CLI consumes.
- Phase 61 (Protocol auth) — the Bearer-JWT auth.Middleware the CLI authenticates against.
- Phase 64 (`harbor dev`) — the live server the smoke runs against.

## Risks / open questions

- **Bus retention vs. operator expectations.** `inspect-runs` shows ONLY runs whose events are still in the retained window — a long-finished run whose events were GC'd is invisible. The CLI surfaces this honestly (the human row count + the JSON `[]` empty case + the `run_not_found` error on the trajectory mode), but operators used to a persistent run history will need education. The Phase 57 durable event log addresses retention on the Runtime side; the CLI inherits whatever the server returns.
- **`task.spawned` run-id projection.** Today the Protocol `start` method dispatches `Quadruple{Identity: id}` (no RunID), so the `task.spawned` event lands on the bus with `Event.Identity.RunID == ""`. The CLI falls back to the payload's `TaskID` field. A future Phase 60 stream upgrade that adds payload-aware run filtering on the server side would let `--run` work as a server-side filter; today it's client-side via `runIDFromEvent`. Tracked as an enhancement note in the source comments.

## Glossary additions

None. The subcommands re-use already-glossed terms (run, trajectory, identity triple, SSE).

## Pre-merge checklist

- [x] `make drift-audit` passes
- [x] `make preflight` passes
- [x] `make check-mirror` passes
- [x] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [x] Coverage on touched packages ≥ stated target
- [x] If multi-isolation paths changed: cross-session isolation test passes — N/A (the CLI is per-process, scoped to the operator-supplied triple).
- [x] **If this phase builds a reusable artifact** — N/A (CLI subcommands are not reusable artifacts in the D-025 sense; each invocation is process-lifetime-scoped).
- [x] **If this phase consumes a shipped subsystem's surface OR closes a cross-subsystem seam:** `test/integration/phase69_inspect_cli_test.go` boots the real Phase 60 transports against real `events` / `state` / `tasks` drivers and exec's the built binary against them. Identity propagation asserted; fail-mode (missing token) covered.
- [x] If new vocabulary: glossary updated — N/A.
- [x] If a brief finding was departed from: justified above + decisions.md entry filed (D-101).
