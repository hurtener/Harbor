// Package protocol implements the admin-scoped `agent_config.*` Protocol
// methods the Console agent-config control plane consumes:
//
//   - agent_config.get — read an agent's active config revision.
//   - agent_config.set_revision — write a new immutable revision.
//   - agent_config.list_revisions — the agent's revision chain.
//   - agent_config.diff — server-side compare of two revisions.
//   - agent_config.rollback — repoint the active pointer.
//   - agent_config.skills.{list,upsert,delete} — skills control (the
//     first consumer of the registry primitive; see skills.go).
//
// Every write is admin-scoped (the verified `auth.ScopeAdmin` claim,
// enforced at the wire handler — the agent-config authorization model) and identity-mandatory. A config
// edit applies to the agent's NEXT run (next-turn projection — never
// mid-flight, per the concurrent-reuse contract).
//
// # The seam (CLAUDE.md §4.4)
//
// The Service depends on the narrow `agentcfg.Registry` interface (the
// StateStore-backed concrete satisfies it) plus an optional
// `skills.SkillStore` for the skills consumer; tests inject fakes. The
// Service owns wire validation + the wire↔domain mapping; the registry
// owns persistence + the revision events; the SkillStore owns the skill
// rows + the skill events.
//
// # Identity is mandatory (CLAUDE.md §6 rule 9)
//
// Every method takes the wire request's IdentityScope. An incomplete
// triple fails closed with ErrIdentityRequired. The handler overlays the
// verified identity onto the request body, so a caller cannot target
// another identity's config through this surface.
//
// # Concurrent reuse
//
// A constructed *Service is immutable after NewService and safe to share
// across N goroutines: it holds only the registry / skill-store references
// + a clock + logger. Per-call state lives in arguments and locals.
package protocol

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/agentcfg/sessionoverlay"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
	"github.com/hurtener/Harbor/internal/skills"
)

// Sentinel errors the Service returns. The wire handler maps each onto a
// canonical Protocol Code + HTTP status; in-process callers compare with
// errors.Is.
var (
	// ErrIdentityRequired — the request carried an incomplete identity
	// triple or an empty agent id. Fails closed.
	ErrIdentityRequired = errors.New("agentcfg/protocol: identity scope incomplete")
	// ErrMisconfigured — NewService was called with a nil registry.
	ErrMisconfigured = errors.New("agentcfg/protocol: NewService missing a mandatory dependency")
	// ErrSkillsUnavailable — a skills method was called but no SkillStore
	// was wired into the Service.
	ErrSkillsUnavailable = errors.New("agentcfg/protocol: skills control not wired on this runtime")
	// ErrUnknownModel — a set_llm_params (or a set_revision carrying an
	// LLMParams.Model) named a model with no configured ModelProfile. The
	// per-agent model is validated at set time (parity with the tenant
	// model-swap), so an invalid model can never be persisted — fail loud,
	// never a silent fallback (CLAUDE.md §13).
	ErrUnknownModel = errors.New("agentcfg/protocol: unknown model (no configured ModelProfile)")
	// ErrInvalidLLMParams — a set_llm_params (or a set_revision carrying an
	// LLMParams section) supplied an out-of-range sampling value (temperature
	// outside [0,2], non-positive max-tokens, or an unknown reasoning-effort).
	// Validated at set time so an invalid value never reaches a run (parity
	// with runs.set_overrides), fail loud (CLAUDE.md §13).
	ErrInvalidLLMParams = errors.New("agentcfg/protocol: invalid LLM parameters")
)

// validLLMReasoningEffort is the canonical reasoning-effort taxonomy the
// per-agent LLM-params section accepts — the set the planner honours
// (off | low | medium | high).
var validLLMReasoningEffort = map[string]struct{}{
	"off": {}, "low": {}, "medium": {}, "high": {},
}

// Clock is the time source the Service stamps response timestamps from.
type Clock func() time.Time

