package protocol

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"regexp"
	"strings"
	"time"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
	toolauth "github.com/hurtener/Harbor/internal/tools/auth"
)

// addconnection.go — the admin-scoped `agent_config.add_mcp_connection`
// control method: the separable hard piece of the agent-config control
// plane. Unlike pause/resume (a projection-time flag on an already-attached
// server), adding a genuinely NEW MCP connection drives an async dial + the
// MCP `initialize` handshake + discover, durable desired-state write, then
// atomic live activation, with possible OAuth.
//
// # The lifecycle (fail loud)
//
// The add models an explicit lifecycle pending → online | failed |
// auth_required. The runtime emits `mcp.connection.pending` at the start,
// then exactly one terminal lifecycle event. Connect and discovery happen on
// an unpublished prepared transport. A successful preparation records the
// NON-SECRET descriptor as a config revision (preserving the skills /
// tool-exposure / prompt-layer / llm-params / hooks sections — the
// bidirectional section-merge invariant), then publishes the catalog and
// registry. A failed preparation records NO revision and publishes NO server;
// a failed activation compensates the landed revision and restores the exact
// prior live state. An auth-required attach parks on the unified pause/resume
// primitive (the tool-side OAuth lineage — NOT a new auth dance).
//
// # Ambiguous persistence outcomes converge by exact descriptor
//
// A revision driver may report an error after the exact desired state landed.
// The service point-reads the active revision and activates only when both the
// normalized connection and inline OAuth descriptors match exactly. A
// same-name sibling is not proof. Unknown or non-matching outcomes close the
// unpublished transport. Compatibility callers that only implement the older
// live-first [ConnectionAttacher] seam retain scoped compensation; production
// wiring implements [ConnectionPreparer].
//
// # Secret hygiene (CLAUDE.md §7, load-bearing)
//
// The recorded revision, the diff, and every emitted event carry ONLY the
// non-secret descriptor (name, transport, command/URL). Operator-supplied
// auth headers flow to the live attach and are NEVER persisted in a
// revision, a diff, an event, or a log line.
//
// # The stdio gate (CLAUDE.md §7, fail-closed)
//
// Adding a stdio server runs an operator-supplied command (an RCE surface).
// Beyond the admin scope, a stdio add is allowlist-gated: command[0] MUST be
// in the operator-declared allowlist. An empty allowlist rejects every stdio
// add. argv-form only — the descriptor is never passed to a shell.

// ConnectionAttacher is the compatibility seam for live-first runtime adds:
// dial → `initialize` handshake → discover → register, fail-loud per step.
// It is the seam that keeps the concrete MCP driver out of this package (the
// §4.4 boundary): the cmd/harbor + devstack boundary injects a concrete that
// builds a `config.MCPServerConfig` and calls the MCP driver's `Attach`.
//
// Attach returns:
//   - nil on a successful attach (online — the server's tools are registered
//     on the live catalog);
//   - an error wrapping ErrAuthRequired when the server needs authorization
//     (the service parks on the unified pause/resume primitive);
//   - any other error when an attach step failed (the service records the
//     `failed` lifecycle, never a half-attached server).
type ConnectionAttacher interface {
	Attach(ctx context.Context, req AttachRequest) error
}

// ConnectionPreparer performs the network-dependent half of an MCP attach
// without publishing tools or a registry entry. The service persists desired
// state between PrepareConnection and PreparedConnection.Activate.
type ConnectionPreparer interface {
	PrepareConnection(ctx context.Context, req AttachRequest) (PreparedConnection, error)
}

type connectionMatcher interface {
	ConnectionMatches(owner toolauth.Owner, name, descriptorFingerprint string) bool
}

const mcpAddContinuationKind = "agentcfg.mcp.add_connection"

func mcpAddContinuation(agentID string, desc agentcfg.MCPConnectionDescriptor) pauseresume.Continuation {
	return pauseresume.Continuation{Kind: mcpAddContinuationKind, Data: map[string]string{
		"agent_id":               agentID,
		"server":                 desc.Name,
		"descriptor_fingerprint": agentcfg.MCPConnectionFingerprint(desc),
	}}
}

// PreparedConnection owns one connected, discovered, unpublished MCP
// connection. Activate publishes it once; Close drains it on refusal.
type PreparedConnection interface {
	Activate(ctx context.Context) error
	Close(ctx context.Context) error
}

// AuthorityBoundPreparedConnection is the mandatory signed-capability
// publication seam. ActivateIf establishes an exact, non-dispatchable provider
// reservation before prove runs, then publishes only if that same reservation
// is still current. Exact teardown can invalidate and close the reservation;
// callers must treat a non-implementing prepared connection as unavailable,
// never fall back to ordinary Activate.
type AuthorityBoundPreparedConnection interface {
	PreparedConnection
	ActivateIf(ctx context.Context, prove func(context.Context) error) error
	// ActivateUnder hands the exact local publication callback to admit. The
	// caller uses this to hold a durable operation-slot fence across catalog and
	// registry publication without putting network preparation inside the fence.
	ActivateUnder(ctx context.Context, admit func(context.Context, func() error) error) error
}

// ProviderPreparer builds an unpublished OAuth provider for an MCP prepare.
// The provider can be used privately during dial/discovery, then published
// reversibly after the durable revision lands.
type ProviderPreparer interface {
	PrepareProvider(ctx context.Context, tenant, agentID string, desc agentcfg.OAuthProviderDescriptor) (PreparedOAuthProvider, error)
}

// SignedCapabilityProviderPreparer constructs the pair-owned provider used by
// signed capability flow. It deliberately has no Publish-to-ProviderSet operation: the MCP
// prepared connection receives the private binding directly and its catalog
// activation is the sole data-plane publication point.
type SignedCapabilityProviderPreparer interface {
	PrepareSignedCapabilityProvider(ctx context.Context, broker string, binding toolauth.SignedCapabilityExchangeBinding, scopes []string) (PreparedOAuthProvider, error)
}

// PreparedOAuthProvider owns one unpublished provider instance.
type PreparedOAuthProvider interface {
	Binding() toolauth.OAuthProvider
	Publish(ctx context.Context) error
	Commit(ctx context.Context)
	Rollback(ctx context.Context) error
	Close(ctx context.Context) error
}

type legacyConnectionPreparer struct{ attacher ConnectionAttacher }

func (p legacyConnectionPreparer) PrepareConnection(ctx context.Context, req AttachRequest) (PreparedConnection, error) {
	if err := p.attacher.Attach(ctx, req); err != nil {
		return nil, err
	}
	return legacyPreparedConnection{}, nil
}

type legacyPreparedConnection struct{}

func (legacyPreparedConnection) Activate(context.Context) error { return nil }
func (legacyPreparedConnection) Close(context.Context) error    { return nil }

// ConnectionDetacher is the compensating twin of the compatibility
// [ConnectionAttacher] seam: it tears down a server that older live-first
// callers attached before a revision write failed.
//
// It exists because the add door's two side effects are not one transaction.
// The attach is live and irreversible-by-itself; the revision write can be
// refused after it (an expected-revision precondition whose base moved is the
// reachable case). Without this seam a refused write would leave a dialed,
// handshaken, registered server that NO revision names — and therefore one
// `agent_config.remove_mcp_connection` answers ErrConnectionNotFound for and
// the run-start reconcile never sees, because the reconcile diffs the LIVE
// owner view against the DECLARED set and an undeclared-but-live server is
// exactly what it would detach only if it also appeared in the owner view of
// a later run. A live server nothing can remove is worse than a failed add.
//
// The concrete (wired at the cmd/harbor + devstack boundary) is the same
// object that satisfies [ConnectionAttacher], so attach and compensating
// detach share one registry + one catalog and cannot drift apart. Optional —
// a nil detacher makes the compensation report the leak loudly instead of
// pretending it happened (CLAUDE.md §13).
type ConnectionDetacher interface {
	// DetachConnection deregisters the named source's tools from the live
	// catalog + MCP registry and closes its transport, scoped to the
	// (tenant, agentID) owner the attach stamped. Idempotent: a source
	// already gone is a no-op.
	DetachConnection(ctx context.Context, tenant, agentID, name string) error
}

// ExactConnectionDetacher is the mandatory teardown seam for a signed pair.
// The complete descriptor fingerprint is proved at the registry/catalog
// linearization point before any source is withdrawn.
type ExactConnectionDetacher interface {
	DetachExactConnection(ctx context.Context, tenant, agentID, name, descriptorFingerprint string) error
}

// SubjectExactConnectionDetacher is the user-scoped companion to
// ExactConnectionDetacher. Implementations prove the complete physical owner,
// including the verified user, before withdrawing a connection.
type SubjectExactConnectionDetacher interface {
	DetachExactConnectionForOwner(ctx context.Context, owner toolauth.Owner, name, descriptorFingerprint string) error
}

// ExactConnectionTeardownFence is the process-local admission receipt that
// spans desired-state removal. Seal is called once pair absence is durable;
// Cancel is called only when the CAS is proven not to have committed.
type ExactConnectionTeardownFence interface {
	Seal()
	Cancel(ctx context.Context) error
}

// ExactConnectionTeardownFencer prevents a matching private preparation from
// publishing between the final durable authority proof and pair removal CAS.
// It is a companion to ExactConnectionDetacher so existing non-signed detach
// implementations do not acquire lifecycle ceremony they cannot use.
type ExactConnectionTeardownFencer interface {
	BeginExactConnectionTeardown(tenant, agentID, name, descriptorFingerprint string) (ExactConnectionTeardownFence, error)
}

