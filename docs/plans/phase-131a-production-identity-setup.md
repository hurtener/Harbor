# Phase 131a — Production identity setup guide

## Summary

Ships `docs/site/protocol/production-identity-setup.md`, the missing operator
manual for getting a verifiable JWT into a client and attaching it to
`harbor serve`. It documents the claim shape `serve`'s verifier enforces
(lifted from the authoritative parser, not the illustrative comment), the
`iss`/`aud` exact-match contract, an OIDC app-registration walkthrough with
Auth0 / Okta / Keycloak / Cognito snippets, a mint-and-test loop, and **both**
attach on-ramps — a real IdP and the no-IdP `harbor token` self-issuing path.
This is the P0 that leads the v1.8.0 Adopter-Path wave: it closes the
serve-attach cliff documentation-first, while `serve`'s verifier stays
untouched.

## RFC anchor

- RFC §5.5 — Authentication (the Protocol's JWT verification surface; identity
  triple in the claims; the Protocol rejects any request without an identity
  scope).
- RFC §4.2 — Mandatory identity (the `(tenant, user, session)` triple is
  mandatory and fails closed).
- RFC §8 — CLI layer (`harbor serve` is the production verifier; the no-IdP
  `harbor token` on-ramp this guide forward-references is a CLI subcommand).

## Briefs informing this phase

- brief 06
- brief 09

## Brief findings incorporated

- brief 06 (Events, Observability, and Developer Experience): the adopter's
  first-attach experience is a make-or-break DevX surface — an undocumented
  step where a developer "follows a stale step, hits a wall, and abandons" is a
  silent funnel leak. This guide closes the single largest documentation gap on
  the PROTOCOL adopter path (no documented way to obtain a token and attach),
  turning a hard cliff into a copy-pasteable walkthrough.
- brief 09 (MCP OAuth, lessons from bifrost): OAuth/OIDC token handling is full
  of foot-guns where a self-consistent-but-wrong configuration passes locally
  and fails against a real verifier. The guide's "decode a real token and
  confirm `iss`/`aud`/claims match `serve.yaml` before wiring a client" step
  applies that lesson directly: a mint with a leftover default `iss`/`aud`
  signs fine but 401s against any real `serve.yaml`, so the guide makes the
  exact-match contract explicit and gives operators a decode-and-verify habit.

## Findings I'm departing from (if any)

None.

## Goals

- A real-framework-quality, honest operator guide for production identity setup
  that an adopter can follow end-to-end without reading Go source.
- Document the authoritative claim shape `serve` verifies, lifted from the
  parser (`internal/protocol/auth/auth.go`), not the illustrative godoc
  comment.
- Make the `iss`/`aud` exact-match contract — the most common production 401 —
  explicit and unmissable.
- Document **both** attach on-ramps so no adopter is left at the cliff: a real
  IdP (this guide + the worked OIDC client) and the no-IdP `harbor token`
  self-issuing path, with an honest grade callout.
- Wire the new page into the published docs site (nav + dead-link gate) and
  cross-link it from the existing auth and build-a-client pages and the
  `use-the-harbor-protocol` skill, in the same change.

## Non-goals

- No code change to `harbor serve`'s auth edge — the verifier is untouched
  (D-263; D-220 preserved). This is a docs phase.
- No new Protocol method, error code, wire type, or config field.
- The `harbor token` subcommand itself ships in 131d; this guide forward-points
  to it (its full reference ships with that subcommand). The worked OIDC client
  and binding round-trip smoke ship in 131c.
- No `harbor serve` config schema change; `examples/serve.yaml` already carries
  the production `identity:` block (131d adds the `jwks_file` stanza beside the
  OIDC one).

## Acceptance criteria

- [ ] `docs/site/protocol/production-identity-setup.md` exists and covers: the
  two on-ramps, the `serve` verification contract, the full claim-shape table
  (lifted from the parser), the `iss`/`aud` exact-match contract, the scope
  vocabulary, OIDC app registration, Auth0 / Okta / Keycloak / Cognito
  snippets, the mint-and-test walkthrough, and the `harbor token` self-issuing
  on-ramp with its honesty/grade callout.
- [ ] The page is registered in `docs/site/.vitepress/config.ts` nav (protocol
  → Adopt) so it is reachable and the dead-link gate covers links into it.
- [ ] `docs/site/protocol/auth-and-identity.md` and
  `docs/site/protocol/build-a-client.md` link to the new guide.
