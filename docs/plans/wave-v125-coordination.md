# Wave v1.25 — coordination

The §17.7 staging record for v1.25. The wave is one coherent slice: **the
prompt-composition surface** — how a Protocol client contributes to what the model
sees — plus the hygiene phase that repairs the instrument measuring all of it.

## Where this wave came from

Two upstream asks (HA-45, HA-46) from a coordinator that composes prompt contributions
from several independent sources and found only whole-spine replacement. One of its
claims was refuted on review and the ask reshaped twice. The final scope reflects a
finding **neither side stated initially**:

> The defect is not "there is no additive path." It is that **the safe path is not
> reachable over the Protocol, so the surface steers consumers toward the unsafe one.**

`planner.MemoryBlocks` already carries recalled memory with UNTRUSTED
anti-prompt-injection framing, in a purpose-built tier. It is absent from the Protocol.
So a consumer needing to inject recalled conversation memory reached for
`SystemPromptOverride` — which replaces the whole base+user spine (silently suppressing
the operator's durable user layer) and lands user-authored content in the **trusted**
base position. Phase 219 exists to make the safe path reachable.

## Scope

| Phase | Title | Stage | D |
|---|---|---|---|
| 219 | Memory tiers on the Protocol run surface | 1 | D-364 |
| 221 | Expected-revision token on the agent-config writes | 1 | D-366 |
| 223 | Drain the inert-smoke baseline | 1 | D-368 |
| 220 | `extra_instructions` on `RunOverrides` | 2 | D-365 |
| 222 | `ExtraSystemBlocks` on the agent-config payload | 2 | D-367 |

## Staging — by file ownership

The pairs that share a file are split across stages deliberately:

- **219 and 220** both touch `internal/protocol/types/runs.go`.
- **221 and 222** both touch `internal/protocol/types/agentconfig.go`.

**Stage 1:** 219, 221, 223 — no shared files. **219 OWNS the generated wire manifest**
(`make protocol-ts-gen` / `protocol-docs-gen`); 221 rebases on it rather than
regenerating in parallel.

**Stage 2:** 220 (rebases on 219), 222 (rebases on 221). **Stage 2 carries the wave-end
E2E** (§17.7 step 5) in a PHASE PLAN's file list — not in this file, which is outside
§2's authority chain and unread by `scripts/drift-audit.sh`.

**Stage 3:** the §17.5 checkpoint audit, landing as one `chore(checkpoint)` PR. It gates
v1.26 scoping.

## Decisions already settled — do not re-litigate

1. **219 — caller-supplied, External tier only, one fixed runtime-owned map key.** The
   identity contract binds a STORE READ and is not engaged on this path: bytes arrive
   under the caller's own verified triple and reach the run minted for it. §6's boundary
   is "can A's data reach B's run"; content flows in, never out. `Conversation` stays
   runtime-only — `ProjectMemoryBlocks` writes it unconditionally, so a caller writing it
   is two producers on one slot.
2. **220 — the run-level value composes BELOW the tenant value and can never clear it.**
   `governance.set_tenant_overrides` is admin-gated; `runs.set_overrides` is not.
   Per-field last-writer-wins would hand a non-admin caller a silent delete on an
   admin-set compliance block — a privilege inversion.
3. **220 — no per-field scope gate.** `system_prompt_override` sits on the same struct,
   same caller, and is strictly more powerful; gating the weaker while the stronger stays
   open is incoherent. The METHOD's authorization tier is an open risk, named not hidden.
4. **221 — `expected_content_hash`, not revision id.** `rollback` moves the pointer
   without changing content, so a revision-id token raises a false conflict on Harbor's
   own recovery path.
5. **221 — the CAS guarantee is exact within one process and ABSENT across processes.**
   Atomicity comes from the service's striped write locks, not the store
   (`internal/state/state.go` says so verbatim). Stated in godoc, the reference, the
   skill, and a test that asserts the residual AS ABSENT.
6. **222 — `NormalizePayload` must NOT sort blocks.** Block order is render order, so a
   re-order must change the `ContentHash` and appear in the diff.
7. **A trusted position must never hold user-authored content.** 220's
   `<additional_guidance>` is verbatim and unescaped; recalled memory belongs in 219's
   untrusted-framed tiers. Binding non-goal on 220.

## What Gate-0 already found in shipped code

Four defects surfaced while verifying the plans. Each is bundled into the phase that
found it, per §17.6:

- `memory_fetch.go` claims `ErrContextLeak` backstops an oversized memory tier. **It
  never has** — `findContextLeak` sets `offloadableText = (Role == RoleTool)` and memory
  tiers render as `RoleSystem`.
- `maxOutputSchemaBytes` is **already unreachable dead code** — the control transport
  caps the whole body at the same 64 KiB with the same error code. Its unit tests pass
  only because they call `dispatchStart` directly, bypassing the real path.
- **`phase_is_shipped` misclassifies 106 of 334 master-plan rows** two independent ways,
  and 13 of the 24 baselined inert smokes are its false positives.
- `drift-audit.sh` writes markdownlint output to a fixed `/tmp` path, so two concurrent
  audits clobber each other (`make preflight` runs drift-audit internally).

## The failure mode this wave must not repeat

**Dispatch plan-authoring agents WITH `isolation: "worktree"`.** Gate-0's five agents were
dispatched into the shared checkout while the coordinator was switching branches in it
for the v1.24 release cut. One agent had its untracked files wiped mid-task and had to
recreate them and re-verify every citation. Nothing was ultimately lost; the waste was
avoidable. Implementation agents already get worktrees — plan agents need them too.

Carried from prior waves:

- `pwd` first; STOP if a path resolves outside the worktree. Never `git merge main`
  mid-build.
- `docs/decisions.md` is APPEND-ordered, not numerically sorted. Keep every entry on a
  conflict and verify by heading count that none was dropped. Re-run `markdownlint-cli2`
  on it afterwards — conflict resolution is what eats the required blank lines.
- **Mutation-verify every guard**: break it, watch `OK` → `FAIL`, never `OK` → `SKIP`.
  The v1.24 wave found roughly ten instruments that could not fail; this wave's Gate-0
  found four more before a line of code was written. Assume the instrument is broken
  until you have watched it fail.
- **Verify what executed, not the exit code.** A green run that skipped everything looks
  identical to one that passed.
