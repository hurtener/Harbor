// Package inmem is Harbor's V1 in-memory StateStore driver. It is
// the test reference for the conformance suite — every later driver
// (SQLite, Postgres, durable-log) inherits
// the same suite verbatim.
//
// Internal model:
//
//   - A primary map keyed on (Quadruple, Kind) holds the active
//     record per slot. A secondary map keyed on EventID resolves
//     idempotency lookups and `LoadByEventID`.
//   - A single `sync.RWMutex` guards both maps. The driver does no
//     I/O so contention is bounded by Go's map throughput; a
//     finer-grained lock structure would be premature.
//   - `Bytes` is deep-copied on Save and on Load to defend against
//     callers mutating the slice they passed in (or the slice they
//     received). Future SQL drivers naturally avoid this issue
//     (rows are independent of the caller's slice).
//   - `Close(ctx)` flips an atomic flag; subsequent calls return
//     `ErrStoreClosed`. There are no driver-owned goroutines to
//     join, so Close is fast.
package inmem

import (
	"bytes"
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/state"
)

// New constructs a StateStore directly. Exposed for tests that want
// to skip the registry; production callers go through `state.Open`.
func New(_ config.StateConfig) (state.StateStore, error) {
	return &driver{
		records:  map[indexKey]state.StateRecord{},
		eventIdx: map[state.EventID]indexKey{},
	}, nil
}

func init() {
	state.Register("inmem", New)
}

// indexKey is the composite primary key. Struct-typed (rather than
// string-concatenated) so tenant IDs containing delimiters can't
// collide.
type indexKey struct {
	Tenant  string
	User    string
	Session string
	Run     string
	Kind    string
}

func keyFor(q identity.Quadruple, kind string) indexKey {
	return indexKey{
		Tenant:  q.TenantID,
		User:    q.UserID,
		Session: q.SessionID,
		Run:     q.RunID,
		Kind:    kind,
	}
}

type driver struct {
	mu       sync.RWMutex
	records  map[indexKey]state.StateRecord
	eventIdx map[state.EventID]indexKey
	closed   atomic.Bool
}

