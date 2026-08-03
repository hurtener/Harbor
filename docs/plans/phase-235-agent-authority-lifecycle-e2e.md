# Phase 235 — Agent authority and lifecycle wave E2E

## Summary

Compose signed reach, triad-wide conditional save, durable session overlays/personal records, session erasure, and agent-config retirement through real runtime/Protocol paths before the v1.26.0 release. Run adversarial two-runtime races, N≥10 mixed-identity stress, and the mandatory read-only wave checkpoint audit, then land every release-blocking finding.

## RFC anchor

- RFC §5.5.
- RFC §6.11.
- RFC §6.16.
- RFC §9.

## Briefs informing this phase

- brief 05
- brief 06
- brief 07
- brief 11

## Brief findings incorporated

- brief 05 §10: real-driver restart and concurrency behavior are part of the persistence contract.
- brief 06 §6: observable event ordering and negative paths are tested end to end.
- brief 07 §11: cancellation, concurrency, and leak behavior are runtime acceptance criteria.
- brief 11 §4: clients observe only canonical Protocol outcomes and never internal lifecycle objects.

## Findings I'm departing from (if any)

- None.

## Goals

- Prove the wave's authority and lifecycle mechanisms compose on real transports and stores.
- Close every FAIL/WARN from a read-only checkpoint audit before release.
- Produce complete v1.26.0 release evidence and operator migration notes.

## Checkpoint status

The checkpoint and v1.26.0 publication are complete.
The named suite executes real SQLite signed-capability registration, publisher
takeover, stale-publisher denial, empty-runtime removal, restart/reconcile,
four-slot session-personal authority, erasure, inflight-run retirement drain,
retirement cleanup restart, and byte-exact reasoning durability. Its isolated-
schema Postgres leg races two independent runtimes across retirement versus
agent/user config, rollback, second retirement, signed registration/removal,
and erasure versus overlay/personal writes. The read-only audit and hosted
PR-to-main preflight passed before the annotated `v1.26.0` tag. The tag object
is `d4208deaaf89f0f836dd4ca20ad583cfc40ce5ed`, pointing at release commit
`173dd53beb72eae1bc9a1c9bf402446ea9726c16`. Release workflow `30771486167`
completed successfully: six CGo-free matrix builds produced six binaries and
six SHA-256 sidecars; the publish job verified `checksums.txt` against all six;
all six build provenance attestations verified; and the GitHub Release contains
13 assets (six binaries, six sidecars, and `checksums.txt`). Native
linux/arm64 `harbor version --json` reports exactly
`{"harbor":"v1.26.0","protocol":"0.1.0","build_hash":"173dd53beb72eae1bc9a1c9bf402446ea9726c16"}`.
This separate post-tag `chore(release)` change updates the scaffold pin and its
goldens; cloud validation for that follow-up remains an explicit PR gate.

## Non-goals

- No new feature surface beyond corrections required by composing tests or audit findings.

## Acceptance criteria

- [x] `test/integration/wave_v126_test.go` exercises reach-authorized start/config/tools paths, D-401 signed OAuth capability registration/restart/reconcile/removal, durable session overlay/personal resolution, erasure, and retirement over real SQLite, with identity propagation and at least one denial per seam.
- [x] Two independent runtimes over shared Postgres race retirement and session erasure against agent write, signed-capability registration/removal, user write, rollback, overlay write, personal-record write/delete, and second retirement; exactly one valid transition wins and restart preserves the result.
- [x] HA-50 remains a v1.26 release gate. A narrow boot-pinned compatibility
  flow is not the generic capability contract and cannot substitute for the
  atomic production Protocol registration of one provider plus one connection,
  its closed registration-only descriptor, durable paired-removal recovery and
  first-write pending-activation fence, fail-loud scope ceiling, zero wire
  `token_url`/credential/host-list fields, or its race and restart coverage.
- [x] HA-51 remains a v1.26 release gate. The decoded Bifrost JSON/SSE
  reasoning fixture is byte-identical from raw callback through completed
  response, planner decision, live `tasks.get`, durable restart history, and
  Console rendering; details-only multi-block behavior and N>=100 shared-driver
  race/cancel/no-bleed/leak coverage remain green.
- [x] The existing `state-postgres` Postgres 16 CI job runs the named
  `TestE2E_WaveV126...` integration suite under `-race` with
  `HARBOR_PG_DSN` set. A missing local DSN may skip only outside that CI step;
  the real Postgres leg is never skipped in CI.
- [x] The immutable per-run resolver gives Directory and
  `skill_get`/`skill_list`/`skill_search` the same current personal,
  legacy-fallback, `ScopeUser`, lexical, and semantic view; session-skills
  list returns only the session tier. `ActiveSkillViews` is no second
  membership authority, logical tombstones suppress fallback, and no new
  session body reaches the shared SkillStore.
- [x] Missing/invalid/false or unlisted `skills.session_personal_cutover.tenants`
  declarations keep that tenant in `dual_read`: legacy session rows remain
  visible and session-personal mutations return
  `session_skill_cutover_pending` (HTTP 409). A valid declared tenant persists
  through restart, then a fault-injected `ScanKindForTenant` migration and a
  final fresh verification pass prove every currently eligible schema-1
  reference copied or terminal before `state_only`; the phase does not invent
  runtime membership, unknown-tenant discovery, or a restart-surviving DB
  snapshot.
