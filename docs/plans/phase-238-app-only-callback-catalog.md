# Phase 238 — App-only callback catalog (HA-56)

## Summary

Retain App-only callbacks at discovery while keeping them out of the planner projection. Dispatch them only through the matching Harbor host-derived server catalog and existing policy gates.

## RFC anchor

- RFC §6.4
- RFC §7.3
- RFC §5.2
- RFC §7

## Briefs informing this phase

- brief 03
- brief 14

## Brief findings incorporated

- brief 03 §1: source pluggability and provenance are uniform across transports.
- brief 14 §6: App content is sandboxed and bridge-proxied, not a runtime authorization shortcut.
- brief 14 §3: advertised capability must match serviced behavior.

## Findings I'm departing from (if any)

- None.

## Goals

- Implement D-412's per-server callback catalog and atomic dual projection.
- Preserve identity, agent reach, capability, OAuth/approval, current-state, redaction, and audit gates.

## Non-goals

- No provider exceptions, generic planner exposure, string-only authorization, or progressive streaming.

## Acceptance criteria

- [ ] Real SDK/spec-derived fixture publishes ordinary plus `_meta.ui.visibility:["app"]`; only ordinary is planner-visible and matching App callback succeeds.
- [ ] Cross-server, generic planner, missing/mismatched server identity fail before invocation with zero callback executions.
- [ ] Attach/reconnect/refresh/replacement/detach update both views atomically for HTTP and stdio; no stale entry.
- [ ] Paused/disabled/scope-filtered sources cannot bypass filtering; OAuth/approval gates still execute.
- [ ] Mixed identity/agent-reach concurrent tests prove no cross-talk.
- [ ] Compatibility transcripts cover supported metadata variants and rendered App behavior.

## Governance amendment — corrected fresh render-admission contract (Shipped v1.28)

> This section is the authoritative correction to the render-admission half of
> the shipped contract. It is a governance amendment only: **HA-56 stays phase
> 238 / D-412 — no new HA, phase, or decision is allocated**, and the original
> shipped app-only callback catalog history above and in D-412 is preserved
> verbatim. The amendment is recorded in D-412, RFC §6.10 (the callback and
> tool-context contracts section), the master-plan detail block and index row,
> the HA-56 register entry, the glossary, this plan, the operator skill, and
> the config/example docs. The amendment's implementation ships in the v1.28
> release wave: the sealed render-admission authority core
> (`internal/mcpconsole/admission`), the wire surface
> (`request_render_admission` opt-in on `mcp.servers.read_resource`, the
> distinct `render_admission` field on `mcp.apps.call_tool`, the typed
> unavailable/invalid/expired/mismatch codes), the current provider/catalog
> generation binding, the serve-band readiness-loud composition
> (`tools.mcp_app_render_admission.enabled` + `tools.oauth_token_kek_env`),
> and the real Console embedded/reopened consumer (the distinct
> admission-aware AppsAccessor path).

**The corrected contract.**

1. **Live provider-local binding is a bounded, short-lived compatibility path
   for a LIVE tool-result App only.** It is never durable and is never
   restored on reopen.
2. **Embedded/durable reopen uses a FRESH stateless, integrity-protected,
   shared-KEK admission** minted only after ALL of: verified identity, signed
   effective-agent reach, retirement, erasure, current session/agent exposure,
   exact server, exact current `ui://` resource availability, paused/disabled
   state, and the exact CURRENT provider/catalog generation — a deterministic,
   replica-stable generation of the server's current discovered catalog that
   changes on detach, replacement, and ANY successful discovery/catalog/
   resource change even when deployment descriptor configuration did not
   change (the existing exact registration descriptor fingerprint remains a
   retained input but is never alone sufficient authority; a process-local
   discovery counter is not acceptable, and a replica holding a different
   current catalog fails closed as a generation mismatch).
3. **The admission claim binds** claim schema, mint time, `(tenant, user,
   session)`, effective agent, server, resource, and the current
   provider/catalog generation — and carries NO raw args, secrets, provider
   output, callback name, or general capability.
4. **Ordinary resource reads never mint.** Only the explicit
   admission-requesting read path mints an admission.
5. **The callback stays absent from planner / `tools.list` / search / generic
   resolution** and dispatches via the same-server `ResolveAppTool` followed
   by the existing approval/OAuth/policy/redaction/retry/audit gates.
6. **Durable turn rows (HA-64 / D-425) retain metadata/component availability
   only — no token**; `mcp.apps.tool_context` replay remains unchanged and
   never reruns the originating tool.
7. **Typed unavailable/expired behavior is explicit**; refresh requires fresh
   checks — nothing is silently extended.
