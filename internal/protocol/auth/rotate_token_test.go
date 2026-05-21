package auth_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	"github.com/hurtener/Harbor/internal/protocol/types"
)

// fakeIssuer is the in-test auth.TokenIssuer. It re-mints a
// deterministic token string keyed on the identity so the
// concurrent-reuse test can assert no per-run cross-talk. Defined in a
// *_test.go file — the production TokenIssuer is the `harbor dev` /
// `harbor console` dev signer (CLAUDE.md §13).
type fakeIssuer struct {
	calls   atomic.Int64
	failNow bool
}

func (f *fakeIssuer) IssueToken(_ context.Context, id identity.Identity, _ []auth.Scope, now time.Time) (string, time.Time, error) {
	f.calls.Add(1)
	if f.failNow {
		return "", time.Time{}, errors.New("fake issuer: forced failure")
	}
	// The token deterministically encodes the identity so the
	// concurrent-reuse test can prove run A's token never reaches run B.
	return "token-for-" + id.TenantID + "-" + id.UserID + "-" + id.SessionID, now.Add(24 * time.Hour), nil
}

// rotateCapturingBus counts published events; concurrency-safe so the
// concurrent-reuse test can share one instance.
type rotateCapturingBus struct {
	mu     sync.Mutex
	events []events.Event
}

func (b *rotateCapturingBus) Publish(_ context.Context, ev events.Event) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.events = append(b.events, ev)
	return nil
}
func (b *rotateCapturingBus) Subscribe(context.Context, events.Filter) (events.Subscription, error) {
	return nil, nil
}
func (b *rotateCapturingBus) Close(context.Context) error { return nil }
func (b *rotateCapturingBus) count() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.events)
}

func adminVerified() auth.Verified {
	return auth.Verified{
		Identity: identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"},
		Scopes:   []auth.Scope{auth.ScopeAdmin},
	}
}

func TestNewRotateSurface_FailsLoudlyOnNilDeps(t *testing.T) {
	if _, err := auth.NewRotateSurface(nil, testNoopRedactor{}); !errors.Is(err, auth.ErrRotateMisconfigured) {
		t.Errorf("NewRotateSurface(nil issuer) err = %v, want ErrRotateMisconfigured", err)
	}
	if _, err := auth.NewRotateSurface(&fakeIssuer{}, nil); !errors.Is(err, auth.ErrRotateMisconfigured) {
		t.Errorf("NewRotateSurface(nil redactor) err = %v, want ErrRotateMisconfigured", err)
	}
}

func TestRotate_HappyPath_AdminScope(t *testing.T) {
	iss := &fakeIssuer{}
	bus := &rotateCapturingBus{}
	s, err := auth.NewRotateSurface(iss, testNoopRedactor{}, auth.WithRotateBus(bus))
	if err != nil {
		t.Fatalf("NewRotateSurface: %v", err)
	}
	resp, err := s.Rotate(context.Background(), adminVerified(), types.AuthRotateTokenRequest{})
	if err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	if resp.NewToken != "token-for-t1-u1-s1" {
		t.Errorf("NewToken = %q, want token-for-t1-u1-s1", resp.NewToken)
	}
	if resp.ExpiresAt.IsZero() {
		t.Error("ExpiresAt is zero, want a real expiry")
	}
	if bus.count() != 1 {
		t.Errorf("Rotate published %d events, want 1 audit.admin_scope_used", bus.count())
	}
	if bus.events[0].Type != events.EventTypeAdminScopeUsed {
		t.Errorf("event type = %q, want %q", bus.events[0].Type, events.EventTypeAdminScopeUsed)
	}
}

func TestRotate_RejectsWithoutAdminScope(t *testing.T) {
	s, err := auth.NewRotateSurface(&fakeIssuer{}, testNoopRedactor{})
	if err != nil {
		t.Fatalf("NewRotateSurface: %v", err)
	}
	v := auth.Verified{
		Identity: identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"},
		Scopes:   []auth.Scope{auth.ScopeConsoleFleet}, // not admin
	}
	_, err = s.Rotate(context.Background(), v, types.AuthRotateTokenRequest{})
	if !errors.Is(err, auth.ErrRotateScopeRequired) {
		t.Errorf("Rotate without admin scope err = %v, want ErrRotateScopeRequired", err)
	}
}

