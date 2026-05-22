// Phase 78 — the master chaos / fault-injection harness (RFC §6.1,
// §6.5, §6.11, §6.13, §3.4).
//
// # What this is
//
// Phases 76 (cross-tenant isolation) and 77 (goroutine leak) each ship
// a conformance gate that proves a happy-path invariant under stress.
// Phase 78 is the complementary gate: it proves the Runtime behaves
// correctly UNDER FAILURE. It injects each of the five master-plan-
// named failure modes against the REAL Runtime components and asserts
// every fault produces its DOCUMENTED loud error / event AND the
// DOCUMENTED recovery path runs.
//
//  1. Kill mid-run — cancel a run while envelopes are in flight;
//     assert the runtime.run_cancelled event fires + Engine.Stop
//     tears down cleanly (no goroutine leak).
//  2. Drop messages — saturate a bounded subscription; assert the
//     bus emits the typed bus.dropped backpressure event.
//  3. Provider quirks — a mock LLM driver returns malformed output;
//     wrapped by the REAL retry-with-feedback layer, assert the
//     llm.retry_with_feedback event fires + the call exhausts with
//     ErrRetryExhausted (and a recovery sub-case succeeds).
//  4. StateStore disconnect — a fault-injecting decorator over the
//     real StateStore returns a transport error; assert the error
//     surfaces LOUDLY (no silent degradation) + the reconnect
//     recovery path works.
//  5. Pause-deserialize failure — a PauseRequest whose trajectory
//     carries a live channel fails Coordinator.Request loud with
//     trajectory.ErrUnserializable — never a half-persisted
//     checkpoint, never (nil, nil).
//
// # Why a table
//
// Each failure mode is one `chaosCase` row: a name plus an `inject`
// closure that wires the real component, injects the fault, and
// asserts both the loud-failure half and the recovery half. A future
// failure mode is one new row.
//
// # No silent degradation (CLAUDE.md §13)
//
// Every row asserts the fault is SURFACED — a loud error or a typed
// event. The harness proves recovery; it never masks a failure. A row
// that "passed" by swallowing the injected fault would defeat the gate.
//
// # Real drivers at the seam (CLAUDE.md §17.3)
//
//  1. Every component is opened through its production registry
//     factory / constructor (`events.Open`, `state.Open`,
//     `engine.New`, `pauseresume.New`, `retry.Wrap`). Faults are
//     injected by THIN DECORATORS over those real components
//     (phase78_faults_test.go) — never by substituting a stub for a
//     real driver (that would be the §13 anti-pattern; see D-137).
//  2. Identity propagation — every workload carries a real
//     (tenant, user, session) triple through the component under test.
//  3. ≥1 failure mode — the harness IS five failure modes.
//  4. -race is the CI gate — the dedicated `chaos` job in
//     `.github/workflows/ci.yml` runs this file under `-race`.
//
// The harness is NOT t.Parallel: the kill-mid-run row reads
// process-global `runtime.NumGoroutine`, and a parallel sibling would
// pollute the count (matching Phase 77).
package integration_test

import (
	"context"
	"errors"
	"runtime"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/llm/retry"
	"github.com/hurtener/Harbor/internal/planner/trajectory"
	"github.com/hurtener/Harbor/internal/runtime/engine"
	"github.com/hurtener/Harbor/internal/runtime/messages"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
	"github.com/hurtener/Harbor/internal/state"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	// Driver self-registration (blank import — §4.4 seam pattern).
	_ "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	_ "github.com/hurtener/Harbor/internal/state/drivers/inmem"
)

// chaosIdentity is the identity triple every chaos workload runs
// under. Identity is mandatory (CLAUDE.md §6) — the harness drives a
// real triple through every component, never an empty one.
func chaosIdentity() identity.Identity {
	return identity.Identity{
		TenantID:  "phase78-tenant",
		UserID:    "phase78-user",
		SessionID: "phase78-session",
	}
}

// chaosCase is one row of the chaos table: a named failure mode plus
// an `inject` closure that wires the real component, injects the
// fault, asserts the fault is surfaced LOUDLY, and asserts the
// documented recovery path runs.
type chaosCase struct {
	name   string
	inject func(t *testing.T)
}

