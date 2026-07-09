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
// session-id catalog. The StateStore surface is point-read
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

// mutateCatalog is the ONE serialized read-modify-write path for the
// per-(tenant, user) discovery catalog. It loads the catalog, applies fn
// (which mutates the passed *sessionCatalog and reports whether anything
// changed), and — only when fn reports a change — persists the result.
//
// It MUST be called with r.mu held. The catalog is a single shared record
// per (tenant, user); a load→modify→save that does NOT hold r.mu across all
// three steps races a concurrent catalog writer as a classic lost update.
// The confirmed shape: an erasure's removeFromCatalog loads {A,B}; an
// Open(C) then completes its own add ({A,B,C}); the eraser saves its stale
// difference ({A}); session C's record survives but its catalog entry is
// gone — invisible to sessions.list and never re-discovered by a fresh
// process after a restart (the StateStore has no List). Holding the SAME
// r.mu the session-record writers (mutateSession) hold serializes the Open
// add against the erasure remove and closes the window.
//
// Lock order is unchanged: the eraser holds its striped per-session lock
// OUTSIDE r.mu (striped → r.mu, the order clearErased already used);
// mutateCatalog acquires no other lock and performs no bus I/O, so no cycle
// exists.
func (r *Registry) mutateCatalog(ctx context.Context, tenant, user string, fn func(cat *sessionCatalog) bool) error {
	cat, err := r.loadCatalog(ctx, tenant, user)
	if err != nil {
		return err
	}
	if !fn(&cat) {
		return nil // no change — skip the save
	}
	return r.saveCatalog(ctx, tenant, user, cat)
}

// saveCatalog persists the (tenant, user) catalog record. Shared by the
// mutateCatalog RMW path.
func (r *Registry) saveCatalog(ctx context.Context, tenant, user string, cat sessionCatalog) error {
	bytes, err := json.Marshal(cat)
	if err != nil {
		return fmt.Errorf("sessions: marshal catalog: %w", err)
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

// addToCatalog records sessionID in the (tenant, user) catalog. A no-op
// when the id is already present (idempotent). MUST be called with r.mu
// held: it routes through mutateCatalog, the single serialized catalog RMW
// path (see there for why the lock is load-bearing). Open holds r.mu across
// its whole critical section; that is where this call runs.
func (r *Registry) addToCatalog(ctx context.Context, tenant, user, sessionID string) error {
	return r.mutateCatalog(ctx, tenant, user, func(cat *sessionCatalog) bool {
		for _, id := range cat.SessionIDs {
			if id == sessionID {
				return false // already catalogued
			}
		}
		cat.SessionIDs = append(cat.SessionIDs, sessionID)
		return true
	})
}

// removeFromCatalog drops sessionID from the (tenant, user) catalog. A
// no-op when the id is absent (idempotent). The catalog record is keyed
// by (tenant, user) — NOT by the session triple — so a session-scoped
// StateStore.DeleteScope never reaches it; the erasure cascade calls
// this explicitly so an erased session leaves no dangling catalog entry
// to re-hydrate after a restart. MUST be called with r.mu held: it routes
// through mutateCatalog (folded into clearErased's r.mu critical section)
// so the erasure remove serializes with Open's add.
func (r *Registry) removeFromCatalog(ctx context.Context, tenant, user, sessionID string) error {
	return r.mutateCatalog(ctx, tenant, user, func(cat *sessionCatalog) bool {
		kept := make([]string, 0, len(cat.SessionIDs))
		found := false
		for _, id := range cat.SessionIDs {
			if id == sessionID {
				found = true
				continue
			}
			kept = append(kept, id)
		}
		if !found {
			return false // already absent
		}
		cat.SessionIDs = kept
		return true
	})
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