// SubjectExactConnectionTeardownFencer admits exact teardown for a complete
// physical owner. It prevents one user from reserving another user's source.
type SubjectExactConnectionTeardownFencer interface {
	BeginExactConnectionTeardownForOwner(owner toolauth.Owner, name, descriptorFingerprint string) (ExactConnectionTeardownFence, error)
}

// AttachRequest is the input to a ConnectionAttacher. It carries the
// non-secret descriptor PLUS the optional operator-supplied auth headers
// used ONLY for the live transport — the attacher never persists them.
type AttachRequest struct {
	// Identity is the verified caller triple (the attach runs under it; the
	// driver stamps it on transport-side events).
	Identity identity.Identity
	// AgentID is the agent whose config revision owns this runtime-added
	// connection. With Identity.TenantID it forms the (tenant, agent)
	// reconcile-view owner tag the attacher stamps on the registry entry so a
	// run-start reconcile scopes to its own owner. Registration metadata, never
	// an isolation key.
	AgentID string
	// Owner optionally overrides the default (tenant, agent) owner for a
	// user-scoped signed registration. A non-empty User must equal
	// Identity.UserID; the attacher rejects any broader owner so physical
	// provider/connection state cannot be redirected to another subject.
	Owner toolauth.Owner
	// Name is the unique MCP source id.
	Name string
	// Transport is "stdio" or "http".
	Transport agentcfg.MCPTransport
	// Command is the stdio argv (argv[0] is the binary; no shell).
	Command []string
	// URL is the http(s) endpoint.
	URL string
	// Headers are operator-supplied auth headers used ONLY for the live
	// attach. SECRET — never persisted (CLAUDE.md §7).
	Headers map[string]string
	// OAuthProvider is the non-secret provider NAME to bind for per-identity
	// southbound bearer injection (empty leaves the connection on its static
	// Headers). The attacher resolves it against the declared registry.
	OAuthProvider string
	// OAuthProviderOverride is an unpublished provider instance used only by
	// this private preparation. It never enters the shared provider set until
	// the durable desired state has landed.
	OAuthProviderOverride toolauth.OAuthProvider
	// OwnOAuthProvider transfers teardown ownership of OAuthProviderOverride to
	// the prepared MCP connection. It is used only for a pair-private binding.
	OwnOAuthProvider bool
	// ToolAllowlist and ToolDenylist restrict the server-side tool names that
	// may be projected into the live catalog. Empty allowlist means all except
	// explicitly denied names.
	ToolAllowlist []string
	ToolDenylist  []string
	// ConnectTimeoutMS bounds connect plus initial discovery. RequestTimeoutMS
	// becomes the default per-request tool policy for the live connection.
	ConnectTimeoutMS int
	RequestTimeoutMS int
	// DescriptorFingerprint, when set by a server-owned signed pair, commits the
	// complete connection policy. Generic connections derive their fingerprint
	// from the ordinary descriptor fields.
	DescriptorFingerprint string
	// MetaAnnotations is the non-secret operator `_meta` annotation set the
	// attacher carries onto the live connection's per-call `_meta`.
	MetaAnnotations map[string]string
	// OAuthDiscoveryAllowedOrigins is the non-secret per-connection cross-origin
	// allow-list for OAuth-requirement discovery fetches. The attacher carries
	// it into the config.MCPServerConfig it builds so the live registry snapshots
	// it (closing the wiring gap — the walker's allowance input now flows from
	// the add request through to the registry, no longer inert for a
	// runtime-added connection). Empty leaves the authorization-server hop
	// needs-allowance.
	OAuthDiscoveryAllowedOrigins []string
	// Injection is the non-secret per-user credential-INJECTION mapping for a
	// receiver-style server (nil when the connection binds none). The attacher
	// carries it into the config.MCPServerConfig it builds so the shared injection
	// engine sources + injects the acting principal's credential per outbound call.
	// NON-SECRET (a broker name + a target key/form); the pulled value is resolved
	// per-call from the ctx identity and never rides this request.
	Injection *agentcfg.MCPCredentialInjectionDescriptor
	// ArtifactByteEligible is the operator's declaration that this
	// connection MAY receive artifact bytes through egress substitution.
	// NON-SECRET. The attacher carries it into the config.MCPServerConfig
	// it builds so the driver's shared egress engine is armed for this
	// connection — a field on the descriptor that nothing carries forward
	// is inert, which is the wiring-gap shape a discovery allow-list
	// already hit on this exact path.
	ArtifactByteEligible bool
	// ArtifactParams is the non-secret per-tool artifact-parameter
	// mapping. The attacher carries it into the config.MCPServerConfig so
	// the boot path and the runtime-add path share ONE egress engine
	// rather than growing a second. Requires ArtifactByteEligible.
	ArtifactParams map[string][]string
}

// ConnectionState is the explicit attach lifecycle state surfaced on the
// response + the lifecycle events. The set is closed.
type ConnectionState string

// The canonical attach lifecycle states.
const (
	// ConnectionStatePending — the attach has begun (dial → handshake →
	// discover → register). The transient initial state.
	ConnectionStatePending ConnectionState = "pending"
	// ConnectionStateOnline — the attach completed; the server's tools are
	// registered on the live catalog.
	ConnectionStateOnline ConnectionState = "online"
	// ConnectionStateFailed — an attach step failed; no revision recorded,
	// no half-attached server registered.
	ConnectionStateFailed ConnectionState = "failed"
	// ConnectionStateAuthRequired — the attach parked on the unified
	// pause/resume primitive awaiting authorization. An accepted resume re-reads
	// the exact durable descriptor, privately prepares it, then activates it.
	ConnectionStateAuthRequired ConnectionState = "auth_required"
)

// Add-connection sentinel errors. The wire handler maps each onto a
// canonical Protocol Code + HTTP status; in-process callers compare with
// errors.Is.
var (
	// ErrAuthRequired — the attacher signals the MCP server needs
	// authorization. The service parks on the unified pause/resume primitive.
	// Concrete attachers wrap this so the service reaches it via errors.Is.
	ErrAuthRequired = errors.New("agentcfg/protocol: mcp connection requires authorization")
	// ErrConnectionAttachUnavailable — add_mcp_connection was called but no
	// ConnectionAttacher is wired on this runtime (→ 501).
	ErrConnectionAttachUnavailable = errors.New("agentcfg/protocol: mcp connection attach not wired on this runtime")
	// ErrStdioNotAllowed — a stdio add named a command absent from the
	// fail-closed allowlist (→ 403). Adding a stdio server is the most
	// privileged action; the allowlist is the §7 RCE gate.
	ErrStdioNotAllowed = errors.New("agentcfg/protocol: stdio mcp connection command is not allowlisted")
	// ErrInvalidConnection — the connection descriptor failed validation
	// (empty name, unknown transport, missing/incoherent command/URL) (→ 400).
	ErrInvalidConnection = errors.New("agentcfg/protocol: invalid mcp connection descriptor")
	// ErrCoordinatorUnavailable — an auth-required attach fired but no
	// pause/resume Coordinator is wired (→ 500). Fail loud rather than drop
	// the auth requirement silently (CLAUDE.md §13).
	ErrCoordinatorUnavailable = errors.New("agentcfg/protocol: pause/resume coordinator not wired for the mcp oauth path")
)

