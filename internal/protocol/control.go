package protocol

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
	"github.com/hurtener/Harbor/internal/runtime/steering"
	"github.com/hurtener/Harbor/internal/tasks"
)

// methodToControlType maps each of the nine steering-control Protocol
// method names onto its steering.ControlType. MethodStart is NOT in this
// map — it routes to the task registry, not the steering inbox. The map
// is the single place the Protocol method vocabulary (lowercase
// snake_case, client-facing) is bridged to the steering taxonomy
// (uppercase, runtime-internal). Keeping the two namespaces distinct is
// deliberate (see internal/protocol/methods doc).
var methodToControlType = map[methods.Method]steering.ControlType{
	methods.MethodCancel:        steering.ControlCancel,
	methods.MethodPause:         steering.ControlPause,
	methods.MethodResume:        steering.ControlResume,
	methods.MethodRedirect:      steering.ControlRedirect,
	methods.MethodInjectContext: steering.ControlInjectContext,
	methods.MethodApprove:       steering.ControlApprove,
	methods.MethodReject:        steering.ControlReject,
	methods.MethodPrioritize:    steering.ControlPrioritize,
	methods.MethodUserMessage:   steering.ControlUserMessage,
}

// Dispatch is the single transport-agnostic entry point for a Protocol
// task-control method call. A HTTP/SSE handler decodes a
// request, calls Dispatch, and encodes the response — Dispatch IS the
// surface; the wire transport is a thin adapter over it.
//
// method selects the handler. req MUST be the wire request type the
// method expects:
//
//   - MethodStart        → *types.StartRequest,  returns *types.StartResponse
//   - the nine controls  → *types.ControlRequest, returns *types.ControlResponse
//
// Dispatch fails closed with a *errors.Error in every error case (the
// caller reaches a stable errors.Code via errors.As):
//
//   - CodeUnknownMethod   — method is not one of the ten canonical methods.
//   - CodeInvalidRequest  — req is nil or the wrong wire type for method.
//   - CodeIdentityRequired — the request's identity scope is incomplete
//     (RFC §5.5: the Protocol rejects any request without an identity
//     scope); for a control method, a missing run id also lands here.
//   - CodeScopeMismatch   — the caller's steering scope is below the
//     control method's RFC §6.3 minimum (mapped from steering.CheckScope).
//   - CodePayloadInvalid  — the control payload violated an RFC §6.3
//     bound (mapped from steering.ValidatePayload).
//   - CodeNotFound        — the target run has no live steering inbox.
//   - CodeRuntimeError    — an unclassified runtime-side failure.
//
// Dispatch holds no per-call state on the ControlSurface — it reads
// everything from ctx + req. One ControlSurface serves N
// concurrent Dispatch goroutines safely.
func (s *ControlSurface) Dispatch(ctx context.Context, method methods.Method, req any) (any, error) {
	if !methods.IsValidMethod(method) {
		return nil, protoerrors.Newf(protoerrors.CodeUnknownMethod,
			"method %q is not a canonical Protocol method", string(method))
	}

	if method == methods.MethodStart {
		return s.dispatchStart(ctx, req)
	}
	// The streaming-events
	// methods are served by their own transports (SSE for subscribe;
	// POST /v1/events/aggregate for aggregate), NOT by the REST control
	// surface. A caller that hits Dispatch with one of them is using
	// the wrong transport for the wrong vocabulary — surface it loud
	// rather than silently routing onto the steering inbox.
	if method == methods.MethodEventsSubscribe {
		return nil, protoerrors.Newf(protoerrors.CodeInvalidRequest,
			"method %q is a streaming-events method; open the SSE transport at GET /v1/events instead", string(method))
	}
	if method == methods.MethodEventsAggregate {
		return nil, protoerrors.Newf(protoerrors.CodeInvalidRequest,
			"method %q is a streaming-events method; POST to /v1/events/aggregate instead", string(method))
	}
	// the five `search.*` methods are dispatched by
	// SearchSurface, not ControlSurface — a caller that hits the REST
	// control surface with a search method is using the wrong transport
	// for the wrong vocabulary. Surface it loud rather than silently
	// routing onto the steering inbox.
	if methods.IsSearchMethod(method) {
		// The route named here is the one an operator can actually call:
		// `POST /v1/control/{method}` fronts the SearchSurface for all five
		// `search.*` methods. There is no `/v1/search` route and never was —
		// an error that names a non-existent endpoint sends the reader
		// somewhere that 404s, which is worse than naming no route at all.
		return nil, protoerrors.Newf(protoerrors.CodeInvalidRequest,
			"method %q is a search method; dispatch through the SearchSurface (POST /v1/control/%s) instead",
			string(method), string(method))
	}
	// the five `runtime.*` / `metrics.*` posture
	// methods are dispatched by PostureSurface, not ControlSurface — a
	// caller that hits the task-control Dispatch with a posture method
	// is using the wrong surface. Surface it loud rather than silently
	// routing onto the steering inbox.
	if methods.IsPostureMethod(method) {
		return nil, protoerrors.Newf(protoerrors.CodeInvalidRequest,
			"method %q is a posture method; dispatch through the PostureSurface instead", string(method))
	}
	// `pause.list` is a read-only snapshot over the
	// pauseresume.Coordinator — it routes through its own HTTP handler
	// (POST /v1/pause/list), not the task-control ControlSurface. A
	// caller that hits the control Dispatch with the pause method is
	// using the wrong surface. Surface it loud rather than silently
	// routing onto the steering inbox.
	if methods.IsPauseMethod(method) {
		return nil, protoerrors.Newf(protoerrors.CodeInvalidRequest,
			"method %q is a pause-snapshot method; POST to /v1/pause/list instead", string(method))
	}
	// `topology.snapshot` is a read-only projection
	// method — it reaches the engine's Topology accessor, not the
	// steering inbox. ControlSurface dispatches it directly (unlike
	// the streaming-events / search methods which route to other
	// surfaces) because the topology projection is a plain
	// request → reply with no streaming and no separate dispatcher.
	if methods.IsTopologyMethod(method) {
		return s.dispatchTopology(ctx, req)
	}
	// the twelve `mcp.servers.*` methods are
	// dispatched by the MCPSurface, not the task-control ControlSurface
	// — a caller that hits the control Dispatch with an MCP method is
	// using the wrong surface. Surface it loud rather than silently
	// routing onto the steering inbox.
	if methods.IsMCPServersMethod(method) {
		return nil, protoerrors.Newf(protoerrors.CodeInvalidRequest,
			"method %q is an MCP method; dispatch through the MCPSurface instead", string(method))
	}
	return s.dispatchControl(ctx, method, req)
}

