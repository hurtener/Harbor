package sessionadmission_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/runtime/sessionadmission"
	"github.com/hurtener/Harbor/internal/state"
	"github.com/hurtener/Harbor/internal/state/drivers/inmem"
	"github.com/hurtener/Harbor/internal/state/drivers/postgres"
	"github.com/hurtener/Harbor/internal/state/drivers/sqlite"
	"github.com/hurtener/Harbor/internal/tools"
)

type pair struct {
	left, right state.StateStore
	reopen      func() state.StateStore
}

func stores(t *testing.T, driver string) pair {
	t.Helper()
	cfg := config.StateConfig{Driver: driver}
	open := inmem.New
	switch driver {
	case "sqlite":
		cfg.DSN = filepath.Join(t.TempDir(), "admission.sqlite")
		open = sqlite.New
	case "postgres":
		base := os.Getenv("HARBOR_PG_DSN")
		if base == "" {
			t.Skip("HARBOR_PG_DSN not set; live PostgreSQL admission conformance requires the isolated test database")
		}
		db, err := sql.Open("pgx", base)
		if err != nil {
			t.Fatal(err)
		}
		schema := "admission_" + strings.ToLower(string(state.NewEventID()))
		if _, err := db.ExecContext(t.Context(), `CREATE SCHEMA "`+schema+`"`); err != nil {
			db.Close()
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_, _ = db.ExecContext(context.Background(), `DROP SCHEMA "`+schema+`" CASCADE`)
			_ = db.Close()
		})
		cfg.DSN = base + " search_path=" + schema
		if u, err := url.Parse(base); err == nil && (u.Scheme == "postgres" || u.Scheme == "postgresql") {
			q := u.Query()
			q.Set("search_path", schema)
			u.RawQuery = q.Encode()
			cfg.DSN = u.String()
		}
		open = postgres.New
	}
	newStore := func() state.StateStore {
		s, err := open(cfg)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.Close(context.Background()) })
		return s
	}
	left := newStore()
	right := left
	if driver != "inmem" {
		right = newStore()
	}
	return pair{left: left, right: right, reopen: func() state.StateStore {
		if driver == "inmem" {
			return left
		}
		return newStore()
	}}
}

