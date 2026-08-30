package events_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/events/drivers/durable"
	"github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	stateinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
)

// indexedFanoutConfig keeps each stress subscriber in its own identity
// bucket. The one-subscriber cap also verifies that Cancel removes the entry
// from both the canonical set and the secondary index.
func indexedFanoutConfig(driver string) config.EventsConfig {
	return config.EventsConfig{
		Driver:                   driver,
		MaxSubscribersPerSession: 1,
		SubscriberBufferSize:     4,
		IdleTimeout:              time.Hour,
		DropWindow:               time.Hour,
		ReplayBufferSize:         0,
		LegacyWritersDrained:     true,
	}
}

func newIndexedFanoutBus(t *testing.T, driver string) events.EventBus {
	t.Helper()
	cfg := indexedFanoutConfig(driver)
	redactor := auditpatterns.New()
	var bus events.EventBus
	switch driver {
	case "inmem":
		var err error
		bus, err = inmem.New(cfg, redactor)
		if err != nil {
			t.Fatalf("inmem.New: %v", err)
		}
	case "durable":
		store, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
		if err != nil {
			t.Fatalf("stateinmem.New: %v", err)
		}
		t.Cleanup(func() { _ = store.Close(context.Background()) })
		bus, err = durable.New(context.Background(), cfg, redactor, store)
		if err != nil {
			t.Fatalf("durable.New: %v", err)
		}
	default:
		t.Fatalf("unknown fan-out driver %q", driver)
	}
	t.Cleanup(func() {
		if err := bus.Close(context.Background()); err != nil {
			t.Errorf("%s bus Close: %v", driver, err)
		}
	})
	return bus
}

func indexedFanoutIdentity(n int) identity.Quadruple {
	return identity.Quadruple{Identity: identity.Identity{
		TenantID:  fmt.Sprintf("fanout-tenant-%d", n),
		UserID:    fmt.Sprintf("fanout-user-%d", n),
		SessionID: fmt.Sprintf("fanout-session-%d", n),
	}}
}

func indexedFanoutEvent(id identity.Quadruple, typ events.EventType) events.Event {
	return events.Event{
		Type:     typ,
		Identity: id,
		Payload:  events.SubscriptionIdleClosedPayload{SubscriberID: 1},
	}
}

func subscribeIndexed(t *testing.T, bus events.EventBus, id identity.Quadruple) events.Subscription {
	t.Helper()
	sub, err := bus.Subscribe(context.Background(), events.Filter{
		Tenant:  id.TenantID,
		User:    id.UserID,
		Session: id.SessionID,
	})
	if err != nil {
		t.Fatalf("Subscribe(%s/%s/%s): %v", id.TenantID, id.UserID, id.SessionID, err)
	}
	return sub
}