// Service implements the admin-scoped agent-config methods.
type Service struct {
	registry agentcfg.Registry
	skills   skills.SkillStore // optional — nil ⇒ skills methods return ErrSkillsUnavailable
	bus      events.EventBus   // optional — nil ⇒ tool-exposure edits emit no mcp.connection.* events
	logger   *slog.Logger
	now      Clock

	// attacher drives the real MCP attach lifecycle for add_mcp_connection.
	// Optional — nil ⇒ add_mcp_connection returns ErrConnectionAttachUnavailable
	// (→ 501 at the wire edge). The concrete that calls the MCP driver is
	// injected at the cmd/harbor + devstack boundary (the §4.4 boundary keeps
	// the concrete MCP driver out of this package).
	attacher ConnectionAttacher
	// coordinator is the unified pause/resume primitive an auth-required
	// attach parks on. Optional — nil ⇒ an auth-required attach fails loud
	// with ErrCoordinatorUnavailable rather than silently dropping the auth
	// requirement (CLAUDE.md §13).
	coordinator pauseresume.Coordinator
	// stdioAllowlist is the fail-closed set of permitted stdio commands
	// (matched on argv[0]). A stdio add whose command[0] is absent from this
	// set is rejected (an empty / nil set rejects EVERY stdio add — the
	// secure default). http adds are admin-scoped but not gated here.
	stdioAllowlist map[string]struct{}

	// sessionOverlay backs the session-safe (non-admin) lower tier: the
	// `agent_config.session.*` verbs write the session's user prompt layer +
	// narrow-only disable set + ephemeral personal-skill names. Optional —
	// nil ⇒ the session methods return ErrSessionOverlayUnavailable (→ 501 at
	// the wire edge). Keyed by the REAL (tenant, user, session) triple, so it
	// is session-isolated by construction.
	sessionOverlay sessionoverlay.Store

	// validModels is the set of model names with a configured ModelProfile.
	// `set_llm_params` (and a `set_revision` carrying an LLMParams.Model)
	// validate a set model against it — an unknown model is rejected with
	// ErrUnknownModel (parity with the tenant model-swap). A nil / empty set
	// means model validation is disabled (no model is pinnable via this
	// service): the binary always wires the configured profiles, so an empty
	// set signals model-pinning is unavailable, never a silent accept-all.
	validModels map[string]struct{}

	// writeLocks serialises the read-modify-write of each admin write verb
	// PER AGENT. A convenience verb reads the active revision, rebuilds the
	// sibling sections from that snapshot, then writes a new revision — and
	// the registry is last-write-wins (no CAS). Without serialisation two
	// concurrent edits to the SAME agent (e.g. set_skills racing
	// set_tool_exposure) would each rebuild from the other's pre-write
	// snapshot, silently reverting the concurrent sibling change (an MCP
	// pause could vanish) and forking the parent chain. The Service is the
	// sole registry-writer, so a per-(tenant, agent) lock held across the
	// whole read-modify-write makes it atomic within the process. (Keyed by
	// (tenant, agent) because the registry collapses user/session into the
	// synthetic per-agent slot. Cross-replica atomicity would need StateStore
	// CAS — out of scope while the registry is a single in-process artifact.)
	writeLocks sync.Map // map[string]*sync.Mutex
}

// lockAgent acquires the per-(tenant, agent) write lock and returns the
// release func (call via defer). It serialises every admin write verb's
// read-modify-write against concurrent writes to the same agent.
func (s *Service) lockAgent(tenant, agentID string) func() {
	key := tenant + "\x00" + agentID
	mu, _ := s.writeLocks.LoadOrStore(key, &sync.Mutex{})
	m, ok := mu.(*sync.Mutex)
	if !ok {
		// Impossible by construction: writeLocks only ever stores *sync.Mutex.
		panic("agentcfg/protocol: writeLocks held a non-mutex value")
	}
	m.Lock()
	return m.Unlock
}

// Option configures NewService.
type Option func(*Service)

// WithLogger sets the slog.Logger. A nil logger routes to slog.Default().
func WithLogger(l *slog.Logger) Option {
	return func(s *Service) {
		if l != nil {
			s.logger = l
		}
	}
}

// WithClock injects the time source. Defaults to time.Now.
func WithClock(c Clock) Option {
	return func(s *Service) {
		if c != nil {
			s.now = c
		}
	}
}

