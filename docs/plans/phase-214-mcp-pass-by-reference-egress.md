# Phase 214 — the MCP arm of pass-by-reference routing

## Summary

Phase 210 shipped pass-by-reference routing's in-process arm and deferred the
third-party arm on three named blockers, all of which concern handing a remote consumer
something it can DEREFERENCE. This phase delivers a different shape those blockers do
not reach: the runtime resolves the reference itself and places the BYTES into the
outbound MCP tool call. No address is published, no grant is minted, no reusable handle
exists.

Three things carry the phase, and each one is a correction of a first draft that reasoned
where it should have grepped:

1. **The wire encoding is normative, not an open question.** `internal/tools/artifactref.Substitute`
   walks a Go TYPE tree (`artifactref.go:274-283`) and structurally cannot serve an MCP
   tool whose `inputSchema` is authored by the remote server, so the MCP arm needs its
   own encoder — and the encoding is decided by the spec, not by preference. §"Wire
   encoding" below records the measurement.
2. **The security posture is stated, not claimed away.** Byte-eligibility is
   wire-configurable and admin-writable, which places it inside a trust boundary D-301
   already accepted and named. The phase says so, and pays for it with a compensating
   control: the FACT of a substitution — ids, sizes, a digest, never the bytes — is
   recorded in the audit trail and the trajectory, because the one real difference
   between this path and an admin pasting content by hand is that pasting leaves a
   trace.
3. **The fence claim is honest.** The shipped AST scan bounds where a function is
   CALLED, not where its output TRAVELS, and it resolves by import path so a
   same-package call site is invisible to it. The invariant is held by the same three
   mechanisms D-354 established, re-derived for a value that is not a `Ref`.

## RFC anchor

- RFC §6.4 — tool catalog and transports (the MCP driver, the outbound `tools/call`
  path the substitution rides, and the operator-declared connection descriptor that
  carries the mapping).
- RFC §6.5 — LLM client layer (the context-window safety net this feature exists to keep
  honest: bytes reach the consumer without transiting the model).
- RFC §6.10 — Artifacts (the store the reference resolves through, on the isolation-triple
  read key).
- RFC §7 — the Console as a Protocol client (the MCP-App tool-context surface that turns
  out to be the sixth sink the substitution invariant must clear).

## Briefs informing this phase

- brief 03
- brief 05
- brief 14

## Brief findings incorporated

- **brief 05 §"ArtifactStore" (l.12):** "Returns compact `ArtifactRef`s that are safe to
  embed in LLM context." The reference id is the safe-to-embed half. This phase is the
  UNSAFE half moving the other way — store → remote consumer — and the whole design
  question is how to move it without it becoming embeddable anywhere else.
- **brief 05 §"Runtime → ArtifactStore" (l.248):** "the runtime injects a
  `ScopedArtifacts` facade per task that auto-stamps the identity triple on writes and
  scope-checks on reads. Tools call `Upload` / `Download` against the facade — they never
  see raw scopes." The MCP arm is the strongest form of that finding: the remote server
  never sees a scope, a store handle, an id or an address. It sees one parameter value on
  one request, chosen by the runtime under an identity the server never learns.
