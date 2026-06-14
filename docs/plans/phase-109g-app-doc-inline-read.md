# Phase 109g — app-doc-inline-read

## Summary

A live test against a real `io.modelcontextprotocol/ui` ext-apps server (go-study-mcp) found MCP App documents fail to render on every non-S3 artifact driver. The 109 MCP Apps host gated a `ui://` App document on the LLM-context heavy-output threshold (32 KiB); go-study-mcp's studio App is ~86 KB, so `read_resource` offloaded it to the ArtifactStore and returned an `artifactRef` the Console can only fetch via a presigned URL — which fails loud on inmem/fs/sqlite/postgres. This phase re-scopes the heavy threshold OUT of `ui://` App documents: an App document is a Console-render payload, never LLM context, so it rides inline up to a dedicated, larger App-document cap (2 MiB) and renders on every driver.

## RFC anchor

- RFC §6.5
- RFC §7

## Briefs informing this phase

- brief 14
- brief 11

## Brief findings incorporated

- brief 14 (MCP client compliance): `ui://`-scheme resources are a distinct resource class declared via the `io.modelcontextprotocol/ui` extension and rendered in a sandboxed iframe by the Console. The App HTML is fetched ONLY by the host renderer — it never enters the agent's LLM context — so the LLM-context heavy-output net (RFC §6.5) is the wrong gate for it; an App document is a render payload, not a context payload.
- brief 14 §6: "MCP Apps … Render in a sandboxed iframe." — the document the iframe loads must reach the renderer; offloading a routine-sized App by reference (a presigned URL the common drivers cannot mint) leaves the iframe empty.
- brief 11 (Console is a pure Protocol client): the Console reads the App document through the `mcp.servers.read_resource` Protocol method and renders it; it never reads a Runtime internal. Returning the bytes inline keeps the rendering path on the typed Protocol surface with no S3-only escape hatch.
- brief 11 §"Playground": the inline renderer mounts on a `{resourceUri, serverID}` reference; the resource read must hand it real bytes on every supported store, not just S3.

## Findings I'm departing from (if any)

None. This phase narrows the *application* of D-026's heavy-output threshold (it never applied to a render-only payload in spirit); it does not weaken the LLM-context safety net, which keeps governing every byte that can reach the `LLMClient`.

## Goals

- A `ui://` MCP App document fetched via `mcp.servers.read_resource` rides INLINE up to a dedicated App-document cap (2 MiB), bypassing the 32 KiB LLM-context heavy threshold, so it renders on EVERY artifact driver.
- Above the App-document cap, the existing D-026 offload→artifactRef path is preserved (the loud `mcp.resource_offloaded` bypass), so a pathologically large App is never inlined unbounded and never silently truncated.
- An ordinary (non-`ui://`) resource read keeps the LLM-context heavy threshold unchanged.

## Non-goals

- No change to the LLM-edge safety net (`internal/llm/safety.go`) or the heavy-output threshold for any other producer.
- No change to the `CallTool` proxy's tool-result heavy-content discipline (a tool RESULT can reach the LLM; it keeps the heavy threshold).
- No Protocol wire-shape change — `ReadMCPResourceResponse.Content` already carries inline bytes; this phase only populates it for App documents under the cap.
- No new artifact driver capability (presigned URLs stay S3-only; the >cap fallback acceptably depends on them).

## Acceptance criteria

- [ ] `AppsAccessor.ReadResource` for a `ui://` resource whose content is above the 32 KiB heavy threshold but below the 2 MiB App-document cap returns the bytes INLINE (`Content`), against a real in-memory ArtifactStore, with NO `mcp.resource_offloaded` event.
- [ ] The below-cap App-doc test FAILS if the gate is reverted to the 32 KiB heavy threshold (an 86 KiB doc would offload to an artifactRef).
- [ ] A `ui://` resource above the 2 MiB App-document cap still offloads to an artifactRef and fires `mcp.resource_offloaded`.
- [ ] An ordinary (non-`ui://`) resource above the heavy threshold still offloads (the heavy threshold is unchanged for non-app content).
- [ ] The App-document cap is a named const with godoc explaining why it differs from the heavy-output threshold.
- [ ] The `AppsAccessor` stays immutable-after-construction; the concurrent-reuse test (N≥100) passes under `-race`.