8. **Production and devstack share ONE implementation and ONE immutable
   shared sealer instance**; the surface is enabled by
   `tools.mcp_app_render_admission.enabled` (default `false`) and seals with
   the existing `tools.oauth_token_kek_env` KEK-backed AES-256-GCM sealer —
   no second authority field and no literal key. An enabled surface with an
   empty env name, a missing/unset/invalid KEK, or a sealer construction
   failure fails readiness loud, even when no OAuth provider or credential
   broker is configured; restart and multi-replica admission verification
   succeeds only when the shared KEK AND the same current provider/catalog
   generation are present — a replica holding a different current catalog
   fails closed as a generation mismatch, and a process-local discovery
   counter is not acceptable (the generation must be deterministic and
   stable across replicas holding the same current catalog).
9. **Pinned non-goals:** no generic capability framework, no persisted
   callback authority, no arbitrary origins, no provider exceptions, no hot
   registry, and no transcript impersonation.

**Amended acceptance (binding for the v1.28 implementation wave).**

1. **Durable reopen through a real Console consumer:** reopen a session and
   remount an App through the fresh admission path using the real Console
   App host/bridge, not a unit-level stand-in; the live provider-local
   binding path is separately exercised for a live tool-result App and proven
   bounded (expires with the live App) and never restored. The exact reopen order is binding:
   the durable App reference from the reopened session's turn rows, a
   successful `mcp.apps.tool_context` replay (a failed / unavailable /
   evicted / foreign replay mints no authority), the current `ui://` read
   explicitly requesting one fresh admission
   (`request_render_admission: true` — the only minting read; ordinary and
   AppBridge-secondary resource reads never mint), the iframe/AppBridge
   mount, and then same-server app-only callback dispatch through the
   existing wrapped invocation (the distinct admission-aware AppsAccessor path)
   echoing the fresh admission as the distinct `render_admission`
   authority. The fresh admission is distinct from, never aliases, and never
   coexists with the legacy live binding; neither is persisted or restored.
2. **Negative admission cases each fail typed before render or dispatch, with
   zero callback executions:** wrong/missing identity, retired effective
   agent, erased session/agent, changed session/agent exposure, wrong server,
   missing current `ui://` resource, paused/disabled server, stale
   provider/catalog generation (changed by detach, replacement, or any
   successful discovery/catalog/resource change even with unchanged
   deployment descriptor configuration), tampered claim, and expired claim.
3. **Claim-content pin:** the admission claim carries schema/time/triple/
   effective-agent/server/resource/current provider/catalog generation only; a
   test asserts raw args, secrets, provider output, callback name, and
   general-capability content are absent from the claim.
4. **Ordinary `mcp.servers.read_resource` never mints;** only the explicit
   admission-requesting read path does — proven by a test that ordinary reads
   yield no admission.
5. **Callback dispatch stays on the same-server `ResolveAppTool` path** with
   the existing approval/OAuth/policy/redaction/retry/audit gates; planner /
   `tools.list` / search / generic resolution never see the callback.
6. **Durable turn rows (HA-64 / D-425) carry metadata/component availability
   only** — a test asserts no admission token is persisted in the turn rows;
   `mcp.apps.tool_context` replay is unchanged and never reruns the
   originating tool.
7. **Typed unavailable/expired:** an unavailable or expired admission answers
   typed `unavailable`/`expired`; refresh re-runs the full fresh check list.
8. **One implementation:** production and devstack resolve the same
   implementation and one immutable shared sealer instance; the surface is
   STRICTLY opt-in (sealer availability alone never enables it, even when an
   OAuth broker already supplies the shared KEK) and every mint/verify reads
   the reach-admitted effective agent stamped in the request context, never
   a fixed boot/default fallback; it is
   enabled by `tools.mcp_app_render_admission.enabled` (default `false`) and
   seals with the existing `tools.oauth_token_kek_env` KEK — no second
   authority field; an enabled surface with an empty env name, a
   missing/unset/invalid KEK, or a sealer construction failure fails
   readiness loud even with no OAuth provider or credential broker
   configured; restart and multi-replica admission verification succeeds
   only when the shared KEK AND the same current provider/catalog generation
   are present — a replica holding a different current catalog fails closed
   as a generation mismatch.
9. **N≥100 concurrent reopen/isolation under `-race`** with no cross-talk,
   and zero originating-tool rerun / callback execution on refusal across all
   negative cases.
10. **Generation determinism:** the provider/catalog generation is
    deterministic and stable across replicas holding the same current
    catalog — a process-local discovery counter is rejected as a generation
    source; a replica holding a different current catalog fails closed as a
    generation mismatch before render or dispatch, even when the deployment
    descriptor configuration is identical.

## Files added or changed

