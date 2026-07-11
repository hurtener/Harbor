# Phase 92p — Spec-faithful MCP OAuth discovery (401 → RFC 9728 → AS)

> Part of the **Runtime MCP tool-side OAuth** wave (`docs/plans/wave-mcp-oauth-decomposition.md`, §3 "92p"). PLANNING — parked; not yet implemented. Reserved decision: **D-246**.
>
> **Discovery reconciliation (added with the Phase 164 implementation PR, D-297).**
> The `401 → RFC 9728 → RFC 8414` DISCOVERY chain (the `WWW-Authenticate`
> `resource_metadata` step-up capture + the metadata walk) is **single-homed
> in Phase 164's mechanism** (`docs/plans/phase-164-mcp-oauth-discovery-surfacing.md`
> — shipped: the guardrailed chain walker in `internal/tools/auth/discovery.go`
> plus the driver's `WWW-Authenticate` capture). When 92p is unparked it MUST
> REUSE that walker's output (`auth.OAuthRequirement`) and add ONLY its
> flow-execution legs (config synthesis + RFC 7591 registration CALL + PKCE
> authorize + pause/resume park) — it must NOT grow a second discovery chain
> (§13 one-mechanism, N-consumers; the Phase 148 precedent). 164 is the
> *discovery-as-data* phase; 92p is a *runtime-flow* consumer of it.

## Summary

Removes the operator-supplied-OAuth-config requirement for the common case. When a runtime-added MCP server answers a dial with `401` + a `WWW-Authenticate` challenge, the runtime follows the MCP 2025-11-25 auth spec: RFC 9728 protected-resource-metadata fetch → authorization-server metadata discovery (RFC 8414, already in `Provider.resolveEndpoints`) → RFC 7591 dynamic registration (already in `Provider.ensureClient`) → PKCE authorize. The agent-config service synthesises the `OAuthConfig` from discovery and registers it through the runtime-config seam, then parks the consent via `InitiateFlow` — no operator-typed client/endpoint config required.

## RFC anchor

- RFC §6.4 — Tool catalog and transports (the MCP southbound driver + its OAuth edge; this phase adds the discovery probe that synthesises the agent-bound config).
- RFC §3.3 — the unified pause/resume primitive (the discovered flow parks on it via `InitiateFlow`, exactly as the operator-supplied path does).

## Briefs informing this phase

- brief 09
- brief 14

## Brief findings incorporated

- **brief 14 §item 9 ("OAuth for HTTP servers"):** Harbor's MCP HTTP auth is static `Headers` only today — "No RFC 9728, no RFC 8707." This phase closes the discovery half: a `401` triggers RFC 9728 protected-resource-metadata fetch, then authorization-server discovery, matching the spec's `WWW-Authenticate` 401 step-up.
- **brief 14 §item (85b row):** the canonical sequence is "RFC 9728 protected-resource-metadata discovery; `WWW-Authenticate` 401 step-up; RFC 8707 resource indicators; interactive flow via the unified pause/resume primitive." This phase implements that sequence for the runtime-add control-plane variant and converges on the same `InitiateFlow` parking — not a parallel auth dance.
- **brief 09 §"What bifrost provides":** discovery lazily populates `AuthorizeURL`/`TokenURL` from `ServerURL/.well-known/...`, and RFC 7591 dynamic registration means operators "don't have to hand-register a client app per server." Harbor's `Provider.resolveEndpoints` (RFC 8414) and `Provider.ensureClient` (RFC 7591) already implement both — this phase wires the upstream RFC 9728 step that feeds them, then reuses them wholesale.
- **brief 09 §"the dynamic-registration footguns":** registration can be rejected (server demands a pre-registered client, unsupported grant, bad redirect). This phase fails loud on each — no silent fallback to an unauthenticated dial.
- **brief 09 §PKCE:** PKCE is mandatory for public clients from day one; the synthesised config carries the verifier through the existing `flowRecord`, so the discovered path is PKCE-correct by construction.

## Findings I'm departing from (if any)

None.

## Goals