// dispatchTopology handles the `topology.snapshot` method:
// it returns the Runtime engine's canonical TopologyProjection.
//
// The gauntlet, in order:
//
//  1. A nil topology accessor (a Runtime hosting no engine) →
//     CodeUnknownMethod. The route does not exist on this Runtime;
//     surfacing it as unknown-method maps to HTTP 404, which the
//     smoke script's 404 → SKIP convention picks up cleanly.
//  2. A wrong / nil request type → CodeInvalidRequest.
//  3. An incomplete identity triple → CodeIdentityRequired (RFC §5.5).
//  4. A cross-tenant request (caller tenant ≠ the engine's tenant)
//     without the verified auth.ScopeAdmin claim → CodeAuthRejected.
//
// The admin path additionally emits audit.admin_scope_used — the
// transport adapter owns the bus emit; here we record that an
// elevated read occurred by leaving the gate result observable.
//
//  5. The engine's Topology accessor builds the projection; an
//     identity-rejection from the accessor maps to CodeIdentityRequired,
//     anything else to CodeRuntimeError.
func (s *ControlSurface) dispatchTopology(ctx context.Context, req any) (*types.TopologyProjection, error) {
	method := methods.MethodTopologySnapshot

	if s.topology == nil {
		return nil, protoerrors.Newf(protoerrors.CodeUnknownMethod,
			"method %q: this Runtime hosts no engine — topology projection is not available", string(method))
	}

	tr, ok := req.(*types.TopologySnapshotRequest)
	if !ok || tr == nil {
		return nil, protoerrors.Newf(protoerrors.CodeInvalidRequest,
			"method %q: request is nil or not a *types.TopologySnapshotRequest", string(method))
	}

	// Identity-mandatory at the Protocol edge (RFC §5.5).
	callerID := identity.Identity{
		TenantID:  tr.Identity.Tenant,
		UserID:    tr.Identity.User,
		SessionID: tr.Identity.Session,
	}
	if err := identity.Validate(callerID); err != nil {
		return nil, protoerrors.Newf(protoerrors.CodeIdentityRequired,
			"method %q: identity scope incomplete: %v", string(method), err)
	}

	// Cross-tenant gate: a caller whose tenant differs from the
	// engine's tenant needs the verified auth.ScopeAdmin claim. The
	// projection carries no tenant id, but a cross-tenant read of
	// another tenant's graph is an admin-only operation. A granted
	// cross-tenant read emits audit.admin_scope_used (RFC §6.13 —
	// admin-scope use is retroactively auditable).
	crossTenant := callerID.TenantID != s.topology.TenantID()
	if crossTenant {
		if s.adminScope == nil || !s.adminScope(ctx, auth.ScopeAdmin) {
			return nil, protoerrors.Newf(protoerrors.CodeAuthRejected,
				"method %q: cross-tenant topology snapshot requires the admin scope", string(method))
		}
		// Audit the elevated read BEFORE returning the projection. A
		// failed audit emit fails the read closed (CLAUDE.md §5 — an
		// un-auditable admin read is rejected, never silently granted).
		if err := s.emitAdminScopeUsed(ctx, callerID); err != nil {
			return nil, protoerrors.Newf(protoerrors.CodeRuntimeError,
				"method %q: admin-scope audit emit failed; cross-tenant read refused", string(method))
		}
	}

	// The engine's Topology accessor reads identity from ctx. Seat the
	// validated caller identity so the accessor's identity-mandatory
	// gate passes. A cross-tenant read is seated as an audited crossing:
	// the admin claim was verified and the record written above, so the
	// crossing carries its justification with it.
	var (
		topoCtx context.Context
		err     error
	)
	if crossTenant {
		topoCtx, err = identity.WithElevated(ctx, callerID,
			fmt.Sprintf("method %q: cross-tenant topology snapshot under the admin scope claim", string(method)))
	} else {
		topoCtx, err = identity.With(ctx, callerID)
	}
	if err != nil {
		// Unreachable — callerID already passed identity.Validate above.
		return nil, protoerrors.Newf(protoerrors.CodeIdentityRequired,
			"method %q: identity scope incomplete", string(method))
	}

	proj, err := s.topology.Topology(topoCtx)
	if err != nil {
		return nil, mapTopologyError(string(method), err)
	}
	proj.SortDeterministic()
	return &proj, nil
}