func gate(t *testing.T, s state.StateStore) *sessionadmission.Gate {
	t.Helper()
	g, err := sessionadmission.New(s)
	if err != nil {
		t.Fatal(err)
	}
	return g
}
func principal(t *testing.T, g *sessionadmission.Gate, id identity.Identity, admin bool) context.Context {
	t.Helper()
	ctx, err := identity.WithVerified(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	ctx = sessionadmission.WithGate(ctx, g)
	ctx = auth.WithTokenAuthority(ctx, auth.TokenAuthority{Issuer: "https://issuer.example", Subject: "coordinator"})
	if admin {
		ctx = auth.WithScopes(ctx, []auth.Scope{auth.ScopeAdmin})
	}
	return ctx
}
func scoped(ctx context.Context, id identity.Identity, epoch uint64, methodsAllowed ...methods.Method) context.Context {
	ctx = auth.WithMethodReach(ctx, methodsAllowed)
	return auth.WithSessionAdmission(ctx, &auth.SessionAdmissionAuthority{Identity: id, Epoch: epoch, Coordinator: "coordinator"})
}
func permit(t *testing.T, ctx context.Context, id identity.Identity, m methods.Method) *sessionadmission.Acceptance {
	t.Helper()
	_, p, err := sessionadmission.Begin(ctx, id, m)
	if err != nil {
		t.Fatal(err)
	}
	return p
}
func finish(t *testing.T, ctx context.Context, p *sessionadmission.Acceptance) {
	t.Helper()
	if err := p.Finish(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestAdmission_RealDriverConformance(t *testing.T) {
	for _, driver := range []string{"inmem", "sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			p := stores(t, driver)
			left, right := gate(t, p.left), gate(t, p.right)
			id := identity.Identity{TenantID: "tenant", UserID: "owner", SessionID: "session"}
			old := principal(t, left, id, false)
			admin := principal(t, right, id, true)
			finish(t, old, permit(t, old, id, methods.MethodUserMessage))
			policy, err := right.Enroll(admin, id, 0, 1)
			if err != nil || policy.Epoch != 1 {
				t.Fatalf("enrollment: %+v %v", policy, err)
			}
			if _, _, err := sessionadmission.Begin(old, id, methods.MethodUserMessage); !errors.Is(err, sessionadmission.ErrDenied) {
				t.Fatalf("same old authority after enrollment: %v", err)
			}
			current := scoped(old, id, 1, methods.MethodUserMessage, methods.MethodResume, methods.MethodApprove, methods.MethodReject, methods.MethodArtifactsPut)
			for _, m := range []methods.Method{methods.MethodUserMessage, methods.MethodResume, methods.MethodApprove, methods.MethodReject, methods.MethodArtifactsPut} {
				finish(t, current, permit(t, current, id, m))
			}
			reader := auth.WithMethodReach(old, []methods.Method{methods.MethodStateHistory, methods.MethodEventsSubscribe, methods.MethodArtifactsGet})
			if err := auth.AuthorizeMethod(reader, methods.MethodArtifactsGet); err != nil {
				t.Fatal(err)
			}
			if _, _, err := sessionadmission.Begin(reader, id, methods.MethodUserMessage); !errors.Is(err, auth.ErrMethodReachDenied) {
				t.Fatalf("reader steered: %v", err)
			}
			for name, bad := range map[string]context.Context{
				"old_epoch":           scoped(old, id, 2, methods.MethodUserMessage),
				"foreign_issuer":      auth.WithTokenAuthority(current, auth.TokenAuthority{Issuer: "https://foreign.example", Subject: "coordinator"}),
				"foreign_coordinator": auth.WithSessionAdmission(current, &auth.SessionAdmissionAuthority{Identity: id, Epoch: 1, Coordinator: "other"}),
				"foreign_owner":       scoped(old, identity.Identity{TenantID: id.TenantID, UserID: "other", SessionID: id.SessionID}, 1, methods.MethodUserMessage),
				"foreign_session":     scoped(old, identity.Identity{TenantID: id.TenantID, UserID: id.UserID, SessionID: "other"}, 1, methods.MethodUserMessage),
			} {
				if _, _, err := sessionadmission.Begin(bad, id, methods.MethodUserMessage); !errors.Is(err, sessionadmission.ErrDenied) {
					t.Fatalf("%s: %v", name, err)
				}
			}
			if _, err := right.Enroll(auth.WithTokenAuthority(admin, auth.TokenAuthority{Issuer: "https://foreign.example", Subject: "coordinator"}), id, 1, 2); !errors.Is(err, sessionadmission.ErrDenied) {
				t.Fatalf("replaced issuer: %v", err)
			}
			if _, err := right.Enroll(admin, id, 0, 1); err != nil {
				t.Fatalf("exact lost enrollment reply retry: %v", err)
			}
			if _, err := right.Enroll(admin, id, 1, 3); !errors.Is(err, sessionadmission.ErrConflict) {
				t.Fatalf("skipped epoch: %v", err)
			}
			if _, err := right.Enroll(admin, id, 1, 2); err != nil {
				t.Fatal(err)
			}
			if _, _, err := sessionadmission.Begin(current, id, methods.MethodUserMessage); !errors.Is(err, sessionadmission.ErrDenied) {
				t.Fatalf("old epoch after advance: %v", err)
			}
			// Session erasure cannot remove the reserved coordination record.
			if _, err := p.left.DeleteScope(t.Context(), id); err != nil {
				t.Fatal(err)
			}
			if driver != "inmem" {
				_ = p.left.Close(context.Background())
				_ = p.right.Close(context.Background())
			}
			reopened := gate(t, p.reopen())
			got, err := reopened.PolicyFor(t.Context(), id)
			if err != nil || got.Epoch != 2 {
				t.Fatalf("enrollment lost across reopen/delete: %+v %v", got, err)
			}
		})
	}
}

func TestAdmission_IndependentActorsSerializeAbsentPolicy(t *testing.T) {
	for _, driver := range []string{"inmem", "sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			p := stores(t, driver)
			left, right := gate(t, p.left), gate(t, p.right)
			id := identity.Identity{TenantID: "tenant", UserID: "owner", SessionID: "race"}
			old := principal(t, left, id, false)
			admin := principal(t, right, id, true)
			// A legacy mutation reserves even an absent enrollment slot.
			accepted := permit(t, old, id, methods.MethodCancel)
			if _, err := right.Enroll(admin, id, 0, 1); !errors.Is(err, sessionadmission.ErrBusy) {
				t.Fatalf("enrolled across legacy acceptance: %v", err)
			}
			finish(t, old, accepted)
			if _, err := right.Enroll(admin, id, 0, 1); err != nil {
				t.Fatal(err)
			}
			if _, _, err := sessionadmission.Begin(old, id, methods.MethodCancel); !errors.Is(err, sessionadmission.ErrDenied) {
				t.Fatalf("old mutation accepted: %v", err)
			}
			// Race absent policy creation through two independent store actors. Hold a
			// winning mutation reservation until enrollment has returned.
			for n := 0; n < 32; n++ {
				target := id
				target.SessionID = fmt.Sprintf("absent-%d", n)
				start := make(chan struct{})
				done := make(chan error, 1)
				go func() { <-start; _, err := right.Enroll(principal(t, right, target, true), target, 0, 1); done <- err }()
				close(start)
				_, a, mutationErr := sessionadmission.Begin(principal(t, left, target, false), target, methods.MethodUserMessage)
				enrollErr := <-done
				if mutationErr == nil && enrollErr == nil {
					t.Fatal("enrollment passed unresolved legacy acceptance")
				}
				if mutationErr == nil {
					finish(t, old, a)
				}
				if mutationErr != nil && !errors.Is(mutationErr, sessionadmission.ErrDenied) && !errors.Is(mutationErr, sessionadmission.ErrConflict) {
					t.Fatal(mutationErr)
				}
				if enrollErr != nil && !errors.Is(enrollErr, sessionadmission.ErrBusy) && !errors.Is(enrollErr, sessionadmission.ErrConflict) {
					t.Fatal(enrollErr)
				}
			}
		})
	}
}

