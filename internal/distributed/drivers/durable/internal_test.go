package durable

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	eventsinmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/state"
)

// fakeStore is a minimal state.StateStore for exercising the poller's
// error branches deterministically. ListKind returns the configured
// records / error.
type fakeStore struct {
	listRecs []state.StateRecord
	listErr  error
}

func (f *fakeStore) Save(context.Context, state.StateRecord) error { return nil }
func (f *fakeStore) SaveIf(context.Context, []state.SlotExpectation, state.StateRecord) error {
	return nil
}
func (f *fakeStore) SaveBatchIf(context.Context, []state.SlotExpectation, []state.StateRecord) error {
	return nil
}
func (f *fakeStore) FenceIf(_ context.Context, _ state.SlotExpectation, fn func() error) error {
	return fn()
}
func (f *fakeStore) DeleteIf(context.Context, state.SlotExpectation) (bool, error) {
	return false, nil
}
func (f *fakeStore) Load(context.Context, identity.Quadruple, string) (state.StateRecord, error) {
	return state.StateRecord{}, state.ErrNotFound
}
func (f *fakeStore) LoadByEventID(context.Context, state.EventID) (state.StateRecord, error) {
	return state.StateRecord{}, state.ErrNotFound
}
func (f *fakeStore) Delete(context.Context, identity.Quadruple, string) error { return nil }
func (f *fakeStore) DeleteScope(context.Context, identity.Identity) (int, error) {
	return 0, nil
}
func (f *fakeStore) ListKind(context.Context, state.ListScope, string) ([]state.StateRecord, error) {
	return f.listRecs, f.listErr
}
func (f *fakeStore) ListKindBounded(_ context.Context, _ state.ListScope, _ string, limit int) ([]state.StateRecord, error) {
	if len(f.listRecs) > limit {
		return f.listRecs[:limit], f.listErr
	}
	return f.listRecs, f.listErr
}
func (f *fakeStore) ListKindForIdentity(context.Context, identity.Quadruple, string) ([]state.StateRecord, error) {
	return f.listRecs, f.listErr
}
func (f *fakeStore) ListKindForIdentityBounded(_ context.Context, _ identity.Quadruple, _ string, limit int) ([]state.StateRecord, error) {
	if len(f.listRecs) > limit {
		return f.listRecs[:limit], f.listErr
	}
	return f.listRecs, f.listErr
}
func (f *fakeStore) ScanKindForTenant(context.Context, state.ListScope, string, string, int, string) (state.StateScanPage, error) {
	return state.StateScanPage{Records: f.listRecs}, f.listErr
}
func (f *fakeStore) Close(context.Context) error { return nil }

func mkInmemBus(t *testing.T) events.EventBus {
	t.Helper()
	eb, err := eventsinmem.New(config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     256,
		IdleTimeout:              60 * time.Second,
		DropWindow:               time.Second,
		ReplayBufferSize:         1024,
	}, auditpatterns.New())
	if err != nil {
		t.Fatalf("events inmem New: %v", err)
	}
	return eb
}

// newBusNoPoller builds a *bus without starting the poll loop, so a
// test can drive pollOnce directly and deterministically.
func newBusNoPoller(store state.StateStore, eb events.EventBus) *bus {
	pollCtx, pollCancel := context.WithCancel(context.Background())
	return &bus{
		store:      store,
		eventBus:   eb,
		interval:   time.Hour,
		projected:  make(map[string]struct{}),
		logger:     slog.Default(),
		pollCtx:    pollCtx,
		pollCancel: pollCancel,
	}
}

// TestPollOnce_ListKindError covers the poller's ListKind-error branch:
// it logs and returns without panicking.
func TestPollOnce_ListKindError(t *testing.T) {
	eb := mkInmemBus(t)
	defer func() { _ = eb.Close(context.Background()) }()
	b := newBusNoPoller(&fakeStore{listErr: errors.New("scan failed")}, eb)
	b.pollOnce(context.Background()) // must not panic
}

// TestPollOnce_CorruptEntry covers the decode-error branch: a corrupt
// entry is marked seen + skipped, never projected, never retried.
func TestPollOnce_CorruptEntry(t *testing.T) {
	eb := mkInmemBus(t)
	defer func() { _ = eb.Close(context.Background()) }()
	corrupt := state.StateRecord{
		ID:       state.NewEventID(),
		Identity: identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}},
		Kind:     entryKindPrefix + "BADENTRY",
		Bytes:    []byte("{not valid json"),
	}
	store := &fakeStore{listRecs: []state.StateRecord{corrupt}}
	b := newBusNoPoller(store, eb)

	b.pollOnce(context.Background())
	if _, seen := b.projected[corrupt.Kind]; !seen {
		t.Error("corrupt entry should be marked projected (so it is not retried forever)")
	}
}

// TestDecodeEnvelope_Bad covers decodeEnvelope's unmarshal-error path.
func TestDecodeEnvelope_Bad(t *testing.T) {
	if _, err := decodeEnvelope([]byte("{not valid json")); err == nil {
		t.Error("decodeEnvelope of bad bytes: want error, got nil")
	}
}
