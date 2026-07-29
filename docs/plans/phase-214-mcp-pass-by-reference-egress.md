# Phase 214 — the MCP arm of pass-by-reference routing

## Summary

Phase 210 shipped pass-by-reference routing's in-process arm and deferred the
third-party arm on three named blockers, all of which concerned handing a remote
consumer something it could dereference. This phase delivers a different shape that
those blockers do not reach: the runtime resolves the reference itself and places the
BYTES into the outbound MCP tool call. No address is published, no grant is minted, no
reusable handle exists. The direction of travel is identical to the in-process arm —
store → consumer, the model authors an id and never sees content — with a wire between
two of its parts.

## RFC anchor

- RFC §6.4
- RFC §6.10
- RFC §6.5

## Briefs informing this phase

- brief 05
- brief 03
- brief 14

## Brief findings incorporated

- brief 05 (state, tasks, artifacts, sessions): mandatory artifact routing means the
  store is where heavy content lives and every consumer reaches it by reference. A
  consumer that can only be reached by pasting content into a model's context is not
  actually served by the routing guarantee — it is served by the model's willingness to
  act as a copier, which is exactly the shape the guarantee exists to remove.
- brief 03 (tools + integrations): the tool catalog is transport-agnostic by design and
  a capability that exists on one transport and not the others is a seam that will be
  papered over by callers rather than closed. The in-process arm shipping alone left
  three transports without it.
- brief 14 (MCP client/host compliance): an MCP tool's `inputSchema` is authored by the
  remote server. Harbor cannot reflect over a Go type to discover a byte-shaped
  parameter, so the mapping between "this parameter takes artifact bytes" and a named
  MCP tool parameter must come from somewhere Harbor controls.
- brief 14: a host must not let a remote server's declarations drive host-side
  privileged behaviour. A server-declared "this parameter is an artifact reference"
  annotation would let a remote server decide when the runtime reads its own store; the
  mapping is therefore operator-declared, not server-declared.

## Findings I'm departing from (if any)

**Departing from D-347 part 5's deferral, on the ground that the deferral's three
blockers describe a different design.** D-347 deferred "an HTTP / MCP / A2A tool that
must be handed something it can dereference" on: the ADDRESS (no configured
externally-reachable address exists), the GRANT'S SEMANTICS (a presigned URL cannot
claim single-use), and the CREDENTIAL OBLIGATION (a grant URL is a bearer capability).

None of the three reaches this phase's shape:

- **Address** — nothing dials in. The runtime dials out, as it already does for every
  MCP call, on a connection the operator configured.
- **Grant semantics** — there is no grant. One tool call carries the bytes in its body;
  there is no reusable artefact whose retry, redirect, HEAD-then-GET or partial-transfer
  behaviour needs defining.
- **Credential obligation** — no bearer capability is minted. The obligation transforms
  into "the outbound body must not be persisted unredacted", which is a rule §7 rule 7
  and §13 already impose on tool arguments generally.

The departure is recorded in D-359 rather than applied quietly, and D-347's deferral
stands unchanged for the URL-minting design it actually describes.

## Goals

- An MCP tool can receive artifact bytes without those bytes passing through the
  model's context.
- The mapping from "artifact-bearing parameter" to a named MCP tool parameter is
  operator-declared, writable over the Protocol, and persisted in the agent-config
  revision like its sibling connection fields.
- A server is byte-eligible only when the operator says so.
- The substitution invariant survives the wire: a resolved value still never reaches
  the trajectory, an observation, an event payload, an audit payload or a log.
- The egress path has its own size ceiling, sized for a network hop rather than a
  context window.

## Non-goals

- The HTTP and A2A transports. This phase closes the MCP arm and states the other two
  as remaining, so the seam is visibly partial rather than silently so. The mechanism is
  built at the transport-agnostic layer where it can be reused, but no second consumer
  is claimed in this phase.
- Any URL-minting, presign or grant design. D-347's deferral stands for that shape.
- Server-declared artifact-reference annotations. Recorded as considered and rejected
  in D-359 so a future author does not re-derive it.
- Streaming or chunked egress. A substituted value is one bounded byte slice in one
  request body; progressive delivery stays reserved (D-343, untouched).

## Acceptance criteria

- [ ] A connection descriptor carries an optional per-tool artifact-parameter mapping,
      accepted at BOTH persistence doors (`add_mcp_connection` and the full-payload
      `set_revision`), validated by ONE shared validator.
- [ ] A connection is byte-eligible only when the operator has declared it so; a
      mapping on a non-eligible connection is REFUSED at the door with a distinct typed
      error, and nothing is persisted.