// emitAdminScopeUsed publishes an `audit.admin_scope_used` event onto
// the wired bus when a cross-tenant `topology.snapshot` read is granted
// under the admin scope (RFC §6.13). The event is identity-scoped to
// the CALLER's triple — the admin who performed the read — so a Console
// auditing that admin's sessions sees the trail. The payload is the
// SafePayload events.AdminScopeUsedPayload (no secret-shaped fields).
//
// A nil bus (the surface was built without WithEventBus) is a no-op
// and returns nil — the cross-tenant read is still gated on the admin
// scope; only the audit trail is absent. Production wires the bus so
// the trail is complete. A non-nil bus that rejects the Publish
// returns the error: dispatchTopology fails the read closed rather
// than silently granting an un-auditable admin read (CLAUDE.md §5).
func (s *ControlSurface) emitAdminScopeUsed(ctx context.Context, caller identity.Identity) error {
	if s.bus == nil {
		return nil
	}
	return s.bus.Publish(ctx, events.Event{
		Type: events.EventTypeAdminScopeUsed,
		Identity: identity.Quadruple{
			Identity: caller,
		},
		Payload: events.AdminScopeUsedPayload{
			Tenant:  caller.TenantID,
			User:    caller.UserID,
			Session: caller.SessionID,
		},
	})
}

// maxOutputSchemaBytes caps the `start` request's `output_schema`
// document size. A JSON Schema large enough to matter is pathological;
// the cap keeps a hostile caller from forcing an expensive compile on
// every start. 64 KiB is generous for any realistic answer schema.
const maxOutputSchemaBytes = 64 * 1024

// maxCallerMemoryBytes caps the `start` request's `caller_memory`
// document size. It is a RESOURCE BOUND AND WIRE-SIZE GUARD — read the
// next paragraph before reasoning from it, because it is easy to mistake
// for something it is not.
//
// IT IS NOT A SECURITY BOUNDARY, AND NOTHING MAY BE INFERRED FROM IT
// ABOUT HOW MUCH CONTENT A CALLER CAN PUT IN FRONT OF THE MODEL. The
// same principal, on the same request, can send substantially more —
// through paths that land in positions carrying LESS framing, not more:
//
//   - `StartRequest.Query` has no cap of its own. A caller may spend the
//     whole remaining envelope on it, and it becomes the run's user turn
//     — the conversation position, which carries no anti-injection
//     preamble at all.
//   - `agent_config.session.set_user_prompt` requires a valid identity
//     and NO scope claim (it is the session-safe tier). It writes
//     `prompt_layers.user`, which renders INSIDE the system prompt, and
//     its transport bounds the body at 1 MiB — 32× this constant.
//
// So "the caller's content is capped at 32 KiB, therefore X is
// contained" does not hold for any X. What actually contains this
// payload is POSITIONAL: it reaches exactly one tier, that tier ships an
// anti-prompt-injection preamble, and it can never reach the trusted
// spine. The bound below is about bytes, not about trust.
//
// What the bound DOES do, which is real work: nothing downstream
// re-checks these bytes. The LLM-edge context-leak guard byte-exempts
// everything that is not a tool-role message (`internal/llm/safety.go`,
// `offloadableText := m.Role == RoleTool`) and memory tiers render under
// the system role, so an unbounded document would travel all the way to
// the token-budget guard and fail the whole run late, after the prompt
// was assembled. The bound therefore lives at the edge, before a task
// exists, so an oversized document costs one refusal instead of a run.
//
// CAP ORDERING IS AN INVARIANT, NOT A COINCIDENCE. This constant MUST
// stay strictly below the control transport's whole-body cap
// (`maxBodyBytes`, 64 KiB). Both refuse with the identical
// CodeInvalidRequest, so a field cap that rises to meet the envelope cap
// becomes unreachable dead code that every status-code test keeps
// passing against — the transport simply answers first. The refusal text
// below names the field precisely so a test can tell the two apart, and
// the phase smoke pins the ordering mechanically.
//
// It is still not an operator knob, for a reason that survives the
// correction above: a per-request byte dial on an admission whose real
// containment is positional buys nothing an operator can act on, and the
// cap-ordering invariant makes a configurable value a foot-gun (a value
// at or above the envelope cap silently disables the field's own check).
// If a legitimate caller is ever refused here, the answer is an additive
// optional config key defaulting to this constant, validated strictly
// below `maxBodyBytes`.
const maxCallerMemoryBytes = 32 * 1024