- On a typed `auth.ErrAuthRequired` carrying a `401` + `WWW-Authenticate` challenge (surfaced by 92l), fetch the RFC 9728 protected-resource metadata referenced by the challenge's `resource_metadata` parameter (or the well-known path).
- From the protected-resource metadata, resolve the authorization server and reuse the EXISTING `Provider.resolveEndpoints` (RFC 8414) + `Provider.ensureClient` (RFC 7591) — no new discovery/registration code in `auth`.
- Synthesise an `OAuthConfig` (binding scope = agent) from discovery and register it via the 92k runtime-config seam, then park consent via `InitiateFlow` — making operator-typed OAuth config optional for the common case.
- Cover the failure modes briefs 09/14 name (no protected-resource metadata, AS metadata absent, registration rejected, no PKCE support, redirect mismatch) — each fails loud with a typed error, never a silent unauthenticated dial.

## Non-goals

- The operator-supplied-OAuth-config path itself (that is 92m; it remains the fallback/override when discovery is impossible or an operator pre-resolves endpoints).
- The resume-completes-attach bridge (92n) and run-start reconciliation (92o) — this phase only synthesises + parks; the resume machinery is unchanged.
- The Console advisory + wave-end live E2E (92q).
- RFC 8707 resource-indicator hardening beyond what discovery needs, and exotic AS quirks (recorded if encountered, per wave-doc §7 risk 5).
- Any change to the `auth.Provider` discovery/registration internals — they exist and are reused as-is.

## Acceptance criteria