- [ ] With no mapping declared, outbound MCP calls are byte-identical to today.
- [ ] A mapped parameter carrying an artifact id resolves through the SAME run-scoped
      resolver the in-process arm uses (`dispatch.go`'s
      `withArtifactResolver`), so the reachable set is the dispatching run's own
      `(tenant, user, session)` and nothing wider.
- [ ] A run naming an artifact id belonging to another tenant, another user, or another
      session receives not-found — asserted by a cross-identity test that fails if the
      resolver is ever replaced with a privileged read.
- [ ] The resolved value is emitted into the outbound JSON-RPC body and NOWHERE else:
      not the trajectory step, not the interleaved observation, not a canonical event
      payload, not an audit payload, not a log line. Asserted by walking each of those
      five sinks in the test, not by inspection.
- [ ] The content-emitting encode path is held to a reviewed allow-list by a mechanical
      AST scan, in the shape `ScanSubstitutionSites` already establishes; an allow-list
      entry matching no call site is itself reported.
- [ ] A substituted value above the egress ceiling FAILS LOUD with a typed error naming
      the artifact, its size and the ceiling — it is not truncated. A truncated document
      delivered to an ingester is a corruption, not a bounded read, so the truthful-
      truncation posture of the read path deliberately does NOT apply here.
- [ ] The egress ceiling is operator config with a documented default, validated in
      `loader.go::Validate`, and is NOT derived from the heavy-output threshold.
- [ ] An unresolvable id on a mapped parameter produces the recoverable observation
      phase 212 introduces, not a step-terminating error.
- [ ] A `Ref` reaching `json.Marshal` through any path OTHER than the fenced egress
      encoder still emits the id — the projection bound holds everywhere else.
- [ ] Mutation-verified: reverting each guard turns a smoke `OK` into a `FAIL`, never a
      `SKIP`.

## Files added or changed

- `internal/tools/artifactref/egress.go` — the fenced content-emitting encoder.
- `internal/tools/artifactref/scan.go` — the allow-list extends to the egress site.
- `internal/tools/drivers/mcp/mcp.go` — mapping resolution + substitution on the
  outbound call path.
- `internal/tools/drivers/mcp/attach.go` — byte-eligibility + mapping on the attach
  path.
- `internal/config/config.go`, `internal/config/validate.go` — the eligibility flag,
  the mapping shape, the egress ceiling.
- `internal/runtime/agentcfg/protocol/addconnection.go`,
  `internal/runtime/agentcfg/protocol/service.go` — wire descriptor + both doors.
- `internal/protocol/agentconfig.go`, `internal/protocol/types/` — wire type.
- `web/console/src/lib/protocol/agentconfig.ts` + `wire-manifest.gen.json` — TS mirror
  and regenerated manifest (D-223).
- `docs/site/protocol/*.md` — regenerated (D-209).
- `test/integration/mcp_artifact_egress_test.go`
- `scripts/smoke/phase-214.sh`
- `docs/decisions.md` — D-359.
- `docs/glossary.md` — "egress substitution", "byte-eligible connection".
- `docs/skills/` — the MCP-surface skill gains the mapping (§18).

## Public API surface

```go
// internal/config
// MCPArtifactParams maps an MCP tool name to the parameter names on that
// tool which carry artifact bytes. Operator-declared: a remote server
// never decides when the runtime reads its own store.
type MCPArtifactParams map[string][]string

// DefaultMCPEgressMaxBytes bounds ONE substituted value on one outbound
// call. It is deliberately not derived from the heavy-output threshold:
// substituted bytes never enter a model's context, so the budget is a
// network and memory budget, not a token budget.
const DefaultMCPEgressMaxBytes = 8 * 1024 * 1024

// internal/tools/artifactref
// ErrEgressTooLarge is returned when a resolved value exceeds the
// configured egress ceiling. Loud rather than truncating: a partial
// document delivered to a consumer is a corruption, not a bounded read.
var ErrEgressTooLarge = errors.New("artifactref: resolved value exceeds the egress ceiling")
```

## Test plan

- **Unit:** mapping validation at both doors (unknown tool name, empty parameter name,
  duplicate, mapping on a non-eligible connection, mapping on a stdio transport).
  Egress-ceiling arithmetic. The encoder emits content; every other door still emits
  the id.