// maxExternalGrantBytes bounds the signed carrier before a task exists. A
// grant is content-free and should remain small; this is deliberately below
// the control envelope limit so the field check is reachable.
const maxExternalGrantBytes = 16 * 1024

// jsonNullLiteral is the four bytes `json.RawMessage` holds after
// decoding an explicit `"caller_memory": null`. An absent field decodes
// to a nil RawMessage; an explicit null decodes to this. The two are
// deliberately NOT collapsed: a caller that wrote null asked for
// something, and answering "accepted" while dropping it is the silent
// degradation CLAUDE.md §13 forbids.
const jsonNullLiteral = "null"

// validateCallerMemory applies the `start` request's caller-memory edge
// checks: the byte cap, the explicit-null refusal, and a JSON validity
// check. It returns nil for an OMITTED field (the unchanged path —
// byte-identical to a Runtime without it) and a Protocol error for every
// payload the Runtime will not admit.
//
// Every refusal names `caller_memory`. That is load-bearing: the control
// transport's own body-size refusal carries the SAME CodeInvalidRequest,
// so the field name is the only thing that distinguishes this check's
// answer from the envelope's.
func validateCallerMemory(method string, raw json.RawMessage) error {
	if raw == nil {
		return nil
	}
	if len(raw) > maxCallerMemoryBytes {
		return protoerrors.Newf(protoerrors.CodeInvalidRequest,
			"method %q: caller_memory exceeds the %d-byte cap (%d bytes)",
			method, maxCallerMemoryBytes, len(raw))
	}
	if strings.TrimSpace(string(raw)) == jsonNullLiteral {
		return protoerrors.Newf(protoerrors.CodeInvalidRequest,
			"method %q: caller_memory is explicitly null — omit the field to send no caller memory", method)
	}
	if !json.Valid(raw) {
		return protoerrors.Newf(protoerrors.CodeInvalidRequest,
			"method %q: caller_memory is not a valid JSON document", method)
	}
	return nil
}

func validateExternalGrant(method string, raw json.RawMessage) error {
	if raw == nil {
		return nil
	}
	if len(raw) == 0 || len(raw) > maxExternalGrantBytes {
		return protoerrors.Newf(protoerrors.CodeInvalidRequest,
			"method %q: external_grant must be a non-empty JSON object below %d bytes", method, maxExternalGrantBytes)
	}
	if !json.Valid(raw) {
		return protoerrors.Newf(protoerrors.CodeInvalidRequest,
			"method %q: external_grant is not valid JSON", method)
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		return protoerrors.Newf(protoerrors.CodeInvalidRequest,
			"method %q: external_grant must be a JSON object", method)
	}
	return nil
}

// agentNotResolvableMsg is the SINGLE refusal text for an unresolvable
// caller-named agent. It deliberately names neither the rejected id nor
// the reason: "registered under another tenant" and "never existed" must
// be indistinguishable, so the edge cannot be used as a cross-tenant
// existence oracle. One constant means a future edit cannot split the
// two cases apart by accident.
const agentNotResolvableMsg = "agent_id is not resolvable for the caller's tenant"

// validateNamedAgent applies the two-check rule to a `start` request's agent
// selection. A reach-enabled surface resolves an omitted id to the configured
// default. Every ControlSurface carries the shared fail-closed gate, so an
// omitted target is never a bypass.
//
// The unwired seam FAILS CLOSED. A surface built without an
// AgentResolver refuses a non-empty id rather than skipping the check:
// the run-loop driver would silently ignore the value, so accepting it
// would report success for a request that was not honoured. This is
// deliberately NOT the optional-and-skip posture of the SessionEnsurer
// seam, whose absence degrades an auxiliary behaviour rather than the
// caller's explicit instruction (CLAUDE.md §13).
//
// A resolver ERROR fails the request loud with CodeRuntimeError; it
// never falls through to the default agent.
func (s *ControlSurface) validateNamedAgent(ctx context.Context, method string, id identity.Identity, agentID string) (string, error) {
	return admitEffectiveAgent(ctx, method, id, agentID, s.agents, s.reach)
}