// chaosCases is the conformance table — one row per master-plan
// failure mode. A future failure mode is one new row.
var chaosCases = []chaosCase{
	{
		// 1. Kill mid-run. Cancel a run while envelopes are in
		// flight; assert the engine fires its run-cancelled seam
		// (the production wiring publishes runtime.run_cancelled on
		// the bus from this notice) AND Engine.Stop tears the engine
		// down cleanly within a bounded deadline — no goroutine leak,
		// no orphaned run state. A teardown that hung would be the
		// inherited deadlock-on-shutdown bug (brief 01).
		name:   "kill-mid-run",
		inject: injectKillMidRun,
	},
	{
		// 2. Drop messages. Saturate a bounded subscription so the
		// bus's drop-oldest backpressure policy fires; assert the
		// typed bus.dropped event is delivered carrying the dropped
		// sequence range — the documented signal that a consumer
		// missed events (RFC §6.13, brief 06).
		name:   "drop-messages",
		inject: injectDropMessages,
	},
	{
		// 3. Provider quirks. A mock LLM driver returns malformed
		// output; wrapped in the REAL retry-with-feedback layer with
		// a rejecting Validator, assert the llm.retry_with_feedback
		// event fires AND the call exhausts loudly with
		// ErrRetryExhausted — then assert the recovery path: a driver
		// that emits one bad response then a good one succeeds.
		name:   "provider-quirks",
		inject: injectProviderQuirks,
	},
	{
		// 4. StateStore disconnect. A fault-injecting decorator over
		// the real in-mem StateStore returns a transport error;
		// assert the error surfaces LOUDLY out of Save/Load (no
		// silent degradation) AND the reconnect recovery path works
		// once the fault clears (RFC §6.11, brief 05).
		name:   "statestore-disconnect",
		inject: injectStateStoreDisconnect,
	},
	{
		// 5. Force pause-deserialize failure. A PauseRequest whose
		// trajectory carries a non-serialisable handle (a live
		// channel) fails Coordinator.Request LOUD with
		// trajectory.ErrUnserializable naming the offending field
		// path — never a half-persisted checkpoint, never (nil, nil)
		// (RFC §3.4, D-069, brief 02 §4).
		name:   "pause-deserialize-failure",
		inject: injectPauseDeserializeFailure,
	},
}

// TestE2E_Phase78_ChaosFaultInjection is the Phase 78 chaos gate. For
// every master-plan failure mode in `chaosCases` it injects the fault
// against the real Runtime component and asserts the documented loud
// error / event AND the documented recovery path.
//
// NOT t.Parallel — the kill-mid-run row reads process-global
// runtime.NumGoroutine (matching Phase 77).
func TestE2E_Phase78_ChaosFaultInjection(t *testing.T) {
	for _, tc := range chaosCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			tc.inject(t)
		})
	}
}

// ---------------------------------------------------------------------
// 1. Kill mid-run
// ---------------------------------------------------------------------

