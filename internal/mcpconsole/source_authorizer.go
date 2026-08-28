package mcpconsole

import (
	"context"
	"errors"
	"fmt"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/identity"
	protocolauth "github.com/hurtener/Harbor/internal/protocol/auth"
	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/auth"
	mcp "github.com/hurtener/Harbor/internal/tools/drivers/mcp"
)

// UserConfigReader is the narrow durable desired-state read seam used to
// decide whether a user-owned MCP source is currently selected. The concrete
// agent-config Registry satisfies this interface, as do test drivers and
// other runtime adapters. Keeping the seam narrow makes the source authority
// a projection over the existing ConfigScopeUser revision, not a second
// lifecycle or grant store.
type UserConfigReader interface {
	Active(context.Context, identity.Quadruple, string, agentcfg.ConfigScope) (agentcfg.Revision, bool, error)
}

// ErrSourceAuthorityUnavailable means a user-owned source was observed but no
// durable ConfigScopeUser reader was wired. User-owned sources fail closed in
// this posture; owner filtering alone is only a cache privacy check and is not
// an authorization decision.
var ErrSourceAuthorityUnavailable = errors.New("mcpconsole: user-source authority unavailable")

// SourceAuthorizer is the single effective-source admission helper consumed
// by the registry read adapter, MCP Apps resource/callback paths, and render
// admission. The MCP registry supplies only the live owner/logical metadata;
// the current ConfigScopeUser revision remains the sole personal authority.
// The value is immutable after construction and safe for concurrent reuse.
type SourceAuthorizer struct {
	registry *mcp.Registry
	config   UserConfigReader
	reach    protocolauth.AgentReachAuthorizer
}

// NewSourceAuthorizer composes a live MCP registry with the durable
// ConfigScopeUser reader. A nil config is intentionally retained: ownerless
// sources remain compatible, while user-owned sources fail closed with
// ErrSourceAuthorityUnavailable.
func NewSourceAuthorizer(registry *mcp.Registry, config UserConfigReader, reaches ...protocolauth.AgentReachAuthorizer) *SourceAuthorizer {
	reach := protocolauth.AgentReachAuthorizer(protocolauth.NewAgentReachAuthorizer())
	if len(reaches) > 0 && reaches[0] != nil {
		reach = reaches[0]
	}
	return &SourceAuthorizer{registry: registry, config: config, reach: reach}
}

// Visible reports whether source is admitted for the verified caller and the
// optional effective agent. Unknown or ownerless sources are visible to the
// caller after the registry's identity check. A user-owned source is visible
// only when its exact logical pair is present in the caller's current
// ConfigScopeUser revision. The session id is deliberately absent from this
// durable pair match.
func (a *SourceAuthorizer) Visible(ctx context.Context, source tools.ToolSourceID, effectiveAgent string) (bool, error) {
	if err := sourceIdentity(ctx); err != nil {
		return false, err
	}
	if a == nil || a.registry == nil {
		return false, ErrSourceAuthorityUnavailable
	}
	owner, logical, registered, err := a.registry.SourceAccess(ctx, source)
	if err != nil {
		// SourceAccess deliberately collapses a foreign user-owned source
		// into not-found. Preserve that non-oracle result here.
		return false, nil
	}
	if !registered {
		return true, nil
	}
	return a.VisibleRegistration(ctx, source, owner, logical, effectiveAgent)
}

// VisibleRegistration is the snapshot form used by paginated registry reads.
// The owner and logical name must come from one Registry snapshot; callers
// must not synthesize either value. It shares the exact same ConfigScopeUser
// pair admission as Visible.
func (a *SourceAuthorizer) VisibleRegistration(ctx context.Context, _ tools.ToolSourceID, owner auth.Owner, logical, effectiveAgent string) (bool, error) {
	if err := sourceIdentity(ctx); err != nil {
		return false, err
	}
	if owner.User == "" {
		return true, nil
	}
	id, _ := identity.From(ctx)
	if owner.Tenant != id.TenantID || owner.User != id.UserID {
		return false, nil
	}
	if effectiveAgent != "" && owner.Agent != effectiveAgent {
		return false, nil
	}
	if a == nil || a.reach == nil || a.reach.AuthorizeAgentReach(ctx, owner.Agent) != nil {
		return false, nil
	}
	if a == nil || a.config == nil {
		return false, ErrSourceAuthorityUnavailable
	}
	active, has, err := a.config.Active(ctx, identity.Quadruple{Identity: id}, owner.Agent, agentcfg.ConfigScopeUser)
	if err != nil {
		return false, fmt.Errorf("read active user config: %w", err)
	}
	if !has {
		return false, nil
	}
	pairs, err := active.Payload.EffectiveSignedOAuthMCPPairs()
	if err != nil {
		return false, fmt.Errorf("read user connection pairs: %w", err)
	}
	for _, pair := range pairs {
		if pair.Connection.Name == logical && pair.OwnerAgentID == owner.Agent && pair.OwnerUserID == owner.User {
			return true, nil
		}
	}
	return false, nil
}

// sourceIdentity keeps every source admission fail-closed before it reaches
// either the live registry or the durable config reader.
func sourceIdentity(ctx context.Context) error {
	id, ok := identity.From(ctx)
	if !ok || id.TenantID == "" || id.UserID == "" || id.SessionID == "" {
		return mcp.ErrIdentityMissing
	}
	return nil
}