func TestAdmission_RestartRetainsUnresolvedAcceptance(t *testing.T) {
	for _, driver := range []string{"inmem", "sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) {
			p := stores(t, driver)
			g := gate(t, p.left)
			id := identity.Identity{TenantID: "tenant", UserID: "owner", SessionID: "uncertain"}
			old := principal(t, g, id, false)
			permit(t, old, id, methods.MethodUserMessage) // model process loss after admission
			if driver != "inmem" {
				_ = p.left.Close(context.Background())
				_ = p.right.Close(context.Background())
			}
			reopened := gate(t, p.reopen())
			admin := principal(t, reopened, id, true)
			for i := 0; i < 3; i++ {
				if _, err := reopened.Enroll(admin, id, 0, 1); !errors.Is(err, sessionadmission.ErrBusy) {
					t.Fatalf("uncertain reservation expired: %v", err)
				}
				if _, _, err := sessionadmission.Begin(admin, id, methods.MethodCancel); !errors.Is(err, sessionadmission.ErrBusy) {
					t.Fatalf("uncertain mutation retried: %v", err)
				}
			}
		})
	}
}

func TestAdmission_ConcurrentReuseIsolation(t *testing.T) {
	p := stores(t, "inmem")
	g := gate(t, p.left)
	const n = 128
	var wg sync.WaitGroup
	var accepted atomic.Int64
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := identity.Identity{TenantID: fmt.Sprintf("tenant-%d", i%4), UserID: fmt.Sprintf("user-%d", i%8), SessionID: fmt.Sprintf("session-%d", i)}
			ctx := principal(t, g, id, true)
			if _, err := g.Enroll(ctx, id, 0, 1); err != nil {
				errs <- err
				return
			}
			_, a, err := sessionadmission.Begin(scoped(ctx, id, 1, methods.MethodUserMessage), id, methods.MethodUserMessage)
			if err != nil {
				errs <- err
				return
			}
			if err := a.Finish(ctx); err != nil {
				errs <- err
				return
			}
			accepted.Add(1)
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if accepted.Load() != n {
		t.Fatalf("accepted %d/%d", accepted.Load(), n)
	}
}

