package memory_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/memory"
	sessionmemory "github.com/hurtener/Harbor/internal/memory/session"
	"github.com/hurtener/Harbor/internal/state"
)

func TestSourceReference_BindsExactSource(t *testing.T) {
	id := identity.Quadruple{Identity: identity.Identity{TenantID: "tenant", UserID: "user", SessionID: "session"}}
	item := memory.Item{Key: "source", Value: []byte(`{"version":9007199254740993123,"more":false}`), ExpiresAt: time.Unix(2000000000, 0)}
	ref, err := memory.SourceReference(id, item)
	if err != nil {
		t.Fatal(err)
	}
	for _, axis := range []string{"tenant", "user", "session", "key", "value", "expiry"} {
		otherID, otherItem := id, item
		switch axis {
		case "tenant":
			otherID.TenantID += "x"
		case "user":
			otherID.UserID += "x"
		case "session":
			otherID.SessionID += "x"
		case "key":
			otherItem.Key += "x"
		case "value":
			otherItem.Value = []byte(`{"version":9007199254740993124,"more":false}`)
		case "expiry":
			otherItem.ExpiresAt = item.ExpiresAt.Add(time.Nanosecond)
		}
		changed, err := memory.SourceReference(otherID, otherItem)
		if err != nil || changed.ID == ref.ID {
			t.Fatalf("%s not bound: %v", axis, err)
		}
	}
	id.RunID = "another-viewing-run"
	same, err := memory.SourceReference(id, item)
	if err != nil || same.ID != ref.ID {
		t.Fatalf("viewing run changed a session source: %v", err)
	}
	item.Value = []byte("not JSON")
	if _, err := memory.SourceReference(id, item); !errors.Is(err, memory.ErrInvalidInspection) {
		t.Fatalf("invalid source: %v", err)
	}
	if _, err := memory.SourceReference(identity.Quadruple{}, item); !errors.Is(err, memory.ErrIdentityRequired) {
		t.Fatalf("missing identity: %v", err)
	}
}

func TestSourceReference_RealStoreLifetimeAndRestart(t *testing.T) {
	for _, driver := range []string{"inmem", "sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			red := patterns.New()
			bus, err := events.Open(t.Context(), config.Defaults().Events, red)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = bus.Close(context.Background()) })
			dsn := ""
			if driver == "postgres" {
				dsn = os.Getenv("HARBOR_PG_DSN")
				if dsn == "" {
					t.Skip("HARBOR_PG_DSN required for source-reference PostgreSQL lifetime checks")
				}
			}
			if driver == "sqlite" {
				dsn = filepath.Join(t.TempDir(), "source.db")
			}
			open := func() (state.StateStore, memory.MemoryStore) {
				st, err := state.Open(t.Context(), config.StateConfig{Driver: driver, DSN: dsn})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = st.Close(context.Background()) })
				mem, err := memory.Open(t.Context(), memory.ConfigSnapshot{Driver: driver, DSN: dsn, RecentTurns: 20}, memory.Deps{State: st, Bus: bus, Redactor: red, RetentionTTL: time.Hour})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = mem.Close(context.Background()) })
				return st, mem
			}
			st, mem := open()
			id := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}}
			key, err := mem.Put(t.Context(), id, memory.ConversationTurn{UserMessage: "keep the original constraint", AssistantResponse: "noted"})
			if err != nil {
				t.Fatal(err)
			}
			view, err := mem.Inspect(t.Context(), id)
			if err != nil || len(view.Items) != 1 {
				t.Fatalf("inspect: %v", err)
			}
			ref, err := memory.SourceReference(id, view.Items[0])
			if err != nil {
				t.Fatal(err)
			}
			check := func() {
				t.Helper()
				got, data, err := memory.ResolveSourceReference(t.Context(), mem, id, ref.ID)
				if err != nil || got.ID != ref.ID || got.SHA256 != ref.SHA256 || !bytes.Equal(data, view.Items[0].Value) {
					t.Fatalf("resolve exact source: %v", err)
				}
			}
			check()
			if driver != "inmem" {
				if err := mem.Close(t.Context()); err != nil {
					t.Fatal(err)
				}
				if err := st.Close(t.Context()); err != nil {
					t.Fatal(err)
				}
				st, mem = open()
				check()
			}
			cancelled, cancel := context.WithCancel(t.Context())
			cancel()
			if _, _, err := memory.ResolveSourceReference(cancelled, mem, id, ref.ID); !errors.Is(err, context.Canceled) {
				t.Fatalf("cancelled read: %v", err)
			}
			check() // cancellation of one read cannot affect another.
			if _, err := mem.Delete(t.Context(), id, key); err != nil {
				t.Fatal(err)
			}
			if _, _, err := memory.ResolveSourceReference(t.Context(), mem, id, ref.ID); !errors.Is(err, memory.ErrNotFound) {
				t.Fatalf("deleted source: %v", err)
			}

			// Exercise real retention with the owner's controllable clock. No
			// sleep and no copied blob can make expired information reappear.
			past := time.Now().Add(-2 * time.Hour)
			now := func() time.Time { return past }
			id.SessionID = "expired"
			if _, err := sessionmemory.Put(t.Context(), st, red, id, "expired private data", "noted", 20, time.Hour, now); err != nil {
				t.Fatal(err)
			}
			expiredView, err := sessionmemory.Inspect(t.Context(), st, id, now)
			if err != nil || len(expiredView.Items) != 1 {
				t.Fatalf("inspect before expiry: %v", err)
			}
			expired, err := memory.SourceReference(id, expiredView.Items[0])
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := memory.ResolveSourceReference(t.Context(), mem, id, expired.ID); !errors.Is(err, memory.ErrNotFound) {
				t.Fatalf("expired source: %v", err)
			}
			if err := mem.Close(t.Context()); err != nil {
				t.Fatal(err)
			}
			if _, _, err := memory.ResolveSourceReference(t.Context(), mem, id, expired.ID); !errors.Is(err, memory.ErrStoreClosed) {
				t.Fatalf("required read failure: %v", err)
			}
		})
	}
}