// WithSkillStore wires the SkillStore so the `agent_config.skills.*`
// methods are live. A nil store leaves the skills methods returning
// ErrSkillsUnavailable (→ 501 at the wire edge).
func WithSkillStore(st skills.SkillStore) Option {
	return func(s *Service) {
		if st != nil {
			s.skills = st
		}
	}
}

// WithBus wires the EventBus the tool-exposure consumer publishes the
// `mcp.connection.paused` / `.resumed` overlay events through. A nil bus
// leaves those events unpublished (the revision is still recorded — the
// generic `agent.config.revised` still fires from the registry).
func WithBus(b events.EventBus) Option {
	return func(s *Service) {
		if b != nil {
			s.bus = b
		}
	}
}

// WithConnectionAttacher wires the concrete that drives the real MCP attach
// lifecycle for `agent_config.add_mcp_connection`. A nil attacher leaves the
// method returning ErrConnectionAttachUnavailable (→ 501 at the wire edge).
// The concrete (which imports the MCP driver) is injected at the cmd/harbor +
// devstack boundary; this package depends only on the interface.
func WithConnectionAttacher(a ConnectionAttacher) Option {
	return func(s *Service) {
		if a != nil {
			s.attacher = a
		}
	}
}

// WithCoordinator wires the unified pause/resume primitive an auth-required
// MCP attach parks on. A nil coordinator leaves an auth-required attach
// failing loud with ErrCoordinatorUnavailable (never a silent drop).
func WithCoordinator(c pauseresume.Coordinator) Option {
	return func(s *Service) {
		if c != nil {
			s.coordinator = c
		}
	}
}

// WithStdioAllowlist sets the fail-closed allowlist of permitted stdio
// commands (matched on argv[0]) for `agent_config.add_mcp_connection`. A
// stdio add whose command[0] is absent is rejected with ErrStdioNotAllowed.
// An empty / nil allowlist rejects EVERY stdio add (the secure default);
// http adds are unaffected. Adding a stdio server runs an operator-supplied
// command (an RCE surface) — this gate is the §7 fail-closed boundary.
func WithStdioAllowlist(commands []string) Option {
	return func(s *Service) {
		if len(commands) == 0 {
			return
		}
		set := make(map[string]struct{}, len(commands))
		for _, c := range commands {
			if c != "" {
				set[c] = struct{}{}
			}
		}
		s.stdioAllowlist = set
	}
}

// WithSessionOverlay wires the session-scoped safe-subset overlay store so
// the NON-admin `agent_config.session.*` verbs are live. A nil store leaves
// those methods returning ErrSessionOverlayUnavailable (→ 501 at the wire
// edge). The store is keyed by the caller's real (tenant, user, session)
// triple, so a session's overlay is invisible to another session.
func WithSessionOverlay(st sessionoverlay.Store) Option {
	return func(s *Service) {
		if st != nil {
			s.sessionOverlay = st
		}
	}
}

// WithValidModels sets the set of model names with a configured
// ModelProfile. `set_llm_params` (and a `set_revision` carrying an
// LLMParams.Model) reject a model outside this set with ErrUnknownModel —
// parity with the tenant model-swap, validated at SET time so an invalid
// model can never be persisted (never a silent run-start fallback).
//
// An empty / nil set means model-pinning is UNAVAILABLE: every non-empty
// model set fails loud (the binary always wires at least the bound default
// model, so an empty set signals a misconfiguration, not "accept anything").
// This matches the governance tenant-override policy's fail-loud-on-empty
// stance; it deliberately DIFFERS from runs.set_overrides (the one-shot
// session swap), which accepts any model when no set is configured.
func WithValidModels(models []string) Option {
	return func(s *Service) {
		if len(models) == 0 {
			return
		}
		set := make(map[string]struct{}, len(models))
		for _, m := range models {
			if m != "" {
				set[m] = struct{}{}
			}
		}
		s.validModels = set
	}
}

// validateModel rejects a set, non-empty model that has no configured
// ModelProfile. A nil model (the dimension is unset) is always allowed —
// only a model the run would actually request is validated. With no
// validModels configured, any model set fails loud (model-pinning is
// unavailable on this service), so a misconfiguration cannot silently
// accept an arbitrary model.
func (s *Service) validateModel(model *string) error {
	if model == nil || *model == "" {
		return nil
	}
	if _, ok := s.validModels[*model]; !ok {
		return fmt.Errorf("%w: %q", ErrUnknownModel, *model)
	}
	return nil
}

