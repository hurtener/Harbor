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
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/agentcfg/sessionoverlay"
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

var errLifecycleMalformed = errors.New("agentcfg/statestore: lifecycle envelope malformed")

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

const (
	retirementCleanupSessionPersonal  = "session_personal"
	retirementCleanupLegacyOverlay    = "legacy_session_overlay"
	retirementDiscoveryConfig         = "config"
	retirementDiscoverySignedOAuthMCP = "signed_oauth_mcp"
	retirementDiscoveryPersonal       = "personal"
	retirementDiscoveryLegacy         = "legacy"
	retirementManifestKindPrefix      = "agentcfg.retirement.manifest."
	retirementManifestSchema          = 1
	retirementDiscoveryScanLimit      = 1
	maxRetirementManifestItemBytes    = 1024 * 1024
)

// activeRecord is the JSON-encoded active-pointer record.
type activeRecord struct {
	Schema     int               `json:"schema"`
	RevisionID string            `json:"revision_id"`
	UpdatedAt  time.Time         `json:"updated_at"`
	Retirement *retirementRecord `json:"retirement,omitempty"`
}

// retirementRecord is deliberately embedded in the active slot rather than
// placed beside it: all legacy writers already condition that exact slot, so
// a terminal transition cannot be bypassed by an older config writer.
type retirementRecord struct {
	OperationID      string                     `json:"operation_id"`
	RetiredAt        time.Time                  `json:"retired_at"`
	PriorRevisionID  string                     `json:"prior_revision_id,omitempty"`
	PriorContentHash string                     `json:"prior_content_hash,omitempty"`
	Generation       uint64                     `json:"generation"`
	Completed        bool                       `json:"completed"`
	PendingEvent     *retirementEventCheckpoint `json:"pending_event,omitempty"`
	Discovery        *retirementDiscovery       `json:"discovery,omitempty"`
	ManifestFrozen   bool                       `json:"manifest_frozen,omitempty"`
	ManifestCount    uint64                     `json:"manifest_count"`
	ManifestDigest   string                     `json:"manifest_digest"`
	CleanupCompleted uint64                     `json:"cleanup_completed"`
	CleanupDigest    string                     `json:"cleanup_digest"`
	ScrubCompleted   uint64                     `json:"scrub_completed"`
}

type retirementDiscovery struct {
	Stage        string `json:"stage"`
	Continuation string `json:"continuation"`
	ConfigIndex  uint64 `json:"config_index"`
}

type retirementManifestItem struct {
	Schema        int                         `json:"schema"`
	OperationHash string                      `json:"operation_hash"`
	Ordinal       uint64                      `json:"ordinal"`
	Class         string                      `json:"class"`
	Resource      string                      `json:"resource"`
	PriorDigest   string                      `json:"prior_digest"`
	Digest        string                      `json:"digest"`
	Source        retirementDiscovery         `json:"source"`
	Successor     retirementManifestSuccessor `json:"successor"`
}

type retirementManifestSuccessor struct {
	State          string               `json:"state"`
	Discovery      *retirementDiscovery `json:"discovery,omitempty"`
	ManifestCount  uint64               `json:"manifest_count"`
	ManifestDigest string               `json:"manifest_digest"`
}

type retirementManifestScrub struct {
	Schema        int    `json:"schema"`
	OperationHash string `json:"operation_hash"`
	Ordinal       uint64 `json:"ordinal"`
	PriorDigest   string `json:"prior_digest"`
	Digest        string `json:"digest"`
	Scrubbed      bool   `json:"scrubbed"`
}

type retirementSessionTarget struct {
	TenantID      string `json:"tenant_id"`
	UserID        string `json:"user_id"`
	SessionID     string `json:"session_id"`
	RunID         string `json:"run_id,omitempty"`
	Kind          string `json:"kind"`
	AgentID       string `json:"agent_id"`
	CanonicalName string `json:"canonical_name,omitempty"`
}

