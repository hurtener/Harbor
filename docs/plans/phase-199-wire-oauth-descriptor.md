# Phase 199 — Wire-carried OAuth-provider descriptor, dev-gated (HA-32)

## Summary

`agent_config.set_oauth_provider` and the `oauth_provider` binding on `agent_config.add_mcp_connection` may carry a FULL provider descriptor over the wire (`token_url`, `audience`, `scopes`, `remote{}`) so a coordinator can stand up a NEW OAuth-fronted MCP server at runtime without a static `tools.oauth_providers[]` block and a redeploy — but ONLY behind a fail-closed, boot-only opt-in (`tools.allow_wire_oauth_descriptor` OR `HARBOR_ALLOW_WIRE_OAUTH_DESCRIPTOR`, default off). With the opt-in off (all of production), a wire descriptor carrying any credential-sink field is rejected exactly as the name-only binding (D-303) rejects it today. When opted in, the relaxation stays honest: `allowed_downstream_hosts` is DERIVED from the connected server's URL (never a free-form wire field), and the wire `token_url` is dialed through the identical tokenexchange SSRF backstop (D-300/D-338).

## RFC anchor

- RFC §6.4

## Briefs informing this phase

- brief 09
- brief 03

## Brief findings incorporated

- brief 09 §"MCP OAuth — lessons from bifrost": the credential SINK (token endpoint, downstream host) is the security-critical surface — a runtime-declarable provider must pin the downstream sink to the server actually being connected, never a caller-supplied host list; this phase DERIVES `allowed_downstream_hosts` from `connection.url` and rejects any wire-supplied downstream-host list.
- brief 09 §"broker custody": the runtime's own broker credential must never transit the wire — the wire descriptor names a boot-declared `credential_broker`; no `client_secret`/broker secret is ever a wire field.
- brief 03 §"static auth + retry": a token endpoint is a credential-bearing dial that must be hardened (no redirect, no proxy, SSRF backstop) identically whether declared at boot or over the wire — the wire `token_url` reuses the boot path's `hardenTokenExchangeClient` unchanged.

## Findings I'm departing from (if any)

None. This phase revisits D-303 (the zero-URL name-only default) but does NOT depart from it — the name-only binding remains the default and the only production-safe shape; the wire descriptor is a fail-closed gated extension (D-340), so the departure is authorized by the new decision, not a silent one.

## Goals

- With the opt-in OFF (default): a wire descriptor carrying ANY sink field (`token_url`/`audience`/`remote`/downstream-host list) is rejected with a clear error — the D-303 posture is byte-for-byte unchanged.
- With the opt-in ON: `set_oauth_provider` / `add_mcp_connection` install a provider from the wire descriptor at runtime; `allowed_downstream_hosts` is DERIVED as `NormalizeDownstreamHost(connection.url)`; a wire-supplied downstream-host list is rejected.
- The wire `token_url` faces the identical SSRF backstop as the boot path (refuse resolved private/link-local/ULA/unspecified, no redirect, no proxy, loopback carve-out).
- The boot opt-in (config OR env) is fail-closed, boot-captured, and prints the `[DEV-ONLY WIRE OAUTH DESCRIPTOR — DO NOT USE IN PRODUCTION]` stderr banner when the env fires.

## Non-goals

- Carrying the broker secret (`client_id`/`client_secret`/broker auth token) over the wire — never; the descriptor names a boot-declared `credential_broker`.
- Relaxing the private-dial refusal for a wire `token_url` — that remains gated INDEPENDENTLY by D-338's opt-in; a wire token_url still faces the default private-dial refusal unless D-338 is also opted in.
- HA-34's injection mapping (phase 200) — even though the wire descriptor is its natural carrier, the mapping field lands in phase 200.
- Bumping `ProtocolVersion` — the new fields are additive `omitempty`, so `0.1.0` stands.

## Acceptance criteria

- [ ] `AgentConfigOAuthProviderDescriptor` (and the `add_mcp_connection` inline-oauth path) gains `token_url` / `audience` / `remote{}` wire fields, all `omitempty`; `scopes` already exists.
- [ ] Opt-in OFF → a descriptor carrying any sink field is rejected (fail-loud, names the field + the opt-in key); the name-only path is unchanged.
- [ ] Opt-in ON → the provider installs; its `allowed_downstream_hosts` equals `NormalizeDownstreamHost(connection.url)`; a wire-supplied downstream-host list is rejected even when opted in.
- [ ] A wire `token_url` that resolves to a private/link-local/ULA/unspecified address, or that redirects, is refused by the SSRF backstop (opt-in does NOT relax D-300/D-338).
- [ ] `tools.allow_wire_oauth_descriptor` (config, default false) + `HARBOR_ALLOW_WIRE_OAUTH_DESCRIPTOR` (boot env) feed one effective posture (OR); env fire prints the banner at the dev/console/serve boot sites.
- [ ] Lockstep: `singlesource` + Console typed client + `wire-manifest.gen.json` mirror the new fields (`make protocol-ts-gen-check` green); generated protocol docs regenerated (`make protocol-docs-gen-check` green).
- [ ] `scripts/smoke/phase-199.sh` asserts the exfil guard: opt-in-off rejects a sink-bearing descriptor (400); models on the phase-169 shape.

## Files added or changed

