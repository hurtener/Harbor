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
	"time"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
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
)

// Clock is the time source the Service stamps response timestamps from.
type Clock func() time.Time

// Service implements the admin-scoped agent-config methods.
type Service struct {
	registry agentcfg.Registry
	skills   skills.SkillStore // optional — nil ⇒ skills methods return ErrSkillsUnavailable
	bus      events.EventBus   // optional — nil ⇒ tool-exposure edits emit no mcp.connection.* events
	logger   *slog.Logger
	now      Clock
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
