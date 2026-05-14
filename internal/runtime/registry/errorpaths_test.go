package registry_test

import (
	"context"
	"errors"
	"testing"
	"time"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	eventsinmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/runtime/registry"
	"github.com/hurtener/Harbor/internal/state"
	stateinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
)

// errInjected is the sentinel a faultStore returns from a faulted op.
var errInjected = errors.New("injected store fault")

// faultStore wraps a real StateStore and injects a failure on the
// first Save / Load / Delete matching faultKind. Used to exercise the
// registry's storage-error branches (which the happy-path tests do not
// reach). It is intentionally minimal — one fault, then it behaves
// normally — so a test can register cleanly then fault a follow-up op.
type faultStore struct {
	state.StateStore
	faultSave   bool
	faultLoad   bool
	faultDelete bool
}

func (f *faultStore) Save(ctx context.Context, r state.StateRecord) error {
	if f.faultSave {
		f.faultSave = false
		return errInjected
	}
	return f.StateStore.Save(ctx, r)
}

func (f *faultStore) Load(ctx context.Context, id identity.Quadruple, kind string) (state.StateRecord, error) {
	if f.faultLoad {
		f.faultLoad = false
		return state.StateRecord{}, errInjected
	}
	return f.StateStore.Load(ctx, id, kind)
}

func (f *faultStore) Delete(ctx context.Context, id identity.Quadruple, kind string) error {
	if f.faultDelete {
		f.faultDelete = false
		return errInjected
	}
	return f.StateStore.Delete(ctx, id, kind)
}

func newFaultRegistry(t *testing.T) (*registry.Registry, *faultStore) {
	t.Helper()
	inner, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state inmem.New: %v", err)
	}
	fs := &faultStore{StateStore: inner}
	bus, err := eventsinmem.New(testEventsCfg(), auditpatterns.New())
	if err != nil {
		t.Fatalf("events inmem.New: %v", err)
	}
	reg, err := registry.New(registry.Deps{Store: fs, Bus: bus, Redactor: auditpatterns.New()})
	if err != nil {
		t.Fatalf("registry.New: %v", err)
	}
	t.Cleanup(func() {
		_ = reg.Close(context.Background())
		_ = bus.Close(context.Background())
		_ = inner.Close(context.Background())
	})
	return reg, fs
}

// TestRegister_EmptyKeyRejected exercises the empty-registration-key
// branch of register().
func TestRegister_EmptyKeyRejected(t *testing.T) {
	reg, _, _ := newTestRegistry(t)
	ctx := identityCtx(t, "T", "U", "S")
	if _, err := reg.Register(ctx, "", sampleConfig(), registry.RegisterOptions{}); !errors.Is(err, registry.ErrInvalidConfig) {
		t.Fatalf("Register accepted an empty key: %v", err)
	}
	if _, err := reg.RegisterRemote(ctx, "", "ref", registry.RegisterOptions{}); !errors.Is(err, registry.ErrInvalidConfig) {
		t.Fatalf("RegisterRemote accepted an empty key: %v", err)
	}
}

// TestRegister_StoreSaveFault exercises the saveRecord / saveIndex
// error branch in the first-registration path.
func TestRegister_StoreSaveFault(t *testing.T) {
	reg, fs := newFaultRegistry(t)
	ctx := identityCtx(t, "T", "U", "S")
	fs.faultSave = true
	if _, err := reg.Register(ctx, "agent", sampleConfig(), registry.RegisterOptions{}); !errors.Is(err, errInjected) {
		t.Fatalf("Register did not surface the store Save fault: %v", err)
	}
}

// TestRegister_IndexLoadFault exercises the loadIndex error branch.
func TestRegister_IndexLoadFault(t *testing.T) {
	reg, fs := newFaultRegistry(t)
	ctx := identityCtx(t, "T", "U", "S")
	// Register once so an index document exists, then fault its Load.
	if _, err := reg.Register(ctx, "agent", sampleConfig(), registry.RegisterOptions{}); err != nil {
		t.Fatalf("Register #1: %v", err)
	}
	fs.faultLoad = true
	if _, err := reg.Register(ctx, "agent-2", sampleConfig(), registry.RegisterOptions{}); !errors.Is(err, errInjected) {
		t.Fatalf("Register did not surface the index Load fault: %v", err)
	}
}

// TestGet_StoreLoadFault exercises loadRecord's non-NotFound error
// branch.
func TestGet_StoreLoadFault(t *testing.T) {
	reg, fs := newFaultRegistry(t)
	ctx := identityCtx(t, "T", "U", "S")
	rec, err := reg.Register(ctx, "agent", sampleConfig(), registry.RegisterOptions{})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	fs.faultLoad = true
	if _, err := reg.Get(ctx, rec.AgentID); !errors.Is(err, errInjected) {
		t.Fatalf("Get did not surface the store Load fault: %v", err)
	}
}