type retirementEventCheckpoint struct {
	Stage string `json:"stage"`
	Class string `json:"class,omitempty"`
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
	if err := r.ensureNotRetired(ctx, id, agentID); err != nil {
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
	if scope == agentcfg.ConfigScopeAgent {
		payload, err = preserveSignedOAuthMCPPairs(ctx, active, hasActive, payload)
		if err != nil {
			return agentcfg.Revision{}, err
		}
	}
	norm := agentcfg.NormalizePayload(payload)
	hash, err := agentcfg.ContentHash(norm)
	if err != nil {
		return agentcfg.Revision{}, err
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
			if retiredErr := r.ensureNotRetired(ctx, id, agentID); retiredErr != nil {
				if !errors.Is(retiredErr, errLifecycleMalformed) {
					return agentcfg.Revision{}, retiredErr
				}
			}
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

func preserveSignedOAuthMCPPairs(ctx context.Context, active agentcfg.Revision, hasActive bool, payload agentcfg.ConfigPayload) (agentcfg.ConfigPayload, error) {
	operation := agentcfg.SignedOAuthMCPFenceOperation(ctx)
	current := agentcfg.ConfigPayload{}
	if hasActive {
		current.SignedOAuthMCPPair = active.Payload.SignedOAuthMCPPair
		current.SignedOAuthMCPPairs = active.Payload.SignedOAuthMCPPairs
	}
	requestedOmitted := payload.SignedOAuthMCPPair == nil && payload.SignedOAuthMCPPairs == nil
	if operation == "" && requestedOmitted {
		payload.SignedOAuthMCPPair = current.SignedOAuthMCPPair
		payload.SignedOAuthMCPPairs = current.SignedOAuthMCPPairs
		return payload, nil
	}
	if err := validateSignedOAuthMCPStateTransition(current, payload, operation); err != nil {
		return agentcfg.ConfigPayload{}, err
	}
	if operation == "" {
		payload.SignedOAuthMCPPair = current.SignedOAuthMCPPair
		payload.SignedOAuthMCPPairs = current.SignedOAuthMCPPairs
	}
	return payload, nil
}

func validateSignedOAuthMCPStateTransition(current, requested agentcfg.ConfigPayload, operation string) error {
	currentNorm := agentcfg.NormalizePayload(agentcfg.ConfigPayload{SignedOAuthMCPPair: current.SignedOAuthMCPPair, SignedOAuthMCPPairs: current.SignedOAuthMCPPairs})
	requestedNorm := agentcfg.NormalizePayload(agentcfg.ConfigPayload{SignedOAuthMCPPair: requested.SignedOAuthMCPPair, SignedOAuthMCPPairs: requested.SignedOAuthMCPPairs})
	currentPairs, err := currentNorm.EffectiveSignedOAuthMCPPairs()
	if err != nil {
		return fmt.Errorf("%w: current signed capability state is invalid: %w", agentcfg.ErrSignedCapabilityReplay, err)
	}
	requestedPairs, err := requestedNorm.EffectiveSignedOAuthMCPPairs()
	if err != nil {
		return fmt.Errorf("%w: requested signed capability state is invalid: %w", agentcfg.ErrSignedCapabilityReplay, err)
	}
	if operation == "" {
		if !reflect.DeepEqual(currentNorm, requestedNorm) {
			return fmt.Errorf("%w: generic writer cannot alter signed capability pairs", agentcfg.ErrSignedCapabilityReplay)
		}
		return nil
	}
	if reflect.DeepEqual(currentNorm, requestedNorm) {
		return nil
	}
	changed := 0
	seen := make(map[string]struct{}, len(currentPairs)+len(requestedPairs))
	for provider, pair := range currentPairs {
		seen[provider] = struct{}{}
		requestedPair, ok := requestedPairs[provider]
		if ok {
			if !reflect.DeepEqual(pair, requestedPair) {
				return fmt.Errorf("%w: fenced writer cannot mutate immutable signed capability %q", agentcfg.ErrSignedCapabilityReplay, provider)
			}
			continue
		}
		if pair.AuthorityOperationKind != operation {
			return fmt.Errorf("%w: fenced writer would alter foreign signed capability %q", agentcfg.ErrSignedCapabilityReplay, provider)
		}
		changed++
	}
	for provider, pair := range requestedPairs {
		if _, ok := seen[provider]; ok {
			continue
		}
		if pair.AuthorityOperationKind != operation {
			return fmt.Errorf("%w: fenced writer would create foreign signed capability %q", agentcfg.ErrSignedCapabilityReplay, provider)
		}
		changed++
	}
	if changed != 1 {
		return fmt.Errorf("%w: fenced writer must change exactly one signed capability pair", agentcfg.ErrSignedCapabilityReplay)
	}
	return nil
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
	if err := r.ensureNotRetired(ctx, id, agentID); err != nil {
		return agentcfg.Revision{}, false, err
	}
	active, set, err := r.loadActiveRevision(ctx, keys.quad, keys.activeKind, keys.revPfx)
	if err != nil || scope != agentcfg.ConfigScopeAgent {
		return active, set, err
	}
	return r.applySignedOAuthMCPFence(ctx, id, agentID, keys, active, set)
}

// PhysicalActive returns the revision named by the durable pointer without
// applying the activation-fence visibility projection. It is intentionally a
// recovery-only seam: ordinary authorization callers use Active, while the
// signed-capability reconciler must inspect the hidden candidate in order to
// commit or neutralize that exact revision after a crash.
func (r *registry) PhysicalActive(ctx context.Context, id identity.Quadruple, agentID string, scope agentcfg.ConfigScope) (agentcfg.Revision, bool, error) {
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

// DeactivateIfActive removes the physical active-pointer slot only when it
// still names revisionID. StateStore.DeleteIf applies the exact EventID
// predicate atomically, so compensation restores a genuinely absent authority
// state and can never erase another Runtime's winner.
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
	switch agentcfg.ClassifyLifecycleRecord(current.Bytes) {
	case agentcfg.LifecycleRecordInvalid:
		return false, fmt.Errorf("%w: lifecycle pointer is malformed", agentcfg.ErrStateUnavailable)
	case agentcfg.LifecycleRecordTerminal:
		return false, agentcfg.ErrAgentRetired
	}
	var pointer activeRecord
	if err := json.Unmarshal(current.Bytes, &pointer); err != nil {
		return false, fmt.Errorf("%w: unmarshal active pointer: %w", agentcfg.ErrStateUnavailable, err)
	}
	if pointer.Schema != 0 && pointer.Schema != recordSchema {
		return false, fmt.Errorf("%w: active pointer schema=%d, runtime supports %d", agentcfg.ErrStateUnavailable, pointer.Schema, recordSchema)
	}
	if pointer.RevisionID != revisionID {
		return false, nil
	}
	changed, err := r.state.DeleteIf(ctx, state.SlotExpectation{Identity: keys.quad, Kind: keys.activeKind, ExpectedEventID: current.ID})
	if err != nil {
		return false, fmt.Errorf("%w: conditionally deactivate active pointer: %w", agentcfg.ErrStateUnavailable, err)
	}
	return changed, nil
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
	if err := r.ensureNotRetired(ctx, id, agentID); err != nil {
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
	if aerr != nil {
		// The pointer generation above is only half of the rollback decision:
		// pair immutability and the emitted from-revision require the active
		// revision content too. An unreadable active revision is never treated
		// as an empty one, even for an unconditional caller, and no SaveIf is
		// attempted from partial knowledge.
		return agentcfg.Revision{}, aerr
	}
	if hasActive {
		fromID = active.RevisionID
	}
	if scope == agentcfg.ConfigScopeAgent {
		operation := agentcfg.SignedOAuthMCPFenceOperation(ctx)
		if err := validateSignedOAuthMCPStateTransition(active.Payload, target.Payload, operation); err != nil {
			return agentcfg.Revision{}, fmt.Errorf("%w: rollback would alter immutable signed capability pair", agentcfg.ErrSignedCapabilityReplay)
		}
	}
	// The precondition, on the pointer-move door: the CURRENTLY-ACTIVE
	// revision must still carry the expected content or the repoint is
	// refused and the pointer is left where it was. Evaluated before the
	// save, against the same read the from-pointer uses.
	//
	if opts.ExpectedContentHash != "" {
		if err := agentcfg.CheckExpectedRevision(opts, active, hasActive); err != nil {
			return agentcfg.Revision{}, err
		}
	}
	now := r.clock().UTC()
	if err := r.saveActiveIf(ctx, expectations, q, keys.activeKind, revisionID, now); err != nil {
		if errors.Is(err, state.ErrConditionFailed) {
			if retiredErr := r.ensureNotRetired(ctx, id, agentID); retiredErr != nil {
				return agentcfg.Revision{}, retiredErr
			}
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
		// The versioned virtual-agent profile map diff.
		VirtualAgents: agentcfg.DiffVirtualAgents(from.Payload, to.Payload),
	}, nil
}

func (r *registry) Close(_ context.Context) error {
	r.closed.Store(true)
	return nil
}

// Retire CAS-replaces the agent active slot with the terminal lifecycle
// envelope. It never deletes a revision: the prior id/hash are a durable
// replay/audit anchor and immutable history continues to use its existing
// record kinds.
func (r *registry) Retire(ctx context.Context, id identity.Quadruple, agentID string, req agentcfg.RetirementRequest) (agentcfg.RetirementStatus, error) {
	if err := r.validate(id, agentID); err != nil {
		return agentcfg.RetirementStatus{}, err
	}
	if err := ctx.Err(); err != nil {
		return agentcfg.RetirementStatus{}, err
	}
	operationID := strings.TrimSpace(req.OperationID)
	if operationID == "" || len(operationID) > 128 {
		return agentcfg.RetirementStatus{}, fmt.Errorf("%w: operation id must be 1..128 bytes", agentcfg.ErrRetirementConflict)
	}
	if req.ExpectedContentHash == "" {
		return agentcfg.RetirementStatus{}, fmt.Errorf("%w: expected active content hash is required", agentcfg.ErrRetirementConflict)
	}
	q := syntheticQuad(id.TenantID, agentID)
	current, eventID, found, err := r.loadActiveRecord(ctx, q)
	if err != nil {
		return agentcfg.RetirementStatus{}, err
	}
	if current.Retirement != nil {
		if current.Retirement.OperationID != operationID {
			return agentcfg.RetirementStatus{}, fmt.Errorf("%w: durable operation differs", agentcfg.ErrRetirementConflict)
		}
		// The operation id alone is not a replay authority. The original
		// precondition identifies the retired slot too: a config-backed
		// tombstone replays only with its exact prior hash, while a
		// first-retirement tombstone replays only with the no-active sentinel.
		expected := current.Retirement.PriorContentHash
		if expected == "" {
			expected = agentcfg.ExpectNoActiveRevision
		}
		if req.ExpectedContentHash != expected {
			return agentcfg.RetirementStatus{}, fmt.Errorf("%w: replay expected content hash differs", agentcfg.ErrRetirementConflict)
		}
		return r.resumeRetirement(ctx, id, agentID, q)
	}

	var prior agentcfg.Revision
	hasPrior := false
	if current.RevisionID != "" {
		rr, loadErr := r.loadRevision(ctx, q, kindRevisionPfx, current.RevisionID)
		if loadErr != nil {
			return agentcfg.RetirementStatus{}, loadErr
		}
		prior, hasPrior = rr.toRevision(), true
	}
	if err := agentcfg.CheckExpectedRevision(agentcfg.SetOptions{ExpectedContentHash: req.ExpectedContentHash}, prior, hasPrior); err != nil {
		return agentcfg.RetirementStatus{}, fmt.Errorf("%w: %w", agentcfg.ErrRetirementConflict, err)
	}
	t := &retirementRecord{
		OperationID:    operationID,
		RetiredAt:      r.clock().UTC(),
		Generation:     1,
		PendingEvent:   &retirementEventCheckpoint{Stage: "started"},
		Discovery:      &retirementDiscovery{Stage: retirementDiscoveryConfig},
		ManifestDigest: emptyRetirementManifestDigest(),
		CleanupDigest:  emptyRetirementManifestDigest(),
	}
	if hasPrior {
		t.PriorRevisionID = prior.RevisionID
		t.PriorContentHash = prior.ContentHash
	}
	// Completion is impossible until both tenant-bounded session-record scans are
	// durably exhausted and the manifest is frozen.
	t.Completed = false
	next := activeRecord{Schema: recordSchema, UpdatedAt: t.RetiredAt, Retirement: t}
	buf, err := json.Marshal(next)
	if err != nil {
		return agentcfg.RetirementStatus{}, fmt.Errorf("agentcfg/statestore: marshal retirement tombstone: %w", err)
	}
	expected := state.EventID("")
	if found {
		expected = eventID
	}
	err = r.state.SaveIf(ctx, []state.SlotExpectation{{Identity: q, Kind: kindActive, ExpectedEventID: expected}}, state.StateRecord{
		ID:        state.NewEventID(),
		Identity:  q,
		Kind:      kindActive,
		Bytes:     buf,
		UpdatedAt: t.RetiredAt,
	})
	if err == nil {
		return r.resumeRetirement(ctx, id, agentID, q)
	}
	// A competing retire OR an unknown acknowledgement is resolved only by an
	// exact reread. A SaveIf error is not evidence that the write did not land:
	// the database may have committed the tombstone before its acknowledgement
	// was lost. Never retry or compensate blindly over that ambiguity.
	landed, _, _, rereadErr := r.loadActiveRecord(ctx, q)
	if rereadErr != nil {
		return agentcfg.RetirementStatus{}, fmt.Errorf("%w: save retirement tombstone: %w; reread: %w", agentcfg.ErrStateUnavailable, err, rereadErr)
	}
	if landed.Retirement != nil && landed.Retirement.OperationID == operationID {
		return r.resumeRetirement(ctx, id, agentID, q)
	}
	if !errors.Is(err, state.ErrConditionFailed) {
		return agentcfg.RetirementStatus{}, fmt.Errorf("%w: save retirement tombstone: %w", agentcfg.ErrStateUnavailable, err)
	}
	return agentcfg.RetirementStatus{}, fmt.Errorf("%w: active slot moved", agentcfg.ErrRetirementConflict)
}

// resumeRetirement first drains the durable started-event checkpoint, then
// advances the deterministic session-record scans one CAS checkpoint at a time.
// A same-operation replay resumes from the stored opaque continuation.
func (r *registry) resumeRetirement(ctx context.Context, id identity.Quadruple, agentID string, q identity.Quadruple) (agentcfg.RetirementStatus, error) {
	for {
		current, eventID, found, err := r.loadActiveRecord(ctx, q)
		if err != nil {
			return agentcfg.RetirementStatus{}, err
		}
		if !found || current.Retirement == nil {
			return agentcfg.RetirementStatus{}, fmt.Errorf("%w: retirement lifecycle disappeared", agentcfg.ErrRetirementConflict)
		}
		if current.Retirement.PendingEvent != nil {
			if current.Retirement.PendingEvent.Stage == "progress" && current.Retirement.ScrubCompleted < current.Retirement.CleanupCompleted {
				if err := r.scrubRetirementDebt(ctx, q); err != nil {
					return agentcfg.RetirementStatus{}, err
				}
			}
			if _, err := r.ackRetirementEvent(ctx, id, agentID, q); err != nil {
				return agentcfg.RetirementStatus{}, err
			}
			continue
		}
		if current.Retirement.ManifestFrozen {
			if current.Retirement.ScrubCompleted < current.Retirement.CleanupCompleted {
				if err := r.scrubRetirementDebt(ctx, q); err != nil {
					return agentcfg.RetirementStatus{}, err
				}
				continue
			}
			if !current.Retirement.Completed && current.Retirement.CleanupCompleted == current.Retirement.ManifestCount && current.Retirement.ScrubCompleted == current.Retirement.ManifestCount {
				return r.queueRetirementEvent(ctx, id, agentID, q, "completed", "")
			}
			return r.retirementStatus(ctx, q, current.Retirement)
		}
		if current.Retirement.Discovery == nil {
			return agentcfg.RetirementStatus{}, fmt.Errorf("%w: retirement discovery checkpoint is absent", agentcfg.ErrStateUnavailable)
		}
		item, found, err := r.loadPendingRetirementManifestItem(ctx, q, current.Retirement)
		if err != nil {
			return agentcfg.RetirementStatus{}, err
		}
		var nextDiscovery *retirementDiscovery
		frozen := false
		if found {
			nextDiscovery = item.Successor.Discovery
			frozen = item.Successor.State == "frozen"
		} else {
			item, nextDiscovery, frozen, err = r.nextRetirementDiscovery(ctx, q, id.TenantID, agentID, current.Retirement)
			if err != nil {
				return agentcfg.RetirementStatus{}, err
			}
			if item != nil {
				if err := r.persistRetirementManifestItem(ctx, q, eventID, *item); err != nil {
					return agentcfg.RetirementStatus{}, err
				}
			}
		}
		if item != nil {
			if err := validateRetirementManifestSuccessor(current.Retirement, *item); err != nil {
				return agentcfg.RetirementStatus{}, err
			}
		}
		next := current
		next.Retirement = cloneRetirement(current.Retirement)
		next.Retirement.Generation++
		if item != nil {
			next.Retirement.ManifestDigest = item.Successor.ManifestDigest
			next.Retirement.ManifestCount = item.Successor.ManifestCount
		}
		if frozen {
			next.Retirement.Discovery = nil
			next.Retirement.ManifestFrozen = true
			next.Retirement.Completed = next.Retirement.ManifestCount == 0
			if next.Retirement.Completed {
				next.Retirement.PendingEvent = &retirementEventCheckpoint{Stage: "completed"}
			}
		} else {
			next.Retirement.Discovery = nextDiscovery
		}
		next.UpdatedAt = r.clock().UTC()
		buf, err := json.Marshal(next)
		if err != nil {
			return agentcfg.RetirementStatus{}, fmt.Errorf("agentcfg/statestore: marshal retirement discovery: %w", err)
		}
		if len(buf) > agentcfg.MaxLifecycleRecordBytes {
			return agentcfg.RetirementStatus{}, fmt.Errorf("%w: retirement manifest exceeds lifecycle record bound", agentcfg.ErrStateUnavailable)
		}
		err = r.state.SaveIf(ctx, []state.SlotExpectation{{Identity: q, Kind: kindActive, ExpectedEventID: eventID}}, state.StateRecord{ID: state.NewEventID(), Identity: q, Kind: kindActive, Bytes: buf, UpdatedAt: next.UpdatedAt})
		if err == nil {
			if next.Retirement.PendingEvent != nil {
				return r.ackRetirementEvent(ctx, id, agentID, q)
			}
			continue
		}
		landed, _, _, rereadErr := r.loadActiveRecord(ctx, q)
		if rereadErr != nil {
			return agentcfg.RetirementStatus{}, fmt.Errorf("%w: save retirement discovery: %w; reread: %w", agentcfg.ErrStateUnavailable, err, rereadErr)
		}
		if landed.Retirement != nil && landed.Retirement.OperationID == current.Retirement.OperationID && landed.Retirement.Generation >= next.Retirement.Generation {
			continue
		}
		if !errors.Is(err, state.ErrConditionFailed) {
			return agentcfg.RetirementStatus{}, fmt.Errorf("%w: save retirement discovery: %w", agentcfg.ErrStateUnavailable, err)
		}
		return agentcfg.RetirementStatus{}, fmt.Errorf("%w: retirement discovery moved", agentcfg.ErrRetirementConflict)
	}
}

func (r *registry) nextRetirementDiscovery(ctx context.Context, q identity.Quadruple, tenantID, agentID string, retirement *retirementRecord) (*retirementManifestItem, *retirementDiscovery, bool, error) {
	checkpoint := *retirement.Discovery
	switch checkpoint.Stage {
	case retirementDiscoveryConfig:
		var steps []agentcfg.CleanupStep
		if retirement.PriorRevisionID != "" {
			prior, err := r.loadRevision(ctx, q, kindRevisionPfx, retirement.PriorRevisionID)
			if err != nil {
				return nil, nil, false, err
			}
			steps = cleanupManifest(prior.toRevision())
		}
		if checkpoint.ConfigIndex < uint64(len(steps)) {
			step := steps[checkpoint.ConfigIndex]
			checkpoint.ConfigIndex++
			if checkpoint.ConfigIndex == uint64(len(steps)) {
				checkpoint = retirementDiscovery{Stage: retirementDiscoverySignedOAuthMCP}
			}
			item, err := newRetirementManifestItem(retirement, step.Class, step.Resource, &checkpoint, false)
			return item, &checkpoint, false, err
		}
		return nil, &retirementDiscovery{Stage: retirementDiscoverySignedOAuthMCP}, false, nil
	case retirementDiscoverySignedOAuthMCP:
		operations, err := agentcfg.NewSignedOAuthMCPOperationStore(r.state)
		if err != nil {
			return nil, nil, false, err
		}
		page, continuation, err := operations.ScanTenantPage(ctx, tenantID, retirementDiscoveryScanLimit, checkpoint.Continuation)
		if err != nil {
			return nil, nil, false, fmt.Errorf("%w: scan signed OAuth MCP retirement records: %w", agentcfg.ErrStateUnavailable, err)
		}
		next := &retirementDiscovery{Stage: retirementDiscoverySignedOAuthMCP, Continuation: continuation}
		if continuation == "" {
			next = &retirementDiscovery{Stage: retirementDiscoveryPersonal}
		}
		if len(page) == 0 {
			return nil, next, false, nil
		}
		op := page[0]
		if op.Binding.AgentID != agentID || !agentcfg.SignedOAuthMCPRetirementPending(op.Phase) {
			return nil, next, false, nil
		}
		resource, err := agentcfg.SignedOAuthMCPRetirementResource(op)
		if err != nil {
			return nil, nil, false, err
		}
		item, err := newRetirementManifestItem(retirement, agentcfg.RetirementCleanupClassSignedOAuthMCPPair, resource, next, false)
		return item, next, false, err
	case retirementDiscoveryPersonal, retirementDiscoveryLegacy:
		prefix := sessionoverlay.LegacyOverlayPrefix()
		if checkpoint.Stage == retirementDiscoveryPersonal {
			var err error
			prefix, err = sessionoverlay.PersonalSkillPrefix(agentID)
			if err != nil {
				return nil, nil, false, err
			}
		}
		page, err := r.state.ScanKindForTenant(ctx, state.ListScope{MaintenanceScoped: true}, tenantID, prefix, retirementDiscoveryScanLimit, checkpoint.Continuation)
		if err != nil {
			return nil, nil, false, fmt.Errorf("%w: scan retirement %s records: %w", agentcfg.ErrStateUnavailable, checkpoint.Stage, err)
		}
		next := &retirementDiscovery{Stage: checkpoint.Stage, Continuation: page.Continuation}
		frozen := false
		if page.Continuation == "" {
			if checkpoint.Stage == retirementDiscoveryPersonal {
				next = &retirementDiscovery{Stage: retirementDiscoveryLegacy}
			} else {
				next = nil
				frozen = true
			}
		}
		if len(page.Records) == 0 {
			return nil, next, frozen, nil
		}
		candidate := page.Records[0]
		target := retirementSessionTarget{TenantID: candidate.Identity.TenantID, UserID: candidate.Identity.UserID, SessionID: candidate.Identity.SessionID, RunID: candidate.Identity.RunID, Kind: candidate.Kind, AgentID: agentID}
		if checkpoint.Stage == retirementDiscoveryPersonal {
			canonicalName, err := sessionoverlay.InspectRetirementPersonalCandidate(candidate, tenantID, agentID)
			if err != nil {
				return nil, nil, false, err
			}
			target.CanonicalName = canonicalName
			resource, err := encodeRetirementSessionTarget(target)
			if err != nil {
				return nil, nil, false, err
			}
			item, err := newRetirementManifestItem(retirement, retirementCleanupSessionPersonal, resource, next, frozen)
			return item, next, frozen, err
		}
		if candidate.Kind != sessionoverlay.LegacyOverlayKind(agentID) {
			return nil, next, frozen, nil
		}
		if err := sessionoverlay.InspectRetirementLegacyCandidate(candidate, tenantID, agentID); err != nil {
			return nil, nil, false, err
		}
		resource, err := encodeRetirementSessionTarget(target)
		if err != nil {
			return nil, nil, false, err
		}
		item, err := newRetirementManifestItem(retirement, retirementCleanupLegacyOverlay, resource, next, frozen)
		return item, next, frozen, err
	default:
		return nil, nil, false, fmt.Errorf("%w: unknown retirement discovery stage %q", agentcfg.ErrStateUnavailable, checkpoint.Stage)
	}
}

func newRetirementManifestItem(retirement *retirementRecord, class, resource string, next *retirementDiscovery, frozen bool) (*retirementManifestItem, error) {
	stateName := "discovering"
	if frozen {
		stateName = "frozen"
	}
	item := &retirementManifestItem{
		Schema:        retirementManifestSchema,
		OperationHash: agentcfg.RetirementOperationHash(retirement.OperationID),
		Ordinal:       retirement.ManifestCount,
		Class:         class,
		Resource:      resource,
		PriorDigest:   retirement.ManifestDigest,
		Source:        *retirement.Discovery,
		Successor: retirementManifestSuccessor{
			State:         stateName,
			Discovery:     next,
			ManifestCount: retirement.ManifestCount + 1,
		},
	}
	digest, err := advanceRetirementManifestDigest(item.PriorDigest, *item)
	if err != nil {
		return nil, err
	}
	item.Digest = digest
	item.Successor.ManifestDigest = digest
	return item, nil
}

func (r *registry) persistRetirementManifestItem(ctx context.Context, q identity.Quadruple, lifecycleEvent state.EventID, item retirementManifestItem) error {
	kind := retirementManifestKind(item.OperationHash, item.Ordinal)
	encoded, err := json.Marshal(item)
	if err != nil {
		return fmt.Errorf("marshal retirement manifest item: %w", err)
	}
	if len(encoded) == 0 || len(encoded) > maxRetirementManifestItemBytes {
		return fmt.Errorf("%w: retirement manifest item size %d exceeds bound", agentcfg.ErrStateUnavailable, len(encoded))
	}
	existing, err := r.state.Load(ctx, q, kind)
	if err == nil {
		if bytes.Equal(existing.Bytes, encoded) {
			return nil
		}
		return fmt.Errorf("%w: manifest ordinal already carries different content", agentcfg.ErrRetirementConflict)
	}
	if !errors.Is(err, state.ErrNotFound) {
		return fmt.Errorf("%w: load retirement manifest item: %w", agentcfg.ErrStateUnavailable, err)
	}
	next := state.StateRecord{ID: state.NewEventID(), Identity: q, Kind: kind, Bytes: encoded, UpdatedAt: r.clock().UTC()}
	expectations := []state.SlotExpectation{{Identity: q, Kind: kind}, {Identity: q, Kind: kindActive, ExpectedEventID: lifecycleEvent}}
	if err := r.state.SaveIf(ctx, expectations, next); err != nil {
		landed, loadErr := r.state.Load(ctx, q, kind)
		if loadErr == nil && bytes.Equal(landed.Bytes, encoded) {
			return nil
		}
		if errors.Is(err, state.ErrConditionFailed) {
			return fmt.Errorf("%w: retirement manifest lifecycle moved", agentcfg.ErrRetirementConflict)
		}
		return fmt.Errorf("%w: persist retirement manifest item: %w", agentcfg.ErrStateUnavailable, err)
	}
	return nil
}

func (r *registry) loadPendingRetirementManifestItem(ctx context.Context, q identity.Quadruple, retirement *retirementRecord) (*retirementManifestItem, bool, error) {
	operationHash := agentcfg.RetirementOperationHash(retirement.OperationID)
	item, scrub, _, found, err := r.loadRetirementManifestRecord(ctx, q, operationHash, retirement.ManifestCount)
	if err != nil || !found {
		return item, found, err
	}
	if scrub != nil || item == nil {
		return nil, false, fmt.Errorf("%w: occupied pending manifest ordinal is not a full item", agentcfg.ErrRetirementConflict)
	}
	if err := validateRetirementManifestSuccessor(retirement, *item); err != nil {
		return nil, false, err
	}
	return item, true, nil
}

func validateRetirementManifestSuccessor(retirement *retirementRecord, item retirementManifestItem) error {
	if retirement.Discovery == nil || item.Ordinal != retirement.ManifestCount || item.PriorDigest != retirement.ManifestDigest || item.Source != *retirement.Discovery {
		return fmt.Errorf("%w: manifest item does not match its lifecycle predecessor", agentcfg.ErrRetirementConflict)
	}
	digest, err := advanceRetirementManifestDigest(item.PriorDigest, item)
	if err != nil {
		return err
	}
	if item.Digest != digest || item.Successor.ManifestCount != retirement.ManifestCount+1 || item.Successor.ManifestDigest != item.Digest {
		return fmt.Errorf("%w: manifest item successor authority is invalid", agentcfg.ErrRetirementConflict)
	}
	switch item.Successor.State {
	case "discovering":
		if item.Successor.Discovery == nil || !validRetirementDiscovery(*item.Successor.Discovery) {
			return fmt.Errorf("%w: manifest successor lacks valid discovery state", agentcfg.ErrRetirementConflict)
		}
	case "frozen":
		if item.Successor.Discovery != nil {
			return fmt.Errorf("%w: frozen manifest successor retains discovery state", agentcfg.ErrRetirementConflict)
		}
	default:
		return fmt.Errorf("%w: unknown manifest successor state", agentcfg.ErrRetirementConflict)
	}
	return nil
}

func validRetirementDiscovery(discovery retirementDiscovery) bool {
	if discovery.Stage != retirementDiscoveryConfig && discovery.Stage != retirementDiscoverySignedOAuthMCP && discovery.Stage != retirementDiscoveryPersonal && discovery.Stage != retirementDiscoveryLegacy {
		return false
	}
	return (discovery.Stage != retirementDiscoveryConfig || discovery.Continuation == "") && (discovery.Stage == retirementDiscoveryConfig || discovery.ConfigIndex == 0)
}

func retirementManifestKind(operationHash string, ordinal uint64) string {
	return fmt.Sprintf("%s%s.%020d", retirementManifestKindPrefix, operationHash, ordinal)
}

func emptyRetirementManifestDigest() string {
	sum := sha256.Sum256(nil)
	return hex.EncodeToString(sum[:])
}

func advanceRetirementManifestDigest(previous string, item retirementManifestItem) (string, error) {
	encoded, err := json.Marshal(struct {
		Schema         int                  `json:"schema"`
		OperationHash  string               `json:"operation_hash"`
		Ordinal        uint64               `json:"ordinal"`
		Class          string               `json:"class"`
		Resource       string               `json:"resource"`
		Source         retirementDiscovery  `json:"source"`
		SuccessorState string               `json:"successor_state"`
		Discovery      *retirementDiscovery `json:"successor_discovery,omitempty"`
		ManifestCount  uint64               `json:"successor_manifest_count"`
	}{
		Schema: item.Schema, OperationHash: item.OperationHash, Ordinal: item.Ordinal,
		Class: item.Class, Resource: item.Resource, Source: item.Source,
		SuccessorState: item.Successor.State, Discovery: item.Successor.Discovery,
		ManifestCount: item.Successor.ManifestCount,
	})
	if err != nil {
		return "", fmt.Errorf("marshal retirement manifest digest input: %w", err)
	}
	sum := sha256.Sum256(append(append([]byte(previous), '\n'), encoded...))
	return hex.EncodeToString(sum[:]), nil
}

func encodeRetirementSessionTarget(target retirementSessionTarget) (string, error) {
	data, err := json.Marshal(target)
	if err != nil {
		return "", fmt.Errorf("marshal retirement session target: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}

func decodeRetirementSessionTarget(resource string) (retirementSessionTarget, error) {
	data, err := base64.RawURLEncoding.DecodeString(resource)
	if err != nil || len(data) == 0 || len(data) > maxRetirementManifestItemBytes {
		return retirementSessionTarget{}, fmt.Errorf("%w: invalid retirement session resource", agentcfg.ErrRetirementConflict)
	}
	if err := agentcfg.ValidateUniqueJSONFields(data); err != nil {
		return retirementSessionTarget{}, fmt.Errorf("%w: duplicate retirement session resource field", agentcfg.ErrRetirementConflict)
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	var target retirementSessionTarget
	if err := decoder.Decode(&target); err != nil || target.TenantID == "" || target.UserID == "" || target.SessionID == "" || target.Kind == "" || target.AgentID == "" {
		return retirementSessionTarget{}, fmt.Errorf("%w: invalid retirement session resource", agentcfg.ErrRetirementConflict)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return retirementSessionTarget{}, fmt.Errorf("%w: trailing retirement session resource", agentcfg.ErrRetirementConflict)
	}
	return target, nil
}

// ackRetirementEvent implements the durable at-least-once transition: the
// pending checkpoint was written before this publish, and is CAS-cleared only
// after delivery. A publish failure remains pending for the same-operation
// retry; duplicate publication is therefore safe and intentional.
func (r *registry) ackRetirementEvent(ctx context.Context, id identity.Quadruple, agentID string, q identity.Quadruple) (agentcfg.RetirementStatus, error) {
	current, eventID, _, err := r.loadActiveRecord(ctx, q)
	if err != nil {
		return agentcfg.RetirementStatus{}, err
	}
	if current.Retirement == nil {
		return agentcfg.RetirementStatus{}, fmt.Errorf("%w: lifecycle disappeared", agentcfg.ErrRetirementConflict)
	}
	p := current.Retirement.PendingEvent
	if p == nil {
		return r.retirementStatus(ctx, q, current.Retirement)
	}
	var typ events.EventType
	switch p.Stage {
	case "started":
		typ = agentcfg.EventTypeRetirementStarted
	case "completed":
		typ = agentcfg.EventTypeRetirementCompleted
	default:
		typ = agentcfg.EventTypeRetirementProgress
	}
	completed, err := retirementEventCount(current.Retirement.CleanupCompleted)
	if err != nil {
		return agentcfg.RetirementStatus{}, err
	}
	total, err := retirementEventCount(current.Retirement.ManifestCount)
	if err != nil {
		return agentcfg.RetirementStatus{}, err
	}
	if err := r.bus.Publish(ctx, events.Event{Type: typ, Identity: id, OccurredAt: r.clock().UTC(), Payload: agentcfg.RetirementEventPayload{AgentID: agentID, OperationHash: agentcfg.RetirementOperationHash(current.Retirement.OperationID), Stage: p.Stage, Class: p.Class, Completed: completed, Total: total, Generation: current.Retirement.Generation, OccurredAt: r.clock().UTC()}}); err != nil {
		return agentcfg.RetirementStatus{}, fmt.Errorf("%w: publish retirement %s: %w", agentcfg.ErrStateUnavailable, p.Stage, err)
	}
	next := current
	next.Retirement = cloneRetirement(current.Retirement)
	next.Retirement.PendingEvent = nil
	if p.Stage == "progress" && next.Retirement.ManifestFrozen && next.Retirement.CleanupCompleted == next.Retirement.ManifestCount && next.Retirement.ScrubCompleted == next.Retirement.ManifestCount {
		next.Retirement.Completed = true
		next.Retirement.PendingEvent = &retirementEventCheckpoint{Stage: "completed"}
	}
	next.UpdatedAt = r.clock().UTC()
	buf, err := json.Marshal(next)
	if err != nil {
		return agentcfg.RetirementStatus{}, fmt.Errorf("marshal retirement event acknowledgement: %w", err)
	}
	if err := r.state.SaveIf(ctx, []state.SlotExpectation{{Identity: q, Kind: kindActive, ExpectedEventID: eventID}}, state.StateRecord{ID: state.NewEventID(), Identity: q, Kind: kindActive, Bytes: buf, UpdatedAt: next.UpdatedAt}); err != nil {
		landed, _, _, rereadErr := r.loadActiveRecord(ctx, q)
		if rereadErr != nil {
			return agentcfg.RetirementStatus{}, fmt.Errorf("%w: acknowledge retirement event: %w; reread: %w", agentcfg.ErrStateUnavailable, err, rereadErr)
		}
		if landed.Retirement != nil && landed.Retirement.OperationID == current.Retirement.OperationID {
			if landed.Retirement.PendingEvent == nil {
				if p.Stage == "started" && landed.Retirement.Completed {
					return r.queueRetirementEvent(ctx, id, agentID, q, "completed", "")
				}
				return r.retirementStatus(ctx, q, landed.Retirement)
			}
			if errors.Is(err, state.ErrConditionFailed) {
				return r.ackRetirementEvent(ctx, id, agentID, q)
			}
		}
		return agentcfg.RetirementStatus{}, fmt.Errorf("%w: acknowledge retirement event: %w", agentcfg.ErrStateUnavailable, err)
	}
	// An empty manifest is still a two-transition lifecycle. Persist the
	// completed checkpoint only after the started acknowledgement wins, so a
	// crash/retry cannot reorder or lose either canonical event.
	if p.Stage == "started" && next.Retirement.Completed {
		return r.queueRetirementEvent(ctx, id, agentID, q, "completed", "")
	}
	if next.Retirement.PendingEvent != nil {
		return r.ackRetirementEvent(ctx, id, agentID, q)
	}
	return r.retirementStatus(ctx, q, next.Retirement)
}

func retirementEventCount(value uint64) (int, error) {
	out, err := strconv.Atoi(strconv.FormatUint(value, 10))
	if err != nil {
		return 0, fmt.Errorf("%w: retirement event count exceeds platform int: %w", agentcfg.ErrStateUnavailable, err)
	}
	return out, nil
}

func (r *registry) queueRetirementEvent(ctx context.Context, id identity.Quadruple, agentID string, q identity.Quadruple, stage, class string) (agentcfg.RetirementStatus, error) {
	current, eventID, _, err := r.loadActiveRecord(ctx, q)
	if err != nil {
		return agentcfg.RetirementStatus{}, err
	}
	if current.Retirement == nil || current.Retirement.PendingEvent != nil {
		return r.ackRetirementEvent(ctx, id, agentID, q)
	}
	next := current
	next.Retirement = cloneRetirement(current.Retirement)
	if stage == "completed" {
		if !next.Retirement.ManifestFrozen || next.Retirement.CleanupCompleted != next.Retirement.ManifestCount || next.Retirement.ScrubCompleted != next.Retirement.ManifestCount {
			return agentcfg.RetirementStatus{}, fmt.Errorf("%w: retirement completion precedes cleanup scrubbing", agentcfg.ErrRetirementConflict)
		}
		next.Retirement.Completed = true
	}
	next.Retirement.PendingEvent = &retirementEventCheckpoint{Stage: stage, Class: class}
	next.UpdatedAt = r.clock().UTC()
	buf, err := json.Marshal(next)
	if err != nil {
		return agentcfg.RetirementStatus{}, fmt.Errorf("marshal retirement event checkpoint: %w", err)
	}
	if err := r.state.SaveIf(ctx, []state.SlotExpectation{{Identity: q, Kind: kindActive, ExpectedEventID: eventID}}, state.StateRecord{ID: state.NewEventID(), Identity: q, Kind: kindActive, Bytes: buf, UpdatedAt: next.UpdatedAt}); err != nil {
		landed, _, _, rereadErr := r.loadActiveRecord(ctx, q)
		if rereadErr != nil {
			return agentcfg.RetirementStatus{}, fmt.Errorf("%w: save retirement event checkpoint: %w; reread: %w", agentcfg.ErrStateUnavailable, err, rereadErr)
		}
		if landed.Retirement != nil && landed.Retirement.OperationID == current.Retirement.OperationID {
			if landed.Retirement.PendingEvent != nil {
				return r.ackRetirementEvent(ctx, id, agentID, q)
			}
			if errors.Is(err, state.ErrConditionFailed) {
				return r.queueRetirementEvent(ctx, id, agentID, q, stage, class)
			}
		}
		return agentcfg.RetirementStatus{}, fmt.Errorf("%w: save retirement event checkpoint: %w", agentcfg.ErrStateUnavailable, err)
	}
	return r.ackRetirementEvent(ctx, id, agentID, q)
}

func cloneRetirement(in *retirementRecord) *retirementRecord {
	out := *in
	if in.PendingEvent != nil {
		p := *in.PendingEvent
		out.PendingEvent = &p
	}
	if in.Discovery != nil {
		d := *in.Discovery
		out.Discovery = &d
	}
	return &out
}

// RetirementStatus reads the lifecycle envelope, its next frozen manifest
// item, and that item's exact personal target/fences when applicable. It is
// intentionally available after retirement so a same-operation caller can
// resume cleanup; it does not reveal config content.
func (r *registry) RetirementStatus(ctx context.Context, id identity.Quadruple, agentID string) (agentcfg.RetirementStatus, bool, error) {
	if err := r.validate(id, agentID); err != nil {
		return agentcfg.RetirementStatus{}, false, err
	}
	if err := ctx.Err(); err != nil {
		return agentcfg.RetirementStatus{}, false, err
	}
	rec, _, _, err := r.loadActiveRecord(ctx, syntheticQuad(id.TenantID, agentID))
	if err != nil {
		return agentcfg.RetirementStatus{}, false, err
	}
	if rec.Retirement == nil {
		return agentcfg.RetirementStatus{}, false, nil
	}
	if rec.Retirement.ManifestFrozen && rec.Retirement.ScrubCompleted < rec.Retirement.CleanupCompleted {
		if err := r.scrubRetirementDebt(ctx, syntheticQuad(id.TenantID, agentID)); err != nil {
			return agentcfg.RetirementStatus{}, true, err
		}
		rec, _, _, err = r.loadActiveRecord(ctx, syntheticQuad(id.TenantID, agentID))
		if err != nil {
			return agentcfg.RetirementStatus{}, true, err
		}
	}
	status, err := r.retirementStatus(ctx, syntheticQuad(id.TenantID, agentID), rec.Retirement)
	return status, true, err
}

// CompleteRetirementStep records completion only after the owner-scoped live
// teardown has succeeded. The frozen manifest is the authority: callers
// cannot invent an item, and a stale process cannot acknowledge a newer
// lifecycle generation.
func (r *registry) CompleteRetirementStep(ctx context.Context, id identity.Quadruple, agentID, operationID, class, resource string) (agentcfg.RetirementStatus, error) {
	if err := r.validate(id, agentID); err != nil {
		return agentcfg.RetirementStatus{}, err
	}
	if err := ctx.Err(); err != nil {
		return agentcfg.RetirementStatus{}, err
	}
	q := syntheticQuad(id.TenantID, agentID)
	current, eventID, found, err := r.loadActiveRecord(ctx, q)
	if err != nil {
		return agentcfg.RetirementStatus{}, err
	}
	if !found || current.Retirement == nil || current.Retirement.OperationID != operationID {
		return agentcfg.RetirementStatus{}, fmt.Errorf("%w: lifecycle operation changed", agentcfg.ErrRetirementConflict)
	}
	// One persisted event checkpoint is the ordering barrier for the entire
	// cleanup manifest. A later step may not overwrite a failed earlier
	// progress/completed event: flush it first, and make the caller retry this
	// step after the prior transition is acknowledged.
	if current.Retirement.PendingEvent != nil {
		if current.Retirement.PendingEvent.Stage == "progress" && current.Retirement.ScrubCompleted < current.Retirement.CleanupCompleted {
			if err := r.scrubRetirementDebt(ctx, q); err != nil {
				return agentcfg.RetirementStatus{}, err
			}
		}
		return r.ackRetirementEvent(ctx, id, agentID, q)
	}
	if !current.Retirement.ManifestFrozen {
		return agentcfg.RetirementStatus{}, fmt.Errorf("%w: cleanup manifest is not frozen", agentcfg.ErrRetirementConflict)
	}
	if current.Retirement.CleanupCompleted >= current.Retirement.ManifestCount {
		return r.retirementStatus(ctx, q, current.Retirement)
	}
	if current.Retirement.ScrubCompleted != current.Retirement.CleanupCompleted {
		if err := r.scrubRetirementDebt(ctx, q); err != nil {
			return agentcfg.RetirementStatus{}, err
		}
		return r.CompleteRetirementStep(ctx, id, agentID, operationID, class, resource)
	}
	item, err := r.verifiedRetirementCleanupItem(ctx, q, current.Retirement)
	if err != nil {
		return agentcfg.RetirementStatus{}, err
	}
	if item.Class != class || item.Resource != resource {
		return agentcfg.RetirementStatus{}, fmt.Errorf("%w: cleanup item is not the next frozen manifest entry", agentcfg.ErrRetirementConflict)
	}
	if class == retirementCleanupSessionPersonal || class == retirementCleanupLegacyOverlay {
		if err := r.completeSessionRetirementStep(ctx, id.TenantID, agentID, class, resource); err != nil {
			return agentcfg.RetirementStatus{}, err
		}
	}
	next := current
	next.Retirement = cloneRetirement(current.Retirement)
	next.Retirement.Generation++
	next.Retirement.CleanupCompleted++
	next.Retirement.CleanupDigest = item.Digest
	next.Retirement.Completed = false
	next.Retirement.PendingEvent = &retirementEventCheckpoint{Stage: "progress", Class: class}
	next.UpdatedAt = r.clock().UTC()
	buf, err := json.Marshal(next)
	if err != nil {
		return agentcfg.RetirementStatus{}, fmt.Errorf("agentcfg/statestore: marshal retirement progress: %w", err)
	}
	err = r.state.SaveIf(ctx, []state.SlotExpectation{{Identity: q, Kind: kindActive, ExpectedEventID: eventID}}, state.StateRecord{
		ID: state.NewEventID(), Identity: q, Kind: kindActive, Bytes: buf, UpdatedAt: next.UpdatedAt,
	})
	if err == nil {
		if err := r.scrubRetirementDebt(ctx, q); err != nil {
			return agentcfg.RetirementStatus{}, err
		}
		return r.ackRetirementEvent(ctx, id, agentID, q)
	}
	// A commit followed by a lost acknowledgement converges by exact reread;
	// any other movement is a conflict, never a blind retry over new state.
	landed, _, _, rereadErr := r.loadActiveRecord(ctx, q)
	if rereadErr != nil {
		return agentcfg.RetirementStatus{}, fmt.Errorf("%w: save retirement progress: %w; reread: %w", agentcfg.ErrStateUnavailable, err, rereadErr)
	}
	if landed.Retirement != nil && landed.Retirement.OperationID == operationID {
		if landed.Retirement.CleanupCompleted > current.Retirement.CleanupCompleted {
			if err := r.scrubRetirementDebt(ctx, q); err != nil {
				return agentcfg.RetirementStatus{}, err
			}
			return r.ackRetirementEvent(ctx, id, agentID, q)
		}
	}
	if !errors.Is(err, state.ErrConditionFailed) {
		return agentcfg.RetirementStatus{}, fmt.Errorf("%w: save retirement progress: %w", agentcfg.ErrStateUnavailable, err)
	}
	return agentcfg.RetirementStatus{}, fmt.Errorf("%w: cleanup progress moved", agentcfg.ErrRetirementConflict)
}

func (r *registry) scrubRetirementDebt(ctx context.Context, q identity.Quadruple) error {
	for {
		current, lifecycleEvent, found, err := r.loadActiveRecord(ctx, q)
		if err != nil {
			return err
		}
		if !found || current.Retirement == nil || !current.Retirement.ManifestFrozen {
			return fmt.Errorf("%w: scrub requires a frozen retirement lifecycle", agentcfg.ErrRetirementConflict)
		}
		if current.Retirement.ScrubCompleted == current.Retirement.CleanupCompleted {
			return nil
		}
		if current.Retirement.ScrubCompleted+1 != current.Retirement.CleanupCompleted {
			return fmt.Errorf("%w: retirement scrub debt is not a single durable step", agentcfg.ErrRetirementConflict)
		}
		ordinal := current.Retirement.ScrubCompleted
		operationHash := agentcfg.RetirementOperationHash(current.Retirement.OperationID)
		item, compact, itemEvent, present, err := r.loadRetirementManifestRecord(ctx, q, operationHash, ordinal)
		if err != nil {
			return err
		}
		if !present {
			return fmt.Errorf("%w: cleanup manifest item disappeared before scrub", agentcfg.ErrRetirementConflict)
		}
		var scrub retirementManifestScrub
		if compact != nil {
			scrub = *compact
		} else {
			if item == nil || item.Digest != current.Retirement.CleanupDigest {
				return fmt.Errorf("%w: cleanup item changed before scrub", agentcfg.ErrRetirementConflict)
			}
			scrub = retirementManifestScrub{Schema: retirementManifestSchema, OperationHash: operationHash, Ordinal: ordinal, PriorDigest: item.PriorDigest, Digest: item.Digest, Scrubbed: true}
			encoded, err := json.Marshal(scrub)
			if err != nil {
				return fmt.Errorf("marshal scrubbed retirement manifest item: %w", err)
			}
			err = r.state.SaveIf(ctx, []state.SlotExpectation{
				{Identity: q, Kind: retirementManifestKind(operationHash, ordinal), ExpectedEventID: itemEvent},
				{Identity: q, Kind: kindActive, ExpectedEventID: lifecycleEvent},
			}, state.StateRecord{ID: state.NewEventID(), Identity: q, Kind: retirementManifestKind(operationHash, ordinal), Bytes: encoded, UpdatedAt: r.clock().UTC()})
			if err != nil {
				landedItem, landedScrub, _, landed, loadErr := r.loadRetirementManifestRecord(ctx, q, operationHash, ordinal)
				if loadErr == nil && landed && landedItem == nil && landedScrub != nil && *landedScrub == scrub {
					continue
				}
				if errors.Is(err, state.ErrConditionFailed) {
					return fmt.Errorf("%w: retirement manifest moved before scrub", agentcfg.ErrRetirementConflict)
				}
				return fmt.Errorf("%w: compact retirement manifest item: %w", agentcfg.ErrStateUnavailable, err)
			}
			continue
		}
		if scrub.Digest != current.Retirement.CleanupDigest {
			return fmt.Errorf("%w: scrubbed manifest digest differs from cleanup authority", agentcfg.ErrRetirementConflict)
		}
		next := current
		next.Retirement = cloneRetirement(current.Retirement)
		next.Retirement.Generation++
		next.Retirement.ScrubCompleted++
		next.UpdatedAt = r.clock().UTC()
		encoded, err := json.Marshal(next)
		if err != nil {
			return fmt.Errorf("marshal retirement scrub progress: %w", err)
		}
		err = r.state.SaveIf(ctx, []state.SlotExpectation{{Identity: q, Kind: kindActive, ExpectedEventID: lifecycleEvent}}, state.StateRecord{ID: state.NewEventID(), Identity: q, Kind: kindActive, Bytes: encoded, UpdatedAt: next.UpdatedAt})
		if err == nil {
			continue
		}
		landed, _, _, loadErr := r.loadActiveRecord(ctx, q)
		if loadErr == nil && landed.Retirement != nil && landed.Retirement.OperationID == current.Retirement.OperationID && landed.Retirement.ScrubCompleted > current.Retirement.ScrubCompleted {
			continue
		}
		if errors.Is(err, state.ErrConditionFailed) {
			return fmt.Errorf("%w: retirement scrub progress moved", agentcfg.ErrRetirementConflict)
		}
		return fmt.Errorf("%w: save retirement scrub progress: %w", agentcfg.ErrStateUnavailable, err)
	}
}

func (r *registry) completeSessionRetirementStep(ctx context.Context, tenantID, agentID, class, resource string) error {
	target, err := decodeRetirementSessionTarget(resource)
	if err != nil {
		return err
	}
	if target.TenantID != tenantID || target.AgentID != agentID || target.RunID != "" {
		return fmt.Errorf("%w: session cleanup target crossed retirement scope", agentcfg.ErrRetirementConflict)
	}
	switch class {
	case retirementCleanupSessionPersonal:
		if target.CanonicalName == "" {
			return fmt.Errorf("%w: personal cleanup target lacks canonical name", agentcfg.ErrRetirementConflict)
		}
		personal, err := sessionoverlay.NewDurableStore(r.state, r.clock)
		if err != nil {
			return err
		}
		return personal.RetirePersonalCandidate(ctx, identity.Quadruple{Identity: identity.Identity{TenantID: target.TenantID, UserID: target.UserID, SessionID: target.SessionID}, RunID: target.RunID}, target.Kind, tenantID, agentID, target.CanonicalName)
	case retirementCleanupLegacyOverlay:
		if target.CanonicalName != "" || target.Kind != sessionoverlay.LegacyOverlayKind(agentID) {
			return fmt.Errorf("%w: legacy cleanup target is not exact", agentcfg.ErrRetirementConflict)
		}
		// Schema-1 overlays remain intact for compatibility. The terminal
		// lifecycle is their retirement fence; no physical or unconditional
		// Delete is necessary or permitted.
		return nil
	default:
		return fmt.Errorf("%w: unknown session cleanup class %q", agentcfg.ErrRetirementConflict, class)
	}
}

func (r *registry) inspectSessionRetirementStep(ctx context.Context, tenantID, agentID, class, resource string) error {
	if class != retirementCleanupSessionPersonal {
		return nil
	}
	target, err := decodeRetirementSessionTarget(resource)
	if err != nil {
		return err
	}
	if target.TenantID != tenantID || target.AgentID != agentID || target.RunID != "" || target.CanonicalName == "" {
		return fmt.Errorf("%w: personal cleanup target crossed retirement scope or lacks canonical name", agentcfg.ErrRetirementConflict)
	}
	personal, err := sessionoverlay.NewDurableStore(r.state, r.clock)
	if err != nil {
		return err
	}
	return personal.InspectRetirementPersonalCandidate(ctx, identity.Quadruple{Identity: identity.Identity{TenantID: target.TenantID, UserID: target.UserID, SessionID: target.SessionID}, RunID: target.RunID}, target.Kind, tenantID, agentID, target.CanonicalName)
}

func (r *registry) ensureNotRetired(ctx context.Context, id identity.Quadruple, agentID string) error {
	rec, _, _, err := r.loadActiveRecord(ctx, syntheticQuad(id.TenantID, agentID))
	if err != nil {
		return err
	}
	if rec.Retirement != nil {
		return fmt.Errorf("%w: operation=%q", agentcfg.ErrAgentRetired, rec.Retirement.OperationID)
	}
	return nil
}

func (r *registry) loadActiveRecord(ctx context.Context, q identity.Quadruple) (activeRecord, state.EventID, bool, error) {
	rec, err := r.state.Load(ctx, q, kindActive)
	if errors.Is(err, state.ErrNotFound) {
		return activeRecord{}, "", false, nil
	}
	if err != nil {
		return activeRecord{}, "", false, fmt.Errorf("%w: load active pointer: %w", agentcfg.ErrStateUnavailable, err)
	}
	if agentcfg.ClassifyLifecycleRecord(rec.Bytes) == agentcfg.LifecycleRecordInvalid {
		return activeRecord{}, "", false, fmt.Errorf("%w: %w", agentcfg.ErrStateUnavailable, errLifecycleMalformed)
	}
	var out activeRecord
	if len(rec.Bytes) > 0 {
		if err := json.Unmarshal(rec.Bytes, &out); err != nil {
			return activeRecord{}, "", false, fmt.Errorf("%w: unmarshal active pointer: %w", agentcfg.ErrStateUnavailable, err)
		}
		if out.Schema != 0 && out.Schema != recordSchema {
			return activeRecord{}, "", false, fmt.Errorf("%w: active pointer schema=%d, runtime supports %d", agentcfg.ErrStateUnavailable, out.Schema, recordSchema)
		}
	}
	return out, rec.ID, true, nil
}

func cleanupManifest(prior agentcfg.Revision) []agentcfg.CleanupStep {
	var out []agentcfg.CleanupStep
	if prior.Payload.Connections != nil {
		for _, c := range prior.Payload.Connections.Servers {
			out = append(out, agentcfg.CleanupStep{Class: "mcp_connection", Resource: c.Name})
		}
	}
	if prior.Payload.OAuthProviders != nil {
		for _, p := range prior.Payload.OAuthProviders.Providers {
			out = append(out, agentcfg.CleanupStep{Class: "oauth_provider", Resource: p.Name})
		}
	}
	return out
}

func (r *registry) retirementStatus(ctx context.Context, q identity.Quadruple, in *retirementRecord) (agentcfg.RetirementStatus, error) {
	out := agentcfg.RetirementStatus{OperationID: in.OperationID, RetiredAt: in.RetiredAt, PriorRevisionID: in.PriorRevisionID, PriorContentHash: in.PriorContentHash, Generation: in.Generation, Completed: in.Completed}
	if !in.ManifestFrozen || in.ManifestCount == 0 || in.Completed {
		return out, nil
	}
	if in.ScrubCompleted != in.CleanupCompleted {
		return agentcfg.RetirementStatus{}, fmt.Errorf("%w: retirement cleanup awaits manifest scrub", agentcfg.ErrStateUnavailable)
	}
	if in.CleanupCompleted == in.ManifestCount {
		return out, nil
	}
	item, err := r.verifiedRetirementCleanupItem(ctx, q, in)
	if err != nil {
		return agentcfg.RetirementStatus{}, err
	}
	if err := r.inspectSessionRetirementStep(ctx, q.TenantID, q.SessionID, item.Class, item.Resource); err != nil {
		return agentcfg.RetirementStatus{}, err
	}
	out.Cleanup = []agentcfg.CleanupStep{{Class: item.Class, Resource: item.Resource}}
	return out, nil
}

func (r *registry) verifiedRetirementCleanupItem(ctx context.Context, q identity.Quadruple, retirement *retirementRecord) (retirementManifestItem, error) {
	ordinal := retirement.CleanupCompleted
	item, err := r.loadRetirementManifestItem(ctx, q, retirement, ordinal)
	if err != nil {
		return retirementManifestItem{}, err
	}
	if item.PriorDigest != retirement.CleanupDigest {
		return retirementManifestItem{}, fmt.Errorf("%w: cleanup item breaks lifecycle digest linkage", agentcfg.ErrRetirementConflict)
	}
	if ordinal+1 == retirement.ManifestCount {
		if item.Digest != retirement.ManifestDigest {
			return retirementManifestItem{}, fmt.Errorf("%w: final cleanup item differs from frozen manifest digest", agentcfg.ErrRetirementConflict)
		}
		return item, nil
	}
	next, err := r.loadRetirementManifestItem(ctx, q, retirement, ordinal+1)
	if err != nil {
		return retirementManifestItem{}, err
	}
	if next.PriorDigest != item.Digest {
		return retirementManifestItem{}, fmt.Errorf("%w: cleanup item breaks successor digest linkage", agentcfg.ErrRetirementConflict)
	}
	return item, nil
}

func (r *registry) loadRetirementManifestItem(ctx context.Context, q identity.Quadruple, retirement *retirementRecord, ordinal uint64) (retirementManifestItem, error) {
	if ordinal >= retirement.ManifestCount {
		return retirementManifestItem{}, fmt.Errorf("%w: manifest ordinal exceeds frozen count", agentcfg.ErrRetirementConflict)
	}
	operationHash := agentcfg.RetirementOperationHash(retirement.OperationID)
	item, scrub, _, found, err := r.loadRetirementManifestRecord(ctx, q, operationHash, ordinal)
	if err != nil {
		return retirementManifestItem{}, err
	}
	if !found || scrub != nil || item == nil {
		return retirementManifestItem{}, fmt.Errorf("%w: retirement manifest item is absent or scrubbed", agentcfg.ErrRetirementConflict)
	}
	return *item, nil
}

func (r *registry) loadRetirementManifestRecord(ctx context.Context, q identity.Quadruple, operationHash string, ordinal uint64) (*retirementManifestItem, *retirementManifestScrub, state.EventID, bool, error) {
	record, err := r.state.Load(ctx, q, retirementManifestKind(operationHash, ordinal))
	if errors.Is(err, state.ErrNotFound) {
		return nil, nil, "", false, nil
	}
	if err != nil {
		return nil, nil, "", false, fmt.Errorf("%w: load retirement manifest item: %w", agentcfg.ErrStateUnavailable, err)
	}
	if len(record.Bytes) == 0 || len(record.Bytes) > maxRetirementManifestItemBytes {
		return nil, nil, "", false, fmt.Errorf("%w: retirement manifest item exceeds bound", agentcfg.ErrStateUnavailable)
	}
	if err := agentcfg.ValidateUniqueJSONFields(record.Bytes); err != nil {
		return nil, nil, "", false, fmt.Errorf("%w: manifest item duplicate field: %w", agentcfg.ErrStateUnavailable, err)
	}
	var header struct {
		Scrubbed bool `json:"scrubbed"`
	}
	if err := json.Unmarshal(record.Bytes, &header); err != nil {
		return nil, nil, "", false, fmt.Errorf("%w: decode retirement manifest header: %w", agentcfg.ErrStateUnavailable, err)
	}
	if header.Scrubbed {
		decoder := json.NewDecoder(bytes.NewReader(record.Bytes))
		decoder.DisallowUnknownFields()
		var scrub retirementManifestScrub
		if err := decoder.Decode(&scrub); err != nil {
			return nil, nil, "", false, fmt.Errorf("%w: decode scrubbed retirement manifest item: %w", agentcfg.ErrStateUnavailable, err)
		}
		if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
			return nil, nil, "", false, fmt.Errorf("%w: trailing scrubbed retirement manifest item", agentcfg.ErrStateUnavailable)
		}
		if scrub.Schema != retirementManifestSchema || scrub.OperationHash != operationHash || scrub.Ordinal != ordinal || !scrub.Scrubbed || !validRetirementDigest(scrub.PriorDigest) || !validRetirementDigest(scrub.Digest) {
			return nil, nil, "", false, fmt.Errorf("%w: invalid scrubbed retirement manifest item", agentcfg.ErrStateUnavailable)
		}
		return nil, &scrub, record.ID, true, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(record.Bytes))
	decoder.DisallowUnknownFields()
	var item retirementManifestItem
	if err := decoder.Decode(&item); err != nil {
		return nil, nil, "", false, fmt.Errorf("%w: decode retirement manifest item: %w", agentcfg.ErrStateUnavailable, err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return nil, nil, "", false, fmt.Errorf("%w: trailing retirement manifest item", agentcfg.ErrStateUnavailable)
	}
	if item.Schema != retirementManifestSchema || item.OperationHash != operationHash || item.Ordinal != ordinal || !validRetirementCleanupClass(item.Class) || item.Resource == "" || !validRetirementDigest(item.PriorDigest) || !validRetirementDigest(item.Digest) || !validRetirementDiscovery(item.Source) {
		return nil, nil, "", false, fmt.Errorf("%w: invalid retirement manifest item", agentcfg.ErrStateUnavailable)
	}
	if !retirementManifestDiscoveryFieldsPresent(record.Bytes, item.Successor.State) {
		return nil, nil, "", false, fmt.Errorf("%w: retirement manifest discovery authority is incomplete", agentcfg.ErrStateUnavailable)
	}
	digest, err := advanceRetirementManifestDigest(item.PriorDigest, item)
	if err != nil {
		return nil, nil, "", false, err
	}
	if item.Digest != digest || item.Successor.ManifestDigest != item.Digest || item.Successor.ManifestCount != item.Ordinal+1 {
		return nil, nil, "", false, fmt.Errorf("%w: invalid retirement manifest digest or successor", agentcfg.ErrStateUnavailable)
	}
	if (item.Successor.State == "discovering" && (item.Successor.Discovery == nil || !validRetirementDiscovery(*item.Successor.Discovery))) || (item.Successor.State == "frozen" && item.Successor.Discovery != nil) || (item.Successor.State != "discovering" && item.Successor.State != "frozen") {
		return nil, nil, "", false, fmt.Errorf("%w: invalid retirement manifest successor state", agentcfg.ErrStateUnavailable)
	}
	return &item, nil, record.ID, true, nil
}

func retirementManifestDiscoveryFieldsPresent(data []byte, successorState string) bool {
	type requiredDiscovery struct {
		Stage        *string `json:"stage"`
		Continuation *string `json:"continuation"`
		ConfigIndex  *uint64 `json:"config_index"`
	}
	var required struct {
		Source    *requiredDiscovery `json:"source"`
		Successor *struct {
			State          *string            `json:"state"`
			Discovery      *requiredDiscovery `json:"discovery"`
			ManifestCount  *uint64            `json:"manifest_count"`
			ManifestDigest *string            `json:"manifest_digest"`
		} `json:"successor"`
	}
	if err := json.Unmarshal(data, &required); err != nil || required.Source == nil || required.Source.Stage == nil || required.Source.Continuation == nil || required.Source.ConfigIndex == nil || required.Successor == nil || required.Successor.State == nil || required.Successor.ManifestCount == nil || required.Successor.ManifestDigest == nil {
		return false
	}
	if successorState == "discovering" {
		return required.Successor.Discovery != nil && required.Successor.Discovery.Stage != nil && required.Successor.Discovery.Continuation != nil && required.Successor.Discovery.ConfigIndex != nil
	}
	return successorState == "frozen" && required.Successor.Discovery == nil
}

func validRetirementDigest(value string) bool {
	if len(value) != sha256.Size*2 || value != strings.ToLower(value) {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func validRetirementCleanupClass(class string) bool {
	switch class {
	case "mcp_connection", "oauth_provider", agentcfg.RetirementCleanupClassSignedOAuthMCPPair, retirementCleanupSessionPersonal, retirementCleanupLegacyOverlay:
		return true
	default:
		return false
	}
}

// --- internal helpers ---

// guardSignedOAuthMCPFenceWriter is the cross-runtime half of the signed
// activation fence. Every Registry pointer-mutating door reaches this driver,
// so a generic writer cannot race a pending first-install candidate into
// authority. Only the internally-marked exact operation may write while the
// fence is pending.
func (r *registry) guardSignedOAuthMCPFenceWriter(ctx context.Context, id identity.Quadruple, agentID string, scope agentcfg.ConfigScope) error {
	if scope != agentcfg.ConfigScopeAgent {
		return nil
	}
	fences, err := r.signedOAuthMCPFences(ctx, id.TenantID, agentID)
	if err != nil {
		return err
	}
	operation := agentcfg.SignedOAuthMCPFenceOperation(ctx)
	for _, fence := range fences {
		if fence.Phase != agentcfg.SignedOAuthMCPFencePending {
			continue
		}
		if operation == fence.OperationKind {
			return nil
		}
		if operation == "" {
			return fmt.Errorf("%w: foreign writer cannot mutate agent %q while operation %q is pending", agentcfg.ErrSignedCapabilityPending, agentID, fence.OperationKind)
		}
	}
	for _, fence := range fences {
		if fence.Phase == agentcfg.SignedOAuthMCPFencePending {
			return fmt.Errorf("%w: operation %q does not own a pending fence for agent %q", agentcfg.ErrSignedCapabilityPending, operation, agentID)
		}
	}
	return nil
}

// applySignedOAuthMCPFence hides exactly the candidate named by a pending
// receipt. A physical active pointer is not authority until the receipt and
// fence both commit; callers observe the prior revision (or no active state).
func (r *registry) applySignedOAuthMCPFence(ctx context.Context, id identity.Quadruple, agentID string, keys scopeKeys, active agentcfg.Revision, set bool) (agentcfg.Revision, bool, error) {
	if !set {
		return active, set, nil
	}
	fences, err := r.signedOAuthMCPFences(ctx, id.TenantID, agentID)
	if err != nil {
		return agentcfg.Revision{}, false, err
	}
	var match *agentcfg.SignedOAuthMCPActivationFence
	for i := range fences {
		fence := &fences[i]
		if fence.Phase != agentcfg.SignedOAuthMCPFencePending || active.ContentHash != fence.CandidateContentHash ||
			(fence.CandidateRevisionID != "" && active.RevisionID != fence.CandidateRevisionID) {
			continue
		}
		if match != nil && match.PriorRevisionID != fence.PriorRevisionID {
			return agentcfg.Revision{}, false, fmt.Errorf("%w: conflicting signed capability activation fences", agentcfg.ErrStateUnavailable)
		}
		match = fence
	}
	if match == nil {
		return active, set, nil
	}
	if match.PriorRevisionID == "" {
		return agentcfg.Revision{}, false, nil
	}
	prior, err := r.loadRevision(ctx, keys.quad, keys.revPfx, match.PriorRevisionID)
	if err != nil {
		return agentcfg.Revision{}, false, fmt.Errorf("%w: load activation-fence prior revision: %w", agentcfg.ErrStateUnavailable, err)
	}
	return prior.toRevision(), true, nil
}

func (r *registry) signedOAuthMCPFences(ctx context.Context, tenant, agentID string) ([]agentcfg.SignedOAuthMCPActivationFence, error) {
	quad := identity.Quadruple{Identity: identity.Identity{TenantID: tenant, UserID: agentCfgUser, SessionID: agentID}}
	records, err := r.state.ListKindForIdentity(ctx, quad, agentcfg.SignedOAuthMCPActivationFenceKind())
	if err != nil {
		return nil, fmt.Errorf("%w: list signed capability activation fences: %w", agentcfg.ErrStateUnavailable, err)
	}
	fences := make([]agentcfg.SignedOAuthMCPActivationFence, 0, len(records))
	for _, record := range records {
		var fence agentcfg.SignedOAuthMCPActivationFence
		if err := json.Unmarshal(record.Bytes, &fence); err != nil || fence.TenantID != tenant || fence.AgentID != agentID || fence.OperationKind == "" || fence.CandidateContentHash == "" ||
			(fence.Phase != agentcfg.SignedOAuthMCPFencePending && fence.Phase != agentcfg.SignedOAuthMCPFenceCommitted && fence.Phase != agentcfg.SignedOAuthMCPFenceAborted) {
			return nil, fmt.Errorf("%w: corrupt signed capability activation fence", agentcfg.ErrStateUnavailable)
		}
		fence.EventID = record.ID
		fence.StateKind = record.Kind
		fences = append(fences, fence)
	}
	return fences, nil
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
	if ar.Retirement != nil {
		if strings.TrimSpace(ar.Retirement.OperationID) == "" {
			return agentcfg.Revision{}, false, fmt.Errorf("%w: malformed retirement tombstone", agentcfg.ErrStateUnavailable)
		}
		return agentcfg.Revision{}, false, fmt.Errorf("%w: operation=%q", agentcfg.ErrAgentRetired, ar.Retirement.OperationID)
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
