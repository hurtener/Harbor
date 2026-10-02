# Phase 275 — Scoped session admission

## Summary

Add an exact signed method restriction and durable session mutation acceptance
fence. Existing Runtime task, steering, approval and OAuth execution paths remain
the sole executors. Qualification is in progress; this is not a release claim.

## RFC anchor

- RFC §5.5
- RFC §6.11
- RFC §6.3

## Briefs informing this phase

- brief 02
- brief 05
- brief 09

## Brief findings incorporated

- brief 02 §3: the Runtime validates steering and deposits it on its existing
  per-run inbox; planners observe projected control, not another inbox
- brief 05 §5: all persistence drivers implement one mandatory surface; reuse
  StateStore conditional writes instead of an optional driver capability
- brief 05 §6: real backend conformance, restart, idempotency and cross-tenant
  isolation must be exercised on the storage seam
- brief 09: OAuth completion composes with the unified pause/resume primitive;
  the existing exact flow-state and pause binding remains authoritative

## Findings I'm departing from (if any)

None. The acceptance fence does not extend the single-active-runtime task
registry into a distributed task executor.

## Goals

- Signed canonical method restriction with absence distinct from explicit empty
- Strict malformed, duplicate, unknown, alias and partial-authority rejection
- Immutable full owner/issuer/coordinator enrollment with monotonic epochs
- Serialize enrollment with mutation acceptance, including legacy acceptance
  before the first enrollment record exists
- Preserve explicitly authorized reads, uploads, native decisions and OAuth
- Refuse stale issued bearers, unsupported runtimes and uncertain acceptance

## Non-goals

- Production audience, issuer, trust-key, registration or grant changes
- A second executor, unbounded permit ledger, lease expiry or automatic reruns
- Revoking separately held tenant/admin or durable user-wide configuration power
- Rolling back controls, tasks or external effects already accepted
- Supporting simultaneous active task ownership by multiple Runtime processes

## Acceptance criteria

- [ ] Claim absence preserves legacy behavior; explicit empty denies every method
- [ ] One restricted credential is rejected by the legacy audience verifier
- [ ] Same issued broad credential mutates before enrollment and is refused after
- [ ] Exact full owner, immutable issuer/coordinator, epoch and method are required
- [ ] Enrollment and mutation races are linearizable on inmem, SQLite and Postgres
- [ ] Independent store actors observe the same enrollment and pending acceptance
- [ ] Restart and unknown commit cannot turn unresolved admission into permission
- [ ] Existing same-key Start retries preserve task idempotency
- [ ] Direct service calls, HTTP aliases, controls, overrides, session mutations,
  App callbacks, artifact mutations and memory/flow mutations are fenced
- [ ] History/SSE/artifact reads and explicit native approve/reject/resume/OAuth
  paths remain available under their existing authority and new restrictions
- [ ] Token rotation cannot strip method, epoch or scoped-audience restrictions
- [ ] Canonical SDK, generated docs and Console type lockstep pass
- [ ] Same-version boot refuses disabled admission over recorded admission state

## Files added or changed

- `internal/protocol/auth/`: signed claims, exact audience and route restriction
- `internal/runtime/sessionadmission/`: durable bounded CAS acceptance fence
- `internal/protocol/`, `internal/sessions/protocol/`, runtime services: exact
  resolved-target acceptance calls and canonical enrollment method
- `internal/tools/auth/callback.go`: existing native OAuth acceptance fence
- `internal/runtime/serve/`: explicit assembly and downgrade refusal
- `internal/config/`, `examples/harbor.yaml`: code-only opt-in contract
- `sdk/protocolclient/`, canonical method/types, generators and Console types
- RFC, decision log, glossary, operator skill, published compatibility guidance
- `scripts/smoke/phase-275.sh`: real focused contract gate

## Public API surface

- `method_reach`: optional signed unique array of exact canonical method names
- `session_admission_epoch` and `session_admission_coordinator`: paired signed
  authority, valid only with method reach and an exact original JWT identity
- `sessions.set_admission`: admin request `{identity,expected_epoch,epoch}`;
  immutable authority derives from verified token issuer and subject
- `scoped_session_admission_v1`: conditional implemented contract, not fleet proof
- `identity.scoped_token_audience` and
  `identity.session_admission_legacy_writers_drained`: boot-only opt-in
- `Client.SessionsSetAdmission`: canonical typed SDK operation

## Test plan

- **Unit:** strict JWT claims and exact audiences; method universe/alias mapping;
  rotation narrowing refusal; immutable authority, epochs and downgrade refusal
- **Integration:** real JWT HTTP and direct Protocol consumers with real
  TaskRegistry/steering/state; native OAuth provider and real callback; real
  inmem/SQLite/Postgres acceptance seams
