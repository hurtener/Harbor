package auth_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
)

// sessionReachStub returns a stubValidator whose Verified carries the
// given session reach (nil = absent claim) and a token default session.
func sessionReachStub(defaultSession string, reach []string) *stubValidator {
	return &stubValidator{verified: auth.Verified{
		Identity:     identity.Identity{TenantID: "dev", UserID: "dev", SessionID: defaultSession},
		SessionReach: reach,
	}}
}

// perTokenValidator returns a Verified per raw token from a read-only
// map. It is race-free by construction (no mutation after build), so ONE
// shared middleware instance can serve N concurrent requests with
// distinct per-request authority — the shape the D-025 concurrent-reuse
// contract requires.
type perTokenValidator struct {
	results map[string]auth.Verified
}

func (v *perTokenValidator) Validate(_ context.Context, token string) (auth.Verified, error) {
	res, ok := v.results[token]
	if !ok {
		return auth.Verified{}, errors.New("unknown token")
	}
	return res, nil
}

// TestMiddleware_SessionReach_ConcurrentReuse_N128 pins the D-025
// concurrent-reuse contract for the shared middleware under session
// reach: ONE decorator serves N≥128 concurrent requests with distinct
// per-request reach + effective session, the race detector is live, and
// the goroutine baseline is restored at teardown. Each goroutine's
// effective session must be its own — no context bleed across runs.
func TestMiddleware_SessionReach_ConcurrentReuse_N128(t *testing.T) {
	const n = 128
	results := make(map[string]auth.Verified, n)
	tokens := make([]string, n)
	for i := range n {
		mySession := fmt.Sprintf("sess-%04d", i)
		tok := fmt.Sprintf("token-%04d", i)
		tokens[i] = tok
		results[tok] = auth.Verified{
			Identity:     identity.Identity{TenantID: "dev", UserID: "dev", SessionID: mySession},
			SessionReach: []string{mySession},
		}
	}
	mw := auth.Middleware(&perTokenValidator{results: results})

	settleGoroutines()
	baseline := runtime.NumGoroutine()

	var (
		wg    sync.WaitGroup
		start = make(chan struct{})
		errs  = make([]error, n)
	)
	for i := range n {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start
			mySession := fmt.Sprintf("sess-%04d", idx)
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/v1/events", nil)
			req.Header.Set("Authorization", "Bearer "+tokens[idx])
			req.Header.Set(auth.HeaderSession, mySession)
			mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				id, ok := identity.From(r.Context())
				if !ok {
					errs[idx] = errors.New("no identity on ctx")
					return
				}
				if id.SessionID != mySession {
					errs[idx] = fmt.Errorf("context bleed: got session %q, want %q", id.SessionID, mySession)
				}
			})).ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				errs[idx] = fmt.Errorf("status %d (body=%q)", rec.Code, rec.Body.String())
			}
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d: %v", i, err)
		}
	}

	settleGoroutines()
	post := runtime.NumGoroutine()
	if post > baseline+2 {
		t.Errorf("goroutine leak: baseline=%d, post=%d (drift=%d)", baseline, post, post-baseline)
	}
}

// TestMiddleware_SessionReach_SharedMux_ConcurrentMix exercises ONE
// shared middleware instance across a mix of allowed / denied / absent
// reach requests under -race, asserting no cross-talk: a denied request
// never reaches the handler and an allowed request resolves its own
// session.
func TestMiddleware_SessionReach_SharedMux_ConcurrentMix(t *testing.T) {
	const n = 100
	results := make(map[string]auth.Verified, n)
	allowed := make([]bool, n)
	for i := range n {
		mySession := fmt.Sprintf("mix-%04d", i)
		tok := fmt.Sprintf("mix-token-%04d", i)
		// Mix the three authority shapes:
		//   - %3==0: out-of-reach (present reach, non-member) → denied
		//   - %5==0: explicit empty reach → denied
		//   - %7==0: absent claim (nil) → allowed, dynamic selection
		//   - else:  in-reach member → allowed
		var reach []string
		allow := true
		switch {
		case i%5 == 0:
			reach = []string{}
			allow = false
		case i%3 == 0:
			reach = []string{"other-session"}
			allow = false
		case i%7 == 0:
			reach = nil // absent — dynamic selection preserved
		default:
			reach = []string{mySession}
		}
		allowed[i] = allow
		results[tok] = auth.Verified{
			Identity:     identity.Identity{TenantID: "dev", UserID: "dev", SessionID: mySession},
			SessionReach: reach,
		}
	}
	mw := auth.Middleware(&perTokenValidator{results: results})

	var (
		wg    sync.WaitGroup
		start = make(chan struct{})
		errs  = make([]error, n)
	)
	for i := range n {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			<-start
			mySession := fmt.Sprintf("mix-%04d", idx)
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/v1/events", nil)
			req.Header.Set("Authorization", "Bearer "+fmt.Sprintf("mix-token-%04d", idx))
			req.Header.Set(auth.HeaderSession, mySession)
			mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				id, _ := identity.From(r.Context())
				w.Header().Set("X-Session", id.SessionID)
				w.WriteHeader(http.StatusOK)
			})).ServeHTTP(rec, req)
			if allowed[idx] {
				if rec.Code != http.StatusOK {
					errs[idx] = fmt.Errorf("allowed request got %d (body=%q)", rec.Code, rec.Body.String())
					return
				}
				if got := rec.Header().Get("X-Session"); got != mySession {
					errs[idx] = fmt.Errorf("context bleed: handler saw session %q, want %q", got, mySession)
				}
			} else if rec.Code != http.StatusForbidden {
				errs[idx] = fmt.Errorf("denied request got %d, want 403", rec.Code)
			}
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Errorf("goroutine %d: %v", i, err)
		}
	}
}

