package sessions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/state"
)

// catalogKind is the StateStore Kind for the per-(tenant, user)
// session-id catalog (D-171). The StateStore surface is point-read
// only — `(Quadruple, Kind) → Bytes`, with no List operation (the
// `idIndex` doc on Registry says so). To make `sessions.list` survive a
// runtime restart, the Registry persists, per (tenant, user), the set
// of SessionIDs it has ever Opened. On boot the in-memory `idIndex`
// starts empty; the first read for a (tenant, user) lazily hydrates it
// from this catalog so a fresh process re-discovers the sessions a
// prior process created.
//
// This is the typed-wrapper-owns-enumeration seam called out on the
// Registry struct: rather than widen the StateStore interface (3
// drivers + a conformance suite), the Registry keeps its own catalog
// index inside the same StateStore, keyed by a sentinel session slot.
const catalogKind = "session.catalog"

// catalogSession is the sentinel value occupying the `session` slot of
// the catalog record's identity quadruple. The StateStore key is
// `(tenant, user, session, run, kind)`; the catalog is one record per
// (tenant, user), so it needs a fixed, reserved session id. The value
// is deliberately not a legal user-chosen session id shape (the angle
// brackets never appear in a JWT claim or an X-Harbor-Session header in
// practice) so it can never collide with a real conversation.
const catalogSession = "<session-catalog>"

// sessionCatalog is the persisted per-(tenant, user) set of SessionIDs.
// Stored as a slice (JSON-friendly, stable order on read) and
// deduplicated on write.
type sessionCatalog struct {
	SessionIDs []string `json:"session_ids"`
}

// catalogQuad builds the StateStore key for the (tenant, user) catalog
// record. The session slot is the reserved sentinel.
func catalogQuad(tenant, user string) identity.Quadruple {
	return identity.Quadruple{
		Identity: identity.Identity{
			TenantID:  tenant,
			UserID:    user,
			SessionID: catalogSession,
		},
	}
}

// loadCatalog reads the (tenant, user) catalog. A missing record is the
// zero catalog (no sessions yet) — NOT an error.
func (r *Registry) loadCatalog(ctx context.Context, tenant, user string) (sessionCatalog, error) {
	rec, err := r.store.Load(ctx, catalogQuad(tenant, user), catalogKind)
	if err != nil {
		if errors.Is(err, state.ErrNotFound) {
			return sessionCatalog{}, nil
		}
		return sessionCatalog{}, fmt.Errorf("sessions: load catalog: %w", err)
	}
	var cat sessionCatalog
	if uerr := json.Unmarshal(rec.Bytes, &cat); uerr != nil {
		return sessionCatalog{}, fmt.Errorf("sessions: unmarshal catalog: %w", uerr)
	}
	return cat, nil
}

// addToCatalog records sessionID in the (tenant, user) catalog. A no-op
// when the id is already present (idempotent). Caller need not hold
// r.mu — the StateStore is its own concurrency boundary, and the
// read-modify-write is benign under concurrency because the only
// mutation is set-union (adding an id twice is a no-op).
func (r *Registry) addToCatalog(ctx context.Context, tenant, user, sessionID string) error {
	cat, err := r.loadCatalog(ctx, tenant, user)
	if err != nil {
		return err
	}
	for _, id := range cat.SessionIDs {
		if id == sessionID {
			return nil // already catalogued
		}
	}
	cat.SessionIDs = append(cat.SessionIDs, sessionID)
	bytes, merr := json.Marshal(cat)
	if merr != nil {
		return fmt.Errorf("sessions: marshal catalog: %w", merr)
	}
	rec := state.StateRecord{
		ID:        state.NewEventID(),
		Identity:  catalogQuad(tenant, user),
		Kind:      catalogKind,
		Bytes:     bytes,
		UpdatedAt: r.clock.Now(),
	}
	if serr := r.store.Save(ctx, rec); serr != nil {
		return fmt.Errorf("sessions: save catalog: %w", serr)
	}
	return nil
}

// hydrateFromCatalog repopulates the in-memory idIndex / openSessions
// for a (tenant, user) from the persisted catalog. Called lazily on the
// read path so a fresh process re-discovers a prior process's sessions.
// Each catalogued SessionID is Loaded from the StateStore: a still-open
// record re-enters openSessions (so GC + posture counters see it); a
// closed record enters idIndex only (so ListSnapshots surfaces the
// closed row). A catalogued id whose record vanished out-of-band is
// skipped. Idempotent: re-hydrating an already-known id is a no-op.
func (r *Registry) hydrateFromCatalog(ctx context.Context, tenant, user string) error {
	cat, err := r.loadCatalog(ctx, tenant, user)
	if err != nil {
		return err
	}
	for _, sid := range cat.SessionIDs {
		ident := identity.Identity{TenantID: tenant, UserID: user, SessionID: sid}
		stored, lerr := r.loadSession(ctx, ident)
		if lerr != nil {
			// Record gone (e.g. a future Delete path) — drop silently;
			// the catalog is a best-effort discovery index, not the
			// source of truth (the session record is).
			if errors.Is(lerr, ErrSessionNotFound) {
				continue
			}
			return fmt.Errorf("sessions: hydrate load %q: %w", sid, lerr)
		}
		r.mu.Lock()
		if _, known := r.idIndex[sid]; !known {
			r.idIndex[sid] = ident
		}
		if !stored.Closed {
			if _, open := r.openSessions[sid]; !open {
				r.openSessions[sid] = identity.Quadruple{Identity: ident}
			}
		}
		r.mu.Unlock()
	}
	return nil
}
