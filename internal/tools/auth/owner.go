package auth

// Owner is the (tenant, agent) reconcile-view tag stamped on a runtime-added
// MCP connection or a Protocol-installed OAuth provider. It mirrors the
// agent-config revision owner that already governs the entry (the agent config
// scope) and is used for EXACTLY one thing beyond that revision ownership:
// scoping the run-start reconcile VIEW so a run only ever touches its OWN
// owner's runtime-added entries — never boot-declared entries, never another
// owner's runtime-adds.
//
// Owner is deliberately NOT an isolation principal: agent_id is not part of the
// (tenant, user, session) isolation tuple, so the owner tag is never used as a
// dispatch key or a storage WHERE clause. Boot-declared infrastructure and the
// bare-name tool catalog stay process-global and deployment-shared: resolution
// and dispatch happen by bare name across every session, and boot servers stay
// visible to every session's read surface. A shared runtime therefore TRUSTS
// its co-tenant admins for runtime-added connection / provider names — a name
// collision fails loud rather than silently cross-serving — and a deployment
// that needs hard isolation of runtime-added tools runs one runtime per tenant,
// which then gets full isolation for free (one tenant; everything in the global
// catalog is theirs).
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
}

// IsZero reports whether o is the zero (boot-declared / untagged) owner. A
// zero-owner entry is never enumerated by the owner-scoped reconcile view.
func (o Owner) IsZero() bool { return o.Tenant == "" && o.Agent == "" }