func TestAdmission_NativeOAuthAuthorityIsNarrow(t *testing.T) {
	p := stores(t, "inmem")
	g := gate(t, p.left)
	id := identity.Identity{TenantID: "t", UserID: "u", SessionID: "oauth"}
	admin := principal(t, g, id, true)
	if _, err := g.Enroll(admin, id, 0, 1); err != nil {
		t.Fatal(err)
	}
	ctx := auth.WithMethodReach(principal(t, g, id, false), nil)
	_, err := sessionadmission.RunNativeResume(ctx, id, func(accepted context.Context) (bool, error) {
		if _, _, err := sessionadmission.Begin(accepted, id, methods.MethodUserMessage); !errors.Is(err, auth.ErrMethodReachDenied) {
			return false, fmt.Errorf("native resume widened user input: %v", err)
		}
		if _, err := g.Enroll(admin, id, 1, 2); !errors.Is(err, sessionadmission.ErrBusy) {
			return false, fmt.Errorf("native callback crossed enrollment: %v", err)
		}
		return true, nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// An acquire whose commit reply is lost never invokes domain mutation. The
// committed reservation is retained and discovered by another store actor.
type uncertainAcquire struct {
	state.StateStore
	once atomic.Bool
}

func (s *uncertainAcquire) SaveIf(ctx context.Context, expected []state.SlotExpectation, next state.StateRecord) error {
	err := s.StateStore.SaveIf(ctx, expected, next)
	if err == nil && s.once.CompareAndSwap(false, true) {
		return state.ErrCommitOutcomeUnknown
	}
	return err
}
func TestAdmission_UncertainAcquireNeverExecutesAndCannotDowngrade(t *testing.T) {
	p := stores(t, "inmem")
	wrapped := &uncertainAcquire{StateStore: p.left}
	g := gate(t, wrapped)
	id := identity.Identity{TenantID: "t", UserID: "u", SessionID: "lost-commit"}
	ctx := principal(t, g, id, false)
	invoked := false
	_, err := sessionadmission.Run(ctx, id, methods.MethodCancel, func(context.Context) (bool, error) { invoked = true; return true, nil })
	if err == nil || invoked {
		t.Fatalf("uncertain acquire invoked=%v err=%v", invoked, err)
	}
	other := gate(t, p.right)
	if _, err := other.Enroll(principal(t, other, id, true), id, 0, 1); !errors.Is(err, sessionadmission.ErrBusy) {
		t.Fatalf("lost reservation after uncertain commit: %v", err)
	}
	if err := sessionadmission.RequireEnabled(t.Context(), p.left, false); !errors.Is(err, sessionadmission.ErrUnavailable) {
		t.Fatalf("configuration downgrade: %v", err)
	}
	if err := sessionadmission.RequireEnabled(t.Context(), p.left, true); err != nil {
		t.Fatal(err)
	}
}

type nativeFaultStore struct {
	state.StateStore
	writes atomic.Int64
	failAt atomic.Int64
}

func (s *nativeFaultStore) SaveIf(ctx context.Context, expected []state.SlotExpectation, next state.StateRecord) error {
	count := s.writes.Add(1)
	err := s.StateStore.SaveIf(ctx, expected, next)
	if err == nil && count == s.failAt.Load() {
		return state.ErrCommitOutcomeUnknown
	}
	return err
}

func TestAdmission_NativeHandoffRechecksAndRetainsUncertainty(t *testing.T) {
	for _, driver := range []string{"inmem", "sqlite", "postgres"} {
		t.Run(driver, func(t *testing.T) { nativeHandoffCases(t, driver) })
	}
}

func nativeHandoffCases(t *testing.T, driver string) {
	for _, name := range []string{"same_epoch", "new_epoch", "lost_reacquire", "lost_park"} {
		t.Run(name, func(t *testing.T) {
			p := stores(t, driver)
			observed := &nativeFaultStore{StateStore: p.left}
			g := gate(t, observed)
			other := gate(t, p.right)
			id := identity.Identity{TenantID: "t", UserID: "u", SessionID: name}
			admin := principal(t, g, id, true)
			if _, err := g.Enroll(admin, id, 0, 1); err != nil {
				t.Fatal(err)
			}
			observed.writes.Store(0)
			ctx := scoped(admin, id, 1, methods.MethodMCPAppsCallTool)
			accepted, a, err := sessionadmission.Begin(ctx, id, methods.MethodMCPAppsCallTool)
			if err != nil {
				t.Fatal(err)
			}
			if name == "lost_park" {
				observed.failAt.Store(2)
			}
			parkErr := a.ParkForNative(accepted)
			if name == "lost_park" {
				if parkErr == nil {
					t.Fatal("lost park commit reported certainty")
				}
				if err := a.ResumeAfterNative(accepted); err == nil {
					t.Fatal("uncertain handoff executed continuation")
				}
				return
			}
			if parkErr != nil {
				t.Fatal(parkErr)
			}
			if name == "new_epoch" {
				if _, err := other.Enroll(admin, id, 1, 2); err != nil {
					t.Fatal(err)
				}
			}
			if name == "lost_reacquire" {
				observed.failAt.Store(observed.writes.Load() + 1)
			}
			resumeErr := a.ResumeAfterNative(accepted)
			switch name {
			case "same_epoch":
				if resumeErr != nil {
					t.Fatal(resumeErr)
				}
				writes := observed.writes.Load()
				if err := a.ResumeAfterNative(accepted); err != nil {
					t.Fatal(err)
				}
				if observed.writes.Load() != writes {
					t.Fatal("repeated native delivery reacquired twice")
				}
				if _, err := other.Enroll(admin, id, 1, 2); !errors.Is(err, sessionadmission.ErrBusy) {
					t.Fatalf("reacquired invocation was not fenced: %v", err)
				}
				finish(t, ctx, a)
			case "new_epoch":
				if resumeErr == nil {
					t.Fatal("stale native continuation reacquired")
				}
				finish(t, ctx, a)
			case "lost_reacquire":
				if resumeErr == nil {
					t.Fatal("uncertain reacquisition continued")
				}
				if _, err := other.Enroll(admin, id, 1, 2); !errors.Is(err, sessionadmission.ErrBusy) {
					t.Fatalf("uncertain reacquisition lost durable reservation: %v", err)
				}
				if err := a.ParkForNative(accepted); err == nil {
					t.Fatal("native decline refunded uncertain reacquisition")
				}
			}
		})
	}
}

func TestAdmission_ReusedCallbackContextDoesNotBypassReservation(t *testing.T) {
	p := stores(t, "inmem")
	g := gate(t, p.left)
	id := identity.Identity{TenantID: "t", UserID: "u", SessionID: "reused-context"}
	ctx := principal(t, g, id, true)
	accepted, a, err := sessionadmission.Begin(ctx, id, methods.MethodStart)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := sessionadmission.Begin(accepted, id, methods.MethodCancel); !errors.Is(err, sessionadmission.ErrBusy) {
		t.Fatalf("nested mutation bypassed reservation: %v", err)
	}
	finish(t, ctx, a)
	if _, err := g.Enroll(ctx, id, 0, 1); err != nil {
		t.Fatal(err)
	}
	if _, _, err := sessionadmission.Begin(accepted, id, methods.MethodCancel); !errors.Is(err, sessionadmission.ErrDenied) {
		t.Fatalf("reused finished context bypassed epoch: %v", err)
	}
}

func TestAdmission_NativeChallengeCannotRefundDispatchedAttempt(t *testing.T) {
	p := stores(t, "inmem")
	g := gate(t, p.left)
	id := identity.Identity{TenantID: "t", UserID: "u", SessionID: "dispatched-before-challenge"}
	admin := principal(t, g, id, true)
	if _, err := g.Enroll(admin, id, 0, 1); err != nil {
		t.Fatal(err)
	}
	accepted, _, err := sessionadmission.Begin(scoped(admin, id, 1, methods.MethodMCPAppsCallTool), id, methods.MethodMCPAppsCallTool)
	if err != nil {
		t.Fatal(err)
	}
	if err := tools.MarkInvocationEffectStarted(accepted); err != nil {
		t.Fatal(err)
	}
	if err := tools.ParkInvocationAdmission(accepted); !errors.Is(err, tools.ErrInvocationAdmission) {
		t.Fatalf("post-dispatch challenge released prior liability: %v", err)
	}
	if _, err := g.Enroll(admin, id, 1, 2); !errors.Is(err, sessionadmission.ErrBusy) {
		t.Fatalf("post-dispatch challenge lost acceptance: %v", err)
	}
}
