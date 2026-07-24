# Phase 203 — Wire-carried per-user credential INJECTION for dynamically-added receiver-style MCP servers (HA-37)

## Summary

The `agent_config.add_mcp_connection` connection descriptor gains an OPTIONAL `injection` object (`AgentConfigMCPCredentialInjectionDescriptor`) so a coordinator can ATTACH a RECEIVER-STYLE MCP server at runtime and wire per-user credential delivery to it — without a static `tools.mcp_servers[].injection` block and a redeploy. It NAMES a boot-declared `tools.oauth_providers[]` broker (the per-user credential is still PULLED per outbound call via the acting ctx identity) and declares WHERE the pulled value is placed (a header / `Authorization: Basic` / a `_meta` key). It is accepted ONLY behind a fail-closed, boot-only opt-in (`tools.allow_wire_injection` OR `HARBOR_ALLOW_WIRE_INJECTION`, default off) that is INDEPENDENT of the wire-OAuth opt-in (D-340). With the opt-in OFF (all of production) a connection carrying any injection field is REJECTED with a distinct typed error. This is the wire-plumbing sibling of the HA-34 injection engine (D-341), mirroring the HA-32 wire-OAuth-descriptor posture (D-340); the injection engine is reused unchanged.

## RFC anchor

- RFC §6.4

## Briefs informing this phase

- brief 09
- brief 03

## Brief findings incorporated

- brief 09 §"MCP OAuth — lessons from bifrost": the credential SINK is the security-critical surface — a runtime-declarable credential delivery must pin the downstream sink to the server actually being connected, never a caller-supplied host list; this phase DERIVES the reachable sink from `connection.url` and validates it against the named broker's boot-declared `allowed_downstream_hosts` at attach time. There is no host field on the wire injection descriptor.
- brief 09 §"broker custody": the runtime's own broker credential must never transit the wire — the wire injection descriptor NAMES a boot-declared broker; only the NON-SECRET mapping (broker name + target key/form) rides the wire, the value is broker-pulled per acting user at call time.
- brief 03 §"static auth + retry": a credential delivered on an outbound request is a secret that must never reach a log or an audit payload uncredacted — the wire-descriptor validation rejects a target key the audit redactor (D-341's extension) would not hold to `***`, using the SAME predicate the redactor consults.

## Findings I'm departing from (if any)

None. This phase revisits neither D-303 nor D-341's engine; it is a fail-closed gated wire extension (D-346) of the shipped injection engine, authorized by the new decision, not a silent departure.

## Goals

- With the opt-in OFF (default): an `add_mcp_connection` carrying any injection field is rejected with a clear error (`ErrWireInjectionNotAllowed`, → 400) — no attach, nothing persisted.
- With the opt-in ON: the add comes online, the injection mapping is carried into the attach so the shared engine sources + injects the per-user credential, and the mapping is PERSISTED in the config revision (diff / rollback / list parity).
- The reachable sink is DERIVED from `connection.url` + validated against the named broker's boot-declared allow-list at attach time (the shared engine's authority); there is no wire-supplied host field.
- Every declared target key is redaction-covered (validation rejects a header / `_meta` leaf the audit redactor would not hold to `***`); reserved `_meta` segments and the `Authorization` header (for form=header) are rejected.
- The boot opt-in (config OR env) is fail-closed, boot-captured, INDEPENDENT of the wire-OAuth opt-in, and prints the `[DEV-ONLY WIRE INJECTION — DO NOT USE IN PRODUCTION]` stderr banner when the env fires.

## Non-goals

- A NEW Protocol method — `add_mcp_connection` already exists; `injection` is a new optional field on its connection descriptor.
- Reusing `allow_wire_oauth_descriptor` — this phase adds a PARALLEL, independent opt-in (an operator may enable one without the other).
- Changing the injection ENGINE (`resolveInjectionBinding`, `CredentialInjection`) — reused unchanged; this phase is wire-plumbing + the gate + persistence + validation.
- Bumping `ProtocolVersion` — the new field + type are additive `omitempty`, so `0.1.0` stands.

## Acceptance criteria