// validateLLMParams validates a per-agent LLM-params section at set time: the
// model (against the configured ModelProfiles) plus the sampling ranges
// (temperature ∈ [0,2], max-tokens > 0, reasoning-effort in the canonical
// taxonomy) — parity with runs.set_overrides so an out-of-range value never
// reaches a run (fail loud, never silently passed through to the provider
// edge). A nil section (no LLM-params edit) is a no-op. Each field is checked
// only when set.
func (s *Service) validateLLMParams(lp *prototypes.AgentConfigLLMParams) error {
	if lp == nil {
		return nil
	}
	if err := s.validateModel(lp.Model); err != nil {
		return err
	}
	if lp.Temperature != nil && (*lp.Temperature < 0 || *lp.Temperature > 2) {
		return fmt.Errorf("%w: temperature %v outside [0,2]", ErrInvalidLLMParams, *lp.Temperature)
	}
	if lp.MaxTokens != nil && *lp.MaxTokens <= 0 {
		return fmt.Errorf("%w: max_tokens %d must be positive", ErrInvalidLLMParams, *lp.MaxTokens)
	}
	if lp.ReasoningEffort != nil && *lp.ReasoningEffort != "" {
		if _, ok := validLLMReasoningEffort[*lp.ReasoningEffort]; !ok {
			return fmt.Errorf("%w: unknown reasoning_effort %q (want off/low/medium/high)",
				ErrInvalidLLMParams, *lp.ReasoningEffort)
		}
	}
	return nil
}

// NewService builds the agent-config Service over a Registry. registry is
// mandatory — a nil fails loud with ErrMisconfigured rather than building
// a Service that would nil-panic on the first request (CLAUDE.md §5).
//
// The returned *Service is immutable after construction and safe for
// concurrent use by N goroutines.
func NewService(registry agentcfg.Registry, opts ...Option) (*Service, error) {
	if registry == nil {
		return nil, fmt.Errorf("%w: agentcfg.Registry is nil", ErrMisconfigured)
	}
	s := &Service{registry: registry, logger: slog.Default(), now: time.Now}
	for _, opt := range opts {
		opt(s)
	}
	return s, nil
}

// Get reads the agent's active config revision.
func (s *Service) Get(ctx context.Context, req prototypes.AgentConfigGetRequest) (prototypes.AgentConfigGetResponse, error) {
	if err := ctx.Err(); err != nil {
		return prototypes.AgentConfigGetResponse{}, err
	}
	id, err := identityFromScope(req.Identity, req.AgentID)
	if err != nil {
		return prototypes.AgentConfigGetResponse{}, err
	}
	rev, set, err := s.registry.Active(ctx, identity.Quadruple{Identity: id}, req.AgentID)
	if err != nil {
		return prototypes.AgentConfigGetResponse{}, err
	}
	resp := prototypes.AgentConfigGetResponse{Set: set, ProtocolVersion: prototypes.ProtocolVersion}
	if set {
		v := revisionToWire(rev)
		resp.Revision = &v
	}
	return resp, nil
}

// SetRevision writes a new immutable revision and advances the active
// pointer.
func (s *Service) SetRevision(ctx context.Context, req prototypes.AgentConfigSetRevisionRequest) (prototypes.AgentConfigSetRevisionResponse, error) {
	if err := ctx.Err(); err != nil {
		return prototypes.AgentConfigSetRevisionResponse{}, err
	}
	id, err := identityFromScope(req.Identity, req.AgentID)
	if err != nil {
		return prototypes.AgentConfigSetRevisionResponse{}, err
	}
	defer s.lockAgent(id.TenantID, req.AgentID)()
	// A full-payload set that pins per-agent LLM params is validated at set
	// time (parity with set_llm_params / the tenant model-swap) so an invalid
	// model or out-of-range sampling value can never be persisted.
	if err := s.validateLLMParams(req.Payload.LLMParams); err != nil {
		return prototypes.AgentConfigSetRevisionResponse{}, err
	}
	rev, err := s.registry.SetRevision(ctx, identity.Quadruple{Identity: id}, req.AgentID, payloadToDomain(req.Payload))
	if err != nil {
		return prototypes.AgentConfigSetRevisionResponse{}, err
	}
	return prototypes.AgentConfigSetRevisionResponse{
		Revision:        revisionToWire(rev),
		ProtocolVersion: prototypes.ProtocolVersion,
	}, nil
}

