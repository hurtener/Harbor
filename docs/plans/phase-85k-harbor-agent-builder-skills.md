# Phase 85k — Harbor agent-builder skills

## Summary

Ship `docs/skills/` — Claude-Code-style agent skills (one focused playbook per surface, frontmatter + 100–200 line walkthrough) modelled on the sibling Dockyard project's `skills/` convention. **Adoption surface, not MCP-specific.** Dockyard's skills are scoped to its product (build production MCP servers and Apps in Go); Harbor's skills are scoped to its product — building durable, steerable, event-driven AI agents fast and reliably. Wiring MCP servers is ONE skill in the broader set; the other nine cover scaffolding, the agent yaml, adding in-process tools, the LLM provider, memory + skills configuration, the dev loop, the Playground operator UX, the Console as observability surface, and validate+package. The set is bounded (~10 skills, matching Dockyard's surface size). Cross-references Dockyard where the agent-author flow naturally crosses into server-building (`attach-an-mcp-server` cites Dockyard's `scaffold-a-server`).

## RFC anchor

- RFC §1 — Harbor as a Go-native runtime SDK for durable, steerable AI agents (the product these skills drive adoption of).
- RFC §7 — Console as Protocol client (the surface most skills walk an operator through).
- RFC §11 Q-3 — operator-UX bar.

## Briefs informing this phase

- brief 13 — ReAct prompt engineering + Playground operator UX.
- brief 14 — MCP client compliance (anchors the `attach-an-mcp-server` skill).

## Brief findings incorporated

- **brief 13 §"Playground operator UX".** An agent author who can't reliably get a working Playground chat in <5 minutes from `harbor init` won't adopt the framework. Skills are the load-bearing answer: `scaffold-a-harbor-agent` + `run-the-dev-loop` + `drive-the-playground` form the V1 onboarding chain.
- **brief 14 §"operator-facing surfaces".** MCP client compliance is necessary but not sufficient for adoption — operators need a playbook for wiring servers, troubleshooting connections, and understanding what shows up in the Console MCP Connections page. `attach-an-mcp-server` + `observe-with-the-console` cover this.
- **Dockyard skills convention (sibling repo `~/Repos/Dockyard/skills/`).** Each Dockyard skill is a single `SKILL.md` with a frontmatter block (`name`, `description` starting "Use when…", `license`, `metadata` with `framework`/`surface`/`verbs`) plus a focused 100–200 line walkthrough. Dockyard ships 8 skills covering its core product flow: `scaffold-a-server`, `add-a-tool`, `define-contracts`, `run-the-dev-loop`, `attach-a-ui-resource`, `test-with-the-inspector`, `validate`, `package`. Phase 85k mirrors this convention exactly — same frontmatter shape, same scoping discipline (one activity per skill), same bounded length — so Claude-Code-style agents can discover and chain Harbor + Dockyard skills as one continuous flow when helping an operator build an MCP-driven agent (Dockyard skill → Harbor skill, or vice-versa).

## Findings I'm departing from (if any)

None.

## Goals

- Agent author who has never touched Harbor before can boot a working agent + Playground in <5 minutes by following `scaffold-a-harbor-agent` → `run-the-dev-loop` → `drive-the-playground`.
- Each Console-surfaced subsystem (Sessions, Tasks, Events, MCP Connections, Memory, Playground, Live Runtime) has a corresponding operator playbook so "what does this page mean" never requires reading source.
- Wiring an MCP server (built with Dockyard or any other framework) into a Harbor agent is one skill cleanly cross-referenced with Dockyard's `scaffold-a-server`, so the chain (build server → wire to host → drive in Console) reads end-to-end.
- The skill surface is mechanically discoverable: fixed `docs/skills/<slug>/SKILL.md` layout matching Dockyard 1:1. A Claude-Code-style agent that works against Dockyard skills can immediately work against Harbor skills.
- The bounded scope (~10 skills) is the V1 ceiling; a future docs phase can backfill adjacent surfaces (eval, deploy-to-cloud, etc.) without re-litigating the skill convention.

## Non-goals

- Migrating Harbor's existing `internal/skills/` (the planner's runtime token-savvy skills subsystem — RFC §6.7) to this surface. The two share a name but model different things; `docs/skills/` is purely operator-facing playbooks.
- Owning the Dockyard half of the cross-reference chain. The Dockyard skills already exist (sibling repo); Phase 85k links to them, does NOT mirror or fork them. A Dockyard-side follow-up adding "See also: Harbor `attach-an-mcp-server`" cross-references lives in that repo's plan.
- Writing a skill for every possible Harbor activity. The bounded set (~10) is the V1 ceiling. Phase 85k commits to the agent-builder core flow; adjacent surfaces (cloud deploy, custom planner concretes, eval harness, multi-tenant operator playbooks) are tracked as a future docs phase.
- A web-rendered docs site for `docs/skills/`. Skills are markdown files designed to be read in-repo or piped to an agent. A Hugo / MkDocs site is a separate hat (Dockyard ships its own — phase 85k stays in-repo).
- Tutorial-style "build a SaaS agent in 30 minutes" walkthroughs. Skills are focused playbooks (one activity per skill), not tutorials. A long-form tutorial that *uses* the skills is a separate docs artifact.

