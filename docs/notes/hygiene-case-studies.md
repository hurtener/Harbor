# Hygiene case studies — the incidents behind the §17 rules

The binding rules live in `AGENTS.md` / `CLAUDE.md` §17 ("End-to-end + cross-subsystem integration testing") and §17.8 (spec-derived fixtures). This note preserves the concrete incidents that motivated them, extracted from the rule file so the rules stay short while the evidence stays findable. Each entry names the rule it motivates.

## Cross-package wiring gap — the `BusEmitter` ↔ `EventBus` seam (PR #11)

Motivates: §17 intro (wiring gaps), §17.5 (wave-end checkpoint audit — PR #11, the Wave 2 audit, is the reference audit).

Phase 04 and Phase 05 each shipped their side of the `BusEmitter` ↔ `EventBus` seam; each plan assumed the OTHER phase would close it. Both packages' unit tests passed in isolation while the seam between them was dead. The Wave 2 checkpoint audit (PR #11) caught it; the fix pattern — an integration test that proves the consumption works, landed in the same PR as the consuming phase — became §17.1.

## Validator regression — stale `wave2Config()` helper (PR #15)

Motivates: §17.6 (fix what the test finds, wherever the bug lives).

Wave 2's shared test config helper `wave2Config()` stopped validating once Phase 08's `validateSessions` required non-zero fields — a previous phase's test fixture silently went stale after a later phase tightened a rule. Fixed in PR #15, in the PR that surfaced it, not as a "Phase N follow-up."

## Test-time non-idempotency — the `TestOpen_HonoursCfgDriver` flake (PR #16)

Motivates: §17.6.

The test registered a process-wide driver name without cleanup. The bug lived since Phase 05 and only surfaced under `go test -count=N` when Wave 3's stress run flushed it out. Fixed in PR #16, bundled with the wave work (`feat(...) wave-3 + fix Phase 05 driver-registration flake` is the reference PR-title shape for a bundled cross-phase fix).

## Fixture-only fix masking a production gap — the devstack bus-wiring omission (PR #121, Wave 11.5 §17.5 closeout audit, finding F1)

Motivates: §17.6 ("when the test fixture's bug shape mirrors a latent production bug, fix BOTH").

PR #121 patched a bus-wiring omission in `harbortest/devstack.Assemble` but missed the same omission at `cmd/harbor/cmd_dev.go::bootDevStack`. The wave-end E2E "passed" only because devstack carried the fix; production silently emitted no `pause.resumed` events on the bus. The Wave 11.5 closeout audit pinned it as F1. The lesson: whenever you fix a bug shape on the test side, grep production for the same call site and fix it too — a fixture-only fix turns the test green while perpetuating the test↔production divergence.

## Hand-authored fixture encoding the wrong interpretation — the `_meta.ui` placement bug (D-216)

Motivates: §17.8 (external-protocol conformance fixtures derive from the real spec).

The MCP Apps `_meta.ui` discovery shipped four phases of green tests that all put `_meta.ui` on the tool RESULT — matching the code, but not the spec. The canonical schema (and every real ext-apps server) puts it on the tool DEFINITION, so discovery never fired against a real server. The hand-authored fixture was self-consistent with the implementation's misreading, so the tests could not distinguish right-field from wrong-field. D-216 corrected both the code and the fixture; the rule became: derive fixtures from the vendored/official schema, the official package's types, or a captured transcript from a real server — and where a real server can be driven in dev, an env-gated live probe (`HARBOR_LIVE_*`) is the gate.