// ListRevisions returns the agent's revision chain, newest-first.
func (s *Service) ListRevisions(ctx context.Context, req prototypes.AgentConfigListRevisionsRequest) (prototypes.AgentConfigListRevisionsResponse, error) {
	if err := ctx.Err(); err != nil {
		return prototypes.AgentConfigListRevisionsResponse{}, err
	}
	id, err := identityFromScope(req.Identity, req.AgentID)
	if err != nil {
		return prototypes.AgentConfigListRevisionsResponse{}, err
	}
	revs, err := s.registry.ListRevisions(ctx, identity.Quadruple{Identity: id}, req.AgentID, req.Limit)
	if err != nil {
		return prototypes.AgentConfigListRevisionsResponse{}, err
	}
	out := make([]prototypes.AgentConfigRevisionView, 0, len(revs))
	for _, r := range revs {
		out = append(out, revisionToWire(r))
	}
	return prototypes.AgentConfigListRevisionsResponse{
		Revisions:       out,
		ProtocolVersion: prototypes.ProtocolVersion,
	}, nil
}

// Diff returns the server-side compare of two existing revisions.
func (s *Service) Diff(ctx context.Context, req prototypes.AgentConfigDiffRequest) (prototypes.AgentConfigDiffResponse, error) {
	if err := ctx.Err(); err != nil {
		return prototypes.AgentConfigDiffResponse{}, err
	}
	id, err := identityFromScope(req.Identity, req.AgentID)
	if err != nil {
		return prototypes.AgentConfigDiffResponse{}, err
	}
	d, err := s.registry.Diff(ctx, identity.Quadruple{Identity: id}, req.AgentID, req.FromRevision, req.ToRevision)
	if err != nil {
		return prototypes.AgentConfigDiffResponse{}, err
	}
	return prototypes.AgentConfigDiffResponse{
		Diff:            diffToWire(d),
		ProtocolVersion: prototypes.ProtocolVersion,
	}, nil
}

// Rollback repoints the active pointer to an existing revision.
func (s *Service) Rollback(ctx context.Context, req prototypes.AgentConfigRollbackRequest) (prototypes.AgentConfigRollbackResponse, error) {
	if err := ctx.Err(); err != nil {
		return prototypes.AgentConfigRollbackResponse{}, err
	}
	id, err := identityFromScope(req.Identity, req.AgentID)
	if err != nil {
		return prototypes.AgentConfigRollbackResponse{}, err
	}
	defer s.lockAgent(id.TenantID, req.AgentID)()
	rev, err := s.registry.Rollback(ctx, identity.Quadruple{Identity: id}, req.AgentID, req.RevisionID)
	if err != nil {
		return prototypes.AgentConfigRollbackResponse{}, err
	}
	return prototypes.AgentConfigRollbackResponse{
		Revision:        revisionToWire(rev),
		ProtocolVersion: prototypes.ProtocolVersion,
	}, nil
}

// identityFromScope validates a wire IdentityScope + agent id into an
// identity.Identity, failing closed on an incomplete triple or empty
// agent id.
func identityFromScope(scope prototypes.IdentityScope, agentID string) (identity.Identity, error) {
	id := identity.Identity{TenantID: scope.Tenant, UserID: scope.User, SessionID: scope.Session}
	if id.TenantID == "" || id.UserID == "" || id.SessionID == "" {
		return identity.Identity{}, fmt.Errorf("%w: (tenant=%q user=%q session=%q)",
			ErrIdentityRequired, id.TenantID, id.UserID, id.SessionID)
	}
	if agentID == "" {
		return identity.Identity{}, fmt.Errorf("%w: agent_id is empty", ErrIdentityRequired)
	}
	return id, nil
}