// TestMiddleware_SessionReach_AbsentClaim_PreservesDynamicSelection —
// D-409: no session_reach claim means D-171's per-request dynamic
// session selection is preserved exactly. Both the X-Harbor-Session
// override and the token-default fallback pass.
func TestMiddleware_SessionReach_AbsentClaim_PreservesDynamicSelection(t *testing.T) {
	stub := sessionReachStub("default-session", nil)
	mw := auth.Middleware(stub)

	// Header override to an arbitrary session passes.
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v1/events", nil)
	r.Header.Set("Authorization", "Bearer faketoken")
	r.Header.Set(auth.HeaderSession, "conversation-Z")
	mw(echoHandler(t)).ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("header override status: got %d, want 200 (body=%q)", w.Code, w.Body.String())
	}
	body := decodeEchoBody(t, w)
	if body["session"] != "conversation-Z" {
		t.Errorf("session: got %v, want conversation-Z", body["session"])
	}

	// No header: the token's default session claim is used and passes.
	w2 := httptest.NewRecorder()
	r2 := httptest.NewRequest(http.MethodGet, "/v1/events", nil)
	r2.Header.Set("Authorization", "Bearer faketoken")
	mw(echoHandler(t)).ServeHTTP(w2, r2)
	if w2.Code != http.StatusOK {
		t.Fatalf("default-session status: got %d, want 200 (body=%q)", w2.Code, w2.Body.String())
	}
	body2 := decodeEchoBody(t, w2)
	if body2["session"] != "default-session" {
		t.Errorf("session: got %v, want default-session", body2["session"])
	}
}

// TestMiddleware_SessionReach_AllowedEffectiveSessionPasses — a request
// whose effective session (header-selected) is a member of the signed
// reach set proceeds.
func TestMiddleware_SessionReach_AllowedEffectiveSessionPasses(t *testing.T) {
	stub := sessionReachStub("default-session", []string{"sess-a", "sess-b"})
	mw := auth.Middleware(stub)

	for _, sess := range []string{"sess-a", "sess-b"} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/v1/events", nil)
		r.Header.Set("Authorization", "Bearer faketoken")
		r.Header.Set(auth.HeaderSession, sess)
		mw(echoHandler(t)).ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("session %q status: got %d, want 200 (body=%q)", sess, w.Code, w.Body.String())
		}
	}
}

// TestMiddleware_SessionReach_DifferentHeaderSessionFails — a header
// session outside the signed reach is refused fail-closed (403
// scope_mismatch) before any handler side effect.
func TestMiddleware_SessionReach_DifferentHeaderSessionFails(t *testing.T) {
	stub := sessionReachStub("default-session", []string{"sess-a"})
	mw := auth.Middleware(stub)
	called := false

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v1/events", nil)
	r.Header.Set("Authorization", "Bearer faketoken")
	r.Header.Set(auth.HeaderSession, "sess-evil")
	mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	})).ServeHTTP(w, r)

	if called {
		t.Fatal("handler ran for an out-of-reach session — enforcement must precede side effects")
	}
	if w.Code != http.StatusForbidden {
		t.Fatalf("status: got %d, want 403", w.Code)
	}
	perr := readErrorBody(t, w)
	if perr.Code != protoerrors.CodeScopeMismatch {
		t.Errorf("error code: got %q, want %q", perr.Code, protoerrors.CodeScopeMismatch)
	}
}

