# Phase 216 — user-scope MCP connections

## Summary

The per-user durable config tier (`ConfigScopeUser`) already composes into a run for
prompts, skills membership and tool exposure. It does not compose for connections: all
three reconcile legs read `ConfigScopeAgent` exclusively
(`projection.go:171`, `:249`, `:324`), and `prototypes.AgentConfigUserPayload` carries
no connections field at all. So a user can curate WHICH of the admin's tools they see,
and extend the prompt, but cannot bring their own MCP server. This phase closes that,
with the credential plane held out of scope by construction rather than by policy.

## RFC anchor

- RFC §6.4
- RFC §6.16
- RFC §7

## Briefs informing this phase

- brief 03
- brief 14
- brief 09

## Brief findings incorporated

- brief 03 (tools + integrations): the tool catalog is a per-run projection, not a
  process-global truth. A connection tier that exists for one config scope and not
  another leaves the projection incomplete in a way callers work around rather than
  report.
- brief 14 (MCP client/host compliance): a host is responsible for what it dials.
  Widening who may declare a dial target widens the host's exposure, so the widening
  must be scoped to targets that carry no credential.
- brief 09 (MCP OAuth from bifrost): credential custody is the hardest part of MCP host
  design and the place prior art most often got it wrong — a per-connection credential
  binding that any caller can author is the failure mode. Harbor's answer is D-300: no
  admin-writable field determines where a credential is sent. A user-writable one would
  be strictly worse, so this phase does not create one.

## Findings I'm departing from (if any)

None.

## Goals

- A user can declare their own MCP connections in their durable user-scope config, and
  those connections compose into their runs alongside the admin's.
- A user's connections are invisible to, and unaffected by, every other user.
- The credential plane is untouched: no user-writable field determines where a
  credential is sent.
- The reconcile legs partition per user as cleanly as they already partition per agent.

## Non-goals

- **Any credential-plane field on a user-scope connection.** No `injection` mapping, no
  `oauth_provider` binding, no inline `oauth`, no static `Authorization`, no attach-time
  `Headers`. A user-scope descriptor carrying any of them is refused at the door. This
  is the phase's central containment decision and it is structural: the user-scope
  descriptor type does not declare the fields, mirroring how
  `AgentConfigUserPayload` declines to declare `Base`.
- **Stdio transports on user-scope connections.** A user-declared subprocess is a
  remote-code-execution surface; the fail-closed stdio allowlist is an operator control
  and this phase does not extend it to user authorship.
- Per-user OAuth for user-declared servers. A user who needs an authenticated MCP server
  goes through an admin-declared connection. Whether a per-user OAuth flow over the
  unified pause/resume primitive should later serve this case is named in D-361 as the
  successor question, not answered here.
- Changing which scope the admin tier reads.

## Acceptance criteria

- [ ] `AgentConfigUserPayload` gains a connections section carrying ONLY: name,
      transport (http/streamable/sse), URL, and the non-secret `meta_annotations`
      set. No credential-plane field is declared on the type.
- [ ] A user-scope connection on a stdio transport is refused at the door with a typed
      error; nothing is persisted.
- [ ] A user-scope connection whose URL is not https (loopback excepted, matching the
      existing origin rule) is refused.
- [ ] The three reconcile legs compose the user tier: a user's declared connections
      attach for that user's runs and detach when the user's revision stops declaring
      them.
- [ ] **Name collision is resolved, not left to chance.** Two users declaring a
      connection under the same bare name, and a user declaring a name the admin tier
      already uses, both have a defined outcome asserted by test. The registry's
      process-global bare-name model (D-287/D-301) is PRESERVED — the resolution is at
      the projection layer, not by changing what a registration name means.
- [ ] Reconcile is partitioned per user: user A's run start never detaches user B's
      connection, and never detaches an admin connection.
- [ ] A user's connection is absent from another user's planner catalog view, asserted
      cross-user under one tenant AND across tenants.
- [ ] `agent_config.user.*` diff and rollback cover the connections section with the
      same parity the existing sections have.
- [ ] Mutation-verified: reverting the per-user partition turns a smoke `OK` into a
      `FAIL`.

## Files added or changed

- `internal/protocol/types/agentconfig.go` — the user-scope connections wire shape.
- `internal/runtime/agentcfg/protocol/user.go` — `userPayloadToDomain` extension +
  validation door.
- `internal/agentcfg/agentcfg.go` — payload section + normalisation + diff.
- `internal/runtime/agentcfg/projection/projection.go` — the three reconcile legs
  compose the user tier; the owner axis.
- `internal/tools/auth/` — the owner shape, if the user axis lands there.
- `internal/tools/drivers/mcp/registry.go` — projection-layer name resolution.
- `web/console/src/lib/protocol/` + `wire-manifest.gen.json` (D-223).
- `docs/site/protocol/*.md` (D-209).
- `test/integration/user_scope_connections_test.go`
- `scripts/smoke/phase-216.sh`
- `docs/decisions.md` — D-361.
- `docs/glossary.md` — "user-scope connection".
- `docs/skills/` — the MCP- and agent-yaml-surface skills (§18).

## Public API surface