// injectKillMidRun emits envelopes into a real engine, cancels one run
// mid-flight, and asserts: (a) the engine's RunCancelledHandler seam
// fires (the production wiring publishes runtime.run_cancelled from
// this notice); (b) the cancelled run's FetchByRun observes
// ErrRunCancelled — the cancellation propagated; (c) Engine.Stop tears
// the engine down within a bounded deadline; (d) no goroutine leak.
func injectKillMidRun(t *testing.T) {
	t.Helper()

	// Goroutine baseline BEFORE constructing the engine — settle the
	// scheduler without time.Sleep (§17.4).
	runtime.GC()
	for i := 0; i < 100; i++ {
		runtime.Gosched()
	}
	baseline := runtime.NumGoroutine()

	// Real engine: a linear A->B graph. Node A holds the run
	// IN-FLIGHT: it signals the test the moment a worker picks the
	// envelope up, then blocks until the test releases it — so the
	// run is genuinely mid-flight when Cancel fires. The blocked
	// worker observes the cancellation flag on release and the engine
	// has an active worker for the run, the `wasActive` signal Cancel
	// reports.
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	nodeA := func(ctx context.Context, in messages.Envelope, _ *engine.NodeContext) (messages.Envelope, error) {
		select {
		case entered <- struct{}{}:
		default:
		}
		select {
		case <-release:
		case <-ctx.Done():
		}
		return in, nil
	}
	passthrough := func(_ context.Context, in messages.Envelope, _ *engine.NodeContext) (messages.Envelope, error) {
		return in, nil
	}
	a := engine.Node{Name: "A", Func: nodeA}
	b := engine.Node{Name: "B", Func: passthrough}

	// The RunCancelledHandler seam is what production wiring uses to
	// publish runtime.run_cancelled on the bus. The harness installs
	// a recording handler so it can assert the notice fired with the
	// cancelled run's id — the loud-event half of the failure mode.
	cancelled := make(chan engine.RunCancelledNotice, 1)
	e, err := engine.New([]engine.Adjacency{
		{From: a, To: []engine.Node{b}},
		{From: b, To: nil},
	}, engine.WithRunCancelledHandler(func(_ context.Context, n engine.RunCancelledNotice) {
		select {
		case cancelled <- n:
		default:
		}
	}))
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	if err := e.Run(context.Background()); err != nil {
		t.Fatalf("engine.Run: %v", err)
	}

	id := chaosIdentity()
	env := messages.Envelope{
		Headers:   messages.Headers{TenantID: id.TenantID, UserID: id.UserID},
		SessionID: id.SessionID,
		RunID:     "R-killed",
		Payload:   "phase78-chaos",
	}
	if err := e.Emit(context.Background(), env); err != nil {
		t.Fatalf("engine.Emit R-killed: %v", err)
	}

	// Wait — bounded — until node A's worker has the envelope: the run
	// is now genuinely in flight.
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("kill-mid-run: run never entered node A — could not get a run in-flight to kill")
	}

	// Kill the run mid-flight.
	observed, err := e.Cancel(context.Background(), "R-killed")
	if err != nil {
		t.Fatalf("engine.Cancel: %v", err)
	}
	if !observed {
		t.Fatal("kill-mid-run: Cancel reported the run was not active — expected an in-flight run")
	}
	// Release the blocked worker so it observes the cancellation flag
	// and unwinds — the in-flight worker's clean-unwind path.
	close(release)

	// Loud-event assertion: the run-cancelled seam fired for the
	// killed run. This is the notice the production bus wiring
	// translates into the runtime.run_cancelled event.
	select {
	case n := <-cancelled:
		if n.RunID != "R-killed" {
			t.Errorf("kill-mid-run: run-cancelled notice carried RunID=%q, want R-killed", n.RunID)
		}
		if n.CancelledAt.IsZero() {
			t.Error("kill-mid-run: run-cancelled notice has a zero CancelledAt timestamp")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("kill-mid-run: engine never fired the RunCancelledHandler — cancellation event lost")
	}

	// Recovery-path assertion: the cancellation propagated — an
	// in-flight FetchByRun for the killed run observes ErrRunCancelled
	// loudly, never a stale envelope.
	fetchCtx, fcancel := context.WithTimeout(context.Background(), 3*time.Second)
	if _, ferr := e.FetchByRun(fetchCtx, "R-killed"); !errors.Is(ferr, engine.ErrRunCancelled) {
		t.Errorf("kill-mid-run: FetchByRun(R-killed) = %v, want ErrRunCancelled", ferr)
	}
	fcancel()

	// Recovery-path assertion: clean teardown within a bounded
	// deadline — Engine.Stop joins every goroutine. A hang here is
	// the inherited deadlock-on-shutdown bug (brief 01).
	stopCtx, scancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer scancel()
	if err := e.Stop(stopCtx); err != nil {
		t.Fatalf("kill-mid-run: engine.Stop after cancellation: %v", err)
	}

	// No goroutine leak after teardown — bounded eventually-poll,
	// never an instant snapshot (§17.4).
	if got, ok := waitForGoroutineFloor(3*time.Second, baseline+8); !ok {
		t.Errorf("kill-mid-run: goroutine leak after Stop: baseline=%d after=%d (delta=%d)",
			baseline, got, got-baseline)
	}
}

