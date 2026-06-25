// Package statestore implements the StateStore-backed agentcfg Registry
// driver — the default driver for the agent-config control plane. It
// stores immutable, content-addressed config revisions and an
// active-revision pointer on the runtime's StateStore, reusing the §9
// persistence triad (in-mem / SQLite / Postgres) for identity isolation,
// exactly as the governance tenant-override policy does.
//
// # Keying (CLAUDE.md §6)
//
// Each agent's config persists under a synthetic identity
// {TenantID: caller-tenant, UserID: "__agentcfg__", SessionID: agentID}.
// The caller's verified TENANT is the isolation boundary: a tenant's
// config is invisible to another tenant. agent_id occupies the synthetic
// session slot as a per-agent KEY — never an isolation filter. Two record
// kinds persist at that synthetic identity:
//
//   - the active-pointer record at Kind "agentcfg.active";
//   - one record per revision at Kind "agentcfg.revision.<revisionID>",
//     so revisions are enumerable via the elevated maintenance scan.
//
// Parent pointer and content hash live INSIDE the persisted record bytes,
// never relying on the evictable StateStore EventID slot.
//
// # Concurrent reuse (the concurrent-reuse contract)
//
// The driver is immutable after construction except for the closed atomic
// flag; every read is fresh from the StateStore (no cache). Safe to share
// across N concurrent goroutines; per-call state lives in arguments and
// locals.
package statestore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync/atomic"
	"time"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/state"
)

// Driver name registered with the agentcfg factory registry.
const driverName = "statestore"

// Record kinds and the reserved synthetic-identity user slot.
const (
	kindActive      = "agentcfg.active"
	kindRevisionPfx = "agentcfg.revision."
	// The DISTINCT per-user record kinds. The user variant persists under
	// the caller's REAL (tenant, user) identity AND these kinds, so the two
	// key spaces are namespaced on TWO independent dimensions (identity slot
	// occupant + record-kind prefix) and can never alias regardless of
	// identity values. Note "agentcfg.user.revision." is NOT a string prefix
	// of "agentcfg.revision." (and vice-versa), so the ListKind prefix scan
	// never crosses scopes.
	kindUserActive      = "agentcfg.user.active"
	kindUserRevisionPfx = "agentcfg.user.revision."
	// agentCfgUser is the reserved synthetic user slot the agent-level
	// config persists under. The double-underscore prefix is reserved for
	// runtime-internal scopes (matches the governance __governance__
	// convention); no real session collides with it.
	agentCfgUser = "__agentcfg__"
)

// scopeKeys is the resolved keying for one (scope, identity, agent) tuple:
// the identity to persist under plus the active-pointer kind and the
// revision-kind prefix. The PINNED keying scheme (the 126 band keys around
// it) is the single source of this dispatch.
type scopeKeys struct {
	quad       identity.Quadruple
	activeKind string
	revPfx     string
}

// keysFor resolves the keying for scope. ConfigScopeAgent keeps the existing
// synthetic-slot keying AND the agentcfg.* kinds (byte-identical to before).
// ConfigScopeUser keys under the caller's REAL (tenant, user) with the agent
// id in the session slot AND the distinct agentcfg.user.* kinds; a verified
// user id equal to the reserved sentinel is REJECTED loud (ErrReservedUser)
// BEFORE any read or write, as fail-loud defence-in-depth.
func keysFor(scope agentcfg.ConfigScope, id identity.Quadruple, agentID string) (scopeKeys, error) {
	switch scope {
	case agentcfg.ConfigScopeUser:
		if id.UserID == agentCfgUser {
			return scopeKeys{}, fmt.Errorf("%w: user_id=%q", agentcfg.ErrReservedUser, id.UserID)
		}
		return scopeKeys{
			quad:       userQuad(id.TenantID, id.UserID, agentID),
			activeKind: kindUserActive,
			revPfx:     kindUserRevisionPfx,
		}, nil
	default: // ConfigScopeAgent
		return scopeKeys{
			quad:       syntheticQuad(id.TenantID, agentID),
			activeKind: kindActive,
			revPfx:     kindRevisionPfx,
		}, nil
	}
}

// Persisted-record schema version. Bumped only on a breaking record shape
// change; a forward-schema record fails loud rather than silently reset.
const recordSchema = 1

