package credsource_test

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/audit"
	patternsAudit "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	eventsInmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/llm/credsource"
	"github.com/hurtener/Harbor/internal/llm/credsource/inferencebrokertest"
)

// Documented dummy fixtures — never real secrets (§7 rule 2).
const (
	dummyServiceToken = "dummy-runtime-service-token-not-a-secret"
	dummyProviderKey  = "dummy-openai-provider-key-not-a-secret"
	dummyRotatedKey   = "dummy-rotated-provider-key-not-a-secret"
	authTokenEnv      = "HARBOR_INFERENCE_BROKER_TEST_TOKEN"
	testPrincipal     = "runtime-principal-alpha"
	testProvider      = "openai"
	testBroker        = "openai-broker"
)

type mutClock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *mutClock { return &mutClock{t: time.Unix(1_700_000_000, 0)} }
func (c *mutClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}
func (c *mutClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func mkRedactor() audit.Redactor { return patternsAudit.New() }

func mkBus(t *testing.T, red audit.Redactor) events.EventBus {
	t.Helper()
	b, err := eventsInmem.New(config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     64,
		IdleTimeout:              500 * time.Millisecond,
		DropWindow:               50 * time.Millisecond,
	}, red)
	if err != nil {
		t.Fatalf("events inmem.New: %v", err)
	}
	t.Cleanup(func() { _ = b.Close(context.Background()) })
	return b
}

// runtimePrincipalScope is the reserved runtime-principal identity the pull
// event is published under.
func runtimePrincipalScope() identity.Identity {
	return identity.Identity{
		TenantID:  credsource.RuntimePrincipalTenant,
		UserID:    credsource.RuntimePrincipalUser,
		SessionID: testPrincipal,
	}
}

func subscribeRuntime(t *testing.T, bus events.EventBus) <-chan events.Event {
	t.Helper()
	id := runtimePrincipalScope()
	sub, err := bus.Subscribe(context.Background(), events.Filter{Tenant: id.TenantID, User: id.UserID, Session: id.SessionID})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	t.Cleanup(sub.Cancel)
	return sub.Events()
}

func waitEvent(t *testing.T, ch <-chan events.Event, want events.EventType) events.Event {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case ev := <-ch:
			if ev.Type == want {
				return ev
			}
		case <-deadline:
			t.Fatalf("timed out waiting for event %q", want)
		}
	}
}

func mkSource(t *testing.T, srv *inferencebrokertest.FixtureBroker, live *llm.LiveKey, clock func() time.Time, bus events.EventBus, red audit.Redactor) *credsource.InferenceKeySource {
	t.Helper()
	t.Setenv(authTokenEnv, dummyServiceToken)
	src, err := credsource.NewInferenceKeySource(credsource.Config{
		Provider:         testProvider,
		BrokerName:       testBroker,
		RuntimePrincipal: testPrincipal,
		CredentialURL:    srv.URL(),
		AuthTokenEnv:     authTokenEnv,
		CacheTTL:         5 * time.Minute,
		Live:             live,
		Bus:              bus,
		Redactor:         red,
		Clock:            clock,
	})
	if err != nil {
		t.Fatalf("NewInferenceKeySource: %v", err)
	}
	return src
}

func TestInferenceKeySource_Connect_SeedsLiveKeyAndAudits(t *testing.T) {
	srv := inferencebrokertest.New(t, dummyServiceToken, dummyProviderKey)
	live := llm.NewLiveKey()
	red := mkRedactor()
	bus := mkBus(t, red)
	ch := subscribeRuntime(t, bus)
	src := mkSource(t, srv, live, newClock().now, bus, red)

	if err := src.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if got := live.Get(); got != dummyProviderKey {
		t.Fatalf("live key = %q, want the pulled key", got)
	}
	ev := waitEvent(t, ch, llm.EventTypeProviderCredentialFetched)
	p, ok := ev.Payload.(llm.LLMProviderCredentialFetchedPayload)
	if !ok {
		t.Fatalf("payload type %T", ev.Payload)
	}
	if p.RuntimePrincipal != testPrincipal || p.Broker != testBroker || p.Provider != testProvider {
		t.Fatalf("attribution wrong: %+v", p)
	}
	if p.KeyFingerprint == "" || p.KeyFingerprint == dummyProviderKey {
		t.Fatalf("fingerprint must be a non-reversible digest, never the key: %q", p.KeyFingerprint)
	}
	if p.Phase != "connect" || p.Outcome != "ok" {
		t.Fatalf("phase/outcome: %+v", p)
	}
	// The event is attributed to the runtime principal, NOT a real session.
	if ev.Identity.TenantID != credsource.RuntimePrincipalTenant {
		t.Fatalf("event must ride the reserved runtime scope, got tenant %q", ev.Identity.TenantID)
	}
}

