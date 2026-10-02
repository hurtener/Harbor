package auth_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sync"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	"github.com/hurtener/Harbor/internal/protocol/methods"
)

func TestParseMethodReach_StrictCanonicalArray(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  any
	}{
		{"null", nil},
		{"typed nil", []any(nil)},
		{"scalar", string(methods.MethodStart)},
		{"object", map[string]any{string(methods.MethodStart): true}},
		{"boolean", true},
		{"number", float64(1)},
		{"nonstring element", []any{float64(1)}},
		{"null element", []any{nil}},
		{"nested array", []any{[]any{string(methods.MethodStart)}}},
		{"duplicate", []any{string(methods.MethodStart), string(methods.MethodStart)}},
		{"unknown", []any{"future.method"}},
		{"alias", []any{"control.start"}},
		{"path alias", []any{"/v1/control/start"}},
		{"uppercase", []any{"START"}},
		{"whitespace", []any{" start "}},
		{"wildcard", []any{"*"}},
		{"blank", []any{""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := auth.ParseMethodReach(tc.raw); !errors.Is(err, auth.ErrMethodReachMalformed) {
				t.Fatalf("ParseMethodReach(%#v) = %v", tc.raw, err)
			}
		})
	}
	all := methods.Methods()
	raw := make([]any, len(all))
	for i, method := range all {
		raw[i] = string(method)
	}
	got, err := auth.ParseMethodReach(raw)
	if err != nil || !reflect.DeepEqual(got, all) {
		t.Fatalf("canonical method set = %#v, %v", got, err)
	}
	empty, err := auth.ParseMethodReach([]any{})
	if err != nil || empty == nil || len(empty) != 0 {
		t.Fatalf("explicit empty reach = %#v, %v", empty, err)
	}
}

func TestAuthorizeMethod_AbsentEmptyAndNarrowed(t *testing.T) {
	for _, tc := range []struct {
		name string
		ctx  context.Context
		want map[methods.Method]bool
	}{
		{"absent", context.Background(), nil},
		{"empty", auth.WithMethodReach(context.Background(), []methods.Method{}), map[methods.Method]bool{}},
		{"explicit nil", auth.WithMethodReach(context.Background(), nil), map[methods.Method]bool{}},
		{"limited", auth.WithMethodReach(context.Background(), []methods.Method{methods.MethodStart}), map[methods.Method]bool{methods.MethodStart: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Admin never bypasses the restriction.
			ctx := auth.WithScopes(tc.ctx, []auth.Scope{auth.ScopeAdmin, auth.ScopeConsoleFleet})
			for _, method := range methods.Methods() {
				err := auth.AuthorizeMethod(ctx, method)
				allowed := tc.want == nil || tc.want[method]
				if allowed && err != nil || !allowed && !errors.Is(err, auth.ErrMethodReachDenied) {
					t.Fatalf("AuthorizeMethod(%q) = %v, allowed %v", method, err, allowed)
				}
			}
			if err := auth.AuthorizeMethod(ctx, methods.Method("control.start")); !errors.Is(err, auth.ErrMethodReachDenied) {
				t.Fatalf("noncanonical method = %v", err)
			}
		})
	}
	ctx := auth.WithMethodReach(context.Background(), []methods.Method{methods.MethodStart})
	if err := auth.AuthorizeMethod(ctx, methods.MethodStart); err != nil {
		t.Fatal(err)
	}
	if auth.HasScope(ctx, auth.ScopeAdmin) {
		t.Fatal("method membership must not mint scope")
	}
	if err := auth.NewAgentReachAuthorizer().AuthorizeAgentReach(ctx, "agent-a"); !errors.Is(err, auth.ErrAgentReachDenied) {
		t.Fatalf("method membership must not mint agent reach: %v", err)
	}
	ctx = auth.WithSessionReach(ctx, []string{"session-a"})
	if err := auth.NewSessionReachAuthorizer().AuthorizeSessionReach(ctx, "session-b"); !errors.Is(err, auth.ErrSessionReachDenied) {
		t.Fatalf("method membership must not widen session reach: %v", err)
	}
}

func TestMethodReach_ContextDefensiveCopies(t *testing.T) {
	reach := []methods.Method{methods.MethodStart}
	ctx := auth.WithMethodReach(context.Background(), reach)
	reach[0] = methods.MethodCancel
	got, present := auth.MethodReachFrom(ctx)
	if !present || !reflect.DeepEqual(got, []methods.Method{methods.MethodStart}) {
		t.Fatalf("attached reach changed: %#v, %v", got, present)
	}
	got[0] = methods.MethodCancel
	if err := auth.AuthorizeMethod(ctx, methods.MethodStart); err != nil {
		t.Fatalf("returned copy mutated context: %v", err)
	}
	if _, present := auth.MethodReachFrom(context.Background()); present {
		t.Fatal("absent claim became present")
	}
}