// admitEffectiveAgent is the shared reach-before-resolver admission used by
// every Protocol edge that consumes agent-addressed runtime authority. Reach
// MUST run before tenant-local resolution so the edge cannot become an agent
// existence oracle. The returned id is the sole value callers may stamp as an
// EffectiveAgentConfig execution capability.
func admitEffectiveAgent(ctx context.Context, method string, id identity.Identity, agentID string, agents AgentResolver, reach auth.AgentReachAuthorizer) (string, error) {
	// Determine the effective target without consulting tenant-local state.
	// Reach is the authority boundary, so it MUST run before ResolveAgent:
	// otherwise an untrusted caller can distinguish configured targets from
	// unknown or foreign ones by observing which lookup occurred. An omitted
	// id is an effective selection too; a bare surface deliberately leaves it
	// empty, which the same fail-closed gate rejects.
	effectiveID := agentID
	if resolver, ok := agents.(EffectiveAgentResolver); ok {
		var err error
		effectiveID, err = resolver.EffectiveAgentID(agentID)
		if err != nil {
			return "", protoerrors.Newf(protoerrors.CodeInvalidRequest, "method %q: %s", method, agentNotResolvableMsg)
		}
	} else if agents != nil && effectiveID == "" {
		return "", protoerrors.Newf(protoerrors.CodeInvalidRequest, "method %q: %s", method, agentNotResolvableMsg)
	}
	if reach == nil {
		return "", protoerrors.Newf(protoerrors.CodeScopeMismatch,
			"method %q: caller is not authorized for the effective agent", method)
	}
	if err := reach.AuthorizeAgentReach(ctx, effectiveID); err != nil {
		return "", protoerrors.Newf(protoerrors.CodeScopeMismatch,
			"method %q: caller is not authorized for the effective agent", method)
	}
	if agents == nil {
		return "", protoerrors.Newf(protoerrors.CodeInvalidRequest,
			"method %q: %s", method, agentNotResolvableMsg)
	}
	allowed, err := agents.ResolveAgent(ctx, id, effectiveID)
	if err != nil {
		if errors.Is(err, ErrAgentRetired) {
			return "", protoerrors.Newf(protoerrors.CodeAgentRetired,
				"method %q: agent is retired", method)
		}
		return "", protoerrors.Newf(protoerrors.CodeRuntimeError,
			"method %q: agent_id resolution failed: %v", method, err)
	}
	if !allowed {
		return "", protoerrors.Newf(protoerrors.CodeInvalidRequest,
			"method %q: %s", method, agentNotResolvableMsg)
	}
	return effectiveID, nil
}