// ---------------------------------------------------------------------
// 2. Drop messages
// ---------------------------------------------------------------------

// injectDropMessages opens a real in-mem EventBus, subscribes with a
// deliberately tiny buffer, publishes strictly more events than the
// buffer holds WITHOUT draining (so the drop-oldest backpressure
// policy fires), then drains and asserts the typed bus.dropped event
// is delivered carrying a non-empty dropped sequence range.
func injectDropMessages(t *testing.T) {
	t.Helper()
	ctx := context.Background()

	redactor := auditpatterns.New()

	// Tiny subscriber buffer: a small buffer guarantees the bus's
	// bounded-channel drop-oldest policy fires once we publish past
	// it without draining. A short DropWindow lets the windowed
	// bus.dropped notice fire after the window elapses + one more
	// publish — the documented emit cadence (RFC §6.13).
	const (
		bufSize    = 4
		dropWindow = 10 * time.Millisecond
	)
	bus, err := events.Open(ctx, config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     bufSize,
		IdleTimeout:              60 * time.Second,
		DropWindow:               dropWindow,
	}, redactor)
	if err != nil {
		t.Fatalf("events.Open(inmem): %v", err)
	}
	defer func() { _ = bus.Close(ctx) }()

	id := chaosIdentity()
	subCtx, err := identity.With(ctx, id)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	sub, err := bus.Subscribe(subCtx, events.Filter{
		Tenant:  id.TenantID,
		User:    id.UserID,
		Session: id.SessionID,
	})
	if err != nil {
		t.Fatalf("bus.Subscribe: %v", err)
	}
	defer sub.Cancel()

	publish := func(label string, i int) {
		ev := events.Event{
			Type:     events.EventTypeRuntimeWarning,
			Identity: identity.Quadruple{Identity: id},
			Payload: events.RuntimeErrorPayload{
				Message: "phase78-chaos-drop",
			},
		}
		if err := bus.Publish(subCtx, ev); err != nil {
			t.Fatalf("bus.Publish %s #%d: %v", label, i, err)
		}
	}

	// Saturate: publish strictly more events than the buffer holds,
	// WITHOUT draining. The bus drops the oldest under saturation and
	// records the dropped range into the open drop window.
	const burst = bufSize * 6
	for i := 0; i < burst; i++ {
		publish("burst", i)
	}

	// The bus.dropped notice is WINDOWED — it emits on the first
	// enqueue at or after DropWindow has elapsed since the last emit
	// (RFC §6.13). Let the window elapse, then publish exactly ONE
	// more event to trigger the windowed emit. One trigger event
	// (not a second burst) keeps the just-landed notice from being
	// displaced back out of the small buffer. The sleep here waits
	// for a real duration (the drop window itself) to pass — it is
	// NOT a synchronisation primitive standing in for an async signal
	// (§17.4); the assertion below is a bounded poll on the channel.
	time.Sleep(2 * dropWindow)
	publish("trigger", 0)

	// Drain and assert: somewhere in the delivered stream is the
	// typed bus.dropped backpressure event. Bounded eventually-poll
	// on the channel — never a time.Sleep (§17.4).
	deadline := time.After(5 * time.Second)
	sawDropped := false
	for !sawDropped {
		select {
		case got, ok := <-sub.Events():
			if !ok {
				t.Fatal("drop-messages: subscription closed before a bus.dropped event arrived")
			}
			if got.Type != events.EventTypeBusDropped {
				continue
			}
			drop, isDrop := got.Payload.(events.BusDroppedPayload)
			if !isDrop {
				t.Fatalf("drop-messages: bus.dropped event payload is %T, want BusDroppedPayload", got.Payload)
			}
			// Loud-event assertion: the dropped range is non-empty —
			// the bus told the consumer exactly which sequences it
			// missed (RFC §6.13, brief 06).
			if drop.DroppedCount == 0 {
				t.Error("drop-messages: bus.dropped event reports DroppedCount=0 — expected a non-empty dropped range")
			}
			if drop.ToSeq < drop.FromSeq {
				t.Errorf("drop-messages: bus.dropped range is inverted: [%d,%d]", drop.FromSeq, drop.ToSeq)
			}
			sawDropped = true
		case <-deadline:
			t.Fatal("drop-messages: no bus.dropped event within deadline — backpressure policy did not fire")
		}
	}
}

