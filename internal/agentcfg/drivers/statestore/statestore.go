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
	"reflect"
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
	kindActive      = agentcfg.ActiveSlotKind
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
	agentCfgUser = agentcfg.ReservedAgentConfigUser
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

// keysFor resolves the keying for scope. ConfigScopeAgent (the zero value)
// keeps the existing synthetic-slot keying AND the agentcfg.* kinds
// (byte-identical to before). ConfigScopeUser keys under the caller's REAL
// (tenant, user) with the agent id in the session slot AND the distinct
// agentcfg.user.* kinds; a verified user id equal to the reserved sentinel is
// REJECTED loud (ErrReservedUser) BEFORE any read or write, as fail-loud
// defence-in-depth. The scope discriminator is matched EXPLICITLY: an
// unrecognized value fails closed with an error rather than defaulting to the
// more-privileged agent tier — this keying function is a security boundary, so
// an out-of-range scope is a loud error, not a silent privilege grant (§13).
func keysFor(scope agentcfg.ConfigScope, id identity.Quadruple, agentID string) (scopeKeys, error) {
	var quad identity.Quadruple
	var revPfx string
	switch scope {
	case agentcfg.ConfigScopeAgent:
		quad, revPfx = syntheticQuad(id.TenantID, agentID), kindRevisionPfx
	case agentcfg.ConfigScopeUser:
		if id.UserID == agentCfgUser {
			return scopeKeys{}, fmt.Errorf("%w: user_id=%q", agentcfg.ErrReservedUser, id.UserID)
		}
		quad, revPfx = userQuad(id.TenantID, id.UserID, agentID), kindUserRevisionPfx
	default:
		return scopeKeys{}, fmt.Errorf("agentcfg/statestore: unrecognized config scope %d", scope)
	}
	activeKind, err := activeKindFor(revPfx)
	if err != nil {
		return scopeKeys{}, err
	}
	return scopeKeys{quad: quad, activeKind: activeKind, revPfx: revPfx}, nil
}

// activeKindFor maps a revision-kind prefix to the active-pointer kind that
// belongs to it. It is the ONE place the two halves of a scope's keying are
// paired, so no code path can read a pointer that belongs to a different scope
// than the revision it is reasoning about — the compensating delete asks
// exactly that question and must not be able to get it wrong. An unpaired
// prefix is a loud error rather than a defaulted-to-agent-scope answer.
func activeKindFor(revPfx string) (string, error) {
	switch revPfx {
	case kindRevisionPfx:
		return kindActive, nil
	case kindUserRevisionPfx:
		return kindUserActive, nil
	default:
		return "", fmt.Errorf("agentcfg/statestore: no active-pointer kind is paired with revision prefix %q", revPfx)
	}
}

// Persisted-record schema version. Bumped only on a breaking record shape
// change; a forward-schema record fails loud rather than silently reset.
const recordSchema = 1

