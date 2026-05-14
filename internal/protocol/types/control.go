package types

// IdentityScope is the flat wire identity a Protocol task-control request
// carries. It is the wire projection of the runtime's identity quadruple
// `(tenant, user, session, run)` plus the caller's steering scope claim —
// flat strings, never a re-export of `identity.Quadruple` (a Protocol type
// that mapped 1:1 onto an internal Go struct would be the RFC §5.1
// reject-on-sight smell). The protocol.ControlSurface translates an
// IdentityScope into the runtime's `identity.Quadruple` + `steering.Scope`
// at the edge.
//
// Identity is mandatory (CLAUDE.md §6 rule 9, RFC §5.5: "the Protocol
// rejects any request without an identity scope"). The ControlSurface
// fails closed on an incomplete triple — there is no identity-downgrading
// knob.
type IdentityScope struct {
	// Tenant / User / Session are the mandatory isolation triple. An
	// empty component fails the request closed at the Protocol edge.
	Tenant  string `json:"tenant"`
	User    string `json:"user"`
	Session string `json:"session"`
	// Run is the per-execution scope inside a session. Mandatory for the
	// nine steering-control methods (they target a specific run's inbox);
	// optional for `start` (a `start` request mints a new run / task, so
	// it carries no pre-existing run id).
	Run string `json:"run,omitempty"`
	// Scope is the caller's steering scope claim — one of the three
	// canonical steering scopes (`session_user` / `owner_user` /
	// `admin`). It is trust-based until Phase 61 Protocol auth, exactly
	// as `events.Filter.Admin` is; the ControlSurface enforces the
	// per-method scope via the Phase 52 steering CheckScope. Ignored for
	// `start` (task creation is not a steering control).
	Scope string `json:"scope,omitempty"`
}

// StartRequest is the wire request for the `start` Protocol method — it
// asks the Runtime to spawn a new task / foreground run. It maps onto the
// Phase 20 tasks.TaskRegistry.Spawn surface.
type StartRequest struct {
	// Identity is the request's identity scope. The triple is mandatory;
	// Run is ignored (a `start` mints the run). Scope is ignored.
	Identity IdentityScope `json:"identity"`
	// Query is the user-facing query that starts the run. Optional —
	// some runs are kicked off without a natural-language query.
	Query string `json:"query,omitempty"`
	// Description is an operator-facing description of the task.
	// Optional.
	Description string `json:"description,omitempty"`
	// Priority is the task's initial scheduling priority. Zero is the
	// default priority.
	Priority int `json:"priority,omitempty"`
	// IdempotencyKey, when non-empty, deduplicates the spawn: a second
	// `start` with the same key (namespaced by session) returns the
	// existing task handle with Reused=true. Empty disables dedup.
	IdempotencyKey string `json:"idempotency_key,omitempty"`
}

// StartResponse is the wire response for the `start` Protocol method.
type StartResponse struct {
	// TaskID is the runtime-assigned task identifier for the spawned
	// (or, on an idempotency-key match, the existing) task.
	TaskID string `json:"task_id"`
	// Reused is true when an IdempotencyKey match returned an existing
	// task rather than spawning a fresh one.
	Reused bool `json:"reused"`
	// ProtocolVersion echoes the Protocol version the Runtime answered
	// under so a client can detect a version skew.
	ProtocolVersion string `json:"protocol_version"`
}

// ControlRequest is the wire request shared by the nine steering-control
// Protocol methods (`cancel`, `pause`, `resume`, `redirect`,
// `inject_context`, `approve`, `reject`, `prioritize`, `user_message`).
// The method name selects which steering ControlType the surface
// constructs; the Payload carries the method-specific arguments (the
// `goal` for redirect, the `message` for user_message, the `priority`
// for prioritize, `hard` for a hard cancel, etc.).
//
// The ControlSurface constructs a steering.ControlEvent from a
// ControlRequest and lets the Phase 52 Inbox.Enqueue do the validation,
// the RFC §6.3 payload-bounds enforcement, and the per-event scope check
// — Phase 54 does not re-implement any of that (CLAUDE.md §13 forbids a
// second validator).
type ControlRequest struct {
	// Identity is the request's identity scope. The full quadruple
	// (triple + Run) is mandatory — a steering control targets a
	// specific run's inbox. Scope is the caller's steering scope claim.
	Identity IdentityScope `json:"identity"`
	// Payload is the method-specific control payload. May be nil — a
	// bare `cancel` / `pause` carries no payload. The Phase 52
	// ValidatePayload enforces the RFC §6.3 bounds (depth ≤ 6, ≤ 64
	// keys, ≤ 50 list items, ≤ 4096 chars/string, ≤ 16 KiB total) at the
	// edge; an oversize payload fails the request closed.
	Payload map[string]any `json:"payload,omitempty"`
	// EventID is the caller-supplied idempotency / correlation key
	// (ULID-shaped). Optional — Phase 53's control-history dedupe uses
	// it. Empty is permitted.
	EventID string `json:"event_id,omitempty"`
}

// ControlResponse is the wire response shared by the nine steering-control
// Protocol methods. It is a minimal acknowledgement: the control was
// validated, scope-checked, and enqueued on the run's inbox. The control's
// *effect* on the run (the redirected goal taking hold, the pause
// blocking the loop, the approval advancing it) is observed via the
// canonical event stream (`control.received` / `control.applied`, Phase
// 53), NOT synchronously in this response — a richer synchronous response
// would couple the Protocol edge to the run loop's step timing.
type ControlResponse struct {
	// Accepted is true when the control event was validated, scope-checked,
	// and enqueued on the run's steering inbox. A false Accepted is never
	// returned — a rejected control surfaces as a *protocol.Error from
	// Dispatch, not an Accepted=false response.
	Accepted bool `json:"accepted"`
	// Method echoes the Protocol method name the control was submitted
	// under (`cancel`, `pause`, …) so a client correlating async
	// responses can match them up.
	Method string `json:"method"`
	// ProtocolVersion echoes the Protocol version the Runtime answered
	// under so a client can detect a version skew.
	ProtocolVersion string `json:"protocol_version"`
}
