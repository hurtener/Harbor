# Phase 116 — non-admin session-scoped token contract

## Summary

Introduce lesser-privileged tokens — session-scoped and owner-scoped —
so the runtime issues and accepts callers below `admin`. This is the
consumer that makes Phase 114's verified-identity steering derivation
load-bearing: today every token is admin, so the escalation Phase 114
closed has no lesser principal to exploit. This phase mints non-admin
tokens, threads their verified principal through the edge, and makes the
`session_user` steering tier safe to grant (a verified session claim,
not a bare session-id match).

## RFC anchor

- RFC §5.5 — Authentication (the identity scope a token carries).
- RFC §6.3 — Steering per-event scope mapping (the `session_user` /
  `owner_user` / `admin` tiers a non-admin token must satisfy).

## Briefs informing this phase

- brief 02
- brief 05

## Brief findings incorporated

- brief 02 §511–515 (steering authz Q-3, resolved by RFC §6.3): the
  per-event tiers (`session_user` for `INJECT_CONTEXT` / `USER_MESSAGE`,
  `owner_user` for the originating-user controls, `admin` for
  `PRIORITIZE` + cross-tenant) only become meaningful once a token can
  carry a tier below admin.
- brief 05 §14: *"A session contains many runs (foreground tasks).
  Identity is the triple `(tenant_id, user_id, session_id)`."* — a
  session-scoped token's verified principal is exactly this triple; the
  steering edge derives `session_user` from a *verified* session claim,
  never an attacker-supplied session id (the gap Phase 114 left open).

## Findings I'm departing from (if any)

None.

## Goals

- Mint non-admin tokens (session-scoped, owner-scoped) carrying a
  verified principal + tier claim, distinct from the admin dev bootstrap.
- Extend `deriveSteeringScope` (Phase 114) to grant `session_user` from a
  verified session principal safely — closing Phase 114's deferred tier.
- Prove a non-admin caller can do exactly what RFC §6.3 permits and no
  more (owner can cancel/redirect their run; session_user can inject;
  neither can prioritize or steer cross-tenant).

## Non-goals

- The JWKS verifier itself (Phase 115) — this phase consumes it.
- Token exchange / passthrough (RFC 8693).

## Acceptance criteria

- [x] The runtime issues a session-scoped and an owner-scoped token with
      a verified principal; neither carries `admin`. (Dev mint via the
      bootstrap endpoint's optional `scopes:[]` override; in the
      single-participant session model the two resolve to the same
      principal — D-221.)
- [x] `deriveSteeringScope` grants `session_user` only from a verified
      session principal; a bare session-id match still confers nothing.
      (Resolved as option (a): a verified session participant IS the run
      owner, so a full-triple match earns the strictly-higher
      `owner_user`; a collision confers nothing. D-221.)
- [x] A non-admin owner: cancel/pause/resume/redirect/approve/reject on
      their own run succeed; `prioritize` and cross-tenant are rejected.
- [~] A session_user: `inject_context` / `user_message` succeed;
      owner-level controls are rejected. (The distinct `session_user`
      tier is RESERVED — no V1 Dispatch path mints it; the tier semantics
      are covered at `steering.CheckScope`. §4.3 deviation in D-221.)
- [x] Cross-session / cross-user isolation test: a non-admin token cannot
      steer another principal's run. (Surface + wire collision-safety
      tests + integration test.)

## Files added or changed

- `internal/protocol/auth/` — non-admin token claims + issuance.
- `internal/protocol/control.go` — `deriveSteeringScope` session-tier arm.
- `cmd/harbor/` — dev issuance of a non-admin token (gated dev convenience).
- `scripts/smoke/phase-116.sh`.

## Public API surface

- The token-claims shape gains a verified tier; no new Protocol method.
  `deriveSteeringScope` stays unexported.

## Test plan

- **Unit:** per-tier authorization matrix (owner/session/admin × the nine
  controls); verified-session-principal vs bare-session-id grant.
- **Integration:** `test/integration/` — a non-admin token over the wire
  steers its own run and is rejected on another's (real surface, identity
  propagation, ≥1 failure mode, `-race`).
- **Conformance:** N/A — no new method.
- **Concurrency / leak:** N concurrent non-admin callers across distinct
  sessions, asserting no cross-talk (extends the steering concurrent test).

## Smoke script additions

- `scripts/smoke/phase-116.sh`: a non-admin token can `inject_context` on
  its own run (200) and is rejected `scope_mismatch` (403) on
  `prioritize` — the live negative-escalation assertion Phase 114's smoke
  deferred. Skips on builds without non-admin issuance.

## Coverage target

- `internal/protocol`: ≥ 85%; `internal/protocol/auth`: ≥ 85%.

## Dependencies

- Phase 114 (verified-identity derivation), Phase 115 (production verifier).

## Risks / open questions

- The exact tier-claim representation (a scope string vs a structured
  claim) — settle against the existing `auth.Scope` set at authoring.
- Multi-user sessions: whether a session can have more than one principal
  changes `session_user` semantics (today a session belongs to one user).
- Full §16 brief pass (brief 02 + 05 + RFC §5.5/§6.3) when dispatched.

## Glossary additions

- `session-scoped token` / `owner-scoped token` — add to glossary.

## Pre-merge checklist

- [x] `make drift-audit` passes
- [ ] `make preflight` passes (coordinator runs the live gate; static gate + a local live-server smoke run green)
- [x] `make check-mirror` passes (no AGENTS.md/CLAUDE.md edits)
- [x] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [x] Coverage on touched packages ≥ stated target
- [x] If multi-isolation paths changed: cross-session isolation test passes (collision + concurrency stress)
- [x] Concurrent-reuse test passes (steering edge, `-race`) (bootstrap N=100 + integration N=16, `-race`)
- [x] Integration test exists, real drivers, identity propagation, ≥1 failure mode, `-race`
- [x] If new vocabulary: glossary updated (`session-scoped token`, `owner-scoped token`)
- [x] If a brief finding was departed from: justified + decisions.md entry (D-221 §4.3 deviations)