## Acceptance criteria

- [ ] `docs/skills/` exists with one directory per shipped skill, each carrying a `SKILL.md` whose frontmatter matches the Dockyard convention exactly: `name`, `description` (one sentence starting or containing "Use when…"), `license: Apache-2.0`, `metadata.framework: harbor`, `metadata.surface` (one of `cli` / `agent-yaml` / `tools` / `mcp` / `llm` / `memory` / `playground` / `console` / `tasks`), `metadata.verbs` (the relevant `harbor` CLI verbs or the empty string).
- [ ] The following 10 skills ship (one `SKILL.md` per directory):
  - `scaffold-a-harbor-agent` — `harbor init` (or equivalent) to start a new agent project. Lands a config + Console attach that works on first `harbor dev` invocation. Parallel to Dockyard's `scaffold-a-server`.
  - `define-the-agent-yaml` — the `harbor.yaml` contract end-to-end: identity, llm, planner, memory, skills, tools, state, events, artifacts. The "edit your config" playbook. Parallel to Dockyard's `define-contracts`.
  - `add-an-in-process-tool` — write a typed Go tool, register on the catalog, validate args, surface in the planner's prompt. Parallel to Dockyard's `add-a-tool`.
  - `attach-an-mcp-server` — wire an MCP server (built with Dockyard or any other framework) into `harbor.yaml`'s `tools.mcp_servers` block; verify via the Console MCP Connections page. **Cross-references Dockyard's `scaffold-a-server` + `add-a-tool` for the server-build half.**
  - `wire-the-llm-provider` — configure bifrost + provider (OpenRouter, OpenAI, Anthropic direct), model selection, the dev-only `HARBOR_DEV_ALLOW_MOCK` posture vs the production fail-loud-on-missing-key path. Anchors §13 "test stubs are NEVER production defaults".
  - `configure-memory-and-skills` — strategy choice (`truncation` vs `summarization`), token budget, the localdb skill store + `skills_context_max`, the Skills.md importer for operator-curated skills.
  - `run-the-dev-loop` — `harbor dev` boot, single-process vs multi-process Console attach, hot reload, the `HARBOR_DEV_TOKEN` for the Console handshake. Parallel to Dockyard's `run-the-dev-loop`.
  - `drive-the-playground` — operator chat flow end-to-end: send a message, attach an image (multimodal — V1.1 phase 84a `HandlesMIME` + the per-MIME dispatcher), queue vs steer while a run is in flight (V1.1 phase F10), inspect the right rails, restart a run. Parallel to Dockyard's `test-with-the-inspector`.
  - `observe-with-the-console` — 14-page tour: Sessions, Tasks, Events, MCP Connections, Memory, Artifacts, Live Runtime, Playground, Agents, Tools, Flows, Background Jobs, Settings, Overview. Where to look for what; the four-state `<PageState>` async contract (D-121) so an operator reads disconnected / loading / error / info / empty / ready correctly.
  - `validate-and-package` — `make preflight`-equivalent for operator projects, `harbor build` (CGo-free static binary stamped via ldflags), release posture (D-139). Parallel to Dockyard's `validate` + `package`.