// dispatchStart handles the `start` method: it spawns a foreground task
// via the tasks.TaskRegistry. A `start` request carries the
// identity triple (RunID is ignored — Spawn assigns the TaskID) and no
// steering scope (task creation is not a steering control).
//
// The method name is read from methods.MethodStart, never hardcoded —
// the single-source lint forbids a Protocol method string
// literal anywhere under internal/protocol/ outside the methods package
// (CLAUDE.md §8).
func (s *ControlSurface) dispatchStart(ctx context.Context, req any) (*types.StartResponse, error) {
	method := methods.MethodStart

	sr, ok := req.(*types.StartRequest)
	if !ok || sr == nil {
		return nil, protoerrors.Newf(protoerrors.CodeInvalidRequest,
			"method %q: request is nil or not a *types.StartRequest", string(method))
	}

	// Identity-mandatory at the Protocol edge (RFC §5.5). The triple is
	// required; RunID is not (a `start` mints the run).
	id := identity.Identity{
		TenantID:  sr.Identity.Tenant,
		UserID:    sr.Identity.User,
		SessionID: sr.Identity.Session,
	}
	if err := identity.Validate(id); err != nil {
		return nil, protoerrors.Newf(protoerrors.CodeIdentityRequired,
			"method %q: identity scope incomplete: %v", string(method), err)
	}

	// Caller-named-agent edge validation (reject-early, fail-closed).
	// Runs BEFORE the session ensurer and before Spawn: a request naming
	// an agent this Runtime cannot honour must not materialise a session
	// row OR a task. The refusal is never a fallback to the default agent
	// — a caller that named A and silently got B was told it succeeded,
	// which is the defect this field closes.
	effectiveAgentID, err := s.validateNamedAgent(ctx, string(method), id, sr.AgentID)
	if err != nil {
		return nil, err
	}

	// create-on-first-use. The session id is the per-request
	// session the client chose; a `start` on a not-yet-existing session
	// materialises its registry row BEFORE the task spawns so the
	// Console's sessions.list surfaces the conversation even if the first
	// turn later fails. A closed session id fails loud
	// (CodeReopenAfterClose-shaped — mapped to CodeInvalidRequest):
	// reopening a GC-reaped conversation is forbidden (RFC §6.9); the
	// client must pick a new session id for a new conversation. A surface
	// built without a SessionEnsurer skips this (control-only Runtime).
	if s.sessions != nil {
		if eerr := s.sessions.EnsureSession(ctx, id); eerr != nil {
			return nil, mapSessionEnsureError(string(method), eerr)
		}
	}

	// validate the optional per-attachment
	// disposition hints at the edge so the registry / run loop only
	// ever see canonical values. Each key must name an
	// InputArtifactIDs entry; each value must satisfy the disposition
	// grammar (planner.ParseDisposition — the planner-homed policy
	// core this Protocol field is a thin carrier over).
	if len(sr.InputArtifactDispositions) > 0 {
		attached := make(map[string]struct{}, len(sr.InputArtifactIDs))
		for _, artID := range sr.InputArtifactIDs {
			attached[artID] = struct{}{}
		}
		for artID, value := range sr.InputArtifactDispositions {
			if _, ok := attached[artID]; !ok {
				return nil, protoerrors.Newf(protoerrors.CodeInvalidRequest,
					"method %q: input_artifact_dispositions key %q names no input_artifact_ids entry", string(method), artID)
			}
			if _, perr := planner.ParseDisposition(value); perr != nil {
				return nil, protoerrors.Newf(protoerrors.CodeInvalidRequest,
					"method %q: input_artifact_dispositions[%q] = %q is not a valid disposition (want ref | inline | provider_native | tool:<name>)",
					string(method), artID, value)
			}
		}
	}

	// Output-schema edge validation (reject-early). A present-but-empty,
	// non-compiling, or over-cap output_schema is rejected with
	// CodeInvalidRequest BEFORE Spawn runs — no task is created that the
	// edge could have rejected. The schema is compiled via the ONE
	// compile implementation (planner.CompileOutputSchema) the driver run
	// start also uses (§13). The size cap keeps a hostile caller from
	// forcing a large compile on every start; the run-start compile
	// re-checks the persisted bytes.
	if sr.OutputSchema != nil {
		if len(sr.OutputSchema) > maxOutputSchemaBytes {
			return nil, protoerrors.Newf(protoerrors.CodeInvalidRequest,
				"method %q: output_schema exceeds the %d-byte cap (%d bytes)",
				string(method), maxOutputSchemaBytes, len(sr.OutputSchema))
		}
		if _, cErr := planner.CompileOutputSchema(sr.OutputSchema); cErr != nil {
			return nil, protoerrors.Newf(protoerrors.CodeInvalidRequest,
				"method %q: output_schema is not a valid JSON Schema: %v", string(method), cErr)
		}
	}

	// Caller-memory edge validation (reject-early). An over-cap,
	// explicitly-null or malformed `caller_memory` is refused with
	// CodeInvalidRequest BEFORE Spawn runs — no task is created that the
	// edge could have rejected. This is the ONLY bound on the content
	// class: see maxCallerMemoryBytes for why nothing downstream re-checks
	// it.
	if err := validateCallerMemory(string(method), sr.CallerMemory); err != nil {
		return nil, err
	}
	if err := validateExternalGrant(string(method), sr.ExternalGrant); err != nil {
		return nil, err
	}

	spawnCtx := ctx
	if s.reachAdmissions != nil {
		var admissionErr error
		spawnCtx, admissionErr = s.reachAdmissions.Admit(ctx, id, effectiveAgentID)
		if admissionErr != nil {
			return nil, protoerrors.Newf(protoerrors.CodeRuntimeError,
				"method %q: authenticate agent reach admission: %v", string(method), admissionErr)
		}
	}
	handle, err := s.tasks.Spawn(spawnCtx, tasks.SpawnRequest{
		Identity:                  identity.Quadruple{Identity: id},
		Kind:                      tasks.KindForeground,
		Description:               sr.Description,
		Query:                     sr.Query,
		Priority:                  sr.Priority,
		IdempotencyKey:            sr.IdempotencyKey,
		InputArtifactIDs:          sr.InputArtifactIDs,
		InputArtifactDispositions: sr.InputArtifactDispositions,
		OutputSchema:              sr.OutputSchema,
		AgentID:                   sr.AgentID,
		CallerMemory:              sr.CallerMemory,
		ExternalGrant:             append([]byte(nil), sr.ExternalGrant...),
	})
	if err != nil {
		return nil, mapTaskError(string(method), err)
	}

	return &types.StartResponse{
		TaskID:          string(handle.ID),
		Reused:          handle.Reused,
		ProtocolVersion: types.ProtocolVersion,
	}, nil
}

