package search_test

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"

	"github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/search"
)

type auditRecorder struct {
	mu     sync.Mutex
	events []events.Event
	err    error
}

func (r *auditRecorder) publish(ctx context.Context, ev events.Event) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.err != nil {
		return r.err
	}
	r.events = append(r.events, ev)
	return nil
}

func (r *auditRecorder) snapshot() []events.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]events.Event(nil), r.events...)
}

func adminDeps(rec *auditRecorder) search.Deps {
	return search.Deps{
		Redactor:   patterns.New(),
		AdminScope: func(context.Context) bool { return true },
		Audit:      rec.publish,
	}
}

func TestAuthorizeScope_CanonicalIndexesAuditBothWideningAxes(t *testing.T) {
	t.Parallel()
	caller := identity.Identity{TenantID: "tenant-own", UserID: "user-own", SessionID: "session-own"}
	axes := []struct {
		name string
		req  types.SearchRequest
		want events.AdminScopeUsedPayload
	}{
		{
			name: "tenant",
			req: types.SearchRequest{Query: "must-not-enter-audit", Filter: types.SearchFilter{
				TenantIDs: []string{"tenant-target"}, UserIDs: []string{caller.UserID},
			}},
			want: events.AdminScopeUsedPayload{Tenant: "tenant-target", User: caller.UserID},
		},
		{
			name: "user",
			req: types.SearchRequest{Query: "must-not-enter-audit", Filter: types.SearchFilter{
				TenantIDs: []string{caller.TenantID}, UserIDs: []string{"user-target"},
			}},
			want: events.AdminScopeUsedPayload{Tenant: caller.TenantID, User: "user-target"},
		},
	}

	indexes := types.CanonicalSearchIndexes()
	if len(indexes) != 4 {
		t.Fatalf("canonical index population = %v, want 4 members", indexes)
	}
	for _, axis := range axes {
		for _, index := range indexes {
			t.Run(axis.name+"/"+string(index), func(t *testing.T) {
				t.Parallel()
				rec := &auditRecorder{}
				decision, err := adminDeps(rec).AuthorizeScope(context.Background(), index, caller, axis.req)
				if err != nil {
					t.Fatalf("AuthorizeScope: %v", err)
				}
				if !decision.Elevated {
					t.Fatal("granted widening was not marked elevated")
				}

				got := rec.snapshot()
				if index == types.SearchIndexEvents {
					if len(got) != 0 {
						t.Fatalf("events index emitted through the search sink: %+v; Replay owns its single notice", got)
					}
					return
				}
				if len(got) != 1 {
					t.Fatalf("audit events = %d, want exactly 1: %+v", len(got), got)
				}
				ev := got[0]
				if ev.Type != events.EventTypeAdminScopeUsed {
					t.Errorf("event type = %q, want %q", ev.Type, events.EventTypeAdminScopeUsed)
				}
				if !reflect.DeepEqual(ev.Identity.Identity, caller) {
					t.Errorf("event identity = %+v, want verified caller %+v", ev.Identity.Identity, caller)
				}
				payload, ok := ev.Payload.(events.AdminScopeUsedPayload)
				if !ok {
					t.Fatalf("payload type = %T, want events.AdminScopeUsedPayload", ev.Payload)
				}
				if payload.Tenant != axis.want.Tenant || payload.User != axis.want.User || payload.Session != axis.want.Session {
					t.Errorf("payload target = (%q,%q,%q), want (%q,%q,%q)",
						payload.Tenant, payload.User, payload.Session,
						axis.want.Tenant, axis.want.User, axis.want.Session)
				}
			})
		}
	}
}

