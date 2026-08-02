package sessions_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/sessions"
	"github.com/hurtener/Harbor/internal/state"
)

func TestRegistry_NewMissingDependenciesFailsLoud(t *testing.T) {
	t.Parallel()
	if _, err := sessions.New(nil, config.SessionsConfig{}, nil); err == nil {
		t.Fatal("New with nil StateStore returned nil error")
	}

	_, store, _ := titleTestWiring(t)
	if _, err := sessions.New(store, config.SessionsConfig{}, nil); err == nil {
		t.Fatal("New with nil EventBus returned nil error")
	}
}

func TestErasureFenceSlots_StayOutsideErasedScope(t *testing.T) {
	t.Parallel()
	id := identity.Quadruple{Identity: ident("tenant-a", "user-a", "session-a"), RunID: "run-a"}

	pendingScope, pendingKind, err := sessions.ErasurePendingSlot(id)
	if err != nil {
		t.Fatalf("ErasurePendingSlot: %v", err)
	}
	tombstoneScope, tombstoneKind, err := sessions.ErasureTombstoneSlot(id)
	if err != nil {
		t.Fatalf("ErasureTombstoneSlot: %v", err)
	}

	for name, scope := range map[string]identity.Quadruple{
		"pending": pendingScope, "tombstone": tombstoneScope,
	} {
		if scope.TenantID != id.TenantID || scope.UserID != id.UserID {
			t.Errorf("%s scope owner = (%q,%q), want (%q,%q)", name, scope.TenantID, scope.UserID, id.TenantID, id.UserID)
		}
		if scope.SessionID == id.SessionID || scope.RunID != "" {
			t.Errorf("%s scope = %+v, must survive erased session/run scope", name, scope)
		}
	}
	if !strings.Contains(pendingKind, id.SessionID) || !strings.Contains(tombstoneKind, id.SessionID) {
		t.Fatalf("fence kinds do not bind session id: pending=%q tombstone=%q", pendingKind, tombstoneKind)
	}
	if pendingKind == tombstoneKind {
		t.Fatalf("pending and terminal fence kinds alias: %q", pendingKind)
	}
}

func TestCascadeEraser_IncompleteIdentityFailsBeforeStores(t *testing.T) {
	t.Parallel()
	f := newErasureFixture(t, nil)

	_, err := f.eraser.Erase(context.Background(), identity.Identity{})
	if !errors.Is(err, identity.ErrIdentityIncomplete) {
		t.Fatalf("Erase with incomplete identity = %v, want ErrIdentityIncomplete", err)
	}
}

func TestRegistry_IdentityGuardsFailClosed(t *testing.T) {
	t.Parallel()
	reg, _ := testWiring(t)
	id := ident("tenant-a", "user-a", "session-a")

	missingIdentity := []struct {
		name string
		call func() error
	}{
		{name: "get", call: func() error { _, err := reg.Get(context.Background(), id.SessionID); return err }},
		{name: "touch", call: func() error { return reg.Touch(context.Background(), id.SessionID) }},
		{name: "close", call: func() error { return reg.Close(context.Background(), id.SessionID, "operator") }},
		{name: "erase", call: func() error { return reg.Erase(context.Background(), id.SessionID) }},
		{name: "inspect", call: func() error { _, err := reg.Inspect(context.Background(), id.SessionID); return err }},
	}
	for _, tc := range missingIdentity {
		if err := tc.call(); !errors.Is(err, identity.ErrIdentityMissing) {
			t.Errorf("%s without identity = %v, want ErrIdentityMissing", tc.name, err)
		}
	}

	if _, err := reg.Open(ctxFor(id), "different-session", id); err == nil {
		t.Error("Open accepted an id different from identity.SessionID")
	}
	if _, err := reg.EnsureOpen(ctxFor(id), identity.Identity{}); err == nil {
		t.Error("EnsureOpen accepted an incomplete identity")
	}
	if _, err := reg.Get(ctxFor(id), "different-session"); err == nil {
		t.Error("Get accepted an id different from ctx SessionID")
	}
	if err := reg.Erase(ctxFor(id), "different-session"); err == nil {
		t.Error("Erase accepted an id different from ctx SessionID")
	}
	if _, err := reg.Inspect(ctxFor(id), "different-session"); err == nil {
		t.Error("Inspect accepted an id different from ctx SessionID")
	}
}

