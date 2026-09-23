// The administrative memory surface shares the execution owner's storage.
// Real drivers verify persistence, isolation, capacity refusal and identity
// events. Runtime compaction is exercised by the served/embedded cumulative
// acceptance suites; Put itself never runs inference or discards older notes.
package integration_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/audit"
	_ "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	_ "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/memory"
	_ "github.com/hurtener/Harbor/internal/memory/drivers/inmem"
	_ "github.com/hurtener/Harbor/internal/memory/drivers/sqlite"
	sessionmemory "github.com/hurtener/Harbor/internal/memory/session"
	"github.com/hurtener/Harbor/internal/state"
	_ "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	_ "github.com/hurtener/Harbor/internal/state/drivers/sqlite"
)

func phase24Memory(t *testing.T, backend, dsn string, turns int) (memory.MemoryStore, events.EventBus, func()) {
	t.Helper()
	ctx := context.Background()
	red, err := audit.Open(ctx, config.AuditConfig{})
	if err != nil {
		t.Fatal(err)
	}
	bus, err := events.Open(ctx, config.Defaults().Events, red)
	if err != nil {
		t.Fatal(err)
	}
	st, err := state.Open(ctx, config.StateConfig{Driver: backend, DSN: dsn})
	if err != nil {
		_ = bus.Close(ctx)
		t.Fatal(err)
	}
	mem, err := memory.Open(ctx, memory.ConfigSnapshot{
		Driver: backend, DSN: dsn, Strategy: memory.StrategyRollingSummary, RecentTurns: turns,
	}, memory.Deps{State: st, Bus: bus, Redactor: red, RetentionTTL: time.Hour})
	if err != nil {
		_ = st.Close(ctx)
		_ = bus.Close(ctx)
		t.Fatal(err)
	}
	closeAll := func() { _ = mem.Close(ctx); _ = st.Close(ctx); _ = bus.Close(ctx) }
	t.Cleanup(closeAll)
	return mem, bus, closeAll
}

func TestE2E_Phase24_CumulativeNotes_SurviveSQLiteRestart(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "memory.sqlite")
	mem, _, closeAll := phase24Memory(t, "sqlite", dsn, 20)
	id := identity.Quadruple{Identity: identity.Identity{TenantID: "T", UserID: "U", SessionID: "S"}}
	var keys []string
	for i := range 3 {
		key, err := mem.Put(t.Context(), id, memory.ConversationTurn{
			UserMessage:       fmt.Sprintf("Constraint %d: preserve exact version 9007199254740993 and more:false.", i),
			AssistantResponse: "Noted.",
		})
		if err != nil {
			t.Fatal(err)
		}
		keys = append(keys, key)
	}
	before, err := mem.Inspect(t.Context(), id)
	if err != nil || len(before.Items) != 3 {
		t.Fatalf("initial inspection: %+v, %v", before, err)
	}
	closeAll()
	reopened, _, _ := phase24Memory(t, "sqlite", dsn, 20)
	after, err := reopened.Inspect(t.Context(), id)
	if err != nil || len(after.Items) != 3 {
		t.Fatalf("restart inspection: %+v, %v", after, err)
	}
	for i, item := range after.Items {
		if item.Key != keys[i] || string(item.Value) != string(before.Items[i].Value) || !item.ExpiresAt.Equal(before.Items[i].ExpiresAt) {
			t.Fatalf("restart changed source %d or renewed its retention", i)
		}
	}
	for _, foreign := range []identity.Identity{
		{TenantID: "other", UserID: "U", SessionID: "S"},
		{TenantID: "T", UserID: "other", SessionID: "S"},
		{TenantID: "T", UserID: "U", SessionID: "other"},
	} {
		q := identity.Quadruple{Identity: foreign}
		view, err := reopened.Inspect(t.Context(), q)
		if err != nil || len(view.Items) != 0 {
			t.Fatalf("foreign read: %+v, %v", view, err)
		}
		if _, err := reopened.Delete(t.Context(), q, keys[0]); !errors.Is(err, memory.ErrNotFound) {
			t.Fatalf("foreign delete: %v", err)
		}
	}
	if _, err := reopened.Delete(t.Context(), id, keys[0]); err != nil {
		t.Fatal(err)
	}
	remaining, err := reopened.Inspect(t.Context(), id)
	if err != nil || len(remaining.Items) != 2 {
		t.Fatalf("delete: %+v, %v", remaining, err)
	}
}

func TestE2E_Phase24_CapacityFailurePreservesUnsummarizedNotes(t *testing.T) {
	mem, _, _ := phase24Memory(t, "inmem", "", 3)
	id := identity.Quadruple{Identity: identity.Identity{TenantID: "T", UserID: "U", SessionID: "S"}}
	for i := range 3 {
		if _, err := mem.Put(t.Context(), id, memory.ConversationTurn{UserMessage: fmt.Sprintf("constraint-%d", i)}); err != nil {
			t.Fatal(err)
		}
	}
	before, err := mem.Inspect(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mem.Put(t.Context(), id, memory.ConversationTurn{UserMessage: "must not evict the oldest"}); !errors.Is(err, sessionmemory.ErrRetainedContextCapacity) {
		t.Fatalf("full unsummarized window: %v", err)
	}
	after, err := mem.Inspect(t.Context(), id)
	if err != nil || len(after.Items) != 3 || after.Summary != "" {
		t.Fatalf("failed Put changed history: %+v, %v", after, err)
	}
	for i, item := range after.Items {
		if item.Key != before.Items[i].Key || string(item.Value) != string(before.Items[i].Value) {
			t.Fatalf("capacity refusal changed source %d", i)
		}
	}
	if !strings.Contains(string(after.Items[0].Value), "constraint-0") {
		t.Fatal("oldest unsummarized constraint was discarded")
	}
}

func TestE2E_Phase24_RemovedTruncationIsNotACompatibilityPath(t *testing.T) {
	_, bus, _ := phase24Memory(t, "inmem", "", 20)
	st, err := state.Open(t.Context(), config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close(context.Background()) })
	_, err = memory.Open(t.Context(), memory.ConfigSnapshot{Driver: "inmem", Strategy: "truncation"}, memory.Deps{State: st, Bus: bus})
	if !errors.Is(err, memory.ErrStrategyNotImplemented) {
		t.Fatalf("removed strategy: %v", err)
	}
}

func TestE2E_Phase24_FailsClosedOnMissingIdentity(t *testing.T) {
	mem, bus, _ := phase24Memory(t, "inmem", "", 20)
	sub, err := bus.Subscribe(t.Context(), events.Filter{Admin: true, Types: []events.EventType{memory.EventTypeMemoryIdentityRejected}})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Cancel()
	bogus := identity.Quadruple{Identity: identity.Identity{TenantID: "T", UserID: "U"}}
	if _, err := mem.Put(t.Context(), bogus, memory.ConversationTurn{UserMessage: "x"}); !errors.Is(err, memory.ErrIdentityRequired) {
		t.Fatalf("Put identity: %v", err)
	}
	select {
	case ev := <-sub.Events():
		if ev.Type != memory.EventTypeMemoryIdentityRejected {
			t.Fatalf("event=%q", ev.Type)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for identity rejection event")
	}
}
