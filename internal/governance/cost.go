package governance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/state"
)

// kindGovernanceCost is the StateStore Kind used to persist per-identity
// cost accumulators. Centralised so every code path that loads / writes
// the record references one symbol (matches the memory subsystem's
// `kindMemoryState` convention).
const kindGovernanceCost = "governance.cost"

// CostAccumulator is the Phase 36a `Subsystem` that aggregates LLM cost
// per `(tenant, user, session, model)` and enforces per-tier ceilings
// in PreCall. State persists to a `state.StateStore` so the accumulator
// survives runtime restart; three V1 drivers (in-mem / SQLite / Postgres)
// pass identical conformance tests.
//
// Latent default: an empty `Config.IdentityTiers` map disables every
// enforcement path. PreCall returns nil; PostCall still records cost so
// the accumulator's observability surface stays alive (operators get
// per-identity cost dashboards without opting into enforcement). This
// satisfies the Wave 7b scoping decision: "interface + math only,
// ceilings opt-in."
//
// Concurrent reuse (D-025): the accumulator's per-key state lives in a
// `sync.Map` of `*costKeyState`; each `costKeyState` uses an atomic
// `uint64`-packed float64 for the cumulative total. The CAS loop in
// `add` is lock-free and N concurrent PostCalls cannot lose updates.
// PreCall reads the atomic snapshot — the race window between PreCall
// (read) and the next call's PostCall (write) is bounded by
// `in_flight × per_call_cost`; this is documented in the phase plan.
type CostAccumulator struct {
	cfg   Config
	state state.StateStore
	bus   events.EventBus
	clock Clock

	// keys is the per-(identity, tier) in-memory aggregation cache.
	// Each entry's totals are atomic; per-model totals also live as
	// atomics. The map itself is internally synchronised by sync.Map.
	keys sync.Map // map[quadKey]*costKeyState

	closed atomic.Bool
}

// costKeyState is the per-quadruple cumulative state. Loaded lazily on
// first reference (PostCall write OR PreCall check), persisted on every
// successful PostCall mutation. Backing record is JSON-encoded so the
// wire shape is byte-stable across StateStore drivers.
//
// Atomicity model:
//
//   - `totalBits` is the cumulative TotalCost across every model under
//     this identity, encoded as `math.Float64bits`. Lock-free CAS loop
//     for atomic add (`addAtomic`); atomic.Load for the PreCall read.
//   - `modelTotals` is a sync.Map keyed by model name; each value is a
//     pointer to a uint64 holding the per-model encoded float. Same CAS
//     pattern.
//   - `loaded` (atomic.Bool) gates the lazy-load-from-state path; once
//     loaded the in-memory state is the source of truth and writes
//     write-through to state.
//   - `persistMu` serialises persistence writes so concurrent PostCalls
//     don't race the JSON encode + state.Save sequence (each individual
//     write is one upsert; the mutex is short-lived).
type costKeyState struct {
	modelTotals sync.Map
	totalBits   atomic.Uint64
	updatedAt   atomic.Int64
	loadMu      sync.Mutex
	persistMu   sync.Mutex
	loaded      atomic.Bool
}

// costRecord is the JSON-encoded wire shape persisted at
// (Identity, Kind="governance.cost"). Stable across drivers per the
// `internal/state` Bytes-opaque contract.
type costRecord struct {
	UpdatedAt time.Time          `json:"updated_at"`
	ByModel   map[string]float64 `json:"by_model,omitempty"`
	Total     float64            `json:"total"`
	Schema    int                `json:"schema"`
}

const costRecordSchema = 1

// NewCostAccumulator constructs a Phase 36a `CostAccumulator`. `state`
// is the persistence floor (mandatory); `bus` is the observability bus
// (mandatory); `cfg` carries operator-supplied tier ceilings.
//
// Validation: nil state or nil bus → `ErrInvalidConfig`. Config itself
// may have an empty IdentityTiers map — that's the latent default.
func NewCostAccumulator(state state.StateStore, bus events.EventBus, cfg Config) (*CostAccumulator, error) {
	if state == nil {
		return nil, fmt.Errorf("%w: state.StateStore is required", ErrInvalidConfig)
	}
	if bus == nil {
		return nil, fmt.Errorf("%w: events.EventBus is required", ErrInvalidConfig)
	}
	a := &CostAccumulator{
		cfg:   cfg,
		state: state,
		bus:   bus,
		clock: cfg.clock(),
	}
	return a, nil
}