// AddMCPConnection adds a NEW MCP server connection. See the file doc for
// the full lifecycle, secret-hygiene, and stdio-gate contract.
func (s *Service) AddMCPConnection(ctx context.Context, req prototypes.AgentConfigAddMCPConnectionRequest) (prototypes.AgentConfigAddMCPConnectionResponse, error) {
	if err := ctx.Err(); err != nil {
		return prototypes.AgentConfigAddMCPConnectionResponse{}, err
	}
	id, err := identityFromScope(req.Identity, req.AgentID)
	if err != nil {
		return prototypes.AgentConfigAddMCPConnectionResponse{}, err
	}
	q := identity.Quadruple{Identity: id}

	desc, inlineOAuth, wireInjection, err := validateConnection(req.Connection)
	if err != nil {
		return prototypes.AgentConfigAddMCPConnectionResponse{}, err
	}

	// Dev-gated wire-carried credential-INJECTION mapping (receiver-style server).
	// Fail-closed behind the `tools.allow_wire_injection` opt-in (independent of
	// the wire-OAuth opt-in): a connection carrying an injection mapping with the
	// opt-in off is rejected loud BEFORE any dial or revision write. When opted in,
	// the mapping is validated (form + redaction-covered target key) and normalised
	// onto the domain descriptor so it is carried into the attach AND persisted in
	// the revision (diff / rollback). The broker-name resolution + downstream-sink
	// derivation are re-enforced at attach time in the shared injection engine.
	injDesc, err := s.gateAndValidateInjectionDescriptor(wireInjection)
	if err != nil {
		return prototypes.AgentConfigAddMCPConnectionResponse{}, err
	}
	desc.Injection = injDesc

	// stdio gate (fail-closed, §7). A stdio add whose command[0] is absent
	// from the allowlist is rejected BEFORE any dial or revision write — the
	// most privileged action never spawns an un-allowlisted process. http
	// adds are admin-scoped but not gated here. Nothing is persisted on a
	// rejection.
	if desc.Transport == agentcfg.MCPTransportStdio {
		if _, ok := s.stdioAllowlist[desc.Command[0]]; !ok {
			return prototypes.AgentConfigAddMCPConnectionResponse{}, fmt.Errorf("%w: %q", ErrStdioNotAllowed, desc.Command[0])
		}
	}

	if s.preparer == nil {
		return prototypes.AgentConfigAddMCPConnectionResponse{}, ErrConnectionAttachUnavailable
	}

	// One owner lock spans expectation validation, preparation, persistence and
	// activation. No same-owner writer can move the base between the early
	// side-effect guard and the driver's authoritative write check.
	release := s.lockAgent(id.TenantID, req.AgentID)
	defer release()
	opts := agentcfg.SetOptions{ExpectedContentHash: req.ExpectedContentHash}
	if err := s.precheckExpectedRevision(ctx, q, req.AgentID, agentcfg.ConfigScopeAgent, opts); err != nil {
		return prototypes.AgentConfigAddMCPConnectionResponse{}, err
	}

	// Dev-gated wire-carried OAuth binding. An inline wire descriptor privately
	// prepares a NEW provider whose downstream sink is DERIVED from this connection's own URL
	// (never a wire field); a NAME binding to an already-installed WIRE provider
	// (e.g. from a prior set_oauth_provider) re-derives that provider's sink from
	// this connection's URL. Both fail-closed behind the opt-in (the gate is
	// inside gateAndValidateOAuthProviderDescriptor). The private instance is
	// threaded directly into MCP preparation and enters the shared provider set
	// only after the revision lands; its receipt restores the exact prior binding
	// if MCP publication then fails.
	wireProvider, wireProviderIsNew, preparedProvider, err := s.prepareWireOAuthBinding(ctx, id, req.AgentID, &desc, inlineOAuth)
	if err != nil {
		return prototypes.AgentConfigAddMCPConnectionResponse{}, err
	}

	// Emit the `pending` lifecycle transition before the dial (loud
	// observability — never a secret).
	s.emitConnectionLifecycle(ctx, q, req.AgentID, desc, ConnectionStatePending, "", "", "")

	// Connect and discover privately. The headers flow to the transport ONLY;
	// a successful preparation has not changed the shared catalog or registry.
	continuation := mcpAddContinuation(req.AgentID, desc)
	prepareCtx := pauseresume.WithContinuation(ctx, continuation)
	prepared, prepareErr := s.preparer.PrepareConnection(prepareCtx, AttachRequest{
		Identity:                     id,
		AgentID:                      req.AgentID,
		Name:                         desc.Name,
		Transport:                    desc.Transport,
		Command:                      append([]string(nil), desc.Command...),
		URL:                          desc.URL,
		Headers:                      req.Headers,
		OAuthProvider:                desc.OAuthProvider,
		OAuthProviderOverride:        preparedProviderBinding(preparedProvider),
		MetaAnnotations:              cloneAnnotations(desc.MetaAnnotations),
		OAuthDiscoveryAllowedOrigins: append([]string(nil), desc.OAuthDiscoveryAllowedOrigins...),
		Injection:                    desc.Injection.Clone(),
		ArtifactByteEligible:         desc.ArtifactByteEligible,
		ArtifactParams:               desc.CloneArtifactParams(),
	})

	switch {
	case prepareErr == nil:
		rev, recErr := s.recordConnectionRevision(ctx, q, req.AgentID, desc, wireProvider, opts)
		if recErr != nil {
			return prototypes.AgentConfigAddMCPConnectionResponse{}, s.handlePreparedRecordFailure(ctx, q, id, req.AgentID, desc, wireProvider, wireProviderIsNew, preparedProvider, prepared, recErr)
		}
		if preparedProvider != nil {
			if publishErr := preparedProvider.Publish(ctx); publishErr != nil {
				return prototypes.AgentConfigAddMCPConnectionResponse{}, s.rollbackFailedActivation(ctx, q, req.AgentID, desc, preparedProvider, prepared, rev, publishErr)
			}
		}
		if activateErr := prepared.Activate(ctx); activateErr != nil {
			return prototypes.AgentConfigAddMCPConnectionResponse{}, s.rollbackFailedActivation(ctx, q, req.AgentID, desc, preparedProvider, prepared, rev, activateErr)
		}
		if preparedProvider != nil {
			preparedProvider.Commit(ctx)
		}
		s.emitConnectionLifecycle(ctx, q, req.AgentID, desc, ConnectionStateOnline, rev.RevisionID, "", "")
		view := revisionToWire(rev)
		return prototypes.AgentConfigAddMCPConnectionResponse{
			Revision:        &view,
			Connection:      descriptorToWire(desc),
			State:           string(ConnectionStateOnline),
			ProtocolVersion: prototypes.ProtocolVersion,
		}, nil

	case errors.Is(prepareErr, ErrAuthRequired):
		// The server needs authorization — park on the unified pause/resume
		// primitive (NOT a new auth dance). Fail loud if no coordinator is
		// wired rather than dropping the auth requirement.
		if s.coordinator == nil {
			return prototypes.AgentConfigAddMCPConnectionResponse{}, errors.Join(ErrCoordinatorUnavailable, rollbackPreparedProvider(ctx, preparedProvider))
		}
		var providerAuth *toolauth.ErrAuthRequired
		_ = errors.As(prepareErr, &providerAuth)
		rev, recErr := s.recordConnectionRevision(ctx, q, req.AgentID, desc, wireProvider, opts)
		if recErr != nil {
			landedRevision, connExact, providerExact, readErr := s.activeMatches(ctx, q, req.AgentID, desc, wireProvider)
			if readErr == nil && connExact && providerExact {
				// The persistence acknowledgement is ambiguous, but the active
				// pointer proves this exact descriptor/provider pair landed. The
				// producer-owned pause is already the continuation rendezvous: do
				// not reject it or turn a durable auth-required add into a failed
				// response merely because the write acknowledgement was lost.
				rev = landedRevision
				if preparedProvider != nil {
					if publishErr := preparedProvider.Publish(ctx); publishErr != nil {
						return prototypes.AgentConfigAddMCPConnectionResponse{}, errors.Join(recErr, publishErr, rollbackPreparedProvider(ctx, preparedProvider), s.rejectProducerPause(ctx, providerAuth))
					}
					preparedProvider.Commit(ctx)
				}
				var token pauseresume.Token
				var parkErr error
				if providerAuth != nil && providerAuth.PauseToken != "" {
					token = pauseresume.Token(providerAuth.PauseToken)
				} else {
					token, parkErr = s.parkForAuth(ctx, id, req.AgentID, desc)
				}
				if parkErr != nil {
					s.emitConnectionLifecycle(ctx, q, req.AgentID, desc, ConnectionStateFailed, rev.RevisionID, "", safeReason(parkErr))
					return prototypes.AgentConfigAddMCPConnectionResponse{}, errors.Join(recErr, parkErr)
				}
				s.emitConnectionLifecycle(ctx, q, req.AgentID, desc, ConnectionStateAuthRequired, rev.RevisionID, string(token), "")
				view := revisionToWire(rev)
				return prototypes.AgentConfigAddMCPConnectionResponse{
					Revision:        &view,
					Connection:      descriptorToWire(desc),
					State:           string(ConnectionStateAuthRequired),
					PauseToken:      string(token),
					ProtocolVersion: prototypes.ProtocolVersion,
				}, nil
			}
			providerErr := rollbackPreparedProvider(ctx, preparedProvider)
			pauseErr := s.rejectProducerPause(ctx, providerAuth)
			return prototypes.AgentConfigAddMCPConnectionResponse{}, errors.Join(recErr, readErr, providerErr, pauseErr)
		}
		if preparedProvider != nil {
			if publishErr := preparedProvider.Publish(ctx); publishErr != nil {
				return prototypes.AgentConfigAddMCPConnectionResponse{}, errors.Join(publishErr, rollbackPreparedProvider(ctx, preparedProvider), s.rollbackRevision(ctx, q, req.AgentID, rev), s.rejectProducerPause(ctx, providerAuth))
			}
			preparedProvider.Commit(ctx)
		}
		var token pauseresume.Token
		var parkErr error
		if providerAuth != nil && providerAuth.PauseToken != "" {
			token = pauseresume.Token(providerAuth.PauseToken)
		} else {
			token, parkErr = s.parkForAuth(ctx, id, req.AgentID, desc)
		}
		if parkErr != nil {
			// The revision IS recorded (so the connection is nameable and
			// removable) but no terminal lifecycle event has fired — leaving a
			// Console reader on the transient `pending` forever, which is the
			// silent half of the same defect the compensation above closes. Emit
			// the terminal `failed` with a scrubbed reason before returning.
			s.emitConnectionLifecycle(ctx, q, req.AgentID, desc, ConnectionStateFailed, rev.RevisionID, "", safeReason(parkErr))
			return prototypes.AgentConfigAddMCPConnectionResponse{}, parkErr
		}
		s.emitConnectionLifecycle(ctx, q, req.AgentID, desc, ConnectionStateAuthRequired, rev.RevisionID, string(token), "")
		view := revisionToWire(rev)
		return prototypes.AgentConfigAddMCPConnectionResponse{
			Revision:        &view,
			Connection:      descriptorToWire(desc),
			State:           string(ConnectionStateAuthRequired),
			PauseToken:      string(token),
			ProtocolVersion: prototypes.ProtocolVersion,
		}, nil

	default:
		// Failed — record NO revision, register NO server, emit the loud
		// `failed` lifecycle event with a SAFE reason. The failure is surfaced
		// explicitly on the response (state=failed + reason): this is the
		// opposite of a silent drop (CLAUDE.md §13), not an error to the
		// caller (the operator gets the recorded failure).
		//
		// A wire provider newly installed for THIS inline binding is rolled back
		// (uninstalled + closed) so a failed attach leaves no orphan live provider
		// (nothing was persisted; the live install must not linger). A re-derived
		// pre-existing provider (bind-by-name) is left in place — it was installed
		// by its own set_oauth_provider and survives this failed attach.
		if providerErr := rollbackPreparedProvider(ctx, preparedProvider); providerErr != nil {
			s.logger.ErrorContext(ctx, "agent-config: private inline oauth provider cleanup failed after MCP preparation failure",
				"agent_id", req.AgentID, "server_id", desc.Name, "error", providerErr.Error())
		}
		reason := safeReason(prepareErr)
		s.emitConnectionLifecycle(ctx, q, req.AgentID, desc, ConnectionStateFailed, "", "", reason)
		return prototypes.AgentConfigAddMCPConnectionResponse{
			Connection:      descriptorToWire(desc),
			State:           string(ConnectionStateFailed),
			Reason:          reason,
			ProtocolVersion: prototypes.ProtocolVersion,
		}, nil
	}
}