func TestInferenceKeySource_Connect_BrokerDown_FailsLoud_NoLocalFallback(t *testing.T) {
	srv := inferencebrokertest.New(t, dummyServiceToken, dummyProviderKey)
	srv.SetPosture(inferencebrokertest.PostureDown)
	live := llm.NewLiveKey()
	red := mkRedactor()
	src := mkSource(t, srv, live, newClock().now, mkBus(t, red), red)

	err := src.Connect(context.Background())
	if !errors.Is(err, credsource.ErrProviderKeyUnavailable) {
		t.Fatalf("want ErrProviderKeyUnavailable, got %v", err)
	}
	if got := live.Get(); got != "" {
		t.Fatalf("a failed connect must NEVER seed a key (no local fallback), got %q", got)
	}
}

func TestInferenceKeySource_Refresh_FailsLoud_NoStaleCachePastTTL(t *testing.T) {
	srv := inferencebrokertest.New(t, dummyServiceToken, dummyProviderKey)
	live := llm.NewLiveKey()
	clock := newClock()
	red := mkRedactor()
	src := mkSource(t, srv, live, clock.now, mkBus(t, red), red)

	if err := src.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if live.Get() != dummyProviderKey {
		t.Fatalf("connect did not seed")
	}
	// Broker goes unreachable; advance the clock PAST the cache TTL.
	srv.SetPosture(inferencebrokertest.PostureDown)
	clock.advance(6 * time.Minute)
	err := src.Refresh(context.Background())
	if !errors.Is(err, credsource.ErrProviderKeyUnavailable) {
		t.Fatalf("want ErrProviderKeyUnavailable on refresh, got %v", err)
	}
	if got := live.Get(); got != "" {
		t.Fatalf("MUST NOT serve a stale key past its refresh contract; live still = %q", got)
	}
}

func TestInferenceKeySource_Refresh_TransientBeforeTTL_KeepsServing(t *testing.T) {
	srv := inferencebrokertest.New(t, dummyServiceToken, dummyProviderKey)
	live := llm.NewLiveKey()
	clock := newClock()
	red := mkRedactor()
	src := mkSource(t, srv, live, clock.now, mkBus(t, red), red)

	if err := src.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	// A transient blip WELL before the TTL: refresh fails loud, but the still-
	// valid cached key stays served (fail-every-call-on-a-blip is the wrong
	// reading of "no stale past refresh contract").
	srv.SetPosture(inferencebrokertest.PostureDown)
	clock.advance(30 * time.Second)
	if err := src.Refresh(context.Background()); !errors.Is(err, credsource.ErrProviderKeyUnavailable) {
		t.Fatalf("want ErrProviderKeyUnavailable, got %v", err)
	}
	if got := live.Get(); got != dummyProviderKey {
		t.Fatalf("a transient pre-TTL failure must leave the valid cached key served, got %q", got)
	}
}

func TestInferenceKeySource_Rotation_LandsOnAtomicSwap(t *testing.T) {
	srv := inferencebrokertest.New(t, dummyServiceToken, dummyProviderKey)
	live := llm.NewLiveKey()
	clock := newClock()
	red := mkRedactor()
	src := mkSource(t, srv, live, clock.now, mkBus(t, red), red)

	if err := src.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	srv.Rotate(dummyRotatedKey)
	clock.advance(6 * time.Minute)
	if err := src.Refresh(context.Background()); err != nil {
		t.Fatalf("Refresh: %v", err)
	}
	if got := live.Get(); got != dummyRotatedKey {
		t.Fatalf("a broker rotation must land on the next call via the atomic swap (no ReloadConfig), got %q", got)
	}
}

func TestInferenceKeySource_SingleFlight_CollapsesConcurrentPulls(t *testing.T) {
	srv := inferencebrokertest.NewSlow(t, dummyServiceToken, dummyProviderKey, 40*time.Millisecond)
	live := llm.NewLiveKey()
	red := mkRedactor()
	src := mkSource(t, srv, live, newClock().now, mkBus(t, red), red)

	const n = 32
	var wg sync.WaitGroup
	wg.Add(n)
	for range n {
		go func() {
			defer wg.Done()
			_ = src.Connect(context.Background())
		}()
	}
	wg.Wait()
	if hits := srv.Hits(); hits != 1 {
		t.Fatalf("single-flight must collapse the burst onto ONE broker pull, got %d hits", hits)
	}
	if live.Get() != dummyProviderKey {
		t.Fatalf("burst did not seed the key")
	}
}