- [ ] `AgentConfigMCPConnectionDescriptor` gains an optional `injection` object (`AgentConfigMCPCredentialInjectionDescriptor` with `provider` / `form` / `header` / `basic_username` / `meta_key`), all `omitempty`.
- [ ] Opt-in OFF → a connection carrying any injection field is rejected (fail-loud, distinct typed error, names the opt-in key); a connection without injection is unaffected.
- [ ] The gate + shape validation are applied at BOTH persistence doors — `add_mcp_connection` AND the full-payload `agent_config.set_revision` (which also accepts `connections.servers[].injection` from the wire); opt-in-off + injection via set_revision → rejected, nothing persisted.
- [ ] Opt-in ON → the mapping is validated, carried into the AttachRequest so the shared engine fires, and PERSISTED in the revision (round-trips through get / list / diff / rollback).
- [ ] Validation rejects a non-redaction-covered header/`_meta` leaf, a reserved `_meta` segment, a `meta_key` deeper than the redactor-safe cap, the `Authorization` header (form=header), and an unknown/empty form; injection is mutually exclusive with `oauth_provider` / inline `oauth` and rejected on stdio.
- [ ] The reachable sink is derived from `connection.url` (never a wire field) and validated against the named broker's boot-declared allow-list at attach time.
- [ ] `tools.allow_wire_injection` (config, default false) + `HARBOR_ALLOW_WIRE_INJECTION` (boot env) feed one effective posture (OR), INDEPENDENT of the wire-OAuth opt-in; env fire prints the banner at the dev/console/serve boot sites.
- [ ] The audit redactor covers the wire-declared keys (verified: D-341 extended it via the SAME predicate the wire validation uses — no gap).
- [ ] Lockstep: `singlesource` + both generator type-indexes + Console typed client + `wire-manifest.gen.json` mirror the new type/field (`make protocol-ts-gen-check` green); generated protocol docs regenerated (`make protocol-docs-gen-check` green).
- [ ] `scripts/smoke/phase-203.sh` asserts the exfil guard: opt-in-off rejects an injection-bearing add (400); a plain add is unaffected; models on the phase-199 shape.

## Files added or changed

```text
internal/protocol/types/agentconfig.go                     # NEW AgentConfigMCPCredentialInjectionDescriptor + injection field (additive omitempty)
internal/protocol/singlesource/singlesource.go             # register the new canonical wire type
cmd/harbor-protocol-ts-lockstep/typeindex.go               # typeInstanceIndex entry for the new type
cmd/harbor-gen-protocol-docs/typeindex.go                  # typeInstanceIndex entry for the new type
internal/agentcfg/agentcfg.go                              # domain MCPCredentialInjectionDescriptor + injection field + canonicalisation
internal/runtime/agentcfg/protocol/wireinjectiondescriptor.go  # NEW gate + shape validation (ErrWireInjectionNotAllowed)
internal/runtime/agentcfg/protocol/addconnection.go        # thread injection through validate → gate → attach → revision → wire projection
internal/runtime/agentcfg/protocol/service.go              # allowWireInjection field + WithAllowWireInjection + revision-view injection projection
internal/runtime/serve/mcp_attacher.go                     # project injection into config.MCPServerConfig (shared engine fires)
internal/runtime/serve/mux.go + serve.go                   # thread the effective opt-in (config OR captured env)
internal/tools/auth/wire_injection_gate.go                 # NEW boot-capture atomic (parallel to wire_descriptor_gate.go)
internal/config/config.go                                  # tools.allow_wire_injection (bool, default false)
cmd/harbor/{cmd_dev,cmd_console,cmd_serve,devmock}.go       # HARBOR_ALLOW_WIRE_INJECTION capture + banner at the boot sites
harbortest/devstack/devstack.go                            # thread cfg.Tools.AllowWireInjection into the mux input
web/console/src/lib/protocol/agentconfig.ts                # TS mirror of the new type + field
web/console/src/lib/protocol/wire-manifest.gen.json        # regenerated (make protocol-ts-gen)
docs/site/protocol/{types,methods}.md                      # regenerated (make protocol-docs-gen)
examples/harbor.yaml                                       # documented tools.allow_wire_injection default-off
docs/skills/use-the-harbor-protocol/SKILL.md               # note the gated wire-injection path (surface: protocol)
docs/site/skills/use-the-harbor-protocol.md                # (include stub — unchanged; body mirrored)
internal/.../*_test.go                                     # gate on/off, validate, persist, concurrency
test/integration/phase203_wire_injection_test.go           # NEW — full protocol→agentcfg→serve→tools/mcp→audit path
scripts/smoke/phase-203.sh                                 # NEW — exfil guard (opt-in-off rejects injection field)
docs/plans/phase-203-wire-injection-descriptor.md          # this plan
docs/plans/README.md                                       # phase-203 row + detail block
docs/decisions.md                                          # D-346
docs/glossary.md                                           # "Wire-carried credential-injection mapping"
```

## Public API surface