// PreCall checks the cost ceiling for the request's identity. With no
// ceiling configured (latent default) the function returns nil
// unconditionally. On state-read failure → wrapped `ErrStateUnavailable`
// (no silent permit per AGENTS.md §13).
func (a *CostAccumulator) PreCall(ctx context.Context, req llm.CompleteRequest) error {
	if a.closed.Load() {
		return ErrClosed
	}
	id, err := identityFromCtx(ctx)
	if err != nil {
		return err
	}
	tier, ok := a.cfg.tierConfig(id)
	if !ok || tier.BudgetCeilingUSD <= 0 {
		// Latent — no enforcement for this identity.
		return nil
	}

	quad, err := quadrupleFromCtx(ctx)
	if err != nil {
		return err
	}
	ks, err := a.keyState(ctx, quad)
	if err != nil {
		return err
	}
	total := math.Float64frombits(ks.totalBits.Load())
	if total >= tier.BudgetCeilingUSD {
		a.emitBudgetExceeded(ctx, quad, a.cfg.resolveTier(id), req.Model, total, tier.BudgetCeilingUSD)
		return errorWith(ErrBudgetExceeded,
			"identity=%s/%s/%s total=%.6f ceiling=%.6f",
			id.TenantID, id.UserID, id.SessionID, total, tier.BudgetCeilingUSD)
	}
	return nil
}

// PostCall accumulates `resp.Cost.TotalCost` regardless of `callErr`.
// Failures still incur whatever cost the provider reported (some report
// 0 on failure; that's fine — the accumulator records 0).
//
// The accumulator path is in-band synchronous (RFC §6.15 line 1128)
// rather than event-subscriber: the next PreCall sees the latest total
// without a bus-delivery race.
//
// `governance.budget_exceeded` events are NOT emitted from PostCall;
// they fire only from PreCall on the NEXT call that exceeds. A PostCall
// that pushes the accumulator over the ceiling is accepted (the call
// already happened); the operator sees the breach via the cost-recorded
// observability stream that bifrost already publishes.
func (a *CostAccumulator) PostCall(ctx context.Context, req llm.CompleteRequest, resp llm.CompleteResponse, _ error) error {
	if a.closed.Load() {
		return ErrClosed
	}
	// Identity check first — AGENTS.md §6 rule 9: identity is
	// mandatory; missing triple fails closed. Mirrors PreCall.
	quad, err := quadrupleFromCtx(ctx)
	if err != nil {
		return err
	}
	if resp.Cost.TotalCost == 0 && len(resp.Content) == 0 && resp.Usage.TotalTokens == 0 {
		// No accounting work to do — likely a failed call that the
		// provider didn't price. The bifrost driver still emits
		// llm.cost.recorded; governance just doesn't accumulate zero.
		return nil
	}
	ks, err := a.keyState(ctx, quad)
	if err != nil {
		return err
	}
	a.addAtomic(ks, req.Model, resp.Cost.TotalCost)
	a.touch(ks)
	if err := a.persist(ctx, quad, ks); err != nil {
		return fmt.Errorf("governance/cost: persist: %w", err)
	}
	return nil
}

