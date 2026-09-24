// Package conformancetest applies one cumulative-memory administrative contract
// to every driver. Execution rollover is covered by the session-owner suite.
package conformancetest

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/memory"
)

// Harness supplies a fresh real driver and its cleanup.
type Harness struct {
	Store    memory.MemoryStore
	Bus      events.EventBus
	Strategy memory.Strategy
	Cleanup  func()
}

// Factory constructs an isolated store for each case.
type Factory func() Harness

func scope(session string) identity.Quadruple {
	return identity.Quadruple{Identity: identity.Identity{TenantID: "conformance-t", UserID: "conformance-u", SessionID: session}}
}
func methods(store memory.MemoryStore, id identity.Quadruple) map[string]func(context.Context) error {
	return map[string]func(context.Context) error{
		"Inspect": func(ctx context.Context) error { _, err := store.Inspect(ctx, id); return err },
		"Put": func(ctx context.Context) error {
			_, err := store.Put(ctx, id, memory.ConversationTurn{UserMessage: "note"})
			return err
		},
		"Delete": func(ctx context.Context) error { _, err := store.Delete(ctx, id, "missing"); return err },
	}
}

// Run verifies identity, isolation, cancellation, mutation and lifecycle parity.
func Run(t *testing.T, factory Factory) {
	open := func(t *testing.T) Harness {
		t.Helper()
		h := factory()
		if h.Store == nil {
			t.Fatal("nil store")
		}
		if h.Cleanup != nil {
			t.Cleanup(h.Cleanup)
		}
		return h
	}
	t.Run("RoundTripAndDeletion", func(t *testing.T) {
		h := open(t)
		id := scope("roundtrip")
		key, err := h.Store.Put(t.Context(), id, memory.ConversationTurn{UserMessage: "early constraint: keep keyboard navigation", AssistantResponse: "recorded"})
		if err != nil {
			t.Fatal(err)
		}
		view, err := h.Store.Inspect(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		if h.Strategy == memory.StrategyNone {
			if key != "" || len(view.Items) != 0 {
				t.Fatal("disabled memory retained content")
			}
			return
		}
		if len(view.Items) != 1 || view.Items[0].Key != key || !strings.Contains(string(view.Items[0].Value), "keyboard navigation") || view.Items[0].ExpiresAt.IsZero() {
			t.Fatalf("note lost: %+v", view)
		}
		id.RunID = "later-run"
		later, err := h.Store.Inspect(t.Context(), id)
		if err != nil || len(later.Items) != 1 || later.Items[0].Key != key {
			t.Fatalf("run changed session projection: %+v %v", later, err)
		}
		if n, err := h.Store.Delete(t.Context(), id, key); err != nil || n != 0 {
			t.Fatalf("delete: %d %v", n, err)
		}
		view, err = h.Store.Inspect(t.Context(), id)
		if err != nil || len(view.Items) != 0 || view.Summary != "" {
			t.Fatalf("deleted context remains: %+v %v", view, err)
		}
		if _, err := h.Store.Delete(t.Context(), id, key); !errors.Is(err, memory.ErrNotFound) {
			t.Fatalf("missing delete: %v", err)
		}
	})
	t.Run("IdentityRequired", func(t *testing.T) {
		h := open(t)
		for _, id := range []identity.Quadruple{{}, {Identity: identity.Identity{TenantID: "t", UserID: "u"}}, {Identity: identity.Identity{TenantID: "t", SessionID: "s"}}, {Identity: identity.Identity{UserID: "u", SessionID: "s"}}} {
			for name, call := range methods(h.Store, id) {
				if err := call(t.Context()); !errors.Is(err, memory.ErrIdentityRequired) {
					t.Fatalf("%s identity: %v", name, err)
				}
			}
		}
	})
	t.Run("TenantUserSessionIsolation", func(t *testing.T) {
		h := open(t)
		id := scope("isolated")
		key, err := h.Store.Put(t.Context(), id, memory.ConversationTurn{UserMessage: "private marker"})
		if err != nil {
			t.Fatal(err)
		}
		for _, field := range []string{"tenant", "user", "session"} {
			other := id
			switch field {
			case "tenant":
				other.TenantID += "-foreign"
			case "user":
				other.UserID += "-foreign"
			case "session":
				other.SessionID += "-foreign"
			}
			view, err := h.Store.Inspect(t.Context(), other)
			if err != nil || len(view.Items) != 0 {
				t.Fatalf("cross-%s read: %+v %v", field, view, err)
			}
			if _, err := h.Store.Delete(t.Context(), other, key); !errors.Is(err, memory.ErrNotFound) {
				t.Fatalf("cross-%s delete: %v", field, err)
			}
		}
	})
	t.Run("CancellationDoesNotCrossTalk", func(t *testing.T) {
		h := open(t)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		for name, call := range methods(h.Store, scope("cancelled")) {
			if err := call(ctx); !errors.Is(err, context.Canceled) {
				t.Fatalf("%s cancelled: %v", name, err)
			}
		}
		if _, err := h.Store.Put(t.Context(), scope("unaffected"), memory.ConversationTurn{UserMessage: "still live"}); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("ConcurrentReuse", func(t *testing.T) {
		h := open(t)
		baseline := runtime.NumGoroutine()
		const n = 128
		var wg sync.WaitGroup
		errs := make(chan error, n)
		for i := range n {
			wg.Add(1)
			go func() {
				defer wg.Done()
				id := scope(fmt.Sprintf("concurrent-%d", i))
				marker := fmt.Sprintf("unique-%d-end", i)
				for range 8 {
					key, err := h.Store.Put(t.Context(), id, memory.ConversationTurn{UserMessage: marker})
					if err != nil {
						errs <- err
						return
					}
					view, err := h.Store.Inspect(t.Context(), id)
					if err != nil {
						errs <- err
						return
					}
					if h.Strategy == memory.StrategyNone {
						if len(view.Items) != 0 || key != "" {
							errs <- errors.New("disabled memory wrote a note")
							return
						}
						continue
					}
					if len(view.Items) != 1 || view.Items[0].Key != key || !strings.Contains(string(view.Items[0].Value), marker) {
						errs <- fmt.Errorf("session %d context bleed", i)
						return
					}
					if _, err := h.Store.Delete(t.Context(), id, key); err != nil {
						errs <- err
						return
					}
				}
			}()
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Error(err)
		}
		deadline := time.Now().Add(2 * time.Second)
		for runtime.NumGoroutine() > baseline && time.Now().Before(deadline) {
			runtime.Gosched()
		}
		if runtime.NumGoroutine() > baseline {
			t.Errorf("goroutine leak: before=%d after=%d", baseline, runtime.NumGoroutine())
		}
	})
	t.Run("CloseIdempotentAndTerminal", func(t *testing.T) {
		h := open(t)
		for range 2 {
			if err := h.Store.Close(t.Context()); err != nil {
				t.Fatal(err)
			}
		}
		for name, call := range methods(h.Store, scope("closed")) {
			if err := call(t.Context()); !errors.Is(err, memory.ErrStoreClosed) {
				t.Fatalf("%s closed: %v", name, err)
			}
		}
	})
}