func TestRotate_RejectsIncompleteIdentity(t *testing.T) {
	s, _ := auth.NewRotateSurface(&fakeIssuer{}, testNoopRedactor{})
	v := auth.Verified{
		Identity: identity.Identity{TenantID: "t1", UserID: "", SessionID: "s1"},
		Scopes:   []auth.Scope{auth.ScopeAdmin},
	}
	_, err := s.Rotate(context.Background(), v, types.AuthRotateTokenRequest{})
	if !errors.Is(err, auth.ErrRotateIdentityRequired) {
		t.Errorf("Rotate with incomplete identity err = %v, want ErrRotateIdentityRequired", err)
	}
}

func TestRotate_RejectsBodyIdentityMismatch(t *testing.T) {
	s, _ := auth.NewRotateSurface(&fakeIssuer{}, testNoopRedactor{})
	// Body claims a DIFFERENT tenant than the verified JWT.
	req := types.AuthRotateTokenRequest{
		Identity: types.IdentityScope{Tenant: "other-tenant", User: "u1", Session: "s1"},
	}
	_, err := s.Rotate(context.Background(), adminVerified(), req)
	if !errors.Is(err, auth.ErrRotateIdentityMismatch) {
		t.Errorf("Rotate with body-identity mismatch err = %v, want ErrRotateIdentityMismatch", err)
	}
}

func TestRotate_AcceptsEmptyBodyIdentity(t *testing.T) {
	// An elided body identity is filled from the verified JWT — must NOT
	// be a mismatch.
	s, _ := auth.NewRotateSurface(&fakeIssuer{}, testNoopRedactor{})
	_, err := s.Rotate(context.Background(), adminVerified(), types.AuthRotateTokenRequest{})
	if err != nil {
		t.Errorf("Rotate with elided body identity err = %v, want nil", err)
	}
}

func TestRotate_IssuerFailureFailsLoudly(t *testing.T) {
	s, _ := auth.NewRotateSurface(&fakeIssuer{failNow: true}, testNoopRedactor{})
	_, err := s.Rotate(context.Background(), adminVerified(), types.AuthRotateTokenRequest{})
	if !errors.Is(err, auth.ErrRotateIssueFailed) {
		t.Errorf("Rotate with failing issuer err = %v, want ErrRotateIssueFailed", err)
	}
}

// TestRotate_ConcurrentReuse_NoCrossTalk runs N≥100 concurrent Rotate
// invocations against a single shared RotateSurface, each with a
// distinct identity, asserting (a) no data race (the -race gate),
// (b) no context bleed — each run's response token encodes exactly its
// own identity (D-025).
func TestRotate_ConcurrentReuse_NoCrossTalk(t *testing.T) {
	iss := &fakeIssuer{}
	bus := &rotateCapturingBus{}
	s, err := auth.NewRotateSurface(iss, testNoopRedactor{}, auth.WithRotateBus(bus))
	if err != nil {
		t.Fatalf("NewRotateSurface: %v", err)
	}
	const n = 128
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tenant := "tenant-" + itoa(i)
			v := auth.Verified{
				Identity: identity.Identity{TenantID: tenant, UserID: "u", SessionID: "s"},
				Scopes:   []auth.Scope{auth.ScopeAdmin},
			}
			resp, e := s.Rotate(context.Background(), v, types.AuthRotateTokenRequest{})
			if e != nil {
				errs[i] = e
				return
			}
			want := "token-for-" + tenant + "-u-s"
			if resp.NewToken != want {
				errs[i] = errors.New("context bleed: got " + resp.NewToken + " want " + want)
			}
		}(i)
	}
	wg.Wait()
	for i, e := range errs {
		if e != nil {
			t.Errorf("run %d: %v", i, e)
		}
	}
	if iss.calls.Load() != n {
		t.Errorf("issuer called %d times, want %d", iss.calls.Load(), n)
	}
	if bus.count() != n {
		t.Errorf("bus received %d events, want %d", bus.count(), n)
	}
}

// itoa is a tiny base-10 int formatter — avoids importing strconv just
// for the concurrent-reuse test's identity labels.
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [12]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(b[pos:])
}