```go
// internal/protocol/types
// AgentConfigUserConnectionDescriptor is a user-declared MCP connection.
// It deliberately declares NO credential-plane field — no injection, no
// oauth binding, no headers. A user-scope descriptor cannot express where
// a credential is sent, in the same way AgentConfigUserPayload cannot
// express a base prompt layer: the boundary is the type's shape, not a
// validator that could be bypassed by a second door.
type AgentConfigUserConnectionDescriptor struct {
    Name            string            `json:"name"`
    Transport       string            `json:"transport"`
    URL             string            `json:"url"`
    MetaAnnotations map[string]string `json:"meta_annotations,omitempty"`
}
```

## Test plan

- **Unit:** door validation (stdio refused; non-https refused; credential field
  rejected by `DisallowUnknownFields` decode, proving the shape boundary rather than
  asserting a validator). Normalisation, content-hash stability, diff coverage.
- **Integration:** `test/integration/user_scope_connections_test.go` — real agent-config
  registry, real MCP registry, real state store, a spec-derived MCP fixture server
  (§17.8). Two users under one tenant each declare a distinct connection; assert each
  sees only their own in the planner catalog view and that a run for one does not
  detach the other's. A third user declares a connection with the same bare name as
  user A's; assert the defined resolution. A user declares a name the admin tier uses;
  assert the defined resolution. Cross-tenant isolation on every leg. Failure modes: a
  server that refuses the dial (must not poison the other tier), a revision rollback
  that removes a connection mid-session.
- **Conformance:** N/A — no persistence interface gains a method.
- **Concurrency / leak:** N≥128 concurrent run starts across four users and two tenants,
  each with their own connection set, under `-race`: no cross-user catalog bleed, no
  connection thrash, goroutines back to baseline. The reconcile legs are the specific
  target — this is the phase most able to introduce a detach race.

## Smoke script additions

- Write a user-scope revision with a connection; assert `agent_config.user.get` reads
  it back.
- Assert a stdio user-scope connection is refused with the typed error and nothing
  persists.
- Assert a credential-plane field on the user descriptor is rejected by the decode.
- Assert two users' connections do not appear in each other's `tools.list`.
- Assert diff/rollback cover the section.
- Skip-if-404 across the block.

## Coverage target

- `internal/runtime/agentcfg/protocol`: 85%
- `internal/runtime/agentcfg/projection`: 85%
- `internal/agentcfg`: 90% (no regression)
- `internal/tools/drivers/mcp`: 85% (no regression)

## Dependencies

- 215 (caller-named agent selection — a per-user connection set is only meaningful once
  a user's runs can resolve against a chosen agent; without it the tier composes into a
  single hardcoded agent and the feature is untestable at its own boundary)
- 206, 211 (the owner-scoped registry and its sibling mutators, whose owner axis this
  phase extends)

## Risks / open questions

- **This is the highest-risk phase in the wave and the risk is the credential plane.**
  Every mitigation above is a subtraction — the user-scope descriptor cannot express a
  credential binding because the type has no field for it. A future PR that adds one
  "for convenience" would silently reopen D-300 from the least-privileged direction.
  D-361 must state this as an invariant, not as a current implementation detail.
- **The bare-name collision question is the phase's central design work and is
  deliberately left open here rather than pre-decided.** The registry's process-global
  bare-name model is load-bearing (D-287/D-301, preserved through phase 206 and 211),
  so the resolution belongs at the projection layer. The plausible answers —
  per-user namespacing in the projected catalog, first-writer-wins with a loud refusal
  on the second, or an explicit precedence rule (admin wins, user shadows) — have
  materially different operator-facing behaviour and the phase author must pick one,
  justify it against D-287, and record it. **Picking silently is the failure mode this
  bullet exists to prevent.**
- **The owner axis.** `auth.Owner{Tenant, Agent}` currently partitions reconcile. A user
  tier needs either a user axis on that type — which touches every owner-scoped call
  site shipped in phases 206 and 211 — or a defined reason a user's connections are
  partitioned some other way. Sized as the largest mechanical piece of the phase.
- **A user attaching an arbitrary https URL is an SSRF-adjacent surface.** The existing
  post-DNS private-range refusal (D-300/D-338) applies to the credential plane's dial;
  whether it applies to an ordinary MCP connection dial must be confirmed rather than
  assumed, and if it does not, this phase is where it starts to matter — a user-writable
  dial target is a materially different exposure from an admin-writable one.

## Glossary additions

- **User-scope connection** — an MCP connection declared in a user's durable
  `ConfigScopeUser` config rather than the admin tier. Composes into that user's runs
  only, and cannot carry a credential binding by the shape of its type.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] `make protocol-ts-gen-check` and `make protocol-docs-gen-check` pass
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] Cross-user AND cross-tenant isolation tests pass on the connection projection
- [ ] Concurrent-reuse test passes — N≥128 concurrent starts under `-race`
- [ ] Integration test wires real drivers and a spec-derived MCP fixture (§17.8),
      asserts identity propagation, covers ≥2 failure modes, runs under `-race`
- [ ] The bare-name collision resolution is chosen, justified against D-287, and
      recorded in D-361
- [ ] If new vocabulary: glossary updated
- [ ] Operator skills updated (§18)