func TestAuthorizeScope_NoWidenAndRefusalNeverEmit(t *testing.T) {
	t.Parallel()
	caller := identity.Identity{TenantID: "tenant-own", UserID: "user-own", SessionID: "session-own"}
	for _, index := range types.CanonicalSearchIndexes() {
		t.Run(string(index), func(t *testing.T) {
			t.Parallel()
			t.Run("elevated own scope", func(t *testing.T) {
				rec := &auditRecorder{}
				_, err := adminDeps(rec).AuthorizeScope(context.Background(), index, caller, types.SearchRequest{
					Filter: types.SearchFilter{TenantIDs: []string{caller.TenantID}, UserIDs: []string{caller.UserID}},
				})
				if err != nil {
					t.Fatalf("AuthorizeScope: %v", err)
				}
				if got := len(rec.snapshot()); got != 0 {
					t.Fatalf("no-widen audit events = %d, want 0", got)
				}
			})

			for _, axis := range []struct {
				name string
				req  types.SearchRequest
				want error
			}{
				{name: "tenant", req: types.SearchRequest{Filter: types.SearchFilter{TenantIDs: []string{"tenant-other"}}}, want: search.ErrCrossTenantRequiresAdmin},
				{name: "user", req: types.SearchRequest{Filter: types.SearchFilter{UserIDs: []string{"user-other"}}}, want: search.ErrCrossUserRequiresAdmin},
			} {
				t.Run("refused "+axis.name, func(t *testing.T) {
					rec := &auditRecorder{}
					deps := search.Deps{
						Redactor: patterns.New(), AdminScope: func(context.Context) bool { return false }, Audit: rec.publish,
					}
					_, err := deps.AuthorizeScope(context.Background(), index, caller, axis.req)
					if !errors.Is(err, axis.want) {
						t.Fatalf("AuthorizeScope error = %v, want %v", err, axis.want)
					}
					if got := len(rec.snapshot()); got != 0 {
						t.Fatalf("refused audit events = %d, want 0", got)
					}
				})
			}
		})
	}
}

func TestAuthorizeScope_PrivilegedElidedUserAuditsActualWildcardIndexes(t *testing.T) {
	t.Parallel()
	caller := identity.Identity{TenantID: "tenant-own", UserID: "user-own", SessionID: "session-own"}
	for _, index := range types.CanonicalSearchIndexes() {
		rec := &auditRecorder{}
		_, err := adminDeps(rec).AuthorizeScope(context.Background(), index, caller, types.SearchRequest{})
		if err != nil {
			t.Fatalf("%s: AuthorizeScope: %v", index, err)
		}
		want := 1
		if index == types.SearchIndexEvents {
			want = 0 // same-tenant elision remains folded in this index.
		}
		if got := len(rec.snapshot()); got != want {
			t.Errorf("%s: audit events = %d, want %d", index, got, want)
		}
		if want == 1 {
			payload := rec.snapshot()[0].Payload.(events.AdminScopeUsedPayload)
			if payload.User != "" || payload.Session != "" {
				t.Errorf("%s: elided wildcard payload = %+v, want blank user/session", index, payload)
			}
		}
	}
}

func TestAuthorizeScope_AuditFailureFailsClosed(t *testing.T) {
	t.Parallel()
	caller := identity.Identity{TenantID: "tenant-own", UserID: "user-own", SessionID: "session-own"}
	sinkErr := errors.New("sink unavailable")
	rec := &auditRecorder{err: sinkErr}
	_, err := adminDeps(rec).AuthorizeScope(context.Background(), types.SearchIndexSessions, caller, types.SearchRequest{
		Filter: types.SearchFilter{UserIDs: []string{"user-target"}},
	})
	if !errors.Is(err, search.ErrAuditFailed) || !errors.Is(err, sinkErr) {
		t.Fatalf("AuthorizeScope error = %v, want ErrAuditFailed wrapping sink error", err)
	}
}

type refusingRedactor struct{ err error }

func (r refusingRedactor) Redact(context.Context, any) (any, error) { return nil, r.err }

func TestAuthorizeScope_RedactionFailureNeverPublishes(t *testing.T) {
	t.Parallel()
	caller := identity.Identity{TenantID: "tenant-own", UserID: "user-own", SessionID: "session-own"}
	redactErr := errors.New("redactor unavailable")
	rec := &auditRecorder{}
	deps := search.Deps{
		Redactor:   refusingRedactor{err: redactErr},
		AdminScope: func(context.Context) bool { return true },
		Audit:      rec.publish,
	}
	_, err := deps.AuthorizeScope(context.Background(), types.SearchIndexTasks, caller, types.SearchRequest{
		Filter: types.SearchFilter{TenantIDs: []string{"tenant-target"}},
	})
	if !errors.Is(err, search.ErrAuditFailed) || !errors.Is(err, redactErr) {
		t.Fatalf("AuthorizeScope error = %v, want ErrAuditFailed wrapping redactor error", err)
	}
	if got := len(rec.snapshot()); got != 0 {
		t.Fatalf("published %d events after redaction refusal, want 0", got)
	}
}

type rewritingRedactor struct{}