func (s *Service) handlePreparedRecordFailure(ctx context.Context, q identity.Quadruple, id identity.Identity, agentID string, desc agentcfg.MCPConnectionDescriptor, wireProvider *agentcfg.OAuthProviderDescriptor, wireProviderIsNew bool, preparedProvider PreparedOAuthProvider, prepared PreparedConnection, cause error) error {
	// Compatibility adapters are live-first; retain the old scoped compensation
	// only for those non-production callers.
	if _, legacy := prepared.(legacyPreparedConnection); legacy {
		s.compensateAttach(ctx, q, id, agentID, desc, wireProvider, wireProviderIsNew, cause)
		return cause
	}
	landedRevision, connDeclared, providerDeclared, readErr := s.activeMatches(ctx, q, agentID, desc, wireProvider)
	if readErr != nil {
		closeErr := closePreparedConnection(ctx, prepared)
		providerErr := rollbackPreparedProvider(ctx, preparedProvider)
		s.emitConnectionLifecycle(ctx, q, agentID, desc, ConnectionStateFailed, "", "", safeReason(cause))
		return errors.Join(cause, readErr, closeErr, providerErr)
	}
	if connDeclared && providerDeclared {
		if preparedProvider != nil {
			if publishErr := preparedProvider.Publish(ctx); publishErr != nil {
				return s.rollbackFailedActivation(ctx, q, agentID, desc, preparedProvider, prepared, landedRevision, errors.Join(cause, publishErr))
			}
		}
		activateErr := prepared.Activate(ctx)
		if activateErr != nil {
			return s.rollbackFailedActivation(ctx, q, agentID, desc, preparedProvider, prepared, landedRevision, errors.Join(cause, activateErr))
		}
		if preparedProvider != nil {
			preparedProvider.Commit(ctx)
		}
		s.emitConnectionLifecycle(ctx, q, agentID, desc, ConnectionStateOnline, "", "", "")
		return cause
	}
	closeErr := closePreparedConnection(ctx, prepared)
	providerErr := rollbackPreparedProvider(ctx, preparedProvider)
	s.emitConnectionLifecycle(ctx, q, agentID, desc, ConnectionStateFailed, "", "", safeReason(cause))
	return errors.Join(cause, closeErr, providerErr)
}

// activeMatches confirms that the ambiguous write landed THIS operation's
// exact normalized descriptors. A same-name sibling is not proof: activating
// this prepared transport for a different winning descriptor would make live
// state disagree with the durable revision.
func (s *Service) activeMatches(ctx context.Context, q identity.Quadruple, agentID string, conn agentcfg.MCPConnectionDescriptor, provider *agentcfg.OAuthProviderDescriptor) (active agentcfg.Revision, connExact, providerExact bool, err error) {
	active, hasActive, err := s.registry.Active(ctx, identity.Quadruple{Identity: q.Identity}, agentID, agentcfg.ConfigScopeAgent)
	if err != nil || !hasActive {
		return agentcfg.Revision{}, false, false, err
	}
	wantPayload := agentcfg.NormalizePayload(agentcfg.ConfigPayload{
		Connections: &agentcfg.ConnectionsSection{Servers: []agentcfg.MCPConnectionDescriptor{conn}},
	})
	wantConnections := wantPayload.ConnectionDescriptors()
	if len(wantConnections) == 1 {
		for _, got := range active.Payload.ConnectionDescriptors() {
			if got.Name == conn.Name {
				connExact = reflect.DeepEqual(got, wantConnections[0])
				break
			}
		}
	}
	if provider == nil {
		return active, connExact, true, nil
	}
	wantPayload = agentcfg.NormalizePayload(agentcfg.ConfigPayload{
		OAuthProviders: &agentcfg.OAuthProvidersSection{Providers: []agentcfg.OAuthProviderDescriptor{*provider}},
	})
	wantProviders := wantPayload.OAuthProviderDescriptors()
	if len(wantProviders) == 1 {
		for _, got := range active.Payload.OAuthProviderDescriptors() {
			if got.Name == provider.Name {
				providerExact = reflect.DeepEqual(got, wantProviders[0])
				break
			}
		}
	}
	return active, connExact, providerExact, nil
}

func (s *Service) rollbackFailedActivation(ctx context.Context, q identity.Quadruple, agentID string, desc agentcfg.MCPConnectionDescriptor, preparedProvider PreparedOAuthProvider, prepared PreparedConnection, rev agentcfg.Revision, cause error) error {
	rollbackErr := s.rollbackRevision(ctx, q, agentID, rev)
	closeErr := closePreparedConnection(ctx, prepared)
	providerErr := rollbackPreparedProvider(ctx, preparedProvider)
	s.emitConnectionLifecycle(ctx, q, agentID, desc, ConnectionStateFailed, rev.RevisionID, "", safeReason(cause))
	return errors.Join(cause, rollbackErr, closeErr, providerErr)
}

func (s *Service) rollbackRevision(ctx context.Context, q identity.Quadruple, agentID string, rev agentcfg.Revision) error {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if rev.ParentRevisionID != "" {
		_, err := s.registry.Rollback(cleanupCtx, q, agentID, rev.ParentRevisionID, agentcfg.ConfigScopeAgent, compensatingWrite())
		return err
	}
	_, err := s.registry.SetRevision(cleanupCtx, q, agentID, agentcfg.ConfigScopeAgent, agentcfg.ConfigPayload{}, compensatingWrite())
	return err
}

func preparedProviderBinding(p PreparedOAuthProvider) toolauth.OAuthProvider {
	if p == nil {
		return nil
	}
	return p.Binding()
}