// dispatchControl handles the nine steering-control methods. It builds a
// steering.ControlEvent from the Protocol request and enqueues it on the
// run's steering.Inbox — the Inbox.Enqueue does the validation
// (RFC §6.3 payload bounds), the per-event scope check (steering.
// CheckScope), and the identity gate. Harbor does NOT re-implement any
// of that (CLAUDE.md §13 forbids a second validator); it constructs the
// event, hands it to Enqueue, and maps the steering sentinel onto a
// Protocol error code.
//
// Caller authority is read from the VERIFIED identity on ctx (placed
// there by the auth middleware), never from the request body. The body
// names which run the control targets — a routing key — but the caller's
// tenant and privilege tier come from the authenticated context. Trusting
// a body-supplied scope claim would let any caller assert admin against
// the steering control plane (a privilege escalation); failing closed on
// a missing verified identity is the CLAUDE.md §7 + §13 posture.
func (s *ControlSurface) dispatchControl(ctx context.Context, method methods.Method, req any) (*types.ControlResponse, error) {
	cr, ok := req.(*types.ControlRequest)
	if !ok || cr == nil {
		return nil, protoerrors.Newf(protoerrors.CodeInvalidRequest,
			"method %q: request is nil or not a *types.ControlRequest", string(method))
	}

	ctrlType, ok := methodToControlType[method]
	if !ok {
		// Unreachable: Dispatch already rejected non-canonical methods,
		// and MethodStart is routed away before here. Fail loud rather
		// than silently no-op (CLAUDE.md §5).
		return nil, protoerrors.Newf(protoerrors.CodeRuntimeError,
			"method %q: no steering control type mapping (Protocol-surface invariant violated)", string(method))
	}

	// Build the TARGET run quadruple from the request body. This is a
	// routing key — it names which run the control targets — not an
	// authority claim. The full quadruple (triple + run) is mandatory:
	// an incomplete quadruple fails closed here, before the steering
	// Registry is ever touched.
	q := identity.Quadruple{
		Identity: identity.Identity{
			TenantID:  cr.Identity.Tenant,
			UserID:    cr.Identity.User,
			SessionID: cr.Identity.Session,
		},
		RunID: cr.Identity.Run,
	}
	if err := identity.Validate(q.Identity); err != nil {
		return nil, protoerrors.Newf(protoerrors.CodeIdentityRequired,
			"method %q: identity scope incomplete: %v", string(method), err)
	}
	if q.RunID == "" {
		return nil, protoerrors.Newf(protoerrors.CodeIdentityRequired,
			"method %q: run id is required for a steering control", string(method))
	}

	// Read the VERIFIED caller identity from ctx. A request that has not
	// been through the auth middleware carries no identity — fail closed
	// rather than fall back to the (untrusted) request body.
	caller, ok := identity.From(ctx)
	if !ok {
		return nil, protoerrors.Newf(protoerrors.CodeIdentityRequired,
			"method %q: no verified caller identity on the request context", string(method))
	}
	if err := identity.Validate(caller); err != nil {
		return nil, protoerrors.Newf(protoerrors.CodeIdentityRequired,
			"method %q: verified caller identity is incomplete: %v", string(method), err)
	}

	// Derive the caller's steering privilege tier from the verified
	// identity + JWT scope claims — NEVER from cr.Identity.Scope (a
	// caller-supplied string). A caller with no authority over the
	// target run is rejected before the control reaches the inbox.
	scope, ok := deriveSteeringScope(ctx, caller, q)
	if !ok {
		return nil, protoerrors.Newf(protoerrors.CodeScopeMismatch,
			"method %q: the verified caller is not authorised to steer run %q", string(method), q.RunID)
	}

	// Enforce the per-control scope minimum (and the
	// cross-tenant-requires-admin rule) at the edge, BEFORE the inbox
	// Lookup. This is the SAME steering.CheckScope the inbox runs inside
	// Enqueue — calling it here is not a second validator (CLAUDE.md §13),
	// it is the one authoritative gate run one step earlier so a caller who
	// holds SOME authority over the run but not enough for this control
	// (e.g. the owning user submitting the admin-only PRIORITIZE) is
	// refused with a scope error instead of leaking whether the run exists
	// via a not-found. Enqueue runs CheckScope again — defence in depth for
	// any caller that reaches the inbox by another path.
	if err := steering.CheckScope(ctrlType, scope, caller.TenantID, q); err != nil {
		return nil, mapSteeringError(string(method), err)
	}

	// Look up the run's live inbox. A run with no inbox (never started,
	// or already ended) fails closed with CodeNotFound.
	if method == methods.MethodResume && s.pauseCoordinator != nil {
		if token, ok := cr.Payload["token"].(string); ok && token != "" {
			status, statusErr := pauseresume.StatusForIdentity(ctx, s.pauseCoordinator, pauseresume.Token(token), caller, q.RunID)
			if statusErr == nil && status.Reason == pauseresume.ReasonConstraintsConflict && !status.Available {
				return nil, protoerrors.Newf(protoerrors.CodeRestartUnavailable,
					"method %q: exact restart redrive is unavailable", string(method))
			}
			if statusErr != nil && !errors.Is(statusErr, pauseresume.ErrPauseNotFound) {
				return nil, mapSteeringError(string(method), statusErr)
			}
		}
	}
	inbox, err := s.steering.Lookup(q)
	if err != nil {
		return nil, mapSteeringError(string(method), err)
	}

	// Construct the control event and hand it to Enqueue. Enqueue runs
	// the full gauntlet: identity match, canonical-type check,
	// CheckScope (per-event scope + cross-tenant-requires-admin),
	// ValidatePayload (the RFC §6.3 bounds). Any failure surfaces as a
	// steering sentinel, mapped onto a Protocol error code.
	ev := steering.ControlEvent{
		Type:         ctrlType,
		Identity:     q,
		CallerScope:  scope,
		CallerTenant: caller.TenantID, // the VERIFIED caller tenant — drives steering.CheckScope's cross-tenant-requires-admin gate. A control whose target run lives in a different tenant only passes when the caller holds the admin scope.
		Payload:      cr.Payload,
		EventID:      cr.EventID,
	}
	if err := inbox.Enqueue(ev); err != nil {
		return nil, mapSteeringError(string(method), err)
	}

	return &types.ControlResponse{
		Accepted:        true,
		Method:          string(method),
		ProtocolVersion: types.ProtocolVersion,
	}, nil
}

