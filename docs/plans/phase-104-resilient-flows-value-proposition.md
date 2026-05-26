# Phase 104 — Composable resilient flows as a load-bearing value proposition

## Summary

Surface Harbor's existing composable-flow surface (Engine + Flow-as-Tool + routers + concurrency utils + subflows + the unified pause/resume primitive) as a **headline value proposition**, alongside multi-isolation, swappable planner, and headless runtime. Today the resilience + composition story is implemented end-to-end but barely mentioned in operator-facing material — operators discover it incidentally rather than as a reason to choose Harbor. Eino (`cloudwego/eino`) positions a smaller composable surface explicitly ("composition as the deterministic complement to agent autonomy") and outranks Harbor on that signal despite Harbor having the deeper feature set. Phase 104 fixes the framing: RFC §1, README.md, the operator-skills entry chain, and a new build-a-resilient-flow skill all foreground the flow surface as a first-class adoption story.

## RFC anchor

- RFC §1 — Harbor's product framing (the headline value props live here).
- RFC §6.1 — runtime engine, routers, concurrency utils, subflows (the implementation the framing now points at).

## Briefs informing this phase

- brief 01 — core runtime (the Engine + Flow primitives that motivate the positioning).
- brief 13 — operator UX (the adoption funnel; framing decisions here are operator-facing).

## Brief findings incorporated

- **brief 01 (core runtime).** "The Engine + routers + subflows + reliability shell give Harbor a composable-flow surface that is uncommon among Go agent frameworks. The decision to ship it as a first-class primitive (rather than a stretch goal) was load-bearing." Phase 104 makes the implementation's depth visible in the framing.
- **brief 13 (operator UX).** "Operators don't choose frameworks on feature checklists; they choose on the one-sentence framing they read in the README. Harbor's current framing emphasises isolation and steerability — both true, both important, but the composable-flow surface is INVISIBLE in the framing despite being a peer feature." Phase 104 inserts flow-composition into the headline framing without displacing the existing value props.
- **Eino's positioning (`cloudwego/eino`).** Their README header — "Composition: connect components into graphs and workflows that can run standalone or be exposed as tools for agents" — packages a smaller surface than Harbor's into a punchier framing. Harbor mirrors this style with concrete Harbor surfaces.

## Findings I'm departing from (if any)

None.

## Goals

- RFC §1 names "composable resilient flows" as one of Harbor's headline non-negotiable product properties, alongside multi-isolation, headless-runtime-as-Protocol-client, and swappable planner. The four headline props form a balanced quartet rather than a triumvirate.
- README.md "Why Harbor" / framing section gains a "Composable resilient flows" paragraph using language modelled on eino's positioning — concrete Harbor surfaces (Flow-as-Tool, retry/timeout policies, routers, concurrency utils, subflows, pause/resume) rather than abstract framing.
- A new operator skill `docs/skills/build-a-resilient-flow/SKILL.md` walks an operator from "I have a deterministic multi-step pipeline" to "it runs as a tool the planner can call, with bounded retry + timeout, cancellation-aware concurrency, and an explicit pause point for HITL approval." The skill is the load-bearing concrete that earns the framing.
- A comparison note (in README.md or a new `docs/comparison.md`) explicitly contrasts Harbor's flow surface with eino's compose primitive and LangGraph's graph + retry policies, so operators evaluating frameworks see why Harbor's composition story is deeper.
- The skill INDEX (`docs/skills/INDEX.md`) gets a new "Build a resilient flow" entry in its "Build the agent" section.
- The existing `docs/recipes/` (if present) gains a runnable example that pairs with the skill.

## Non-goals

- Building new Engine / Flow primitives. The implementation already ships; Phase 104 is purely framing + skill authoring.
- Migrating existing operator skills to mention flows. Each existing skill keeps its scope; only the INDEX + RFC §1 + README are updated for top-level framing.
- Adding GitHub Discussions / Twitter / Hacker News announcement copy. The positioning lands in-repo; external announcements are a marketing follow-up.
- Editing benchmark numbers or performance claims. The positioning is qualitative (what Harbor offers) not quantitative (how fast).
- Comparing against every framework. The comparison is bounded — eino + LangGraph — because those are the two operators commonly mention in framework-pick conversations alongside Harbor.

## Acceptance criteria

- [ ] RFC §1 names "composable resilient flows" as one of four (was: three) non-negotiable product properties.
- [ ] README.md gains a "Composable resilient flows" framing paragraph that:
  - References Flow-as-Tool, retry/timeout policies, routers + concurrency utils, subflows, pause/resume by name.
  - Cites the language model eino uses ("deterministic complement to agent autonomy") with explicit attribution.
  - Is one paragraph + 4-6 bullets, not a wall of text.