- **Integration:** `test/integration/mcp_artifact_egress_test.go` — a real MCP server
  fixture (§17.8: driven from the official SDK's types, not a hand-authored shape) over
  a real artifact store and a real dispatch executor. Store a PDF, dispatch a tool call
  naming its id on a mapped parameter, assert the server received the exact stored bytes
  byte-for-byte. Then walk the trajectory, the observation, the event stream, the audit
  log and the captured log output and assert none contains the content. Identity
  propagation across the seam; cross-tenant id → not-found. Failure modes: unresolvable
  id (recoverable observation), oversize value (loud typed error), store read error.
- **Conformance:** N/A — no persistence interface gains a method.
- **Concurrency / leak:** the MCP provider is a compiled artifact under D-025. N≥128
  concurrent invocations against one shared provider across two tenants and two
  sessions, half carrying artifact parameters, under `-race`: no data race, no
  cross-run byte bleed (each call receives its OWN run's artifact), no cancellation
  cross-talk, goroutines back to baseline after teardown. The cross-run byte-bleed
  assertion is the one that matters most here and is called out separately from the
  generic race check.

## Smoke script additions

- Attach a byte-eligible connection with a mapping; assert it persists and reads back
  through `agent_config.get`.
- Attach a connection with a mapping but no eligibility; assert a 400 with the typed
  error and assert nothing was persisted.
- Assert an existing connection with no mapping is unchanged in the revision bytes.
- Drive a mapped call against the preflight fixture server and assert the server echoed
  the stored bytes' digest.
- Assert an oversize artifact produces the loud error rather than a truncated body.
- Skip-if-404 across the whole block.

## Coverage target

- `internal/tools/artifactref`: 90%
- `internal/tools/drivers/mcp`: 85%
- `internal/runtime/agentcfg/protocol`: 85%
- `internal/config`: 90% (no regression)

## Dependencies

- 212 (byte correctness on the read path — shipping an egress arm over a path that
  corrupts binary would deliver corrupted bytes very efficiently)
- 210 (the substitution invariant, the AST scan shape, the run-scoped resolver)
- 209 (the artifact byte read)
- 206 (the owner-scoped registry the connection mutators run through)

## Risks / open questions

- **The carrier's projection bound is breached, deliberately, at one place.** Today the
  invariant holds by construction: there is no way to marshal a `Ref` with content.
  This phase creates one, and from then on the property is "by construction except at
  the fenced egress site." That is the single largest risk in the phase, and the AST
  fence is the mitigation rather than a nicety — it is why the scan extension is an
  acceptance criterion and not an implementation note.
- **An agent that can read artifact X and call server Y can move X to Y.** This is
  inherent to the feature and is not designed away. Containment is the per-connection
  eligibility flag: the operator decides which servers may receive bytes. Stated in
  D-359 as an accepted property rather than an open question, so it is not rediscovered
  as a defect.
- **The mapping is admin-writable over the wire and this phase argues it needs no
  fail-closed boot gate.** The D-338 / D-340 / D-346 gates exist because those fields
  determine WHERE A CREDENTIAL IS SENT (D-300). This field names a parameter, carries
  no secret, and grants no reach the connection did not already have — the resolver
  stays run-scoped, so the only bytes that can move are ones the run's own identity
  already reaches, to a server the tenant's admin already attached and already sends
  arguments to. The reasoning is recorded explicitly because the repo's pattern-match
  would otherwise add the gate by reflex.
- **Base64 inflation on the wire.** A JSON-RPC body carrying an 8 MiB artifact is ~11 MB
  encoded. Acceptable over HTTP; the ceiling exists partly for this reason, and the
  mapping is refused on stdio transports where it is least appropriate.
- **Open question for the phase author:** whether the mapping should also be expressible
  against MCP's native typed content blocks (`blob` / `image` / `audio`) rather than
  only against named string parameters. Named parameters are the V1 because they work
  against every server; the typed-block route is the more idiomatic target and should be
  evaluated during implementation and recorded either way.

## Glossary additions

- **Egress substitution** — the runtime resolving an artifact reference and placing the
  resolved bytes into an outbound tool-call body. Distinct from a grant: nothing
  dereferenceable leaves the runtime, and the substituted value is bounded to that one
  request.
- **Byte-eligible connection** — an MCP connection an operator has declared may receive
  artifact bytes through egress substitution. The containment boundary for the feature.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] `make protocol-ts-gen-check` and `make protocol-docs-gen-check` pass
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] Cross-session AND cross-tenant isolation tests pass on the egress path
- [ ] Concurrent-reuse test passes — N≥128 concurrent invocations against a single
      shared provider under `-race`, including the cross-run byte-bleed assertion
- [ ] Integration test wires real drivers and a spec-derived MCP fixture (§17.8),
      asserts identity propagation, covers ≥3 failure modes, runs under `-race`
- [ ] If new vocabulary: glossary updated
- [ ] Operator skill updated (§18) — the MCP surface gains the mapping
- [ ] Departure from D-347 part 5 recorded in D-359