// activeRecord is the JSON-encoded active-pointer record.
type activeRecord struct {
	Schema     int       `json:"schema"`
	RevisionID string    `json:"revision_id"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// revisionRecord is the JSON-encoded immutable revision record. Parent
// pointer + content hash live here, never in the EventID slot.
type revisionRecord struct {
	Schema           int                    `json:"schema"`
	RevisionID       string                 `json:"revision_id"`
	ParentRevisionID string                 `json:"parent_revision_id,omitempty"`
	ContentHash      string                 `json:"content_hash"`
	Author           identity.Quadruple     `json:"author"`
	CreatedAt        time.Time              `json:"created_at"`
	Payload          agentcfg.ConfigPayload `json:"payload"`
}

func (r revisionRecord) toRevision() agentcfg.Revision {
	return agentcfg.Revision{
		RevisionID:       r.RevisionID,
		ParentRevisionID: r.ParentRevisionID,
		ContentHash:      r.ContentHash,
		Author:           r.Author,
		CreatedAt:        r.CreatedAt,
		Payload:          r.Payload,
	}
}

// registry is the StateStore-backed agentcfg.Registry implementation.
type registry struct {
	state  state.StateStore
	bus    events.EventBus
	clock  func() time.Time
	logger *slog.Logger
	closed atomic.Bool
}

func init() {
	agentcfg.Register(driverName, func(_ agentcfg.Config, deps agentcfg.Deps) (agentcfg.Registry, error) {
		if deps.State == nil {
			return nil, fmt.Errorf("%w: state.StateStore is required", agentcfg.ErrInvalidConfig)
		}
		if deps.Bus == nil {
			return nil, fmt.Errorf("%w: events.EventBus is required", agentcfg.ErrInvalidConfig)
		}
		clk := deps.Clock
		if clk == nil {
			clk = time.Now
		}
		return &registry{
			state:  deps.State,
			bus:    deps.Bus,
			clock:  clk,
			logger: slog.Default(),
		}, nil
	})
}

// syntheticQuad builds the synthetic identity an agent's config records
// persist under. The caller's verified tenant is the isolation boundary;
// agent_id is the per-agent key in the session slot.
func syntheticQuad(tenant, agentID string) identity.Quadruple {
	return identity.Quadruple{Identity: identity.Identity{
		TenantID:  tenant,
		UserID:    agentCfgUser,
		SessionID: agentID,
	}}
}

// userQuad builds the per-user identity the user-scope config variant
// persists under: the caller's REAL (tenant, user) with the agent id in the
// session slot (a per-agent KEY, never an isolation filter — RFC §6.16) and
// the run component left zeroed. The real user is the isolation principal for
// the variant; the isolation tuple is not widened.
func userQuad(tenant, user, agentID string) identity.Quadruple {
	return identity.Quadruple{Identity: identity.Identity{
		TenantID:  tenant,
		UserID:    user,
		SessionID: agentID,
	}}
}

// validate checks the caller identity triple and the agent id. Fails
// closed on any empty component (CLAUDE.md §6).
func (r *registry) validate(id identity.Quadruple, agentID string) error {
	if r.closed.Load() {
		return agentcfg.ErrClosed
	}
	if id.TenantID == "" || id.UserID == "" || id.SessionID == "" {
		return fmt.Errorf("%w: (tenant=%q user=%q session=%q)", agentcfg.ErrIdentityRequired, id.TenantID, id.UserID, id.SessionID)
	}
	if agentID == "" {
		return fmt.Errorf("%w: agent id is empty", agentcfg.ErrIdentityRequired)
	}
	return nil
}

func (r *registry) SetRevision(ctx context.Context, id identity.Quadruple, agentID string, scope agentcfg.ConfigScope, payload agentcfg.ConfigPayload) (agentcfg.Revision, error) {
	if err := r.validate(id, agentID); err != nil {
		return agentcfg.Revision{}, err
	}
	if err := ctx.Err(); err != nil {
		return agentcfg.Revision{}, err
	}
	keys, err := keysFor(scope, id, agentID)
	if err != nil {
		return agentcfg.Revision{}, err
	}
	norm := agentcfg.NormalizePayload(payload)
	hash, err := agentcfg.ContentHash(norm)
	if err != nil {
		return agentcfg.Revision{}, err
	}
	q := keys.quad

	// Idempotent re-set: if the current active revision pins byte-identical
	// canonical content, return it unchanged (no new revision, no event).
	active, hasActive, err := r.loadActiveRevision(ctx, q, keys.activeKind, keys.revPfx)
	if err != nil {
		return agentcfg.Revision{}, err
	}
	parentID := ""
	if hasActive {
		parentID = active.RevisionID
		if active.ContentHash == hash {
			return active, nil
		}
	}

	now := r.clock().UTC()
	revID := string(state.NewEventID())
	rec := revisionRecord{
		Schema:           recordSchema,
		RevisionID:       revID,
		ParentRevisionID: parentID,
		ContentHash:      hash,
		Author:           id,
		CreatedAt:        now,
		Payload:          norm,
	}
	if err := r.saveRevision(ctx, q, keys.revPfx, rec); err != nil {
		return agentcfg.Revision{}, err
	}
	if err := r.saveActive(ctx, q, keys.activeKind, revID, now); err != nil {
		return agentcfg.Revision{}, err
	}
	rev := rec.toRevision()
	r.emitRevised(ctx, agentID, rev)
	return rev, nil
}

func (r *registry) Active(ctx context.Context, id identity.Quadruple, agentID string, scope agentcfg.ConfigScope) (agentcfg.Revision, bool, error) {
	if err := r.validate(id, agentID); err != nil {
		return agentcfg.Revision{}, false, err
	}
	if err := ctx.Err(); err != nil {
		return agentcfg.Revision{}, false, err
	}
	keys, err := keysFor(scope, id, agentID)
	if err != nil {
		return agentcfg.Revision{}, false, err
	}
	return r.loadActiveRevision(ctx, keys.quad, keys.activeKind, keys.revPfx)
}

func (r *registry) Get(ctx context.Context, id identity.Quadruple, agentID, revisionID string, scope agentcfg.ConfigScope) (agentcfg.Revision, error) {
	if err := r.validate(id, agentID); err != nil {
		return agentcfg.Revision{}, err
	}
	if revisionID == "" {
		return agentcfg.Revision{}, fmt.Errorf("%w: revision id is empty", agentcfg.ErrRevisionNotFound)
	}
	if err := ctx.Err(); err != nil {
		return agentcfg.Revision{}, err
	}
	keys, err := keysFor(scope, id, agentID)
	if err != nil {
		return agentcfg.Revision{}, err
	}
	rec, err := r.loadRevision(ctx, keys.quad, keys.revPfx, revisionID)
	if err != nil {
		return agentcfg.Revision{}, err
	}
	return rec.toRevision(), nil
}

func (r *registry) ListRevisions(ctx context.Context, id identity.Quadruple, agentID string, scope agentcfg.ConfigScope, limit int) ([]agentcfg.Revision, error) {
	if err := r.validate(id, agentID); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	keys, err := keysFor(scope, id, agentID)
	if err != nil {
		return nil, err
	}
	q := keys.quad
	recs, err := r.state.ListKind(ctx, state.ListScope{MaintenanceScoped: true}, keys.revPfx)
	if err != nil {
		return nil, fmt.Errorf("%w: list revisions: %w", agentcfg.ErrStateUnavailable, err)
	}
	out := make([]agentcfg.Revision, 0, len(recs))
	for _, sr := range recs {
		// Filter to THIS agent's identity slot — ListKind crosses identity
		// boundaries, so the scope narrowing is explicit here (agent_id is
		// the key, the tenant is the isolation boundary).
		if sr.Identity.TenantID != q.TenantID || sr.Identity.UserID != q.UserID || sr.Identity.SessionID != q.SessionID {
			continue
		}
		var rr revisionRecord
		if err := json.Unmarshal(sr.Bytes, &rr); err != nil {
			return nil, fmt.Errorf("%w: unmarshal revision record: %w", agentcfg.ErrStateUnavailable, err)
		}
		if rr.Schema != 0 && rr.Schema != recordSchema {
			return nil, fmt.Errorf("%w: revision record schema=%d, runtime supports %d", agentcfg.ErrStateUnavailable, rr.Schema, recordSchema)
		}
		out = append(out, rr.toRevision())
	}
	// Newest-first by CreatedAt (nanosecond resolution from the clock),
	// with the revision id as a stable tiebreaker for the rare same-instant
	// case. Revision ids are ULIDs but their intra-millisecond entropy is
	// random (not monotonic), so CreatedAt — not the id — is the ordering
	// key.
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.After(out[j].CreatedAt)
		}
		return out[i].RevisionID > out[j].RevisionID
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (r *registry) Rollback(ctx context.Context, id identity.Quadruple, agentID, revisionID string, scope agentcfg.ConfigScope) (agentcfg.Revision, error) {
	if err := r.validate(id, agentID); err != nil {
		return agentcfg.Revision{}, err
	}
	if revisionID == "" {
		return agentcfg.Revision{}, fmt.Errorf("%w: revision id is empty", agentcfg.ErrRevisionNotFound)
	}
	if err := ctx.Err(); err != nil {
		return agentcfg.Revision{}, err
	}
	keys, err := keysFor(scope, id, agentID)
	if err != nil {
		return agentcfg.Revision{}, err
	}
	q := keys.quad
	// Load the target revision — a missing target fails loud (never a
	// silent repoint to nothing). This read also proves the revision was
	// never mutated by the repoint below.
	target, err := r.loadRevision(ctx, q, keys.revPfx, revisionID)
	if err != nil {
		return agentcfg.Revision{}, err
	}
	fromID := ""
	if active, hasActive, aerr := r.loadActiveRevision(ctx, q, keys.activeKind, keys.revPfx); aerr == nil && hasActive {
		fromID = active.RevisionID
	}
	now := r.clock().UTC()
	if err := r.saveActive(ctx, q, keys.activeKind, revisionID, now); err != nil {
		return agentcfg.Revision{}, err
	}
	rev := target.toRevision()
	r.emitReverted(ctx, id, agentID, revisionID, fromID, now)
	return rev, nil
}

func (r *registry) Diff(ctx context.Context, id identity.Quadruple, agentID, fromRev, toRev string, scope agentcfg.ConfigScope) (agentcfg.Diff, error) {
	if err := r.validate(id, agentID); err != nil {
		return agentcfg.Diff{}, err
	}
	if fromRev == "" || toRev == "" {
		return agentcfg.Diff{}, fmt.Errorf("%w: diff requires two revision ids", agentcfg.ErrRevisionNotFound)
	}
	if err := ctx.Err(); err != nil {
		return agentcfg.Diff{}, err
	}
	keys, err := keysFor(scope, id, agentID)
	if err != nil {
		return agentcfg.Diff{}, err
	}
	q := keys.quad
	from, err := r.loadRevision(ctx, q, keys.revPfx, fromRev)
	if err != nil {
		return agentcfg.Diff{}, err
	}
	to, err := r.loadRevision(ctx, q, keys.revPfx, toRev)
	if err != nil {
		return agentcfg.Diff{}, err
	}
	return agentcfg.Diff{
		FromRevisionID: fromRev,
		ToRevisionID:   toRev,
		Skills:         agentcfg.DiffSkills(from.Payload.SkillNames(), to.Payload.SkillNames()),
		ToolExposure:   agentcfg.DiffToolExposure(from.Payload, to.Payload),
		PromptLayers:   agentcfg.DiffPromptLayers(from.Payload, to.Payload),
		Connections:    agentcfg.DiffConnections(from.Payload, to.Payload),
		LLMParams:      agentcfg.DiffLLMParams(from.Payload, to.Payload),
	}, nil
}

func (r *registry) Close(_ context.Context) error {
	r.closed.Store(true)
	return nil
}

// --- internal helpers ---

func (r *registry) loadActiveRevision(ctx context.Context, q identity.Quadruple, activeKind, revPfx string) (agentcfg.Revision, bool, error) {
	rec, err := r.state.Load(ctx, q, activeKind)
	if err != nil {
		if errors.Is(err, state.ErrNotFound) {
			return agentcfg.Revision{}, false, nil
		}
		return agentcfg.Revision{}, false, fmt.Errorf("%w: load active pointer: %w", agentcfg.ErrStateUnavailable, err)
	}
	if len(rec.Bytes) == 0 {
		return agentcfg.Revision{}, false, nil
	}
	var ar activeRecord
	if err := json.Unmarshal(rec.Bytes, &ar); err != nil {
		return agentcfg.Revision{}, false, fmt.Errorf("%w: unmarshal active pointer: %w", agentcfg.ErrStateUnavailable, err)
	}
	if ar.Schema != 0 && ar.Schema != recordSchema {
		return agentcfg.Revision{}, false, fmt.Errorf("%w: active pointer schema=%d, runtime supports %d", agentcfg.ErrStateUnavailable, ar.Schema, recordSchema)
	}
	if ar.RevisionID == "" {
		return agentcfg.Revision{}, false, nil
	}
	rr, err := r.loadRevision(ctx, q, revPfx, ar.RevisionID)
	if err != nil {
		// The active pointer references a missing revision — fail loud
		// rather than silently report "no active config" (CLAUDE.md §13).
		return agentcfg.Revision{}, false, fmt.Errorf("active pointer references missing revision %q: %w", ar.RevisionID, err)
	}
	return rr.toRevision(), true, nil
}

func (r *registry) loadRevision(ctx context.Context, q identity.Quadruple, revPfx, revisionID string) (revisionRecord, error) {
	rec, err := r.state.Load(ctx, q, revPfx+revisionID)
	if err != nil {
		if errors.Is(err, state.ErrNotFound) {
			return revisionRecord{}, fmt.Errorf("%w: %q", agentcfg.ErrRevisionNotFound, revisionID)
		}
		return revisionRecord{}, fmt.Errorf("%w: load revision %q: %w", agentcfg.ErrStateUnavailable, revisionID, err)
	}
	var rr revisionRecord
	if err := json.Unmarshal(rec.Bytes, &rr); err != nil {
		return revisionRecord{}, fmt.Errorf("%w: unmarshal revision %q: %w", agentcfg.ErrStateUnavailable, revisionID, err)
	}
	if rr.Schema != 0 && rr.Schema != recordSchema {
		return revisionRecord{}, fmt.Errorf("%w: revision %q schema=%d, runtime supports %d", agentcfg.ErrStateUnavailable, revisionID, rr.Schema, recordSchema)
	}
	return rr, nil
}

func (r *registry) saveRevision(ctx context.Context, q identity.Quadruple, revPfx string, rec revisionRecord) error {
	buf, err := json.Marshal(rec)
	if err != nil {
		return fmt.Errorf("agentcfg/statestore: marshal revision: %w", err)
	}
	if err := r.state.Save(ctx, state.StateRecord{
		ID:        state.EventID(rec.RevisionID),
		Identity:  q,
		Kind:      revPfx + rec.RevisionID,
		Bytes:     buf,
		UpdatedAt: rec.CreatedAt,
	}); err != nil {
		return fmt.Errorf("%w: save revision: %w", agentcfg.ErrStateUnavailable, err)
	}
	return nil
}

func (r *registry) saveActive(ctx context.Context, q identity.Quadruple, activeKind, revisionID string, now time.Time) error {
	ar := activeRecord{Schema: recordSchema, RevisionID: revisionID, UpdatedAt: now}
	buf, err := json.Marshal(ar)
	if err != nil {
		return fmt.Errorf("agentcfg/statestore: marshal active pointer: %w", err)
	}
	if err := r.state.Save(ctx, state.StateRecord{
		ID:        state.NewEventID(),
		Identity:  q,
		Kind:      activeKind,
		Bytes:     buf,
		UpdatedAt: now,
	}); err != nil {
		return fmt.Errorf("%w: save active pointer: %w", agentcfg.ErrStateUnavailable, err)
	}
	return nil
}

// emitRevised publishes agent.config.revised. The revision is already
// persisted, so a bus failure does not change it — but a privileged config
// mutation must never go to the bus SILENTLY (CLAUDE.md §13): a publish
// failure is logged loud at Warn.
func (r *registry) emitRevised(ctx context.Context, agentID string, rev agentcfg.Revision) {
	if err := r.bus.Publish(ctx, events.Event{
		Type:       agentcfg.EventTypeConfigRevised,
		Identity:   rev.Author,
		OccurredAt: rev.CreatedAt,
		Payload: agentcfg.ConfigRevisedPayload{
			Author:           rev.Author,
			AgentID:          agentID,
			RevisionID:       rev.RevisionID,
			ParentRevisionID: rev.ParentRevisionID,
			ContentHash:      rev.ContentHash,
			OccurredAt:       rev.CreatedAt,
		},
	}); err != nil {
		r.logger.WarnContext(ctx, "agentcfg/statestore: failed to publish agent.config.revised",
			slog.String("revision_id", rev.RevisionID),
			slog.String("error", err.Error()))
	}
}

func (r *registry) emitReverted(ctx context.Context, author identity.Quadruple, agentID, revisionID, fromID string, at time.Time) {
	if err := r.bus.Publish(ctx, events.Event{
		Type:       agentcfg.EventTypeConfigReverted,
		Identity:   author,
		OccurredAt: at,
		Payload: agentcfg.ConfigRevertedPayload{
			Author:         author,
			AgentID:        agentID,
			RevisionID:     revisionID,
			FromRevisionID: fromID,
			OccurredAt:     at,
		},
	}); err != nil {
		r.logger.WarnContext(ctx, "agentcfg/statestore: failed to publish agent.config.reverted",
			slog.String("revision_id", revisionID),
			slog.String("error", err.Error()))
	}
}