// deriveSteeringScope maps a verified caller onto the steering privilege
// tier for the run being steered. It reads ONLY the verified ctx identity
// + JWT scope claims and compares them against the target run — it never
// consults the request body, whose scope field is a caller-supplied
// claim. It fails closed: a caller who is neither an administrator nor
// the run's owning user gets no authority (ok=false), and dispatchControl
// rejects the control before it reaches the steering inbox. The contract
// holds for a non-admin token (a verified token without the admin scope)
// exactly as it does for an admin one — authority is the verified
// identity-vs-run comparison, never a token-carried tier field.
//
//   - admin JWT scope                  → steering.ScopeAdmin (sufficient
//     for every control type, cross-tenant included).
//   - verified (tenant,user) == run's  → steering.ScopeOwnerUser, which
//     by rank also satisfies the session-scoped controls (INJECT_CONTEXT,
//     USER_MESSAGE).
//   - otherwise                        → no authority (ok=false).
//
// # Why the session-scoped tier collapses into the owner tier here
//
// A session belongs to exactly one (tenant, user): a run is keyed by the
// full triple (tenant, user, session), so the principal authenticated
// into a run's session IS that run's owning user. Granting the
// session-scoped tier therefore requires a full-triple match, which is a
// subset of the (tenant, user) match that already earns the strictly
// higher owner tier — so a verified session participant is handed
// owner_user, and the distinct session-scoped tier (where session_user is
// strictly below owner_user) is never minted. That distinct tier would
// only be meaningful for multi-participant sessions — a capability the
// runtime does not have at V1 — so it stays reserved (the constant, the
// rank, and the per-control minimum in steering.CheckScope keep it
// defined for that future and for the admin/owner total order).
//
// # Why a session-id collision cannot escalate
//
// Session ids are client-chosen and not unique across users, so a
// same-tenant collision is expected: a verified token for
// (tenant-a, user-B, session-x) and a run owned by
// (tenant-a, user-A, session-x) share the session-id STRING but are
// different sessions. Collision safety is structural, not a special case:
// the only non-admin grant compares the user component too, so a verified
// principal whose user differs from the run owner's earns nothing — a
// bare (tenant, session) match never confers authority over another
// user's run. The collision token is rejected with CodeScopeMismatch
// before the control reaches the inbox.
//
// steering.CheckScope (run at the dispatchControl edge AND again inside
// Inbox.Enqueue) then enforces the per-control minimum and the
// cross-tenant-requires-admin rule on top of the tier this function
// returns — defence in depth, not a substitute.
func deriveSteeringScope(ctx context.Context, caller identity.Identity, run identity.Quadruple) (steering.Scope, bool) {
	if auth.HasScope(ctx, auth.ScopeAdmin) {
		return steering.ScopeAdmin, true
	}
	// The run owner is identified by (tenant, user). A verified principal
	// whose full triple (tenant, user, session) equals the run's is BOTH
	// the session participant and — in Harbor's single-participant session
	// model — the owner, so it is handed the strictly-higher owner tier by
	// this same branch. A principal whose user differs (a session-id
	// collision) falls through to "no authority".
	if caller.TenantID == run.TenantID && caller.UserID == run.UserID {
		return steering.ScopeOwnerUser, true
	}
	return "", false
}

// compile-time assertion: every steering-control method
// (IsControlMethod=true) has a steering.ControlType mapping. MethodStart,
// the streaming-events methods (MethodEventsSubscribe /
// MethodEventsAggregate), and the `search.*` cluster are NOT
// control methods and route through their own surfaces (the task
// registry for Start; the SSE handler + events-aggregate handler for
// the streaming-events two; the search dispatcher in
// internal/protocol/search.go for the search cluster) — IsControlMethod
// gates the exhaustive check so a new non-control method does NOT need
// a mapping. If a new steering-control method is added to
// internal/protocol/methods without a mapping here, this fails —
// keeping the Protocol method table and the steering bridge in lockstep.
func init() {
	for _, m := range methods.Methods() {
		if !methods.IsControlMethod(m) {
			// MethodStart, the streaming-events methods, the
			// search cluster, and any future non-control
			// method are routed elsewhere — no steering.ControlType is
			// expected for them.
			continue
		}
		if _, ok := methodToControlType[m]; !ok {
			panic(fmt.Sprintf("protocol: steering-control method %q has no steering.ControlType mapping — methodToControlType is out of sync with internal/protocol/methods", m))
		}
	}
}
