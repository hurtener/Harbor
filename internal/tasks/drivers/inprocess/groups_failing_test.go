package inprocess_test

import (
	"context"
	"errors"
	"testing"
	"time"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	eventsinmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/state"
	stateinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	"github.com/hurtener/Harbor/internal/tasks"
)

// failingStateStore wraps a real StateStore but returns a configurable
// error from Save. Used to exercise the persistGroup / persistPatch
// error paths in the inprocess driver.
//
// Save fails when `failOnKind` matches the record's Kind (empty
// string = fail on every Save). Other methods passthrough.
type failingStateStore struct {
	inner      state.StateStore
	saveErr    error
	failOnKind string
}

func (f *failingStateStore) Save(ctx context.Context, rec state.StateRecord) error {
	if f.saveErr != nil && (f.failOnKind == "" || rec.Kind == f.failOnKind) {
		return f.saveErr
	}
	return f.inner.Save(ctx, rec)
}

func (f *failingStateStore) Load(ctx context.Context, id identity.Quadruple, kind string) (state.StateRecord, error) {
	return f.inner.Load(ctx, id, kind)
}

func (f *failingStateStore) LoadByEventID(ctx context.Context, eventID state.EventID) (state.StateRecord, error) {
	return f.inner.LoadByEventID(ctx, eventID)
}

func (f *failingStateStore) Delete(ctx context.Context, id identity.Quadruple, kind string) error {
	return f.inner.Delete(ctx, id, kind)
}

func (f *failingStateStore) Close(ctx context.Context) error {
	return f.inner.Close(ctx)
}

var _ state.StateStore = (*failingStateStore)(nil)

// buildWithFailingStore constructs a TaskRegistry whose StateStore
// returns errSave on Save calls matching failOnKind.
func buildWithFailingStore(t *testing.T, errSave error, failOnKind string) (tasks.TaskRegistry, *failingStateStore, func()) {
	t.Helper()
	innerStore, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state inmem New: %v", err)
	}
	store := &failingStateStore{inner: innerStore, saveErr: errSave, failOnKind: failOnKind}
	redactor := auditpatterns.New()
	bus, err := eventsinmem.New(config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     256,
		IdleTimeout:              60 * time.Second,
		DropWindow:               1 * time.Second,
		ReplayBufferSize:         1024,
	}, redactor)
	if err != nil {
		t.Fatalf("events inmem New: %v", err)
	}
	r, err := tasks.OpenDriver("inprocess", tasks.Dependencies{
		Store:    store,
		Bus:      bus,
		Redactor: redactor,
		Cfg:      config.TasksConfig{Driver: "inprocess"},
	})
	if err != nil {
		t.Fatalf("OpenDriver: %v", err)
	}
	return r, store, func() {
		ctx := context.Background()
		_ = r.Close(ctx)
		_ = bus.Close(ctx)
		_ = innerStore.Close(ctx)
	}
}

// TestResolveOrCreateGroup_PersistError covers the persistGroup error
// path when StateStore.Save fails.
func TestResolveOrCreateGroup_PersistError(t *testing.T) {
	r, _, cleanup := buildWithFailingStore(t, errors.New("disk full"), tasks.GroupKind)
	defer cleanup()
	ctx := ctxA(t)
	_, err := r.ResolveOrCreateGroup(ctx, tasks.GroupRequest{
		SessionID: tripleA().Identity,
	})
	if err == nil {
		t.Fatal("ResolveOrCreateGroup with failing store: err=nil, want non-nil")
	}
}

// TestCreatePendingPatch_PersistError covers the persistPatch error
// path.
func TestCreatePendingPatch_PersistError(t *testing.T) {
	r, _, cleanup := buildWithFailingStore(t, errors.New("disk full"), tasks.PatchKind)
	defer cleanup()
	ctx := ctxA(t)
	type seeder interface {
		CreatePendingPatch(ctx context.Context, sessionID identity.Identity, patchID string, payload []byte) (*tasks.Patch, error)
	}
	s, ok := r.(seeder)
	if !ok {
		t.Skip("driver does not expose CreatePendingPatch")
	}
	_, err := s.CreatePendingPatch(ctx, tripleA().Identity, "p-fail", []byte(`{}`))
	if err == nil {
		t.Fatal("CreatePendingPatch with failing store: err=nil")
	}
}

// TestApplyPatch_PersistError covers persistPatch failure on the
// pending → applied transition.
func TestApplyPatch_PersistError(t *testing.T) {
	r, store, cleanup := buildWithFailingStore(t, nil, tasks.PatchKind)
	defer cleanup()
	ctx := ctxA(t)
	type seeder interface {
		CreatePendingPatch(ctx context.Context, sessionID identity.Identity, patchID string, payload []byte) (*tasks.Patch, error)
	}
	s, ok := r.(seeder)
	if !ok {
		t.Skip("driver does not expose CreatePendingPatch")
	}
	// First the seed call succeeds (no Save error configured yet).
	if _, err := s.CreatePendingPatch(ctx, tripleA().Identity, "p-x", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
	// Now toggle Save to fail on the next patch save.
	store.saveErr = errors.New("save failed")
	_, err := r.ApplyPatch(ctx, tripleA().Identity, "p-x", tasks.PatchAccept)
	if err == nil {
		t.Fatal("ApplyPatch with mid-transition failing store: err=nil")
	}
}

// TestSealGroup_PersistError covers persistGroup failure on the
// SealGroup transition.
func TestSealGroup_PersistError(t *testing.T) {
	r, store, cleanup := buildWithFailingStore(t, nil, tasks.GroupKind)
	defer cleanup()
	ctx := ctxA(t)
	g, err := r.ResolveOrCreateGroup(ctx, tasks.GroupRequest{
		SessionID: tripleA().Identity,
	})
	if err != nil {
		t.Fatal(err)
	}
	store.saveErr = errors.New("save failed")
	err = r.SealGroup(ctx, g.ID)
	if err == nil {
		t.Fatal("SealGroup with failing store: err=nil")
	}
}
