# Phase 105 — Console first-attach UX (zero-clicks-to-attached)

## Summary

Fix two bugs that together make the Console's first-attach experience adoption-killing:

1. **The `Settings → Connected Runtimes` card collects only `name` + `baseURL`** — the four other connection fields the Console's connection-loader requires (`token`, `tenant`, `user`, `session`) are missing from the form. An operator who walks the documented Settings path adds a row that LOOKS attached but the connection loader still returns `null`. The card silently does nothing useful.
2. **First load with no attached runtime lands on a page that doesn't direct the operator anywhere actionable.** It should auto-redirect to Settings so the operator at least sees the surface that's supposed to fix the problem.

Together these turn the "open the Console URL, you're attached" UX described in `cmd_console.go` lines 11-23 into a one-page operator dead-end. Both are V1.2-blocking for the first-five-minutes adoption chain documented in `docs/skills/run-the-dev-loop/SKILL.md`.

## RFC anchor

- RFC §7 — Console as Protocol client (the connection surface).
- RFC §1 — Harbor's first-five-minutes adoption guarantee.

## Briefs informing this phase

- brief 13 — operator UX (the first-attach flow is the load-bearing entry point; absence of a zero-config path here erases the entry).
- brief 11 — Console feature surface (Settings page is the documented connection-management home).

## Brief findings incorporated

- **brief 13 (operator UX).** "An operator who can't reliably get a working Playground chat in <5 minutes from `harbor init` won't adopt the framework." The current first-attach flow makes this impossible without DevTools-snippet seeding — both surfaces an operator might find (Settings card + root page) are dead ends.
- **brief 11 (Console feature surface).** Settings is the documented home for connection management; the card MUST be the load-bearing path, not a stub. CONVENTIONS.md §5 already forbids stubbed actions — the partial-field form qualifies.

## Findings I'm departing from (if any)

None.

## Goals

- An operator who runs `harbor console` (zero config), opens the printed URL, and does nothing else lands on a page that directs them toward attachment. No DevTools required.
- The Settings → Connected Runtimes card collects every field the connection loader requires. Submitting the form ATTACHES — no silent no-op.
- The localStorage seed snippet currently documented in `docs/skills/run-the-dev-loop/SKILL.md` becomes a SUPPORTING path (for power users / scripted attach), not the only path. The skill is updated to lead with the Settings card.
- Co-resident `harbor console` (Console + Runtime on the same port) detects the same-origin Runtime and offers a one-click attach on first load — no manual URL entry, no manual token entry.

## Non-goals

- Removing the localStorage-seed code path. Power users and scripted attach (CI, e2e harnesses) rely on it.
- Auto-attaching the Console to ANY same-origin Runtime without operator confirmation. The first-click is the security boundary — the Console asks "Attach to local Runtime?" before writing identity into localStorage.
- Building a new identity-issuance flow. `harbor dev` and `harbor console` already mint dev tokens; Phase 105 plumbs them into the first-attach UX, doesn't replace them.
- A multi-tenant Console deployment first-run flow. That's a separate concern — Phase 105 is single-operator dev posture only.

## Acceptance criteria

- [ ] `ConnectedRuntimesCard.svelte` form gains four fields: `token` (textarea, ≥200 chars expected), `tenant`, `user`, `session` (text inputs, each required). The `onadd` callback signature becomes `(name, baseURL, token, tenant, user, session) → Promise<void>`. The Settings page's `addRuntime` handler writes ALL of them to localStorage via the existing `setConnection` helper (`BgFEiPKu.js::g`).
- [ ] Form-level validation: all six fields required; baseURL must be a parseable URL; token must look like a JWT (three `.`-separated segments). Inline error on submit, no silent no-op.
- [ ] First-load redirect: when `resolveConnection()` returns `null`, the Console's root layout redirects to `/(console)/settings` instead of rendering an empty / disconnected root page. The redirect is one-shot — after attaching, subsequent loads honour the operator's last-visited page.
- [ ] When `harbor console` is co-resident with a Runtime on the same port (always true today), the Settings page detects `window.location.origin` is reachable as a Harbor Runtime (`GET /v1/runtime/info` returns 2xx) and surfaces a prominent "Attach to local Runtime" button that auto-fills baseURL + fetches a fresh dev token from a new `/v1/dev/bootstrap.json` endpoint + seeds identity to `(dev, dev, dev)`.
- [ ] New `/v1/dev/bootstrap.json` endpoint on `harbor dev` + `harbor console` returns `{ baseURL, token, identity: { tenant, user, session }, scopes }` ONLY when the request originates from `127.0.0.1` / `::1` (loopback gate). The endpoint mints a fresh token per call (no token reuse across operator sessions). Returns 403 from non-loopback origins; returns 404 when `HARBOR_DEV_ALLOW_MOCK` is unset AND no operator-configured dev identity exists (fail-loud per §13).
- [ ] `docs/skills/run-the-dev-loop/SKILL.md` is rewritten to lead with the Settings-card path (or the "Attach to local Runtime" button when co-resident); the localStorage DevTools snippet drops to a "Power-user / scripted" footer.
- [ ] `scripts/smoke/phase-105.sh` (live-server) exercises the bootstrap endpoint round-trip + the Settings card's six-field form via Playwright (extending the Phase 75 baseline).