- [x] Release evidence explicitly asks v1.26 reviewers to accept the default
  `dual_read` session-personal mutation refusal as a compatibility/deployment
  change. `docs/CONFIG.md`, `CHANGELOG.md`, `configure-memory-and-skills`, its
  docs-site stub, and example config describe the drain attestation, resumable
  scan, and final `state_only` condition.
- [x] A retirement or erasure fence change during overlay/personal/composite
  read enumeration yields only a retry or typed failure, never a returned
  post-fence record; personal upsert/delete proves one `SaveIf` target mutation
  and no companion overlay-membership mutation. Deterministic perpetual churn
  exhausts after `MaxSessionSkillReadAttempts = 3`, honors cancellation/deadline,
  and externally maps `ErrSessionSkillReadUnstable` to HTTP 409
  `session_skill_read_unstable`.
- [x] Commit-then-error injections across every Phase 233a `SaveIf` writer
  separately prove overlay/personal, cutover, retirement, and cleanup-item
  reread convergence with their own expectations and no unconditional
  compensation. Raw-agent schema-1 overlays remain readable/migratable; the
  `a`/`ab` adjacency test proves migration and retirement exact-kind equality,
  and retirement uses no unconditional delete.
- [x] N≥10 mixed tenants/users/sessions/reach sets against shared runtime artifacts show no authority, config, or cancellation bleed; goroutine baseline restores.
- [x] Faults after tombstone, cleanup side effect, and progress persistence resume only for the same operation and never alter immutable history or boot/global resources.
- [x] Canonical error registry/matrix, HTTP mapping, Protocol docs, Console
  types/manifest, operator skills, config examples, glossary, plans, and
  CHANGELOG are current.
- [x] A separate read-only reviewer audits phases 232–235 against RFC/plans/code/tests; the coordinator fixes and re-verifies every release-blocking finding.
- [x] Before tagging `v1.26.0` from `main`, the cloud PR-to-main preflight is
  green and authoritative. Maintainers run focused local race, smoke, lint,
  drift, mirror, release-dryrun, and Linux release-build gates; they do not
  claim or run the full local preflight for this release process.
- [x] Release evidence records six CGo-free binaries
  (`linux`/`darwin`/`windows` × `amd64`/`arm64`), every per-binary SHA-256
  sidecar, successful `checksums.txt` verification, build provenance
  attestations, every GitHub Release asset, and native `harbor version --json`
  reporting `v1.26.0`.
- [x] After the tag publishes, a separate `chore(release)` commit bumps `cmd/harbor/scaffold.FallbackModuleVersion` to `v1.26.0` and regenerates scaffold goldens; targeted validation passes and the PR-to-main cloud gate remains the merge gate.

## Files added or changed

- `test/integration/wave_v126_test.go`
- `scripts/smoke/phase-235.sh`
- Corrections identified by composing tests and checkpoint review
- `CHANGELOG.md`, `docs/plans/README.md`, phase plans, generated docs/manifests, and affected operator skills

## Public API surface

- None beyond phases 232–234 and 233b including Phase 233a's pending-cutover error;
  this phase verifies the shipped surface.

## Test plan

- **Unit:** targeted regressions for every checkpoint finding.
- **Integration:** real mux/runtime/SQLite/Postgres wave composition with restart, erasure-ledger replay, resolver parity, and fault injection.
- **Conformance:** rerun StateStore and agentcfg conformance suites plus closed reach/mutation method censuses.
- **Concurrency / leak:** N≥10 wave stress, N≥100 primitive reuse inherited from phases 232–234 and 233b, `-race`, cancellation isolation, goroutine baseline.

## Smoke script additions

- Replace the skeleton with a `go test -list` no-match-fails guard for the
  named `TestE2E_WaveV126...` suite, then run it and assert its reach,
  four-slot CAS, resolver, erasure, retirement, restart, cleanup, and
  isolation subtests execute. A zero-match `go test -run` exit is not evidence.

## Coverage target

- No touched package falls below its Phase 232–234 target or v1.25 floor.

## Dependencies

- 232, 233, 233a, 233b, 233c, 234.

## Risks / open questions

- The existing Postgres 16 CI service must receive the named wave integration
  invocation with `HARBOR_PG_DSN`; without it, the environment-gated Postgres
  subtest would skip and the release criterion would be unproven. StateStore
  has no atomic collection-count primitive, so the wave must verify
  per-record/request bounds rather than assert a hard personal-record
  cardinality cap.
- Any audit finding that changes authority semantics returns to the RFC before code changes.

## Glossary additions

- None.

## Pre-merge checklist

- [x] `make drift-audit` passes
- [x] Focused local race, smoke, lint, drift, mirror, release-dryrun, and Linux
  release-build gates pass; local full preflight is intentionally not run
- [x] Cloud PR-to-main `make preflight` passes before merge/tag
- [x] `make check-mirror` passes
- [x] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [x] Coverage on touched packages ≥ stated target
- [x] Cross-session and cross-tenant wave stress passes
- [x] N≥10 wave stress and inherited N≥100 artifact tests pass under `-race` with no leak
- [x] Real-driver integration covers identity and at least one failure per seam
- [x] `state-postgres` CI runs the named v1.26 Postgres E2E with
  `HARBOR_PG_DSN`; no Postgres skip is accepted there
- [x] Release evidence verifies the six artifacts, SHA-256 sidecars,
  `checksums.txt`, provenance, GitHub Release assets, and native version JSON
- [x] If new vocabulary: glossary updated
- [x] If a brief finding was departed from: justified above + decisions.md entry filed
