# Phase 131b — configure-production-identity skill

## Summary

Ships `docs/skills/configure-production-identity/SKILL.md`, the Claude-Code-style
operator playbook that operationalizes the 131a production identity setup guide:
a fast path for getting a verifiable JWT into a client and attaching it to a
production `harbor serve`. It carries the `(tenant, user, session)` + `scopes`
claim shape, the `iss`/`aud` exact-match contract, the OIDC app-registration
flow, and both attach on-ramps — a real IdP and the no-IdP `harbor token`
self-issuing path. Same-PR site mirror: the include stub + the VitePress nav
entry, so `phase-103.sh`'s skill→page mapping passes.

## RFC anchor

- RFC §5.5 — Authentication (the Protocol's JWT verification surface; the
  identity triple lives in the claims; the Protocol rejects any request without
  an identity scope — the surface this skill teaches operators to satisfy).
- RFC §4.2 — Mandatory identity (the `(tenant, user, session)` triple is
  mandatory and fails closed; the skill's claim-shape table reflects that).
- RFC §8 — CLI layer (`harbor serve` is the production verifier; the no-IdP
  `harbor token` on-ramp this skill documents is a CLI subcommand).

## Briefs informing this phase

- brief 06
- brief 09

## Brief findings incorporated

- brief 06 (Events, Observability, and Developer Experience): the adopter's
  first-attach experience is a make-or-break DevX surface — a developer who
  "follows a stale step, hits a wall, and abandons" is a silent funnel leak. The
  skill is the operator-facing playbook that turns the production-identity cliff
  into a copy-pasteable path, and §18's same-PR drift rule keeps it in lockstep
  with the `harbor serve` / `harbor token` surfaces it documents so the
  five-minute-then-five-months adoption guarantee holds.
- brief 09 (MCP OAuth, lessons from bifrost): OAuth/OIDC token handling is full
  of foot-guns where a self-consistent-but-wrong configuration passes locally
  and fails against a real verifier. The skill bakes in the "decode a real token
  and confirm `iss`/`aud`/claims match `serve.yaml` before wiring a client" habit
  (the mint-and-test step) and makes the `iss`/`aud` exact-match contract — the
  most common production 401 — unmissable.

## Findings I'm departing from (if any)

None.

## Goals

- A real-framework-quality, honest operator skill an adopter can follow
  end-to-end to obtain a verifiable JWT and attach a client to `harbor serve`,
  without reading Go source.
- Carry the authoritative claim shape `serve` verifies (the mandatory
  `(tenant, user, session)` triple + optional `scopes`) and the `iss`/`aud`
  exact-match contract.
- Document **both** attach on-ramps — a real IdP (Auth0 / Okta / Keycloak /
  Cognito) and the no-IdP `harbor token` self-issuing path — with the honest
  single-issuer-grade callout on the latter.
- Mirror the skill into the published docs site (the include stub + the nav
  entry) in the same change, so the `phase-103.sh` skill→page mapping gate and
  the VitePress dead-link gate both pass.

## Non-goals

- No code change. This is a docs/skill phase — `harbor serve`'s verifier is
  untouched (D-220 preserved) and the `harbor token` subcommand itself ships in
  131d; this skill forward-points to it.
- No new Protocol method, error code, wire type, or config field.
- Not a duplicate of the 131a guide — the skill is the fast path through it and
  links to it as the authority for the full per-provider detail.

## Acceptance criteria

- [ ] `docs/skills/configure-production-identity/SKILL.md` exists with
  well-formed frontmatter (`name` matching the slug, `license: Apache-2.0`,
  `metadata.framework: harbor`, `metadata.surface: protocol`, `metadata.verbs`).
- [ ] The skill documents: the `serve` verification contract, the
  `(tenant, user, session)` + `scopes` claim shape, the `iss`/`aud` exact-match
  contract, OIDC app registration, both attach on-ramps (real IdP + the no-IdP
  `harbor token` self-issuing path with its honesty/grade callout), and the
  mint-and-test loop.
- [ ] The skill is listed in `docs/skills/INDEX.md`.
- [ ] `docs/site/skills/configure-production-identity/SKILL.md` include stub
  exists and `docs/site/.vitepress/config.ts` carries the nav entry, so
  `phase-103.sh`'s skill→page mapping passes and the dead-link gate covers it.
- [ ] `scripts/smoke/phase-131b.sh` (static-only) asserts the skill present +
  frontmatter + both on-ramps + claim shape + the exact-match contract + the
  site stub + nav mapping + INDEX entry.
- [ ] `make drift-audit` passes (skill frontmatter audit, RFC `§N.M` references
  resolve, no forbidden names); the VitePress build passes with no dead
  site-internal links.

## Files added or changed

- `docs/skills/configure-production-identity/SKILL.md` — new (the skill).
- `docs/skills/INDEX.md` — index entry under "Ship".
- `docs/site/skills/configure-production-identity/SKILL.md` — new (the include
  stub mirroring the canonical SKILL.md).
- `docs/site/.vitepress/config.ts` — nav entry under skills → Ship.
- `scripts/smoke/phase-131b.sh` — new (static-only): the per-plan smoke the
  drift-audit requires; pins the skill's presence + frontmatter + on-ramps +
  claim shape + site mirror + INDEX entry.
- `docs/plans/README.md` — index row + detail block (Status: Pending).

## Public API surface

N/A — documentation / operator-skill phase. No Go API, no Protocol surface, no
config schema change.

## Test plan

- **Unit:** N/A — no code.
- **Integration:** N/A — no cross-subsystem seam. The IdP→serve round-trip is
  exercised by 131c's binding smoke and the `harbor token`→serve leg by 131d's
  smoke; this skill is the docs consumer those phases reference.
- **Conformance:** N/A.
- **Concurrency / leak:** N/A — no reusable artifact.
- **Docs gate:** `scripts/smoke/phase-131b.sh` (skill present + frontmatter +
  on-ramps + claim shape + site stub + nav mapping + INDEX) and the VitePress
  build (no dead site-internal links) are the binding checks; the skill
  frontmatter audit runs under `make drift-audit`.

## Smoke script additions

`scripts/smoke/phase-131b.sh` (new, static-only — the per-plan smoke the
drift-audit requires) asserts: the SKILL.md exists with the protocol-surface
frontmatter the §18 audit expects; it documents both on-ramps (the IdP path and
the `harbor token` self-issuing path), the `(tenant, user, session)` claim shape,
and the `iss`/`aud` exact-match contract; it forward-points to the
production-identity guide and the `use-the-harbor-protocol` wire skill; the
docs-site include stub exists, the skill is nav-mapped in
`docs/site/.vitepress/config.ts`, and it is listed in `docs/skills/INDEX.md`.
The VitePress dead-link gate (run by `.github/workflows/docs.yml` on every PR)
is the binding check that the mirrored page is reachable and every cross-link
resolves.

## Coverage target

N/A — documentation / operator-skill phase (no Go package touched).

## Dependencies

- 131a (the production identity setup guide this skill operationalizes and links
  to as the authority). No code dependency — 131c (worked OIDC client) and 131d
  (`harbor token` subcommand) are forward-referenced; they do not block this.

## Risks / open questions

- The skill forward-references the `harbor token` subcommand (131d) before it
  merges. Mitigation: the reference is descriptive and flagged "forthcoming"; the
  §18 same-PR skill-drift rule and 131d's own PR keep this skill honest if the
  flag surface shifts.
- A surface change (a `harbor serve` identity field, the claim shape, the
  `harbor token` flags) could drift this skill out of sync. Mitigation: §18
  mandates the matching skill update in the same PR as any
  `surface: protocol` / `cli` change; this skill's `metadata.surface: protocol`
  makes it grep-discoverable for that rule.

## Glossary additions

None — the skill reuses the existing **on-ramp** and **production-identity
guide** glossary terms (added with 131a); no new vocabulary.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target — N/A (docs-only; no Go
  package touched).
- [ ] If multi-isolation paths changed: cross-session isolation test passes —
  N/A (no code).
- [ ] If this phase builds a reusable artifact: concurrent-reuse test passes —
  N/A (docs-only; no artifact built).
- [ ] If this phase consumes a shipped subsystem's surface OR closes a
  cross-subsystem seam: an integration test exists — N/A (docs-only; the
  IdP→serve and `harbor token`→serve round-trips are gated by 131c/131d smokes).
- [ ] If new vocabulary: glossary updated — N/A (reuses existing terms).
- [ ] If a brief finding was departed from: justified above + decisions.md
  entry filed — N/A (no departure).
