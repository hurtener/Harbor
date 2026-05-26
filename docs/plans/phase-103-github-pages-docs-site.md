# Phase 103 — GitHub Pages docs site (Dockyard parity)

## Summary

Ship a published Harbor docs site on GitHub Pages, mirroring the sibling Dockyard project's VitePress-based posture. Pulls together the operator-facing surfaces Harbor already maintains (`docs/skills/`, `docs/recipes/`, `docs/CONFIG.md`, RFC, glossary, decisions log) into a navigable site at `https://hurtener.github.io/Harbor/` (or the equivalent custom domain). The site is built on every PR (link-check is a load-bearing gate per CLAUDE.md §18) and deployed on every push to `main`.

## RFC anchor

- RFC §1 — Harbor as a Go-native runtime SDK (the docs site is part of the public adoption surface).
- RFC §7 — Console as the in-product UI; the docs site complements it as the asynchronous reference surface.
- RFC §12 — release engineering hygiene (the build + deploy pipeline is CI-owned).

## Briefs informing this phase

- brief 13 — operator UX. "A polished published docs site is one of the strongest adoption signals; absence of one is read as project immaturity."
- brief 06 — events / observability / devx (docs site is a devx product).

## Brief findings incorporated

- **brief 13 (operator UX).** The first-five-minutes adoption chain in `docs/skills/INDEX.md` is the load-bearing entry point. A published site exposes the chain visually (grouped navigation, search, syntax highlighting) and removes the "browse a git repo" friction.
- **brief 06 (devx surface).** Cross-link the godoc surface from the docs site — operators reading the operator playbooks should be one click from the API reference, not two repos away.
- **Dockyard posture (sibling repo `~/Repos/Dockyard/`).** VitePress for the site; `docs/site/` for the VitePress project; `.github/workflows/docs.yml` for build + deploy with link-checking on PR; `DOCS_BASE` env override for the published path. Phase 103 mirrors this exactly so the two projects age together and contributors moving between them see the same shape.

## Findings I'm departing from (if any)

None — VitePress is the chosen stack at Dockyard; reusing the choice keeps drift between the two repos low.

## Goals

- Published site at `https://hurtener.github.io/Harbor/` serves the operator-facing docs (skills, recipes, CONFIG, glossary, RFC index).
- The site is generated, not hand-curated — every section maps to a tracked source file. Drift between source and site is impossible by construction.
- Link-check gate on every PR — a dead internal link (a renamed skill, a deleted glossary term, a moved RFC heading) fails the build.
- Theme matches Dockyard's posture so the two project sites feel like one ecosystem.
- Search works (VitePress ships local search out of the box).
- godoc cross-links: every package landing page on the site links to its pkg.go.dev counterpart.

## Non-goals

- Migrating the README, RFC, or in-repo docs INTO the site as their only home. The repo stays canonical; the site renders from it.
- Replacing the Console with a docs site. The site is asynchronous reference material; the Console is the in-product runtime view.
- Sub-domain / custom-domain setup. Initial deploy is `hurtener.github.io/Harbor/`; a custom domain is an operator follow-up (DNS + Pages config), not a Phase 103 acceptance criterion.
- Hosting search via Algolia DocSearch. VitePress's bundled local search is the V1 ceiling; Algolia is a follow-up if search needs scale.

## Acceptance criteria

- [ ] `docs/site/` (the VitePress project) exists with `package-lock.json` committed.
- [ ] `.github/workflows/docs.yml` builds the site on every PR + every push to `main`, deploys to GitHub Pages on `main` only.
- [ ] The build is itself a link-check gate — a dead internal link to a skill / recipe / glossary term / RFC heading fails CI.
- [ ] Site sitemap renders: Home / Operator skills (with grouped nav matching `docs/skills/INDEX.md`) / Recipes / CONFIG / Glossary / RFC / Decisions log / Changelog / Releases / Contributing.
- [ ] godoc cross-links: every operator skill linking to a Go package has a working pkg.go.dev link in the rendered site.
- [ ] CLAUDE.md §18 drift rule is extended to call out the docs site: a change that renames a skill / recipe / glossary term MUST update the navigation manifest in the same PR.
- [ ] `make docs` builds the site locally (the workflow's local equivalent).
- [ ] The site honours the existing `docs/skills/INDEX.md` grouping (start / build / drive / observe / ship / frontend) as its top-level skills nav.

## Files added or changed

- `docs/site/` — new VitePress project.
  - `docs/site/package.json` + `docs/site/package-lock.json`
  - `docs/site/.vitepress/config.ts` — site config, navigation, theme, search.
  - `docs/site/.vitepress/theme/` — minor theme customisations (logo, color tokens parallel to Dockyard's).
  - `docs/site/index.md` — landing page.
- `.github/workflows/docs.yml` — new workflow (build on PR, build + deploy on `main`).
- `Makefile` — add `docs` + `docs-serve` targets.
- `CLAUDE.md` + `AGENTS.md` §18 — extend drift rule to the docs site's navigation manifest.
- `scripts/smoke/phase-103.sh` — static-only smoke asserting site project exists + workflow is wired.

## Public API surface

N/A — docs site is build artifact only.

## Test plan

- **Unit:** N/A (no Go code touched).
- **Integration:** The PR's docs workflow build is itself the integration gate. A dead link fails CI.
- **Conformance:** N/A.
- **Concurrency / leak:** N/A.

## Smoke script additions

- `scripts/smoke/phase-103.sh` (static-only):
  - assert `docs/site/package.json` exists.
  - assert `docs/site/.vitepress/config.ts` exists.
  - assert `.github/workflows/docs.yml` exists.
  - assert `Makefile` has a `docs:` target.
  - assert CLAUDE.md §18 mentions the docs site navigation.

## Coverage target

N/A — no Go code.

## Dependencies

- 102 (godoc hygiene). Land Phase 102 first so the site renders the cleaned godoc cross-links, not the noisy version.
- 85k (operator skills). The site renders the skills as its load-bearing nav surface; the skills must exist first.

## Risks / open questions

- **Build wall-clock.** VitePress builds Dockyard's site in ~30s; Harbor's larger source tree may push that to ~60-90s. Acceptable; the gate runs in parallel with other CI.
- **Theme drift from Dockyard.** Two project sites with subtly-different themes erode the ecosystem signal. Phase 103 explicitly mirrors Dockyard's theme; future divergence requires an RFC PR.
- **Source-link rot.** The site links to in-repo files (RFC, glossary, skills). Heading renames or file moves break links. Phase 103's link-check gate catches this on PR, but only if the link-check coverage extends to every published page.
- **Search quality.** VitePress local search is good for small sites (<200 pages). Harbor's full surface (skills + recipes + RFC + glossary + decisions) is on the boundary. A follow-up may migrate to Algolia DocSearch (free for open-source projects) if local search degrades.

## Glossary additions

- **docs site** — the published VitePress site at `hurtener.github.io/Harbor/`. Distinct from `docs/` (the in-repo markdown) and `Console` (the in-product runtime UI).

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target — N/A (no Go code touched)
- [ ] If multi-isolation paths changed: cross-session isolation test passes — N/A
- [ ] Concurrent-reuse test — N/A (no Go artifact built)
- [ ] Integration test — N/A (Dependencies are docs-only; the docs workflow's link-check IS the integration gate per §17.1 acceptable shapes)
- [ ] If new vocabulary: glossary updated
- [ ] If a brief finding was departed from: justified above + decisions.md entry filed — N/A