- **Conformance:** one admission suite across the mandatory persistence triad,
  including independent SQLite/Postgres handles and full tuple collisions
- **Concurrency / leak:** N=128 shared-gate isolation; absent-policy admission
  races, concurrent exact Start replay, cancellation and unresolved reservation

## Smoke script additions

- Real auth, session admission and Protocol mutation regressions under `-race`
- Typed client enrollment route and native OAuth callback regression
- PostgreSQL checks require the dedicated `HARBOR_PG_DSN` test database; a skipped
  backend is reported as unverified and cannot qualify release

## Coverage target

New `internal/runtime/sessionadmission`: 85%; changed auth authority branches:
90%. Existing broad service packages retain their repository baselines. Final
coverage numbers and aggregate gates remain pending execution.

## Dependencies

- 271 — durable exact-task input receipts
- 16 — StateStore persistence triad
- 30 — tool OAuth
- 50 — unified pause/resume

## Risks / open questions

A new claim cannot constrain an old server that ignores it. Before issuing
restricted credentials, every reachable old verifier must enforce the distinct
legacy audience; no audience-less verifier may remain reachable. All old writers
must be drained and old binaries prevented from reopening enrolled state. The
boot acknowledgment is an operator assertion, not fleet attestation.

One bounded pending slot covers synchronous mutation acceptance, not task
lifetime. A successful enrollment does not retract previously accepted queued
controls or running tasks. Reconcile/drain their domain receipts and executions
before relying on coordinated continuation. A lost acquire commit may retain a
pending slot without executing the domain operation. Crash-uncertain slots have
no expiry or general clear endpoint: positive domain evidence can be inspected
through existing task/input reads, but automated safe recovery is not claimed.
This conservative attention state may block a session until explicit recovery
can prove the old accepting actor has stopped and the exact operation settled.

A failed release may have committed despite its missing reply. Return uncertainty;
never infer non-acceptance or issue a new idempotency key. The existing task
registry remains single-active-runtime even though the admission CAS supports
independent durable store actors. In-memory admission lasts only for its store.
Independent review of the final integrated candidate and deployed current docs,
plus complete repository checks, remain required before a stable release.

## Glossary additions

- Signed method reach
- Session admission epoch
- Unresolved session acceptance

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: cross-session isolation test passes
- [ ] Concurrent-reuse N≥100 under `-race` passes
- [ ] Real-driver cross-subsystem integration with identity and failure passes
- [x] New vocabulary added to glossary
- [x] No brief departure; D-491 records the architectural boundary

Outstanding independently signed artifact imports remain governed by their exact
transfer grants and recipient admissions. Migration must reconcile/quiesce those
admissions too; session enrollment is not a retroactive revocation of transfer
permission.

## Local qualification checkpoint

The focused race batch passed on 2026-10-02 using the pinned Go 1.27.1 toolchain
and the real inmem, SQLite and local PostgreSQL drivers. It exercised signed
claims/audience, old-token cutover, direct/HTTP controls, keyed replay, unknown
outcomes, native OAuth completion, exact SDK route, config and early restart
refusal. The package log is an execution artifact outside the repository; no
private environment paths or credentials are part of the public contract.

Four additional typed-service packages compiled in that filtered batch without
selected tests. Broader existing suites, final post-review reruns, measured
coverage, generated joins and full preflight remain open. Markdownlint passed
with an explicit writable npm cache. The first drift audit reported 1607 checks
passed and only its npm-cache startup failure; the complete audit must be rerun
with that cache configuration before marking the phase complete.

### Native App acceptance handoff qualified locally

The initial synchronous wrapper could block the native control needed to finish
an App invocation. The candidate now attaches a runtime-owned invocation
lifecycle to the existing composed descriptor. A real approval pause parks the
acceptance slot before waiting; approval reentry acquires the exact original
method/owner/epoch against current durable enrollment before invoking the
original descriptor. Canonical OAuth credential preflight can park only after
establishing its native pause. Arbitrary post-invocation tool errors never cause
that handoff. MCP transport dispatch marks that effects may have started, so a
later challenge cannot discard prior unknown liability.

The original catalog wrapper chain, native coordinator, OAuth flow and task
executor remain intact. There is no second executor, raw bearer persistence or
automatic new action. Duplicate native decisions cannot reacquire an active
invocation twice. Uncertain park/acquire commits stop continuation; an unknown
accepted write remains blocked. Real AppSurface/accessor/catalog approval/OAuth composition, repeated native
delivery, declined/cancelled waits, stale-epoch reentry, unknown-effect retention,
and triad handoff tests passed under `-race` on 2026-10-02. The later
configuration/assembly gate also proved volatile production state is refused.
In-memory remains the reference/test seam; served opt-in requires SQLite or
PostgreSQL. These focused passes do not replace full existing suites, measured
coverage, generated joins, preflight or independent final integrated review.
