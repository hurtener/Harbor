package protocol

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/adminwrite"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
)

// setdiscoveryorigins.go — the admin-scoped
// `agent_config.set_mcp_discovery_origins` control method: a FULL-REPLACE write
// of a runtime-added MCP connection's OAuth-discovery cross-origin allow-list.
//
// The allow-list is the input the OAuth-requirement discovery walker consults
// before fetching a cross-origin authorization-server-metadata document; before
// this verb it existed only as the restart-required boot yaml field, unreachable
// to a Protocol-driven consumer. This verb makes it a first-class, revisioned,
// diffable, rollback-able field AND applies it to the live MCP registry so the
// very next discovery uses it.
//
// # Full-replace, revision + live apply, fail-closed audit (one unit)
//
// The write records a NEW revision whose matched connection descriptor carries
// the replacement allow-list, carrying EVERY sibling section forward (the
// rebuild-completeness guard). It THEN applies the allow-list to the
// process-global bare-name live registry (the registry is never re-keyed by
// identity — identity is mandatory for AUTHORIZATION, not keying) and, on
// revoke, prunes the recorded requirement's now-unallowed authorization-server
// entries. The admin-scope audit event and the mutation ship as ONE fail-closed
// unit ([adminwrite.Apply]): on audit-emit failure the mutation is COMPENSATED
// (the live allow-list is restored AND the active pointer is rolled back to the
// pre-write revision) so an un-auditable write is never left observably applied.
//
// # Governs the caller's own revisioned state only (four distinct loud errors)
//
// A boot-declared (yaml) name fails with ErrBootDeclaredConnection (edit yaml +
// restart) — checked on EVERY path, before any revision read, because the guard
// is a property of the NAME: a caller whose own revision also declares that name
// reaches the same refusal. An unknown name (not in the active revision's
// connections) fails loud with ErrConnectionNotFound; a stdio-transport
// connection fails with ErrDiscoveryOriginsNotHTTP (a stdio server has no HTTP
// 401 edge — there is no discovery walk to allow); a name attached live under a
// DIFFERENT (tenant, agent) owner fails with ErrConnectionOwnerMismatch (→ 403)
// and the accompanying revision write is rolled back. None leaves a recorded
// revision or an emitted event.
//
// # Live on rollback via the owner-scoped reconcile (not here)
//
// A rollback / set_revision past a grant takes effect on the live registry
// through the run-start allowance-reconcile leg (owner-scoped), which
// re-derives each of the owner's connections' allow-list from the CURRENT
// revision and re-applies it — a full idempotent re-prune. This verb writes the
// direct-write path; the reconcile leg is the rollback path (one mechanism, two
// triggers).

// DiscoveryOriginApplier is the driver-agnostic seam the discovery-allowance
// write (and the run-start allowance-reconcile) use to apply a connection's
// OAuth-discovery cross-origin allow-list to the LIVE MCP registry. The
// concrete (wired at the cmd/harbor + devstack boundary) delegates to the
// process-global bare-name registry's SetOAuthDiscoveryOrigins mutator; keeping
// it an injected interface preserves this package's §4.4 boundary (no concrete
// MCP driver import here).
type DiscoveryOriginApplier interface {
	// SetOAuthDiscoveryOrigins FULL-REPLACES the named connection's allow-list on
	// the live registry and returns the prior set so the caller computes the
	// granted / revoked delta. Identity-mandatory for authorization (the registry
	// stays bare-name). A revoke also prunes the recorded requirement's
	// now-unallowed authorization-server entries.
	//
	// (tenant, agentID) is the caller's resolved OWNER — the same (tenant, agent)
	// pair the ProviderInstaller seam carries, and the same tag the attach path
	// stamps on the live registration. The replacement lands only on a
	// registration carrying that tag, so the write applies to the caller's OWN
	// connection; a name owned by someone else is refused with
	// ErrConnectionOwnerMismatch, and a name the owner declares but has not
	// attached yet degrades to ErrDiscoveryTargetNotLive.
	SetOAuthDiscoveryOrigins(ctx context.Context, tenant, agentID, name string, origins []string) (prev []string, err error)
}

// ErrDiscoveryOriginsNotHTTP — set_mcp_discovery_origins named a
// stdio-transport connection. A stdio server speaks over a subprocess pipe with
// no HTTP 401 challenge and no discovery walk, so an allow-list is meaningless
// — refused, never stored (→ 400).
var ErrDiscoveryOriginsNotHTTP = errors.New("agentcfg/protocol: connection is a stdio transport — OAuth-discovery origins apply only to http connections")

