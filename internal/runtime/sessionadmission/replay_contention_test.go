package sessionadmission_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/runtime/sessionadmission"
	"github.com/hurtener/Harbor/internal/state"
)

// Fail only definitive CAS losses, never unknown commits. The durable driver
// still owns all accepted writes and the ordinary enrollment/finish contract.
type replayContentionStore struct {
	state.StateStore
	losses atomic.Int64
	limit  int64
	onLoss func()
}

func (s *replayContentionStore) SaveIf(ctx context.Context, expected []state.SlotExpectation, next state.StateRecord) error {
	if s.losses.Add(1) <= s.limit {
		if s.onLoss != nil {
			s.onLoss()
		}
		return state.ErrConditionFailed
	}
	return s.StateStore.SaveIf(ctx, expected, next)
}

func TestAdmission_KeyedReplayCASContention(t *testing.T) {
	for _, method := range []methods.Method{methods.MethodStart, methods.MethodUserMessage} {
		t.Run(string(method), func(t *testing.T) {
			p := stores(t, "inmem")
			s := &replayContentionStore{StateStore: p.left, limit: 12}
			g := gate(t, s)
			id := identity.Identity{TenantID: "tenant", UserID: "owner", SessionID: "contended"}
			ctx := sessionadmission.WithReplayKey(principal(t, g, id, false), "same-exact-operation")
			_, a, err := sessionadmission.Begin(ctx, id, method)
			if err != nil {
				t.Fatalf("definitive CAS contention exhausted replay before its wait bound: %v", err)
			}
			finish(t, ctx, a)
		})
	}
}

func TestAdmission_KeyedReplayCancellationPreservesPending(t *testing.T) {
	p := stores(t, "inmem")
	g := gate(t, p.left)
	id := identity.Identity{TenantID: "tenant", UserID: "owner", SessionID: "pending"}
	ctx := sessionadmission.WithReplayKey(principal(t, g, id, false), "same-exact-operation")
	_, a, err := sessionadmission.Begin(ctx, id, methods.MethodUserMessage)
	if err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithTimeout(ctx, 25*time.Millisecond)
	defer cancel()
	if _, _, err = sessionadmission.Begin(cancelled, id, methods.MethodUserMessage); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waiting replay ignored cancellation: %v", err)
	}
	if _, _, err = sessionadmission.Begin(principal(t, g, id, false), id, methods.MethodCancel); !errors.Is(err, sessionadmission.ErrBusy) {
		t.Fatalf("cancelled waiter erased pending acceptance: %v", err)
	}
	finish(t, ctx, a)
}

func TestAdmission_KeyedReplayCASCancellation(t *testing.T) {
	p := stores(t, "inmem")
	s := &replayContentionStore{StateStore: p.left, limit: 1000}
	g := gate(t, s)
	id := identity.Identity{TenantID: "tenant", UserID: "owner", SessionID: "cancel-cas"}
	ctx, cancel := context.WithTimeout(sessionadmission.WithReplayKey(principal(t, g, id, false), "exact"), 25*time.Millisecond)
	defer cancel()
	if _, _, err := sessionadmission.Begin(ctx, id, methods.MethodUserMessage); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("CAS waiter ignored cancellation: %v", err)
	}
}

func TestAdmission_KeyedReplayUnknownCommitNeverRetries(t *testing.T) {
	p := stores(t, "inmem")
	s := &uncertainAcquire{StateStore: p.left}
	g := gate(t, s)
	id := identity.Identity{TenantID: "tenant", UserID: "owner", SessionID: "unknown"}
	ctx := sessionadmission.WithReplayKey(principal(t, g, id, false), "exact")
	invoked := false
	_, err := sessionadmission.Run(ctx, id, methods.MethodUserMessage, func(context.Context) (bool, error) {
		invoked = true
		return true, nil
	})
	if err == nil || invoked {
		t.Fatalf("uncertain acquire executed: invoked=%v err=%v", invoked, err)
	}
	if _, _, err = sessionadmission.Begin(principal(t, g, id, false), id, methods.MethodCancel); !errors.Is(err, sessionadmission.ErrBusy) {
		t.Fatalf("uncertain replay acquisition disappeared: %v", err)
	}
}

func TestAdmission_UnkeyedContentionKeepsBound(t *testing.T) {
	p := stores(t, "inmem")
	s := &replayContentionStore{StateStore: p.left, limit: 1000}
	g := gate(t, s)
	id := identity.Identity{TenantID: "tenant", UserID: "owner", SessionID: "unkeyed"}
	if _, _, err := sessionadmission.Begin(principal(t, g, id, false), id, methods.MethodUserMessage); !errors.Is(err, sessionadmission.ErrConflict) || s.losses.Load() != 8 {
		t.Fatalf("ordinary admission bound changed: attempts=%d err=%v", s.losses.Load(), err)
	}
}

func TestAdmission_KeyedReplayReauthorizesAfterCASLoss(t *testing.T) {
	p := stores(t, "inmem")
	other := gate(t, p.right)
	id := identity.Identity{TenantID: "tenant", UserID: "owner", SessionID: "new-epoch"}
	admin := principal(t, other, id, true)
	if _, err := other.Enroll(admin, id, 0, 1); err != nil {
		t.Fatal(err)
	}
	s := &replayContentionStore{StateStore: p.left, limit: 1, onLoss: func() {
		if _, err := other.Enroll(admin, id, 1, 2); err != nil {
			t.Fatal(err)
		}
	}}
	g := gate(t, s)
	ctx := sessionadmission.WithReplayKey(scoped(principal(t, g, id, false), id, 1, methods.MethodUserMessage), "exact")
	if _, _, err := sessionadmission.Begin(ctx, id, methods.MethodUserMessage); !errors.Is(err, sessionadmission.ErrDenied) {
		t.Fatalf("contended replay bypassed new enrollment: %v", err)
	}
}

func TestAdmission_SameKeyConcurrentReplayRealDrivers(t *testing.T) {
	for _, driver := range []string{"inmem", "sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			p := stores(t, driver)
			left, right := gate(t, p.left), gate(t, p.right)
			id := identity.Identity{TenantID: "tenant", UserID: "owner", SessionID: "same-key"}
			if _, err := left.Enroll(principal(t, left, id, true), id, 0, 1); err != nil {
				t.Fatal(err)
			}
			var active atomic.Int64
			var wg sync.WaitGroup
			failures := make(chan error, 100)
			start := make(chan struct{})
			for i := range 100 {
				g := left
				if i%2 != 0 {
					g = right
				}
				ctx := sessionadmission.WithReplayKey(scoped(principal(t, g, id, false), id, 1, methods.MethodUserMessage), "exact-task-and-event")
				wg.Go(func() {
					<-start
					_, err := sessionadmission.Run(ctx, id, methods.MethodUserMessage, func(context.Context) (bool, error) {
						if active.Add(1) != 1 {
							return false, fmt.Errorf("overlapping acceptance callbacks")
						}
						active.Add(-1)
						return true, nil
					})
					failures <- err
				})
			}
			close(start)
			wg.Wait()
			close(failures)
			for err := range failures {
				if err != nil {
					t.Error(err)
				}
			}
		})
	}
}
