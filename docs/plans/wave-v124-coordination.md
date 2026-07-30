# Wave v1.24 — coordination

This file is the §17.7 staging record. The first draft of this wave was rewritten after
four adversarial reviews found FAIL-level defects in every phase; the verified findings
live in `wave-v124-gate0-findings.md` and the operator's rulings are recorded below.

## Operator rulings (settled — do not re-litigate)

1. **The heavy-content threshold splits by purpose rather than moving as one number.**
   The offload path, the LLM-edge leak guard and the trajectory budget rise to 128 KiB;
   the terminal-UI fold, both MCP-App consumers, the search preview bound and every
   Protocol-visible consumer stay at 32 KiB with their own named constants. Rationale:
   the win being bought is the removal of a second tool call for ordinary 32–128 KiB
   results; nothing else should change behaviour, and four of the seven consumers reach
   surfaces where a silent ref→inline flip is a wire-behaviour change (§8).

2. **Byte-eligibility for MCP egress is WIRE-CONFIGURABLE, not boot-declared.** A review
   argued for a fail-closed boot gate on an admin-exfiltration chain. The ruling: D-301
   already records that "a shared runtime TRUSTS its co-tenant admins", so that chain
   sits inside an already-accepted trust boundary — and an admin who can ask an agent a
   question and read the answer on screen can already move that content by hand. A boot
   gate would break the real use case, which is an MCP server attached over the Protocol
   being usable without a redeploy. **Phase 214 states the accepted trust boundary
   explicitly rather than claiming an isolation property it does not have.**
   **Compensating addition:** the FACT of a substitution — artifact id → server, never
   the bytes — is recorded in the audit trail and the trajectory, because the one real
   difference from pasting by hand is that pasting leaves a trace.

3. **A caller-named agent does not change credential identity.** The RFC 8693 acting
   principal continues to derive from the boot value. A caller-named agent changes
   prompts, tools, skills and the other config projections only. This is stated in the
   decision text so nobody later "fixes" it by threading the caller's value through.

4. **Agent validation accepts an id that either equals the runtime's configured default
   OR has a config revision for `(tenant, agentID)`.** There is no base revision — the
   registry returns "no active pointer exists" for an unconfigured agent, which is what
   the boot agent does today — so a config-revision-only check would refuse the one id
   the Console can currently offer.

5. **User-owned MCP connections are dropped from this wave and re-scoped.** The intended
   product shape is users selecting from ALREADY-CONFIGURED capabilities and enabling
   them on their own agent config, at config time — not users declaring arbitrary
   connections at runtime. That removes the SSRF and credential-plane surface entirely
   and makes the eventual phase resemble the existing user-scope `ToolExposure`
   mechanism rather than connection authorship. It also depends on a mechanism that does
   not exist yet, which phase 216 now builds.

6. **The search user-axis gate gets a phase plan, not an issue.** It is a §6 rule 2
   violation and phase 213 is already editing the same package.

7. **Scope is not the constraint.** Parallelise to get the wave built, live-verified and
   merged. **Do not tag a release until the operator aligns.**

## Scope

| Phase | Title | Stage | D-NNN |
|---|---|---|---|
| 212 | Artifact read-path byte correctness | 1 | D-357 |
| 213 | Heavy-content threshold split by purpose | 1 | D-358 |
| 215 | Caller-named agent selection | 1 | D-360 |
| 217 | `meta_annotations` honour `_meta` path nesting | 1 | D-362 |
| 214 | The MCP arm of pass-by-reference routing | 2 | D-359 |
| 216 | Run-start connection attach (the leg deferred since D-287) | 2 | D-361 |
| 218 | Search user-axis isolation gate | 2 | D-363 |

Phase 212 shrank once its premise was checked — the recovery half it proposed building
is already shipped (`steering/runloop.go` converts every executor error into an
observation and continues), so only a machine-distinguishable classification remains.
Phase 213 grew: seven direct consumers, not the four its stale source comment claimed.

## Staging

**Stage 1 — parallel, no inter-dependencies:** 212, 213, 215, 217, plus the two CI
failures.

**Stage 2 — after Stage 1 merges:** 214 (needs 212's byte correctness — an egress arm
over a corrupting read path delivers corrupted bytes efficiently), 216, 218 (rebases on
213's constant split, same package).

**Generated-file ownership, to avoid an unresolvable conflict.** 214, 215 and 216 all
touch `web/console/src/lib/protocol/wire-manifest.gen.json` and
`docs/site/protocol/*.md`, which §13 and D-209/D-223 forbid hand-editing — a conflict
there cannot be hand-resolved, the loser must re-run the generators. **Phase 215 owns
both in Stage 1; 214 and 216 rebase on it.**

**Known file overlaps inside Stage 1**, to be sequenced at merge rather than discovered:
`internal/runtime/dispatch/dispatch_test.go` (212 adds classification tests, 213
retargets threshold assertions) and `examples/harbor.yaml` (213, 217).

**Stage 2 carries the wave-end E2E.** `test/integration/wave_v124_test.go` belongs in a
PHASE PLAN's file list — the first draft named it only here, and a coordination file is
outside §2's authority chain and unread by `scripts/drift-audit.sh`, which globs
`docs/plans/phase-*.md` only. It rides the last Stage-2 phase to merge.

**Stage 3 — the §17.5 checkpoint audit**, landing as one
`chore(checkpoint): wave-v124 audit fixes` PR. Gates v1.25 scoping.

## Track C — hygiene

Bug fixes rather than designs, riding Stage 1 as separate PRs:

1. **The two quarantined Linux CI flakes (#598, #599)**, both gated behind
   `HARBOR_RUN_QUARANTINED` and both declared mandatory for v1.24. Neither may be closed
   by raising a timeout or re-running until green. #599 carries a recorded DISPROVED
   theory so the next author does not re-derive it.
2. **The Console MCP-App byte path rider** — `ArtifactReferenceCard.svelte` is still
   presign-only, so chat previews are dead on a stock store.

## The failure mode this wave must not repeat

The first draft of these plans failed for one reason: **facts were sourced from godoc
comments, decision-log summaries and inference rather than from grep.** Everything that
was actually grepped held up; nearly everything reasoned from a comment was wrong. The
cleanest instance is `internal/config/config.go`'s own "ONE source" comment, which
enumerates three consumers where a grep finds seven — the draft copied the comment.

**Every dispatch prompt in this wave therefore carries: verify every factual claim by
grep and cite `file:line` in the plan.** This is the §17.8 principle — a source that
cannot distinguish right from wrong is not evidence — applied to plan authoring.

Carried from prior waves, because each has bitten before:

- Agents drifting out of their worktree into the main checkout. `pwd` first; STOP if a
  path resolves outside it.
- Latent `docs/decisions.md` markdownlint breakage surfacing one PR late (CI lints
  repo-wide). Blank lines around `---` and `## D-NNN`; run `markdownlint-cli2` repo-wide.
- Committed merge-conflict markers from an agent that ran `git merge main` mid-build.
- **Inert verification instruments.** The v1.23 wave found nine guards that could never
  fail; the first draft of phase 217 then specified another one. Every guard is
  MUTATION-VERIFIED: break the thing, watch the check go `OK` → `FAIL`, never
  `OK` → `SKIP`. A guard nobody has seen fail is not evidence.
- **Verify what executed, not just the exit code.** A green run that skipped everything
  looks identical to one that passed. Count PASS/SKIP before believing a gate.