// Save implements state.StateStore.
//
// Idempotency:
//
//  1. If the EventID was seen before AND the previous record's
//     (Identity, Kind) AND Bytes match the new request: no-op.
//  2. If the EventID was seen before AND anything else differs:
//     ErrIdempotencyConflict.
//  3. Else: insert/update the record at (Identity, Kind); update
//     the EventID secondary index. If a previous record at
//     (Identity, Kind) existed under a DIFFERENT EventID, the old
//     EventID is removed from the secondary index.
func (d *driver) Save(_ context.Context, r state.StateRecord) error {
	if d.closed.Load() {
		return state.ErrStoreClosed
	}
	if err := state.ValidateRecord(r); err != nil {
		return err
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	key := keyFor(r.Identity, r.Kind)

	// Idempotency check: same EventID seen before?
	if prevKey, seen := d.eventIdx[r.ID]; seen {
		prev := d.records[prevKey]
		if prevKey != key {
			return fmt.Errorf("%w: EventID %q already routes to a different (Quadruple, Kind)",
				state.ErrIdempotencyConflict, r.ID)
		}
		if !bytes.Equal(prev.Bytes, r.Bytes) {
			return fmt.Errorf("%w: EventID %q already saved with different Bytes",
				state.ErrIdempotencyConflict, r.ID)
		}
		if prev.Version != r.Version {
			return fmt.Errorf("%w: EventID %q already saved with different Version",
				state.ErrIdempotencyConflict, r.ID)
		}
		// Idempotent no-op.
		return nil
	}

	// New EventID. If a record already exists at this slot under a
	// different EventID, evict the old EventID from the secondary
	// index (the slot now belongs to the new EventID).
	if existing, ok := d.records[key]; ok {
		if existing.ID != r.ID {
			delete(d.eventIdx, existing.ID)
		}
	}

	stored := state.StoredRecord(r)
	stored.Bytes = cloneBytes(r.Bytes)
	if stored.UpdatedAt.IsZero() {
		stored.UpdatedAt = time.Now()
	}
	d.records[key] = stored
	d.eventIdx[r.ID] = key
	return nil
}

// SaveIf implements StateStore's multi-slot atomic compare-and-save. The
// reference driver's one mutex guards both the predicates and the write.
func (d *driver) SaveIf(ctx context.Context, expectations []state.SlotExpectation, next state.StateRecord) error {
	if d.closed.Load() {
		return state.ErrStoreClosed
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := state.ValidateSaveIf(expectations, next); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	for _, expectation := range expectations {
		rec, ok := d.records[keyFor(expectation.Identity, expectation.Kind)]
		if expectation.ExpectedEventID == "" {
			if ok {
				return fmt.Errorf("%w: slot is present", state.ErrConditionFailed)
			}
			continue
		}
		if !ok || rec.ID != expectation.ExpectedEventID {
			return fmt.Errorf("%w: expected event_id %q", state.ErrConditionFailed, expectation.ExpectedEventID)
		}
	}
	return d.saveLocked(next)
}

// SaveBatchIf atomically verifies all predicates and writes under one mutex.
func (d *driver) SaveBatchIf(ctx context.Context, expectations []state.SlotExpectation, writes []state.StateRecord) error {
	if d.closed.Load() {
		return state.ErrStoreClosed
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := state.ValidateSaveBatchIf(expectations, writes); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	for _, expectation := range expectations {
		rec, ok := d.records[keyFor(expectation.Identity, expectation.Kind)]
		if expectation.ExpectedEventID == "" {
			if ok {
				return fmt.Errorf("%w: slot is present", state.ErrConditionFailed)
			}
			continue
		}
		if !ok || rec.ID != expectation.ExpectedEventID {
			return fmt.Errorf("%w: expected event_id %q", state.ErrConditionFailed, expectation.ExpectedEventID)
		}
	}
	// Preflight all globally unique EventIDs before the first mutation so an
	// idempotency conflict cannot partially apply the batch.
	for _, write := range writes {
		if prevKey, seen := d.eventIdx[write.ID]; seen {
			prev := d.records[prevKey]
			if prevKey != keyFor(write.Identity, write.Kind) || !bytes.Equal(prev.Bytes, write.Bytes) || prev.Version != write.Version {
				return fmt.Errorf("%w: EventID %q conflicts", state.ErrIdempotencyConflict, write.ID)
			}
		}
	}
	for _, write := range writes {
		if err := d.saveLocked(write); err != nil {
			return err // unreachable after the complete preflight above
		}
	}
	return nil
}

// DeleteIf implements StateStore's exact-generation conditional delete. The
// reference driver's mutex makes the generation check and removal one atomic
// operation across both the primary slot and EventID index.
func (d *driver) DeleteIf(ctx context.Context, expectation state.SlotExpectation) (bool, error) {
	if d.closed.Load() {
		return false, state.ErrStoreClosed
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if err := state.ValidateDeleteIf(expectation); err != nil {
		return false, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return false, err
	}
	key := keyFor(expectation.Identity, expectation.Kind)
	rec, ok := d.records[key]
	if !ok || rec.ID != expectation.ExpectedEventID {
		return false, nil
	}
	delete(d.records, key)
	delete(d.eventIdx, rec.ID)
	return true, nil
}

// FenceIf implements StateStore's exact-generation callback fence under the
// same mutex used by SaveIf.
func (d *driver) FenceIf(ctx context.Context, expectation state.SlotExpectation, fn func() error) error {
	if d.closed.Load() {
		return state.ErrStoreClosed
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := state.ValidateFenceIf(expectation, fn); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	rec, ok := d.records[keyFor(expectation.Identity, expectation.Kind)]
	if !ok || rec.ID != expectation.ExpectedEventID {
		return fmt.Errorf("%w: expected event_id %q", state.ErrConditionFailed, expectation.ExpectedEventID)
	}
	return fn()
}

// saveLocked implements Save after the caller has acquired d.mu.
func (d *driver) saveLocked(r state.StateRecord) error {
	key := keyFor(r.Identity, r.Kind)
	if prevKey, seen := d.eventIdx[r.ID]; seen {
		prev := d.records[prevKey]
		if prevKey != key {
			return fmt.Errorf("%w: EventID %q already routes to a different (Quadruple, Kind)", state.ErrIdempotencyConflict, r.ID)
		}
		if !bytes.Equal(prev.Bytes, r.Bytes) || prev.Version != r.Version {
			return fmt.Errorf("%w: EventID %q already saved with different content", state.ErrIdempotencyConflict, r.ID)
		}
		return nil
	}
	if existing, ok := d.records[key]; ok && existing.ID != r.ID {
		delete(d.eventIdx, existing.ID)
	}
	stored := state.StoredRecord(r)
	stored.Bytes = cloneBytes(r.Bytes)
	if stored.UpdatedAt.IsZero() {
		stored.UpdatedAt = time.Now()
	}
	d.records[key] = stored
	d.eventIdx[r.ID] = key
	return nil
}

// Load implements state.StateStore.
func (d *driver) Load(_ context.Context, q identity.Quadruple, kind string) (state.StateRecord, error) {
	if d.closed.Load() {
		return state.StateRecord{}, state.ErrStoreClosed
	}
	if err := state.ValidateIdentity(q); err != nil {
		return state.StateRecord{}, err
	}
	if kind == "" {
		return state.StateRecord{}, state.ErrInvalidRecord
	}

	d.mu.RLock()
	defer d.mu.RUnlock()
	rec, ok := d.records[keyFor(q, kind)]
	if !ok {
		return state.StateRecord{}, fmt.Errorf("%w: %s/%s/%s/%s kind=%s",
			state.ErrNotFound, q.TenantID, q.UserID, q.SessionID, q.RunID, kind)
	}
	rec.Bytes = cloneBytes(rec.Bytes)
	return rec, nil
}

// LoadByEventID implements state.StateStore.
func (d *driver) LoadByEventID(_ context.Context, eventID state.EventID) (state.StateRecord, error) {
	if d.closed.Load() {
		return state.StateRecord{}, state.ErrStoreClosed
	}
	if eventID == "" {
		return state.StateRecord{}, state.ErrInvalidRecord
	}

	d.mu.RLock()
	defer d.mu.RUnlock()
	key, ok := d.eventIdx[eventID]
	if !ok {
		return state.StateRecord{}, fmt.Errorf("%w: event_id=%s", state.ErrNotFound, eventID)
	}
	rec, ok := d.records[key]
	if !ok {
		// Secondary points at a slot with no primary record — a
		// driver bug. Surface it loudly.
		return state.StateRecord{}, fmt.Errorf("%w: secondary index points at missing slot for event_id=%s",
			state.ErrNotFound, eventID)
	}
	rec.Bytes = cloneBytes(rec.Bytes)
	return rec, nil
}

// Delete implements state.StateStore.
func (d *driver) Delete(_ context.Context, q identity.Quadruple, kind string) error {
	if d.closed.Load() {
		return state.ErrStoreClosed
	}
	if err := state.ValidateIdentity(q); err != nil {
		return err
	}
	if err := state.ValidateExternalKind(kind); err != nil {
		return err
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	key := keyFor(q, kind)
	rec, ok := d.records[key]
	if !ok {
		return nil // idempotent
	}
	delete(d.records, key)
	delete(d.eventIdx, rec.ID)
	return nil
}

// DeleteScope implements state.StateStore — the kind-agnostic cascade
// primitive. It removes every record whose (tenant, user, session)
// matches id, regardless of run or kind, and evicts each removed
// record's EventID from the secondary index. Identity-scoped (no
// maintenance claim) and idempotent: an absent scope returns (0, nil).
func (d *driver) DeleteScope(_ context.Context, id identity.Identity) (int, error) {
	if d.closed.Load() {
		return 0, state.ErrStoreClosed
	}
	if err := state.ValidateIdentity(identity.Quadruple{Identity: id}); err != nil {
		return 0, err
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	deleted := 0
	for key, rec := range d.records {
		if key.Tenant != id.TenantID || key.User != id.UserID || key.Session != id.SessionID {
			continue
		}
		if identity.IsInternalCoordination(id) && state.IsInternalKind(key.Kind) {
			continue
		}
		delete(d.records, key)
		delete(d.eventIdx, rec.ID)
		deleted++
	}
	return deleted, nil
}

// ListKind implements state.StateStore — the explicitly-elevated
// maintenance scan (RFC §6.11). The prefix matches literally
// via strings.HasPrefix; results carry value copies with cloned Bytes
// (same defensive-copy discipline as Load).
func (d *driver) ListKind(_ context.Context, scope state.ListScope, kindPrefix string) ([]state.StateRecord, error) {
	if d.closed.Load() {
		return nil, state.ErrStoreClosed
	}
	if err := state.ValidateListKind(scope, kindPrefix); err != nil {
		return nil, err
	}

	d.mu.RLock()
	defer d.mu.RUnlock()
	var out []state.StateRecord
	for key, rec := range d.records {
		if !strings.HasPrefix(key.Kind, kindPrefix) {
			continue
		}
		rec.Bytes = cloneBytes(rec.Bytes)
		out = append(out, rec)
	}
	return out, nil
}

// ListKindBounded implements StateStore's storage-side bounded maintenance
// scan. Map iteration cannot push a predicate into a database, but it stops
// copying as soon as the caller's bound is reached.
func (d *driver) ListKindBounded(ctx context.Context, scope state.ListScope, kindPrefix string, limit int) ([]state.StateRecord, error) {
	if d.closed.Load() {
		return nil, state.ErrStoreClosed
	}
	if err := state.ValidateListKindBounded(scope, kindPrefix, limit); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	keys := make([]indexKey, 0, len(d.records))
	for key := range d.records {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if strings.HasPrefix(key.Kind, kindPrefix) {
			keys = append(keys, key)
		}
	}
	sort.Slice(keys, func(i, j int) bool { return indexKeyLess(keys[i], keys[j]) })
	out := make([]state.StateRecord, 0, limit)
	for _, key := range keys {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		rec := d.records[key]
		rec.Bytes = cloneBytes(rec.Bytes)
		out = append(out, rec)
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

// ListKindForIdentity implements state.StateStore's non-elevated
// identity-scoped enumeration surface.
func (d *driver) ListKindForIdentity(ctx context.Context, id identity.Quadruple, kindPrefix string) ([]state.StateRecord, error) {
	if d.closed.Load() {
		return nil, state.ErrStoreClosed
	}
	if err := state.ValidateListKindForIdentity(id, kindPrefix); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	var out []state.StateRecord
	for key, rec := range d.records {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if key.Tenant != id.TenantID || key.User != id.UserID || key.Session != id.SessionID || key.Run != id.RunID || !strings.HasPrefix(key.Kind, kindPrefix) {
			continue
		}
		rec.Bytes = cloneBytes(rec.Bytes)
		out = append(out, rec)
	}
	return out, nil
}

// ListKindForIdentityBounded implements StateStore's bounded identity-local
// admission read. Map iteration may inspect more keys, but it never copies or
// returns more than limit matching records to the caller.
func (d *driver) ListKindForIdentityBounded(ctx context.Context, id identity.Quadruple, kindPrefix string, limit int) ([]state.StateRecord, error) {
	if d.closed.Load() {
		return nil, state.ErrStoreClosed
	}
	if err := state.ValidateListKindForIdentityBounded(id, kindPrefix, limit); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	d.mu.RLock()
	defer d.mu.RUnlock()
	out := make([]state.StateRecord, 0, limit)
	for key, rec := range d.records {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if key.Tenant != id.TenantID || key.User != id.UserID || key.Session != id.SessionID || key.Run != id.RunID || !strings.HasPrefix(key.Kind, kindPrefix) {
			continue
		}
		rec.Bytes = cloneBytes(rec.Bytes)
		out = append(out, rec)
		if len(out) == limit {
			break
		}
	}
	return out, nil
}

// ScanKindForTenant implements StateStore's deterministic tenant-bounded
// maintenance scan. The in-memory map is unordered, so it copies matching
// records and sorts their composite tuple before applying the keyset cursor.
func (d *driver) ScanKindForTenant(ctx context.Context, scope state.ListScope, tenantID, literalKindPrefix string, limit int, continuation string) (state.StateScanPage, error) {
	if d.closed.Load() {
		return state.StateScanPage{}, state.ErrStoreClosed
	}
	if err := state.ValidateScanKindForTenant(scope, tenantID, literalKindPrefix, limit); err != nil {
		return state.StateScanPage{}, err
	}
	cursor, err := state.DecodeStateScanContinuation(continuation, tenantID, literalKindPrefix, scope)
	if err != nil {
		return state.StateScanPage{}, err
	}
	if err := ctx.Err(); err != nil {
		return state.StateScanPage{}, err
	}

	d.mu.RLock()
	defer d.mu.RUnlock()
	rows := make([]state.StateRecord, 0)
	for key, rec := range d.records {
		if err := ctx.Err(); err != nil {
			return state.StateScanPage{}, err
		}
		if key.Tenant != tenantID || !strings.HasPrefix(key.Kind, literalKindPrefix) || !afterScanCursor(key, cursor) {
			continue
		}
		rec.Bytes = cloneBytes(rec.Bytes)
		rows = append(rows, rec)
	}
	sort.Slice(rows, func(i, j int) bool { return scanRecordLess(rows[i], rows[j]) })
	if err := ctx.Err(); err != nil {
		return state.StateScanPage{}, err
	}
	if len(rows) > limit+1 {
		rows = rows[:limit+1]
	}
	page := state.StateScanPage{Records: rows}
	if len(page.Records) <= limit {
		return page, nil
	}
	page.Records = page.Records[:limit]
	last := page.Records[len(page.Records)-1]
	page.Continuation, err = state.EncodeStateScanContinuation(state.StateScanCursor{UserID: last.Identity.UserID, SessionID: last.Identity.SessionID, RunID: last.Identity.RunID, Kind: last.Kind}, tenantID, literalKindPrefix, scope)
	if err != nil {
		return state.StateScanPage{}, err
	}
	return page, nil
}

func afterScanCursor(key indexKey, cursor state.StateScanCursor) bool {
	if cursor.UserID == "" {
		return true
	}
	return tupleCompare(key.User, key.Session, key.Run, key.Kind, cursor.UserID, cursor.SessionID, cursor.RunID, cursor.Kind) > 0
}

func scanRecordLess(left, right state.StateRecord) bool {
	return tupleCompare(left.Identity.UserID, left.Identity.SessionID, left.Identity.RunID, left.Kind, right.Identity.UserID, right.Identity.SessionID, right.Identity.RunID, right.Kind) < 0
}

func indexKeyLess(left, right indexKey) bool {
	for _, pair := range [][2]string{
		{left.Tenant, right.Tenant},
		{left.User, right.User},
		{left.Session, right.Session},
		{left.Run, right.Run},
		{left.Kind, right.Kind},
	} {
		if pair[0] < pair[1] {
			return true
		}
		if pair[0] > pair[1] {
			return false
		}
	}
	return false
}

func tupleCompare(leftUser, leftSession, leftRun, leftKind, rightUser, rightSession, rightRun, rightKind string) int {
	for _, pair := range [][2]string{{leftUser, rightUser}, {leftSession, rightSession}, {leftRun, rightRun}, {leftKind, rightKind}} {
		if pair[0] < pair[1] {
			return -1
		}
		if pair[0] > pair[1] {
			return 1
		}
	}
	return 0
}

// Close implements state.StateStore. Idempotent.
func (d *driver) Close(_ context.Context) error {
	d.closed.Store(true)
	return nil
}

func cloneBytes(b []byte) []byte {
	if b == nil {
		return nil
	}
	out := make([]byte, len(b))
	copy(out, b)
	return out
}

// Compile-time assertion that driver satisfies state.StateStore.
var _ state.StateStore = (*driver)(nil)