- **brief 03 §5 "Sharp Edges Harbor Must Avoid" (l.186-188):** "Harbor picks one
  architecture and bakes the correction in" — the toggle smell. Applied twice here. The
  mapping is refused where it cannot be honoured (stdio, non-eligible connection, a
  parameter the server's own schema does not declare as a string) rather than degrading
  to a second behaviour; and the MCP-App callback path is refused loud rather than given
  a second, differently-scoped resolver (§"Non-goals").
- **brief 14 §4 "What Harbor can honestly claim" (l.99-109):** "never write 'fully MCP
  compliant' unscoped… state the scoped one and let the harness substantiate it." Read
  onto this phase's security section: the claim is not "this grants no new reach", which
  is false when reach is measured at the admin who writes the field. The claim is the
  scoped one — the reachable artifact SET is the dispatching run's own triple and nothing
  wider — plus an explicit statement of the boundary that is NOT claimed.
- **brief 14 §2 item 26 (l.74) — the roots honesty defect** ("Capability advertised; no
  provider wired"), whose lesson §9's glossary entry states as "advertising an unserviced
  capability is a soft protocol violation." Read onto arguments rather than capabilities:
  Harbor declaring "this parameter takes artifact bytes" against a server whose
  `inputSchema` never declared such a parameter is the same shape. That is why the
  mapping is VALIDATED against the discovered `inputSchema` at attach rather than
  trusted.
- **brief 14 §2 item 18 (l.63):** "Tool invocation + content types | Wired | All five
  2025-11-25 content kinds lowered (`content.go`)." Those five kinds are the RESULT
  side. The argument side has no content-block union at all, which is what settles the
  wire encoding — see below.

## Findings I'm departing from (if any)

This phase departs from a settled decision in three distinct places. Each is recorded in
D-359 rather than applied quietly.

### 1. D-347 part 5's deferral — the three blockers describe a different design

D-347 part 5 (`decisions.md:9583-9591`) deferred "an HTTP / MCP / A2A tool that must be
handed something it can dereference" on the ADDRESS, the GRANT'S SEMANTICS, and the
CREDENTIAL OBLIGATION.

- **Address** — nothing dials in. The runtime dials out, as it already does for every MCP
  call, on a connection the operator configured. `internal/config` still has no
  `public_url` / `external_url`, and this phase adds none.
- **Grant semantics** — there is no grant. One tool call carries one bounded byte slice in
  its body; there is no reusable artefact whose retry, redirect, HEAD-then-GET or
  partial-transfer behaviour needs defining.
- **Credential obligation** — no bearer capability is minted. The obligation becomes "the
  outbound body must not be persisted unredacted", which §7 rule 7 and §13 already impose
  and which this phase discharges by never rewriting `args` (see §"The sinks").

### 2. D-347 part 5's §13 clause — the parallel-implementation half

The grant-semantics blocker carries a second sentence the draft skipped
(`decisions.md:9591`): *"shipping one feature whose two mechanisms carry different
security properties is the §13 parallel-implementation shape."* That clause must be
answered, not stepped around, because this phase does ship a second implementation of
pass-by-reference routing.

The answer is that the two implementations carry the SAME security properties on every
axis §13's clause is about:

| Property | In-process arm (D-354) | MCP arm (this phase) |
|---|---|---|
| Reachable artifact set | the dispatching run's `(tenant, user, session)` | the SAME seated resolver, the same triple |
| Value lifetime | one invocation attempt | one request body |
| Dereferenceable later? | no — a Go value | no — inline bytes, no address, no handle |
| Reaches trajectory / observation / events / audit / logs? | no | no |
| Failure posture | loud, typed | loud, typed |
| Who authorises the byte flow | the tool author's Go type | the operator's connection descriptor |

That is ONE mechanism with two transports, which is the distinction §13's clause draws —
and it is exactly why D-347 named the URL-MINTING shape as the parallel-implementation
risk: a grant's lifetime, dereferenceability and credential properties genuinely differ
from an in-process bind, so shipping both would be two mechanisms wearing one name. The
one axis that does differ here is the ceiling (a MiB-scale network budget, where the
in-process arm has none), and it differs because the bounded resource differs. A memory
budget is not a security property.

Two further §13 obligations are discharged rather than assumed:

- **No second reach definition.** The MCP-App callback path is refused loud, not given
  its own resolver. See §"Non-goals".
- **No second substitution primitive alongside `Substitute`.** The new encoder serves a
  shape `Substitute` structurally cannot: an untyped `map[string]any` decoded from a
  remote-authored schema. `Substitute` walks Go types via `reflect` (`artifactref.go:287-299`,
  `TypeContainsRef`), so it has nothing to bind to here. Two functions, one for each of
  two structurally different inputs, is not two implementations of one thing.

### 3. D-347 part 6 consequence 3 — the question the entry declined to answer

`decisions.md:9602`: *"Handing stored-as-authored bytes to their own author and handing
them to a third party are different questions, and this entry declines to answer the
second."*

That is precisely this phase's shape, and the first draft never mentioned it. The
question is answered here, in three parts, and D-359 records the answer as an answer:

1. **The third party is not arbitrary.** It is a server an operator configured, on a
   connection an operator marked byte-eligible, at a parameter an operator named. Three
   operator acts, none of them the model's and none of them the remote server's — a
   server-declared "this parameter is an artifact reference" annotation is considered and
   REJECTED (brief 14's host-obligation reading: a remote server must not drive host-side
   privileged behaviour, and deciding when the runtime reads its own store is exactly
   that).
2. **The recipient widens; the reachable SET does not.** The seated resolver
   (`internal/runtime/dispatch/dispatch.go:342-369`) closes over `(tenant, user,
   session)` from the run's quadruple and answers `ErrArtifactRefNotFound` for anything
   else. So "which bytes can move" is unchanged from the in-process arm; only "where they
   can go" changes, and that is what byte-eligibility governs.
3. **What the deferral protected does not arise.** The deferral's concern is that a third
   party receives something it can dereference LATER, repeatedly, outside the runtime's
   identity. One bounded slice in one request body is none of those.

The compounding fact D-347 part 6 also settles (`decisions.md:9594`) is stated rather
than discovered: **artifact bytes are stored AS AUTHORED — unredacted.** `handlePut`
calls the redactor as an ADMISSION GATE and discards its result. So what egresses is
by-design-unredacted content, and an artifact may itself contain a credential. That is a
fact about the credential plane, not an analogy to it, and it is why the security
section below refuses to describe this field as "carrying no secret."

**D-347's deferral stands unchanged for the URL-minting design it actually describes.**
Answering part 6 consequence 3 for THIS shape does not answer it for that one.

### 4. The D-338 / D-340 / D-346 boot-gate pattern is deliberately NOT taken

Byte-eligibility is WIRE-CONFIGURABLE. Its sibling wire-writable relaxations
(`tools.allow_wire_oauth_descriptor`, `tools.allow_wire_injection`) each sit behind a
fail-closed, boot-only, default-off opt-in. This one does not, and the reasoning is
recorded because the repo's pattern-match would otherwise add the gate by reflex.

Those gates exist because their fields determine WHERE A CREDENTIAL IS SENT — D-300's
invariant, stated verbatim as *"NO ADMIN-WRITABLE FIELD MAY DETERMINE WHERE A CREDENTIAL
IS SENT."* This field determines where a USER'S OWN CONTENT is sent. That is a different
plane, and one D-301 already ruled on: *"The wave claims NO hard cross-tenant isolation
of runtime-added tool DISPATCH in a shared runtime… a shared runtime therefore TRUSTS its
co-tenant admins,"* with the stated remedy being one-runtime-per-tenant. A boot gate
would also break the use case the feature exists for — an MCP server attached over the
Protocol being usable without a redeploy.

The trust boundary is written out in §"Risks", not argued away.

## Goals

- An MCP tool can receive artifact bytes without those bytes passing through the model's
  context, byte-exact for arbitrary binary content.
- The mapping from "artifact-bearing parameter" to a named MCP tool parameter is
  operator-declared, validated against the remote server's own discovered `inputSchema`,
  writable over the Protocol at BOTH persistence doors through ONE shared validator, and
  persisted in the agent-config revision like its sibling connection fields.
- A connection receives bytes only when an operator has declared it byte-eligible.
- The substitution invariant survives the wire: a resolved value reaches the outbound
  JSON-RPC body and nothing else — not the trajectory, not either observation, not an
  event payload, not an audit payload, not a log, and not the durable MCP-App tool-context
  record replayed into a browser.
- The FACT of every substitution is recorded — artifact id, server, tool, parameter,
  size, digest — in the canonical event stream (hence the audit trail) and in the
  trajectory. Never the bytes.
- The egress path has its own ceiling, sized for a network hop rather than a context
  window, and an oversize value fails loud rather than truncating.

## Non-goals

- **The HTTP and A2A transports.** This phase closes the MCP arm and states the other two
  as remaining, so the seam is visibly partial rather than silently so.
- **Any URL-minting, presign or grant design.** D-347's deferral stands for that shape.
- **Server-declared artifact-reference annotations.** Considered and rejected; recorded in
  D-359 so a future author does not re-derive it.
- **Streaming or chunked egress.** One bounded slice in one request body; progressive
  delivery stays reserved (D-343, untouched).
- **A byte-mapped parameter reachable from an MCP-App callback.** `internal/mcpconsole/apps.go:283`
  (`res, err := desc.Invoke(ctx, args)`) invokes the SAME catalog descriptor from a
  browser-driven `mcp.apps.call_tool` Protocol request. There is no run, so
  `dispatch.ExecuteDecision` never ran and no resolver is seated
  (`dispatch.go:303` is the only seat). A mapped tool invoked there therefore hits
  `artifactref.ErrNoResolver` and **fails loud**. That is the posture, and both
  alternatives are rejected on the record:
  - *Seating a second resolver there* would have to close over the browser request's
    triple rather than a run's quadruple, producing a SECOND definition of "what this
    feature can reach" for one feature. That destroys the single sentence the isolation
    story is made of and is the §13 parallel-implementation shape.
  - *Degrading* — sending the raw id string through as the parameter value — is the §13
    silent-degradation shape: the server receives `"art-abc123"` where it expects a
    document and either fails in its own vocabulary or succeeds on garbage.

  The consequence is stated as a known partial in the operator skill and the glossary: an
  MCP App's tool callback cannot use a byte-mapped parameter.
- **A per-user or user-authored byte-eligible connection.** Wave ruling 5 dropped
  user-owned MCP connections from this wave and re-scoped phase 216, so connection
  authorship stays admin-only. The wave-level question "can a user-scope connection be
  byte-eligible?" is therefore answered NO by construction, and is recorded here so it is
  not rediscovered as an open question.

## Wire encoding — normative

**The substituted value is a Go `[]byte`, carried by `artifactegress.Payload`, written
into the DECODED `map[string]any` at the mapped key and emitted on the wire as RFC 4648
§4 standard base64 with padding. It is never a Go `string`, and never an MCP typed
content block.**

The choice is settled by three grepped facts, not by preference.

**1. There is no argument-side content block. The alternative does not exist.**
`CallToolParams.Arguments` is `any` (go-sdk v1.6.1 `mcp/protocol.go:48`), an arbitrary
JSON value validated against the server's `inputSchema`. The `Content` union — `text` /
`image` / `audio` / `resource`, brief 14's "all five content kinds" — appears on exactly
three types, all of them results or sampling messages: `CallToolResult`
(`protocol.go:82`), `SamplingMessageV2` (`:492`) and `CreateMessageWithToolsResult`
(`:579`). MCP has no typed `blob` block for tool ARGUMENTS. Option (b) is eliminated by
the spec.

**2. A Go `string` in the decoded map corrupts binary — measured.** Harbor decodes
arguments into `map[string]any` at `internal/tools/drivers/mcp/mcp.go:850-855` and sets
`Arguments: argMap` at `:885`. `encoding/json` rewrites every invalid-UTF-8 byte in a Go
`string` to `U+FFFD`. Against a 10-byte fixture `25 50 44 46 FF FE 00 80 C3 28`:

| Slot type | Wire form | Round trip | Equal |
|---|---|---|---|
| `string(raw)` | `"%PDF\ufffd\ufffd\u0000\ufffd\ufffd("` | 18 bytes, three `ef bf bd` triples | **no** |
| `[]byte(raw)` | `"JVBERv/+AIDDKA=="` | 10 bytes, exact | **yes** |

The first draft's `MCPArtifactParams map[string][]string` mapped parameter names onto Go
strings, which makes its own byte-for-byte acceptance criterion unsatisfiable under its
own spec — the same defect phase 212 exists to fix (`internal/tools/builtin/artifact_fetch.go:170,:279`
populating `Content string` from `string(window)`), reintroduced one layer out.

**3. The carrier's `MarshalJSON` is reachable on the real wire path.** The SDK marshals
`CallToolParams` through `encoding/json` — `internal/jsonrpc2/messages.go:220-228` uses a
`json.Encoder` with HTML escaping disabled and the trailing newline trimmed — so a
`json.Marshaler` on the substituted value is honoured end to end. This is what lets the
value keep a carrier all the way to the socket instead of decaying into a naked `[]byte`.

### Pinning the choice (§17.8) — and why SDK types alone cannot pin it

**SDK-derived fixtures are self-consistent at either placement here, and there is a
concrete proof of it.** `jsonschema-go`'s `forType` has NO `[]byte` special case
(`jsonschema/infer.go:219-240`, `reflect.Slice` falls straight through to
`{"type":["null","array"], "items": {integer}}`), while `encoding/json` marshals and
unmarshals the same field as a base64 STRING. So an SDK-built fixture server whose
handler declares `Data []byte` ADVERTISES an array-typed parameter and ACCEPTS a base64
string. Its own tests pass either way. That is the D-216 failure shape exactly: a fixture
that cannot tell right-field from wrong-field is a rubber stamp.

The gate is therefore three-part:

1. **A live probe against a real server binary**, following the shipped pattern at
   `internal/tools/drivers/mcp/mcp_live_test.go:36` (`HARBOR_LIVE_MCP=1`, binary path
   overridable by env, `t.Skip` in CI). It drives a byte-shaped tool on a real stdio MCP
   server and asserts the server received the exact stored bytes.
2. **A committed transcript** of that probe's outbound `tools/call` frame, so CI has a
   byte-level golden even when the binary is absent. A future "improvement" that swaps
   the carrier fails a byte diff rather than passing a self-consistent fixture.
3. **A schema-derived attach-time check.** The mapped parameter MUST appear in the
   discovered `inputSchema` and MUST be string-typed; an absent or non-string parameter is
   refused loud at the door. This converts the encoding from Harbor's assumption into a
   contract checked against the server's own declaration, and it is brief 14's
   capability-honesty rule read onto arguments.

## The sinks — where `args` must NOT be touched

The substitution mutates ONLY the decoded `map[string]any`. **The `args json.RawMessage`
value is never rewritten.** This is the rule D-354 established for the in-process arm
("`dispatch` hands `desc.Invoke` the model's own args unchanged, and the bind happens on
the DECODED value inside the policy shell"), and it is load-bearing for two more sinks the
in-process arm never touched:

| # | Sink | Where |
|---|---|---|
| 1 | raw observation | `trajectory.Step.Observation` |
| 2 | LLM observation | `trajectory.Step.LLMObservation` |
| 3 | serialised trajectory | `internal/planner/trajectory` |
| 4 | canonical event payloads AND envelopes | published during the dispatch |
| 5 | audit payloads and log records | the bus's redactor + `slog` |
| 6 | the per-invocation content hash | `mcp.go:952` → `ToolCallID(runID, source, name, args)`, `internal/tools/drivers/mcp/toolcontext.go:62-75` (`h.Write(args)`) |
| 7 | the DURABLE MCP-App tool context | `mcp.go:953` → `captureToolContext` → `:1046` `Input: args` → `internal/mcpconsole/toolcontext.go:145` `s.shape(ctx, in.Input, …)` → a `state.StateRecord` at `:165-175`, offloading to the ArtifactStore above the heavy threshold at `:177-192`, and replayed into a browser by `Load` at `:199` via `mcp.apps.tool_context` |

Sink 7 is the one the first draft missed and it is the worst of the seven: it is durable,
Protocol-readable, session-scoped rather than run-scoped, and it can mint a SECOND
artifact containing the substituted bytes. Sinks 6 and 7 are asserted in the integration
suite alongside the original five.

## The fence — what it bounds, honestly

The shipped `ScanSubstitutionSites` (`internal/tools/artifactref/scan.go:93`) is a
production bound with two properties the first draft's acceptance criterion misread:

- It bounds where a function is **called** (`writers := map[string]struct{}{"Substitute": {}}`,
  `scan.go:94`), not where its output travels. Downstream of an encoder the value is data,
  and an AST walk over call sites says nothing about data flow.
- It resolves the package by **import path** (`local := importLocalName(f, PkgPath)`,
  `scan.go:137-140`; `PkgPath` at `:49`) and returns early for a file that does not import
  it. A file INSIDE `internal/tools/artifactref` is therefore invisible to it — so the
  draft's plan to put `egress.go` in that package would have made the new writer
  unscanned by its own scan.

And extending the existing allow-list covers nothing on this path anyway: the MCP arm
does not and cannot call `Substitute`, because `Substitute` walks a Go type tree
(`artifactref.go:274-299`) and there is no Go type here.

The invariant is instead held by the same three mechanisms D-354 ordered, re-derived for
a value that is not a `Ref`:

**(a) A production bound.** The encoder lives in its OWN package
`internal/tools/artifactegress`, and `artifactref/scan.go` gains `ScanEgressSites` keyed
on that package's import-path STRING. `artifactref` does not import `artifactegress`, so
there is no cycle and no same-package blind spot for the scanner. The residual blind spot
— a second call from inside `artifactegress` itself — is bounded by a test asserting the
package's non-test file set, so "the package is small enough to read" is checked rather
than asserted. Blank allow-list reasons and unmatched entries are reported, as they are
today.

**(b) A projection bound on the carrier.** `Encode` returns `artifactegress.Payload`, not
a naked `[]byte`. `Payload` keeps the bytes in an unexported field and projects itself
through every serialisation door Go offers: `MarshalJSON` emits the base64 string (the
one door that must carry content), while `String` and `LogValue` emit
`artifact <id> (<n> bytes)`. So a `Payload` reaching `fmt` or `slog` emits a reference BY
CONSTRUCTION — the exact idiom `Ref` uses (D-354 part 3b), and the direct answer to
"downstream of the encoder the value is a naked `[]byte` with no carrier."

**(c) An arrival check.** The integration suite walks all seven sinks above with per-arm
vacuity guards.

## Acceptance criteria

- [ ] A connection descriptor carries an optional per-tool artifact-parameter mapping AND
      a byte-eligibility flag, on BOTH the domain type (`internal/agentcfg/agentcfg.go:220`
      `MCPConnectionDescriptor`) and the wire type (`internal/protocol/types/agentconfig.go:102`
      `AgentConfigMCPConnectionDescriptor`), accepted at BOTH persistence doors and
      validated by the ONE shared validator both already use (`validateConnection`,
      `internal/runtime/agentcfg/protocol/addconnection.go:323`, reached from the
      full-payload door via `validateConnectionsSection` at `:421`).
- [ ] The same mapping is expressible at boot on `config.MCPServerConfig`
      (`internal/config/config.go:1583`), keyed per tool name in the shape `ToolPolicies`
      already establishes at `:1603`, so the boot path and the runtime-add path share ONE
      egress engine — the posture D-346's injection descriptor set.
- [ ] A mapping on a NON-eligible connection is REFUSED at the door with a distinct typed
      error and nothing is persisted. A mapping on a `stdio` transport is refused on the
      same rule the sibling http-only fields already use (`addconnection.go:355-366`).
- [ ] The mapped parameter is validated against the remote server's DISCOVERED
      `inputSchema`: absent, or declared non-string, is refused loud. A server that later
      changes its schema so a mapped parameter disappears fails the next attach loudly, not
      the next call silently.
- [ ] With no mapping declared, outbound MCP calls are byte-identical to today — asserted
      against a captured pre-phase frame, not by inspection.
- [ ] A mapped parameter carrying an artifact id resolves through the SAME run-scoped
      resolver the in-process arm uses (`dispatch.go:342-369`, seated once at `:303`), so
      the reachable set is the dispatching run's own `(tenant, user, session)` and nothing
      wider.
- [ ] A run naming an artifact id belonging to another tenant, another user, or another
      session receives not-found — asserted by a cross-identity test that fails if the
      resolver is ever replaced with a privileged read.
- [ ] **Wire encoding:** the value is emitted as RFC 4648 §4 standard base64 from a Go
      `[]byte` behind `artifactegress.Payload`. A binary fixture containing invalid UTF-8
      round-trips BYTE-EXACT to a real server. A test asserts the Go-`string` form
      corrupts, so the reason for the choice is pinned and not merely the choice.
- [ ] **§17.8 pin:** the encoding is gated by (a) an env-gated `HARBOR_LIVE_MCP` probe
      against a real MCP server binary, (b) a committed byte-level transcript golden of
      the outbound `tools/call` frame, and (c) the schema-derived attach check above.
      An SDK-derived fixture alone does NOT satisfy this criterion, and the test file says
      why in a comment naming the `jsonschema-go` / `encoding/json` mismatch.
- [ ] **`args` is never mutated.** The substitution writes only into the decoded
      `map[string]any`. Asserted at all SEVEN sinks in §"The sinks", each with a vacuity
      guard, including the durable MCP-App tool-context record and the artifact it can
      offload to.
- [ ] **The FACT of a substitution is recorded.** Every substitution emits a canonical
      `mcp.artifact_egressed` event with a `SafeSealed` payload carrying the identity
      quadruple, the server id, the tool name, the parameter name, the artifact id, the
      byte count and a `sha256:` digest — and NEVER the bytes. The event rides the
      driver's existing bus (the `publishAppAvailable` precedent, `mcp.go:970-1009`), so
      it flows through the audit redactor by the same path every other tool event does.
- [ ] **The record is fail-closed, not best-effort.** Unlike `publishAppAvailable`, a
      publish failure ABORTS the call before any wire request is issued — the emit-then-act
      ordering D-300 item 4 established when it corrected `handleSetRawHTMLTrust`. A
      substitution that could not be recorded does not happen.
- [ ] **The record reaches the trajectory.** It also rides `MCPToolValue` as an EXPORTED
      field, so it lands in `trajectory.Step.Observation` and in the LLM observation.
      (Contrast `AppRef *AppRef \`json:"-"\`` at `internal/tools/drivers/mcp/content.go:81`,
      which is deliberately excluded from the observation. This one is deliberately
      included: the model authored the id, and telling it "the id you named was delivered,
      N bytes" is honest, content-free, and replayable.)
- [ ] **The resolve happens ONCE per dispatched call, not once per retry attempt.**
      `mcp.go:790` wraps `callTool` in `tools.RunWithPolicy`, whose shell runs
      `totalAttempts := resolved.MaxRetries + 1` (`internal/tools/policy.go:366`) with the
      package default `MaxRetries: 3` (`:101`) — four invocations of the inner function.
      Resolution and encoding happen in the Invoke closure BEFORE the policy shell, and the
      resolved payload is passed in. A resolve failure is therefore not retried, which is
      also correct on the merits: an unresolvable id is a model mistake, not a transient
      fault (phase 212).
- [ ] **D-025 immutability:** the mapping and the eligibility flag are captured BY VALUE
      into the Invoke closure at discovery, exactly as `toolApp := parseAppRef(t.Meta)`
      already is (`mcp.go:783-789`, whose comment states the rule). `Provider.cfg`
      (`mcp.go:415`) is immutable after `New` (`:432`). A live mapping change takes effect
      at the next attach / reconcile, never mid-flight — the next-turn projection posture
      every other agent-config field has.
- [ ] The content-emitting encoder has exactly ONE production call site, held there by
      `ScanEgressSites`; an allow-list entry matching no call site is itself reported; and
      the encoder's package is asserted to contain only the encoder (the same-package blind
      spot named in §"The fence").
- [ ] `artifactegress.Payload` projects a reference — never content — through `String` and
      `LogValue`, and content through `MarshalJSON` alone. Pinned against a planted marker.
- [ ] A substituted value above the egress ceiling FAILS LOUD with a typed error naming the
      artifact, its size and the ceiling — it is not truncated. A truncated document
      delivered to an ingester is a corruption, not a bounded read, so the read path's
      truthful-truncation posture (D-347 part 4) deliberately does NOT apply here.
- [ ] The egress ceiling is operator config with a documented default, validated in
      `loader.go::Validate`, and is NOT derived from the heavy-output threshold — the
      independent-ceiling precedent the fetch bounds already set at
      `internal/config/config.go:2043,:2052,:2060`.
- [ ] An unresolvable id on a mapped parameter produces the recoverable, machine-
      distinguishable observation phase 212 introduces, not a step-terminating error;
      `ErrNoResolver` stays step-terminating, per phase 212's own criterion.
- [ ] A mapped tool invoked through `mcp.apps.call_tool` (`internal/mcpconsole/apps.go:283`)
      fails loud with a typed error naming the tool and the reason. Asserted.
- [ ] Mutation-verified: reverting each guard turns a smoke `OK` into a `FAIL`, never into
      a `SKIP`.

## Files added or changed

- `internal/tools/artifactegress/artifactegress.go` — NEW package: `Payload` (the
  projection-bounded carrier), `Encode` (the ONE content-emitting call site), `Mapping`,
  `ErrEgressTooLarge`.
- `internal/tools/artifactegress/artifactegress_test.go` — NEW, including the
  package-shape assertion.
- `internal/tools/artifactref/scan.go` — `ScanEgressSites` + the egress package path
  constant. (`artifactref` gains no import of `artifactegress`; the scan matches an
  import-path string.)
- `internal/tools/artifactref/scan_test.go` — the live egress-site allow-list, in the
  shape `substitutionSiteAllowList` already establishes.
- `internal/tools/drivers/mcp/mcp.go` — mapping resolution + substitution in the Invoke
  closure ahead of `RunWithPolicy`; the `mcp.artifact_egressed` emit; the egress record on
  `MCPToolValue`.
- `internal/tools/drivers/mcp/events.go` — the new canonical event type + `SafeSealed`
  payload, registered beside the existing `tool.*` / `mcp.*` types.
- `internal/tools/drivers/mcp/content.go` — the exported egress-record field on
  `MCPToolValue`.
- `internal/tools/drivers/mcp/attach.go` — byte-eligibility + mapping on the attach path;
  the `inputSchema` validation against the discovered tool set.
- `internal/config/config.go`, `internal/config/validate.go` — the boot-side mapping +
  eligibility on `MCPServerConfig` (`:1583`), the egress ceiling constant and its
  `Validate` arm.
- `internal/agentcfg/agentcfg.go` — `MCPConnectionDescriptor` (`:220`) gains the two
  fields + `Clone` coverage.
- `internal/runtime/agentcfg/protocol/addconnection.go` — `validateConnection` (`:323`)
  gains the eligibility / mapping / stdio rules; `validateConnectionsSection` (`:421`)
  inherits them unchanged.
- `internal/runtime/agentcfg/protocol/service.go` — the wire↔domain projection on both
  doors.
- `internal/runtime/serve/mcp_attacher.go` — carries the two fields from the descriptor
  into `AttachRequest` → `config.MCPServerConfig` (the D-302 wiring-gap lesson: a field on
  the descriptor that nothing carries forward is inert).
- `internal/protocol/types/agentconfig.go` — the wire descriptor (`:102`). **The first
  draft listed `internal/protocol/agentconfig.go`, which does not exist.**
- `web/console/src/lib/protocol/agentconfig.ts` + `wire-manifest.gen.json` — the TS mirror
  and the regenerated manifest (D-223). See §"Dependencies" on generated-file ownership.
- `docs/site/protocol/*.md` — regenerated (D-209).
- `examples/harbor.yaml` — the boot-side mapping + eligibility + the egress ceiling.
- `docs/CONFIG.md` — the new keys (mirrored to the site by the existing include stub at
  `docs/site/reference/config.md`).
- `test/integration/mcp_artifact_egress_test.go` — NEW.
- `internal/tools/drivers/mcp/testdata/egress_frame.golden.json` — the committed outbound
  `tools/call` transcript (§17.8).
- `scripts/smoke/phase-214.sh` — real assertions replacing the skeleton.
- `docs/decisions.md` — D-359.
- `docs/glossary.md` — the three new terms below.
- `docs/skills/add-an-in-process-tool/SKILL.md` (surface `tools`) — §5's pass-by-reference
  section gains the MCP arm and its two limits (byte-eligibility; no MCP-App callback).
- `docs/skills/use-the-harbor-protocol/SKILL.md` (surface `protocol`) — §8 and §8b's
  account of what an MCP connection admin write can reach. **§18 "when two surfaces
  compete for one skill update" applies: BOTH of the above, plus
  `docs/skills/define-the-agent-yaml/SKILL.md` (surface `agent-yaml`) for the boot-side
  key. There is no skill declaring `surface: mcp` — the first draft's "the MCP-surface
  skill" names a file that does not exist.**

## Public API surface

```go
// internal/config

// MCPArtifactParams maps an MCP tool name to the parameter names on that
// tool which carry artifact bytes. Operator-declared: a remote server
// never decides when the runtime reads its own store. Keyed per tool name
// in the same shape as ToolPolicies.
type MCPArtifactParams map[string][]string

// DefaultMCPArtifactEgressMaxBytes bounds ONE substituted value on one
// outbound call. Deliberately NOT derived from the heavy-output threshold:
// substituted bytes never enter a model's context, so the budget is a
// network and memory budget, not a token budget. An operator sizing a
// deployment multiplies it by the expected number of in-flight mapped
// calls; it is not multiplied by the retry budget, because resolution
// happens once per dispatched call rather than once per attempt.
const DefaultMCPArtifactEgressMaxBytes = 8 * 1024 * 1024

// internal/tools/artifactegress

// Payload carries a resolved artifact value to exactly one outbound tool
// call. The bytes live in an unexported field and the type projects
// itself through every serialisation door: MarshalJSON emits RFC 4648 §4
// standard base64 (the one door that must carry content), String and
// LogValue emit "artifact <id> (<n> bytes)". A Payload reaching fmt or
// slog therefore emits a reference BY CONSTRUCTION.
type Payload struct{ /* unexported fields */ }

func (p Payload) ID() string
func (p Payload) Size() int
func (p Payload) Digest() string                  // "sha256:<hex>"
func (p Payload) String() string                  // the reference
func (p Payload) LogValue() slog.Value            // the reference
func (p Payload) MarshalJSON() ([]byte, error)    // base64 of the content

// Mapping is the compiled per-tool parameter mapping, immutable after
// construction and captured by value into a driver's Invoke closure.
type Mapping struct{ /* unexported fields */ }

func CompileMapping(params config.MCPArtifactParams) (Mapping, error)
func (m Mapping) ParamsFor(tool string) []string

// Encode is THE one content-emitting call site: it resolves each mapped
// parameter's artifact id through the ctx-seated resolver and writes the
// resulting Payload into args in place. Returns one record per
// substitution for the caller to emit and to stamp on the observation.
// Fails loud on an oversize value, an unresolvable id, an absent
// resolver, or a mapped key the arguments did not supply.
func Encode(ctx context.Context, args map[string]any, m Mapping, tool string, maxBytes int) ([]Record, error)

// Record is the FACT of one substitution: ids and sizes, never bytes.
type Record struct {
    ArtifactID string `json:"artifact_id"`
    Param      string `json:"param"`
    SizeBytes  int    `json:"size_bytes"`
    Digest     string `json:"digest"` // "sha256:<hex>"
}

// ErrEgressTooLarge — a resolved value exceeds the configured ceiling.
// Loud rather than truncating: a partial document delivered to a consumer
// is a corruption, not a bounded read.
var ErrEgressTooLarge = errors.New("artifactegress: resolved value exceeds the egress ceiling")

// internal/tools/artifactref

// ScanEgressSites holds the content-emitting encoder to its reviewed call
// sites, resolving the egress package by IMPORT PATH so an alias is
// followed. It is a separate scan from ScanSubstitutionSites because the
// two writers live in two packages and a file that imports neither cannot
// call either.
func ScanEgressSites(root string, allow map[string]string) ([]Violation, int, error)

// internal/tools/drivers/mcp

// EventTypeMCPArtifactEgressed — one substitution occurred. SafePayload:
// ids, names, a size and a digest; never the bytes.
const EventTypeMCPArtifactEgressed events.EventType = "mcp.artifact_egressed"

type ArtifactEgressedPayload struct {
    events.SafeSealed
    Identity   identity.Quadruple
    ServerID   tools.ToolSourceID
    ToolName   string
    Records    []artifactegress.Record
    OccurredAt time.Time
}
```

## Test plan

- **Unit — `internal/tools/artifactegress`:** `Payload`'s three projections against a
  planted marker while the bound value is proven present; `MarshalJSON` byte-exactness
  over a binary fixture and the companion assertion that the Go-`string` form corrupts;
  `CompileMapping` refusals (empty tool name, empty parameter name, duplicate parameter);
  `Encode`'s resolve-and-write, its map write-back, and its four refusals (no resolver,
  empty id, unresolvable id, oversize); ceiling arithmetic at the boundary; the
  package-shape assertion backing the same-package blind spot.
- **Unit — `internal/tools/artifactref`:** `ScanEgressSites` finds a planted second call
  site, reports a stale allow-list entry, follows an import alias, and reads ≥200 files
  (the non-vacuity floor `scan_test.go:56` already uses).
- **Unit — connection validation:** the mapping accepted at both doors and refused for
  each of — non-eligible connection, `stdio` transport, unknown tool name, parameter absent
  from the discovered `inputSchema`, parameter declared non-string, empty parameter name,
  duplicate. Each refusal names the offending `connections.servers[i]` and persists
  nothing.
- **Unit — the driver:** the record's fail-closed ordering (a bus that refuses the publish
  aborts the call and issues NO wire request — asserted by a transport that counts
  frames); the resolve-once property (a resolver counting calls, against a policy forced
  to exhaust its retry budget, asserts exactly one resolve for four attempts); the
  observation carries the record and not the bytes.
- **§17.8 wire pin:** the committed `testdata/egress_frame.golden.json` transcript compared
  byte for byte; the `HARBOR_LIVE_MCP=1` probe against a real server binary asserting the
  received bytes' digest matches the stored artifact's; a comment in the test file naming
  the `jsonschema-go` `forType` / `encoding/json` mismatch as the reason an SDK-derived
  fixture is not sufficient.
- **Integration — `test/integration/mcp_artifact_egress_test.go`:** real drivers at every
  seam (in-memory artifact store, real event bus over the real pattern redactor, real
  catalog with the lifecycle shell live, production `dispatch.ToolExecutor`, a real
  `mcpsdk` server over in-memory transports). Store a binary artifact, dispatch a mapped
  tool call, assert the server received the exact stored bytes. Then walk ALL SEVEN sinks
  from §"The sinks" — including `ToolCallID`'s hash input and the durable
  `mcpconsole.ToolContextStore` record plus any artifact it offloaded to — and assert none
  contains the content, each arm guarded against vacuity. Identity propagation across the
  seam; cross-tenant / cross-user / cross-session ids all answer not-found. Failure modes
  (≥3): an unresolvable id yields the recoverable observation; an oversize value yields
  `ErrEgressTooLarge` and no wire request; a mapped tool invoked through
  `mcpconsole.AppsAccessor.CallTool` yields the typed no-resolver refusal.
- **Conformance:** N/A — no persistence interface gains a method.
- **Concurrency / leak:** the MCP `Provider` is a compiled artifact under D-025. N≥128
  concurrent invocations against ONE shared provider across two tenants and two sessions,
  half carrying mapped parameters, under `-race`: no data race, no cross-run byte bleed,
  no cancellation cross-talk, goroutines back to baseline after teardown. The cross-run
  bleed assertion derives each run's artifact LENGTH from its own index, so a bleed is a
  size mismatch rather than a byte compare (the shape `internal/tools/drivers/inproc/artifactref_test.go`
  already uses). A companion asserts the mapping captured at discovery is unaffected by a
  concurrent config mutation attempt.
- **Wave-end E2E:** if this phase is the last Stage-2 phase to merge, it also carries
  `test/integration/wave_v124_test.go` per the coordination file's staging note (a
  coordination file is outside §2's authority chain, so the E2E must be named in a phase
  plan's file list — this sentence is that naming).

## Smoke script additions

`scripts/smoke/phase-214.sh` (classified `live-server`; the surface is a Protocol write
plus a runtime seam):

- Attach a byte-eligible connection with a mapping; assert it persists and reads back
  through `agent_config.get` in normalised form.
- Attach a connection with a mapping but no eligibility; assert `400
  {"code":"invalid_request"}` with the typed error, and assert the active revision is
  unchanged.
- Attach a `stdio` connection with a mapping; assert the same refusal on the transport
  rule.
- Assert an existing connection with no mapping round-trips byte-identically through
  `set_revision` — the no-op guarantee.
- Source guards (`static-only` companions, so a deletion fails preflight rather than only
  `go test`): `artifactegress.Payload` carries `String` / `LogValue` / `MarshalJSON`;
  `ScanEgressSites` exists and is registered in the live allow-list test; `mcp.go` does not
  reassign `args` on the substitution path.
- Run the egress unit tests, the scan test, and the integration suite.
- Skip-if-404 across the whole Protocol block, so the script coexists with a pre-phase
  build.

## Coverage target

Measured on the tree at `plan/v124-wave` before the phase, so no target sanctions a
regression:

- `internal/tools/artifactegress` (new): 90%.
- `internal/tools/artifactref`: no regression (92.3%).
- `internal/tools/drivers/mcp`: no regression (85.7%).
- `internal/runtime/agentcfg/protocol`: no regression (85.3%).
- `internal/config`: no regression (82.9%).
- `internal/mcpconsole`: no regression (74.9%) — touched only by the refusal test.

## Dependencies

- **212** — byte correctness on the read path. Shipping an egress arm over a path that
  corrupts binary would deliver corrupted bytes very efficiently. 212 also supplies the
  machine-distinguishable unresolvable-ref observation this phase's failure criterion
  reuses.
- **210** — the run-scoped resolver (`dispatch.go:303,:342-369`), the substitution
  invariant, and the AST-scan shape `ScanEgressSites` mirrors.
- **209** — the artifact byte read.
- **206** — the owner-scoped registry the connection mutators run through.
- **211** — the owner-scoped registry MUTATORS. Both sibling phases touching this attach
  door declare it; the first draft omitted it. This phase writes a new field through the
  same `add_mcp_connection` / `set_revision` doors whose owner scoping 211 completes.

**Generated-file ownership.** Phase 215 owns `web/console/src/lib/protocol/wire-manifest.gen.json`
and `docs/site/protocol/*.md` in Stage 1. This phase REBASES on 215 and re-runs
`make protocol-ts-gen` + `make protocol-docs-gen` afterwards. §13 and D-209 / D-223
forbid hand-editing either, so a merge conflict there cannot be hand-resolved — the loser
re-runs the generators, and this phase is the loser by construction.

**Known file overlaps to sequence at merge**, not discover: `internal/config/validate.go`
and `internal/tools/drivers/mcp/mcp.go` with phase 217 (`_meta` nesting), and
`internal/runtime/agentcfg/protocol/addconnection.go` with 217 — both phases add a new
rule at the same attach door. 217 is Stage 1; this phase rebases.

## Risks / open questions

- **The security boundary this phase operates inside, stated rather than claimed away.**
  Byte-eligibility and the mapping are admin-writable over the wire: every non-safe-subset
  `agent_config.*` route gates on `auth.HasScope(ctx, auth.ScopeAdmin)`
  (`internal/protocol/transports/stream/agentconfig_handler.go:152`). So a tenant admin
  can attach a server they control, map a parameter, declare the connection byte-eligible,
  and receive a user's artifact bytes on the next run that names an id. **The first draft
  claimed this "grants no reach the connection did not already have"; that measured reach
  at the RUN, and it is false measured at the admin who writes the field.** What this
  phase claims instead, scoped:
  - The reachable artifact SET is unchanged — the dispatching run's own `(tenant, user,
    session)`, enforced by the same seated resolver the in-process arm uses. Only the
    RECIPIENT widens.
  - The admin already shapes the prompt and the tool set for the tenant's runs, so the
    approximation available today is "instruct the model to paste the content into a tool
    argument". That path is bounded by `findContextLeak` and leaves a trajectory trail.
    Egress substitution is MiB-scale and dispatch-local, so it would leave none.
  - **D-301 already accepted this boundary**: *"a shared runtime therefore TRUSTS its
    co-tenant admins,"* with the stated remedy that a deployment needing hard isolation
    runs ONE RUNTIME PER TENANT. This phase does not move that boundary and does not
    invent a new one.
  - **The compensating control is the substitution record.** The single real difference
    between this path and pasting by hand was tracelessness, and the record closes it at
    no cost: id → server, in the audit trail and the trajectory, fail-closed. It is an
    acceptance criterion, not a nicety.
  - **A byte-eligible connection can move a secret.** D-347 part 6 (`decisions.md:9594`)
    settles that artifact bytes are stored AS AUTHORED — unredacted — so an artifact may
    itself contain a credential. This is stated plainly rather than softened to "the field
    carries no secret", which is true of the FIELD and irrelevant to the FLOW.
- **The carrier's projection bound is breached, deliberately, at one place.** After this
  phase the property is "a resolved value serialises as a reference by construction,
  except at one fenced encoder." That is the single largest structural risk, and it is why
  the three-mechanism replacement in §"The fence" is spelled out rather than gestured at.
- **Memory arithmetic, sized rather than hoped.** `mcp.go:790` wraps `callTool` in the
  policy shell, which runs up to `MaxRetries + 1` attempts (`internal/tools/policy.go:366`,
  default 3 → four). Resolving inside that loop would make the transient footprint
  `ceiling × attempts × in-flight`; at 8 MiB × 4 × 128 that is 4 GiB. Resolving ONCE per
  dispatched call reduces it to `ceiling × in-flight`, which is the number documented on
  the config key and in `docs/CONFIG.md`. Stated as an accepted, sized property.
- **Base64 inflation on the wire.** An 8 MiB artifact is ~11 MB encoded. Acceptable over
  HTTP; the ceiling exists partly for this reason, and the mapping is refused on stdio
  where a large frame is least appropriate.
- **The `inputSchema` check is a point-in-time contract.** It runs at attach against the
  server's discovered tool set. A server that mutates its schema without a
  `tools/list_changed` notification (brief 14 §2 item 17 records that Harbor wires no
  `ToolListChangedHandler` today) can drift out from under a validated mapping. The bound:
  the drift surfaces as a server-side argument-validation error on the wire, which is a
  loud failure rather than a silent wrong-shape send. Named here so it is not rediscovered
  as a defect; closing it properly is the tool-list-changed phase's job, not this one's.
- **No remaining open questions.** The first draft's open question about typed content
  blocks is answered in §"Wire encoding" — the option does not exist on the argument side.

## Glossary additions

- **Egress substitution** — the runtime resolving an artifact reference and placing the
  resolved bytes into an outbound tool-call body. Distinct from a grant: nothing
  dereferenceable leaves the runtime, and the substituted value is bounded to that one
  request body. Carried by `artifactegress.Payload`, which projects a reference through
  `String` / `LogValue` and content through `MarshalJSON` alone, and encoded on the wire as
  RFC 4648 §4 standard base64 because MCP has no argument-side typed content block.
- **Byte-eligible connection** — an MCP connection an operator has declared may receive
  artifact bytes through egress substitution. Wire-configurable and admin-writable; the
  containment boundary for the feature, inside the co-tenant-admin trust boundary D-301
  accepted. Not reachable from an MCP-App tool callback, which has no run and therefore no
  seated resolver.
- **Substitution record** — the FACT of one egress substitution: artifact id, server, tool,
  parameter, byte count and `sha256:` digest, never the bytes. Emitted as
  `mcp.artifact_egressed` (fail-closed, before the wire request) and stamped on the
  observation so it reaches the trajectory. It exists because dispatch-local substitution
  would otherwise be the one content-movement path that leaves no trace.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] `make protocol-ts-gen-check` and `make protocol-docs-gen-check` pass, re-run AFTER
      rebasing on phase 215's generated artifacts
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ the stated target (no target below the measured
      pre-phase figure)
- [ ] Cross-session AND cross-tenant isolation tests pass on the egress path
- [ ] Concurrent-reuse test passes — N≥128 concurrent invocations against a single shared
      provider under `-race`, including the cross-run byte-bleed assertion and the mapping
      immutability companion
- [ ] Integration test wires real drivers, asserts identity propagation, walks all SEVEN
      sinks with vacuity guards, covers ≥3 failure modes, and runs under `-race`
- [ ] §17.8: the wire encoding is pinned by a committed real-server transcript AND an
      env-gated `HARBOR_LIVE_MCP` probe — an SDK-derived fixture alone does not satisfy the
      gate, and the test says why
- [ ] Glossary updated with the three terms above
- [ ] Operator skills updated (§18) — `add-an-in-process-tool` (surface `tools`),
      `use-the-harbor-protocol` (surface `protocol`) and `define-the-agent-yaml` (surface
      `agent-yaml`); `docs/CONFIG.md` + `examples/harbor.yaml` carry the new keys
- [ ] D-359 records: the departure from D-347 part 5, the answer to part 6 consequence 3,
      the §13 parallel-implementation discharge, the rejection of a server-declared
      annotation, the deliberate absence of a boot gate with its D-301 grounding, and the
      normative wire encoding with its measurement
