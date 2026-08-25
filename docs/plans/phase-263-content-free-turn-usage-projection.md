# Phase 263 — Content-free user-scoped turn usage projection

## Summary

Add an optional `usage` selector to the existing `sessions.turns.get` method so
an exact-session consumer can read durable turn lifecycle/timing and canonical
usage facts without receiving conversation content. Reuse the existing durable
turn row and authority gates; add no new runtime work or persistence path.

## RFC anchor

- RFC §5.2
- RFC §5.5
- RFC §6.9
- RFC §6.13
- RFC §7
- RFC §9

## Briefs informing this phase

- brief 05
- brief 06

## Brief findings incorporated

- brief 05 §1: session identity is the `(tenant_id, user_id, session_id)`
  triple and durable projections must preserve it through every read boundary.
- brief 05 §6: cross-tenant and concurrent-session isolation are required
  acceptance, not presentation-layer assumptions.
- brief 06 §1: Protocol clients consume runtime-owned projections rather than
  importing runtime internals or creating a privileged parallel view.
- brief 06 §3: filtering and identity authorization are enforced server-side.

## Findings I'm departing from (if any)

None.

## Goals

- Add `projection: "usage"` to `sessions.turns.get` without changing omitted
  or conversation behavior.
- Return only lifecycle/timing, turn/task/session/agent identifiers, canonical
  usage measures with honest availability, and optional bounded model.
- Preserve exact-session/effective-agent authorization and non-oracular
  foreign-identity refusal; admin/fleet must not enable or widen the lane.
- Perform exactly the existing durable indexed turn lookup and no additional
  runtime work.

## Non-goals

- Conversation content, operator detail, run IDs, messages, reasoning,
  Activity, pauses, Apps, attachments, tenant/user identifiers, or raw events.
- Lists, prefix scans, polling, workers, timers, receipts/outboxes, schemas,
  configuration, provider calls, credentials, or external grants.
- Billing truth, quotas, admission policy, aggregation, or dashboards.
- Protocol-version changes, release artifacts, or downstream deployment.

## Acceptance criteria

- [x] `sessions.turns.get` accepts the explicit `usage` selector while omitted
      and conversation requests retain their existing response shape.
- [x] Exactly one of `turn`, `ops_turn`, or `usage_turn` is returned.
- [x] The usage row is structurally content-free and exposes all eight
      canonical measures with their exact/estimated/unavailable state.
- [x] The exact-session and effective-agent gates apply; admin/fleet cannot
      widen the read; foreign identities receive the existing non-oracular
      refusal.
- [x] The path invokes one existing durable projector `Get` and introduces no
      scan, poller, worker, persistence, config, provider, credential, receipt,
      or grant path.
- [x] Canonical Go, TypeScript, generated manifest/reference docs, client
      fixture, and operator skill stay aligned.
- [x] Two independent adversarial reviews report P0=0/P1=0 at the reviewed
      implementation head after narrow re-review of concrete fixes.

## Files added or changed

- `internal/sessions/turns/protocol/` — selector, service projection, authority
  and adversarial tests.
- `internal/protocol/types/` and `internal/protocol/transports/stream/` — wire
  DTO, mutually exclusive envelope, mapping, and transport tests.
- `internal/protocol/singlesource/`, `cmd/harbor-*` — canonical type indexes.
- `web/console/src/lib/protocol/` — TypeScript mirror and generated manifest.
- `docs/site/protocol/` and `docs/skills/use-the-harbor-protocol/` — generated
  reference and operator guidance.
- `CHANGELOG.md`, `RFC-001-Harbor.md`, `docs/decisions.md`, and this tracker.

## Public API surface

- Existing `sessions.turns.get` request gains optional
  `projection: "usage"`.
- Existing response gains mutually exclusive `usage_turn` containing
  `SessionUsageTurnRow` and canonical `SessionTurnUsage` measures.
- Protocol version remains `0.1.0`.

## Test plan

- **Unit:** content-free JSON key exclusion, all measure states, selector and
  response-union validation.
- **Integration:** real turn projector through the Protocol stream handler,
  exact-session/effective-agent allow and foreign/admin refusal.
- **Conformance:** Go/TypeScript/generated manifest/docs lockstep and pinned
  client fixture.
- **Concurrency / leak:** existing immutable projector reuse contract remains;
  no goroutine, timer, poller, or background component is added.

## Smoke script additions

- Static Phase 263 smoke pins the plan/decision/index, selector, structurally
  distinct DTO, content-free contract test, authority adversary, single-Get
  implementation, generated reference, and operator skill.

## Coverage target

- Preserve the touched session-turn Protocol/service/transport package floors;
  every new branch has focused tests. No unrelated package coverage is claimed.

## Dependencies

- Phase 130
- Phase 161
- Phase 246
- D-293
- D-425

## Risks / open questions

- A future consumer could mislabel estimated/provider-reported usage as invoice
  truth; this phase deliberately preserves per-measure availability and makes
  no billing claim.
- A future list or poll loop could defeat the one-action/one-indexed-read
  boundary; neither is part of this contract.

## Glossary additions

None — the existing conversation-turn projection, content-free metadata, and
canonical usage vocabulary are reused.

## Pre-merge checklist

- [ ] Full local `make drift-audit` was intentionally terminated at the
      owner's direction because hosted web CI is authoritative and local
      Harbor resource-heavy gates are forbidden. The release-cut-only static
      Phase 263 smoke, mirror, and diff gates pass.
- [ ] `make preflight` was still in progress in hosted PR run `32911031711` at
      the owner-authorized fast merge; this release cut does not claim it.
- [x] `make check-mirror` passes in hosted CI
- [x] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [x] New branches have focused tests on the hosted implementation head
- [x] Cross-session/cross-identity isolation adversaries pass in hosted CI
- [x] Concurrent-reuse N/A — no reusable mutable artifact or background work
      was added; the existing immutable projector is reused for one `Get`.
- [x] Real-driver integration exists through the existing durable turn
      projector and Protocol stream handler, including identity and failure.
- [x] New vocabulary N/A
- [x] Brief departures N/A

## v1.30.4 release-candidate evidence

PR #752 reviewed exact head `87a58c400fa9a553937a2fe02a020cd6b91ddb7b`
and squash-merged at exact current `origin/main`
`4c955b6ad97e35810fda11cec8807fcc05f15a1d`. At the owner-authorized fast
merge, hosted PR run `32911031711` had both Go platforms, lint, frontend,
examples, performance, PostgreSQL/S3 conformance, isolation, leak, chaos,
mirror, and markdown checks green; documentation run `32911031604` was also
green. Console Playwright and the full preflight gate were still in progress
with no failed job. No completed final preflight/Playwright result, tag,
release, assets, module provenance, checksums, attestations, post-tag cleanup,
downstream deployment, or acceptance is claimed here.