// TestFanOut_IdentityIndex_1KAnd10K verifies that a publish reaches only the
// exact identity subscriber among deterministic 1K and 10K mostly-nonmatching
// populations in both shipped EventBus drivers. The non-blocking checks are
// intentional: each unrelated channel must remain empty after the synchronous
// Publish returns.
func TestFanOut_IdentityIndex_1KAnd10K(t *testing.T) {
	for _, driver := range []string{"inmem", "durable"} {
		t.Run(driver, func(t *testing.T) {
			for _, subscriberCount := range []int{1_000, 10_000} {
				t.Run(fmt.Sprintf("subscribers=%d", subscriberCount), func(t *testing.T) {
					bus := newIndexedFanoutBus(t, driver)
					subs := make([]events.Subscription, subscriberCount)
					for i := range subscriberCount {
						subs[i] = subscribeIndexed(t, bus, indexedFanoutIdentity(i))
					}
					target := subscriberCount / 2
					targetID := indexedFanoutIdentity(target)
					if err := bus.Publish(context.Background(), indexedFanoutEvent(targetID, events.EventTypeRuntimeWarning)); err != nil {
						t.Fatalf("Publish target event: %v", err)
					}

					select {
					case got, ok := <-subs[target].Events():
						if !ok {
							t.Fatal("target subscription closed before delivery")
						}
						if got.Identity != targetID || got.Type != events.EventTypeRuntimeWarning {
							t.Fatalf("target received %+v, want identity %+v and runtime.warning", got, targetID)
						}
					case <-time.After(2 * time.Second):
						t.Fatal("timed out waiting for target delivery")
					}

					for i, sub := range subs {
						if i == target {
							continue
						}
						select {
						case got, ok := <-sub.Events():
							if ok {
								t.Fatalf("nonmatching subscriber %d received %+v", i, got)
							}
							t.Fatalf("nonmatching subscriber %d closed before cleanup", i)
						default:
						}
					}

					// The cap is one per identity. Re-subscribing after Cancel
					// must succeed, proving cancellation removes the exact bucket
					// entry rather than only marking the channel closed.
					subs[target].Cancel()
					replacement := subscribeIndexed(t, bus, targetID)
					if err := bus.Publish(context.Background(), indexedFanoutEvent(targetID, events.EventTypeRuntimeWarning)); err != nil {
						t.Fatalf("Publish after replacement: %v", err)
					}
					select {
					case got, ok := <-replacement.Events():
						if !ok || got.Identity != targetID {
							t.Fatalf("replacement received ok=%v event=%+v, want target identity", ok, got)
						}
					case <-time.After(2 * time.Second):
						t.Fatal("timed out waiting for replacement delivery")
					}
					replacement.Cancel()
				})
			}
		})
	}
}