// ---------------------------------------------------------------------
// 3. Provider quirks
// ---------------------------------------------------------------------

// injectProviderQuirks drives the LLM provider-quirk path. A
// quirkLLMDriver returns malformed output; it is wrapped in the REAL
// retry-with-feedback layer (`retry.Wrap` — the production consumer of
// provider quirks) with a rejecting Validator. The harness asserts two
// halves:
//
//   - Quirk NOT recovered: a driver that emits more bad responses than
//     the retry budget exhausts LOUDLY with llm.ErrRetryExhausted, and
//     the llm.retry_with_feedback event fires on the bus.
//   - Quirk recovered: a driver that emits one bad response then a
//     valid one succeeds after one retry — the documented recovery
//     path.
func injectProviderQuirks(t *testing.T) {
	t.Helper()
	ctx := context.Background()

	redactor := auditpatterns.New()
	bus, err := events.Open(ctx, config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     64,
		IdleTimeout:              60 * time.Second,
		DropWindow:               time.Second,
	}, redactor)
	if err != nil {
		t.Fatalf("events.Open(inmem): %v", err)
	}
	defer func() { _ = bus.Close(ctx) }()

	id := chaosIdentity()
	idCtx, err := identity.With(ctx, id)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}

	// A Validator that rejects the malformed response and accepts the
	// valid one. This is the caller-supplied post-response hook the
	// retry-with-feedback loop drives off.
	const (
		quirkModel  = "phase78-model"
		badContent  = "{ truncated malformed provider output"
		goodContent = `{"ok":true}`
	)
	validator := func(resp llm.CompleteResponse) error {
		if resp.Content != goodContent {
			return errors.New("validator: provider returned malformed output")
		}
		return nil
	}

	// retry.Wrap consults ModelProfile.MaxRetries; one profile entry
	// pins the retry budget so the exhaustion case is deterministic.
	cfg := llm.ConfigSnapshot{
		ModelProfiles: map[string]llm.ModelProfile{
			quirkModel: {MaxRetries: 2},
		},
	}
	deps := llm.Deps{Bus: bus}

	t.Run("quirk-not-recovered", func(t *testing.T) {
		// Subscribe BEFORE the call so the retry event is observable.
		sub, serr := bus.Subscribe(idCtx, events.Filter{
			Tenant:  id.TenantID,
			User:    id.UserID,
			Session: id.SessionID,
			Types:   []events.EventType{llm.EventTypeRetryWithFeedback},
		})
		if serr != nil {
			t.Fatalf("bus.Subscribe: %v", serr)
		}
		defer sub.Cancel()

		// The driver returns malformed output on EVERY call — more
		// than the retry budget — so the loop exhausts.
		quirk := newQuirkLLMDriver(99, badContent, goodContent)
		client := retry.Wrap(quirk, cfg, deps)
		defer func() { _ = client.Close(ctx) }()

		_, cerr := client.Complete(idCtx, llm.CompleteRequest{
			Model:     quirkModel,
			Validator: validator,
		})
		// Loud-failure assertion: the bad provider output is NEVER
		// silently returned — the call exhausts with ErrRetryExhausted.
		if !errors.Is(cerr, llm.ErrRetryExhausted) {
			t.Fatalf("provider-quirks: Complete = %v, want ErrRetryExhausted", cerr)
		}

		// Loud-event assertion: the retry-with-feedback event fired
		// — the documented signal that a provider quirk triggered a
		// corrective re-ask.
		select {
		case got, ok := <-sub.Events():
			if !ok {
				t.Fatal("provider-quirks: subscription closed before a retry event arrived")
			}
			if got.Type != llm.EventTypeRetryWithFeedback {
				t.Errorf("provider-quirks: got event %q, want %q", got.Type, llm.EventTypeRetryWithFeedback)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("provider-quirks: no llm.retry_with_feedback event — the correction path did not fire")
		}
	})

	t.Run("quirk-recovered", func(t *testing.T) {
		// The driver returns ONE bad response then a valid one — the
		// retry budget (2) is enough to recover.
		quirk := newQuirkLLMDriver(1, badContent, goodContent)
		client := retry.Wrap(quirk, cfg, deps)
		defer func() { _ = client.Close(ctx) }()

		resp, cerr := client.Complete(idCtx, llm.CompleteRequest{
			Model:     quirkModel,
			Validator: validator,
		})
		// Recovery-path assertion: the retry loop recovered from the
		// quirk and returned the valid response.
		if cerr != nil {
			t.Fatalf("provider-quirks recovery: Complete = %v, want nil after one retry", cerr)
		}
		if resp.Content != goodContent {
			t.Errorf("provider-quirks recovery: Content = %q, want %q", resp.Content, goodContent)
		}
	})
}

