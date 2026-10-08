# Broker-bound App operations

Status: implementation candidate with real isolated external host/broker
qualification; production activation is separate. This extends the existing phase 238 MCP Apps path and the existing
signed-capability token exchange. It creates no issuer, permission store, planner
capability, browser credential channel, or alternate MCP transport.

## Protocol

`mcp.servers.read_resource` and `mcp.apps.call_tool` accept the optional
`app_operation` string: exactly 64 lowercase hexadecimal characters. The trusted
host obtains this opaque selector from its broker. It is not authority. The
broker owns its association with current login, organization, registered App,
provider, expiry and exact operation input.

Resource requests bind the selector to the reach-admitted effective agent,
exact server and `ui://` resource. Callbacks additionally require the existing
fresh `render_admission`; a legacy binding or missing admission is refused.
Current render checks, atomic catalog generation resolution, source confinement,
approval, policy, redaction and audit remain mandatory. No originating tool call
or chat transcript is required by these existing methods.

The runtime creates a call-local binding after Protocol admission. The MCP driver
checks the actual source, qualified tool name and outgoing arguments before
credential resolution. A transformed argument cannot retain the earlier binding.
Operation calls preserve JSON numbers rather than rounding through floating point.
Only a credential acknowledging that exact binding can reach the MCP transport.
An unbound or older credential provider is refused. The selector is never sent in
MCP metadata or a provider URL.

## Existing authenticated broker exchange

The signed-capability exchange adds one URL-encoded form field,
`app_operation`, containing this closed JSON object:

| Field | Meaning |
| --- | --- |
| `version` | Exactly `app-operation-v1`. |
| `reference` | Broker-owned opaque selector. |
| `kind` | `resource` or `tool`. |
| `agent_id` | Reach-admitted effective agent; must match signed provider. |
| `server_id` | Actual runtime source ID, including its owned physical namespace. |
| `resource_uri` | Exact rendered `ui://` resource. |
| `generation` | Required for tools: verified current catalog generation. |
| `tool` | Required for tools: exact qualified runtime catalog name. |
| `arguments_sha256` | Required for tools: exact canonical input digest. |

A successful operation-bound opt-in resource read also returns
`app_operation_generation`: the exact generation sealed into the accompanying
available render admission. The trusted host uses this returned value when
creating its next exact broker intent. Ordinary reads and unavailable admissions
omit it. It is a fingerprint, not authority; the sealed token and fresh runtime
checks remain mandatory. The host never decodes or reconstructs admission tokens.

Resource bindings omit the last three fields. Existing subject identity,
authenticated runtime client, signed-capability actor, audience and resource
destination are unchanged and still required. Configured broad `scope` is omitted
on this lane. The broker must resolve the selector and compare all coordinates
against its own current state, independently discover required dependencies and
project current policy. It must not treat the selector, requested tool or digest
as permission.

The broker response must retain the exact existing signed audience/resource
acknowledgement and return `token_type: Bearer`, `expires_in` between 1 and 30,
and `app_operation_sha256` equal to SHA-256 of the exact UTF-8 JSON bytes in the
decoded form field. Missing or mismatched acknowledgement refuses the response.
An old broker cannot silently ignore this extension.

Canonical tool input is a JSON object, at most 1 MiB and 32 nested levels.
Duplicate keys at any level, trailing input and non-object roots are rejected.
Absent arguments mean `{}`. Decode using exact JSON numbers; serialize with Go
`encoding/json.Marshal` (sorted object keys, normalized string escaping) and hash
the resulting bytes with SHA-256. Numeric spelling is preserved: `1` and `1.0`
remain distinct. The broker must use these same rules for its saved input.

## Lifecycle and activation

Each operation performs a fresh broker pull, without shared credential cache,
singleflight, or the ordinary profile's re-exchange floor. The request retains
caller cancellation, a maximum 30-second exchange deadline, provider shutdown
cancellation, current signed-use authorization and credential-exchange audit.
No operation credential is persisted. Ordinary requests retain existing behavior.

The MCP reliability shell does not automatically retry operation-bound callbacks:
an uncertain effect returns to the host, which uses the provider's explicit
idempotency or recovery operations. Existing time limits and safety gates remain.
Closing an App, logout, organization changes, withdrawal or stale document state
must invalidate broker-owned selectors. Already issued tokens retain their bounded
offline lifetime; this is not instantaneous revocation.

The host must keep its runtime bearer and operation selectors outside the frame,
bind resource bytes to the approved document digest, and preserve existing
fresh-admission recovery rules. An App-governed provider must deny generic user
exchange paths, including when disabled. Activation must retire existing generic
provider bindings and cached credentials before the new App lane is admitted.
Registration/discovery preparation remains separate from interactive authority.
Until the external broker and host implement and prove this contract, the lane
must remain disabled there. No production activation is part of this candidate.

## Local evidence

- `TestAppsSurface_AppOperationRequiresCurrentAdmission`: current admission,
  identity/agent/server/resource/generation and input-shape refusals.
- `TestAppOperation_ConcurrentCallsNeverShareCredentials`: 128 concurrent pulls,
  an already warm ordinary cache, exact selector isolation and fresh repeats.
- `TestAppOperation_RequiresBoundedBrokerAcknowledgement`: old broker, missing or
  excessive expiry, source swap and failure-cache refusals.
- `TestAppOperation_CancellationDoesNotPoisonAnotherCall`: real HTTP cancellation.
- `TestAppOperation_RealMCPBrokerDispatchAndUnknownEffect`: actual MCP SDK and HTTP
  transport, real token-exchange driver, exact integer input and no effect retry.

These are local runtime tests, not external product acceptance or hosted CI.
The separate real-service consumer qualification is recorded in
[the candidate review](../notes/app-operation-review.md#external-consumer-qualification--2026-10-07).