// activeRecord is the JSON-encoded active-pointer record.
type activeRecord struct {
	Schema     int    `json:"schema"`
	RevisionID string `json:"revision_id"`
	// Inactive is a durable physical-pointer tombstone used only by exact
	// first-write compensation. Keeping the old revision id makes the
	// compensation auditable while Active still returns no-active.
	Inactive  bool      `json:"inactive,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
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
	// The registry validates tenant and agentID before constructing scope
	// keys. Keep the construction total here rather than returning a zero
	// identity on an impossible violated precondition.
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

func (r *registry) SetRevision(ctx context.Context, id identity.Quadruple, agentID string, scope agentcfg.ConfigScope, payload agentcfg.ConfigPayload, opts agentcfg.SetOptions) (agentcfg.Revision, error) {
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
	if err := r.guardSignedOAuthMCPFenceWriter(ctx, id, agentID, scope); err != nil {
		return agentcfg.Revision{}, err
	}
	norm := agentcfg.NormalizePayload(payload)
	hash, err := agentcfg.ContentHash(norm)
	if err != nil {
		return agentcfg.Revision{}, err
	}
	q := keys.quad
	expectations, err := r.activeExpectations(ctx, id, agentID, scope, keys, opts)
	if err != nil {
		return agentcfg.Revision{}, err
	}

	// ONE read serves both the precondition and the idempotence check. The
	// expected pointer generation captured above is rechecked by SaveIf at
	// publication, so a concurrent Runtime cannot turn this read into a
	// lost update. The per-owner write lock only reduces same-process churn.
	active, hasActive, err := r.loadActiveRevision(ctx, q, keys.activeKind, keys.revPfx)
	if err != nil {
		return agentcfg.Revision{}, err
	}

	// ORDER IS LOAD-BEARING — the precondition is evaluated BEFORE the
	// idempotent-re-set short-circuit below. Transposing the two blocks
	// would let a STALE expectation be answered with a success whenever
	// the caller's payload happens to equal the CURRENT content: the
	// caller would be told its write landed on the base it read, when its
	// base had in fact moved. That is silent degradation. A conflict costs
	// one re-read on a rare path and never lies.
	if opts.ExpectedContentHash != "" {
		if err := agentcfg.CheckExpectedRevision(opts, active, hasActive); err != nil {
			return agentcfg.Revision{}, err
		}
	}

	// Idempotent re-set: if the current active revision pins byte-identical
	// canonical content, return it unchanged (no new revision, no event).
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
		return agentcfg.Revision{}, r.compensateFailedRevisionSave(ctx, q, keys.revPfx, rec, err)
	}
	if err := r.saveActiveIf(ctx, expectations, q, keys.activeKind, revID, now); err != nil {
		if errors.Is(err, state.ErrConditionFailed) {
			cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), compensationTimeout)
			defer cancel()
			if deleteErr := r.state.Delete(cleanupCtx, q, keys.revPfx+revID); deleteErr != nil {
				return agentcfg.Revision{}, fmt.Errorf("%w: conditional active-pointer conflict and candidate revision cleanup failed: %w", agentcfg.ErrRevisionConflict, deleteErr)
			}
			return agentcfg.Revision{}, fmt.Errorf("%w: active pointer changed during save", agentcfg.ErrRevisionConflict)
		}
		// The revision record landed and the pointer that would have named
		// it did not — OR the pointer did land and the store reported
		// otherwise. Compensate — see [registry.compensateOrphanRevision] for
		// why a bare return here left a revision in history that no reader can
		// reach, and why the cleanup must not be unconditional.
		landed, cerr := r.compensateOrphanRevision(ctx, q, keys.revPfx, revID, err)
		if landed {
			// The pointer is durably on disk naming this revision: the config
			// DID change. Suppressing the event because the call is about to
			// return an error would leave every observer's view stale behind a
			// change that really happened, which is the silent half of the
			// same defect the compensation closes.
			//
			// The announcement runs on the same un-cancellable, bounded context
			// the compensation uses, and for the same reason: a caller context
			// that went away is one of the likeliest ways to reach this branch,
			// and an announcement issued on it would be dropped on exactly the
			// occasions it is owed.
			ectx, ecancel := context.WithTimeout(context.WithoutCancel(ctx), compensationTimeout)
			r.emitRevised(ectx, agentID, rec.toRevision())
			ecancel()
		}
		return agentcfg.Revision{}, cerr
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
	if scope == agentcfg.ConfigScopeUser {
		if err := r.validatePresentLifecycle(ctx, syntheticQuad(id.TenantID, agentID), kindActive); err != nil {
			return agentcfg.Revision{}, false, err
		}
	}
	active, set, err := r.loadActiveRevision(ctx, keys.quad, keys.activeKind, keys.revPfx)
	if err != nil || scope != agentcfg.ConfigScopeAgent {
		return active, set, err
	}
	return r.applySignedOAuthMCPFence(ctx, id, agentID, keys, active, set)
}

// DeactivateIfActive advances the physical active-pointer slot to an inactive
// marker only when it still names revisionID. It uses the exact StateStore
// EventID predicate, so a compensation can never erase another Runtime's
// winner after the caller lost its acknowledgement.
func (r *registry) DeactivateIfActive(ctx context.Context, id identity.Quadruple, agentID, revisionID string, scope agentcfg.ConfigScope) (bool, error) {
	if err := r.validate(id, agentID); err != nil {
		return false, err
	}
	if revisionID == "" {
		return false, fmt.Errorf("%w: revision id is empty", agentcfg.ErrRevisionNotFound)
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	keys, err := keysFor(scope, id, agentID)
	if err != nil {
		return false, err
	}
	if err := r.guardSignedOAuthMCPFenceWriter(ctx, id, agentID, scope); err != nil {
		return false, err
	}
	current, err := r.state.Load(ctx, keys.quad, keys.activeKind)
	if errors.Is(err, state.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("%w: load active pointer: %w", agentcfg.ErrStateUnavailable, err)
	}
	var pointer activeRecord
	if err := json.Unmarshal(current.Bytes, &pointer); err != nil {
		return false, fmt.Errorf("%w: unmarshal active pointer: %w", agentcfg.ErrStateUnavailable, err)
	}
	if pointer.Schema != 0 && pointer.Schema != recordSchema {
		return false, fmt.Errorf("%w: active pointer schema=%d, runtime supports %d", agentcfg.ErrStateUnavailable, pointer.Schema, recordSchema)
	}
	if pointer.Inactive || pointer.RevisionID != revisionID {
		return false, nil
	}
	pointer.Schema = recordSchema
	pointer.Inactive = true
	pointer.UpdatedAt = r.clock().UTC()
	encoded, err := json.Marshal(pointer)
	if err != nil {
		return false, fmt.Errorf("agentcfg/statestore: marshal inactive pointer: %w", err)
	}
	err = r.state.SaveIf(ctx, []state.SlotExpectation{{Identity: keys.quad, Kind: keys.activeKind, ExpectedEventID: current.ID}}, state.StateRecord{
		ID: state.NewEventID(), Identity: keys.quad, Kind: keys.activeKind, Bytes: encoded, UpdatedAt: pointer.UpdatedAt,
	})
	if errors.Is(err, state.ErrConditionFailed) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("%w: conditionally deactivate active pointer: %w", agentcfg.ErrStateUnavailable, err)
	}
	return true, nil
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

// ListRevisions returns the revision history for the (identity, agent, scope)
// slot, newest first.
//
// The StateStore performs the identity-and-kind-prefix narrowing before
// returning records. The defensive identity check below keeps this consumer
// fail-closed if a driver ever violates that mandatory contract.
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
	recs, err := r.state.ListKindForIdentity(ctx, keys.quad, keys.revPfx)
	if err != nil {
		return nil, fmt.Errorf("%w: list revisions: %w", agentcfg.ErrStateUnavailable, err)
	}
	out := make([]agentcfg.Revision, 0, len(recs))
	for _, sr := range recs {
		// Defensively retain only this identity slot. Agent ID remains an
		// entity key, not an isolation principal.
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

func (r *registry) Rollback(ctx context.Context, id identity.Quadruple, agentID, revisionID string, scope agentcfg.ConfigScope, opts agentcfg.SetOptions) (agentcfg.Revision, error) {
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
	if err := r.guardSignedOAuthMCPFenceWriter(ctx, id, agentID, scope); err != nil {
		return agentcfg.Revision{}, err
	}
	q := keys.quad
	expectations, err := r.activeExpectations(ctx, id, agentID, scope, keys, opts)
	if err != nil {
		return agentcfg.Revision{}, err
	}
	// Load the target revision — a missing target fails loud (never a
	// silent repoint to nothing). This read also proves the revision was
	// never mutated by the repoint below.
	target, err := r.loadRevision(ctx, q, keys.revPfx, revisionID)
	if err != nil {
		return agentcfg.Revision{}, err
	}
	fromID := ""
	active, hasActive, aerr := r.loadActiveRevision(ctx, q, keys.activeKind, keys.revPfx)
	if aerr == nil && hasActive {
		fromID = active.RevisionID
	}
	// The precondition, on the pointer-move door: the CURRENTLY-ACTIVE
	// revision must still carry the expected content or the repoint is
	// refused and the pointer is left where it was. Evaluated before the
	// save, against the same read the from-pointer uses.
	//
	// The unconditional path keeps its historical tolerance of a failed
	// active read (fromID stays empty and the repoint proceeds); a
	// CONDITIONAL caller cannot be answered from a read that failed, so
	// the error is surfaced instead of swallowed.
	if opts.ExpectedContentHash != "" {
		if aerr != nil {
			return agentcfg.Revision{}, aerr
		}
		if err := agentcfg.CheckExpectedRevision(opts, active, hasActive); err != nil {
			return agentcfg.Revision{}, err
		}
	}
	now := r.clock().UTC()
	if err := r.saveActiveIf(ctx, expectations, q, keys.activeKind, revisionID, now); err != nil {
		if errors.Is(err, state.ErrConditionFailed) {
			return agentcfg.Revision{}, fmt.Errorf("%w: active pointer changed during rollback", agentcfg.ErrRevisionConflict)
		}
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
		OAuthProviders: agentcfg.DiffOAuthProviders(from.Payload, to.Payload),
		LLMParams:      agentcfg.DiffLLMParams(from.Payload, to.Payload),
		Hooks:          agentcfg.DiffHooks(from.Payload, to.Payload),
		Naming:         agentcfg.DiffNaming(from.Payload, to.Payload),
		// Order is render order for the additive prompt blocks, so a pure
		// re-ordering is a real change and the diff reports it.
		ExtraSystemBlocks: agentcfg.DiffExtraSystemBlocks(from.Payload, to.Payload),
	}, nil
}

func (r *registry) Close(_ context.Context) error {
	r.closed.Store(true)
	return nil
}

// --- internal helpers ---

// guardSignedOAuthMCPFenceWriter is the cross-runtime half of D-401's
// activation fence. Every Registry pointer-mutating door reaches this driver,
// so a generic writer cannot race a pending first-install candidate into
// authority. Only the internally-marked exact operation may write while the
// fence is pending.
func (r *registry) guardSignedOAuthMCPFenceWriter(ctx context.Context, id identity.Quadruple, agentID string, scope agentcfg.ConfigScope) error {
	if scope != agentcfg.ConfigScopeAgent {
		return nil
	}
	fence, set, err := r.signedOAuthMCPFence(ctx, id.TenantID, agentID)
	if err != nil || !set || fence.Phase != agentcfg.SignedOAuthMCPFencePending {
		return err
	}
	if agentcfg.SignedOAuthMCPFenceOperation(ctx) != fence.OperationKind {
		return fmt.Errorf("%w: foreign writer cannot mutate agent %q while operation %q is pending", agentcfg.ErrSignedCapabilityPending, agentID, fence.OperationKind)
	}
	return nil
}

// applySignedOAuthMCPFence hides exactly the candidate named by a pending
// receipt. A physical active pointer is not authority until the receipt and
// fence both commit; callers observe the prior revision (or no active state).
func (r *registry) applySignedOAuthMCPFence(ctx context.Context, id identity.Quadruple, agentID string, keys scopeKeys, active agentcfg.Revision, set bool) (agentcfg.Revision, bool, error) {
	if !set || active.Payload.SignedOAuthMCPPair == nil {
		return active, set, nil
	}
	fence, fenceSet, err := r.signedOAuthMCPFence(ctx, id.TenantID, agentID)
	if err != nil || !fenceSet || fence.Phase != agentcfg.SignedOAuthMCPFencePending {
		return active, set, err
	}
	pair := active.Payload.SignedOAuthMCPPair
	if pair.AuthorityOperationKind != fence.OperationKind || active.ContentHash != fence.CandidateContentHash ||
		(fence.CandidateRevisionID != "" && active.RevisionID != fence.CandidateRevisionID) {
		return active, set, nil
	}
	if fence.PriorRevisionID == "" {
		return agentcfg.Revision{}, false, nil
	}
	prior, err := r.loadRevision(ctx, keys.quad, keys.revPfx, fence.PriorRevisionID)
	if err != nil {
		return agentcfg.Revision{}, false, fmt.Errorf("%w: load activation-fence prior revision: %w", agentcfg.ErrStateUnavailable, err)
	}
	return prior.toRevision(), true, nil
}

func (r *registry) signedOAuthMCPFence(ctx context.Context, tenant, agentID string) (agentcfg.SignedOAuthMCPActivationFence, bool, error) {
	quad := identity.Quadruple{Identity: identity.Identity{TenantID: tenant, UserID: agentCfgUser, SessionID: agentID}}
	record, err := r.state.Load(ctx, quad, agentcfg.SignedOAuthMCPActivationFenceKind())
	if errors.Is(err, state.ErrNotFound) {
		return agentcfg.SignedOAuthMCPActivationFence{}, false, nil
	}
	if err != nil {
		return agentcfg.SignedOAuthMCPActivationFence{}, false, fmt.Errorf("%w: load signed capability activation fence: %w", agentcfg.ErrStateUnavailable, err)
	}
	var fence agentcfg.SignedOAuthMCPActivationFence
	if err := json.Unmarshal(record.Bytes, &fence); err != nil || fence.TenantID != tenant || fence.AgentID != agentID || fence.OperationKind == "" || fence.CandidateContentHash == "" ||
		(fence.Phase != agentcfg.SignedOAuthMCPFencePending && fence.Phase != agentcfg.SignedOAuthMCPFenceCommitted && fence.Phase != agentcfg.SignedOAuthMCPFenceAborted) {
		return agentcfg.SignedOAuthMCPActivationFence{}, false, fmt.Errorf("%w: corrupt signed capability activation fence", agentcfg.ErrStateUnavailable)
	}
	fence.EventID = record.ID
	return fence, true, nil
}

func (r *registry) loadActiveRevision(ctx context.Context, q identity.Quadruple, activeKind, revPfx string) (agentcfg.Revision, bool, error) {
	rec, err := r.state.Load(ctx, q, activeKind)
	if err != nil {
		if errors.Is(err, state.ErrNotFound) {
			return agentcfg.Revision{}, false, nil
		}
		return agentcfg.Revision{}, false, fmt.Errorf("%w: load active pointer: %w", agentcfg.ErrStateUnavailable, err)
	}
	if activeKind == kindActive {
		pointerState := agentcfg.ClassifyLifecycleRecord(rec.Bytes)
		if pointerState == agentcfg.LifecycleRecordInvalid {
			return agentcfg.Revision{}, false, fmt.Errorf("%w: lifecycle pointer is malformed", agentcfg.ErrStateUnavailable)
		}
		if pointerState == agentcfg.LifecycleRecordTerminal {
			return agentcfg.Revision{}, false, agentcfg.ErrAgentRetired
		}
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
	if ar.Inactive || ar.RevisionID == "" {
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

// loadActivePointerID reads the active-pointer record and returns ONLY the
// revision id it names — it deliberately does not resolve that revision.
//
// The distinction matters exactly once, in the compensating delete: there the
// question is "what does the pointer say", and resolving the revision would
// conflate the answer with the very condition being compensated (a revision
// that is missing). An absent or empty pointer is a KNOWN answer and returns
// ("", nil); a store that cannot answer returns an error, and the caller must
// treat that as "cannot tell" rather than as "absent".
func (r *registry) loadActivePointerID(ctx context.Context, q identity.Quadruple, activeKind string) (string, error) {
	rec, err := r.state.Load(ctx, q, activeKind)
	if err != nil {
		if errors.Is(err, state.ErrNotFound) {
			return "", nil
		}
		return "", fmt.Errorf("%w: load active pointer: %w", agentcfg.ErrStateUnavailable, err)
	}
	if activeKind == kindActive {
		pointerState := agentcfg.ClassifyLifecycleRecord(rec.Bytes)
		if pointerState == agentcfg.LifecycleRecordInvalid {
			return "", fmt.Errorf("%w: lifecycle pointer is malformed", agentcfg.ErrStateUnavailable)
		}
		if pointerState == agentcfg.LifecycleRecordTerminal {
			return "", nil
		}
	}
	if len(rec.Bytes) == 0 {
		return "", nil
	}
	var ar activeRecord
	if err := json.Unmarshal(rec.Bytes, &ar); err != nil {
		return "", fmt.Errorf("%w: unmarshal active pointer: %w", agentcfg.ErrStateUnavailable, err)
	}
	if ar.Schema != 0 && ar.Schema != recordSchema {
		return "", fmt.Errorf("%w: active pointer schema=%d, runtime supports %d", agentcfg.ErrStateUnavailable, ar.Schema, recordSchema)
	}
	if ar.Inactive {
		return "", nil
	}
	return ar.RevisionID, nil
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

// activeExpectations returns the raw active-pointer generations a config write
// must retain. The agent pointer is an authority lifecycle record, not merely
// a best-effort config cache: a valid terminal or malformed record must refuse
// every writer, and the first-write sentinel requires the SLOT itself to be
// absent. SaveIf then rechecks that exact absence at publication, so a
// concurrent tombstone cannot be reactivated between this read and the write.
//
// Agent writes condition their own lifecycle pointer. User writes additionally
// condition the agent-tier lifecycle slot so a terminal lifecycle transition
// wins over a user write that started earlier.
func (r *registry) activeExpectations(ctx context.Context, id identity.Quadruple, agentID string, scope agentcfg.ConfigScope, keys scopeKeys, opts agentcfg.SetOptions) ([]state.SlotExpectation, error) {
	if scope == agentcfg.ConfigScopeAgent {
		lifecycle, err := r.lifecycleExpectation(ctx, keys.quad, keys.activeKind, opts.ExpectedContentHash == agentcfg.ExpectNoActiveRevision)
		if err != nil {
			return nil, err
		}
		return []state.SlotExpectation{lifecycle}, nil
	}

	local, err := r.slotExpectation(ctx, keys.quad, keys.activeKind)
	if err != nil {
		return nil, err
	}
	lifecycle, err := r.lifecycleExpectation(ctx, syntheticQuad(id.TenantID, agentID), kindActive, false)
	if err != nil {
		return nil, err
	}
	return []state.SlotExpectation{local, lifecycle}, nil
}

// lifecycleExpectation reads and classifies the authoritative agent lifecycle
// slot. Its record is intentionally stricter than a generic active pointer:
// the session-personal resolver also consumes these bytes as its authority
// fence. Do not turn a terminal or malformed generation into "no active
// revision", because accepting a config write on that interpretation would
// overwrite a tombstone or erase forensic evidence of corruption.
func (r *registry) lifecycleExpectation(ctx context.Context, q identity.Quadruple, kind string, requireAbsent bool) (state.SlotExpectation, error) {
	rec, err := r.state.Load(ctx, q, kind)
	if errors.Is(err, state.ErrNotFound) {
		return state.SlotExpectation{Identity: q, Kind: kind}, nil
	}
	if err != nil {
		return state.SlotExpectation{}, fmt.Errorf("%w: load lifecycle pointer generation: %w", agentcfg.ErrStateUnavailable, err)
	}
	classification := agentcfg.ClassifyLifecycleRecord(rec.Bytes)
	switch classification {
	case agentcfg.LifecycleRecordInvalid:
		return state.SlotExpectation{}, fmt.Errorf("%w: lifecycle pointer is malformed", agentcfg.ErrStateUnavailable)
	case agentcfg.LifecycleRecordTerminal:
		return state.SlotExpectation{}, agentcfg.ErrAgentRetired
	case agentcfg.LifecycleRecordActive:
		if requireAbsent {
			return state.SlotExpectation{}, fmt.Errorf("%w: expected an absent lifecycle slot", agentcfg.ErrRevisionConflict)
		}
		return state.SlotExpectation{Identity: q, Kind: kind, ExpectedEventID: rec.ID}, nil
	default:
		return state.SlotExpectation{}, fmt.Errorf("%w: unknown lifecycle pointer state", agentcfg.ErrStateUnavailable)
	}
}

// validatePresentLifecycle protects user-tier active reads from a terminal or
// corrupt agent-tier lifecycle. A genuinely absent lifecycle still reports no
// user config through the normal Active path; only a present invalid authority
// record is fail-closed here.
func (r *registry) validatePresentLifecycle(ctx context.Context, q identity.Quadruple, kind string) error {
	rec, err := r.state.Load(ctx, q, kind)
	if errors.Is(err, state.ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("%w: load lifecycle pointer: %w", agentcfg.ErrStateUnavailable, err)
	}
	switch agentcfg.ClassifyLifecycleRecord(rec.Bytes) {
	case agentcfg.LifecycleRecordActive:
		return nil
	case agentcfg.LifecycleRecordTerminal:
		return agentcfg.ErrAgentRetired
	default:
		return fmt.Errorf("%w: lifecycle pointer is malformed", agentcfg.ErrStateUnavailable)
	}
}

func (r *registry) slotExpectation(ctx context.Context, q identity.Quadruple, kind string) (state.SlotExpectation, error) {
	rec, err := r.state.Load(ctx, q, kind)
	if errors.Is(err, state.ErrNotFound) {
		return state.SlotExpectation{Identity: q, Kind: kind}, nil
	}
	if err != nil {
		return state.SlotExpectation{}, fmt.Errorf("%w: load active pointer generation: %w", agentcfg.ErrStateUnavailable, err)
	}
	return state.SlotExpectation{Identity: q, Kind: kind, ExpectedEventID: rec.ID}, nil
}

func (r *registry) saveActiveIf(ctx context.Context, expectations []state.SlotExpectation, q identity.Quadruple, activeKind, revisionID string, now time.Time) error {
	ar := activeRecord{Schema: recordSchema, RevisionID: revisionID, UpdatedAt: now}
	buf, err := json.Marshal(ar)
	if err != nil {
		return fmt.Errorf("agentcfg/statestore: marshal active pointer: %w", err)
	}
	if err := r.state.SaveIf(ctx, expectations, state.StateRecord{
		ID:        state.NewEventID(),
		Identity:  q,
		Kind:      activeKind,
		Bytes:     buf,
		UpdatedAt: now,
	}); err != nil {
		if errors.Is(err, state.ErrConditionFailed) {
			return err
		}
		return fmt.Errorf("%w: conditional save active pointer: %w", agentcfg.ErrStateUnavailable, err)
	}
	return nil
}

// compensationTimeout bounds the compensating delete below. It exists so a
// store that is hanging rather than erroring cannot hold the caller for the
// compensation's sake: the caller's own write has already failed, and the
// error it is owed must not wait on a best-effort cleanup.
const compensationTimeout = 5 * time.Second

// compensateFailedRevisionSave resolves the first Save's ambiguous outcome.
// The active pointer has not been attempted, so an exact candidate record is
// necessarily unreferenced and may be deleted. An absent record needs no
// cleanup. A mismatched or unreadable point-read is retained and reported;
// deleting on a cannot-tell could remove data this call did not create.
func (r *registry) compensateFailedRevisionSave(ctx context.Context, q identity.Quadruple, revPfx string, expected revisionRecord, cause error) error {
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), compensationTimeout)
	defer cancel()
	actual, err := r.loadRevision(cctx, q, revPfx, expected.RevisionID)
	if errors.Is(err, agentcfg.ErrRevisionNotFound) {
		return cause
	}
	if err != nil {
		r.logger.ErrorContext(ctx, "agentcfg/statestore: revision save failed and its exact record could not be re-read; retained rather than deleting on an unknown answer",
			slog.String("revision_id", expected.RevisionID), slog.String("error", err.Error()), slog.String("cause", cause.Error()))
		return fmt.Errorf("%w; revision %s outcome is unknown and was retained: %w", cause, expected.RevisionID, err)
	}
	if !reflect.DeepEqual(actual, expected) {
		r.logger.ErrorContext(ctx, "agentcfg/statestore: revision save failed but the point-read returned different bytes; retained because it is not this call's record",
			slog.String("revision_id", expected.RevisionID), slog.String("cause", cause.Error()))
		return fmt.Errorf("%w; revision id %s resolves to different content and was retained", cause, expected.RevisionID)
	}
	if err := r.state.Delete(cctx, q, revPfx+expected.RevisionID); err != nil {
		return fmt.Errorf("%w; exact unreferenced revision %s could not be removed: %w", cause, expected.RevisionID, err)
	}
	return cause
}

// compensateOrphanRevision removes a revision record whose active-pointer
// write then failed, and returns the error the caller receives.
//
// # The defect it closes
//
// The write path is two ordinary Saves: the immutable revision record, then
// the pointer that makes it the active one. There is no transaction spanning
// them — the StateStore interface has no such primitive, and adding one that
// only two of the three drivers could honour is the "works only on Postgres"
// design smell the persistence rules forbid. So a store error between the two
// leaves a revision that EXISTS and that nothing references.
//
// Such an orphan is inert to every reader: the pointer is the source of
// truth, so `Active` and the run-start projection never see it, and its
// content was never applied to anything. What it does damage is the operator
// view — `list_revisions` enumerates by record kind, not by walking the
// parent chain, so the orphan appears in history between two real revisions
// with no explanation. An operator reading that history sees a revision that
// was never active and a parent chain that skips it, which reads as a lost
// write. It also consumes a revision id that the next successful write's
// parent chain will not mention.
//
// # Why a delete rather than a sweep
//
// The alternative — leaving the record and filtering unreferenced revisions
// out of the reads, or sweeping them later — keeps the damage and adds a
// second mechanism to hide it, and any filter that walked the parent chain
// would need the whole chain resident to answer one list. Deleting at the
// point of failure needs no new interface method (every driver already
// implements Delete), no new record kind, no migration, and no background
// loop; the compensation runs while the caller still holds the per-owner
// write lock, so nothing can be pointing at the record it removes.
//
// # The delete is CONDITIONAL, and that is the load-bearing part
//
// "The write failed" is what the store SAID, not necessarily what the disk
// did. The commonest production shape of a failed write is not a write that
// did not happen: it is a deadline firing after commit, a dropped ack, a
// proxy timeout, a connection reset while the response was in flight. In every
// one of those the pointer is durably on disk naming the revision this
// function was called to remove — and removing it manufactures a DANGLING
// pointer, which is not a tidier failure but a strictly worse one. Every door
// into an agent's config does a read-modify-write through the active pointer,
// and reading a pointer that names nothing fails loud, so no later write can
// repair it: the agent becomes unrecoverable in exchange for suppressing an
// operator-visible history row.
//
// So the compensation re-reads the pointer first and deletes the revision ONLY
// when the pointer does not name it. When it does, the write landed despite the
// error; the record is left exactly where it is, and the returned error says so
// rather than implying a rollback that did not occur.
//
// # An UNKNOWN answer retains the record
//
// A store that refuses the re-read gives no answer at all, and "the pointer is
// absent" is then indistinguishable from "I cannot tell". Deleting on a
// cannot-tell puts the unrecoverable outcome back, on precisely the population
// where it is likeliest — a store sick enough to fail the write is sick enough
// to fail the read. The two costs are not comparable: retaining risks one
// unreferenced row in `list_revisions`, deleting risks an agent no write can
// repair. **The unknown answer therefore retains**, and the residue is reported
// in the returned error and logged at Error rather than swallowed.
//
// # The context is deliberately un-cancellable
//
// The most likely reason the pointer write failed is the caller's context
// being cancelled or timed out — and a compensation issued on that same
// context would then fail on exactly the occasions it is needed. WithoutCancel
// keeps the identity and trace values and drops only the cancellation; the
// bounded timeout keeps a hung store from outliving the call. Both the re-read
// and the delete run on it, for the same reason.
//
// # What it does NOT claim
//
// This is compensation, not atomicity. A process that dies between the two
// writes still leaves an orphan, and so does a store that refuses the delete
// as well as the write — that second case is REPORTED in the returned error
// and logged at Error rather than swallowed, because an unreferenced record
// is a fact an operator must be told about.
//
// # Returns
//
// The boolean reports whether the pointer was found to NAME this revision —
// i.e. the write landed despite the error. It is not "did the compensation
// succeed": an unknown answer reports false, because the caller may only act
// on a confirmed landing. The error is what the caller receives, and it always
// wraps cause.
func (r *registry) compensateOrphanRevision(ctx context.Context, q identity.Quadruple, revPfx, revisionID string, cause error) (bool, error) {
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), compensationTimeout)
	defer cancel()

	activeKind, err := activeKindFor(revPfx)
	if err != nil {
		// Unpairable prefix: no pointer can be read, so nothing may be
		// deleted. Report rather than guess.
		r.logger.ErrorContext(ctx, "agentcfg/statestore: the active-pointer write failed and the revision record could not be checked against the pointer, so it was retained",
			slog.String("revision_id", revisionID),
			slog.String("error", err.Error()),
			slog.String("cause", cause.Error()))
		return false, fmt.Errorf("%w; the revision record %s was retained because its active-pointer kind could not be resolved: %w", cause, revisionID, err)
	}

	pointed, readErr := r.loadActivePointerID(cctx, q, activeKind)
	if readErr != nil {
		r.logger.ErrorContext(ctx, "agentcfg/statestore: the active-pointer write failed AND the pointer could not be re-read, so it is unknown whether the write landed — the revision record was RETAINED rather than deleted, because deleting a revision the pointer may name would leave the agent config unreadable",
			slog.String("revision_id", revisionID),
			slog.String("error", readErr.Error()),
			slog.String("cause", cause.Error()))
		return false, fmt.Errorf("%w; whether the write landed could not be determined, so the revision record %s was retained and may be unreferenced: %w", cause, revisionID, readErr)
	}
	if pointed == revisionID {
		// The store reported failure but the pointer is durably on disk naming
		// this revision: the write landed. Deleting here is what breaks the
		// agent, so the record stays and the caller is told the truth.
		r.logger.WarnContext(ctx, "agentcfg/statestore: the active-pointer write reported failure but the pointer is durably present and names this revision — the write landed, so no compensation was performed",
			slog.String("revision_id", revisionID),
			slog.String("cause", cause.Error()))
		return true, fmt.Errorf("%w; the active pointer nevertheless names revision %s, so the write landed and the revision was retained — re-read the active revision before retrying", cause, revisionID)
	}

	if delErr := r.state.Delete(cctx, q, revPfx+revisionID); delErr != nil {
		r.logger.ErrorContext(ctx, "agentcfg/statestore: the active-pointer write failed AND the compensating delete of its revision record failed — the revision is persisted but unreferenced, and will appear in revision history",
			slog.String("revision_id", revisionID),
			slog.String("error", delErr.Error()),
			slog.String("cause", cause.Error()))
		return false, fmt.Errorf("%w; the revision record %s could not be removed and is now unreferenced: %w", cause, revisionID, delErr)
	}
	return false, cause
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
