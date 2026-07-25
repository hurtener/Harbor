package protocol

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
)

// addconnection.go — the admin-scoped `agent_config.add_mcp_connection`
// control method: the separable hard piece of the agent-config control
// plane. Unlike pause/resume (a projection-time flag on an already-attached
// server), adding a genuinely NEW MCP connection drives an async dial + the
// MCP `initialize` handshake + discover + register, with possible OAuth.
//
// # The lifecycle (fail loud)
//
// The add models an explicit lifecycle pending → online | failed |
// auth_required. The runtime emits `mcp.connection.pending` at the start,
// then exactly one terminal lifecycle event. A failure at any attach step
// records NO revision and registers NO server (a half-attached server is
// never registered — CLAUDE.md §13). A successful attach records the
// NON-SECRET descriptor as a config revision (preserving the skills /
// tool-exposure / prompt-layer / llm-params / hooks sections — the
// bidirectional section-merge invariant). An auth-required attach parks on
// the unified pause/resume primitive (the tool-side OAuth lineage — NOT a
// new auth dance).
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

// ConnectionAttacher drives the real MCP attach lifecycle for a runtime add:
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
	// pause/resume primitive awaiting authorization. NOTE: the resume
	// continuation that re-drives the attach to online is not yet
	// implemented — resume currently only releases the pause (tracked in
	// issue #375). The descriptor revision is recorded; the server comes
	// online on a subsequent (authorized) add.
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

	if s.attacher == nil {
		return prototypes.AgentConfigAddMCPConnectionResponse{}, ErrConnectionAttachUnavailable
	}

	// Dev-gated wire-carried OAuth binding. An inline wire descriptor installs a
	// NEW provider whose downstream sink is DERIVED from this connection's own URL
	// (never a wire field); a NAME binding to an already-installed WIRE provider
	// (e.g. from a prior set_oauth_provider) re-derives that provider's sink from
	// this connection's URL. Both fail-closed behind the opt-in (the gate is
	// inside gateAndValidateOAuthProviderDescriptor). The live install happens
	// BEFORE the attach so the binding resolves; a wire provider newly installed
	// here is rolled back if the attach fails (no orphan). Returns the domain
	// descriptor to persist alongside the connection revision.
	wireProvider, wireProviderIsNew, err := s.prepareWireOAuthBinding(ctx, id, req.AgentID, &desc, inlineOAuth)
	if err != nil {
		return prototypes.AgentConfigAddMCPConnectionResponse{}, err
	}

	// Emit the `pending` lifecycle transition before the dial (loud
	// observability — never a secret).
	s.emitConnectionLifecycle(ctx, q, req.AgentID, desc, ConnectionStatePending, "", "", "")

	// Drive the real attach. The headers flow to the transport ONLY — they
	// are NOT carried into the revision / diff / events below.
	attachErr := s.attacher.Attach(ctx, AttachRequest{
		Identity:                     id,
		AgentID:                      req.AgentID,
		Name:                         desc.Name,
		Transport:                    desc.Transport,
		Command:                      append([]string(nil), desc.Command...),
		URL:                          desc.URL,
		Headers:                      req.Headers,
		OAuthProvider:                desc.OAuthProvider,
		MetaAnnotations:              cloneAnnotations(desc.MetaAnnotations),
		OAuthDiscoveryAllowedOrigins: append([]string(nil), desc.OAuthDiscoveryAllowedOrigins...),
		Injection:                    desc.Injection.Clone(),
	})

	switch {
	case attachErr == nil:
		// Online — record the non-secret descriptor as a revision (preserving
		// the other sections; upserting the wire provider when one was installed)
		// and emit the terminal `added` event.
		rev, recErr := s.recordConnectionRevision(ctx, q, req.AgentID, desc, wireProvider)
		if recErr != nil {
			return prototypes.AgentConfigAddMCPConnectionResponse{}, recErr
		}
		s.emitConnectionLifecycle(ctx, q, req.AgentID, desc, ConnectionStateOnline, rev.RevisionID, "", "")
		view := revisionToWire(rev)
		return prototypes.AgentConfigAddMCPConnectionResponse{
			Revision:        &view,
			Connection:      descriptorToWire(desc),
			State:           string(ConnectionStateOnline),
			ProtocolVersion: prototypes.ProtocolVersion,
		}, nil

	case errors.Is(attachErr, ErrAuthRequired):
		// The server needs authorization — park on the unified pause/resume
		// primitive (NOT a new auth dance). Fail loud if no coordinator is
		// wired rather than dropping the auth requirement.
		if s.coordinator == nil {
			return prototypes.AgentConfigAddMCPConnectionResponse{}, ErrCoordinatorUnavailable
		}
		rev, recErr := s.recordConnectionRevision(ctx, q, req.AgentID, desc, wireProvider)
		if recErr != nil {
			return prototypes.AgentConfigAddMCPConnectionResponse{}, recErr
		}
		token, parkErr := s.parkForAuth(ctx, id, req.AgentID, desc)
		if parkErr != nil {
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
		if wireProviderIsNew && wireProvider != nil && s.providerInstaller != nil {
			if uErr := s.providerInstaller.UninstallProvider(ctx, id.TenantID, req.AgentID, wireProvider.Name); uErr != nil {
				s.logger.WarnContext(ctx, "agent-config: rollback of inline wire oauth provider after failed attach failed",
					"agent_id", req.AgentID, "provider", wireProvider.Name, "error", uErr.Error())
			}
		}
		reason := safeReason(attachErr)
		s.emitConnectionLifecycle(ctx, q, req.AgentID, desc, ConnectionStateFailed, "", "", reason)
		return prototypes.AgentConfigAddMCPConnectionResponse{
			Connection:      descriptorToWire(desc),
			State:           string(ConnectionStateFailed),
			Reason:          reason,
			ProtocolVersion: prototypes.ProtocolVersion,
		}, nil
	}
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
	// Reserved / spec-prefixed / empty `_meta` annotation keys are rejected
	// for every transport (the runtime stamps the triple + agent_id + trace
	// context; an operator annotation must not shadow them).
	if err := validateConnectionAnnotations(c.MetaAnnotations); err != nil {
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
		return agentcfg.MCPConnectionDescriptor{
			Name:            name,
			Transport:       transport,
			Command:         append([]string(nil), c.Command...),
			MetaAnnotations: cloneAnnotations(c.MetaAnnotations),
		}, nil, nil, nil
	case agentcfg.MCPTransportHTTP:
		if strings.TrimSpace(c.URL) == "" {
			return agentcfg.MCPConnectionDescriptor{}, nil, nil, fmt.Errorf("%w: http transport requires a url", ErrInvalidConnection)
		}
		if len(c.Command) > 0 {
			return agentcfg.MCPConnectionDescriptor{}, nil, nil, fmt.Errorf("%w: http transport must not carry a command", ErrInvalidConnection)
		}
		origins, oerr := normalizeDiscoveryOrigins(c.OAuthDiscoveryAllowedOrigins)
		if oerr != nil {
			return agentcfg.MCPConnectionDescriptor{}, nil, nil, oerr
		}
		return agentcfg.MCPConnectionDescriptor{
			Name:                         name,
			Transport:                    transport,
			URL:                          strings.TrimSpace(c.URL),
			OAuthProvider:                strings.TrimSpace(c.OAuthProvider),
			MetaAnnotations:              cloneAnnotations(c.MetaAnnotations),
			OAuthDiscoveryAllowedOrigins: origins,
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
func (s *Service) recordConnectionRevision(ctx context.Context, q identity.Quadruple, agentID string, desc agentcfg.MCPConnectionDescriptor, wireProvider *agentcfg.OAuthProviderDescriptor) (agentcfg.Revision, error) {
	// Serialise the registry read-modify-write per agent (NOT the preceding
	// dial/handshake, which must not block a quick concurrent edit).
	defer s.lockAgent(q.TenantID, agentID)()
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
		providers = active.Payload.OAuthProviderDescriptors()
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
	return s.registry.SetRevision(ctx, q, agentID, agentcfg.ConfigScopeAgent, payload)
}

// prepareWireOAuthBinding resolves a connection's dev-gated wire OAuth binding
// BEFORE the attach, so the live binding resolves during attach. It returns the
// domain provider descriptor to persist (or nil when the connection carries no
// wire binding), and whether that provider was NEWLY installed by this call (so
// a failed attach can roll it back).
//
//   - Inline binding (`connection.oauth`): gate + validate the wire descriptor,
//     DERIVE the downstream sink from the connection's own URL (never a wire
//     field), install the provider owner-tagged, and rewrite the connection's
//     OAuthProvider to the installed name. Newly installed ⇒ rolled back on a
//     failed attach.
//   - Name binding (`oauth_provider`) to an already-installed WIRE provider (e.g.
//     from a prior set_oauth_provider, whose downstream sink was deferred): re-derive
//     that provider's sink from THIS connection's URL and re-install (upsert). Not
//     "new" (it pre-existed) ⇒ left in place on a failed attach.
//
// A plain name binding to a non-wire (broker-pull or interactive) provider is a
// no-op here (nil, false) — the existing resolution path handles it.
func (s *Service) prepareWireOAuthBinding(ctx context.Context, id identity.Identity, agentID string, desc *agentcfg.MCPConnectionDescriptor, inline *prototypes.AgentConfigOAuthProviderDescriptor) (*agentcfg.OAuthProviderDescriptor, bool, error) {
	if inline != nil {
		pdesc, err := s.gateAndValidateOAuthProviderDescriptor(*inline)
		if err != nil {
			return nil, false, err
		}
		if pdesc.TokenURL == "" {
			// A name-only descriptor inline is pointless (it references a boot
			// broker but installs nothing derivable) — direct the operator to the
			// name binding instead. Fail loud rather than silently install.
			return nil, false, fmt.Errorf("%w: inline oauth binding must carry a wire descriptor (token_url); bind an already-installed provider with oauth_provider (name) instead", ErrInvalidConnection)
		}
		host := config.NormalizeDownstreamHost(desc.URL)
		if host == "" {
			return nil, false, fmt.Errorf("%w: cannot derive a downstream host from the connection url %q", ErrInvalidConnection, desc.URL)
		}
		pdesc.AllowedDownstreamHosts = []string{host}
		if _, boot := s.bootDeclaredOAuthProviders[pdesc.Name]; boot {
			return nil, false, fmt.Errorf("%w: %q", ErrBootDeclaredProvider, pdesc.Name)
		}
		if s.providerInstaller == nil {
			return nil, false, ErrProviderInstallUnavailable
		}
		if err := s.providerInstaller.InstallProvider(ctx, id.TenantID, agentID, pdesc); err != nil {
			return nil, false, err
		}
		desc.OAuthProvider = pdesc.Name
		return &pdesc, true, nil
	}
	if strings.TrimSpace(desc.OAuthProvider) == "" {
		return nil, false, nil
	}
	// Name binding: re-derive iff the named provider is a wire provider whose
	// downstream sink was deferred at install (set_oauth_provider). Read the
	// active revision under the per-agent lock so a concurrent edit does not race
	// the read.
	q := identity.Quadruple{Identity: id}
	release := s.lockAgent(id.TenantID, agentID)
	active, hasActive, err := s.registry.Active(ctx, q, agentID, agentcfg.ConfigScopeAgent)
	release()
	if err != nil {
		return nil, false, err
	}
	if !hasActive {
		return nil, false, nil
	}
	for _, p := range active.Payload.OAuthProviderDescriptors() {
		if p.Name != desc.OAuthProvider || p.TokenURL == "" {
			continue
		}
		host := config.NormalizeDownstreamHost(desc.URL)
		if host == "" {
			return nil, false, fmt.Errorf("%w: cannot derive a downstream host from the connection url %q", ErrInvalidConnection, desc.URL)
		}
		p.AllowedDownstreamHosts = []string{host}
		if s.providerInstaller == nil {
			return nil, false, ErrProviderInstallUnavailable
		}
		if err := s.providerInstaller.InstallProvider(ctx, id.TenantID, agentID, p); err != nil {
			return nil, false, err
		}
		return &p, false, nil
	}
	return nil, false, nil
}

// parkForAuth records a pause on the unified pause/resume primitive for an
// auth-required MCP attach. The pause Payload carries ONLY non-secret
// metadata (agent id, server name, the OAuth reason marker) — never a
// credential or auth code. The agent-bound token keys by the agent's
// registration identity. NOTE: the resume continuation that re-drives the
// attach to online is not yet implemented — resume currently only releases
// the pause (tracked in issue #375).
func (s *Service) parkForAuth(ctx context.Context, id identity.Identity, agentID string, desc agentcfg.MCPConnectionDescriptor) (pauseresume.Token, error) {
	pause, err := s.coordinator.Request(ctx, pauseresume.PauseRequest{
		Identity: id,
		Reason:   pauseresume.ReasonExternalEvent, // tool-side OAuth completion
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

// validateConnectionAnnotations rejects empty / reserved / spec-prefixed
// annotation keys — the attach-time mirror of the config-time rule. The
// reserved set is the single shared authority (config.IsReservedMCPMetaKey)
// so the runtime-add gate can never drift from config validation or the MCP
// driver's merge-time re-check.
func validateConnectionAnnotations(m map[string]string) error {
	for k := range m {
		if strings.TrimSpace(k) == "" {
			return fmt.Errorf("%w: meta_annotations key must not be empty", ErrInvalidConnection)
		}
		if config.IsReservedMCPMetaKey(k) {
			return fmt.Errorf("%w: meta_annotations key %q is reserved (triple/agent_id/traceparent/tracestate and io.modelcontextprotocol/-prefixed keys are runtime-stamped)", ErrInvalidConnection, k)
		}
	}
	return nil
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
	msg := scrubReasonSecrets(err.Error())
	const maxReason = 512
	if len(msg) > maxReason {
		msg = msg[:maxReason] + "…"
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
