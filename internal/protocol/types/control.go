package types

import "encoding/json"

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
//
// admin-impersonation extension.
// The three optional fields Actor / Requester / Impersonating carry the
// admin-on-behalf-of-user triplet so an operator with the `auth.ScopeAdmin`
// claim can steer a run "on behalf of" another `(tenant, user, session)`
// while every request still carries BOTH the requesting admin's verified
// identity AND the impersonated identity for audit. An IdentityScope
// MAY carry zero impersonation fields (today's behaviour, the verified
// JWT identity IS the request identity) OR all three set (admin acting
// on behalf of a target user). The runtime rejects any other shape loudly
// at the Protocol edge — never silently degrades.
type IdentityScope struct {
	// Tenant / User / Session are the mandatory isolation triple. An
	// empty component fails the request closed at the Protocol edge.
	// When impersonation is in use, these fields carry the IMPERSONATED
	// identity — the identity the run executes under (matches the
	// Impersonating triple component-for-component).
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
	// `admin`). It is trust-based until a later phase Protocol auth, exactly
	// as `events.Filter.Admin` is; the ControlSurface enforces the
	// per-method scope via the steering CheckScope. Ignored for
	// `start` (task creation is not a steering control).
	Scope string `json:"scope,omitempty"`

	// Actor is the verified admin identity at the request edge — the
	// identity whose JWT claim was validated by the middleware.
	// V1 invariant: Actor MUST equal the JWT's verified `(tenant, user,
	// session)` triple; the transport rejects a body claiming a different
	// Actor with CodeScopeMismatch. The Actor's audit trail ("admin X
	// impersonated user Y at time T") is what makes impersonation
	// accountable.
	Actor *IdentityScope `json:"actor,omitempty"`

	// Requester is the originating admin identity for delegated
	// impersonation chains (e.g. an admin acting on behalf of another
	// admin's audited request). At V1: Requester MUST equal Actor; the
	// field exists so post-V1 delegated impersonation does not require a
	// wire-shape break. The runtime rejects Requester != Actor with
	// CodeScopeMismatch.
	Requester *IdentityScope `json:"requester,omitempty"`

	// Impersonating is the target identity the run executes under. When
	// non-empty, MUST carry a complete `(tenant, user, session)` triple —
	// identity is mandatory; the impersonated triple is identity too.
	// Setting Impersonating gates on auth.ScopeAdmin on the verified JWT;
	// a non-admin request with Impersonating set is rejected with
	// CodeScopeMismatch before Dispatch runs.
	//
	// V1 semantics: the top-level Tenant/User/Session fields MUST equal
	// the Impersonating triple when impersonation is in use — the run
	// executes as the impersonated identity. The Actor field carries the
	// audit-visible record of WHO impersonated.
	Impersonating *IdentityScope `json:"impersonating,omitempty"`
}

// IsImpersonating reports whether this scope carries a non-empty
// Impersonating field. The transport edge gates impersonation behaviour
// off this predicate; downstream code SHOULD use it rather than checking
// the field directly so a future shape change (e.g. flattening the
// triplet) lands in one place.
func (s IdentityScope) IsImpersonating() bool {
	return s.Impersonating != nil
}