## Files added or changed

- `web/console/src/lib/components/settings/ConnectedRuntimesCard.svelte` — add the four missing form fields + the `onadd` signature widening.
- `web/console/src/routes/(console)/settings/+page.svelte` — wire the new fields through; update `addRuntime` handler.
- `web/console/src/routes/(console)/+layout.svelte` — first-load redirect when `resolveConnection()` is null.
- `web/console/src/lib/components/settings/AttachToLocalCard.svelte` (new) — the "Attach to local Runtime" one-click flow when co-resident.
- `internal/server/dev_bootstrap.go` (new) — the `/v1/dev/bootstrap.json` endpoint + loopback gate.
- `cmd/harbor/cmd_dev.go` + `cmd/harbor/cmd_console.go` — register the bootstrap endpoint on both subcommands.
- `docs/skills/run-the-dev-loop/SKILL.md` — rewrite for the new first-attach paths.
- `scripts/smoke/phase-105.sh` — round-trip smoke.

## Public API surface

- New Protocol-adjacent endpoint `/v1/dev/bootstrap.json` (NOT part of the canonical Protocol surface — it's a dev-only convenience endpoint, gated by loopback). Returns `{ baseURL, token, identity, scopes }`.
- `ConnectedRuntimesCard.svelte` `onadd` signature widens — internal Console API, no Protocol impact.

## Test plan

- **Unit:** `BgFEiPKu.js` / `setConnection` already has tests; extend for the four new fields when set together. `ConnectedRuntimesCard.svelte` gets a component test asserting the form rejects partial submissions.
- **Integration:** Playwright e2e against `harbor console` covering: (1) cold-start root navigates to /settings, (2) Settings card "Attach to local Runtime" button completes round-trip, (3) Settings card manual six-field form completes round-trip. Real `harbor console` co-resident runtime per AGENTS.md §17.3 (no mocks on the seam).
- **Conformance:** N/A.
- **Concurrency / leak:** Bootstrap endpoint goroutine-leak test (mints + serves N=100 concurrent tokens; baseline restored after teardown).

## Smoke script additions

- `scripts/smoke/phase-105.sh` (live-server):
  - assert `GET /v1/dev/bootstrap.json` from `127.0.0.1` returns 200 with required fields.
  - assert `GET /v1/dev/bootstrap.json` with a non-loopback Forwarded header returns 403.
  - assert the Console's Settings page renders the six-field form (HTML smoke, no full Playwright).
  - assert the cold-start redirect to /settings (HTML smoke).

## Coverage target

- `web/console/src/lib/components/settings/`: 80% (existing target).
- `internal/server/`: 80% (existing target).

## Dependencies

- 85k (operator skills — `run-the-dev-loop` is the documented entry; its rewrite is part of this phase).
- 73m (Settings page / Connected Runtimes card baseline — Phase 105 widens the surface it shipped).

## Risks / open questions

- **Loopback gate evasion.** A malicious local-network attacker could spoof `127.0.0.1` via `X-Forwarded-For` if Harbor trusts forwarder headers. The bootstrap endpoint MUST check the actual TCP peer, not request headers. The implementation uses `r.RemoteAddr` and rejects everything that doesn't resolve to a loopback netblock.
- **Token reuse across operator sessions.** A bootstrap-minted token persists in localStorage for 24h (existing dev-token TTL). A second `harbor dev` boot mints a new token — the localStorage seed becomes stale and the Console attaches with a 401-trapping token until the operator hits "Attach to local Runtime" again. Phase 105 surfaces a clear 401-detection + auto-re-bootstrap banner on the Settings page.
- **Same-origin detection false positives.** If the Console is served from a CDN / reverse-proxy that ALSO hosts a Harbor Runtime, the "Attach to local Runtime" button could attach to an unintended runtime. Phase 105 limits the auto-detection to `localhost` / `127.0.0.1` / `::1` origins — non-loopback origins always require manual baseURL entry.
- **The bootstrap endpoint is a fail-loud test-stub failure mode** per §13. It must be off by default in production (`harbor serve`) and on only in `harbor dev` / `harbor console` (the dev subcommands). The endpoint registers via the existing dev-only auth middleware path; `harbor serve` never mounts it.

## Glossary additions

- **Bootstrap endpoint** — the `/v1/dev/bootstrap.json` endpoint `harbor dev` / `harbor console` expose for first-attach. Mints a fresh dev token and returns the full connection envelope (baseURL + token + identity + scopes). Loopback-gated; never exposed by `harbor serve`.
- **First-attach** — the operator-facing flow that turns a fresh `harbor console` open-the-URL into a Console with a live Protocol connection. Phase 105 makes this zero-clicks for the co-resident case and six-clear-fields for the remote case.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: cross-session isolation test passes — covered (the bootstrap endpoint is loopback-only and mints identity-bound tokens; the dev posture is single-operator)
- [ ] Concurrent-reuse test — covered (bootstrap endpoint goroutine-leak + N=100 concurrent mint test under -race)
- [ ] Integration test — covered (Playwright e2e against `harbor console`)
- [ ] If new vocabulary: glossary updated
- [ ] If a brief finding was departed from: justified above + decisions.md entry filed — N/A
