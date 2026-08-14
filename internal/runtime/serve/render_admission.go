// render_admission.go — the serve-band composition of the HA-56 fresh
// render-admission surface: the ONE restart-stable KEK-backed sealer
// shared across every runtime authority, and the production
// RenderAdmissionGate the Protocol AppsSurface runs before every mint
// and before every callback verification.
//
// # The one shared sealer
//
// Every runtime authority that needs a restart-stable sealing key —
// the OAuth token store (constructed inside tools/auth), signed
// capability admissions, HA-61 skill-import proposal tokens, and HA-56
// render admissions — derives from ONE immutable auth.Sealer resolved
// from the operator's deployment-shared `tools.oauth_token_kek_env`.
// resolveSharedKEKSealer never constructs a second sealer over the same
// key: when a credential broker is declared, the ProviderBuilder
// already holds the sealer and it is reused; otherwise exactly one
// instance is built from the env. Resolution is CONSUMER-INDEPENDENT:
// an explicitly configured env resolves the shared sealer even when
// the render-admission surface is disabled and no broker is present
// (HA-61 import needs it), and an enabled render-admission surface
// with an empty env name, an unset/invalid KEK, or a construction
// failure fails readiness LOUD even when no OAuth provider or
// credential broker is declared.
//
// # The explicit opt-in (sealer availability is NOT feature enablement)
//
// The HA-56 authority+gate pair is wired ONLY when the operator's
// `tools.mcp_app_render_admission.enabled` flag is true. A resolved
// sealer — including a declared credential broker's — never enables
// the surface by itself: ordinary OAuth/broker configuration alone
// must not open the opt-in admission read/callback. Disabled means an
// opt-in admission read/callback fails through the surface's compatible
// unwired behavior while ordinary resource reads and the legacy
// binding remain byte-for-byte unchanged.
//
// # The production gate
//
// renderAdmissionGate implements protocol.RenderAdmissionGate: the
// fail-closed authorization seam the AppsSurface invokes BEFORE every
// mint and BEFORE every callback verification. It re-runs the CURRENT
// render-admission conditions for the exact (server, resource) tuple
// under the ctx identity + the request's reach-admitted effective
// agent: verified identity, durable erasure (pending ledger / terminal
// tombstone), retirement, current session/agent exposure (the admin
// desired-state UNION the session overlay's narrow-only disables — the
// same composition the run-start planner-view applies), exact server +
// current `ui://` resource, and paused/disabled state. The effective
// agent is read from ctx (tools.WithEffectiveAgentConfig) on EVERY
// call — never a boot/default fallback — and an absent stamp fails
// closed. It returns the exact CURRENT provider/catalog generation the
// admission binds — never an empty one — so a refresh/replacement/
// detach after mint changes the generation and the callback-time proof
// re-check refuses with zero callbacks.
package serve

import (
	"context"
	"errors"
	"fmt"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/agentcfg/sessionoverlay"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/mcpconsole/admission"
	"github.com/hurtener/Harbor/internal/protocol"
	"github.com/hurtener/Harbor/internal/sessions"
	"github.com/hurtener/Harbor/internal/tools"
	toolauth "github.com/hurtener/Harbor/internal/tools/auth"
	mcp "github.com/hurtener/Harbor/internal/tools/drivers/mcp"
)