// revisionToWire projects a domain Revision onto the wire view. The
// author's session is intentionally dropped (the audit anchor is the
// (tenant, user) actor).
func revisionToWire(r agentcfg.Revision) prototypes.AgentConfigRevisionView {
	return prototypes.AgentConfigRevisionView{
		RevisionID:       r.RevisionID,
		ParentRevisionID: r.ParentRevisionID,
		ContentHash:      r.ContentHash,
		AuthorTenant:     r.Author.TenantID,
		AuthorUser:       r.Author.UserID,
		CreatedAt:        r.CreatedAt,
		Payload:          payloadToWire(r.Payload),
	}
}

// payloadToWire projects a domain ConfigPayload onto the wire envelope.
func payloadToWire(p agentcfg.ConfigPayload) prototypes.AgentConfigPayload {
	var out prototypes.AgentConfigPayload
	if p.PromptLayers != nil {
		out.PromptLayers = &prototypes.AgentConfigPromptLayers{
			Base: copyStringPtr(p.PromptLayers.Base),
			User: copyStringPtr(p.PromptLayers.User),
		}
	}
	if p.Skills != nil {
		out.Skills = &prototypes.AgentConfigSkillsSelection{Names: append([]string(nil), p.Skills.Names...)}
	}
	if p.ToolExposure != nil {
		out.ToolExposure = &prototypes.AgentConfigToolExposure{
			PausedServers: append([]string(nil), p.ToolExposure.PausedServers...),
			DisabledTools: append([]string(nil), p.ToolExposure.DisabledTools...),
		}
	}
	if p.Connections != nil {
		out.Connections = &prototypes.AgentConfigConnections{
			Servers: connectionsToWire(p.Connections.Servers),
		}
	}
	if p.LLMParams != nil {
		out.LLMParams = &prototypes.AgentConfigLLMParams{
			Model:           copyStringPtr(p.LLMParams.Model),
			Temperature:     copyFloat64Ptr(p.LLMParams.Temperature),
			MaxTokens:       copyIntPtr(p.LLMParams.MaxTokens),
			ReasoningEffort: copyStringPtr(p.LLMParams.ReasoningEffort),
		}
	}
	return out
}

// connectionsToWire projects domain connection descriptors onto the wire
// shape (defensive copies; no shared backing slices).
func connectionsToWire(in []agentcfg.MCPConnectionDescriptor) []prototypes.AgentConfigMCPConnectionDescriptor {
	if len(in) == 0 {
		return nil
	}
	out := make([]prototypes.AgentConfigMCPConnectionDescriptor, 0, len(in))
	for _, d := range in {
		out = append(out, prototypes.AgentConfigMCPConnectionDescriptor{
			Name:      d.Name,
			Transport: string(d.Transport),
			Command:   append([]string(nil), d.Command...),
			URL:       d.URL,
		})
	}
	return out
}

// connectionsToDomain projects wire connection descriptors onto the domain
// shape (defensive copies; no shared backing slices).
func connectionsToDomain(in []prototypes.AgentConfigMCPConnectionDescriptor) []agentcfg.MCPConnectionDescriptor {
	if len(in) == 0 {
		return nil
	}
	out := make([]agentcfg.MCPConnectionDescriptor, 0, len(in))
	for _, d := range in {
		out = append(out, agentcfg.MCPConnectionDescriptor{
			Name:      d.Name,
			Transport: agentcfg.MCPTransport(d.Transport),
			Command:   append([]string(nil), d.Command...),
			URL:       d.URL,
		})
	}
	return out
}

