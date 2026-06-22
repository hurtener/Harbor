# Wave decomposition — Runtime MCP tool-side OAuth (completes 92f)

> **Status: PLANNING — parked.** This document and its child phase plans (92k–92q)
> are an approved, RFC-anchored plan that has **not** begun implementation. It was
> authored as a standalone planning deliverable so the work can be picked up later
> without re-deriving the design. No phase below is `Shipped`; the master-plan rows
> are `Pending`. See the umbrella decision **D-240**.

This wave closes the two unfinished halves of `agent_config.add_mcp_connection`
(issue [#375](https://github.com/hurtener/Harbor/issues/375)) the *right* way — by
giving a **runtime-added MCP server** a real **agent-bound OAuth** path that reuses
Harbor's existing tool-side OAuth primitive (`internal/tools/auth`), rather than the
narrower in-memory-credential stopgap #375 first imagined. The two #375 gaps —
(1) the `auth_required` resume is a dead-end (no continuation re-drives the attach),
and (2) persisted `Connection` descriptors are never reconciled at run-start — fall
out as two phases of the wave once the OAuth substrate is in place.

---

## 1. Why this is a wave, not a sub-phase

The 2026-06-22 wave-2 §17.5 checkpoint audit filed #375 as a "separable hard
sub-phase." Tracing the as-built code to the bottom showed the faithful fix is
materially larger than a single phase, because the credential the resume needs has
nowhere to come from today:

- **The OAuth `Provider`'s config set is immutable after construction**
  (`auth.NewProvider(configs, deps)`; `Provider.configs` is read-only). A
  *runtime-added* MCP server has no way to register an `OAuthConfig` — so the
  existing agent-bound dance (`InitiateFlow` → callback `CompleteFlow` →
  agent-bound token in the sealed `TokenStore`) cannot be reached for a server
  added over the Protocol.
- **The MCP driver does not consult `auth.OAuthProvider` at all.** Its transports
  (`transport_streamable.go` / `transport_sse.go`) take only *static* operator
  headers; there is no path that resolves an agent-bound token and injects
  `Authorization`. The runtime-add attacher detects `auth_required` with a **string
  heuristic** (`looksLikeAuthRequired`) because "the driver does not yet surface a
  typed auth error."
- **`add_mcp_connection` carries no OAuth configuration** (only `Connection` +
  static `Headers`), and the MCP OAuth discovery dance (401 →
  `WWW-Authenticate` → RFC 9728 protected-resource metadata → authorization-server
  discovery → dynamic registration → PKCE) is not built.

What *does* already exist and is reused wholesale: `auth.Provider` with `ScopeAgent`
binding, PKCE, RFC 7591 dynamic registration, RFC 8414 metadata discovery, the
sealed agent-bound `TokenStore`, `InitiateFlow`/`CompleteFlow`, the `auth.CallbackHandler`
mounted at `GET /v1/tools/oauth/callback`, and — crucially — `CompleteFlow` already
**resumes the parked pause via the unified Coordinator**. So the wave's job is to
*wire MCP-add into that machinery*, not to reinvent it. The unified pause/resume
primitive stays the ONE auth path (CLAUDE.md §7 rule 4 / §13 — no parallel pause).

---

## 2. End-state (what "done" looks like)

An admin adds an HTTP MCP server over the Protocol that requires authorization.
The runtime dials, the MCP transport asks `auth.OAuthProvider` for an agent-bound
token, finds none, and surfaces a **typed** `auth.ErrAuthRequired`. The agent-config
service registers the server's `OAuthConfig` (operator-supplied in 92m, or
discovered in 92p) and calls `InitiateFlow`, which parks on the unified
Coordinator and returns an authorize URL. The admin completes consent out-of-band;
`auth.CallbackHandler` calls `CompleteFlow`, which persists the **agent-bound**
token and **resumes the pause**. The resume bridge (92n) observes `pause.resumed`
and re-drives the attach — the transport now finds the token and the server comes
**online**. On a later Runtime restart or a config rollback that re-declares the
server, run-start reconciliation (92o) re-attaches it, reading the same agent-bound
token. "A resume completes the attach" (D-237) is true again, end-to-end, and
LIVE-verified against a real OAuth-capable MCP server (env-gated, CI-skipped —
the `HARBOR_LIVE_*` pattern, CLAUDE.md §17.8).

**Explicitly scoped OUT of this wave (D-240):** *detach-on-rollback* — tearing down
a live MCP transport when a rollback/revision no longer declares it. Revocation
remains the job of the pause/resume tool-exposure primitive (D-237: "pause/resume
covers the disable need"), which excludes a server from the next-run planner view
without a risky live-transport teardown. Run-start reconciliation attaches declared
servers (the minimum bar that makes "durable control of MCP connections" honest);
the removal direction is a recorded, deliberate deferral, not an oversight.

---

## 3. Phases (92k–92q)

| Phase | Title | Subsystem | Pre-assigned D | Deps |
|-------|-------|-----------|----------------|------|
| 92k | `auth.Provider` runtime config registration seam | tools/auth | D-241 | 30, 50 |
| 92l | MCP transport agent-bound OAuth + typed `ErrAuthRequired` | tools/mcp, tools/auth | D-242 | 28, 30, 92k |
| 92m | `add_mcp_connection` OAuth config + `InitiateFlow` parking | agentcfg/tools | D-243 | 92f, 92k, 92l |
| 92n | Resume-completes-attach bridge (closes #375 gap 1) | agentcfg/tools | D-244 | 92m, 50 |
| 92o | Run-start connection reconciliation (closes #375 gap 2) | agentcfg/projection | D-245 | 92m |
| 92p | Spec-faithful MCP OAuth discovery (401 → RFC 9728 → AS) | tools/mcp, tools/auth | D-246 | 92l, 92m |
| 92q | Console advisory + wave-end live E2E + §17.5 audit | web/console, integration | D-247 | 92k–92p |

Per CLAUDE.md §17.7 step 3, each `D-24N` is **reserved** here and **logged in
`docs/decisions.md` when that phase ships** — the umbrella **D-240** is the only
decision entry that lands with this planning PR.

### 92k — `auth.Provider` runtime config registration seam (D-241)

The `auth.Provider` gains a mutable-but-internally-synchronised config registry:
`RegisterConfig(cfg OAuthConfig) error` (validates + upserts by `Source`) and
`UnregisterConfig(source)` (idempotent). The `configs` map moves behind the existing
`flowsMu` (or a dedicated `cfgMu`) with a documented internally-synchronised
invariant, preserving the concurrent-reuse contract (D-025): the Provider stays a
compiled artifact; the registry is shared mutable state guarded by a mutex, exactly
like `coordinator.pauses`. Re-registering a `Source` with an in-flight flow is
defined (the in-flight flow keeps its captured config; the new config applies to the
next `Token`/`InitiateFlow`). **Consumer in-phase (§13):** the boot provider
construction migrates from the `NewProvider(configs, …)` static list to
constructing empty + `RegisterConfig`-ing each boot config, so the seam has a real
production caller on day one (not just a test). Concurrent-reuse test: N≥100
concurrent `RegisterConfig`/`Token` against one shared Provider under `-race`.

### 92l — MCP transport agent-bound OAuth + typed `ErrAuthRequired` (D-242)

The MCP driver learns to authenticate via the OAuth provider. `mcpdrv.Config` /
`AttachDeps` gains an optional `auth.OAuthProvider` + the source's binding info; the
streamable-HTTP and SSE transports, before dialing, call `provider.Token(ctx, source)`
and inject `Authorization: Bearer <token>` when one is returned. A `*auth.ErrAuthRequired`
from `Token` is surfaced **verbatim** (typed) out of `mcpdrv.Attach`, replacing the
`looksLikeAuthRequired` string heuristic at the runtime-add boundary with
`errors.As(err, &authErr)`. Static operator headers remain supported and take
precedence when present (back-compat). **Consumer in-phase:** a boot-time attach
against a fixture MCP server that 401s, asserting the typed error propagates and a
provider-supplied token lets the attach succeed. The §17.8 fixture is a *real* local
MCP server (or a committed transcript), never a hand-authored self-consistent JSON
blob.

### 92m — `add_mcp_connection` OAuth config + `InitiateFlow` parking (D-243)

`AgentConfigAddMCPConnectionRequest` gains an optional `OAuth` block (binding scope =
agent by construction; client_id / scopes / server_url or authorize+token URLs /
redirect_uri — the operator-supplied path; discovery in 92p makes most of it
optional). On add, the service `RegisterConfig`s the server's `OAuthConfig` via 92k,
drives the attach via 92l, and **replaces `parkForAuth`** with
`provider.InitiateFlow` on a typed `ErrAuthRequired` — so the pause is the
OAuth-flow pause (correlated to `CompleteFlow`), not a bare park nobody can complete.
The response returns the `authorize_url` + `pause_token`. Secrets (`client_secret`,
any header) flow to the provider/transport only and are **never** persisted in the
revision / diff / event (CLAUDE.md §7) — the non-secret descriptor invariant is
unchanged. Admin-scoped (D-235); `ScopeAgent` `InitiateFlow` requires the control
scope (the provider already enforces `ErrAdminScopeRequired`).

### 92n — Resume-completes-attach bridge — closes #375 gap 1 (D-244)

A long-lived agent-config component subscribes to the bus for `pause.resumed`
(`EventTypePauseResumed`). When a resumed token corresponds to an `mcp_oauth`
add-flow pause, it re-drives the attach (via 92l's token-aware path) under the
per-`(tenant, agentID)` `writeLocks` so a concurrent revise cannot race the resume.
On success the server comes `online` and a terminal `mcp.connection.added` lifecycle
event fires; on attach failure it **fails loud** (a loud `mcp.connection.failed` +
logged error; the server stays offline — never a silent re-park or a silent drop,
CLAUDE.md §13). The bridge is the established "react to a resume" pattern (mirrors
the steering RunLoop's `pause.resumed` wake + the approval gate). **This phase
re-instates the "a resume completes the attach" claim** the checkpoint PR downgraded:
D-237 §2, the `addconnection.go` + `methods.go` godoc, and the 92f plan are reverted
to the now-true wording. Restart caveat documented: a pause that outlived its
process is re-driven by 92o at the next run, not by the in-memory bridge.

### 92o — Run-start connection reconciliation — closes #375 gap 2 (D-245)

The run-start projection (`internal/runtime/agentcfg/projection`) gains a
`ReconcileConnections` helper called by BOTH run-loop drivers (the production
`cmd/harbor/cmd_dev_runloop.go` and the `harbortest/devstack` twin — D-094, so they
cannot drift). It reads the agent's active revision's `ConnectionDescriptors`,
and for each **declared-but-absent** source (not already live in the catalog/registry)
attaches it via the injected `ConnectionAttacher` — reading the agent-bound token, so
a server authorized in a prior process comes online on the next run. Idempotent
(already-attached sources are skipped) and serialised per agent so two concurrent
run-starts cannot double-attach. **Detach-removed is scoped OUT (D-240/D-245):** a
rollback past an add does not tear down the live transport; revocation is the
pause/resume tool-exposure primitive's job. The deferral is logged loud, with the
operator-facing note that pausing is the revoke path.

### 92p — Spec-faithful MCP OAuth discovery (D-246)

Removes the operator-supplied-OAuth-config requirement for the common case: on a
401 with a `WWW-Authenticate` challenge, the runtime follows the MCP 2025 auth spec
— RFC 9728 protected-resource metadata → authorization-server metadata discovery
(RFC 8414, already in `Provider.resolveEndpoints`) → RFC 7591 dynamic registration
(already in `Provider.ensureClient`) → PKCE authorize. The agent-config service then
synthesises the `OAuthConfig` from discovery and registers it via 92k. Brief 09 (MCP
OAuth lessons from bifrost) and brief 14 (MCP client/host compliance, spec
2025-11-25) are the conformance source; the §17.8 fixture derives from the real spec
/ a real server transcript.

### 92q — Console advisory + wave-end live E2E + §17.5 audit (D-247)

The Console renders the "awaiting authorization / paused by an administrator"
advisory for a connection in `auth_required` (extends the 92h panel; pure Protocol
client, D-061 — it never reaches into the flow). The wave-end deliverable bundles
`test/integration/wavemcpoauth_test.go` (real drivers across the wave's surface,
identity propagation, ≥1 failure mode — a denied flow via `DenyFlow` — and N≥10
concurrency stress) plus the env-gated `HARBOR_LIVE_*` probe against a real
OAuth-capable MCP server. The wave-end §17.5 checkpoint audit runs read-only and
lands its punch list as one `chore(checkpoint)` PR before any follow-on wave.

---

## 4. Staging (parallelisation map, §17.7 step 2)

```text
Stage 1 (parallel)   92k  ──┐   92l  ──┐        foundations: provider seam + transport OAuth
                            │          │
Stage 2              92m ◀──┴──────────┘         add-connection wires both
                       │
Stage 3 (parallel)   92n ◀─┤   92o ◀─┤           the #375 mechanism: resume bridge + reconcile
                            │         │
Stage 4 (parallel)   92p ◀──┴─────────┘   92q (wave-end, after 92n/92o/92p)
```

- **Stage 1** {92k, 92l} parallelise: 92l's in-phase consumer uses a *boot-config*
  OAuth source, so it does not block on 92k's runtime seam; 92k carries its own
  boot-path consumer. Each is a §13-complete primitive+consumer unit.
- **Stage 2** {92m} barriers on Stage 1 (it consumes both the seam and the transport
  path).
- **Stage 3** {92n, 92o} parallelise — both consume 92m, neither consumes the other.
- **Stage 4** {92p} parallelises after Stage 2; {92q} is the wave-end E2E + audit and
  lands last, bundled with the final stage PR (§17.5).

Each phase introduces a primitive **with** its consumer in the same phase
(CLAUDE.md §13): 92k's seam + boot caller; 92l's transport path + attach test;
92m's `InitiateFlow` wiring + the add verb; 92n's bridge + the resume regression
guard; 92o's reconcile + the run-start test; 92p's discovery + a live/transcript
probe.

---

## 5. RFC anchor & decisions

- **RFC update (this PR):** RFC §6.4 (tool transports) gains a "Runtime MCP tool-side
  OAuth" subsection; RFC §6.16 (Agent Registry) notes the agent-bound binding for
  runtime-added connections; the unified pause/resume framing (RFC §3.3) is the
  parking substrate. The new runtime OAuth surface (runtime-registered configs +
  MCP-transport OAuth) is an RFC-level addition, recorded as **D-240**.
- **D-240 (lands now):** the umbrella decision — runtime MCP tool-side OAuth via
  runtime-registered agent-bound `OAuthConfig`s + resume-completes-attach +
  run-start attach-reconciliation; detach-on-rollback deliberately deferred
  (pause/resume is the revoke path).
- **D-241..D-247 (reserved):** one per phase, logged on ship (§17.7 step 3).

## 6. Briefs informing this wave

- **brief 09** — MCP OAuth lessons from bifrost (the token lifecycle, refresh,
  agent-bound vs user-bound patterns; the dynamic-registration footguns).
- **brief 14** — MCP client/host compliance (spec 2025-11-25): the auth capability
  matrix, the 401 → protected-resource-metadata → AS-discovery flow, the conformance
  fixtures §17.8 demands.

## 7. Risks / open questions

1. **Mutating a security primitive (92k).** Adding a runtime config registry to the
   OAuth `Provider` widens its mutable surface. Mitigated by: mutex-guarded registry
   with a documented internally-synchronised invariant; a concurrent-reuse test; the
   wave-end adversarial review. An in-flight flow's captured config is immutable for
   that flow (re-registration applies next flow only).
2. **MCP transport token injection ordering (92l).** Static operator headers vs
   provider token precedence must be unambiguous (operator header wins when present;
   otherwise the agent-bound token). Pinned by a transport-level test.
3. **Restart-survival of an in-flight add-flow pause (92n/92o).** The in-memory
   resume bridge cannot re-drive a pause whose process died; 92o's run-start
   reconcile is the durable backstop (the descriptor revision + the agent-bound token
   both survive). Documented, not silently degraded.
4. **Live OAuth MCP fixture (92q).** A real OAuth-capable MCP server is required for
   §17.8 conformance. If none can be driven in CI, a captured transcript is committed
   and an env-gated `HARBOR_LIVE_*` probe targets a real server in dev.
5. **Scope creep into full MCP auth spec (92p).** Discovery is bounded to the 2025
   auth-spec happy path + the failure modes briefs 09/14 name; exotic AS quirks are
   out of scope and recorded if encountered.