// TestDeregister_DeleteFault exercises the Delete error branch.
func TestDeregister_DeleteFault(t *testing.T) {
	reg, fs := newFaultRegistry(t)
	ctx := identityCtx(t, "T", "U", "S")
	rec, err := reg.Register(ctx, "agent", sampleConfig(), registry.RegisterOptions{})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	fs.faultDelete = true
	if err := reg.Deregister(ctx, rec.AgentID); !errors.Is(err, errInjected) {
		t.Fatalf("Deregister did not surface the store Delete fault: %v", err)
	}
}

// TestReportHealth_SaveFault exercises ReportHealth's saveRecord error
// branch.
func TestReportHealth_SaveFault(t *testing.T) {
	reg, fs := newFaultRegistry(t)
	ctx := identityCtx(t, "T", "U", "S")
	rec, err := reg.Register(ctx, "agent", sampleConfig(), registry.RegisterOptions{})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	fs.faultSave = true
	if err := reg.ReportHealth(ctx, rec.AgentID, registry.HealthHealthy); !errors.Is(err, errInjected) {
		t.Fatalf("ReportHealth did not surface the store Save fault: %v", err)
	}
}

// TestControl_SaveFault exercises the control() saveRecord error branch
// (Drain transitions health → saves → faults).
func TestControl_SaveFault(t *testing.T) {
	reg, fs := newFaultRegistry(t)
	ctx := identityCtx(t, "T", "U", "S")
	rec, err := reg.Register(ctx, "agent", sampleConfig(), registry.RegisterOptions{})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	fs.faultSave = true
	ctrl := registry.WithControlScope(ctx)
	if err := reg.Drain(ctrl, rec.AgentID, "deploy"); !errors.Is(err, errInjected) {
		t.Fatalf("Drain did not surface the store Save fault: %v", err)
	}
}

// TestControl_RedactorFault exercises the control() redact-reason error
// branch — a redactor that refuses the reason string must fail the
// command loudly (D-020 fail-loudly).
func TestControl_RedactorFault(t *testing.T) {
	inner, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state inmem.New: %v", err)
	}
	bus, err := eventsinmem.New(testEventsCfg(), auditpatterns.New())
	if err != nil {
		t.Fatalf("events inmem.New: %v", err)
	}
	reg, err := registry.New(registry.Deps{Store: inner, Bus: bus, Redactor: faultRedactor{}})
	if err != nil {
		t.Fatalf("registry.New: %v", err)
	}
	t.Cleanup(func() {
		_ = reg.Close(context.Background())
		_ = bus.Close(context.Background())
		_ = inner.Close(context.Background())
	})
	ctx := identityCtx(t, "T", "U", "S")
	rec, err := reg.Register(ctx, "agent", sampleConfig(), registry.RegisterOptions{})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	ctrl := registry.WithControlScope(ctx)
	if err := reg.Pause(ctrl, rec.AgentID, "some reason"); !errors.Is(err, errInjected) {
		t.Fatalf("Pause did not surface the redactor fault: %v", err)
	}
}

// faultRedactor always refuses — exercises the registry's fail-loudly
// path when audit redaction fails.
type faultRedactor struct{}

func (faultRedactor) Redact(_ context.Context, _ any) (any, error) {
	return nil, errInjected
}

// TestWithClock_UsedByRegistry exercises the WithClock option path and
// confirms the injected clock drives RegisteredAt.
func TestWithClock_UsedByRegistry(t *testing.T) {
	inner, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state inmem.New: %v", err)
	}
	bus, err := eventsinmem.New(testEventsCfg(), auditpatterns.New())
	if err != nil {
		t.Fatalf("events inmem.New: %v", err)
	}
	fixed := time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC)
	reg, err := registry.New(
		registry.Deps{Store: inner, Bus: bus, Redactor: auditpatterns.New()},
		registry.WithClock(fixedClock{t: fixed}),
	)
	if err != nil {
		t.Fatalf("registry.New: %v", err)
	}
	t.Cleanup(func() {
		_ = reg.Close(context.Background())
		_ = bus.Close(context.Background())
		_ = inner.Close(context.Background())
	})
	ctx := identityCtx(t, "T", "U", "S")
	rec, err := reg.Register(ctx, "agent", sampleConfig(), registry.RegisterOptions{})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if !rec.RegisteredAt.Equal(fixed) {
		t.Errorf("RegisteredAt = %v, want injected clock value %v", rec.RegisteredAt, fixed)
	}
}

type fixedClock struct{ t time.Time }

func (c fixedClock) Now() time.Time { return c.t }

// compile-time: faultStore satisfies state.StateStore.
var _ state.StateStore = (*faultStore)(nil)

// compile-time: ensure events import is used even if a refactor drops
// the only reference (keeps the import meaningful for future tests).
var _ = events.EventTypeRuntimeError
