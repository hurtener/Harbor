# Phase 92i — Console: agent-config revision readability + safe rollback + atomic multi-section save

## Summary

Hardens the revision experience of the agent-config control panel (92h) against the two operator questions the band surfaced: "if I can see the history but can't read what each revision MEANT, how do I roll back safely?" and "can I change prompt + temperature + model in ONE revision instead of three?". This is a **pure Console** phase over already-shipped Protocol surface (92a's `list_revisions` / `diff` / `set_revision` / `rollback`): a per-revision **change summary** derived client-side from each revision's payload vs its parent, a mandatory **diff preview before rollback** (you confirm against the structured `agent_config.diff`, never a blind repoint), and an **atomic multi-section "Save all"** that commits edits across every panel area as ONE `set_revision` revision instead of N single-section convenience-verb revisions.

## RFC anchor

- RFC §7 — Console (observability + control plane; the Protocol-client contract).
- RFC §6.16 — Agent Registry / agent-config control plane (the revision surface this consumes).

## Briefs informing this phase

- brief 11
- brief 12

## Brief findings incorporated

- **brief 11 §"Diff view":** "when an output is 'X vs Y', render a side-by-side or inline diff (useful for plan revisions)." This phase renders the structured `agent_config.diff` (active vs target) as the rollback confirmation surface — the diff is not a nice-to-have, it is the safety gate that answers "what will reverting actually change?".
- **brief 11 (operator vs end-user surface):** the operator surface must make audited, capability-level changes legible. A revision list of opaque content-hashes is not legible; this phase derives a human-readable per-revision change summary (which sections changed, e.g. "Prompt + Model & sampling" or "Rolled back to <short-id>") purely from the payloads `list_revisions` already returns — no new Protocol round-trips.
- **brief 12 §"shared UI library":** the Console anchors on the shared component inventory and the D-121 conventions. This phase builds the summary list, the diff-preview modal, and the Save-all affordance entirely from the existing `ui/` primitives + the `<PageState>` contract — no new bespoke components, no raw token literals.

## Findings I'm departing from (if any)

None. In particular, this phase deliberately does NOT add a stored free-text revision label/note field to the backend `Revision` (which would be a new primitive in a Console phase): the change summary is **derived** from the payload diff, so it is always accurate and never drifts from an out-of-date hand-typed message. Recorded as D-239.

## Goals

- **Per-revision change summary (derived):** for each revision in the history list, compute and render a short summary of what changed versus its parent revision — purely client-side from the `payload` each `AgentConfigRevisionView` already carries (prompt base/user, skills ±, MCP pause/disable ±, connections ±, model & sampling ±). A rollback revision is recognised (its payload equals an ancestor's) and labelled "Rolled back to <short-id>".
- **Diff preview before rollback:** the rollback control no longer repoints blindly. Selecting "Roll back to this revision" opens a confirmation that renders `agent_config.diff(active → target)` using the structured per-section diff arms (skills, tool-exposure, prompt, and — once 92j lands — LLM-params); the operator confirms the explicit delta, or cancels. Admin-gated like every write (disabled-with-tooltip for non-admin, the 92b precedent).
- **Atomic multi-section "Save all":** the panel collects pending edits across all areas (prompt, skills, MCP policy, model & sampling) into one staged payload and commits a single `set_revision` (the full merged payload) — one revision, one `agent.config.revised` event, one diffable unit. The per-section convenience verbs remain available for single-area quick edits, but the panel's primary save path is atomic. A clear "unsaved changes in N areas" indicator + a discard affordance.
- All three are built against `docs/design/console/CONVENTIONS.md` (D-121): inside the `(console)` route group, the shared `ui/` inventory, the four-state `<PageState>`, tokens-only, typed `AgentConfigNamespace`, no hand-rolled fetch.

## Non-goals

- Any backend / Protocol change. The surfaces consumed (`list_revisions` with payloads, `diff`, `set_revision`, `rollback`, `agent.config.revised`) all shipped in 92a; the LLM-params section + its diff arm ship in 92j. If a gap is found, it is fixed in the owning backend phase, not bolted onto this one (§17.6).
- A stored revision label/note field (see "Findings I'm departing from" / D-239).
- The end-user (non-admin) revision surface — operator Console only (the white-label end-user UI is downstream, not this repo).
- Revision pruning / retention policy (a separate concern).

## Acceptance criteria

- [ ] Each revision in the history list renders a derived change summary (sections changed vs parent), computed client-side from the `list_revisions` payloads — no extra Protocol calls; a rollback revision is labelled as such.
- [ ] "Roll back to this revision" opens a diff preview (`agent_config.diff` active → target) showing the per-section delta; rollback only fires on explicit confirm; cancel is a no-op. The control is disabled-with-tooltip for a non-admin (no faked affordance — the §13 "never faked" rule).
- [ ] "Save all" commits pending edits across every area as ONE `set_revision` (full merged payload) → exactly one new revision and one `agent.config.revised` event (asserted by counting revisions before/after). Per-section quick-edit verbs still work.
- [ ] An "unsaved changes in N areas" indicator appears when edits are staged; a discard affordance clears them without a write.
- [ ] All async state routes through `<PageState>` (the data-gated section content); the summary list + diff modal + Save-all controls degrade correctly in the disconnected / non-admin (info) / empty states.
- [ ] `svelte-check --fail-on-warnings` clean; `npm run lint` clean (tokens-only, no hand-rolled fetch); the Playwright spec + vitest unit specs pass; `scripts/smoke/phase-92i.sh` green.
- [ ] Console-consistency section satisfied (see below): no raw literals, shared `ui/` primitives, `(console)` route group, `<PageState>` four-state, typed client.

## Files added or changed

- `web/console/src/routes/(console)/agent-config/+page.svelte` — wire the Save-all primary path + the unsaved-changes indicator + discard.
- `web/console/src/lib/agentconfig/state.svelte.ts` — staged-payload accumulation across areas, the derived per-revision summary computation, the diff-before-rollback flow (fetch diff → confirm → rollback), revision-count assertions for "one revision".
- `web/console/src/lib/components/agentconfig/RevisionHistoryCard.svelte` — render the derived summary per revision + the diff-preview confirmation modal (built from shared `ui/`).
- `web/console/src/lib/components/agentconfig/SaveAllBar.svelte` (new, or folded into the panel header) — the staged-edits indicator + Save all / Discard (shared `ui/` primitives + tokens only).
- `web/console/src/lib/agentconfig/tests/state.svelte.spec.ts` — unit specs for summary derivation, staged-payload merge, one-revision-on-save-all, diff-before-rollback gating.
- `web/console/tests/agent-config-page.spec.ts` — Playwright assertions for the summary list, the diff-preview confirm/cancel, and the unsaved-changes indicator (consistent with the data-less-harness pattern: assert surfaces OUTSIDE the `<PageState>` async boundary; data-gated content is covered by vitest).
- `scripts/smoke/phase-92i.sh`.
- `docs/skills/` — any operator skill demonstrating the agent-config Console flow (§18 same-PR rule).

## Public API surface

```text
N/A — pure Console consumer. No Go / Protocol surface added; consumes
agent_config.{list_revisions,diff,set_revision,rollback} + agent.config.revised
(92a) and the LLM-params section + diff arm (92j) through the typed
AgentConfigNamespace client.
```

## Test plan

- **Unit (vitest):** summary derivation (each section's add/remove/change → the right summary string; a rollback revision recognised + labelled); staged-payload merge across areas (editing prompt + model leaves skills untouched in the merged payload); Save-all issues exactly one `set_revision` with the full merged payload (mock client asserts one call, full payload); diff-before-rollback fetches `diff` and does NOT call `rollback` until confirm; non-admin → the rollback control is disabled (info/disabled, never a silent success).
- **Integration (Playwright, skip-if-console-absent):** against the embedded `harbor console`, with the data-less harness Runtime — assert the surfaces that render OUTSIDE `<PageState>` (the Save-all bar, the unsaved-changes indicator wiring, the rail) hold; the diff-preview + summary content (data-gated) is asserted in vitest where the client is mockable. (Mirrors the 92h data-less-harness pattern.)
- **Conformance:** N/A — no driver surface.
- **Concurrency / leak:** N/A — Console state machine; the live `mcp.connection.*` subscription teardown on unmount is already covered by 92h's state spec (extended if the Save-all path opens/closes anything).

## Smoke script additions

- `scripts/smoke/phase-92i.sh`: static — the Save-all path + staged-payload accumulation symbols in `state.svelte.ts`; the derived-summary + diff-preview-modal markup in `RevisionHistoryCard.svelte`; the `SaveAllBar` (or header) staged-changes indicator; a tokens-only / no-hand-rolled-fetch grep over the touched `.svelte` files. (No live Protocol surface — this phase adds none; the Playwright spec is the live gate via the frontend CI job.)

## Coverage target

- `web/console/src/lib/agentconfig` (the state machine: summary derivation + staged merge + diff-before-rollback): the vitest suite covers the new branches; no Go coverage delta (pure Console).

## Dependencies

- 92a (revision list with payloads + `diff` + `rollback` + `set_revision`), 92h (the consolidated panel this extends), **92j** (the LLM-params section + its diff arm, so the summary + Save-all + diff-preview cover model & sampling). Build order: 92j first, then 92i.

## Risks / open questions

- **Summary accuracy.** A derived summary must never mislead. It is computed from the same payloads `diff` operates on, and a dedicated unit suite pins each section's mapping; a rollback (payload-equals-ancestor) is recognised so it does not read as a fresh edit. If a section is added later (beyond LLM-params), the summary derivation must be extended in that section's phase — noted so it does not silently omit a new section.
- **Atomic save vs partial failure.** `set_revision` is one atomic write, so Save-all is all-or-nothing by construction — no partial-revision risk. The staged state is cleared only on a confirmed success; a failed write keeps the staged edits and surfaces the error (no silent drop, §13).
- **Non-admin must never see a faked control.** Diff-preview and rollback and Save-all are all admin writes; for a non-admin the panel is already in the admin-scope info state (92h) — these controls are not reachable. A test asserts no faked affordance.
- **Diff-preview readability for prompt text.** A large base-prompt delta could be visually heavy; the preview uses the structured `prompt_layers` diff (changed/from/to) and truncates with an expand, reusing the shared diff/disclosure primitives rather than dumping raw text.

## Glossary additions

- **revision change summary** — a Console-derived, human-readable one-line description of what an agent-config revision changed versus its parent (which sections, ±), computed client-side from the revision payloads `list_revisions` returns; the agent-config registry stores no free-text revision label (D-239).
- **diff-before-rollback** — the agent-config Console contract that a rollback renders the structured `agent_config.diff` (active → target) for explicit operator confirmation before repointing the active revision; a rollback is never a blind repoint.

## Console consistency

Built against `docs/design/console/CONVENTIONS.md` (D-121): the panel stays in the `(console)` route group (served at `/agent-config`), inside the app shell; all async state routes through the four-state `<PageState>`; every control is a shared `web/console/src/lib/components/ui/` primitive (no hand-rolled component the inventory already provides); design tokens only (no raw color / spacing / type literals — the stylelint gate); the Runtime is reached only through the typed `HarborClient` / `AgentConfigNamespace` (no hand-rolled `fetch`); Svelte 5 runes (D-092). The Save-all bar and diff-preview modal reuse the existing button / modal / disclosure / diff primitives.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target (vitest branches for the new state)
- [ ] If multi-isolation paths changed: N/A — pure Console consumer, no identity-scoped storage path added.
- [ ] **Concurrent-reuse test:** N/A — Console state machine, not a shared Go artifact (one-line reason).
- [ ] **Integration test:** the Playwright spec exercises the embedded `harbor console` end-to-end; vitest covers the data-gated state. (Deps name shipped phases; the Console e2e is the integration shape.)
- [ ] `svelte-check --fail-on-warnings` + `npm run lint` clean
- [ ] If new vocabulary: glossary updated
- [ ] If a brief finding was departed from: justified above + decisions.md entry filed (D-239)