// ---------------------------------------------------------------------
// 4. StateStore disconnect
// ---------------------------------------------------------------------

// injectStateStoreDisconnect opens a real in-mem StateStore through
// its production factory, wraps it in the fault-injecting decorator,
// arms a disconnect, and asserts: (a) Save/Load surface the transport
// error LOUDLY (no silent degradation — CLAUDE.md §13); (b) once the
// fault clears, the reconnect recovery path works — a subsequent
// Save/Load round-trips correctly.
func injectStateStoreDisconnect(t *testing.T) {
	t.Helper()
	ctx := context.Background()

	// Real in-mem StateStore through its production registry factory.
	real, err := state.Open(ctx, config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state.Open(inmem): %v", err)
	}
	// Thin fault-injecting decorator over the real store — it
	// decorates, it does not replace (D-137).
	store := newFaultyStateStore(real)
	defer func() { _ = store.Close(ctx) }()

	id := chaosIdentity()
	quad := identity.Quadruple{Identity: id}
	rec := state.StateRecord{
		ID:       state.NewEventID(),
		Identity: quad,
		Kind:     "phase78.chaos.statestore",
		Bytes:    []byte("phase78-pre-disconnect"),
	}

	// Sanity: a clean write succeeds before any fault is armed.
	if err := store.Save(ctx, rec); err != nil {
		t.Fatalf("statestore-disconnect: pre-fault Save failed: %v", err)
	}

	// Arm a 2-call disconnect: the next Save AND the next Load fault.
	store.armDisconnect(2)

	// Loud-failure assertion: Save surfaces the transport error —
	// the failure is NEVER silently swallowed.
	saveErr := store.Save(ctx, state.StateRecord{
		ID:       state.NewEventID(),
		Identity: quad,
		Kind:     "phase78.chaos.statestore",
		Bytes:    []byte("phase78-during-disconnect"),
	})
	if !errors.Is(saveErr, errStateDisconnected) {
		t.Fatalf("statestore-disconnect: Save during disconnect = %v, want errStateDisconnected", saveErr)
	}

	// Loud-failure assertion: Load surfaces the transport error too.
	if _, loadErr := store.Load(ctx, quad, "phase78.chaos.statestore"); !errors.Is(loadErr, errStateDisconnected) {
		t.Fatalf("statestore-disconnect: Load during disconnect = %v, want errStateDisconnected", loadErr)
	}

	// Recovery-path assertion: the fault budget is now exhausted —
	// the store "reconnected". A subsequent Save + Load round-trips
	// correctly against the real underlying store.
	recovered := state.StateRecord{
		ID:       state.NewEventID(),
		Identity: quad,
		Kind:     "phase78.chaos.statestore",
		Bytes:    []byte("phase78-after-reconnect"),
	}
	if err := store.Save(ctx, recovered); err != nil {
		t.Fatalf("statestore-disconnect: Save after reconnect failed: %v", err)
	}
	got, err := store.Load(ctx, quad, "phase78.chaos.statestore")
	if err != nil {
		t.Fatalf("statestore-disconnect: Load after reconnect failed: %v", err)
	}
	if string(got.Bytes) != "phase78-after-reconnect" {
		t.Errorf("statestore-disconnect: Load after reconnect returned %q, want phase78-after-reconnect", got.Bytes)
	}
	if got.Identity.Identity != id {
		t.Errorf("statestore-disconnect: recovered record scoped to %v, want %v", got.Identity.Identity, id)
	}
}