// ErrDiscoveryTargetNotLive — the named connection EXISTS in the active
// revision but is not attached in the live MCP registry (its server was never
// reached, or it is awaiting the next run-start reconcile). The applier adapter
// translates the driver's server-not-found into this sentinel so the setter can
// DEGRADE — record the allowance in the revision with applied_live=false, so the
// run-start allowance-reconcile applies it once the server comes online —
// instead of failing the write and rolling the revision back. This matches the
// nil-applier path (revision-only) and the sibling revision-only verbs. It is
// DISTINCT from ErrConnectionNotFound (the connection is absent from the
// revision entirely, which still fails loud).
var ErrDiscoveryTargetNotLive = errors.New("agentcfg/protocol: connection not attached in the live MCP registry — allowance recorded in the revision, applied on next reconcile")

// ErrConnectionOwnerMismatch — the named connection is attached in the live MCP
// registry under a DIFFERENT (tenant, agent) owner than the caller's, or is
// boot-declared (untagged). A live connection write applies to the caller's OWN
// registration; a name owned elsewhere is refused as an authorization failure
// (→ 403 / CodeScopeMismatch) and the accompanying revision write is rolled
// back, so the call leaves no observable effect. It is DISTINCT from
// ErrDiscoveryTargetNotLive (the caller owns the declaration but the server is
// not attached yet, which degrades to a revision-only write).
var ErrConnectionOwnerMismatch = errors.New("agentcfg/protocol: connection is registered to a different owner — a live connection write applies to the caller's own connection")

