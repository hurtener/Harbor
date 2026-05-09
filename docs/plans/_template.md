# Phase NN — <slug>

<!--
Phase plan template. Copy to phase-NN-slug.md and fill in.
Every section is mandatory. Do not delete sections; if a section is N/A, write "N/A — <reason>".
The drift-audit script (`scripts/drift-audit.sh`) checks for required headings + cross-reference validity.
-->

## Summary
<!-- 2-3 sentences. What this phase delivers. -->

## RFC anchor
<!-- Required. List the RFC sections this phase implements. Format exactly: RFC §6.X.
     The drift-audit script verifies every RFC §N.M reference resolves to a real heading. -->
- RFC §

## Briefs informing this phase
<!-- Required. List research briefs whose findings this phase depends on. Format: `brief NN`.
     The drift-audit script verifies every `brief NN` reference resolves to docs/research/NN-*.md.
     If you don't list any, the audit fails — at least one brief always informs phase work. -->
- brief

## Brief findings incorporated
<!-- Required. Quote 2-5 specific findings from the briefs (above) that this phase plan adopts.
     Cite the brief number + section. Example: "brief 02 §5: pause-state serialization must FAIL LOUDLY
     with ErrUnserializable instead of returning nil". This forcing function ensures hard-won
     learnings make it INTO the implementation, not just into the briefs. -->
-

## Findings I'm departing from (if any)
<!-- Required (can be "None"). If this phase plan deliberately departs from a brief finding or RFC
     decision, list it here with explicit justification. Silent departure is forbidden — see
     AGENTS.md §15. If "None", write "None.". -->
-

## Goals
<!-- What this phase must achieve. Outcomes, not implementation. -->
-

## Non-goals
<!-- Explicit out-of-scope items. The "we'll do this in a later phase" list. -->
-

## Acceptance criteria
<!-- Required. Bulleted, testable. These are binding. -->
- [ ]

## Files added or changed
<!-- Tree-style list of files this phase touches. Reference AGENTS.md §3 for the canonical layout;
     a phase that adds a new top-level directory must update AGENTS.md §3 in the same PR. -->
-

## Public API surface
<!-- What other phases depend on. Interface signatures (Go-flavored). Do NOT include internal types. -->
-

## Test plan
<!-- Required. Categorize: unit / integration / conformance / concurrency / leak / fuzz. -->
- **Unit:**
- **Integration:**
- **Conformance:**
- **Concurrency / leak:**

## Smoke script additions
<!-- Required. List the assertions `scripts/smoke/phase-NN.sh` adds. The drift-audit script verifies
     scripts/smoke/phase-NN.sh exists for every phase plan. -->
-

## Coverage target
<!-- Required. Per touched package. e.g. "internal/runtime/engine: 80%". -->
-

## Dependencies
<!-- Required. Phase numbers that must land before this one. -->
-

## Risks / open questions
<!-- Surface real risks. Link to RFC §11 Q-N when applicable. -->
-

## Glossary additions
<!-- If this phase introduces new vocabulary, list the terms here AND add them to docs/glossary.md
     in the same PR. -->
-

## Pre-merge checklist
<!-- Tick when complete. Same checklist that gates the PR review (AGENTS.md §14 + drift-audit). -->
- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: cross-session isolation test passes
- [ ] **If this phase builds a reusable artifact (engine, tool, planner, driver, redactor, client, catalog, etc.): concurrent-reuse test passes — N≥100 concurrent invocations against a single shared instance under `-race`, asserting no data races, no context bleed, no cancellation cross-talk, no goroutine leaks.** See AGENTS.md §5 + §11 + D-025. If this phase does NOT build a reusable artifact, mark this checkbox N/A with a one-line reason.
- [ ] If new vocabulary: glossary updated
- [ ] If a brief finding was departed from: justified above + decisions.md entry filed
