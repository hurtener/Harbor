# Wave v1.24 — coordination

The v1.24 wave has two delivery tracks and one hygiene track. This file is the §17.7
staging record: what is in the wave, which phases can be built in parallel, and what
gates the next stage.

## Scope

**Track A — the artifact byte path.** v1.23 shipped pass-by-reference routing's
in-process arm and an artifact read side. Two things were then found: the agent-facing
read path silently corrupts binary content, and the only transport that can receive
bytes by reference is the one that runs inside the Runtime. A PDF in the artifact store
cannot reach an agent intact, and cannot reach an MCP server at all.

**Track B — agent selection.** The agent-config registry, the four-tier prompt
composition, the per-user durable tier and the owner-scoped reconcile legs are all built
and all agent-parameterised. They are wired to a single constant, so a caller can create
agent configs that can never run.

**Track C — hygiene.** Carried from the v1.23 checkpoint and the release gate. Tracked
as issues rather than phase plans; each is a bug fix, not a design.

| Phase | Title | Track | Deps |
|---|---|---|---|
| 212 | Artifact read-path byte correctness + loud reference-resolution failure | A | 210, 209 |
| 213 | Heavy-content threshold rebalance + search-preview split | A | 210, 209 |
| 214 | The MCP arm of pass-by-reference routing | A | 212, 210, 209, 206 |
| 215 | Caller-named agent selection | B | 206, 211 |
| 216 | User-scope MCP connections | B | 215, 206, 211 |
| 217 | `meta_annotations` honour `_meta` path nesting | — | 206, 211 |

Pre-assigned decision numbers, so parallel agents do not collide in `docs/decisions.md`
(§17.7 step 3): **212 → D-357, 213 → D-358, 214 → D-359, 215 → D-360, 216 → D-361,
217 → D-362.**

## Staging

**Stage 1 — four phases in parallel.** No inter-dependencies; every `Deps` entry names
an already-shipped phase.

- 212 (artifact read path)
- 213 (thresholds)
- 215 (agent selection)
- 217 (annotation nesting)

**Stage 2 — two phases, after Stage 1 merges.**

- 214 (MCP egress) — needs 212. Shipping an egress arm over a read path that corrupts
  binary would deliver corrupted bytes very efficiently.
- 216 (user-scope connections) — needs 215. A per-user connection set is only meaningful
  once a user's runs can resolve against a chosen agent; without it the tier composes
  into one hardcoded agent and the feature is untestable at its own boundary.

**Stage 2 carries the wave-end E2E** (§17.7 step 5): `test/integration/wave_v124_test.go`,
bundled with the final phase's PR. Real drivers across the wave's surface — an artifact
stored, read back intact by an agent, substituted into an MCP call as bytes, under a
caller-named agent, with a user-scope connection in the catalog. Identity propagation on
every leg, ≥1 failure mode per leg, N≥10 concurrency stress.

**Stage 3 — the §17.5 checkpoint audit.** Read-only fork over every phase in the wave;
the punch list lands as one `chore(checkpoint): wave-v124 audit fixes` PR. This gates
v1.25 scoping.

## The §13 primitive-with-consumer check

Applied per stage, since this is the rule most easily violated by parallel staging:

- **212** introduces no primitive; it corrects two existing paths. Its own tests are the
  consumer.
- **213** changes a constant. No primitive.
- **214** introduces the egress encoder AND its consumer (the MCP driver's outbound
  call path) in the same phase. The HTTP and A2A transports are explicitly named as
  remaining, so the seam is visibly partial rather than silently so.
- **215** introduces the wire field AND its consumer (the run loop's projection
  resolution) in the same phase.
- **216** introduces the user-scope connection descriptor AND its consumer (the three
  reconcile legs) in the same phase.
- **217** introduces no primitive; it reconciles two existing mechanisms.

No phase in this wave ships a primitive without a consumer.

## Track C — hygiene, tracked as issues not plans

These are in the wave's scope but are bug fixes rather than designs, so they do not get
phase plans. They ride Stage 1 as separate PRs.

1. **The two quarantined Linux CI flakes (#598, #599).** Both were quarantined behind
   `HARBOR_RUN_QUARANTINED` to unblock the v1.23 release gate and both were declared
   **mandatory for v1.24**. Neither may be closed by raising a timeout or re-running
   until green. #599 carries a recorded DISPROVED theory (a test-server
   `select { case <-release; case <-r.Context().Done() }` race that was forced and did
   not reproduce) so the next author does not re-derive it.
2. **`internal/search` has no user-axis gate.** `search.go:268` / `:286` gate tenant
   only; the four searchers (`artifacts/index.go`, `events/index.go`,
   `sessions/index.go`, `tasks/index.go`) pass `UserIDs` through unexamined. §6 rule 2
   makes this a security bug rather than a style nit.
3. **The Console MCP-App byte path rider.** `ArtifactReferenceCard.svelte:39` is still
   presign-only, so chat previews are dead on a stock store — the D-347 Consumer 1
   riders that #594 only partially closed.

## Recurring failure modes to pre-empt in dispatch prompts

Carried from prior waves (§17.7 step 3), because each has bitten this project before:

- Agents drifting out of their worktree into the main checkout. Every dispatch prompt
  says `pwd` first and STOP if a path resolves outside the worktree.
- Latent `docs/decisions.md` markdownlint breakage surfacing one PR late (CI lints
  repo-wide). Blank lines around `---` and `## D-NNN` headings; run `markdownlint-cli2`
  repo-wide before committing.
- Committed merge-conflict markers from an agent that ran `git merge main` mid-build.
- **Inert verification instruments.** The v1.23 wave found nine guards that could never
  fail, plus a `\t`-in-`grep -E` break that could not match on Linux and a smoke SKIP
  that should have been an OK. Every guard in this wave must be MUTATION-VERIFIED:
  break the thing, watch the check go from `OK` to `FAIL` — never to `SKIP`. A guard
  nobody has seen fail is not evidence.
- **Verify what executed, not just the exit code.** A green run that skipped everything
  looks identical to one that passed. Count PASS/SKIP before believing a gate.

## Open questions the wave must answer, not discover

Each is named in its phase plan as an acceptance criterion or explicit open question,
listed here so the coordinator can check them off:

- **214:** whether the artifact-param mapping should target MCP's native typed content
  blocks (`blob` / `image` / `audio`) rather than named string parameters.
- **215:** whether a Protocol method exposes `AgentRegistry.ListTenant` for the Console
  selector. §13 forbids a Console page shipping without its feeding Protocol surface,
  so if none exists it lands here or immediately before the Console work — never after.
- **215:** whether the dev default agent is a registered entity, and therefore whether
  edge validation must exempt the default path.
- **216:** the bare-name collision resolution, justified against D-287.
- **216:** whether the owner axis gains a user component, touching every owner-scoped
  call site from phases 206 and 211.
- **217:** the §10 backward-compatibility survey — whether any in-tree caller populates
  `MetaAnnotations` with a dotted key.
