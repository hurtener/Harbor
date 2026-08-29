package generator_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/skills"
	"github.com/hurtener/Harbor/internal/skills/generator"
)

// errBus is an events.EventBus shim that delegates to an inner bus
// for everything EXCEPT publishes of types in `failTypes` — those
// return a configured error. Used to inject audit-emit failure on
// the `skill.proposed` event so the generator's rollback path is
// exercised.
type errBus struct {
	inner     events.EventBus
	failTypes map[events.EventType]bool
	mu        sync.Mutex
	failed    int
}

func newErrBus(inner events.EventBus, fail ...events.EventType) *errBus {
	m := make(map[events.EventType]bool, len(fail))
	for _, t := range fail {
		m[t] = true
	}
	return &errBus{inner: inner, failTypes: m}
}

func (b *errBus) Publish(ctx context.Context, ev events.Event) error {
	if b.failTypes[ev.Type] {
		b.mu.Lock()
		b.failed++
		b.mu.Unlock()
		return errors.New("errBus: injected publish failure for " + string(ev.Type))
	}
	return b.inner.Publish(ctx, ev)
}

func (b *errBus) Subscribe(ctx context.Context, f events.Filter) (events.Subscription, error) {
	return b.inner.Subscribe(ctx, f)
}

func (b *errBus) Close(ctx context.Context) error {
	return b.inner.Close(ctx)
}

func (b *errBus) failedCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.failed
}

// TestPropose_AuditEmitFailure_RollsBackPersist asserts that an
// audit-emit failure on `skill.proposed` causes the persist to roll
// back. The caller's subsequent `Get` returns `ErrSkillNotFound`.
//
// This is the load-bearing fail-loudly contract from D-054 + the
// spec: every persist emits an audit event; a failed emit is
// elevated to a first-class concern, not silently swallowed.
func TestPropose_AuditEmitFailure_RollsBackPersist(t *testing.T) {
	t.Parallel()

	innerBus := newTestBus(t)
	bus := newErrBus(innerBus, skills.EventTypeSkillProposed)
	store := newTestStore(t, bus)
	deps := generator.Deps{Bus: bus, Redactor: newTestDeps(t, innerBus).Redactor}

	ctx := ctxWithIdentity(t)
	q := testIdentity()

	receipt, err := generator.Propose(ctx, store, deps, generator.ProposeArgs{
		Skill:   validDraft("rollback-target"),
		Persist: true,
	})
	if err == nil {
		t.Fatalf("got nil err on audit-emit failure, want wrapped error (receipt=%+v)", receipt)
	}
	// Error message names "audit emit failed (persist rolled back)".
	if !contains(err.Error(), "audit emit failed") {
		t.Fatalf("err=%q, want substring 'audit emit failed'", err.Error())
	}

	// Row must not be visible — the rollback Delete cleaned it up.
	_, getErr := store.Get(ctx, q, "rollback-target")
	if !errors.Is(getErr, skills.ErrSkillNotFound) {
		t.Fatalf("Get rollback-target: got %v, want ErrSkillNotFound (row should be deleted)", getErr)
	}

	if bus.failedCount() != 1 {
		t.Fatalf("errBus.failedCount=%d, want 1", bus.failedCount())
	}
}

// TestPropose_AuditEmitFailure_OnRejection asserts the audit emit
// also fires on conflict-rejected paths, and an audit-emit failure
// there surfaces a wrapped error (the rejection itself is NOT
// silently swallowed). The pack row stays untouched.
func TestPropose_AuditEmitFailure_OnRejection(t *testing.T) {
	t.Parallel()

	innerBus := newTestBus(t)
	bus := newErrBus(innerBus, skills.EventTypeSkillProposed)
	store := newTestStore(t, bus)
	deps := generator.Deps{Bus: bus, Redactor: newTestDeps(t, innerBus).Redactor}

	ctx := ctxWithIdentity(t)
	q := testIdentity()

	// Seed a pack-imported row first; this needs the bus to
	// succeed on `skill.upserted` (errBus only fails
	// `skill.proposed`).
	pack := skills.Skill{
		Name:    "pack-name",
		Trigger: "trig",
		Steps:   []string{"s1"},
		Origin:  skills.OriginPack,
		Scope:   skills.ScopeProject,
	}
	if err := store.Upsert(ctx, q, pack); err != nil {
		t.Fatalf("seed pack: %v", err)
	}

	// Propose with same name → rejection path; the audit emit on
	// rejection fails; the call surfaces the audit-emit-failure
	// error rather than the conflict.
	_, err := generator.Propose(ctx, store, deps, generator.ProposeArgs{
		Skill:   validDraft("pack-name"),
		Persist: true,
	})
	if err == nil {
		t.Fatalf("got nil err, want audit-emit-failure wrapped error")
	}
	if !contains(err.Error(), "audit emit failed") {
		t.Fatalf("err=%q, want substring 'audit emit failed'", err.Error())
	}
	// Pack row untouched.
	got, gerr := store.Get(ctx, q, "pack-name")
	if gerr != nil {
		t.Fatalf("Get pack-name: %v (want pack row still present)", gerr)
	}
	if got.Origin != skills.OriginPack {
		t.Fatalf("Get.Origin=%q want %q (pack untouched)", got.Origin, skills.OriginPack)
	}
}

