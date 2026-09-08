package auth

// Owner is immutable server-derived ownership metadata for a runtime source.
// Scope exhaustively distinguishes boot infrastructure from tenant-owned
// agent and user registrations. It is not client-selected authority.
type Owner struct {
	// Tenant is the owning tenant id (the verified caller's tenant at attach /
	// install time).
	Tenant string
	// Agent is the owning agent id — the agent whose agent-config revision the
	// runtime-added entry belongs to. Part of the owned-source admission boundary.
	Agent string
	// User is the owning verified user id for a user-scoped registration. It is
	// empty for operator/agent-scoped registrations and is never a dispatch key.
	User string
}

// IsZero reports whether o is the zero (boot-declared / untagged) owner. A
// zero-owner entry is never enumerated by the owner-scoped reconcile view.
func (o Owner) IsZero() bool { return o.Tenant == "" && o.Agent == "" && o.User == "" }

// SourceScope is the exhaustive ownership class of a live source.
type SourceScope string

const (
	// ScopeBootGlobal identifies deliberately ownerless boot infrastructure.
	ScopeBootGlobal SourceScope = "boot_global"
	// ScopeTenantAgent identifies a source owned by one tenant and agent.
	ScopeTenantAgent SourceScope = "tenant_agent"
	// ScopeTenantUser identifies a source additionally owned by one user.
	ScopeTenantUser SourceScope = "tenant_user"
	// ScopeInvalid rejects incomplete ownership metadata.
	ScopeInvalid SourceScope = "invalid"
)

// Scope derives the discriminator from the complete immutable tuple, avoiding
// a second stored authority field that could disagree with the owner.
func (o Owner) Scope() SourceScope {
	if o.IsZero() {
		return ScopeBootGlobal
	}
	if o.Tenant == "" || o.Agent == "" {
		return ScopeInvalid
	}
	if o.User == "" {
		return ScopeTenantAgent
	}
	return ScopeTenantUser
}

// AllowsPrincipal is the preliminary tenant/user check. Effective agent reach
// and current durable selection must still be checked by the caller.
func (o Owner) AllowsPrincipal(tenant, user string) bool {
	switch o.Scope() {
	case ScopeBootGlobal:
		return true
	case ScopeTenantAgent:
		return o.Tenant == tenant
	case ScopeTenantUser:
		return o.Tenant == tenant && o.User == user
	default:
		return false
	}
}