// RenderAdmissionAuthorityDeps bundles the runtime collaborators the
// render-admission gate and authority need.
type RenderAdmissionAuthorityDeps struct {
	// Enabled is the EXPLICIT operator opt-in
	// (`tools.mcp_app_render_admission.enabled`). The authority+gate
	// pair is wired ONLY when it is true — sealer availability is NOT
	// feature enablement, so an OAuth broker sealer alone (or any
	// ordinary broker/credential configuration) never enables the
	// surface. Disabled keeps the compatible unwired surface: ordinary
	// resource reads and the legacy binding path work byte-for-byte,
	// and the opt-in mint / admission-backed callback fail through the
	// surface's unwired-seam posture.
	Enabled bool
	// Sessions is the session erasure authority (the exported Erased
	// seam). Mandatory when the surface is enabled.
	Sessions *sessions.Registry
	// AgentConfig is the agent-config desired-state registry the gate
	// reads retirement + current exposure from. Mandatory when the
	// surface is enabled.
	AgentConfig renderAdmissionAgentConfig
	// SessionOverlay is the session-safe narrow-only disable set the
	// gate unions into the admin exposure. May be nil (no overlay).
	SessionOverlay sessionoverlay.Store
	// Registry is the MCP driver registry the gate reads the exact
	// current provider/catalog generation from. Mandatory.
	Registry *mcp.Registry
	// Sealer is the ONE shared restart-stable KEK-backed sealer.
	Sealer toolauth.Sealer
	// AdmissionOptions are forwarded verbatim to the sealed authority
	// constructor (admission.New). Production callers pass nil (the
	// documented default TTL + wall clock apply); a caller that needs a
	// bounded review window or a controllable clock for the authority —
	// a focused test, an embedder with a strict admission lifetime —
	// supplies them here instead of re-implementing the wiring.
	AdmissionOptions []admission.Option
}

// ResolveSharedKEKSealer resolves the ONE restart-stable KEK-backed
// sealer shared by every runtime authority. It prefers the
// ProviderBuilder's already-constructed sealer (a declared credential
// broker) and otherwise constructs exactly one instance from the
// operator's deployment-shared `tools.oauth_token_kek_env`.
//
// The resolution is CONSUMER-INDEPENDENT: an explicitly configured
// `tools.oauth_token_kek_env` resolves the shared sealer regardless of
// whether the HA-56 render-admission surface is enabled — HA-61 import
// proposal tokens and signed admissions need it even when render
// admission is disabled and no OAuth broker is present. The HA-56 flag
// only controls whether the authority+gate pair is WIRED
// (WireRenderAdmission); sealer availability never enables it. A
// configured-but-unresolvable env (unset value, non-hex, wrong-length
// KEK, or a construction failure) fails loud even when the surface is
// disabled: the operator explicitly declared the key slot, so a broken
// one is a boot error — never a silent (nil, nil) that 501s HA-61
// import. When the ENABLED surface is at stake the error additionally
// names `tools.mcp_app_render_admission.enabled`.
//
// When no env is configured and no broker sealer exists, it returns
// (nil, nil) — no consumer needs a sealer — EXCEPT that an ENABLED
// surface with no env fails readiness loud (an enabled surface with an
// unresolvable shared KEK never falls back to the disabled surface).
func ResolveSharedKEKSealer(cfg *config.Config, builder *toolauth.ProviderBuilder) (toolauth.Sealer, error) {
	if builder != nil && builder.AdmissionSealer() != nil {
		return builder.AdmissionSealer(), nil
	}
	if env := cfg.Tools.OAuthTokenKEKEnv; env != "" {
		sealer, err := toolauth.NewSealerFromEnv(env)
		if err != nil {
			if cfg.Tools.MCPAppRenderAdmission.Enabled {
				return nil, fmt.Errorf("tools.mcp_app_render_admission.enabled: %w", err)
			}
			return nil, fmt.Errorf("tools.oauth_token_kek_env: %w", err)
		}
		return sealer, nil
	}
	if cfg.Tools.MCPAppRenderAdmission.Enabled {
		// An enabled surface with NO configured env cannot resolve the
		// shared KEK — fail readiness loud, naming the surface.
		return nil, fmt.Errorf("tools.mcp_app_render_admission.enabled: tools.oauth_token_kek_env must be set (an enabled render-admission surface requires a resolvable shared KEK)")
	}
	return nil, nil
}

// renderAdmissionAgentConfig is the narrow desired-state read seam the
// render-admission gate needs: the fresh active revision (current
// exposure) plus the retirement gate. agentcfg.RetirementRegistry
// satisfies it; the interface exists so the gate never couples to a
// concrete registry.
type renderAdmissionAgentConfig interface {
	Active(ctx context.Context, id identity.Quadruple, agentID string, scope agentcfg.ConfigScope) (agentcfg.Revision, bool, error)
	RetirementStatus(ctx context.Context, id identity.Quadruple, agentID string) (agentcfg.RetirementStatus, bool, error)
}

