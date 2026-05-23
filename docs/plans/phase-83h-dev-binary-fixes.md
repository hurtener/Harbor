# Phase 83h — dev-binary-fixes

## Summary

Phase 83h bundles two hard-block bugs surfaced by the v1.1 operator-
validation work after 83g landed: (V1) the hot-reload watcher
reboot-loops the `harbor dev` binary every ~700ms when the configured
state / skills drivers are SQLite, because the WAL/SHM/journal
sidecars trigger fsnotify on every commit; (V2) the LLM safety
wrapper rejects every real-bifrost `Complete` call with
`CompleteRequest.Model is empty` because the react planner builds
the request without setting `Model` and no upstream defaults it from
the agent-configured `cfg.Model`. The mock LLM driver used in every
existing integration test does not enforce `Model`, so the gap
escaped Wave 13/14/15 checkpoints. Both fixes are small (filter on
shouldTrigger + a 3-line default-fill); together they unblock real-
bifrost dev-binary runs.

## RFC anchor

- RFC §6.5 — LLM subsystem (the safety wrapper's structural
  validation is the documented gate for `CompleteRequest`).
- RFC §8 — CLI / `harbor dev` (the hot-reload watcher's operator
  contract).

## Briefs informing this phase

- brief 02
- brief 04

## Brief findings incorporated

- brief 02 §6: the runtime fills in defaults at boundaries the
  planner cannot reach. 83h applies the same principle to the LLM
  edge: the planner sets nothing it cannot reasonably know; the
  safety wrapper fills the configured default before the
  driver-side call.
- brief 04 §3: SQLite's WAL mode is the dev-binary default and
  the WAL/SHM files are write-heavy. 83h's hot-reload filter
  accepts that and skips the sidecars deterministically.

## Findings I'm departing from (if any)

None. 83h is a pure consumer-side fix against established
primitives. The fixes match the §13 "test stubs as production
defaults" failure mode read one layer over (the integration tests
all used the mock LLM; production behavior differs); the audit
trail is the v1.1 operator-validation findings (V1, V2).

## Goals

- `harbor dev` boots cleanly against the scaffold-default config
  (sqlite state + sqlite-backed localdb skills) WITHOUT requiring
  `--no-hot-reload`. The watcher ignores SQLite engine sidecars.
- Real-bifrost runs reach the model with `req.Model` defaulted from
  the agent-configured `cfg.Model` when the caller (react planner)
  did not pin one. Existing callers that DO pin `Model` keep
  their pin (the fill is `if req.Model == ""`).
- Unit tests for both fixes — the regressions are the kind that
  recur silently if the test fixtures don't exercise them.

## Non-goals

- Operator-configurable hot-reload ignore patterns. The fixed-list
  approach unblocks v1.1; extending to a YAML-supplied glob list
  is a follow-up if pain accrues.
- Reworking the react planner to pin `Model` directly. The
  wrapper-side default is the right home: a future planner concrete
  may legitimately leave `Model` empty (multi-agent, dynamic model
  selection) and rely on the wrapper.
- Console MCP-page mount (83g's deferred work) — separate phase.
- MCP reconnect-on-drop — separate phase.

## Acceptance criteria

- [ ] `cmd/harbor/cmd_dev_hot_reload.go::shouldTrigger` skips events
      whose `Name` ends with any of `dbSidecarSuffixes` (`.sqlite-wal`,
      `.sqlite-shm`, `.sqlite-journal`, `.db-wal`, `.db-shm`,
      `.db-journal`, `-journal`).
- [ ] `internal/llm/safety.go::Complete` fills `req.Model = c.cfg.Model`
      when the caller did not pin one, before `validateRequest`.
- [ ] `TestShouldTrigger_SkipsDBSidecars` in
      `cmd/harbor/cmd_dev_hot_reload_test.go` covers every sidecar
      suffix + asserts non-sidecar paths still fire.
- [ ] `TestSafety_DefaultsModelFromConfigSnapshot` in
      `internal/llm/safety_test.go` asserts a Model-less request
      reaches the driver after the default-fill.
- [ ] `scripts/smoke/phase-83h.sh` asserts the helper symbols
      (`dbSidecarSuffixes`, `isDBSidecar`, the safety default-fill
      block) exist so they cannot silently disappear.
- [ ] Operator validation against `~/harbor-validation/media-helper-agent/`
      reaches a successful first planner step (manually verified
      post-merge — no longer the operator's job to pass
      `--no-hot-reload`).

## Files added or changed

- `cmd/harbor/cmd_dev_hot_reload.go` — add `dbSidecarSuffixes` +
  `isDBSidecar`; call from `shouldTrigger`.
- `cmd/harbor/cmd_dev_hot_reload_test.go` — `TestShouldTrigger_SkipsDBSidecars`.
- `internal/llm/safety.go` — default `req.Model` from `c.cfg.Model`
  before `validateRequest`.
- `internal/llm/safety_test.go` — `TestSafety_DefaultsModelFromConfigSnapshot`.
- `scripts/smoke/phase-83h.sh` — static surface assertions.
- `docs/plans/phase-83h-dev-binary-fixes.md` — this plan.
- `docs/plans/README.md` — Phase 83h row + flip to Shipped.
- `docs/decisions.md` — D-151 (the two fixes + the audit lesson).

## Public API surface

None. Both fixes are inside existing implementation packages and
preserve the established function signatures.

## Test plan

- **Unit:** `TestShouldTrigger_SkipsDBSidecars`, `TestSafety_DefaultsModelFromConfigSnapshot`.
- **Integration:** N/A — the V1 / V2 fixes are at the unit boundary;
  end-to-end coverage lands when the post-merge operator validation
  reaches the first successful planner step (driven by the §17.5
  audit that lands after this phase).
- **Failure-mode:** the existing safety-wrapper tests still pass
  (the default-fill only fires when Model is empty; an
  unsupported-Model error path is unchanged).
- **Concurrency / leak:** N/A — pure-function fixes, no new
  reusable artifact.

## Smoke script additions

`scripts/smoke/phase-83h.sh` (static-only) asserts:

- `cmd/harbor/cmd_dev_hot_reload.go` references `dbSidecarSuffixes`
  and `isDBSidecar`.
- `internal/llm/safety.go` references the `req.Model = c.cfg.Model`
  default-fill block.

## Coverage target

- `cmd/harbor`: 80% (no change).
- `internal/llm`: 90% (no change).

## Dependencies

- Phase 83g (the MCP southbound consumer wiring that surfaced V1/V2
  during the operator validation work).
- Phase 64 (`harbor dev` hot-reload supervisor).
- Phase 32 / 33 (LLM client + safety wrapper).

## Risks / open questions

- **Operator-supplied ignore patterns deferred.** A future phase
  adds a `dev.hot_reload.ignore_globs` YAML key for operators with
  exotic db backends. Out of 83h's scope.
- **The default-Model fill is silent on the safety wrapper's logs.**
  Operators reading the LLM call logs see `model=<filled>` without
  any "(defaulted)" annotation. Acceptable for V1.1; an
  observability follow-up could add a one-time-per-session emit.

## Glossary additions

None.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes (or documented `HARBOR_PREFLIGHT_SKIP`)
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] N/A — no multi-isolation path changed
- [ ] N/A — no new reusable artifact (D-025 N/A)
- [ ] N/A — pure unit-level fixes; the post-merge §17.5 audit
      covers the end-to-end consumer
- [ ] No new vocabulary
- [ ] If a brief finding was departed from: justified above +
      decisions.md entry filed