// Snapshot returns the current per-(identity, model) accumulator total
// + the identity-level grand total. Used by tests and by Phase 91's
// Console-driven inspection (post-V1). Concurrent-safe.
func (a *CostAccumulator) Snapshot(ctx context.Context, q identity.Quadruple) (float64, map[string]float64, error) {
	if a.closed.Load() {
		return 0, nil, ErrClosed
	}
	if err := state.ValidateIdentity(q); err != nil {
		return 0, nil, fmt.Errorf("%w: %w", ErrIdentityRequired, err)
	}
	ks, err := a.keyState(ctx, q)
	if err != nil {
		return 0, nil, err
	}
	total := math.Float64frombits(ks.totalBits.Load())
	byModel := make(map[string]float64)
	ks.modelTotals.Range(func(k, v any) bool {
		name, _ := k.(string)         //nolint:errcheck // modelTotals only ever stores string keys; assertion cannot fail
		bits, _ := v.(*atomic.Uint64) //nolint:errcheck // modelTotals only ever stores *atomic.Uint64 values; assertion cannot fail
		if bits != nil {
			byModel[name] = math.Float64frombits(bits.Load())
		}
		return true
	})
	return total, byModel, nil
}

// Close marks the accumulator closed; subsequent PreCall / PostCall /
// Snapshot calls fail loud with `ErrClosed`. Idempotent.
func (a *CostAccumulator) Close(_ context.Context) error {
	a.closed.Store(true)
	return nil
}

// keyState returns the per-key in-memory cache. On first miss it lazily
// loads from StateStore (driver read failure → wrapped
// ErrStateUnavailable). Subsequent calls return the cached value.
func (a *CostAccumulator) keyState(ctx context.Context, q identity.Quadruple) (*costKeyState, error) {
	k := quadKeyFor(q)
	if v, ok := a.keys.Load(k); ok {
		ks := v.(*costKeyState) //nolint:errcheck // a.keys only ever stores *costKeyState; assertion cannot fail
		if err := a.lazyLoad(ctx, q, ks); err != nil {
			return nil, err
		}
		return ks, nil
	}
	fresh := &costKeyState{}
	actual, _ := a.keys.LoadOrStore(k, fresh)
	ks := actual.(*costKeyState) //nolint:errcheck // a.keys only ever stores *costKeyState; assertion cannot fail
	if err := a.lazyLoad(ctx, q, ks); err != nil {
		return nil, err
	}
	return ks, nil
}

func (a *CostAccumulator) lazyLoad(ctx context.Context, q identity.Quadruple, ks *costKeyState) error {
	if ks.loaded.Load() {
		return nil
	}
	ks.loadMu.Lock()
	defer ks.loadMu.Unlock()
	if ks.loaded.Load() {
		return nil
	}
	rec, err := a.state.Load(ctx, q, kindGovernanceCost)
	if err != nil {
		if errors.Is(err, state.ErrNotFound) {
			// No prior record — start at zero, mark loaded.
			ks.loaded.Store(true)
			return nil
		}
		return fmt.Errorf("%w: %w", ErrStateUnavailable, err)
	}
	if len(rec.Bytes) == 0 {
		ks.loaded.Store(true)
		return nil
	}
	var cr costRecord
	if err := json.Unmarshal(rec.Bytes, &cr); err != nil {
		// Corrupt record → fail loud rather than silently reset.
		return fmt.Errorf("%w: unmarshal cost record: %w", ErrStateUnavailable, err)
	}
	// Forward-compat guard: a record from a future schema would be
	// partially parsed silently; fail loud per AGENTS.md §13.
	if cr.Schema != 0 && cr.Schema != costRecordSchema {
		return fmt.Errorf("%w: cost record schema=%d, runtime supports %d", ErrStateUnavailable, cr.Schema, costRecordSchema)
	}
	ks.totalBits.Store(math.Float64bits(cr.Total))
	for m, v := range cr.ByModel {
		var u atomic.Uint64
		u.Store(math.Float64bits(v))
		ks.modelTotals.Store(m, &u)
	}
	if !cr.UpdatedAt.IsZero() {
		ks.updatedAt.Store(cr.UpdatedAt.UnixNano())
	}
	ks.loaded.Store(true)
	return nil
}