```text
internal/protocol/types/agentconfig.go                 # additive omitempty wire fields on the OAuth-provider descriptor (+ add_mcp_connection inline-oauth mirror)
internal/protocol/singlesource/singlesource.go         # (types already registered) — no new map entry; field-level mirror only
internal/runtime/agentcfg/protocol/setoauthprovider.go # gate: opt-in off → reject sink fields; on → build wire provider
internal/runtime/agentcfg/protocol/addconnection.go    # inline-oauth wire path → derive allowed_downstream_hosts from connection.url; reject wire host list
internal/tools/auth/providers.go                       # install an OAuthProvider from a wire descriptor into the owner-scoped ProviderSet (reuses tokenexchange construction)
internal/config/config.go                              # tools.allow_wire_oauth_descriptor (bool, default false)
internal/config/validate.go                            # validate the new flag (no cross-field constraints beyond bool)
cmd/harbor/...                                          # HARBOR_ALLOW_WIRE_OAUTH_DESCRIPTOR capture + banner at dev/console/serve boot sites
web/console/src/lib/protocol/agentconfig.ts (+siblings)# TS mirror of the new wire fields
web/console/src/lib/protocol/wire-manifest.gen.json    # regenerated (make protocol-ts-gen)
docs/site/protocol/{types,methods}.md                  # regenerated (make protocol-docs-gen)
examples/harbor.yaml                                   # documented tools.allow_wire_oauth_descriptor default-off
docs/skills/use-the-harbor-protocol/SKILL.md           # note the gated wire-descriptor path (surface: protocol)
internal/.../*_test.go                                 # gate on/off, derive, SSRF-refuse, banner, lockstep
scripts/smoke/phase-199.sh                             # NEW — exfil guard (opt-in-off rejects sink fields)
docs/plans/phase-199-wire-oauth-descriptor.md          # this plan
docs/glossary.md                                       # "Wire-carried OAuth-provider descriptor"
```

## Public API surface

- Wire types (additive, `omitempty`): `AgentConfigOAuthProviderDescriptor.{TokenURL, Audience, Remote}` and the mirror on the `add_mcp_connection` inline-oauth descriptor.
- Config: `config.ToolsConfig.AllowWireOAuthDescriptor bool`.
- `cmd/harbor`: `EnvAllowWireOAuthDescriptor` boot-capture (mirrors `EnvDevAllowPrivateExchange`).

## Test plan

- **Unit:** gate off rejects each sink field; gate on installs + derives `allowed_downstream_hosts` from `connection.url`; wire host-list rejected; config-OR-env effective flag; boot-capture atomic + banner.
- **Integration:** through `internal/runtime/agentcfg` → `internal/tools/auth` — opt-in-on `set_oauth_provider` installs a wire provider; a subsequent `add_mcp_connection` binding it derives the sink from the connection URL; SSRF backstop refuses a private/redirecting wire `token_url` end-to-end.
- **Conformance:** N/A — single provider driver (`tokenexchange`).
- **Concurrency / leak:** the owner-scoped ProviderSet install is internally synchronised — N≥100 concurrent installs/reads under `-race` (reuses/extends the existing ProviderSet reuse test).

## Smoke script additions

- `scripts/smoke/phase-199.sh` (`PREFLIGHT_REQUIRES: live-server`, modeled on `phase-169.sh`): with the opt-in OFF (default preflight boot), a `set_oauth_provider` / `add_mcp_connection` descriptor carrying `token_url` (or a downstream-host list) is REJECTED 400 (the exfil guard); a name-only descriptor is unaffected; assert both verbs are present in `wire-manifest.gen.json` + `methods.md`; SKIP cleanly on 404/405/501.

## Coverage target

- `internal/runtime/agentcfg`: ≥ 80% on the gate/derive paths. `internal/config`: the new flag validated.

## Dependencies

- Gate-0 (D-340). Builds on the shipped name-only provider install (D-303), the tokenexchange SSRF backstop (D-300/D-338), and the add-connection lifecycle (#375) — all on `dev-experimental`.

## Risks / open questions

- The wire `token_url` lets an OPTED-IN operator's admin-scoped caller name a public token endpoint at runtime. This is the deliberate, reviewed relaxation (AskUserQuestion: fail-closed boot opt-in) — bounded by the derived downstream allow-list (a stolen descriptor cannot redirect an exchanged token to an attacker host) and the SSRF backstop (cannot reach internal services / cannot replay via redirect). Default-off keeps production on the D-303 posture. No RFC §11 open question.
- §17.8: the wire round-trip fixture derives from a real add-connection / set-oauth-provider transcript, not a hand-authored shape.

## Glossary additions

- **Wire-carried OAuth-provider descriptor** — a full provider binding (`token_url`/`audience`/`scopes`/`remote`) carried over `set_oauth_provider` / `add_mcp_connection`, accepted only behind the fail-closed `allow_wire_oauth_descriptor` boot opt-in; the downstream allow-list is derived from the connected server URL. D-340.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: cross-session isolation test passes — N/A (provider install is admin-gated, owner-scoped, not an isolation-tuple path).
- [ ] **Concurrent-reuse test passes** — the owner-scoped ProviderSet is a reusable artifact; N≥100 concurrent install/read under `-race`. See §5 + D-025.
- [ ] **Integration test exists** — `agentcfg` → `auth` wire-install + SSRF-refuse end-to-end (Deps names shipped provider-install + tokenexchange phases).
- [ ] If Protocol types changed: `make protocol-ts-gen-check` + `make protocol-docs-gen-check` green; Console typed client mirrors the fields.
- [ ] If config schema changed: `examples/harbor.yaml` updated; backward compatible (new optional bool, default false).
- [ ] If new vocabulary: glossary updated
- [ ] If a brief finding was departed from: justified above + decisions.md entry filed — the D-303 revisit is authorized by D-340 (not a silent departure).