- `internal/tools/{tools.go,catalog.go,planner_view.go}`
- `internal/tools/drivers/mcp/{content.go,registry.go,fixtures_test.go}`
- `internal/mcpconsole/{apps.go,catalog.go,*_test.go}`
- `internal/protocol/{apps.go,types,mcp.go}` if additive identity is needed
- `test/integration/mcp_app_callback_catalog_test.go`
- `docs/glossary.md`, `RFC-001-Harbor.md`, `CHANGELOG.md`, `scripts/smoke/phase-238.sh`

**Governance-amendment (v1.28) implementation surfaces.** The amendment was
recorded on the governance surfaces first — the D-412
decision entry, RFC §6.10, the master-plan detail block + index row
(`docs/plans/README.md`), the HA-56 register entry
(`docs/notes/downstream-asks.md`), this plan, the glossary, the focused
`scripts/smoke/phase-238.sh`, the `drive-the-playground` operator skill, and
the config/example docs (`docs/CONFIG.md`, `examples/harbor.yaml`) — and the
implementation landed in the same v1.28 wave: the sealed render-admission
authority core, the additive wire surface (minted only by the explicit
admission-requesting read), the current provider/catalog generation binding,
the serve-band readiness-loud composition, and the real Console
embedded/reopened consumer. HA-56 stays phase 238 / D-412 with no new phase,
decision, or HA.

## Public API surface

- Internal per-server App dispatch interface; any wire change is additive and host-derived.
- Existing planner catalog remains callback-free; Protocol clients never access internal descriptors.

## Test plan

- **Unit:** metadata parsing, dual projection, typed not-found/denial, refresh replacement, gate ordering.
- **Integration:** real MCP SDK fixtures and rendered App bridge over HTTP/stdio; identity and forced paused/denied failure.
- **Conformance:** provider transcript matrix for all named compatibility fixtures.
- **Concurrency / leak:** N=128 shared catalog refresh/dispatch calls with cancellation and teardown leak assertions.
- **Amendment (v1.28):** durable reopen through the real Console App host/bridge, the negative admission matrix (identity / server / resource / catalog-generation / expiry / tamper / erasure / paused-disabled), the claim-content pin, the ordinary-read-never-mints pin, the HA-64 row metadata-only pin, and N≥100 concurrent reopen/isolation under `-race` with zero originating-tool rerun / callback execution on refusal.

## Smoke script additions

- Static checks for visibility parsing, separate catalogs, host-derived identity, both transports, compatibility fixture names, and no planner callback exposure.
- **Amendment (v1.28):** static checks pinning the fresh render-admission contract across the governance surfaces — the plan's durable-reopen / negative-matrix / N≥100 / zero-rerun acceptance, the D-412 amendment record, the RFC §6.10 amendment, the master-plan and register statuses, the glossary term, the provider/catalog-generation determinism and fail-closed-mismatch pins, and the config/example readiness-loud consequence.

## Coverage target

- `internal/tools/drivers/mcp`: 90%; `internal/mcpconsole`: 90%; protocol adapter: 85%; integration: 80%.

## Dependencies

- Depends on Phases 207, 204, 109k, and 109l; independent of Phases 236, 237, 239, 240, 241, and 242.

## Risks / open questions

- Any additive wire identity must be mirrored by hand-maintained TS types and generated manifests/docs; otherwise keep it internal.
- Compatibility transcripts must be captured from real SDK/provider output, not self-consistent fixtures.

## Validation gate ledger

- **Local skip:** live provider compatibility probes may skip without explicit live-MCP configuration; committed real transcripts and local transport tests are required.
- **Web CI:** Console/App bridge checks and Protocol lockstep are mandatory when wire or Console files change; Go race/conformance is mandatory.

## Glossary additions

- **App-only callback catalog** — Harbor's per-server callback lookup retained beside, but never merged into, the planner projection.
- **Fresh render admission** — the stateless, integrity-protected, shared-KEK admission minted for an MCP App on embedded/durable reopen; never a restored live binding, never a persisted token, never minted by ordinary resource reads, and bound to the current provider/catalog generation (deterministic and replica-stable; a process-local discovery counter is never the generation source). D-412.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] Cross-references resolve
- [ ] Coverage target met
- [ ] Cross-session isolation passes
- [ ] Concurrent-reuse N≥100 under `-race`
- [ ] Real-driver/spec-derived integration with failure mode under `-race`
- [ ] Glossary updated
- [ ] Amendment: the fresh render-admission contract — including the current provider/catalog-generation binding with its deterministic, replica-stable, fail-closed-mismatch semantics — is recorded on every governance surface (D-412, RFC §6.10, master plan, register, glossary, smoke, skill, config docs) and its implementation (sealed authority core, additive wire, generation binding, readiness-loud composition, real Console consumer) ships in the v1.28 wave