func (rewritingRedactor) Redact(_ context.Context, payload any) (any, error) {
	view := payload.(map[string]any)
	view["target_tenant"] = "[redacted-tenant]"
	view["target_user"] = "[redacted-user]"
	view["target_session"] = "[redacted-session]"
	return view, nil
}

func TestAuthorizeScope_PublishesRedactedAuditPayload(t *testing.T) {
	t.Parallel()
	caller := identity.Identity{TenantID: "tenant-own", UserID: "user-own", SessionID: "session-own"}
	rec := &auditRecorder{}
	deps := search.Deps{
		Redactor: rewritingRedactor{}, AdminScope: func(context.Context) bool { return true }, Audit: rec.publish,
	}
	_, err := deps.AuthorizeScope(context.Background(), types.SearchIndexArtifacts, caller, types.SearchRequest{
		Filter: types.SearchFilter{UserIDs: []string{"sensitive-target-user"}},
	})
	if err != nil {
		t.Fatalf("AuthorizeScope: %v", err)
	}
	got := rec.snapshot()
	if len(got) != 1 {
		t.Fatalf("audit events = %d, want 1", len(got))
	}
	if !reflect.DeepEqual(got[0].Identity.Identity, caller) {
		t.Errorf("event routing identity = %+v, want verified caller %+v", got[0].Identity.Identity, caller)
	}
	payload := got[0].Payload.(events.AdminScopeUsedPayload)
	if payload.Tenant != "[redacted-tenant]" || payload.User != "[redacted-user]" || payload.Session != "[redacted-session]" {
		t.Fatalf("published payload bypassed redactor output: %+v", payload)
	}
}

func TestAuthorizeScope_ConcurrentReuseKeepsAuditIdentityIsolated(t *testing.T) {
	t.Parallel()
	const n = 128
	indexes := types.CanonicalSearchIndexes()
	rec := &auditRecorder{}
	deps := adminDeps(rec)

	type expected struct {
		tenant string
		user   string
	}
	wantBySession := make(map[string]expected, n)
	var wantMu sync.Mutex
	var wg sync.WaitGroup
	errCh := make(chan error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			caller := identity.Identity{
				TenantID:  fmt.Sprintf("tenant-own-%03d", i),
				UserID:    fmt.Sprintf("user-own-%03d", i),
				SessionID: fmt.Sprintf("session-own-%03d", i),
			}
			ctx, err := identity.With(context.Background(), caller)
			if err != nil {
				errCh <- err
				return
			}
			index := indexes[i%len(indexes)]
			req := types.SearchRequest{}
			want := expected{tenant: caller.TenantID, user: caller.UserID}
			if i%2 == 0 {
				want.tenant = fmt.Sprintf("tenant-target-%03d", i)
				req.Filter = types.SearchFilter{TenantIDs: []string{want.tenant}, UserIDs: []string{caller.UserID}}
			} else {
				want.user = fmt.Sprintf("user-target-%03d", i)
				req.Filter = types.SearchFilter{TenantIDs: []string{caller.TenantID}, UserIDs: []string{want.user}}
			}
			if _, err := deps.AuthorizeScope(ctx, index, caller, req); err != nil {
				errCh <- fmt.Errorf("run %d index %s: %w", i, index, err)
				return
			}
			if index != types.SearchIndexEvents {
				wantMu.Lock()
				wantBySession[caller.SessionID] = want
				wantMu.Unlock()
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}

	got := rec.snapshot()
	if len(got) != len(wantBySession) {
		t.Fatalf("audit events = %d, want %d", len(got), len(wantBySession))
	}
	seen := make(map[string]bool, len(got))
	for _, ev := range got {
		caller := ev.Identity.Identity
		want, ok := wantBySession[caller.SessionID]
		if !ok {
			t.Errorf("unexpected/cross-run caller identity in audit event: %+v", caller)
			continue
		}
		payload, ok := ev.Payload.(events.AdminScopeUsedPayload)
		if !ok {
			t.Errorf("session %s payload type = %T", caller.SessionID, ev.Payload)
			continue
		}
		if payload.Tenant != want.tenant || payload.User != want.user || payload.Session != "" {
			t.Errorf("session %s audit target = (%s,%s,%s), want (%s,%s,%s)",
				caller.SessionID, payload.Tenant, payload.User, payload.Session,
				want.tenant, want.user, "")
		}
		if seen[caller.SessionID] {
			t.Errorf("session %s emitted more than once", caller.SessionID)
		}
		seen[caller.SessionID] = true
	}
}