func rollbackPreparedProvider(ctx context.Context, p PreparedOAuthProvider) error {
	if p == nil {
		return nil
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	return p.Rollback(cleanupCtx)
}

func closePreparedConnection(ctx context.Context, p PreparedConnection) error {
	if p == nil {
		return nil
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	return p.Close(cleanupCtx)
}

// validateConnection validates + normalises the wire descriptor, failing
// loud with ErrInvalidConnection. stdio requires a non-empty argv command
// and no URL; http requires a URL and no command. An inline dev-gated OAuth
// binding (`connection.oauth`) is mutually exclusive with the `oauth_provider`
// NAME binding and with a credential-injection mapping (`connection.injection`);
// both raw wire objects are returned for the handler to gate + validate (the
// returned pointers are nil unless present). One auth mode per connection.
func validateConnection(c prototypes.AgentConfigMCPConnectionDescriptor) (agentcfg.MCPConnectionDescriptor, *prototypes.AgentConfigOAuthProviderDescriptor, *prototypes.AgentConfigMCPCredentialInjectionDescriptor, error) {
	name := strings.TrimSpace(c.Name)
	if name == "" {
		return agentcfg.MCPConnectionDescriptor{}, nil, nil, fmt.Errorf("%w: name is empty", ErrInvalidConnection)
	}
	// Reserved / spec-prefixed / empty / over-deep / colliding `_meta`
	// annotation PATHS are rejected for every transport (the runtime stamps
	// the triple + agent_id + trace context; an operator annotation must not
	// shadow them, at the whole key OR at any path segment). The injection
	// mapping's own `_meta` path participates in the collision check, so a
	// flat annotation can never silently lose to a nested credential write.
	if err := validateConnectionAnnotations(c.MetaAnnotations, wireInjectionMetaPath(c.Injection)); err != nil {
		return agentcfg.MCPConnectionDescriptor{}, nil, nil, err
	}
	// An inline OAuth binding, a provider-NAME binding, and a credential-injection
	// mapping are three ways to bind ONE auth mode — reject any two set together
	// (one auth mode per connection).
	if c.OAuth != nil && strings.TrimSpace(c.OAuthProvider) != "" {
		return agentcfg.MCPConnectionDescriptor{}, nil, nil, fmt.Errorf("%w: oauth (inline binding) and oauth_provider (name binding) are mutually exclusive — one auth mode per connection", ErrInvalidConnection)
	}
	if c.Injection != nil && strings.TrimSpace(c.OAuthProvider) != "" {
		return agentcfg.MCPConnectionDescriptor{}, nil, nil, fmt.Errorf("%w: injection (credential-injection mapping) and oauth_provider (name binding) are mutually exclusive — one auth mode per connection", ErrInvalidConnection)
	}
	if c.Injection != nil && c.OAuth != nil {
		return agentcfg.MCPConnectionDescriptor{}, nil, nil, fmt.Errorf("%w: injection (credential-injection mapping) and oauth (inline binding) are mutually exclusive — one auth mode per connection", ErrInvalidConnection)
	}
	transport := agentcfg.MCPTransport(strings.TrimSpace(c.Transport))
	switch transport {
	case agentcfg.MCPTransportStdio:
		if len(c.Command) == 0 || strings.TrimSpace(c.Command[0]) == "" {
			return agentcfg.MCPConnectionDescriptor{}, nil, nil, fmt.Errorf("%w: stdio transport requires a non-empty argv command", ErrInvalidConnection)
		}
		if c.URL != "" {
			return agentcfg.MCPConnectionDescriptor{}, nil, nil, fmt.Errorf("%w: stdio transport must not carry a url", ErrInvalidConnection)
		}
		if strings.TrimSpace(c.OAuthProvider) != "" {
			return agentcfg.MCPConnectionDescriptor{}, nil, nil, fmt.Errorf("%w: stdio transport must not bind an oauth_provider (no HTTP request to inject Authorization into)", ErrInvalidConnection)
		}
		if c.OAuth != nil {
			return agentcfg.MCPConnectionDescriptor{}, nil, nil, fmt.Errorf("%w: stdio transport must not carry an inline oauth binding (no HTTP request to inject Authorization into)", ErrInvalidConnection)
		}
		if c.Injection != nil {
			return agentcfg.MCPConnectionDescriptor{}, nil, nil, fmt.Errorf("%w: stdio transport must not carry an injection mapping (a receiver-style server is reached over HTTP; the pulled credential is delivered on the outbound request)", ErrInvalidConnection)
		}
		if len(c.OAuthDiscoveryAllowedOrigins) > 0 {
			return agentcfg.MCPConnectionDescriptor{}, nil, nil, fmt.Errorf("%w: stdio transport must not carry oauth_discovery_allowed_origins (no HTTP discovery walk)", ErrInvalidConnection)
		}
		// Egress substitution is refused on stdio for the same reason
		// every other http-only field is: base64-encoded artifact bytes
		// belong in an HTTP body, not a stdio frame, and a declaration a
		// connection can never exercise advertises a capability nothing
		// services. Both fields are refused, not just the mapping —
		// accepting a bare eligibility flag here would persist a
		// declaration that means nothing on this transport.
		if c.ArtifactByteEligible {
			return agentcfg.MCPConnectionDescriptor{}, nil, nil, fmt.Errorf("%w: stdio transport must not declare artifact_byte_eligible (base64-encoded artifact bytes belong in an HTTP body, not a stdio frame)", ErrInvalidConnection)
		}
		if len(c.ArtifactParams) > 0 {
			return agentcfg.MCPConnectionDescriptor{}, nil, nil, fmt.Errorf("%w: stdio transport must not carry artifact_params (base64-encoded artifact bytes belong in an HTTP body, not a stdio frame)", ErrInvalidConnection)
		}
		return agentcfg.MCPConnectionDescriptor{
			Name:            name,
			Transport:       transport,
			Command:         append([]string(nil), c.Command...),
			MetaAnnotations: cloneAnnotations(c.MetaAnnotations),
		}, nil, nil, nil
	case agentcfg.MCPTransportHTTP:
		rawURL := strings.TrimSpace(c.URL)
		if rawURL == "" {
			return agentcfg.MCPConnectionDescriptor{}, nil, nil, fmt.Errorf("%w: http transport requires a url", ErrInvalidConnection)
		}
		normalizedURL, uerr := config.NormalizeMCPHTTPURL(rawURL)
		if uerr != nil {
			return agentcfg.MCPConnectionDescriptor{}, nil, nil, fmt.Errorf("%w: http transport url: %w", ErrInvalidConnection, uerr)
		}
		if len(c.Command) > 0 {
			return agentcfg.MCPConnectionDescriptor{}, nil, nil, fmt.Errorf("%w: http transport must not carry a command", ErrInvalidConnection)
		}
		origins, oerr := normalizeDiscoveryOrigins(c.OAuthDiscoveryAllowedOrigins)
		if oerr != nil {
			return agentcfg.MCPConnectionDescriptor{}, nil, nil, oerr
		}
		params, perr := normalizeArtifactParams(c.ArtifactByteEligible, c.ArtifactParams)
		if perr != nil {
			return agentcfg.MCPConnectionDescriptor{}, nil, nil, perr
		}
		return agentcfg.MCPConnectionDescriptor{
			Name:                         name,
			Transport:                    transport,
			URL:                          normalizedURL,
			OAuthProvider:                strings.TrimSpace(c.OAuthProvider),
			MetaAnnotations:              cloneAnnotations(c.MetaAnnotations),
			OAuthDiscoveryAllowedOrigins: origins,
			ArtifactByteEligible:         c.ArtifactByteEligible,
			ArtifactParams:               params,
		}, c.OAuth, c.Injection, nil
	default:
		return agentcfg.MCPConnectionDescriptor{}, nil, nil, fmt.Errorf("%w: unknown transport %q (want stdio|http)", ErrInvalidConnection, c.Transport)
	}
}

// validateConnectionsSection runs EVERY connection descriptor in a full-payload
// write through [validateConnection] — the ONE shape authority the
// `add_mcp_connection` door uses (transport/URL/command coherence, the name
// rule, the reserved-`_meta`-key rule, one-auth-mode mutual exclusivity, and
// the stdio rules) — and returns the NORMALISED domain descriptors for the
// caller to persist. The full-payload `agent_config.set_revision` door persists
// the same descriptors the add door does, so it holds them to the same shape
// AND records the same bytes: the trimmed name / URL / provider name and the
// de-duplicated, validated origin list, never the raw wire values. Persisting
// the raw form would give the two doors different content hashes for the same
// logical input.
//
// The whole set is rejected on the first offender, naming its index and the
// specific rule, and the `%w` preserves ErrInvalidConnection so the wire
// handler's 400 mapping fires and nothing is persisted. A nil section (a
// payload that pins no connections) returns (nil, nil) — it is the
// carry-forward case, not a declaration of an empty set.
//
// The credential-injection mapping is carried onto the descriptor verbatim
// (exactly as [connectionsToDomain] projects it); its fail-closed OPT-IN gate
// and shape validation are the separate, composed half
// ([Service.gateAndValidateConnectionsInjection]), which needs Service state
// that shape validation does not. The stdio command allowlist is the other
// Service-state half ([Service.gateStdioConnectionCommands]).
func validateConnectionsSection(conns *prototypes.AgentConfigConnections) ([]agentcfg.MCPConnectionDescriptor, error) {
	if conns == nil {
		return nil, nil
	}
	out := make([]agentcfg.MCPConnectionDescriptor, 0, len(conns.Servers))
	for i := range conns.Servers {
		desc, _, _, err := validateConnection(conns.Servers[i])
		if err != nil {
			return nil, fmt.Errorf("connections.servers[%d]: %w", i, err)
		}
		// The injection mapping rides the descriptor unchanged — the opt-in gate
		// validates it separately and the domain shape is a straight projection.
		desc.Injection = injectionDescriptorToDomain(conns.Servers[i].Injection)
		out = append(out, desc)
	}
	return out, nil
}

// gateStdioConnectionCommands applies the fail-closed stdio command allowlist
// to every stdio descriptor a full-payload write declares — the SAME §7 RCE
// gate the `add_mcp_connection` door applies before it dials, so both doors
// onto the revision spine admit exactly the same stdio commands. Without it the
// full-payload door would persist a descriptor naming a command the add door
// refuses, and the spine is the input any future attach-from-revision leg
// reads.
//
// argv[0] is the gated binary (argv-form only; the descriptor is never passed
// to a shell). An empty allowlist refuses every stdio descriptor — the
// fail-closed default. The sentinel is ErrStdioNotAllowed, so this door answers
// with the same typed error and the same 403 the add door does. Nothing is
// persisted on a refusal.
//
// It gates only; it never rewrites a descriptor.
func (s *Service) gateStdioConnectionCommands(descs []agentcfg.MCPConnectionDescriptor) error {
	for i, d := range descs {
		if d.Transport != agentcfg.MCPTransportStdio {
			continue
		}
		// validateConnectionsSection guarantees a non-empty argv for stdio; the
		// guard keeps this safe for any future caller that skips it.
		if len(d.Command) == 0 {
			return fmt.Errorf("%w: connections.servers[%d]: stdio transport requires a non-empty argv command", ErrInvalidConnection, i)
		}
		if _, ok := s.stdioAllowlist[d.Command[0]]; !ok {
			return fmt.Errorf("%w: connections.servers[%d]: %q", ErrStdioNotAllowed, i, d.Command[0])
		}
	}
	return nil
}

// recordConnectionRevision writes a new config revision whose connections
// section is the prior connections PLUS the new descriptor (the connection
// added / replaced by name), preserving the skills + tool-exposure +
// prompt-layer + llm-params + hooks sections of the active revision (the
// bidirectional section-merge invariant). The descriptor is NON-SECRET — no
// header / token is ever part of the persisted payload.
func (s *Service) recordConnectionRevision(ctx context.Context, q identity.Quadruple, agentID string, desc agentcfg.MCPConnectionDescriptor, wireProvider *agentcfg.OAuthProviderDescriptor, opts agentcfg.SetOptions) (agentcfg.Revision, error) {
	active, hasActive, err := s.registry.Active(ctx, q, agentID, agentcfg.ConfigScopeAgent)
	if err != nil {
		return agentcfg.Revision{}, err
	}
	var servers []agentcfg.MCPConnectionDescriptor
	payload := agentcfg.ConfigPayload{}
	var providers []agentcfg.OAuthProviderDescriptor
	if hasActive {
		servers = append(servers, active.Payload.ConnectionDescriptors()...)
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
		providers = active.Payload.OAuthProviderDescriptors()
		payload.SignedOAuthMCPPair = active.Payload.SignedOAuthMCPPair
		payload.SignedOAuthMCPPairs = active.Payload.SignedOAuthMCPPairs
		payload.VirtualAgents = active.Payload.VirtualAgents
	}
	servers = append(servers, desc)
	payload.Connections = &agentcfg.ConnectionsSection{Servers: servers}
	// Upsert the wire provider (installed / re-derived for this connection's
	// binding) into the oauth-providers section so a rollback / reconcile rebuilds
	// it with the SAME derived downstream sink. The descriptor is NON-SECRET (a
	// token endpoint URL + an env-var NAME; no secret). A nil wireProvider carries
	// the existing section forward unchanged.
	if wireProvider != nil {
		merged := make([]agentcfg.OAuthProviderDescriptor, 0, len(providers)+1)
		replaced := false
		for _, p := range providers {
			if p.Name == wireProvider.Name {
				merged = append(merged, *wireProvider)
				replaced = true
				continue
			}
			merged = append(merged, p)
		}
		if !replaced {
			merged = append(merged, *wireProvider)
		}
		providers = merged
	}
	if len(providers) > 0 {
		payload.OAuthProviders = &agentcfg.OAuthProvidersSection{Providers: providers}
	}
	return s.registry.SetRevision(ctx, q, agentID, agentcfg.ConfigScopeAgent, payload, opts)
}

// compensateAttach undoes the LIVE half of an add whose revision write then
// failed, and emits the terminal `failed` lifecycle so the add never ends on
// the transient `pending`.
//
// # Why this exists (the defect it closes)
//
// A compatibility [ConnectionAttacher] caller's two effects are ordered
// live-first: the attach dials, handshakes, discovers and REGISTERS the server,
// and only then is the descriptor written onto the revision spine. Production
// wiring uses [ConnectionPreparer] and does not enter this path.
//
// So the write can fail AFTER the server is live. The reachable case is the
// expected-revision precondition: a caller whose base moved between its read
// and its add is answered `revision_conflict`, and the wire contract states
// that on that answer NOTHING is persisted. Before this compensation that was
// true of the SPINE and false of the world: no revision named the server, so
// `agent_config.remove_mcp_connection` answered ErrConnectionNotFound for a
// server that was dialed, registered and exposing tools — an unremovable live
// connection, and a Console stuck on `pending` because no terminal event ever
// fired. An inline wire-OAuth provider installed for the binding was left
// installed too; its uninstall existed only on the attach-FAILED path.
//
// # What it does, and what it deliberately does not
//
// Detach (idempotent, owner-scoped), uninstall a NEWLY-installed inline wire
// provider (a pre-existing bind-by-name provider is left alone — it was
// installed by its own set_oauth_provider and outlives this add), then emit
// `failed` carrying the scrubbed write error as the reason.
//
// It does NOT return an error and it does NOT mask the write failure: the
// caller still receives the original `recErr` (the 409), because the
// compensation is how the runtime keeps its own promise, not a second outcome
// for the caller to branch on. A compensation step that itself fails is logged
// LOUD and named precisely — a residual live server is a fact an operator must
// be told about, never swallowed (CLAUDE.md §13).
//
// # It compensates ONLY what THIS call created
//
// A same-owner same-name re-attach is ALLOWED and intended — it is the operator
// replacing their own connection, and the MCP registry supersedes the live
// registration in place. So the attach this compensates may have REPLACED a
// server the ACTIVE revision still names rather than created a new one, and an
// unconditional teardown would then make a REFUSED write DESTRUCTIVE: catalog
// tools deregistered, transport closed, a terminal `failed` emitted for a
// healthy connection, and in-flight runs stripped of its tools until the next
// run-start reconcile re-attaches it. That is the same broken "a refusal
// changes nothing" claim this compensation exists to keep, in the opposite
// direction.
//
// So both teardowns are scoped by the ACTIVE revision, read HERE rather than
// before the attach: the reachable failure is a moved base, so the winning
// sibling revision — not the one this caller read — is what decides whether the
// name is still declared. A connection the active revision still names is left
// LIVE; a wire provider it still names is left INSTALLED. The predicate is
// deliberately the same one the run-start reconcile uses to decide "keep it
// attached", so the two cannot drift into contradicting each other.
//
// The residual, stated rather than papered over: when a racing sibling's
// winning revision declares the same connection with a DIFFERENT descriptor,
// the retained live server carries THIS call's descriptor while the revision
// names the sibling's, and the reconcile's attach pass skips names that are
// already live — so the divergence persists until that connection is next
// detached. Retaining a declared connection is still the better failure: a
// declared-but-dark connection breaks every run immediately, a descriptor
// divergence breaks none.
func (s *Service) compensateAttach(ctx context.Context, q identity.Quadruple, id identity.Identity, agentID string, desc agentcfg.MCPConnectionDescriptor, wireProvider *agentcfg.OAuthProviderDescriptor, wireProviderIsNew bool, cause error) {
	providerName := ""
	if wireProvider != nil {
		providerName = wireProvider.Name
	}
	connDeclared, providerDeclared, rErr := s.activeDeclarations(ctx, q, agentID, desc.Name, providerName)
	if rErr != nil {
		// Unknown is not absent. A refused pointer re-read cannot distinguish an
		// unnamed new attach from a same-name replacement that the winning active
		// revision still declares. Retain both live objects and report the residue;
		// unconditional teardown is destructive in the latter case and was #653.
		connDeclared, providerDeclared = true, true
		s.logger.ErrorContext(ctx, "agent-config: could not read the active revision to scope legacy attach compensation — retained the live connection and provider because an unknown pointer outcome is not authorization to detach",
			"agent_id", agentID, "server_id", desc.Name, "error", rErr.Error(), "cause", cause.Error())
	}

	switch {
	case connDeclared:
		// The add REPLACED a still-declared connection. Tearing it down is the
		// collateral, not the compensation — leave it live and say so.
		s.logger.WarnContext(ctx, "agent-config: mcp connection revision write refused; the connection is still declared by the ACTIVE revision, so the live server was left in place (this add replaced a still-declared connection rather than creating one)",
			"agent_id", agentID, "server_id", desc.Name, "cause", cause.Error())
	case s.detacher == nil:
		// Fail loud rather than pretend. A runtime wired with an attacher but
		// no detacher genuinely cannot undo the attach, and the operator needs
		// to know a live server outlived its refused write.
		s.logger.ErrorContext(ctx, "agent-config: mcp connection attached but its revision write failed AND no ConnectionDetacher is wired — the server is LIVE and unnamed by any revision",
			"agent_id", agentID, "server_id", desc.Name, "error", cause.Error())
	default:
		if dErr := s.detacher.DetachConnection(ctx, id.TenantID, agentID, desc.Name); dErr != nil {
			s.logger.ErrorContext(ctx, "agent-config: compensating detach after a failed connection revision write FAILED — the server may still be live and unnamed by any revision",
				"agent_id", agentID, "server_id", desc.Name, "error", dErr.Error(), "cause", cause.Error())
		}
	}

	// A provider the active revision still names is not this call's to uninstall,
	// whatever the pre-attach install reported: uninstalling it would strip
	// southbound bearer injection from every still-declared connection bound to
	// it.
	if providerDeclared && wireProviderIsNew {
		s.logger.WarnContext(ctx, "agent-config: mcp connection revision write refused; the inline wire oauth provider is still declared by the ACTIVE revision, so it was left installed",
			"agent_id", agentID, "server_id", desc.Name, "provider", providerName)
	}
	s.uninstallWireProvider(ctx, id, agentID, wireProvider, wireProviderIsNew && !providerDeclared, "failed connection revision write")

	// The terminal event ALWAYS fires — a transient `pending` that never closes
	// is the silent half of this defect. On the retained path the reason names
	// the outcome precisely, because a bare `failed` describes a connection that
	// is healthy, declared and still serving tools.
	reason := safeReason(cause)
	if connDeclared {
		reason = boundedReason(retainedConnectionReasonPrefix + reason)
	}
	s.emitConnectionLifecycle(ctx, q, agentID, desc, ConnectionStateFailed, "", "", reason)
}

// retainedConnectionReasonPrefix marks the terminal `failed` lifecycle reason
// emitted when the refused write tore NOTHING down because the connection is
// still declared by the active revision. The lifecycle state set is closed
// (pending | online | failed | auth_required) and this ADD did fail, so
// `failed` is the honest terminal state for the attempt — but a reader of the
// per-connection stream would otherwise read it as "this connection is dead",
// which is exactly wrong here. The reason carries the distinction the state
// cannot.
const retainedConnectionReasonPrefix = "add refused; the pre-existing connection is still declared by the active revision and was NOT torn down: "

// activeDeclarations reads the agent's ACTIVE agent-scope revision ONCE and
// reports whether it currently names the connection and whether it currently
// names the OAuth provider. An empty providerName reports false for the
// provider half without searching.
//
// It is the compensation's scoping read: "is this name still DECLARED?" is the
// same question the run-start reconcile asks before it decides to keep a live
// server attached, so answering it from the same place keeps a refused write
// and a reconcile sweep from contradicting each other. The read takes the
// caller-held per-agent write lock so a concurrent edit cannot be observed
// half-applied. A read error is returned to the caller (never swallowed into a
// false).
func (s *Service) activeDeclarations(ctx context.Context, q identity.Quadruple, agentID, connName, providerName string) (connDeclared, providerDeclared bool, err error) {
	active, hasActive, aErr := s.registry.Active(ctx, identity.Quadruple{Identity: q.Identity}, agentID, agentcfg.ConfigScopeAgent)
	if aErr != nil {
		return false, false, aErr
	}
	if !hasActive {
		return false, false, nil
	}
	for _, d := range active.Payload.ConnectionDescriptors() {
		if d.Name == connName {
			connDeclared = true
			break
		}
	}
	if providerName != "" {
		for _, p := range active.Payload.OAuthProviderDescriptors() {
			if p.Name == providerName {
				providerDeclared = true
				break
			}
		}
	}
	return connDeclared, providerDeclared, nil
}

// uninstallWireProvider rolls back an inline wire-OAuth provider that THIS add
// newly installed, so an add that did not persist leaves no orphan live
// provider. A bind-by-NAME provider that pre-existed this call
// (wireProviderIsNew == false) is left in place: it was installed by its own
// set_oauth_provider and survives this add's outcome. A nil provider, or a
// runtime with no installer wired, is a no-op. `phase` names the failure the
// rollback is compensating so the two call sites' log lines stay
// distinguishable.
func (s *Service) uninstallWireProvider(ctx context.Context, id identity.Identity, agentID string, wireProvider *agentcfg.OAuthProviderDescriptor, wireProviderIsNew bool, phase string) {
	if !wireProviderIsNew || wireProvider == nil || s.providerInstaller == nil {
		return
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if uErr := s.providerInstaller.UninstallProvider(cleanupCtx, id.TenantID, agentID, wireProvider.Name); uErr != nil {
		s.logger.WarnContext(cleanupCtx, "agent-config: rollback of inline wire oauth provider failed",
			"agent_id", agentID, "provider", wireProvider.Name, "phase", phase, "error", uErr.Error())
	}
}

// prepareWireOAuthBinding builds a connection's dev-gated wire OAuth binding
// privately before MCP preparation. It returns the descriptor to persist, a
// compatibility "new declaration" bit used only by the legacy live-first
// adapter, and a reversible unpublished provider. Nothing enters the shared
// provider set here.
//
//   - Inline binding (`connection.oauth`): gate + validate the wire descriptor,
//     DERIVE the downstream sink from the connection's own URL (never a wire
//     field), prepare the provider, and rewrite the connection's OAuthProvider
//     to its eventual shared-set name.
//   - Name binding (`oauth_provider`) to an already-installed WIRE provider (e.g.
//     from a prior set_oauth_provider, whose downstream sink was deferred): re-derive
//     that provider's sink from THIS connection's URL and prepare its replacement.
//
// A plain name binding to a non-wire (broker-pull or interactive) provider is a
// no-op here (nil, false) — the existing resolution path handles it.
func (s *Service) prepareWireOAuthBinding(ctx context.Context, id identity.Identity, agentID string, desc *agentcfg.MCPConnectionDescriptor, inline *prototypes.AgentConfigOAuthProviderDescriptor) (*agentcfg.OAuthProviderDescriptor, bool, PreparedOAuthProvider, error) {
	if inline != nil {
		pdesc, err := s.gateAndValidateOAuthProviderDescriptor(*inline)
		if err != nil {
			return nil, false, nil, err
		}
		if pdesc.TokenURL == "" {
			// A name-only descriptor inline is pointless (it references a boot
			// broker but installs nothing derivable) — direct the operator to the
			// name binding instead. Fail loud rather than silently install.
			return nil, false, nil, fmt.Errorf("%w: inline oauth binding must carry a wire descriptor (token_url); bind an already-installed provider with oauth_provider (name) instead", ErrInvalidConnection)
		}
		host := config.NormalizeDownstreamHost(desc.URL)
		if host == "" {
			return nil, false, nil, fmt.Errorf("%w: cannot derive a downstream host from the connection url %q", ErrInvalidConnection, desc.URL)
		}
		pdesc.AllowedDownstreamHosts = []string{host}
		if _, boot := s.bootDeclaredOAuthProviders[pdesc.Name]; boot {
			return nil, false, nil, fmt.Errorf("%w: %q", ErrBootDeclaredProvider, pdesc.Name)
		}
		if s.providerPreparer == nil {
			return nil, false, nil, ErrProviderInstallUnavailable
		}
		// Does the active revision ALREADY name a provider by this name? If it
		// does, this install is an upsert over a provider that outlives this add,
		// so a rollback must not uninstall it. Read before the install (nothing
		// live has happened yet, so a read error is a plain refusal).
		_, alreadyDeclared, dErr := s.activeDeclarations(ctx, identity.Quadruple{Identity: id}, agentID, "", pdesc.Name)
		if dErr != nil {
			return nil, false, nil, dErr
		}
		prepared, err := s.providerPreparer.PrepareProvider(ctx, id.TenantID, agentID, pdesc)
		if err != nil {
			return nil, false, nil, err
		}
		desc.OAuthProvider = pdesc.Name
		return &pdesc, !alreadyDeclared, prepared, nil
	}
	if strings.TrimSpace(desc.OAuthProvider) == "" {
		return nil, false, nil, nil
	}
	// Name binding: re-derive iff the named provider is a wire provider whose
	// downstream sink was deferred at install (set_oauth_provider). The caller
	// holds the per-owner transaction lock across this read and activation.
	q := identity.Quadruple{Identity: id}
	active, hasActive, err := s.registry.Active(ctx, q, agentID, agentcfg.ConfigScopeAgent)
	if err != nil {
		return nil, false, nil, err
	}
	if !hasActive {
		return nil, false, nil, nil
	}
	for _, p := range active.Payload.OAuthProviderDescriptors() {
		if p.Name != desc.OAuthProvider || p.TokenURL == "" {
			continue
		}
		host := config.NormalizeDownstreamHost(desc.URL)
		if host == "" {
			return nil, false, nil, fmt.Errorf("%w: cannot derive a downstream host from the connection url %q", ErrInvalidConnection, desc.URL)
		}
		p.AllowedDownstreamHosts = []string{host}
		if s.providerPreparer == nil {
			return nil, false, nil, ErrProviderInstallUnavailable
		}
		prepared, err := s.providerPreparer.PrepareProvider(ctx, id.TenantID, agentID, p)
		if err != nil {
			return nil, false, nil, err
		}
		return &p, false, prepared, nil
	}
	return nil, false, nil, nil
}

// parkForAuth records a pause on the unified pause/resume primitive for an
// auth-required MCP attach. The pause Payload carries ONLY non-secret
// metadata (agent id, server name, the OAuth reason marker) — never a
// credential or auth code. The agent-bound token keys by the agent's
// registration identity. The continuation carries only agent/server identity
// and the canonical non-secret descriptor fingerprint.
func (s *Service) parkForAuth(ctx context.Context, id identity.Identity, agentID string, desc agentcfg.MCPConnectionDescriptor) (pauseresume.Token, error) {
	continuation := mcpAddContinuation(agentID, desc)
	pause, err := s.coordinator.Request(ctx, pauseresume.PauseRequest{
		Identity:     id,
		Reason:       pauseresume.ReasonExternalEvent, // tool-side OAuth completion
		Continuation: &continuation,
		Payload: map[string]any{
			"kind":     "mcp_oauth",
			"agent_id": agentID,
			"server":   desc.Name,
		},
	})
	if err != nil {
		return "", fmt.Errorf("agentcfg/protocol: park mcp oauth pause: %w", err)
	}
	return pause.Token, nil
}

func (s *Service) rejectProducerPause(ctx context.Context, authErr *toolauth.ErrAuthRequired) error {
	if authErr == nil || authErr.PauseToken == "" || s.coordinator == nil {
		return nil
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := s.coordinator.Resume(cleanupCtx, pauseresume.Token(authErr.PauseToken), pauseresume.DecisionReject, nil); err != nil {
		return fmt.Errorf("reject producer oauth pause: %w", err)
	}
	return nil
}

// emitConnectionLifecycle publishes one runtime MCP-attach lifecycle event.
// A nil bus is a no-op (the add still completes — event emission is
// observability). A publish failure is logged loud (a privileged config
// mutation never goes to the bus silently — CLAUDE.md §13) but does not undo
// the add. NO secret is ever carried.
func (s *Service) emitConnectionLifecycle(ctx context.Context, q identity.Quadruple, agentID string, desc agentcfg.MCPConnectionDescriptor, state ConnectionState, revisionID, pauseToken, reason string) {
	if s.bus == nil {
		return
	}
	now := s.now().UTC()
	t := lifecycleEventType(state)
	ev := events.Event{
		Type:       t,
		Identity:   q,
		OccurredAt: now,
		Payload: agentcfg.MCPConnectionLifecyclePayload{
			Author:     q,
			AgentID:    agentID,
			ServerID:   desc.Name,
			Transport:  string(desc.Transport),
			State:      string(state),
			RevisionID: revisionID,
			PauseToken: pauseToken,
			Reason:     reason,
			OccurredAt: now,
		},
	}
	if err := s.bus.Publish(ctx, ev); err != nil {
		s.logger.WarnContext(ctx, "agent-config: publish mcp.connection lifecycle event failed",
			"event_type", string(t), "agent_id", agentID, "server_id", desc.Name, "state", string(state), "error", err.Error())
	}
}

// lifecycleEventType maps a lifecycle state onto its canonical event type.
func lifecycleEventType(state ConnectionState) events.EventType {
	switch state {
	case ConnectionStatePending:
		return agentcfg.EventTypeMCPConnectionPending
	case ConnectionStateOnline:
		return agentcfg.EventTypeMCPConnectionAdded
	case ConnectionStateAuthRequired:
		return agentcfg.EventTypeMCPConnectionAuthRequired
	default:
		return agentcfg.EventTypeMCPConnectionFailed
	}
}

// descriptorToWire projects a domain descriptor onto the wire shape.
func descriptorToWire(d agentcfg.MCPConnectionDescriptor) prototypes.AgentConfigMCPConnectionDescriptor {
	return prototypes.AgentConfigMCPConnectionDescriptor{
		Name:                         d.Name,
		Transport:                    string(d.Transport),
		Command:                      append([]string(nil), d.Command...),
		URL:                          d.URL,
		OAuthProvider:                d.OAuthProvider,
		MetaAnnotations:              cloneAnnotations(d.MetaAnnotations),
		OAuthDiscoveryAllowedOrigins: append([]string(nil), d.OAuthDiscoveryAllowedOrigins...),
		Injection:                    injectionDescriptorToWire(d.Injection),
		ArtifactByteEligible:         d.ArtifactByteEligible,
		ArtifactParams:               d.CloneArtifactParams(),
	}
}

// injectionDescriptorToWire projects a domain injection descriptor onto the wire
// shape (nil for nil). NON-SECRET throughout — the mapping only, never a value.
func injectionDescriptorToWire(d *agentcfg.MCPCredentialInjectionDescriptor) *prototypes.AgentConfigMCPCredentialInjectionDescriptor {
	if d == nil {
		return nil
	}
	return &prototypes.AgentConfigMCPCredentialInjectionDescriptor{
		Provider:      d.Provider,
		Form:          d.Form,
		Header:        d.Header,
		BasicUsername: d.BasicUsername,
		MetaKey:       d.MetaKey,
	}
}

// normalizeArtifactParams validates and normalises a connection's
// egress-substitution artifact-parameter mapping.
//
// Two rules are enforced here, both fail-loud with ErrInvalidConnection
// so the wire handler's 400 fires and NOTHING is persisted:
//
//  1. A mapping REQUIRES the byte-eligibility declaration on the same
//     connection. The flag IS the containment boundary for egress
//     substitution, so a mapping without it is refused rather than
//     quietly stored inert — a persisted mapping that does nothing is
//     exactly the shape an operator would later read as "egress is
//     configured".
//  2. The mapping's SHAPE goes through the single shared authority in
//     internal/config (non-empty tool name, at least one parameter, no
//     empty or duplicate parameter name), the same way the `_meta`
//     annotation rules do — so the boot validator, both persistence
//     doors and the driver's attach enforce ONE rule set.
//
// The result is deterministically ordered (parameters sorted per tool)
// and never nil-vs-empty ambiguous, so the two doors record identical
// bytes for identical logical input and a re-order cannot perturb the
// revision's content hash.
//
// The half this cannot do is the schema check — whether the server
// itself declares each mapped parameter, string-typed. That needs the
// live discovered tool set and runs at attach.
func normalizeArtifactParams(eligible bool, in map[string][]string) (map[string][]string, error) {
	if len(in) == 0 {
		return nil, nil
	}
	if !eligible {
		return nil, fmt.Errorf("%w: artifact_params requires artifact_byte_eligible on the same connection — the eligibility declaration is the containment boundary for egress substitution, so a mapping without it is refused rather than persisted inert", ErrInvalidConnection)
	}
	canonical, err := config.NormalizeMCPArtifactParams(config.MCPArtifactParams(in))
	if err != nil {
		return nil, fmt.Errorf("%w: artifact_params: %s", ErrInvalidConnection, err.Error())
	}
	out := make(map[string][]string, len(canonical))
	for tool, params := range canonical {
		out[tool] = append([]string(nil), params...)
	}
	return out, nil
}

// normalizeDiscoveryOrigins trims, de-duplicates, and validates a discovery
// allow-list through the SHARED origin validator (config.ValidateDiscoveryOrigin
// — one implementation, two call sites) so a malformed entry fails loud with a
// typed ErrInvalidConnection naming the entry. The result is deterministically
// ordered (input order, first occurrence) and never nil-vs-empty ambiguous: an
// all-empty input returns nil. Shared by the add path and the
// set_mcp_discovery_origins write.
func normalizeDiscoveryOrigins(in []string) ([]string, error) {
	if len(in) == 0 {
		return nil, nil
	}
	seen := make(map[string]struct{}, len(in))
	var out []string
	for _, raw := range in {
		o := strings.TrimSpace(raw)
		if o == "" {
			continue
		}
		if err := config.ValidateDiscoveryOrigin(o); err != nil {
			return nil, fmt.Errorf("%w: oauth_discovery_allowed_origins entry %q: %s", ErrInvalidConnection, o, err.Error())
		}
		if _, dup := seen[o]; dup {
			continue
		}
		seen[o] = struct{}{}
		out = append(out, o)
	}
	return out, nil
}

// validateConnectionAnnotations rejects a `meta_annotations` declaration that
// is not a well-formed set of `_meta` PATHS — the wire mirror of the
// config-time rule, serving both `add_mcp_connection` and the full-payload
// `agent_config.set_revision` door.
//
// A dotted annotation key nests, exactly like `injection.meta_key`, so each key
// is validated as a path (whole-key AND per-segment reserved guard, no empty
// segment, depth cap) and the declared set is checked for collisions — against
// itself and against the injection mapping's own `_meta` path, which is why the
// injection descriptor's `meta_key` is threaded in. Every rule comes from the
// single shared authority in `internal/config`, so this door cannot drift from
// boot validation, the attach door, or the driver's merge-time re-check.
func validateConnectionAnnotations(m map[string]string, injectionMetaKey string) error {
	keys := make([]string, 0, len(m))
	for k := range m {
		if err := config.ValidateMCPMetaAnnotationKey(k); err != nil {
			return fmt.Errorf("%w: meta_annotations %s", ErrInvalidConnection, err.Error())
		}
		keys = append(keys, k)
	}
	if err := config.ValidateMCPMetaPathCollisions(keys, injectionMetaKey); err != nil {
		return fmt.Errorf("%w: meta_annotations %s", ErrInvalidConnection, err.Error())
	}
	return nil
}

// wireInjectionMetaPath returns the wire descriptor's credential-injection
// `_meta` key path, or "" when the connection declares no injection or injects
// somewhere other than `_meta` (a header / `Authorization: Basic` value writes
// no `_meta` node, so it cannot collide with an annotation path).
func wireInjectionMetaPath(in *prototypes.AgentConfigMCPCredentialInjectionDescriptor) string {
	if in == nil || strings.TrimSpace(in.Form) != config.MCPInjectionFormMeta {
		return ""
	}
	return strings.TrimSpace(in.MetaKey)
}

// cloneAnnotations returns a defensive copy of m (nil for empty).
func cloneAnnotations(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// SafeReason is the exported door onto [safeReason] for the OTHER attach caller
// in the runtime: the run-start re-attach leg, which reports a refused or
// unreachable declared connection on its own canonical event and must scrub that
// reason through the SAME implementation this package's add path uses. One
// scrubber, two call sites — a second copy would drift on the next pattern added
// (CLAUDE.md §13).
func SafeReason(err error) string { return safeReason(err) }

// safeReason returns a bounded, SECRET-SCRUBBED, operator-facing failure
// reason from an attach error. The attach error is the driver's own wrapped
// step error (provider.Connect / provider.Discover / ...). The driver does
// not log headers, but a stdlib transport error can still echo a connection
// URL — and a URL can carry credentials in its userinfo (scheme://user:pass@)
// — so this defence-in-depth pass strips URL userinfo + common inline token
// patterns BEFORE the reason reaches the lifecycle event / logs (CLAUDE.md
// §7: no secret on an event payload, even via an error string). It is then
// length-bounded so a pathological error cannot bloat an event.
func safeReason(err error) string {
	if err == nil {
		return ""
	}
	return boundedReason(scrubReasonSecrets(err.Error()))
}

// boundedReason length-bounds an already-scrubbed operator-facing reason so a
// pathological error cannot bloat an event. Split out of [safeReason] because
// the compensation composes a reason from a fixed prefix plus a scrubbed cause
// and must bound the RESULT, not just the cause.
func boundedReason(msg string) string {
	const maxReason = 512
	if len(msg) > maxReason {
		return msg[:maxReason] + "…"
	}
	return msg
}

// reasonSecretPatterns redacts credentials an error string might carry:
// URL userinfo (scheme://user:pass@host → scheme://***@host) and inline auth
// tokens (Bearer <t>; authorization / api-key / token / password / secret
// =|: <v>). Over-redaction in an operator-facing failure reason is
// acceptable; a leak is not.
var reasonSecretPatterns = []struct {
	re   *regexp.Regexp
	repl string
}{
	{regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.\-]*://)[^/@\s]+@`), `${1}***@`},
	{regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._\-]+`), `Bearer ***`},
	{regexp.MustCompile(`(?i)\b(authorization|api[-_]?key|apikey|access[-_]?token|token|password|secret)(\s*[:=]\s*)\S+`), `${1}${2}***`},
}

func scrubReasonSecrets(s string) string {
	for _, p := range reasonSecretPatterns {
		s = p.re.ReplaceAllString(s, p.repl)
	}
	return s
}