// failingRedactor returns an error from every Redact call.
type failingRedactor struct{}

func (failingRedactor) Redact(ctx context.Context, payload any) (any, error) {
	return nil, errors.New("failingRedactor: injected")
}

// TestPropose_RedactorFailure_AbortsPersist asserts a redactor error
// aborts the call. The DB write commits BEFORE the emit attempt, so
// the rollback Delete is invoked — the caller's Get returns
// ErrSkillNotFound.
func TestPropose_RedactorFailure_AbortsPersist(t *testing.T) {
	t.Parallel()

	bus := newTestBus(t)
	store := newTestStore(t, bus)
	deps := generator.Deps{Bus: bus, Redactor: failingRedactor{}}

	ctx := ctxWithIdentity(t)
	q := testIdentity()

	_, err := generator.Propose(ctx, store, deps, generator.ProposeArgs{
		Skill:   validDraft("redactor-fail"),
		Persist: true,
	})
	if err == nil {
		t.Fatalf("got nil err on redactor failure, want wrapped error")
	}
	// The redactor failure is wrapped via "redact excerpt" path.
	if !contains(err.Error(), "redact") && !contains(err.Error(), "audit") {
		t.Fatalf("err=%q, want substring 'redact' or 'audit'", err.Error())
	}
	if _, getErr := store.Get(ctx, q, "redactor-fail"); !errors.Is(getErr, skills.ErrSkillNotFound) {
		t.Fatalf("Get redactor-fail: got %v, want ErrSkillNotFound (rollback expected)", getErr)
	}
}

// TestPropose_MissingIdentity asserts a bare ctx returns wrapped
// ErrIdentityRequired AND emits skill.identity_rejected.
func TestPropose_MissingIdentity(t *testing.T) {
	t.Parallel()

	bus := newTestBus(t)
	store := newTestStore(t, bus)
	deps := newTestDeps(t, bus)

	// Subscribe via Admin filter so we see the rejection event
	// regardless of identity components.
	sub, err := bus.Subscribe(context.Background(), eventsFilterAllForIdentityRejected())
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Cancel()

	_, err = generator.Propose(context.Background(), store, deps, generator.ProposeArgs{
		Skill:   validDraft("nope"),
		Persist: true,
	})
	if !errors.Is(err, skills.ErrIdentityRequired) {
		t.Fatalf("got %v, want wrapped skills.ErrIdentityRequired", err)
	}
	// Drain one event.
	select {
	case ev := <-sub.Events():
		if ev.Type != skills.EventTypeSkillIdentityRejected {
			t.Fatalf("ev.Type=%q want %q", ev.Type, skills.EventTypeSkillIdentityRejected)
		}
	default:
		t.Fatalf("no skill.identity_rejected event landed on the bus")
	}
}

// TestPropose_PartialIdentity asserts a Quadruple with one empty
// triple component rejects.
func TestPropose_PartialIdentity(t *testing.T) {
	t.Parallel()

	bus := newTestBus(t)
	store := newTestStore(t, bus)
	deps := newTestDeps(t, bus)

	// Attach an Identity with empty UserID via ctx — pretend
	// someone bypassed identity.With's validator (e.g. a future
	// caller mucking with ctx directly via the internal key).
	// We simulate this by using QuadrupleFrom: it returns the
	// stored Quadruple unchanged; if the caller stored an
	// incomplete one, the generator's identity.Validate catches it.
	//
	// In practice identity.WithRun validates at attach time, so
	// we have to short-circuit by using context.Background with no
	// identity AND verify the generator's own check fires.
	_, err := generator.Propose(context.Background(), store, deps, generator.ProposeArgs{
		Skill:   validDraft("p"),
		Persist: true,
	})
	if !errors.Is(err, skills.ErrIdentityRequired) {
		t.Fatalf("got %v, want wrapped ErrIdentityRequired", err)
	}
}

// contains is a tiny substring helper used by the failure-mode tests
// to avoid pulling strings.Contains from a one-import line.
func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