## Files added or changed

- `internal/mcpconsole/apps.go` — `appDocumentInlineCap` const + `ReadResource` effective-cap branch (scoped to `ui://` via `mcp.IsUIResourceURI`).
- `internal/mcpconsole/apps_test.go` — below-cap inline test (revert-guard), above-cap offload test, repurposed ordinary-resource heavy-offload test; concurrent-reuse test retained.
- `internal/mcpconsole/apps_live_test.go` — `HARBOR_LIVE_MCP`-gated probe against the real go-study-mcp studio doc.
- `docs/plans/phase-109g-app-doc-inline-read.md`, `scripts/smoke/phase-109g.sh`, `docs/plans/README.md`, `docs/decisions.md` (D-218).

## Public API surface

No change. `protocol.MCPResourceReader.ReadResource` keeps its signature; `ReadMCPResourceResponse` keeps `Content` / `ArtifactRef` (exactly one set). The behavioural change is which branch a `ui://` document takes.

## Test plan

- **Unit:** below-cap `ui://` inline (real inmem store, no offload event — the revert-guard); above-cap `ui://` offload + event; ordinary-resource heavy offload + event.
- **Integration:** the in-package `apps_test.go` IS the seam (AppsAccessor ↔ real inmem ArtifactStore ↔ real inmem EventBus), identity-scoped, with the offload-event failure surface; the `HARBOR_LIVE_MCP` probe drives a real ext-apps server end-to-end.
- **Conformance:** N/A — no new driver.
- **Concurrency / leak:** `TestAppsAccessor_ConcurrentReuse` (N=128) under `-race`, retained.

## Smoke script additions

- `scripts/smoke/phase-109g.sh` probes `mcp.servers.read_resource`; SKIPs on 404/405/501 (the method may be unwired on a given build) and asserts the response carries the inline/by-reference shape when present.

## Coverage target

- `internal/mcpconsole`: maintain or improve the package baseline (53.4% → 55.0% with this change; the `ReadResource` branch under change is fully covered, and the gated live probe — skipped in CI — counts against the ratio). No regression; the touched code path is exercised by the below-cap / above-cap / ordinary-resource tests.

## Dependencies

- Phase 109a (the `mcp.servers.read_resource` surface + `AppsAccessor`). 109f (heavy-app-doc presigned fetch) is independent — it consumes the >cap fallback this phase preserves; this phase raises the boundary for `ui://` docs so the fallback is hit only for pathologically large apps.

## Risks / open questions

- The 2 MiB cap is a judgment bound. Real apps run 80–100 KiB; 2 MiB leaves ample headroom while still bounding an unbounded inline. Above it, the (S3-only) presigned fetch is the acceptable degradation for a pathological App.
- An operator-configured heavy threshold ABOVE 2 MiB would (for a `ui://` doc) make the App-doc cap the binding limit — intentional: the App-doc cap is the App-document ceiling regardless of the LLM-context threshold.

## Glossary additions

- App-document cap — the dedicated inline byte ceiling (2 MiB) for a `ui://` MCP App document, distinct from the LLM-context heavy-output threshold.

## Pre-merge checklist

- [x] `make drift-audit` passes
- [x] `make preflight` passes
- [x] `make check-mirror` passes
- [x] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [x] Coverage on touched packages ≥ stated target
- [x] If multi-isolation paths changed: cross-session isolation test passes (concurrent-reuse test uses distinct sessions)
- [x] **Reusable artifact:** `AppsAccessor` concurrent-reuse test passes — N=128 under `-race`.
- [x] **Consumes a shipped subsystem's surface:** in-package adapter test wires the real inmem ArtifactStore + EventBus, asserts identity propagation, covers the offload failure surface, runs under `-race`.
- [x] If new vocabulary: glossary updated (App-document cap)
- [x] If a brief finding was departed from: N/A — none departed.
