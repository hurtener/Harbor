# Phase 85k — MCP operator skills + Dockyard cross-reference docs

## Summary

Phases 85a–85j ship the *runtime mechanism* for V1.2's MCP wave (client core compliance, HTTP OAuth, sampling, elicitation, roots, server features, Apps host, Tasks). Phase 85k closes the wave with the *operator-facing playbook* that mechanism enables. Ships `docs/skills/` — Claude-Code-style agent skills (one focused playbook per surface, frontmatter + 100–200 line walkthrough) modelled on the sibling Dockyard project's `skills/` convention. Cross-references Dockyard for the server-building half of the loop so an operator (or an agent acting on their behalf) can move end-to-end: scaffold an MCP server with Dockyard → wire it into a Harbor agent → drive its tools, OAuth, Tasks, and Apps from the Console.

## RFC anchor

- RFC §6.4 — MCP southbound contract (the wave's runtime surface).
- RFC §7 — Console as Protocol client (the host surface operators drive the wave through).
- RFC §11 Q-3 — operator-UX bar for V1's launch-readiness pieces.

## Briefs informing this phase

- brief 14 — MCP client compliance (the wave's design anchor; the skills repeat its acceptance contract in operator vocabulary).

## Brief findings incorporated

- **brief 14 §"operator-facing surfaces".** The MCP client compliance brief calls out that a compliant client is necessary but not sufficient for V1.2 launch readiness — the operator surface (Console MCP Connections page, the Phase 85g Apps renderer, the Tasks panel) needs documentation that walks an operator from a fresh `harbor dev` boot to a running MCP-driven agent. Phase 85k is that documentation.
- **brief 14 §"Apps + Tasks are user-visible surfaces, not just protocol features".** The brief is explicit that an operator hosting an MCP App or driving a Task expects a host-grade experience (sandboxed iframe, theme propagation, lifecycle visibility). The skills make this contract legible: each Console-touching surface gets a playbook documenting the host expectations.
- **Dockyard skills convention (sibling repo `~/Repos/Dockyard/skills/`).** Each Dockyard skill is a single `SKILL.md` with a frontmatter block (`name`, `description`, `license`, `metadata` with `framework`/`surface`/`verbs`) plus a focused 100–200 line walkthrough. Phase 85k mirrors this convention exactly so Claude-Code-style agents can discover Harbor's playbooks the same way they discover Dockyard's — and so the chain (Dockyard skill → Harbor skill) reads as one continuous flow when an agent is helping an operator set up MCP-driven Harbor.

## Findings I'm departing from (if any)

None.

## Goals

- An operator wiring an MCP server (built with Dockyard or any other framework) into a Harbor agent has a step-by-step playbook for each Console-touching surface the V1.2 wave exposes.
- Agents (Claude Code, third-party MCP clients) that consume the skill surface can hand the operator a coherent end-to-end flow — server side via Dockyard, host side via Harbor — without re-deriving the integration details.
- The skill surface is mechanically discoverable: a fixed `docs/skills/` layout with one directory per skill, matching Dockyard's `skills/<slug>/SKILL.md` shape.
- Cross-references are explicit and bidirectional: Harbor skills name the Dockyard skill they pick up from (and vice-versa, captured as a Dockyard-side follow-up that lives in that repo's plan).
- The skills DO NOT duplicate the phase plans. Phase plans are design + acceptance contracts (for contributors); skills are operator playbooks (for users). A skill cites the phase plan as its source of truth; the phase plan does not cite the skill.

## Non-goals

- Migrating Harbor's existing `internal/skills/` (the planner's runtime token-savvy skills subsystem — RFC §6.7) to this surface. The two share a name but model different things; the new `docs/skills/` surface is purely operator-facing.
- Owning the Dockyard half of the cross-reference chain. The Dockyard skills already exist (sibling repo); Phase 85k links to them, does NOT mirror or fork them.
- Writing skills for every operator activity. V1.2's scope is the MCP surfaces shipped by Phases 85a–85j. Skills for adjacent V1.1 / earlier surfaces (Playground multimodal, queue-vs-steer, etc.) are out of scope here — a future docs-cleanup phase can backfill them.
- A web-rendered docs site for `docs/skills/`. The skills are markdown files designed to be read in-repo or piped to an agent; a Hugo/MkDocs site is a separate hat (Dockyard ships its own — phase 85k stays in-repo).

## Acceptance criteria

- [ ] `docs/skills/` exists with one directory per shipped skill, each carrying a `SKILL.md` whose frontmatter matches the Dockyard convention exactly: `name`, `description` (one sentence, "Use when…"), `license` (Apache-2.0), `metadata.framework: harbor`, `metadata.surface` (one of `mcp` / `console` / `runtime` / `tasks` / `oauth`), `metadata.verbs` (the relevant `harbor` CLI verbs or the empty string).
- [ ] At least the following skills ship (one `SKILL.md` per directory), matching Phases 85a–85j's surfaces:
  - `attach-an-mcp-server` — wire a stdio / HTTP MCP server into a Harbor agent's `harbor.yaml`; verify tool catalog via the Console MCP Connections page. Anchors Phase 85a (core compliance).
  - `oauth-with-an-mcp-server` — drive a Phase 85b HTTP-OAuth handshake from the operator side via the unified pause/resume primitive; how the Console surfaces the consent prompt. Anchors Phase 85b.
  - `let-an-mcp-server-sample` — the Phase 85c sampling provider: an MCP server requests an LLM completion from the host; how the operator approves the cost ceiling. Anchors Phase 85c.
  - `respond-to-elicitation` — Phase 85d elicitation: an MCP server requests structured operator input; how the Console renders the form / URL mode. Anchors Phase 85d.
  - `expose-roots-to-mcp` — Phase 85e roots: how the operator declares which filesystem roots an MCP server may see. Anchors Phase 85e.
  - `render-an-mcp-app` — Phase 85g host: how a tool returning a `ui://` resource renders in the Console's chat surface; sandbox + CSP posture; cross-references Dockyard's `attach-a-ui-resource` server-side skill. Anchors Phase 85g.
  - `drive-an-mcp-task` — Phase 85h/i Tasks: the `input_required` lifecycle from the host side, how the Console renders the Tasks panel, how task results flow back. Anchors Phase 85h + 85i. Cross-references Dockyard's `approval-flows` template.
  - `troubleshoot-an-mcp-connection` — Console MCP Connections detail rail + the runtime's event stream as the operator's debugging surface; when to use which.
  - `register-handles-mime-tools-with-an-mcp-server` — operator-side guidance for wiring `Tool.HandlesMIME` (V1.1 phase 84a) on MCP-discovered tools so the Playground's multimodal materializer routes correctly. Anchors V1.1 phase 84a + V1.2 phase 85a.
- [ ] Each skill is bounded: 100–250 lines of markdown after the frontmatter; opens with a one-paragraph framing, then numbered steps; closes with a "Common failure modes" subsection.
- [ ] Every Harbor skill that has a matching Dockyard server-side skill (Apps, Tasks, the contract-first server build) names the Dockyard skill explicitly in its "See also" footer.
- [ ] `docs/skills/INDEX.md` lists every shipped skill with one-line descriptions, grouped by surface (`mcp` / `console` / `oauth` / `tasks`).
- [ ] `README.md` gains a "Operator skills" pointer to `docs/skills/INDEX.md`, mirroring the existing pointers to `docs/plans/README.md` etc.
- [ ] `make drift-audit` extension: the audit script learns to verify the skill frontmatter shape (every `SKILL.md` has the four mandatory frontmatter keys + a recognised `surface` value). A skill with a malformed frontmatter fails the audit.
- [ ] Cross-stack consistency check: at least three Harbor skills cite a Dockyard skill by name (`see also: dockyard <slug>`), proving the cross-reference chain is real.

## Files added or changed

- `docs/skills/INDEX.md` (new) — the one-line index, grouped by surface.
- `docs/skills/attach-an-mcp-server/SKILL.md` (new).
- `docs/skills/oauth-with-an-mcp-server/SKILL.md` (new).
- `docs/skills/let-an-mcp-server-sample/SKILL.md` (new).
- `docs/skills/respond-to-elicitation/SKILL.md` (new).
- `docs/skills/expose-roots-to-mcp/SKILL.md` (new).
- `docs/skills/render-an-mcp-app/SKILL.md` (new).
- `docs/skills/drive-an-mcp-task/SKILL.md` (new).
- `docs/skills/troubleshoot-an-mcp-connection/SKILL.md` (new).
- `docs/skills/register-handles-mime-tools-with-an-mcp-server/SKILL.md` (new).
- `scripts/drift-audit.sh` — new check verifying skill frontmatter shape.
- `scripts/skills/check-frontmatter.sh` (new) — extracted helper the drift-audit invokes.
- `README.md` — add the "Operator skills" pointer to the existing docs section.
- `AGENTS.md` / `CLAUDE.md` — add `docs/skills/` to the §3 layout AND a one-paragraph entry in the "Drift hygiene artifacts" callout pointing to the new INDEX (mirror invariant preserved).
- `docs/glossary.md` — add the "Operator skill" entry distinguishing it from `internal/skills/` (the runtime token-savvy skills subsystem).

## Public API surface

None. Skills are markdown; they have no Go / TS / wire surface.

## Test plan

- **Unit:** `scripts/skills/check-frontmatter.sh` exercised by `scripts/drift-audit.sh`. The audit fails when a `SKILL.md` is missing a mandatory frontmatter key, lists an unknown `surface` value, or its slug doesn't match its directory name.
- **Integration:** the `make drift-audit` invocation in CI exercises the skill check end-to-end. A PR that adds a skill with a broken frontmatter fails the gate.
- **Conformance:** N/A — markdown docs have no conformance surface beyond the drift-audit check.
- **Concurrency / leak:** N/A — no reusable artifact.

## Smoke script additions

`scripts/smoke/phase-85k.sh`:

- `assert_file docs/skills/INDEX.md` — the index exists.
- `assert_dir_nonempty docs/skills/` — at least one skill ships.
- Frontmatter sanity per skill: `name:` key present, `metadata.framework: harbor`, `metadata.surface` is one of the canonical values, `description:` length > 80 chars (a one-sentence-with-"Use when" framing).
- Cross-reference sanity: at least three skills mention `dockyard` in a `See also` section (the operator-flow chain is bidirectional).
- README pointer to `docs/skills/INDEX.md` is present.

## Coverage target

N/A — markdown docs.

## Dependencies

- 85a (core compliance) — `attach-an-mcp-server` documents what works once the core MCP client shipped.
- 85b (HTTP OAuth) — `oauth-with-an-mcp-server` documents the operator-side OAuth flow.
- 85c–85f — each MCP-surface phase has a corresponding skill that documents the operator side.
- 85g (Apps host) — `render-an-mcp-app` documents how a `ui://` resource lands in the Console.
- 85h–85i (Tasks) — `drive-an-mcp-task` documents the host-side Tasks lifecycle.
- 84a — `register-handles-mime-tools-with-an-mcp-server` references the `Tool.HandlesMIME` field shipped in V1.1.

Phase 85k lands LAST in the wave (after 85a–85j) so each skill's content can be authored against the actually-shipped surface, not the design intent.

## Risks / open questions

- **Skill rot.** Skills can drift from the phase plans they document if a later phase changes a surface without updating the skill. Mitigation: the drift-audit's skill check is the trip-wire; a skill that names a removed Console route or Protocol method fails the audit. A future phase that mutates a documented surface MUST update the skill in the same PR (§17.6 rule).
- **Where the Dockyard ↔ Harbor cross-reference chain lives.** Phase 85k owns the Harbor side. The Dockyard side (Dockyard skills mentioning Harbor as a reference host) is captured as a Dockyard-repo follow-up; Phase 85k does NOT modify the Dockyard repo. The Harbor "See also" footers cite Dockyard skills by stable slug; if Dockyard renames a skill, the audit catches the broken link at the next CI pass against a Dockyard-pinned manifest. Open question: do we pin Dockyard's skill slugs in this phase's docs, or treat the cross-reference as a soft link? V1.2 ships as soft links (humans + agents resolve them); V1.3 can harden the chain if drift becomes a real problem.
- **Discoverability.** A `docs/skills/` directory the operator never sees has no value. The README pointer + the AGENTS.md / CLAUDE.md drift-hygiene callout are the discoverability hooks. Whether a future Console-side "Help" surface should also link to the skills is a separate UX call (out of scope for V1.2).
- **License field in the frontmatter.** Dockyard skills declare `license: Apache-2.0`. Harbor's repo carries an Apache-2.0 license too. Use the same value verbatim so a Claude Code agent walking both repos sees a consistent licensing posture.

## Glossary additions

- **Operator skill** — A markdown playbook under `docs/skills/<slug>/SKILL.md` documenting a specific operator activity (e.g. "attach an MCP server", "drive an MCP Task"). Distinct from `internal/skills/` — the planner's runtime token-savvy skills subsystem (RFC §6.7). Operator skills are read by humans and Claude-Code-style agents; runtime skills are consumed by the LLM during planning turns. Add to `docs/glossary.md` with this exact distinction.

## Pre-merge checklist

- [ ] `make drift-audit` passes (including the new skill-frontmatter check)
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target — N/A, docs only
- [ ] If multi-isolation paths changed: N/A — docs only
- [ ] N/A — this phase ships no reusable artifact in the D-025 sense.
- [ ] N/A — this phase consumes phase plans (already-shipped surfaces), not new subsystem seams.
- [ ] Glossary updated (operator-skill vs runtime-skill distinction)
- [ ] If a brief finding was departed from: N/A