// TestFanOut_IdentityIndex_PreservesAdminAndFinalFilters ensures the index
// is only a candidate selector. Admin fan-in still honors Types, while an
// exact identity subscriber still honors Run and Types through Filter.Matches.
func TestFanOut_IdentityIndex_PreservesAdminAndFinalFilters(t *testing.T) {
	for _, driver := range []string{"inmem", "durable"} {
		t.Run(driver, func(t *testing.T) {
			bus := newIndexedFanoutBus(t, driver)
			id := indexedFanoutIdentity(42)
			admin, err := bus.Subscribe(context.Background(), events.Filter{
				Admin: true,
				Types: []events.EventType{events.EventTypeRuntimeWarning},
			})
			if err != nil {
				t.Fatalf("admin Subscribe: %v", err)
			}
			exact, err := bus.Subscribe(context.Background(), events.Filter{
				Tenant:  id.TenantID,
				User:    id.UserID,
				Session: id.SessionID,
				Run:     "run-1",
				Types:   []events.EventType{events.EventTypeRuntimeWarning},
			})
			if err != nil {
				t.Fatalf("exact Subscribe: %v", err)
			}

			wrongType := indexedFanoutEvent(id, events.EventTypeRuntimeError)
			wrongType.Identity.RunID = "run-1"
			if err := bus.Publish(context.Background(), wrongType); err != nil {
				t.Fatalf("Publish wrong type: %v", err)
			}
			wrongRun := indexedFanoutEvent(id, events.EventTypeRuntimeWarning)
			wrongRun.Identity.RunID = "run-2"
			if err := bus.Publish(context.Background(), wrongRun); err != nil {
				t.Fatalf("Publish wrong run: %v", err)
			}
			matching := indexedFanoutEvent(id, events.EventTypeRuntimeWarning)
			matching.Identity.RunID = "run-1"
			if err := bus.Publish(context.Background(), matching); err != nil {
				t.Fatalf("Publish matching event: %v", err)
			}

			select {
			case got, ok := <-admin.Events():
				if !ok || got.Type != events.EventTypeRuntimeWarning || got.Identity.RunID != "run-2" {
					t.Fatalf("admin first event ok=%v %+v, want only runtime.warning run-2", ok, got)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("timed out waiting for admin warning")
			}
			select {
			case got, ok := <-admin.Events():
				if !ok || got.Type != events.EventTypeRuntimeWarning || got.Identity.RunID != "run-1" {
					t.Fatalf("admin second event ok=%v %+v, want runtime.warning run-1", ok, got)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("timed out waiting for admin matching warning")
			}
			select {
			case got, ok := <-exact.Events():
				if !ok || got.Identity.RunID != "run-1" {
					t.Fatalf("exact event ok=%v %+v, want runtime.warning run-1", ok, got)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("timed out waiting for exact matching event")
			}
			select {
			case got := <-exact.Events():
				t.Fatalf("exact subscriber received extra %+v", got)
			default:
			}
		})
	}
}

// TestFanOut_IdentityIndex_ConcurrentReuse exercises both indexed lookup and
// cancellation under N=128 concurrent sessions, which is the required
// reusable-artifact race coverage for this new fan-out path.
func TestFanOut_IdentityIndex_ConcurrentReuse(t *testing.T) {
	for _, driver := range []string{"inmem", "durable"} {
		t.Run(driver, func(t *testing.T) {
			bus := newIndexedFanoutBus(t, driver)
			const workers = 128
			errs := make(chan error, workers)
			var wg sync.WaitGroup
			wg.Add(workers)
			for i := range workers {
				go func(i int) {
					defer wg.Done()
					id := indexedFanoutIdentity(i)
					sub, err := bus.Subscribe(context.Background(), events.Filter{
						Tenant:  id.TenantID,
						User:    id.UserID,
						Session: id.SessionID,
					})
					if err != nil {
						errs <- fmt.Errorf("worker %d Subscribe: %w", i, err)
						return
					}
					if err := bus.Publish(context.Background(), indexedFanoutEvent(id, events.EventTypeRuntimeWarning)); err != nil {
						errs <- fmt.Errorf("worker %d Publish: %w", i, err)
						sub.Cancel()
						return
					}
					select {
					case got, ok := <-sub.Events():
						if !ok || got.Identity != id {
							errs <- fmt.Errorf("worker %d received ok=%v identity=%+v, want %+v", i, ok, got.Identity, id)
						}
					case <-time.After(2 * time.Second):
						errs <- fmt.Errorf("worker %d timed out waiting for event", i)
					}
					sub.Cancel()
				}(i)
			}
			wg.Wait()
			close(errs)
			for err := range errs {
				t.Error(err)
			}
		})
	}
}

// TestFanOut_IdentityIndex_ConcurrentSubscribeCap pins the per-session cap
// under concurrent insertion. Exactly one of 128 same-identity attempts may
// occupy the single-slot bucket; all others must receive the named limit
// error rather than racing through a scan-before-insert gap.
func TestFanOut_IdentityIndex_ConcurrentSubscribeCap(t *testing.T) {
	for _, driver := range []string{"inmem", "durable"} {
		t.Run(driver, func(t *testing.T) {
			bus := newIndexedFanoutBus(t, driver)
			id := indexedFanoutIdentity(9001)
			const attempts = 128
			results := make(chan error, attempts)
			subs := make([]events.Subscription, 0, 1)
			var subsMu sync.Mutex
			var wg sync.WaitGroup
			wg.Add(attempts)
			for range attempts {
				go func() {
					defer wg.Done()
					sub, err := bus.Subscribe(context.Background(), events.Filter{
						Tenant:  id.TenantID,
						User:    id.UserID,
						Session: id.SessionID,
					})
					if err == nil {
						subsMu.Lock()
						subs = append(subs, sub)
						subsMu.Unlock()
						results <- nil
						return
					}
					if !errors.Is(err, events.ErrSubscriberLimitReached) {
						results <- fmt.Errorf("unexpected Subscribe error: %w", err)
						return
					}
					results <- nil
				}()
			}
			wg.Wait()
			close(results)
			successes := 0
			for err := range results {
				if err != nil {
					t.Error(err)
					continue
				}
				successes++
			}
			if successes != attempts {
				t.Fatalf("result accounting got %d/%d attempts", successes, attempts)
			}
			if len(subs) != 1 {
				t.Fatalf("concurrent cap admitted %d subscribers, want exactly 1", len(subs))
			}
			subs[0].Cancel()
		})
	}
}