- [ ] New `docs/skills/build-a-resilient-flow/SKILL.md` exists with the standard Dockyard-style frontmatter (`name`, `description` carrying "Use when", `license: Apache-2.0`, `metadata.framework: harbor`, `metadata.surface: tools` (closest fit) or a new `metadata.surface: flow` value if the canonical-surface set is extended, `metadata.verbs`).
- [ ] If `metadata.surface: flow` is the chosen surface value: `scripts/skills/check-frontmatter.sh` is extended to recognise it.
- [ ] `docs/skills/INDEX.md` lists the new skill under "Build the agent."
- [ ] `docs/comparison.md` (or a section in README.md — implementer's choice) contrasts Harbor's flow surface with eino's `compose` primitive and LangGraph's graph + retry policies. The comparison is factual, not adversarial — each framework's strengths are named.
- [ ] No regressions on the first-five-minutes adoption chain (scaffold → dev-loop → playground). The framing change must NOT bury the existing entry points.
- [ ] CHANGELOG entry under "Added — Positioning" for the V1.2 cut.

## Files added or changed

- `RFC-001-Harbor.md` §1 — extend the headline product properties from three to four.
- `README.md` — new "Composable resilient flows" framing paragraph.
- `docs/skills/build-a-resilient-flow/SKILL.md` (new) — the load-bearing concrete.
- `docs/skills/INDEX.md` — list the new skill.
- `docs/comparison.md` (new, optional) OR a comparison section in `README.md` — vs eino + LangGraph.
- `scripts/skills/check-frontmatter.sh` — if `metadata.surface: flow` is added, extend the canonical set.
- `scripts/smoke/phase-104.sh` — static-only smoke asserting the framing pieces all landed.
- `CHANGELOG.md` — V1.2 entry mentioning the positioning change.
- `docs/glossary.md` — if "Composable resilient flows" becomes a defined term, add it.

## Public API surface

N/A — no Go API changes. The implementation surface stays as-is; Phase 104 is naming + framing.

## Test plan

- **Unit:** N/A (docs-only).
- **Integration:** N/A.
- **Conformance:** N/A.
- **Concurrency / leak:** N/A.
- **Manual:** A fresh contributor (or a Claude-Code agent acting on their behalf) reads README.md and answers "what does Harbor offer that LangGraph doesn't?" — answer should include flow composition + retry/timeout/concurrency-aware policies as a first-class headline, not buried in the architecture section.

## Smoke script additions

- `scripts/smoke/phase-104.sh` (static-only):
  - assert RFC §1 mentions "composable resilient flows" or close synonym.
  - assert README.md has a "Composable resilient flows" section heading.
  - assert `docs/skills/build-a-resilient-flow/SKILL.md` exists with valid frontmatter.
  - assert `docs/skills/INDEX.md` references the new skill.
  - assert (if `docs/comparison.md` is the chosen home) the file exists and mentions both eino and LangGraph.

## Coverage target

N/A — no Go code.

## Dependencies

- 85k (operator skills). The new skill plugs into the existing INDEX structure; that must exist first.
- 103 (docs site, optional). Phase 104 lands cleanly without the docs site, but the framing is more visible WITH it. Ordering: 102 → 104 → 103 is acceptable; so is 102 → 103 → 104.

## Risks / open questions

- **Framing inflation.** Adding a fourth headline value prop dilutes the original three. The reviewer should confirm the four-prop set still reads as the project's identity, not as a feature checklist.
- **Maintenance.** Every framing claim in README.md needs to stay true as implementation evolves. A future phase that weakens retry/timeout coverage must also update the framing or remove the claim — same drift discipline as CLAUDE.md §18 (operator skills).
- **eino attribution etiquette.** Citing a competitor's framing language is fair use, but the cite should be clean — name the project, link the README, quote the line. Phase 104 explicitly names eino in-text rather than borrowing without attribution.
- **Surface label drift.** If `metadata.surface: flow` is introduced, it needs a glossary entry and a permanent home in `scripts/skills/check-frontmatter.sh`'s canonical set + CLAUDE.md §18's enumeration. Phase 104 takes the full slate or chooses an existing surface (`tools` is the safe fit if the flow lives behind Flow-as-Tool).

## Glossary additions

- **Composable resilient flows** — Harbor's combination of Engine + Flow-as-Tool + routers + concurrency utils + subflows + retry/timeout policies + pause/resume. The "composition as deterministic complement to agent autonomy" framing, surfaced as a headline value proposition in V1.2.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target — N/A (docs-only)
- [ ] If multi-isolation paths changed: cross-session isolation test passes — N/A
- [ ] Concurrent-reuse test — N/A (no Go artifact built)
- [ ] Integration test — N/A (docs-only; the smoke is the gate per §17.1 acceptable shapes)
- [ ] If new vocabulary: glossary updated
- [ ] If a brief finding was departed from: justified above + decisions.md entry filed — N/A