func TestRegistry_ClosedRejectsEveryLifecycleEntryPoint(t *testing.T) {
	t.Parallel()
	reg, _ := testWiring(t)
	id := ident("tenant-a", "user-a", "session-a")
	if err := reg.CloseRegistry(context.Background()); err != nil {
		t.Fatalf("CloseRegistry: %v", err)
	}

	cases := []struct {
		name string
		call func() error
	}{
		{name: "open", call: func() error { _, err := reg.Open(ctxFor(id), id.SessionID, id); return err }},
		{name: "ensure_open", call: func() error { _, err := reg.EnsureOpen(ctxFor(id), id); return err }},
		{name: "get", call: func() error { _, err := reg.Get(ctxFor(id), id.SessionID); return err }},
		{name: "touch", call: func() error { return reg.Touch(ctxFor(id), id.SessionID) }},
		{name: "close", call: func() error { return reg.Close(ctxFor(id), id.SessionID, "operator") }},
		{name: "erase", call: func() error { return reg.Erase(ctxFor(id), id.SessionID) }},
		{name: "inspect", call: func() error { _, err := reg.Inspect(ctxFor(id), id.SessionID); return err }},
		{name: "list", call: func() error {
			_, err := reg.ListSnapshots(context.Background(), sessions.SessionListFilter{})
			return err
		}},
	}
	for _, tc := range cases {
		if err := tc.call(); !errors.Is(err, sessions.ErrRegistryClosed) {
			t.Errorf("%s after CloseRegistry = %v, want ErrRegistryClosed", tc.name, err)
		}
	}
}

func TestRegistry_ProbeFailuresRefuseInspectAndErase(t *testing.T) {
	t.Parallel()
	probeErr := errors.New("probe unavailable")
	probe := func(context.Context, identity.Quadruple) (bool, error) { return false, probeErr }
	reg, _ := testWiring(t, sessions.WithGCPolicy(sessions.GCPolicy{RunningProbe: probe}))
	id := ident("tenant-a", "user-a", "session-a")
	if _, err := reg.Open(ctxFor(id), id.SessionID, id); err != nil {
		t.Fatalf("Open: %v", err)
	}

	if _, err := reg.Inspect(ctxFor(id), id.SessionID); !errors.Is(err, probeErr) {
		t.Fatalf("Inspect probe failure = %v, want wrapped probe error", err)
	}
	if err := reg.Erase(ctxFor(id), id.SessionID); !errors.Is(err, probeErr) {
		t.Fatalf("Erase probe failure = %v, want wrapped probe error", err)
	}
	if _, err := reg.Get(ctxFor(id), id.SessionID); err != nil {
		t.Fatalf("probe failure mutated session: Get = %v", err)
	}
}

func TestRegistry_CanceledEnumerationFailsPromptly(t *testing.T) {
	t.Parallel()
	reg, _ := testWiring(t)
	id := ident("tenant-a", "user-a", "session-a")
	if _, err := reg.Open(ctxFor(id), id.SessionID, id); err != nil {
		t.Fatalf("Open: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := reg.ListSnapshots(ctx, sessions.SessionListFilter{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("ListSnapshots canceled = %v, want context.Canceled", err)
	}
	if _, _, err := reg.OldestRetainedAt(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("OldestRetainedAt canceled = %v, want context.Canceled", err)
	}
}

func TestRegistry_CorruptStoredIdentityRefusesMutation(t *testing.T) {
	t.Parallel()
	reg, store, _ := titleTestWiring(t)
	owner := ident("tenant-a", "user-a", "session-a")
	if _, err := reg.Open(ctxFor(owner), owner.SessionID, owner); err != nil {
		t.Fatalf("Open: %v", err)
	}

	poisoned := sessions.Session{ID: owner.SessionID, Identity: ident("tenant-b", "user-b", owner.SessionID)}
	payload, err := json.Marshal(poisoned)
	if err != nil {
		t.Fatalf("marshal poisoned session: %v", err)
	}
	if err := store.Save(context.Background(), state.StateRecord{
		ID: state.NewEventID(), Identity: identity.Quadruple{Identity: owner}, Kind: "session.lifecycle", Bytes: payload,
	}); err != nil {
		t.Fatalf("seed poisoned session: %v", err)
	}

	for name, call := range map[string]func() error{
		"touch": func() error { return reg.Touch(ctxFor(owner), owner.SessionID) },
		"close": func() error { return reg.Close(ctxFor(owner), owner.SessionID, "operator") },
		"erase": func() error { return reg.Erase(ctxFor(owner), owner.SessionID) },
	} {
		if err := call(); !errors.Is(err, sessions.ErrIdentityMismatch) {
			t.Errorf("%s corrupt stored identity = %v, want ErrIdentityMismatch", name, err)
		}
	}
}

func TestRegistry_CorruptStoredJSONFailsLoud(t *testing.T) {
	t.Parallel()
	reg, store, _ := titleTestWiring(t)
	id := ident("tenant-a", "user-a", "session-a")
	if _, err := reg.Open(ctxFor(id), id.SessionID, id); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := store.Save(context.Background(), state.StateRecord{
		ID: state.NewEventID(), Identity: identity.Quadruple{Identity: id}, Kind: "session.lifecycle", Bytes: []byte("{"),
	}); err != nil {
		t.Fatalf("seed corrupt session: %v", err)
	}
	if _, err := reg.Get(ctxFor(id), id.SessionID); err == nil || !strings.Contains(err.Error(), "unmarshal") {
		t.Fatalf("Get corrupt JSON = %v, want fail-loud unmarshal error", err)
	}
}