- [ ] `docs/skills/use-the-harbor-protocol/SKILL.md` §1 (the token step)
  forward-points to the new guide and the `harbor token` on-ramp (today it
  shows only the dev-token path).
- [ ] `scripts/smoke/phase-131a.sh` asserts the page is present + nav-mapped,
  documents both on-ramps + the claim shape + the four IdP snippets, and is
  cross-linked from the auth / build-a-client pages and the skill; the
  VitePress build passes with no dead site-internal links.
- [ ] `make drift-audit` passes (frontmatter, RFC `§N.M` references resolve, no
  forbidden names).

## Files added or changed

- `docs/site/protocol/production-identity-setup.md` — new (the guide).
- `docs/site/.vitepress/config.ts` — nav entry under protocol → Adopt.
- `docs/site/protocol/auth-and-identity.md` — link to the guide from the
  Production block.
- `docs/site/protocol/build-a-client.md` — link to the guide from "Get a token".
- `docs/skills/use-the-harbor-protocol/SKILL.md` — §1 token-step forward
  pointer (§18 same-PR skill-drift rule).
- `scripts/smoke/phase-131a.sh` — new (static-only): page present + nav-mapped,
  both on-ramps documented, the claim shape + the four IdP snippets present, and
  the same-PR cross-links from the auth / build-a-client pages and the skill.
- `docs/plans/README.md` — index row + detail block (Status: Pending).
- `docs/decisions.md` — D-263 block.
- `docs/glossary.md` — "production-identity guide" / "on-ramp" terms.

## Public API surface

N/A — documentation phase. No Go API, no Protocol surface, no config schema
change.

## Test plan

- **Unit:** N/A — no code.
- **Integration:** N/A — no cross-subsystem seam. The IdP→serve round-trip is
  exercised by 131c's binding smoke; the `harbor token`→serve leg by 131d's
  smoke. This phase is the docs consumer those phases reference.
- **Conformance:** N/A.
- **Concurrency / leak:** N/A — no reusable artifact.
- **Docs gate:** `scripts/smoke/phase-131a.sh` (page present + nav-mapped +
  both on-ramps + claim shape + IdP snippets + cross-links) and the VitePress
  build (no dead site-internal links) are the binding checks.

## Smoke script additions

`scripts/smoke/phase-131a.sh` (new, static-only — the per-plan smoke the
drift-audit requires) asserts: the guide page exists and is nav-mapped in
`docs/site/.vitepress/config.ts`; it documents both on-ramps (the IdP path and
the `harbor token` self-issuing path), the `(tenant, user, session)` claim
shape, the `iss`/`aud` exact-match contract, and the four IdP snippets (Auth0 /
Okta / Keycloak / Cognito); and the auth / build-a-client pages plus the
`use-the-harbor-protocol` skill forward-point to it. The VitePress dead-link
gate (run by `.github/workflows/docs.yml` on every PR) is the binding check
that every cross-link resolves and the page is reachable.

## Coverage target

N/A — documentation phase (no Go package touched).

## Dependencies

- 130 (the v1.7 band that precedes the wave; the auth/JWKS surface this guide
  documents is shipped). No code dependency on sibling 131 phases — 131c (worked
  OIDC client) and 131d (`harbor token`) consume this guide's claim-shape and
  are forward-referenced; they do not block it.

## Risks / open questions

- The page forward-references the `harbor token` subcommand (131d) before it
  merges. Mitigation: the reference is descriptive and the attach flow is
  identical regardless of issuer; if 131d's flag surface shifts, the §18
  same-PR skill/doc-drift rule and 131d's own PR keep this guide honest.
- Per-provider IdP UIs drift over time. Mitigation: the guide leans on the
  stable OIDC/OAuth2 primitives (issuer, audience, JWKS, custom claims) and
  frames each provider's snippet as "where this provider adds custom claims,"
  not a click-by-click UI tour.

## Glossary additions

- **on-ramp** — a documented, supported path for an adopter to obtain a
  verifiable token and attach a client to `harbor serve`. Harbor ships two: a
  real IdP and the no-IdP `harbor token` self-issuing path.
- **production-identity guide** — `docs/site/protocol/production-identity-setup.md`,
  the operator manual for OIDC app registration, the Harbor claim shape, and
  both attach on-ramps.

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
- [ ] If new vocabulary: glossary updated
- [ ] If a brief finding was departed from: justified above + decisions.md
  entry filed — N/A (no departure).