- [ ] A `401` + `WWW-Authenticate` challenge from a runtime-added MCP server triggers an RFC 9728 protected-resource-metadata fetch (from the challenge's `resource_metadata` link or the well-known path).
- [ ] The authorization server resolved from the protected-resource metadata is run through the EXISTING `Provider.resolveEndpoints` (RFC 8414) + `Provider.ensureClient` (RFC 7591) — no duplicate discovery/registration logic added.
- [ ] The synthesised `OAuthConfig` (binding scope = agent) is registered via the 92k seam and the flow parks via `InitiateFlow`, returning an authorize URL + pause token — operator-supplied OAuth config is NOT required for the discovered case.
- [ ] The conformance fixture DERIVES from the real MCP 2025-11-25 auth spec / a captured real-server transcript — never a hand-authored self-consistent blob (§17.8); an env-gated `HARBOR_LIVE_*` probe drives a real OAuth-capable MCP server where one is available.
- [ ] Failure modes fail loud with a typed error and a redacted lifecycle event (no protected-resource metadata; AS metadata absent; dynamic registration rejected; no PKCE support; redirect mismatch) — never a silent unauthenticated attach (§13).
- [ ] Secrets (any discovered/registered `client_secret`) flow to the provider only and are never persisted in the revision / diff / event (CLAUDE.md §7).

## Files added or changed

- `internal/tools/drivers/mcp/` — the `401` → `WWW-Authenticate` parse + the RFC 9728 protected-resource-metadata fetch that feeds the discovery, surfaced through the typed `ErrAuthRequired` channel 92l opened (the driver stays registry-unaware).
- `internal/tools/auth/` — a discovery entry point that, given the protected-resource metadata's authorization-server URL, drives the existing `resolveEndpoints` + `ensureClient` and returns a synthesised `OAuthConfig` (reuses, does not reimplement, the RFC 8414 / RFC 7591 code).
- `internal/runtime/agentcfg/protocol/addconnection.go` — synthesise-from-discovery → `RegisterConfig` (92k) → `InitiateFlow` when the add hits a discoverable `ErrAuthRequired` and no operator OAuth block was supplied.
- `internal/tools/auth/testdata/` + `internal/tools/drivers/mcp/testdata/` — the spec-derived / transcript fixture (§17.8).
- `scripts/smoke/phase-92p.sh`.

## Public API surface

```go
// Given a 401 WWW-Authenticate challenge, discover the protected-resource
// metadata (RFC 9728), resolve its authorization server, and synthesise an
// agent-bound OAuthConfig by reusing the existing endpoint resolution
// (RFC 8414) + dynamic registration (RFC 7591). Fails loud on every
// missing-metadata / rejected-registration / no-PKCE branch.
//   DiscoverOAuthConfig(ctx, challenge) (OAuthConfig, error)
```

## Test plan

- **Unit:** the `WWW-Authenticate` parse extracts the `resource_metadata` link; an RFC 9728 document resolves the AS; a synthesised `OAuthConfig` is well-formed (binding scope = agent, PKCE set); each failure branch (no protected-resource metadata, AS metadata absent, registration rejected, no PKCE, redirect mismatch) returns a typed error and emits no unauthenticated dial.
- **Integration:** `test/integration/mcp_oauth_discovery_test.go` — drive the real discovery path against a fixture/transcript MCP server that `401`s: discovery → `RegisterConfig` (92k) → `InitiateFlow` parks → authorize URL returned; identity propagates through the triple; ≥1 failure mode (registration rejected → loud, no attach); under `-race`. The §17.8 fixture derives from the spec / a captured transcript, plus an env-gated `HARBOR_LIVE_*` probe against a real server.
- **Conformance:** the discovery fixture is checked against the MCP 2025-11-25 auth spec artifact (field placement of `resource_metadata`, the well-known paths) — guards against a self-consistent-but-wrong fixture (§17.8).
- **Concurrency / leak:** N concurrent discovered-add flows against one shared `Provider` under `-race`; no cross-talk between flows, no goroutine leak after teardown (the Provider stays a compiled artifact, D-025).

## Smoke script additions

- `scripts/smoke/phase-92p.sh`: static / unit — assert the discovery entry point + the RFC 9728 protected-resource-metadata parse symbols exist; assert the spec-derived fixture is present (not a hand blob); run the `internal/tools/auth` + `internal/tools/drivers/mcp` discovery unit tests. Live discovery against a real server is the env-gated `HARBOR_LIVE_*` probe, skipped in CI.

## Coverage target

- `internal/tools/auth` (the discovery synthesis path): 85%
- `internal/tools/drivers/mcp` (the `401` → `WWW-Authenticate` → RFC 9728 parse): 85%

## Dependencies

- 92l (MCP transport agent-bound OAuth + typed `ErrAuthRequired` — the channel the `401`/`WWW-Authenticate` challenge surfaces through), 92m (`add_mcp_connection` OAuth config + `InitiateFlow` parking — discovery synthesises the config 92m otherwise takes from the operator).

## Risks / open questions

- **Scope creep into the full MCP auth spec (wave-doc §7 risk 5).** Discovery is bounded to the 2025-11-25 auth-spec happy path + the failure modes briefs 09/14 name; exotic AS quirks are out of scope and recorded if encountered.
- **Fixture fidelity (§17.8).** A protected-resource-metadata fixture that can't tell the spec's field placement from a wrong one is a rubber stamp — the fixture derives from the spec artifact / a captured transcript, and a `HARBOR_LIVE_*` probe targets a real server in dev.
- **Discovery vs operator override precedence.** When an operator supplies an OAuth block (92m) AND the server is discoverable, the operator block wins; discovery only fires when no block was supplied. Pinned by a unit test.

## Glossary additions

- **MCP OAuth discovery** — the spec-faithful runtime path that, on a `401` + `WWW-Authenticate` challenge from a runtime-added MCP server, fetches RFC 9728 protected-resource metadata, resolves the authorization server, reuses the existing RFC 8414 endpoint resolution + RFC 7591 dynamic registration, and synthesises an agent-bound `OAuthConfig` — removing the operator-supplied-OAuth-config requirement for the common case.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: cross-session isolation test passes
- [ ] **Concurrent-reuse test passes (N≥100 concurrent discovered-add flows against one shared `Provider` under `-race`).**
- [ ] **Integration test exists, real MCP-server fixture/transcript (§17.8) + the 92k registry seam + bus, identity propagation, ≥1 failure mode (registration rejected → loud, no attach), `-race`.**
- [ ] If new vocabulary: glossary updated
- [ ] If a brief finding was departed from: justified above + decisions.md entry filed