func TestInferenceKeySource_Close_FailsBoundCallsLoud(t *testing.T) {
	srv := inferencebrokertest.New(t, dummyServiceToken, dummyProviderKey)
	live := llm.NewLiveKey()
	red := mkRedactor()
	src := mkSource(t, srv, live, newClock().now, mkBus(t, red), red)

	if err := src.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	src.Close()
	if got := live.Get(); got != "" {
		t.Fatalf("Close must zero the live key so a bound call fails loud, got %q", got)
	}
	// A pull after Close fails loud (never re-serves the old key).
	if err := src.Refresh(context.Background()); !errors.Is(err, credsource.ErrProviderKeyUnavailable) {
		t.Fatalf("a pull after Close must fail loud, got %v", err)
	}
}

// failingRedactor makes every Redact error so the emit-fail-closed leg fires.
type failingRedactor struct{}

func (failingRedactor) Redact(context.Context, any) (any, error) {
	return nil, errors.New("redactor boom")
}

// toggleRedactor delegates to a real redactor until Fail is set, then errors —
// so a test can let the connect pull audit succeed but make a later refresh's
// audit fail (the W1 stale-drop-on-emit-failure leg).
type toggleRedactor struct {
	inner audit.Redactor
	fail  atomic.Bool
}

func (t *toggleRedactor) Redact(ctx context.Context, p any) (any, error) {
	if t.fail.Load() {
		return nil, errors.New("redactor boom (toggled)")
	}
	return t.inner.Redact(ctx, p)
}

// TestInferenceKeySource_ServeHorizon_HonorsBrokerExpiryShorterThanTTL is the
// F1 gate: a broker key advertised with an `expires_in` SHORTER than the
// operator cacheTTL stops being served at the broker's expiry, not the (much
// longer) cacheTTL — never 10x past the broker's declared validity.
func TestInferenceKeySource_ServeHorizon_HonorsBrokerExpiryShorterThanTTL(t *testing.T) {
	srv := inferencebrokertest.New(t, dummyServiceToken, dummyProviderKey)
	srv.SetExpiresIn(30) // broker: key valid 30s
	live := llm.NewLiveKey()
	clock := newClock()
	red := mkRedactor()
	// cacheTTL (5m) is 10x the broker's 30s — the serve horizon must be 30s.
	t.Setenv(authTokenEnv, dummyServiceToken)
	src, err := credsource.NewInferenceKeySource(credsource.Config{
		Provider: testProvider, BrokerName: testBroker, RuntimePrincipal: testPrincipal,
		CredentialURL: srv.URL(), AuthTokenEnv: authTokenEnv, CacheTTL: 5 * time.Minute,
		Live: live, Bus: mkBus(t, red), Redactor: red, Clock: clock.now,
	})
	if err != nil {
		t.Fatalf("NewInferenceKeySource: %v", err)
	}
	if err := src.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if live.Get() != dummyProviderKey {
		t.Fatalf("connect did not seed")
	}
	// Advance PAST the broker's 30s expiry but WELL WITHIN the 5m cacheTTL.
	// The broker is now down; a refresh at +40s must fail loud AND drop the
	// key — because the serve horizon was the broker's 30s, not the cacheTTL.
	srv.SetPosture(inferencebrokertest.PostureDown)
	clock.advance(40 * time.Second)
	if err := src.Refresh(context.Background()); !errors.Is(err, credsource.ErrProviderKeyUnavailable) {
		t.Fatalf("want ErrProviderKeyUnavailable, got %v", err)
	}
	if got := live.Get(); got != "" {
		t.Fatalf("a key advertised expires_in:30 must NOT serve at +40s (past the broker's validity), got %q", got)
	}
}