// renderAdmissionGate is the production protocol.RenderAdmissionGate.
// Immutable after construction and safe for concurrent reuse by N
// goroutines — per-call state lives in ctx and the call's arguments.
type renderAdmissionGate struct {
	sessions       *sessions.Registry
	agentCfg       renderAdmissionAgentConfig
	sessionOverlay sessionoverlay.Store
	registry       *mcp.Registry
}

var _ protocol.RenderAdmissionGate = (*renderAdmissionGate)(nil)

// AuthorizeRender re-runs the CURRENT render-admission conditions for
// the exact (serverID, resourceURI) tuple under the ctx identity and
// the request's reach-admitted EFFECTIVE agent (read from ctx on every
// call — never a boot/default fallback, and an absent stamp fails
// closed), and returns the exact current provider/catalog generation
// to bind. Any refusal wraps protocol.ErrRenderAdmissionRefused; an
// empty generation is likewise a typed refusal — an admission never
// binds an empty generation.
func (g *renderAdmissionGate) AuthorizeRender(ctx context.Context, serverID, resourceURI string) (string, error) {
	// Verified identity is mandatory (AGENTS.md §6 rule 9): the
	// admission binds the exact triple and a call with no identity has
	// nothing to bind against.
	id, ok := identity.From(ctx)
	if !ok {
		return "", fmt.Errorf("%w: mcpconsole: render-admission gate: identity missing from ctx", protocol.ErrRenderAdmissionRefused)
	}
	if err := identity.Validate(id); err != nil {
		return "", fmt.Errorf("%w: mcpconsole: render-admission gate: %v", protocol.ErrRenderAdmissionRefused, err)
	}

	// The request's reach-admitted effective agent: the Protocol
	// surface stamps tools.WithEffectiveAgentConfig AFTER normal
	// identity, signed-reach, and lifecycle resolution on both the mint
	// and the callback-verification path. The gate reads that EXACT
	// stamped agent on every call and fails closed when it is absent —
	// no invented boot/default agent, no fallback. The sealed token and
	// the call-local proof bind the same effective agent, so the
	// authorization this gate performs must agree with them.
	agentID, ok := tools.EffectiveAgentConfigFrom(ctx)
	if !ok || agentID == "" {
		return "", fmt.Errorf("%w: mcpconsole: render-admission gate: effective agent missing from ctx (no reach-admitted agent to authorize)", protocol.ErrRenderAdmissionRefused)
	}

	// The exact server + current `ui://` resource: the server must have
	// a current provider/catalog generation (absent server, detach, or
	// a server whose discovery never established its descriptor set
	// fails closed), and the resource must be a `ui://` App document.
	generation, ok := g.registry.CurrentGeneration(serverID)
	if !ok || generation == "" {
		return "", fmt.Errorf("%w: mcpconsole: render-admission gate: server %q has no current provider/catalog generation", protocol.ErrRenderAdmissionRefused, serverID)
	}
	if !mcp.IsUIResourceURI(resourceURI) {
		return "", fmt.Errorf("%w: mcpconsole: render-admission gate: resource %q is not a ui:// MCP App document", protocol.ErrRenderAdmissionRefused, resourceURI)
	}

	// Durable erasure: an erased (or being-erased) session never mints.
	if g.sessions != nil {
		erased, err := g.sessions.Erased(ctx, id)
		if err != nil {
			return "", fmt.Errorf("mcpconsole: render-admission gate: erasure probe: %w", err)
		}
		if erased {
			return "", fmt.Errorf("%w: mcpconsole: render-admission gate: session %q is erased", protocol.ErrRenderAdmissionRefused, id.SessionID)
		}
	}

	// Retirement + current session/agent exposure: read from the
	// CURRENT agent-config desired state (never a stale run snapshot)
	// for the request's EFFECTIVE agent, unioned with the session
	// overlay's narrow-only disables — the same composition the
	// run-start planner-view projection applies.
	if g.agentCfg != nil {
		q := identity.Quadruple{Identity: id}
		_, retired, err := g.agentCfg.RetirementStatus(ctx, q, agentID)
		if err != nil {
			return "", fmt.Errorf("mcpconsole: render-admission gate: retirement gate: %w", err)
		}
		if retired {
			return "", fmt.Errorf("%w: mcpconsole: render-admission gate: agent %q is retired", protocol.ErrRenderAdmissionRefused, agentID)
		}
		rev, has, err := g.agentCfg.Active(ctx, q, agentID, agentcfg.ConfigScopeAgent)
		if err != nil {
			return "", fmt.Errorf("mcpconsole: render-admission gate: read active config: %w", err)
		}
		var pausedServers, disabledTools []string
		if has && rev.Payload.ToolExposure != nil {
			pausedServers = rev.Payload.PausedServers()
			disabledTools = rev.Payload.DisabledTools()
		}
		if g.sessionOverlay != nil {
			overlay, _, oerr := g.sessionOverlay.Get(ctx, q, agentID)
			if oerr != nil {
				return "", fmt.Errorf("mcpconsole: render-admission gate: read session overlay: %w", oerr)
			}
			pausedServers = append(pausedServers, overlay.DisabledServers...)
			disabledTools = append(disabledTools, overlay.DisabledTools...)
		}
		for _, s := range pausedServers {
			if s == serverID {
				return "", fmt.Errorf("%w: mcpconsole: render-admission gate: server %q is paused", protocol.ErrRenderAdmissionRefused, serverID)
			}
		}
		// Per-tool disable is checked by name at callback dispatch (the
		// requested tool is only known then); at mint time server pause
		// is binding while the tool axis is not yet addressable. The
		// disabled tool set is still part of the exposure union the
		// callback-time gate re-checks.
		_ = disabledTools
	}

	return generation, nil
}

