package auth

// Owner is the (tenant, agent) reconcile-view tag stamped on a runtime-added
// MCP connection or a Protocol-installed OAuth provider. It mirrors the
// agent-config revision owner that already governs the entry (the agent config
// scope) and is used for EXACTLY one thing beyond that revision ownership:
// scoping the run-start reconcile VIEW so a run only ever touches its OWN
// owner's runtime-added entries — never boot-declared entries, never another
// owner's runtime-adds.
//
// User optionally extends the physical owner for a user-scoped registration.
// Agent is not part of the (tenant, user, session) isolation tuple, so the
// owner tag is not a dispatch key. It is an exact ownership and projection
// tag for runtime-added state. Boot-declared infrastructure and the bare-name
// tool catalog stay process-global and deployment-shared: resolution and
// dispatch happen by bare name across every session, and boot servers stay
// visible to every session's read surface. A same-name registration owned by a
// different full owner is rejected rather than silently cross-serving.
//
// A zero Owner denotes a boot-declared / untagged entry, which the reconcile
// view never enumerates.
type Owner struct {
	// Tenant is the owning tenant id (the verified caller's tenant at attach /
	// install time).
	Tenant string
	// Agent is the owning agent id — the agent whose agent-config revision the
	// runtime-added entry belongs to. Registration metadata, never an isolation
	// key.
	Agent string
	// User is the owning verified user id for a user-scoped registration. It is
	// empty for operator/agent-scoped registrations and is never a dispatch key.
	User string
}

// IsZero reports whether o is the zero (boot-declared / untagged) owner. A
// zero-owner entry is never enumerated by the owner-scoped reconcile view.
func (o Owner) IsZero() bool { return o.Tenant == "" && o.Agent == "" && o.User == "" }