func TestValidator_MethodReach_SignedClaim(t *testing.T) {
	v, priv := newScopedRSValidator(t)
	for _, tc := range []struct {
		name    string
		present bool
		raw     any
		want    []methods.Method
		bad     bool
	}{
		{name: "absent"},
		{name: "empty", present: true, raw: []string{}, want: []methods.Method{}},
		{name: "allowed", present: true, raw: []string{string(methods.MethodStart)}, want: []methods.Method{methods.MethodStart}},
		{name: "null", present: true, raw: nil, bad: true},
		{name: "wrong shape", present: true, raw: string(methods.MethodStart), bad: true},
		{name: "unknown", present: true, raw: []string{"future.method"}, bad: true},
		{name: "duplicate", present: true, raw: []string{string(methods.MethodStart), string(methods.MethodStart)}, bad: true},
		{name: "alias", present: true, raw: []string{"control.start"}, bad: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			claims := validClaims(fixedNow)
			claims["aud"] = legacyTestAudience
			if tc.present {
				claims["aud"] = scopedTestAudience
				claims[auth.MethodReachClaim] = tc.raw
			}
			verified, err := v.Validate(context.Background(), signRS256(t, priv, claims, "k1"))
			if tc.bad {
				if !errors.Is(err, auth.ErrMethodReachMalformed) {
					t.Fatalf("Validate = %v", err)
				}
				return
			}
			if err != nil || !reflect.DeepEqual(verified.MethodReach, tc.want) {
				t.Fatalf("verified method reach = %#v, %v", verified.MethodReach, err)
			}
		})
	}
}

func TestMiddleware_MethodReach_SameBroadTokenRetainsDynamicSessions(t *testing.T) {
	v, priv := newScopedRSValidator(t)
	for _, restricted := range []bool{false, true} {
		claims := validClaims(fixedNow)
		claims["aud"] = legacyTestAudience
		if restricted {
			claims["aud"] = scopedTestAudience
			claims[auth.MethodReachClaim] = []string{string(methods.MethodStart)}
		}
		token := signRS256(t, priv, claims, "k1")
		for _, session := range []string{"session-a", "session-b"} {
			for _, method := range []methods.Method{methods.MethodStart, methods.MethodCancel} {
				handler := auth.Middleware(v)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					id, ok := identity.FromVerified(r.Context())
					if !ok || id.SessionID != session {
						t.Errorf("effective identity = %#v, %v", id, ok)
					}
					_, present := auth.MethodReachFrom(r.Context())
					if present != restricted {
						t.Errorf("method reach presence = %v, restricted %v", present, restricted)
					}
					if !auth.HasScope(r.Context(), auth.ScopeAdmin) {
						t.Error("existing admin scope was removed")
					}
					if err := auth.AuthorizeMethod(r.Context(), method); err != nil {
						w.WriteHeader(http.StatusForbidden)
						return
					}
					w.WriteHeader(http.StatusNoContent)
				}))
				req := httptest.NewRequest(http.MethodPost, "/v1/control/"+string(method), nil)
				req.Header.Set("Authorization", "Bearer "+token)
				req.Header.Set(auth.HeaderSession, session)
				rec := httptest.NewRecorder()
				handler.ServeHTTP(rec, req)
				want := http.StatusNoContent
				if restricted && method != methods.MethodStart {
					want = http.StatusForbidden
				}
				if rec.Code != want {
					t.Fatalf("restricted=%v session=%q method=%q: %d, want %d; %s", restricted, session, method, rec.Code, want, rec.Body.String())
				}
			}
		}
	}
}

func TestMethodReach_ConcurrentRequestIsolation(t *testing.T) {
	const count = 128
	root := context.Background()
	var wg sync.WaitGroup
	failures := make(chan error, count)
	for i := range count {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			allowed, denied := methods.MethodStart, methods.MethodCancel
			if i%2 != 0 {
				allowed, denied = denied, allowed
			}
			ctx, cancel := context.WithCancel(root)
			defer cancel()
			ctx = auth.WithMethodReach(ctx, []methods.Method{allowed})
			if i%3 == 0 {
				cancel()
			}
			if auth.AuthorizeMethod(ctx, allowed) != nil || !errors.Is(auth.AuthorizeMethod(ctx, denied), auth.ErrMethodReachDenied) {
				failures <- fmt.Errorf("request %d method authority bled", i)
			}
		}(i)
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		t.Error(err)
	}
	if _, present := auth.MethodReachFrom(root); present || root.Err() != nil {
		t.Fatal("request authority or cancellation leaked to parent")
	}
}