// TestMiddleware_SessionReach_DefaultSessionChecked — with no
// X-Harbor-Session header, the token's default session claim is the
// effective session and MUST be a reach member: inside passes, outside
// is refused.
func TestMiddleware_SessionReach_DefaultSessionChecked(t *testing.T) {
	// Default inside reach: passes.
	inside := sessionReachStub("sess-default", []string{"sess-default"})
	mwOK := auth.Middleware(inside)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v1/events", nil)
	r.Header.Set("Authorization", "Bearer faketoken")
	mwOK(echoHandler(t)).ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("default-in-reach status: got %d, want 200 (body=%q)", w.Code, w.Body.String())
	}

	// Default outside reach: refused 403 even without a header.
	outside := sessionReachStub("sess-default", []string{"sess-other"})
	mwDeny := auth.Middleware(outside)
	w2 := httptest.NewRecorder()
	r2 := httptest.NewRequest(http.MethodGet, "/v1/events", nil)
	r2.Header.Set("Authorization", "Bearer faketoken")
	mwDeny(echoHandler(t)).ServeHTTP(w2, r2)
	if w2.Code != http.StatusForbidden {
		t.Fatalf("default-outside-reach status: got %d, want 403 (body=%q)", w2.Code, w2.Body.String())
	}
}

// TestMiddleware_SessionReach_ExplicitEmptyDenies — a present but
// explicitly empty session_reach claim grants no session: every request
// (header-selected or default) is refused 403.
func TestMiddleware_SessionReach_ExplicitEmptyDenies(t *testing.T) {
	stub := sessionReachStub("default-session", []string{})
	mw := auth.Middleware(stub)
	for _, tc := range []struct {
		name   string
		header string
	}{
		{name: "header session", header: "sess-a"},
		{name: "default session", header: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodGet, "/v1/events", nil)
			r.Header.Set("Authorization", "Bearer faketoken")
			if tc.header != "" {
				r.Header.Set(auth.HeaderSession, tc.header)
			}
			mw(echoHandler(t)).ServeHTTP(w, r)
			if w.Code != http.StatusForbidden {
				t.Fatalf("status: got %d, want 403 (body=%q)", w.Code, w.Body.String())
			}
			perr := readErrorBody(t, w)
			if perr.Code != protoerrors.CodeScopeMismatch {
				t.Errorf("error code: got %q, want %q", perr.Code, protoerrors.CodeScopeMismatch)
			}
		})
	}
}

// TestMiddleware_SessionReach_SSEProjectionCannotEscape — the SSE
// access-token path promotes `?access_token=` and `?session=` onto the
// cloned request, and the middleware enforces the reach against that
// promoted session exactly like the header path. A `?session=` outside
// the reach is refused 403; one inside passes.
func TestMiddleware_SessionReach_SSEProjectionCannotEscape(t *testing.T) {
	stub := sessionReachStub("default-session", []string{"sess-allowed"})
	mw := auth.Middleware(stub)
	h := auth.SSEAccessTokenShim(mw(echoHandler(t)))

	// Out-of-reach session via the SSE query projection: refused.
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v1/events?access_token=faketoken&session=sess-evil", nil)
	h.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("SSE out-of-reach status: got %d, want 403 (body=%q)", w.Code, w.Body.String())
	}

	// In-reach session via the SSE query projection: passes.
	w2 := httptest.NewRecorder()
	r2 := httptest.NewRequest(http.MethodGet, "/v1/events?access_token=faketoken&session=sess-allowed", nil)
	h.ServeHTTP(w2, r2)
	if w2.Code != http.StatusOK {
		t.Fatalf("SSE in-reach status: got %d, want 200 (body=%q)", w2.Code, w2.Body.String())
	}
	body := decodeEchoBody(t, w2)
	if body["session"] != "sess-allowed" {
		t.Errorf("SSE session: got %v, want sess-allowed", body["session"])
	}

	// SSE default (no ?session=): the token default claim is checked
	// and refused when outside the reach.
	w3 := httptest.NewRecorder()
	r3 := httptest.NewRequest(http.MethodGet, "/v1/events?access_token=faketoken", nil)
	h.ServeHTTP(w3, r3)
	if w3.Code != http.StatusForbidden {
		t.Fatalf("SSE default-out-of-reach status: got %d, want 403 (body=%q)", w3.Code, w3.Body.String())
	}
}

// TestMiddleware_SessionReach_MalformedClaim_RejectedAtAuthentication —
// the malformed-claim path is an authentication failure surfaced by the
// validator, and the middleware maps it to 401 auth_rejected (never
// reaching the handler).
func TestMiddleware_SessionReach_MalformedClaim_RejectedAtAuthentication(t *testing.T) {
	stub := &stubValidator{err: auth.ErrSessionReachMalformed}
	mw := auth.Middleware(stub)
	called := false

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/v1/events", nil)
	r.Header.Set("Authorization", "Bearer faketoken")
	mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	})).ServeHTTP(w, r)

	if called {
		t.Fatal("handler ran for a malformed session_reach claim")
	}
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d, want 401", w.Code)
	}
	perr := readErrorBody(t, w)
	if perr.Code != protoerrors.CodeAuthRejected {
		t.Errorf("error code: got %q, want %q", perr.Code, protoerrors.CodeAuthRejected)
	}
	if !strings.Contains(perr.Message, "session_reach_malformed") {
		t.Errorf("wire message should carry the session_reach_malformed reason, got %q", perr.Message)
	}
}