// StartRequest is the wire request for the `start` Protocol method — it
// asks the Runtime to spawn a new task / foreground run. It maps onto the
// tasks.TaskRegistry.Spawn surface.
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
	// InputArtifactIDs attach operator-uploaded
	// artifacts as multimodal inputs the run consumes on its first
	// planner turn. The runtime's per-MIME materializer routes each
	// id: `image/*` bytes inline as `ImagePart.DataURL`; everything
	// else stays an `ArtifactStub` the LLM routes through the tool
	// catalog. The Playground composer's chat-attach control plumbs
	// uploaded ids through this field. Empty is the text-only default.
	InputArtifactIDs []string `json:"input_artifact_ids,omitempty"`
	// InputArtifactDispositions carries the
	// OPTIONAL per-attachment disposition hint, keyed by an
	// InputArtifactIDs entry. Values: `ref` (emit an `ArtifactStub` +
	// `Fetch.Tool` hint — the runtime default for non-image MIMEs),
	// `inline` (DataURL inline; `image/*` only at V1.1),
	// `provider_native` (opt-in provider-side understanding;
	// degrades to `ref` with a logged notice until it ships), or
	// `tool:<name>` (force the named catalog tool). The hint is the
	// TOP precedence layer: hint > the agent's
	// `multimodal.disposition` config map > the runtime default
	// (`image/*` → inline, everything else → ref). A key that names
	// no InputArtifactIDs entry, or a value outside the grammar, is
	// rejected with CodeInvalidRequest. Omitted (the backward-
	// compatible default) defers entirely to the lower layers.
	InputArtifactDispositions map[string]string `json:"input_artifact_dispositions,omitempty"`
	// OutputSchema opts the task-shaped run into per-task structured
	// output: a JSON-Schema document (raw bytes) the completed task's
	// answer is validated against. When set, the completed task's
	// envelope carries the validated `answer_payload` (readable via
	// `tasks.get`'s `result_inline` and via a parent run's AwaitTask
	// observation); a schema-invalid answer after the correction budget
	// fails the task LOUD with the `output_invalid` terminal code, never
	// a schemaless success. Absent (the backward-compatible default) →
	// byte-identical wire shape and spawn behaviour.
	//
	// The schema is validated at the Protocol edge BEFORE the task
	// spawns: an empty, non-compiling, or over-cap (64 KiB) schema is
	// rejected with CodeInvalidRequest and no task is created. It is
	// compiled once more at driver run start (the compile the run
	// consumes) via the same single implementation.
	//
	// Streaming posture (mirrors the run-level structured-output
	// mechanism): a schema-constrained task SUPPRESSES assistant-content
	// and reasoning token deltas on the per-task streaming path
	// (`llm.completion.chunk`) — a validate-and-retry loop cannot retract
	// already-streamed tokens, so the validated answer arrives once, in
	// the envelope. Step boundaries and tool-dispatch events stream as
	// today. A documented behaviour choice, not a surprise.
	//
	// Idempotency: `start` dedupes on `(session, idempotency_key)` and
	// folds `output_schema` into the task's content identity — a genuine
	// retry (same body, same schema) returns the existing handle; a
	// REUSED key with a DIFFERENT schema is caller misuse and is rejected
	// loud as an idempotency conflict.
	OutputSchema json.RawMessage `json:"output_schema,omitempty"`
	// AgentID names which agent's CONFIGURATION the run executes under.
	//
	// OPTIONAL: an omitted field binds the runtime's configured default
	// agent exactly as before — byte-identical wire shape and run
	// behaviour. A non-empty value is accepted when it equals the
	// runtime's configured default agent id, OR when a config revision
	// exists for the caller's tenant under that id. Anything else is
	// REFUSED with CodeInvalidRequest at the Protocol edge, before a task
	// exists — never substituted with the default. A caller that named
	// agent A, silently got agent B, and was told it succeeded is the
	// defect this field closes, so the refusal is never a fallback.
	//
	// The refusal text is INDEPENDENT of why the id failed: an id
	// registered under another tenant and an id that never existed
	// produce the identical error, so the edge is not a cross-tenant
	// existence oracle.
	//
	// SCOPE — configuration only. The run's southbound tool provenance
	// (`_meta.agent_id`) and its RFC 8693 acting principal remain the
	// runtime's boot-derived value and are never influenced by this
	// field. The two agent-id carriers on a run are deliberately
	// distinct and must not be unified.
	AgentID string `json:"agent_id,omitempty"`
	// CallerMemory is caller-supplied content admitted into the run's
	// `<read_only_external_memory>` tier under the FIXED runtime-owned
	// `caller_supplied` map key. It is the ONE additive path for putting
	// retrieved, caller-held content in front of the model; the tier it
	// lands in ships a five-line anti-prompt-injection preamble whose
	// entire premise is that its contents are hostile.
	//
	// It COMPOSES with the runtime's own retrieval rather than replacing
	// it: the External tier renders a map, the runtime writes its own
	// keys, and this field contributes exactly one more. The caller names
	// NO key — it supplies only the value — so a caller can never shadow,
	// rename or displace a runtime-written key, and a runtime producer can
	// never collide with a caller.
	//
	// SCOPE — it can reach exactly one prompt position. It never reaches
	// the trusted system-prompt spine (that is what SystemPromptOverride
	// does, and why this field exists), and it never writes the
	// conversation-memory tier, which is a claim about the session's
	// stored turns that only the runtime may make.
	//
	// Any valid JSON value is accepted — an object, an array, a string, a
	// number. An explicit `null` is REFUSED rather than treated as absent:
	// a caller that believes its memory reached the model when it did not
	// is the silent degradation this field exists to avoid. A document
	// larger than the 32 KiB Protocol-edge cap is REFUSED with
	// CodeInvalidRequest before a task exists, and the refusal names this
	// field. Absent (the backward-compatible default) → byte-identical
	// wire shape and run behaviour.
	//
	// THE CAP IS A RESOURCE BOUND AND WIRE-SIZE GUARD, NOT A SECURITY
	// BOUNDARY, and nothing may be inferred from it about how much content
	// a caller can put in front of the model — the same principal can send
	// substantially more through `Query` (uncapped below the transport
	// envelope, landing in the unframed conversation position) or through
	// `agent_config.session.set_user_prompt` (claim-free, 1 MiB body,
	// landing INSIDE the system prompt). What contains this payload is the
	// tier it lands in, not its size. The reasoning is on
	// `maxCallerMemoryBytes`.
	//
	// NEGOTIATE BEFORE YOU RELY ON IT. A Runtime that predates this field
	// discards the member and answers success — the run then proceeds
	// without the memory the caller believes it supplied. This transport
	// now decodes strictly, so such a member is refused loudly from here
	// forward, but that cannot reach an already-deployed older Runtime. A
	// client checks `VersionHandshake.Accepts(CapCallerMemory)` (or the
	// `runtime.info.capabilities` list) and treats the capability's ABSENCE
	// as "unsupported" rather than discovering the loss after the fact.
	//
	// Idempotency: `caller_memory` folds into the task's content identity,
	// so a reused idempotency key carrying DIFFERENT memory is a loud
	// conflict rather than a silent adoption of the first payload.
	//
	// AT REST: the payload is PERSISTED on the task record, which the
	// StateStore writes to disk. It goes through the audit redactor on the
	// way in — the same one Description and Query take, so the three
	// caller-controlled fields on a task are consistent rather than one of
	// them silently raw — and the redacted form is what both the store and
	// the prompt see (exactly as for Query). A payload that is not valid
	// JSON is refused loud and persists nothing.
	//
	// RESIDUAL RISK, stated rather than implied: that redactor is a
	// PATTERN redactor, not a sanitiser. It replaces secret-shaped KEYS
	// (api_key / password / secret / token / cookie / authorization) and
	// inline `Bearer …` / `Basic …` VALUES anywhere in the document, and it
	// does nothing else — it does not detect PII, it does not detect a
	// credential that looks like ordinary prose, and it cannot make hostile
	// text safe. The untrusted prompt framing remains the mitigation for
	// prompt injection. An operator who pipes third-party content through
	// `caller_memory` still has a data-leakage path no prompt wrapper and
	// no pattern redactor closes.
	CallerMemory json.RawMessage `json:"caller_memory,omitempty"`
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
// ControlRequest and lets the Inbox.Enqueue do the validation,
// the RFC §6.3 payload-bounds enforcement, and the per-event scope check
// does not re-implement any of that (CLAUDE.md §13 forbids a
// second validator).
type ControlRequest struct {
	// Identity is the request's identity scope. The full quadruple
	// (triple + Run) is mandatory — a steering control targets a
	// specific run's inbox. Scope is the caller's steering scope claim.
	Identity IdentityScope `json:"identity"`
	// Payload is the method-specific control payload. May be nil — a
	// bare `cancel` / `pause` carries no payload. The steering
	// ValidatePayload enforces the RFC §6.3 bounds (depth ≤ 6, ≤ 64
	// keys, ≤ 50 list items, ≤ 4096 chars/string, ≤ 16 KiB total) at the
	// edge; an oversize payload fails the request closed.
	Payload map[string]any `json:"payload,omitempty"`
	// EventID is the caller-supplied idempotency / correlation key
	// (ULID-shaped). Optional — the control-history dedupe uses
	// it. Empty is permitted.
	EventID string `json:"event_id,omitempty"`
}

// ControlResponse is the wire response shared by the nine steering-control
// Protocol methods. It is a minimal acknowledgement: the control was
// validated, scope-checked, and enqueued on the run's inbox. The control's
// *effect* on the run (the redirected goal taking hold, the pause
// blocking the loop, the approval advancing it) is observed via the
// canonical event stream (`control.received` / `control.applied`),
// NOT synchronously in this response — a richer synchronous response
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