// addAtomic adds `delta` to the per-key + per-(key,model) totals using
// a CAS loop over the packed float64 bits. Returns the new total.
func (a *CostAccumulator) addAtomic(ks *costKeyState, model string, delta float64) float64 {
	// Per-key total.
	var newTotal float64
	for {
		cur := ks.totalBits.Load()
		newTotal = math.Float64frombits(cur) + delta
		if ks.totalBits.CompareAndSwap(cur, math.Float64bits(newTotal)) {
			break
		}
	}
	// Per-(key, model) total.
	if model != "" {
		var bitsPtr *atomic.Uint64
		if v, ok := ks.modelTotals.Load(model); ok {
			bitsPtr = v.(*atomic.Uint64) //nolint:errcheck // modelTotals only ever stores *atomic.Uint64; assertion cannot fail
		} else {
			fresh := &atomic.Uint64{}
			actual, _ := ks.modelTotals.LoadOrStore(model, fresh)
			bitsPtr = actual.(*atomic.Uint64) //nolint:errcheck // modelTotals only ever stores *atomic.Uint64; assertion cannot fail
		}
		for {
			cur := bitsPtr.Load()
			next := math.Float64bits(math.Float64frombits(cur) + delta)
			if bitsPtr.CompareAndSwap(cur, next) {
				break
			}
		}
	}
	return newTotal
}

func (a *CostAccumulator) touch(ks *costKeyState) {
	ks.updatedAt.Store(a.clock.Now().UnixNano())
}

// persist writes the canonical JSON record for the key. Each persist
// holds `persistMu` so concurrent PostCalls don't race the JSON encode +
// state.Save sequence (only the persistence step is serialised; the
// atomic add already happened).
func (a *CostAccumulator) persist(ctx context.Context, q identity.Quadruple, ks *costKeyState) error {
	ks.persistMu.Lock()
	defer ks.persistMu.Unlock()
	cr := costRecord{
		Total:     math.Float64frombits(ks.totalBits.Load()),
		ByModel:   map[string]float64{},
		UpdatedAt: time.Unix(0, ks.updatedAt.Load()),
		Schema:    costRecordSchema,
	}
	ks.modelTotals.Range(func(k, v any) bool {
		name, _ := k.(string)         //nolint:errcheck // modelTotals only ever stores string keys; assertion cannot fail
		bits, _ := v.(*atomic.Uint64) //nolint:errcheck // modelTotals only ever stores *atomic.Uint64; assertion cannot fail
		if bits != nil {
			cr.ByModel[name] = math.Float64frombits(bits.Load())
		}
		return true
	})
	buf, err := json.Marshal(cr)
	if err != nil {
		return fmt.Errorf("marshal cost record: %w", err)
	}
	rec := state.StateRecord{
		ID:        state.NewEventID(),
		Identity:  q,
		Kind:      kindGovernanceCost,
		Version:   0,
		Bytes:     buf,
		UpdatedAt: cr.UpdatedAt,
	}
	return a.state.Save(ctx, rec)
}

// emitBudgetExceeded publishes the typed event. Best-effort — a bus
// failure does NOT change the PreCall return value (the rejection
// happened regardless of whether the observer saw the event).
func (a *CostAccumulator) emitBudgetExceeded(ctx context.Context, q identity.Quadruple, tier, model string, total, ceiling float64) {
	now := a.clock.Now()
	_ = a.bus.Publish(ctx, events.Event{ //nolint:errcheck // best-effort event emit; publish failure must not fail cost accounting
		Type:       EventTypeBudgetExceeded,
		Identity:   q,
		OccurredAt: now,
		Payload: BudgetExceededPayload{
			Identity:   q,
			Tier:       tier,
			Model:      model,
			TotalCost:  total,
			Ceiling:    ceiling,
			Currency:   "USD",
			OccurredAt: now,
		},
	})
}

// var compile-time assertions.
var (
	_ Subsystem = (*CostAccumulator)(nil)
)

// total is a test-friendly accessor for the in-memory total bypassing
// the lazy-load path. Used only in package-internal tests; not exported.
//
//nolint:unused // referenced by tests in same package.
func (a *CostAccumulator) totalLoaded(q identity.Quadruple) (float64, bool) {
	v, ok := a.keys.Load(quadKeyFor(q))
	if !ok {
		return 0, false
	}
	ks := v.(*costKeyState) //nolint:errcheck // a.keys only ever stores *costKeyState; assertion cannot fail
	return math.Float64frombits(ks.totalBits.Load()), ks.loaded.Load()
}