- Wire types (additive, `omitempty`): `AgentConfigMCPCredentialInjectionDescriptor` + `AgentConfigMCPConnectionDescriptor.Injection`.
- Config: `config.ToolsConfig.AllowWireInjection bool`.
- `cmd/harbor`: `EnvAllowWireInjection` boot-capture + `WireInjectionBanner` (mirror the wire-OAuth capture).
- `agentcfgprotocol`: `WithAllowWireInjection`, `ErrWireInjectionNotAllowed`, `AttachRequest.Injection`.
- `internal/tools/auth`: `RegisterAllowWireInjectionCaptured` / `AllowWireInjectionCaptured`.

## Test plan

- **Unit:** gate off rejects an injection-bearing add (no attach); gate on carries + persists the mapping; validation rejects each malformed form + reserved segment + non-credential key + mutual-exclusivity + stdio; the boot-capture atomic toggles + is independent of the wire-OAuth capture.
- **Integration:** through `internal/runtime/agentcfg` → `internal/runtime/serve` (real MCP attacher) → `internal/tools/drivers/mcp` (real driver + broker + receiver httptest server) → `internal/audit` — opt-in-on `add_mcp_connection` comes online, persists the mapping, injects the per-user value; two users get distinct values (isolation); the redactor holds every form to `***`; a broker outage fails loud with no wire call; opt-in-off rejects.
- **Conformance:** N/A — single injection engine, single provider driver.
- **Concurrency / leak:** N≥100 concurrent injection adds against one shared Service under `-race` (D-025); the per-user isolation stress rides the integration path.

## Smoke script additions

- `scripts/smoke/phase-203.sh` (`PREFLIGHT_REQUIRES: live-server`, modeled on `phase-199.sh`): with the opt-in OFF (default preflight boot), an `add_mcp_connection` carrying an `injection` mapping is REJECTED 400 (the exfil guard) AND a full-payload `set_revision` carrying `connections[].injection` is REJECTED 400 (the second-door guard); a plain add is unaffected; assert the new wire type + method are present in `wire-manifest.gen.json` + the generated docs; SKIP cleanly on 404/405/501.

## Coverage target

- `internal/runtime/agentcfg/protocol`: ≥ 80% on the gate/validate/persist paths. `internal/config`: the new flag parsed (structural).

## Dependencies

- HA-34 (Phase 200, D-341 — the injection engine), HA-32 (Phase 199, D-340 — the wire-OAuth posture this mirrors), the add-connection lifecycle (#375), and the tokenexchange broker-pull (D-271/D-285) — all on `dev-experimental`.

## Risks / open questions

- The wire injection mapping lets an OPTED-IN operator's admin-scoped caller wire a per-user credential to a receiver at runtime. This is the deliberate, reviewed relaxation (a fail-closed boot opt-in) — bounded by the derived+allow-listed downstream sink (a stolen descriptor cannot redirect a pulled credential to an attacker host), the redaction-coverage validation (a target key the redactor cannot cover is rejected), and the broker-pull-per-user posture (only the non-secret mapping rides the wire). Default-off keeps production on the boot-declared-only posture. No RFC §11 open question.
- §17.8: the injection round-trip is exercised against a real receiver-style httptest server on the official go-sdk streamable-HTTP handler, observing the ACTUAL injected value at a recorder (not a self-consistent fixture), so a wrong-field wiring cannot pass.

## Glossary additions

- **Wire-carried credential-injection mapping** — the gated `injection` object on `add_mcp_connection` that NAMES a boot-declared broker + declares the target key/form for a receiver-style server, accepted only behind the fail-closed `tools.allow_wire_injection` boot opt-in; the reachable sink is derived from the connection URL and every target key is redaction-covered. D-346.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: cross-session isolation test passes — the integration per-user isolation test asserts two acting users get distinct injected values.
- [ ] **Concurrent-reuse test passes** — the shared Service is exercised by N≥100 concurrent injection adds under `-race`. See §5 + D-025.
- [ ] **Integration test exists** — `agentcfg` → `serve` → `tools/mcp` → `audit` inject + redact + isolate end-to-end (Deps names shipped injection-engine + attach-lifecycle phases).
- [ ] If Protocol types changed: `make protocol-ts-gen-check` + `make protocol-docs-gen-check` green; Console typed client mirrors the fields.
- [ ] If config schema changed: `examples/harbor.yaml` updated; backward compatible (new optional bool, default false).
- [ ] If new vocabulary: glossary updated
- [ ] If a brief finding was departed from: N/A — no departure.