// TestInferenceKeySource_EmitFailurePastHorizon_DropsStaleKey is the W1 gate:
// a fresh fetch whose AUDIT emit fails, at a time past the prior key's serve
// horizon, must drop the old key (not leave it live on the hot path).
func TestInferenceKeySource_EmitFailurePastHorizon_DropsStaleKey(t *testing.T) {
	srv := inferencebrokertest.New(t, dummyServiceToken, dummyProviderKey)
	live := llm.NewLiveKey()
	clock := newClock()
	red := &toggleRedactor{inner: mkRedactor()}
	t.Setenv(authTokenEnv, dummyServiceToken)
	src, err := credsource.NewInferenceKeySource(credsource.Config{
		Provider: testProvider, BrokerName: testBroker, RuntimePrincipal: testPrincipal,
		CredentialURL: srv.URL(), AuthTokenEnv: authTokenEnv, CacheTTL: 5 * time.Minute,
		Live: live, Bus: mkBus(t, mkRedactor()), Redactor: red, Clock: clock.now,
	})
	if err != nil {
		t.Fatalf("NewInferenceKeySource: %v", err)
	}
	if err := src.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	// Past the 5m horizon, the broker is UP (fetch succeeds) but the audit emit
	// now fails → the pull must fail loud AND drop the stale key.
	red.fail.Store(true)
	clock.advance(6 * time.Minute)
	if err := src.Refresh(context.Background()); !errors.Is(err, credsource.ErrProviderKeyUnavailable) {
		t.Fatalf("an un-auditable refresh must fail loud, got %v", err)
	}
	if got := live.Get(); got != "" {
		t.Fatalf("an un-auditable fresh pull past the horizon must drop the stale key, got %q", got)
	}
}

// TestInferenceKeySource_ServeHorizon_SelfEnforcesWithoutRefresh is the W2
// gate: once the serve horizon is crossed the key stops serving REGARDLESS of
// the refresh loop — the self-arming backstop timer zeroes it even when no
// Refresh is ever called (a revoked key must not serve if the loop stalls).
// Uses the REAL clock + a short TTL with a bounded eventually-assertion.
func TestInferenceKeySource_ServeHorizon_SelfEnforcesWithoutRefresh(t *testing.T) {
	srv := inferencebrokertest.New(t, dummyServiceToken, dummyProviderKey)
	live := llm.NewLiveKey()
	red := mkRedactor()
	t.Setenv(authTokenEnv, dummyServiceToken)
	src, err := credsource.NewInferenceKeySource(credsource.Config{
		Provider: testProvider, BrokerName: testBroker, RuntimePrincipal: testPrincipal,
		CredentialURL: srv.URL(), AuthTokenEnv: authTokenEnv, CacheTTL: 60 * time.Millisecond,
		Live: live, Bus: mkBus(t, red), Redactor: red, // real clock (no Clock override)
	})
	if err != nil {
		t.Fatalf("NewInferenceKeySource: %v", err)
	}
	if err := src.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if live.Get() != dummyProviderKey {
		t.Fatalf("connect did not seed")
	}
	// No Refresh is ever called; the backstop must zero the key at the horizon.
	deadline := time.After(2 * time.Second)
	for live.Get() != "" {
		select {
		case <-deadline:
			t.Fatalf("the serve-horizon backstop must zero the key without any refresh (loop-stall defense)")
		case <-time.After(5 * time.Millisecond):
		}
	}
	src.Close()
}