// SetMCPDiscoveryOrigins FULL-REPLACES a runtime-added MCP connection's
// OAuth-discovery cross-origin allow-list. See the file doc for the full
// contract (revision + owner-scoped live apply + revoke-prune, the four
// distinct loud errors, the fail-closed audit, the rollback reconcile path).
func (s *Service) SetMCPDiscoveryOrigins(ctx context.Context, req prototypes.AgentConfigSetMCPDiscoveryOriginsRequest) (prototypes.AgentConfigSetMCPDiscoveryOriginsResponse, error) {
	if err := ctx.Err(); err != nil {
		return prototypes.AgentConfigSetMCPDiscoveryOriginsResponse{}, err
	}
	id, err := identityFromScope(req.Identity, req.AgentID)
	if err != nil {
		return prototypes.AgentConfigSetMCPDiscoveryOriginsResponse{}, err
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		return prototypes.AgentConfigSetMCPDiscoveryOriginsResponse{}, fmt.Errorf("%w: name is empty", ErrInvalidConnection)
	}
	// Validate + normalise EVERY origin through the shared validator (one
	// implementation, two call sites) BEFORE any state change — a malformed
	// entry fails loud with a typed error naming the entry, nothing persisted.
	origins, oerr := normalizeDiscoveryOrigins(req.AllowedOrigins)
	if oerr != nil {
		return prototypes.AgentConfigSetMCPDiscoveryOriginsResponse{}, oerr
	}
	// Boot wins on EVERY path. A boot-declared (yaml) server is not revisioned
	// state, so the verb refuses the name outright — whether or not the caller's
	// own active revision also declares a connection under it. The guard is a
	// property of the NAME, so it is evaluated here, before any revision read,
	// rather than inside a not-declared branch: a revision that declares the same
	// name reaches exactly the same refusal, and nothing is read or written on
	// the way to it. Edit the yaml + restart.
	if _, boot := s.bootDeclaredMCPServers[name]; boot {
		return prototypes.AgentConfigSetMCPDiscoveryOriginsResponse{}, fmt.Errorf("%w: %q", ErrBootDeclaredConnection, name)
	}
	q := identity.Quadruple{Identity: id}
	// The live registry applier reads the identity triple from ctx for
	// authorization (the registry stays bare-name — identity is for auth, not
	// keying). Stamp the verified triple onto ctx so the applier passes its
	// requireIdentity gate.
	applyCtx, idErr := identity.With(ctx, id)
	if idErr != nil {
		return prototypes.AgentConfigSetMCPDiscoveryOriginsResponse{}, idErr
	}

	// Serialise the per-agent read-modify-write against concurrent section edits.
	defer s.lockAgent(id.TenantID, req.AgentID)()

	active, hasActive, err := s.registry.Active(ctx, q, req.AgentID, agentcfg.ConfigScopeAgent)
	if err != nil {
		return prototypes.AgentConfigSetMCPDiscoveryOriginsResponse{}, err
	}
	var prior []agentcfg.MCPConnectionDescriptor
	if hasActive {
		prior = active.Payload.ConnectionDescriptors()
	}
	desc, found := findConnection(prior, name)
	if !found {
		// A name absent from the caller's active revision fails loud; no revision
		// is recorded. (The boot-declared case is refused earlier, on every path.)
		return prototypes.AgentConfigSetMCPDiscoveryOriginsResponse{}, fmt.Errorf("%w: %q", ErrConnectionNotFound, name)
	}
	if desc.Transport != agentcfg.MCPTransportHTTP {
		return prototypes.AgentConfigSetMCPDiscoveryOriginsResponse{}, fmt.Errorf("%w: %q", ErrDiscoveryOriginsNotHTTP, name)
	}

	// The pre-write active revision id — the compensation target. It always
	// exists (the connection is in the active revision, so hasActive is true).
	prevActiveRevID := active.RevisionID

	// Build the replacing revision: the matched descriptor's allow-list becomes
	// `origins`, every other descriptor and every sibling section carried forward
	// (the rebuild-completeness guard).
	payload := s.rebuildWithDiscoveryOrigins(active, hasActive, prior, name, origins)

	var (
		rev      agentcfg.Revision
		prevLive []string
	)
	// appliedLive reflects whether the allow-list actually reached the LIVE
	// registry. It starts true when an applier is wired but degrades to false if
	// the connection is in the revision yet not attached live (the reconcile will
	// apply it) — so the revert path never tries to un-apply what was never
	// applied, and the response tells the caller honestly.
	appliedLive := s.discoveryApplier != nil
	applyErr, emitErr := adminwrite.Apply(
		func() (revert func() error, err error) {
			written, recErr := s.registry.SetRevision(ctx, q, req.AgentID, agentcfg.ConfigScopeAgent, payload,
				agentcfg.SetOptions{ExpectedContentHash: req.ExpectedContentHash})
			if recErr != nil {
				return nil, recErr
			}
			rev = written
			if s.discoveryApplier != nil {
				live, setErr := s.discoveryApplier.SetOAuthDiscoveryOrigins(applyCtx, id.TenantID, req.AgentID, name, origins)
				switch {
				case setErr == nil:
					prevLive = live
				case errors.Is(setErr, ErrDiscoveryTargetNotLive):
					// The connection is in the revision but not attached in the live
					// registry (server unreachable, or awaiting the next run-start
					// reconcile). Record the allowance in the revision — the reconcile
					// applies it when the server comes online — instead of failing the
					// write. Matches the nil-applier path; the delta is computed against
					// the prior descriptor's revisioned allow-list.
					appliedLive = false
					prevLive = append([]string(nil), desc.OAuthDiscoveryAllowedOrigins...)
				default:
					// A real live apply failure — including the owner refusal
					// (ErrConnectionOwnerMismatch), which is an authorization failure the
					// caller must SEE rather than a silent degrade to a revision-only
					// write — roll the just-written revision back so the call has NO
					// observable effect, then return the apply error.
					if _, rbErr := s.registry.Rollback(ctx, q, req.AgentID, prevActiveRevID, agentcfg.ConfigScopeAgent, compensatingWrite()); rbErr != nil {
						return nil, fmt.Errorf("live allow-list apply failed AND revision rollback failed (state may be inconsistent): %w", errors.Join(setErr, rbErr))
					}
					return nil, setErr
				}
			} else {
				// No live registry wired — the delta is computed against the prior
				// descriptor's allow-list (the revisioned view).
				prevLive = append([]string(nil), desc.OAuthDiscoveryAllowedOrigins...)
			}
			revert = func() error {
				var errs []error
				if appliedLive {
					if _, e := s.discoveryApplier.SetOAuthDiscoveryOrigins(applyCtx, id.TenantID, req.AgentID, name, prevLive); e != nil {
						errs = append(errs, e)
					}
				}
				if _, e := s.registry.Rollback(ctx, q, req.AgentID, prevActiveRevID, agentcfg.ConfigScopeAgent, compensatingWrite()); e != nil {
					errs = append(errs, e)
				}
				return errors.Join(errs...)
			}
			return revert, nil
		},
		func() error {
			granted, revoked := originDelta(prevLive, origins)
			return s.emitDiscoveryOriginsSet(ctx, q, req.AgentID, name, rev.RevisionID, granted, revoked)
		},
	)
	if applyErr != nil {
		return prototypes.AgentConfigSetMCPDiscoveryOriginsResponse{}, applyErr
	}
	if emitErr != nil {
		return prototypes.AgentConfigSetMCPDiscoveryOriginsResponse{}, emitErr
	}

	granted, revoked := originDelta(prevLive, origins)
	return prototypes.AgentConfigSetMCPDiscoveryOriginsResponse{
		Revision:        revisionToWire(rev),
		Name:            name,
		Granted:         granted,
		Revoked:         revoked,
		AppliedLive:     appliedLive,
		ProtocolVersion: prototypes.ProtocolVersion,
	}, nil
}