// payloadToDomain projects a wire envelope onto the domain ConfigPayload.
func payloadToDomain(p prototypes.AgentConfigPayload) agentcfg.ConfigPayload {
	var out agentcfg.ConfigPayload
	if p.PromptLayers != nil {
		out.PromptLayers = &agentcfg.PromptLayers{
			Base: copyStringPtr(p.PromptLayers.Base),
			User: copyStringPtr(p.PromptLayers.User),
		}
	}
	if p.Skills != nil {
		out.Skills = &agentcfg.SkillsSelection{Names: append([]string(nil), p.Skills.Names...)}
	}
	if p.ToolExposure != nil {
		out.ToolExposure = &agentcfg.ToolExposure{
			PausedServers: append([]string(nil), p.ToolExposure.PausedServers...),
			DisabledTools: append([]string(nil), p.ToolExposure.DisabledTools...),
		}
	}
	if p.Connections != nil {
		out.Connections = &agentcfg.ConnectionsSection{
			Servers: connectionsToDomain(p.Connections.Servers),
		}
	}
	if p.LLMParams != nil {
		out.LLMParams = &agentcfg.LLMParams{
			Model:           copyStringPtr(p.LLMParams.Model),
			Temperature:     copyFloat64Ptr(p.LLMParams.Temperature),
			MaxTokens:       copyIntPtr(p.LLMParams.MaxTokens),
			ReasoningEffort: copyStringPtr(p.LLMParams.ReasoningEffort),
		}
	}
	return out
}

// diffToWire projects a domain Diff onto the wire diff.
func diffToWire(d agentcfg.Diff) prototypes.AgentConfigDiff {
	return prototypes.AgentConfigDiff{
		FromRevisionID: d.FromRevisionID,
		ToRevisionID:   d.ToRevisionID,
		Skills: prototypes.AgentConfigSkillsDiff{
			Added:   append([]string(nil), d.Skills.Added...),
			Removed: append([]string(nil), d.Skills.Removed...),
		},
		ToolExposure: prototypes.AgentConfigToolExposureDiff{
			PausedAdded:     append([]string(nil), d.ToolExposure.PausedAdded...),
			PausedResumed:   append([]string(nil), d.ToolExposure.PausedResumed...),
			DisabledAdded:   append([]string(nil), d.ToolExposure.DisabledAdded...),
			DisabledEnabled: append([]string(nil), d.ToolExposure.DisabledEnabled...),
		},
		PromptLayers: prototypes.AgentConfigPromptLayersDiff{
			BaseChanged: d.PromptLayers.BaseChanged,
			BaseFrom:    d.PromptLayers.BaseFrom,
			BaseTo:      d.PromptLayers.BaseTo,
			UserChanged: d.PromptLayers.UserChanged,
			UserFrom:    d.PromptLayers.UserFrom,
			UserTo:      d.PromptLayers.UserTo,
		},
		Connections: prototypes.AgentConfigConnectionsDiff{
			Added:   append([]string(nil), d.Connections.Added...),
			Removed: append([]string(nil), d.Connections.Removed...),
		},
		LLMParams: prototypes.AgentConfigLLMParamsDiff{
			ModelChanged: d.LLMParams.ModelChanged,
			ModelFrom:    d.LLMParams.ModelFrom,
			ModelTo:      d.LLMParams.ModelTo,

			TemperatureChanged: d.LLMParams.TemperatureChanged,
			TemperatureFrom:    d.LLMParams.TemperatureFrom,
			TemperatureTo:      d.LLMParams.TemperatureTo,

			MaxTokensChanged: d.LLMParams.MaxTokensChanged,
			MaxTokensFrom:    d.LLMParams.MaxTokensFrom,
			MaxTokensTo:      d.LLMParams.MaxTokensTo,

			ReasoningEffortChanged: d.LLMParams.ReasoningEffortChanged,
			ReasoningEffortFrom:    d.LLMParams.ReasoningEffortFrom,
			ReasoningEffortTo:      d.LLMParams.ReasoningEffortTo,
		},
	}
}

// copyStringPtr returns a fresh copy of a *string (nil stays nil) so the
// wire↔domain projections never share a pointer with the caller's payload.
func copyStringPtr(s *string) *string {
	if s == nil {
		return nil
	}
	v := *s
	return &v
}

// copyFloat64Ptr / copyIntPtr return a fresh copy of a numeric pointer (nil
// stays nil) so the wire↔domain projections never share a pointer with the
// caller's payload.
func copyFloat64Ptr(f *float64) *float64 {
	if f == nil {
		return nil
	}
	v := *f
	return &v
}

func copyIntPtr(i *int) *int {
	if i == nil {
		return nil
	}
	v := *i
	return &v
}