- [ ] Each skill is bounded: 100–250 lines of markdown after the frontmatter; opens with a one-paragraph framing, then numbered steps; closes with a "Common failure modes" subsection.
- [ ] `attach-an-mcp-server` explicitly cross-references Dockyard's `scaffold-a-server` + `add-a-tool` in a "See also" footer (the cross-reference chain is real, not aspirational).
- [ ] `docs/skills/INDEX.md` lists every shipped skill with a one-line description, grouped by phase of the agent-author flow: **start** (scaffold / yaml / dev-loop), **build** (in-process tools / MCP servers / LLM provider / memory + skills), **drive** (Playground), **observe** (Console), **ship** (validate + package).
- [ ] `README.md` gains an "Operator skills" pointer to `docs/skills/INDEX.md`, mirroring the existing pointers to `docs/plans/README.md` etc.
- [ ] `make drift-audit` extension: the audit script learns to verify the skill frontmatter shape (every `SKILL.md` has the four mandatory frontmatter keys + a recognised `surface` value). A skill with a malformed frontmatter fails the audit.
- [ ] Cross-stack consistency check: at least three Harbor skills cite a Dockyard skill by name (`see also: dockyard <slug>`), proving the cross-reference chain is real.
- [ ] **First-five-minutes test (manual)** — a fresh contributor (or a Claude-Code agent acting on their behalf) follows `scaffold-a-harbor-agent` → `run-the-dev-loop` → `drive-the-playground` end-to-end against a clean repo and gets a working chat against a real LLM provider in under five minutes. This is the load-bearing adoption signal; phase 85k is NOT done if it takes more.

## Files added or changed

- `docs/skills/INDEX.md` (new) — the one-line index, grouped by agent-author phase.
- `docs/skills/scaffold-a-harbor-agent/SKILL.md` (new).
- `docs/skills/define-the-agent-yaml/SKILL.md` (new).
- `docs/skills/add-an-in-process-tool/SKILL.md` (new).
- `docs/skills/attach-an-mcp-server/SKILL.md` (new).
- `docs/skills/wire-the-llm-provider/SKILL.md` (new).
- `docs/skills/configure-memory-and-skills/SKILL.md` (new).
- `docs/skills/run-the-dev-loop/SKILL.md` (new).
- `docs/skills/drive-the-playground/SKILL.md` (new).
- `docs/skills/observe-with-the-console/SKILL.md` (new).
- `docs/skills/validate-and-package/SKILL.md` (new).
- `scripts/drift-audit.sh` — new check verifying skill frontmatter shape.
- `scripts/skills/check-frontmatter.sh` (new) — the extracted helper the drift-audit invokes.
- `README.md` — add the "Operator skills" pointer to the existing docs section.
- `AGENTS.md` / `CLAUDE.md` — add `docs/skills/` to the §3 layout AND a one-paragraph entry in the "Drift hygiene artifacts" callout pointing to the new INDEX (mirror invariant preserved).
- `docs/glossary.md` — add the "Operator skill" entry distinguishing it from `internal/skills/` (the runtime token-savvy skills subsystem).

## Public API surface

None. Skills are markdown; they have no Go / TS / wire surface.

## Test plan

- **Unit:** `scripts/skills/check-frontmatter.sh` exercised by `scripts/drift-audit.sh`. The audit fails when a `SKILL.md` is missing a mandatory frontmatter key, lists an unknown `surface` value, or its slug doesn't match its directory name.
- **Integration:** the `make drift-audit` invocation in CI exercises the skill check end-to-end. A PR that adds a skill with a broken frontmatter fails the gate.
- **Manual:** the first-five-minutes test against a clean repo (acceptance criterion above) — performed once at phase merge; documented in the PR description with a recording or transcript.
- **Conformance:** N/A — markdown docs have no conformance surface beyond the drift-audit check.
- **Concurrency / leak:** N/A — no reusable artifact.

## Smoke script additions

`scripts/smoke/phase-85k.sh`:

- `assert_file docs/skills/INDEX.md` — the index exists.
- `assert_dir_nonempty docs/skills/` — at least one skill ships.
- Frontmatter sanity per skill: `name:` key present, `metadata.framework: harbor`, `metadata.surface` is one of the canonical values, `description:` length > 80 chars (a one-sentence-with-"Use when" framing).
- Cross-reference sanity: at least three skills mention `dockyard` in a `See also` section (the operator-flow chain is bidirectional).
- README pointer to `docs/skills/INDEX.md` is present.
- All 10 expected skill directories exist (the acceptance set is the minimum surface; future PRs can extend without re-litigating the convention).

## Coverage target

N/A — markdown docs.

## Dependencies

- **All V1 + V1.1 surfaces this set documents**: the entire shipped runtime + Console + Playground stack at V1.1 closure. Phase 85k can land any time AFTER V1.1's last surface stabilises (concretely: after phase 84a / round-9, which closes the F1 + F8 V1.1-readiness items).
- **`attach-an-mcp-server` depends on V1.2's 85a** (core compliance) for the wired surface it documents. The skill itself can be authored against the brief 14 design now; the final "verify via Console MCP Connections" walkthrough needs 85a's wire shape to be concrete.
- **Cross-references to Dockyard skills** depend only on those skills existing in the sibling repo at stable slugs. They do today.

Phase 85k can ship V1.2-or-sooner; concretely, it should land alongside V1.2's MCP wave (the wave's headline operator surface — `attach-an-mcp-server` — needs 85a's wire shape stable) but the other nine skills are READY for V1.1.5 or V1.2's first cut.

## Risks / open questions

- **Skill rot.** Skills can drift from the surfaces they document if a later phase changes a Console route or wire shape without updating the skill. Mitigation: the drift-audit's skill check is the trip-wire; a skill that names a removed route or method fails the audit. A future phase that mutates a documented surface MUST update the skill in the same PR (§17.6 rule).
- **Where the Dockyard ↔ Harbor cross-reference chain lives.** Phase 85k owns the Harbor side. The Dockyard side (Dockyard skills mentioning Harbor as a reference host) is captured as a Dockyard-repo follow-up; Phase 85k does NOT modify the Dockyard repo. Harbor "See also" footers cite Dockyard skills by stable slug. If Dockyard renames a skill, the audit catches the broken link at the next CI pass against a Dockyard-pinned manifest. Open question: do we pin Dockyard's skill slugs in this phase's docs, or treat the cross-reference as a soft link? V1.2 ships as soft links (humans + agents resolve them); V1.3 can harden the chain if drift becomes a real problem.
- **First-five-minutes test is manual.** The acceptance criterion is real ("fresh contributor reaches a working chat in <5 min") but un-automatable today. Mitigation: the phase PR description records the actual run (timing, transcript or recording). A future docs-CI phase could automate this via a Playwright-driven scaffold-then-chat fixture.
- **Discoverability.** A `docs/skills/` directory the operator never sees has no value. The README pointer + the AGENTS.md / CLAUDE.md drift-hygiene callout are the discoverability hooks. Whether a future Console-side "Help" surface should also link to the skills is a separate UX call (out of scope for V1.2).
- **License field in the frontmatter.** Dockyard skills declare `license: Apache-2.0`. Harbor's repo carries an Apache-2.0 license too. Use the same value verbatim so a Claude Code agent walking both repos sees a consistent licensing posture.

## Glossary additions

- **Operator skill** — A markdown playbook under `docs/skills/<slug>/SKILL.md` documenting a specific operator activity (e.g. "scaffold a Harbor agent", "attach an MCP server", "drive the Playground"). Distinct from `internal/skills/` — the planner's runtime token-savvy skills subsystem (RFC §6.7). Operator skills are read by humans and Claude-Code-style agents; runtime skills are consumed by the LLM during planning turns. Add to `docs/glossary.md` with this exact distinction.

## Pre-merge checklist

- [ ] `make drift-audit` passes (including the new skill-frontmatter check)
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target — N/A, docs only
- [ ] If multi-isolation paths changed: N/A — docs only
- [ ] N/A — this phase ships no reusable artifact in the D-025 sense.
- [ ] N/A — this phase consumes already-shipped surfaces, not new subsystem seams.
- [ ] First-five-minutes test executed against a clean repo; recording / transcript attached to the PR description
- [ ] Glossary updated (operator-skill vs runtime-skill distinction)
- [ ] If a brief finding was departed from: N/A