// rebuildWithDiscoveryOrigins builds the replacing config payload: the named
// descriptor's OAuthDiscoveryAllowedOrigins becomes `origins`, every other
// descriptor is carried verbatim, and every sibling section (skills /
// tool-exposure / prompt-layers / llm-params / hooks / naming) is carried
// forward (the rebuild-completeness guard).
func (s *Service) rebuildWithDiscoveryOrigins(active agentcfg.Revision, hasActive bool, prior []agentcfg.MCPConnectionDescriptor, name string, origins []string) agentcfg.ConfigPayload {
	servers := make([]agentcfg.MCPConnectionDescriptor, 0, len(prior))
	for _, d := range prior {
		if d.Name == name {
			d.OAuthDiscoveryAllowedOrigins = append([]string(nil), origins...)
		}
		servers = append(servers, d)
	}
	payload := agentcfg.ConfigPayload{}
	if hasActive {
		payload.Skills = active.Payload.Skills
		payload.ToolExposure = active.Payload.ToolExposure
		payload.PromptLayers = active.Payload.PromptLayers
		payload.LLMParams = active.Payload.LLMParams
		payload.Hooks = active.Payload.Hooks
		payload.Naming = active.Payload.Naming
		payload.AgentPacks = copyAgentPackItems(active.Payload.AgentPacks)
		// The ordered additive prompt blocks are a sibling section like any
		// other: this verb replaces only its own, so the blocks survive.
		payload.ExtraSystemBlocks = active.Payload.ExtraSystemBlocks
		payload.OAuthProviders = active.Payload.OAuthProviders
		payload.SignedOAuthMCPPair = active.Payload.SignedOAuthMCPPair
		payload.SignedOAuthMCPPairs = active.Payload.SignedOAuthMCPPairs
	}
	if len(servers) > 0 {
		payload.Connections = &agentcfg.ConnectionsSection{Servers: servers}
	}
	return payload
}

// emitDiscoveryOriginsSet publishes the `mcp.connection.discovery_origins_set`
// admin-scope audit event and RETURNS a publish failure so the caller fails the
// write closed (the fail-closed audit posture). A nil bus is a no-op success
// (no audit sink configured is a deployment choice, mirroring the sibling
// verbs' optional-bus semantics). The origin deltas are non-secret public
// https origins.
func (s *Service) emitDiscoveryOriginsSet(ctx context.Context, q identity.Quadruple, agentID, server, revisionID string, granted, revoked []string) error {
	if s.bus == nil {
		return nil
	}
	now := s.now().UTC()
	ev := events.Event{
		Type:       agentcfg.EventTypeMCPDiscoveryOriginsSet,
		Identity:   q,
		OccurredAt: now,
		Payload: agentcfg.MCPDiscoveryOriginsSetPayload{
			Author:     q,
			AgentID:    agentID,
			ServerID:   server,
			RevisionID: revisionID,
			Granted:    granted,
			Revoked:    revoked,
			OccurredAt: now,
		},
	}
	if err := s.bus.Publish(ctx, ev); err != nil {
		return fmt.Errorf("agentcfg/protocol: publish %s: %w", ev.Type, err)
	}
	return nil
}

// findConnection returns the descriptor with the given name and whether it was
// present. The input slice is never mutated.
func findConnection(in []agentcfg.MCPConnectionDescriptor, name string) (agentcfg.MCPConnectionDescriptor, bool) {
	for _, d := range in {
		if d.Name == name {
			return d, true
		}
	}
	return agentcfg.MCPConnectionDescriptor{}, false
}

// originDelta computes the granted (in next, not in prev) and revoked (in prev,
// not in next) origin sets, each sorted for a stable audit / response. Both are
// nil when empty (never nil-vs-empty ambiguous on the wire via omitempty).
func originDelta(prev, next []string) (granted, revoked []string) {
	prevSet := make(map[string]struct{}, len(prev))
	for _, o := range prev {
		prevSet[o] = struct{}{}
	}
	nextSet := make(map[string]struct{}, len(next))
	for _, o := range next {
		nextSet[o] = struct{}{}
	}
	for _, o := range next {
		if _, ok := prevSet[o]; !ok {
			granted = append(granted, o)
		}
	}
	for _, o := range prev {
		if _, ok := nextSet[o]; !ok {
			revoked = append(revoked, o)
		}
	}
	sort.Strings(granted)
	sort.Strings(revoked)
	return granted, revoked
}