// ---------------------------------------------------------------------
// 5. Force pause-deserialize failure
// ---------------------------------------------------------------------

// injectPauseDeserializeFailure exercises the D-069 / RFC §3.4
// fail-loud pause-serialisation contract. A PauseRequest whose
// trajectory carries a non-serialisable handle (a live channel in the
// LLMContext map) MUST fail Coordinator.Request loudly with
// trajectory.ErrUnserializable naming the offending field path — never
// a half-persisted checkpoint, never a silent (nil, nil).
//
// It also asserts the recovery half: a well-formed pause WITHOUT the
// non-serialisable handle Requests cleanly — the contract rejects the
// bad input, it does not break the coordinator.
func injectPauseDeserializeFailure(t *testing.T) {
	t.Helper()
	ctx := context.Background()

	// Real StateStore-backed Coordinator — the checkpoint path is the
	// one that serialises the trajectory, so a StateStore-backed
	// Coordinator exercises the full fail-loud serialise contract.
	store, err := state.Open(ctx, config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state.Open(inmem): %v", err)
	}
	defer func() { _ = store.Close(ctx) }()

	coord := pauseresume.New(pauseresume.WithCheckpointStore(store))
	id := chaosIdentity()
	// Coordinator.Resume reads the resuming identity from ctx — the
	// pause-record scope check fails closed without it.
	idCtx, err := identity.With(ctx, id)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}

	// A trajectory whose LLMContext carries a live channel — channels
	// are not JSON-encodable, so the fail-loud serialise walker MUST
	// reject it.
	badChan := make(chan int)
	defer close(badChan)
	badTraj := &trajectory.Trajectory{
		Query: "phase78-chaos-pause",
		LLMContext: map[string]any{
			"phase78_non_serialisable_handle": badChan,
		},
	}

	// Loud-failure assertion: Request fails with
	// trajectory.ErrUnserializable — never a half-persisted pause.
	_, reqErr := coord.Request(ctx, pauseresume.PauseRequest{
		Identity:   id,
		Reason:     pauseresume.ReasonApprovalRequired,
		Trajectory: badTraj,
	})
	var unserr trajectory.ErrUnserializable
	if !errors.As(reqErr, &unserr) {
		t.Fatalf("pause-deserialize-failure: Request with a non-serialisable trajectory = %v, want trajectory.ErrUnserializable", reqErr)
	}
	// The error must be actionable — a non-empty field path naming
	// the offending leaf (RFC §3.4 "MUST return ErrUnserializable
	// naming the offending field path").
	if unserr.Field == "" {
		t.Error("pause-deserialize-failure: ErrUnserializable has an empty Field path — not actionable")
	}

	// Recovery-path assertion: a well-formed pause (no non-
	// serialisable handle) Requests cleanly — the fail-loud contract
	// rejects the bad input, it does not break the Coordinator. The
	// recovered pause is then resumable.
	goodTraj := &trajectory.Trajectory{
		Query: "phase78-chaos-pause-recovered",
		LLMContext: map[string]any{
			"note": "fully JSON-encodable",
		},
	}
	pause, err := coord.Request(idCtx, pauseresume.PauseRequest{
		Identity:   id,
		Reason:     pauseresume.ReasonApprovalRequired,
		Trajectory: goodTraj,
	})
	if err != nil {
		t.Fatalf("pause-deserialize-failure: Request with a clean trajectory failed: %v", err)
	}
	if pause.Token == "" {
		t.Fatal("pause-deserialize-failure: clean Request returned an empty Token")
	}
	if err := coord.Resume(idCtx, pause.Token, pauseresume.DecisionApprove, nil); err != nil {
		t.Errorf("pause-deserialize-failure: Resume of the recovered pause failed: %v", err)
	}
}