// TestInferenceKeySource_Run_RefreshesAheadOfExpiry_CancelJoins exercises the
// Run scheduler: it keeps the key fresh across the horizon and returns cleanly
// on ctx cancel (join-on-shutdown), leaking no goroutine.
func TestInferenceKeySource_Run_RefreshesAheadOfExpiry_CancelJoins(t *testing.T) {
	srv := inferencebrokertest.New(t, dummyServiceToken, dummyProviderKey)
	live := llm.NewLiveKey()
	red := mkRedactor()
	t.Setenv(authTokenEnv, dummyServiceToken)
	src, err := credsource.NewInferenceKeySource(credsource.Config{
		Provider: testProvider, BrokerName: testBroker, RuntimePrincipal: testPrincipal,
		CredentialURL: srv.URL(), AuthTokenEnv: authTokenEnv, CacheTTL: 40 * time.Millisecond,
		Live: live, Bus: mkBus(t, red), Redactor: red, // real clock
	})
	if err != nil {
		t.Fatalf("NewInferenceKeySource: %v", err)
	}
	if err := src.Connect(context.Background()); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	// Baseline AFTER bus reapers + httptest server + connect are up, so the
	// assertion isolates the Run scheduler's goroutine.
	base := runtime.NumGoroutine()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { src.Run(ctx); close(done) }()

	// The scheduler re-pulls ahead of expiry, so the key stays served across
	// several horizons (rotate the broker to prove a rotation lands live).
	srv.Rotate(dummyRotatedKey)
	deadline := time.After(2 * time.Second)
	for live.Get() != dummyRotatedKey {
		select {
		case <-deadline:
			t.Fatalf("Run scheduler did not pick up the rotated key; live=%q", live.Get())
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("Run did not return on ctx cancel (join-on-shutdown)")
	}
	src.Close()
	// Goroutine baseline restored.
	gdeadline := time.After(2 * time.Second)
	for runtime.NumGoroutine() > base+2 {
		select {
		case <-gdeadline:
			t.Fatalf("goroutine leak: base=%d now=%d", base, runtime.NumGoroutine())
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestInferenceKeySource_AuditEmitFailure_FailsPullClosed(t *testing.T) {
	srv := inferencebrokertest.New(t, dummyServiceToken, dummyProviderKey)
	live := llm.NewLiveKey()
	red := failingRedactor{}
	// The bus is built over a working redactor; the source uses the failing one.
	src := mkSource(t, srv, live, newClock().now, mkBus(t, mkRedactor()), red)

	err := src.Connect(context.Background())
	if !errors.Is(err, credsource.ErrProviderKeyUnavailable) {
		t.Fatalf("a pull that cannot be audited must fail loud, got %v", err)
	}
	if got := live.Get(); got != "" {
		t.Fatalf("an un-auditable pull must NOT serve the key, got %q", got)
	}
}

// TestConcurrentReuse_InferenceKeySource_NoCrossRuntimeBleed is the D-025
// reusable-artifact gate: N≥128 concurrent hot-path reads + interleaved
// refreshes against a single shared source under -race, plus a second source
// bound to a second broker asserting no cross-runtime key bleed.
func TestConcurrentReuse_InferenceKeySource_NoCrossRuntimeBleed(t *testing.T) {
	srvA := inferencebrokertest.New(t, dummyServiceToken, "dummy-key-runtime-A-not-a-secret")
	srvB := inferencebrokertest.New(t, dummyServiceToken, "dummy-key-runtime-B-not-a-secret")
	liveA, liveB := llm.NewLiveKey(), llm.NewLiveKey()
	red := mkRedactor()
	bus := mkBus(t, red)

	t.Setenv(authTokenEnv, dummyServiceToken)
	mk := func(srv *inferencebrokertest.FixtureBroker, live *llm.LiveKey, principal string) *credsource.InferenceKeySource {
		s, err := credsource.NewInferenceKeySource(credsource.Config{
			Provider: testProvider, BrokerName: testBroker, RuntimePrincipal: principal,
			// A long TTL keeps the serve-horizon backstop from firing mid-run so
			// the assertion observes a stable per-source key (the test exercises
			// concurrent reads + explicit refreshes, not expiry).
			CredentialURL: srv.URL(), AuthTokenEnv: authTokenEnv, CacheTTL: time.Hour,
			Live: live, Bus: bus, Redactor: red, Clock: time.Now,
		})
		if err != nil {
			t.Fatalf("NewInferenceKeySource: %v", err)
		}
		return s
	}
	srcA := mk(srvA, liveA, "runtime-A")
	srcB := mk(srvB, liveB, "runtime-B")
	if err := srcA.Connect(context.Background()); err != nil {
		t.Fatalf("connect A: %v", err)
	}
	if err := srcB.Connect(context.Background()); err != nil {
		t.Fatalf("connect B: %v", err)
	}
	// Baseline AFTER all long-lived infra (bus reapers, httptest servers) is
	// up + both sources connected — so the assertion isolates goroutines the
	// SOURCE's concurrent refreshes might leak (it spawns none: the
	// single-flight leader runs inline).
	base := runtime.NumGoroutine()

	const n = 160
	var bleed atomic.Int64
	var wg sync.WaitGroup
	wg.Add(n)
	for i := range n {
		go func(i int) {
			defer wg.Done()
			if i%8 == 0 {
				_ = srcA.Refresh(context.Background())
				_ = srcB.Refresh(context.Background())
			}
			// Source A's key must NEVER surface on source B's holder.
			if a := liveA.Get(); a != "" && a == liveB.Get() {
				bleed.Add(1)
			}
			if b := liveB.Get(); b == "dummy-key-runtime-A-not-a-secret" {
				bleed.Add(1)
			}
		}(i)
	}
	wg.Wait()
	if bleed.Load() != 0 {
		t.Fatalf("cross-runtime key bleed detected (%d times)", bleed.Load())
	}
	if liveA.Get() != "dummy-key-runtime-A-not-a-secret" || liveB.Get() != "dummy-key-runtime-B-not-a-secret" {
		t.Fatalf("keys diverged: A=%q B=%q", liveA.Get(), liveB.Get())
	}
	srcA.Close()
	srcB.Close()

	// Goroutine baseline restored (bounded settle).
	deadline := time.After(2 * time.Second)
	for runtime.NumGoroutine() > base+4 {
		select {
		case <-deadline:
			t.Fatalf("goroutine leak: base=%d now=%d", base, runtime.NumGoroutine())
		case <-time.After(10 * time.Millisecond):
		}
	}
}