// WireRenderAdmission builds the render-admission authority + gate
// pair ONLY when the operator explicitly opted in
// (`tools.mcp_app_render_admission.enabled`). Sealer availability is
// NOT feature enablement: a resolved shared sealer (including a
// declared credential broker's) never wires the surface by itself —
// disabled means the compatible unwired surface (ordinary reads and
// the legacy binding unchanged, the opt-in mint / admission-backed
// callback failing through the surface's unwired-seam posture).
//
// When enabled, both halves are wired TOGETHER (the surface rejects a
// half-wired pair at construction), and a missing shared KEK sealer
// fails construction LOUD — the readiness-failure posture an enabled
// surface with an unresolvable `tools.oauth_token_kek_env` must take,
// never a silent fallback to the disabled surface. The Sessions
// registry (the durable erasure probe) and the AgentConfig reader (the
// retirement + current-exposure gate) are MANDATORY collaborators of
// the enabled gate: the production gate may never treat a missing
// erasure or retirement/current-exposure reader as permission to skip
// that authorization check, so a nil collaborator fails construction
// LOUD rather than building a gate that silently skips it.
func WireRenderAdmission(deps RenderAdmissionAuthorityDeps) (protocol.RenderAdmissionAuthority, protocol.RenderAdmissionGate, error) {
	if !deps.Enabled {
		return nil, nil, nil
	}
	if deps.Sealer == nil {
		return nil, nil, errors.New("render admission: enabled but the shared KEK-backed sealer is unavailable (tools.mcp_app_render_admission.enabled requires a resolvable tools.oauth_token_kek_env)")
	}
	if deps.Registry == nil {
		return nil, nil, errors.New("render admission: MCP registry is required when the surface is enabled")
	}
	if deps.Sessions == nil {
		return nil, nil, errors.New("render admission: sessions registry is required when the surface is enabled (the durable erasure check may never be skipped)")
	}
	if deps.AgentConfig == nil {
		return nil, nil, errors.New("render admission: agent-config reader is required when the surface is enabled (the retirement/current-exposure checks may never be skipped)")
	}
	authority, err := admission.New(deps.Sealer, deps.AdmissionOptions...)
	if err != nil {
		return nil, nil, fmt.Errorf("render admission authority: %w", err)
	}
	gate := &renderAdmissionGate{
		sessions:       deps.Sessions,
		agentCfg:       deps.AgentConfig,
		sessionOverlay: deps.SessionOverlay,
		registry:       deps.Registry,
	}
	return authority, gate, nil
}
