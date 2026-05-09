package inmem_test

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/audit"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
)

// fakeClock advances manually. Tests that need deterministic reaper
// behaviour use it; tests that want the real clock create the bus
// without WithClock.
type fakeClock struct {
	mu      sync.Mutex
	now     time.Time
	tickers []*fakeTicker
}

func newFakeClock(start time.Time) *fakeClock { return &fakeClock{now: start} }

func (fc *fakeClock) Now() time.Time {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	return fc.now
}

func (fc *fakeClock) NewTicker(d time.Duration) inmem.Ticker {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	ft := &fakeTicker{
		c:    make(chan time.Time, 16),
		d:    d,
		next: fc.now.Add(d),
	}
	fc.tickers = append(fc.tickers, ft)
	return ft
}

func (fc *fakeClock) Advance(d time.Duration) {
	fc.mu.Lock()
	fc.now = fc.now.Add(d)
	now := fc.now
	tickers := append([]*fakeTicker(nil), fc.tickers...)
	fc.mu.Unlock()
	for _, ft := range tickers {
		ft.fire(now)
	}
}

type fakeTicker struct {
	c       chan time.Time
	d       time.Duration
	mu      sync.Mutex
	next    time.Time
	stopped bool
}

func (ft *fakeTicker) Chan() <-chan time.Time { return ft.c }
func (ft *fakeTicker) Stop() {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	ft.stopped = true
}

func (ft *fakeTicker) fire(now time.Time) {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	if ft.stopped {
		return
	}
	for !now.Before(ft.next) {
		select {
		case ft.c <- ft.next:
		default:
		}
		ft.next = ft.next.Add(ft.d)
	}
}

func defaultCfg() config.EventsConfig {
	return config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     8,
		IdleTimeout:              200 * time.Millisecond,
		DropWindow:               50 * time.Millisecond,
	}
}

func newBus(t *testing.T, opts ...inmem.Option) events.EventBus {
	t.Helper()
	bus, err := inmem.New(defaultCfg(), auditpatterns.New(), opts...)
	if err != nil {
		t.Fatalf("inmem.New: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })
	return bus
}

func mkID(seed int) identity.Quadruple {
	return identity.Quadruple{
		Identity: identity.Identity{
			TenantID:  fmt.Sprintf("t-%d", seed),
			UserID:    fmt.Sprintf("u-%d", seed),
			SessionID: fmt.Sprintf("s-%d", seed),
		},
		RunID: fmt.Sprintf("r-%d", seed),
	}
}

func mkEvent(seed int) events.Event {
	return events.Event{
		Type:     events.EventTypeRuntimeError,
		Identity: mkID(seed),
		Payload:  events.SubscriptionIdleClosedPayload{SubscriberID: uint64(seed)},
	}
}

func TestNew_NilRedactorErrors(t *testing.T) {
	_, err := inmem.New(defaultCfg(), nil)
	if err == nil {
		t.Fatal("New with nil redactor returned nil error")
	}
}

func TestNew_InvalidConfigErrors(t *testing.T) {
	cases := []func(*config.EventsConfig){
		func(c *config.EventsConfig) { c.MaxSubscribersPerSession = 0 },
		func(c *config.EventsConfig) { c.SubscriberBufferSize = 0 },
		func(c *config.EventsConfig) { c.IdleTimeout = 0 },
		func(c *config.EventsConfig) { c.DropWindow = 0 },
	}
	for i, mut := range cases {
		c := defaultCfg()
		mut(&c)
		_, err := inmem.New(c, auditpatterns.New())
		if err == nil {
			t.Errorf("case %d: New accepted invalid config %+v", i, c)
		}
	}
}

func TestPublishSubscribe_RoundTrip(t *testing.T) {
	bus := newBus(t)
	id := mkID(1)
	sub, err := bus.Subscribe(context.Background(), events.Filter{
		Tenant:  id.TenantID,
		User:    id.UserID,
		Session: id.SessionID,
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Cancel()

	for i := 0; i < 5; i++ {
		if err := bus.Publish(context.Background(), mkEvent(1)); err != nil {
			t.Fatalf("Publish: %v", err)
		}
	}

	got := drainN(t, sub, 5, time.Second)
	if len(got) != 5 {
		t.Fatalf("got %d events, want 5", len(got))
	}
	for i := 1; i < len(got); i++ {
		if got[i].Sequence <= got[i-1].Sequence {
			t.Errorf("sequences not monotonic: got[%d]=%d got[%d]=%d", i-1, got[i-1].Sequence, i, got[i].Sequence)
		}
	}
}

func TestPublish_RejectsCallerProvidedSequence(t *testing.T) {
	bus := newBus(t)
	ev := mkEvent(1)
	ev.Sequence = 999
	err := bus.Publish(context.Background(), ev)
	if !errors.Is(err, events.ErrSequenceProvided) {
		t.Fatalf("err=%v, want ErrSequenceProvided", err)
	}
}

func TestPublish_RejectsUnknownEventType(t *testing.T) {
	bus := newBus(t)
	ev := mkEvent(1)
	ev.Type = "made.up"
	err := bus.Publish(context.Background(), ev)
	if !errors.Is(err, events.ErrUnknownEventType) {
		t.Fatalf("err=%v, want ErrUnknownEventType", err)
	}
}

func TestPublish_RejectsMissingIdentity(t *testing.T) {
	bus := newBus(t)
	ev := mkEvent(1)
	ev.Identity.TenantID = ""
	err := bus.Publish(context.Background(), ev)
	if !errors.Is(err, events.ErrIdentityRequired) {
		t.Fatalf("err=%v, want ErrIdentityRequired", err)
	}
}

func TestSubscribe_RejectsEmptyTripleNonAdmin(t *testing.T) {
	bus := newBus(t)
	cases := []events.Filter{
		{},
		{Tenant: "T"},
		{Tenant: "T", User: "U"},
	}
	for _, f := range cases {
		_, err := bus.Subscribe(context.Background(), f)
		if !errors.Is(err, events.ErrIdentityScopeRequired) {
			t.Errorf("filter %+v: err=%v, want ErrIdentityScopeRequired", f, err)
		}
	}
}

func TestSubscribe_AdminBypassesTriple_AndAuditEmits(t *testing.T) {
	bus := newBus(t)
	// Admin subscriber picks up everything including its own audit.
	admin, err := bus.Subscribe(context.Background(), events.Filter{Admin: true})
	if err != nil {
		t.Fatalf("admin Subscribe: %v", err)
	}
	defer admin.Cancel()

	// Read at least one event — should be the AdminScopeUsed audit.
	got := drainN(t, admin, 1, time.Second)
	if len(got) != 1 {
		t.Fatalf("admin received %d events, want 1", len(got))
	}
	if got[0].Type != events.EventTypeAdminScopeUsed {
		t.Errorf("first event type=%v, want %v", got[0].Type, events.EventTypeAdminScopeUsed)
	}
}

func TestSubscribe_PerSessionLimit(t *testing.T) {
	cfg := defaultCfg()
	cfg.MaxSubscribersPerSession = 2
	bus, err := inmem.New(cfg, auditpatterns.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })

	id := mkID(1)
	f := events.Filter{Tenant: id.TenantID, User: id.UserID, Session: id.SessionID}
	subs := []events.Subscription{}
	for i := 0; i < 2; i++ {
		s, err := bus.Subscribe(context.Background(), f)
		if err != nil {
			t.Fatalf("subscribe %d: %v", i, err)
		}
		subs = append(subs, s)
	}
	_, err = bus.Subscribe(context.Background(), f)
	if !errors.Is(err, events.ErrSubscriberLimitReached) {
		t.Fatalf("err=%v, want ErrSubscriberLimitReached", err)
	}
	for _, s := range subs {
		s.Cancel()
	}
	// Cap should free up after Cancel.
	s, err := bus.Subscribe(context.Background(), f)
	if err != nil {
		t.Fatalf("re-subscribe after cancel: %v", err)
	}
	s.Cancel()
}

func TestPublish_CrossTenantIsolation(t *testing.T) {
	bus := newBus(t)
	idA := mkID(1)
	subA, err := bus.Subscribe(context.Background(), events.Filter{
		Tenant: idA.TenantID, User: idA.UserID, Session: idA.SessionID,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer subA.Cancel()

	// Publish 50 events as tenant B.
	for i := 0; i < 50; i++ {
		evB := mkEvent(2)
		if err := bus.Publish(context.Background(), evB); err != nil {
			t.Fatalf("publish B: %v", err)
		}
	}

	// A should see ZERO events. Wait briefly to let any cross-talk surface.
	select {
	case ev := <-subA.Events():
		if ev.Identity.TenantID != "" {
			t.Errorf("subscriber A leaked event from tenant %q (event=%+v)", ev.Identity.TenantID, ev)
		}
	case <-time.After(150 * time.Millisecond):
		// Expected: no cross-tenant delivery.
	}
}

func TestPublish_DropOldestEmitsBusDropped(t *testing.T) {
	cfg := defaultCfg()
	cfg.SubscriberBufferSize = 4
	cfg.DropWindow = 10 * time.Millisecond
	bus, err := inmem.New(cfg, auditpatterns.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })

	id := mkID(1)
	sub, err := bus.Subscribe(context.Background(), events.Filter{
		Tenant: id.TenantID, User: id.UserID, Session: id.SessionID,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Cancel()

	// Publish far more than the buffer holds without consuming.
	for i := 0; i < 50; i++ {
		_ = bus.Publish(context.Background(), mkEvent(1))
	}
	// Trigger the windowed emit.
	time.Sleep(20 * time.Millisecond)
	_ = bus.Publish(context.Background(), mkEvent(1))

	// Drain the channel and look for at least one bus.dropped notice.
	deadline := time.After(time.Second)
	sawDropped := false
	for !sawDropped {
		select {
		case ev, ok := <-sub.Events():
			if !ok {
				goto done
			}
			if ev.Type == events.EventTypeBusDropped {
				sawDropped = true
				if p, ok := ev.Payload.(events.BusDroppedPayload); !ok {
					t.Errorf("BusDropped payload type=%T, want BusDroppedPayload", ev.Payload)
				} else if p.DroppedCount == 0 {
					t.Errorf("BusDroppedPayload.DroppedCount=0; expected > 0")
				}
			}
		case <-deadline:
			goto done
		}
	}
done:
	if !sawDropped {
		t.Errorf("did not observe bus.dropped event under saturation")
	}
}

// TestReaper_CancelsIdleSubscription_SaturatedConsumer exercises the
// reaper's CONSUMER-IDLE semantic: a subscriber whose buffer is full
// AND has not had a clean (non-displacing) enqueue for IdleTimeout
// is reaped. A "clean enqueue" means the event landed without
// displacing an older one — i.e., the consumer was keeping up.
//
// The test uses real-time short timeouts (rather than fakeClock)
// because the assertion is inherently racy against fakeClock: a
// fast consumer that starts reading immediately after Advance can
// drain the buffer before the reaper sees `len(ch) > 0`, in which
// case the reaper correctly does NOT reap (the consumer is keeping
// up). To observe the saturated-reap path deterministically we
// must keep the consumer idle until the reaper has had a chance to
// observe the saturation; real-time + sleep is the cleanest shape.
func TestReaper_CancelsIdleSubscription_SaturatedConsumer(t *testing.T) {
	cfg := defaultCfg()
	cfg.SubscriberBufferSize = 4
	cfg.IdleTimeout = 80 * time.Millisecond
	bus, err := inmem.New(cfg, auditpatterns.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })

	id := mkID(1)
	sub, err := bus.Subscribe(context.Background(), events.Filter{
		Tenant: id.TenantID, User: id.UserID, Session: id.SessionID,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Saturate without consuming. Past the 4th publish the displace
	// path freezes lastDrain.
	for i := 0; i < 12; i++ {
		_ = bus.Publish(context.Background(), mkEvent(1))
	}

	// Wait past IdleTimeout WITHOUT reading sub.Events(). The
	// reaper's ticker (IdleTimeout/4 = 20ms) fires repeatedly; the
	// first tick whose `now - lastDrain >= IdleTimeout` AND
	// `len(s.ch) > 0` reaps. After ~3 IdleTimeouts of real time we
	// know the reap has fired.
	time.Sleep(250 * time.Millisecond)

	// NOW start reading. Channel should already be closed and the
	// closing notice (or one of the saturating events) should be
	// queued.
	deadline := time.After(2 * time.Second)
	sawClosed := false
	closedCh := false
	for !closedCh {
		select {
		case ev, ok := <-sub.Events():
			if !ok {
				closedCh = true
				continue
			}
			if ev.Type == events.EventTypeBusSubscriptionIdleClosed {
				sawClosed = true
			}
		case <-deadline:
			t.Fatal("subscription channel never closed within 2s")
		}
	}
	if !sawClosed {
		t.Error("did not observe SubscriptionIdleClosed before channel close")
	}
}

// TestReaper_DoesNotReapQuietConsumer pins the OTHER half of the
// new semantic: a consumer whose channel is empty (because the bus
// is quiet) is NOT reaped, even after IdleTimeout elapses. Reaping
// healthy "I'm just waiting" subscribers would be a regression.
func TestReaper_DoesNotReapQuietConsumer(t *testing.T) {
	clk := newFakeClock(time.Unix(0, 0).UTC())
	cfg := defaultCfg()
	cfg.IdleTimeout = 100 * time.Millisecond
	bus, err := inmem.New(cfg, auditpatterns.New(), inmem.WithClock(clk))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })

	id := mkID(1)
	sub, err := bus.Subscribe(context.Background(), events.Filter{
		Tenant: id.TenantID, User: id.UserID, Session: id.SessionID,
	})
	if err != nil {
		t.Fatal(err)
	}

	// No Publish → channel stays empty.
	clk.Advance(500 * time.Millisecond)

	// Give the reaper goroutine a real-time chance to (incorrectly)
	// fire. If our non-empty-buffer guard works, nothing happens.
	select {
	case ev, ok := <-sub.Events():
		if !ok {
			t.Fatal("quiet subscriber's channel was closed by reaper")
		}
		if ev.Type == events.EventTypeBusSubscriptionIdleClosed {
			t.Fatalf("reaper fired on quiet subscriber: %+v", ev)
		}
	case <-time.After(150 * time.Millisecond):
		// Expected: no reap on quiet bus.
	}
}

func TestPublish_AfterClose_ReturnsBusClosed(t *testing.T) {
	bus := newBus(t)
	if err := bus.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	err := bus.Publish(context.Background(), mkEvent(1))
	if !errors.Is(err, events.ErrBusClosed) {
		t.Fatalf("Publish err=%v, want ErrBusClosed", err)
	}
	_, err = bus.Subscribe(context.Background(), events.Filter{Tenant: "T", User: "U", Session: "S"})
	if !errors.Is(err, events.ErrBusClosed) {
		t.Fatalf("Subscribe err=%v, want ErrBusClosed", err)
	}
}

func TestClose_Idempotent(t *testing.T) {
	bus := newBus(t)
	if err := bus.Close(context.Background()); err != nil {
		t.Fatalf("Close 1: %v", err)
	}
	if err := bus.Close(context.Background()); err != nil {
		t.Fatalf("Close 2: %v", err)
	}
}

// TestBus_CloseDuringActivePublish stresses the race between a
// publisher mid-enqueue and Close cancelling the subscription. The
// driver uses non-blocking sends only, so a "send on closed channel"
// panic would surface here under -race; the test asserts no panic
// and no deadlock.
func TestBus_CloseDuringActivePublish(t *testing.T) {
	bus := newBus(t)
	id := mkID(1)
	sub, err := bus.Subscribe(context.Background(), events.Filter{
		Tenant: id.TenantID, User: id.UserID, Session: id.SessionID,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Drain in the background to keep the buffer churning.
	drainDone := make(chan struct{})
	go func() {
		defer close(drainDone)
		for range sub.Events() {
		}
	}()

	// Publishers running concurrently with Close.
	stop := make(chan struct{})
	var pwg sync.WaitGroup
	for p := 0; p < 8; p++ {
		pwg.Add(1)
		go func() {
			defer pwg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_ = bus.Publish(context.Background(), mkEvent(1))
			}
		}()
	}

	// Let publishers run briefly, then close.
	time.Sleep(20 * time.Millisecond)
	if err := bus.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	close(stop)
	pwg.Wait()
	<-drainDone

	// Re-publish after close — must return ErrBusClosed, not panic.
	err = bus.Publish(context.Background(), mkEvent(1))
	if !errors.Is(err, events.ErrBusClosed) {
		t.Fatalf("post-close Publish err=%v, want ErrBusClosed", err)
	}
}

// boomRedactor errors on every Redact — used to test the
// audit.redaction_failed sibling-emit contract.
type boomRedactor struct{}

func (boomRedactor) Redact(_ context.Context, _ any) (any, error) {
	return nil, fmt.Errorf("%w: forced", audit.ErrRedactionFailed)
}

// notSafePayload is an EventPayload that does NOT implement
// SafePayload, so the bus runs it through the redactor.
type notSafePayload struct {
	events.Sealed
	APIKey string
}

func TestPublish_RedactionFailure_EmitsSibling(t *testing.T) {
	bus, err := inmem.New(defaultCfg(), boomRedactor{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })

	// Admin subscriber picks up the audit.redaction_failed event.
	adminSub, err := bus.Subscribe(context.Background(), events.Filter{Admin: true})
	if err != nil {
		t.Fatal(err)
	}
	defer adminSub.Cancel()

	id := mkID(1)
	ev := events.Event{
		Type:     events.EventTypeRuntimeError,
		Identity: id,
		Payload:  notSafePayload{APIKey: "leak-me-not"},
	}
	err = bus.Publish(context.Background(), ev)
	if err == nil {
		t.Fatal("Publish returned nil error for failing redaction")
	}

	// Drain admin sub — first event is the AdminScopeUsed audit; we
	// look further for the redaction_failed event.
	deadline := time.After(time.Second)
	sawFailure := false
	for !sawFailure {
		select {
		case got := <-adminSub.Events():
			if got.Type == events.EventTypeAuditRedactionFailed {
				sawFailure = true
				p, ok := got.Payload.(events.AuditRedactionFailedPayload)
				if !ok {
					t.Errorf("payload type=%T, want AuditRedactionFailedPayload", got.Payload)
				}
				if p.OriginalType != events.EventTypeRuntimeError {
					t.Errorf("OriginalType=%v, want runtime.error", p.OriginalType)
				}
			}
		case <-deadline:
			goto done
		}
	}
done:
	if !sawFailure {
		t.Error("did not observe audit.redaction_failed event")
	}
}

func TestPublish_RedactsNonSafePayloadIntoMap(t *testing.T) {
	bus := newBus(t)
	id := mkID(1)
	sub, err := bus.Subscribe(context.Background(), events.Filter{
		Tenant: id.TenantID, User: id.UserID, Session: id.SessionID,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Cancel()

	ev := events.Event{
		Type:     events.EventTypeRuntimeError,
		Identity: id,
		Payload:  notSafePayload{APIKey: "real-secret"},
	}
	if err := bus.Publish(context.Background(), ev); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	got := drainN(t, sub, 1, time.Second)
	if len(got) != 1 {
		t.Fatalf("got %d events, want 1", len(got))
	}
	rm, ok := got[0].Payload.(events.RedactedMap)
	if !ok {
		t.Fatalf("payload type=%T, want RedactedMap", got[0].Payload)
	}
	if v, _ := rm.Data["api_key"].(string); v == "real-secret" {
		t.Errorf("api_key not redacted in RedactedMap: %v", rm.Data)
	}
}

func TestPublish_SafePayloadBypassesRedactor(t *testing.T) {
	// boomRedactor errors on every call. SafePayload bypasses it, so
	// publish should succeed.
	bus, err := inmem.New(defaultCfg(), boomRedactor{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })

	id := mkID(1)
	sub, err := bus.Subscribe(context.Background(), events.Filter{
		Tenant: id.TenantID, User: id.UserID, Session: id.SessionID,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Cancel()

	// SubscriptionIdleClosedPayload is a SafePayload.
	ev := events.Event{
		Type:     events.EventTypeBusSubscriptionIdleClosed,
		Identity: id,
		Payload:  events.SubscriptionIdleClosedPayload{SubscriberID: 7},
	}
	if err := bus.Publish(context.Background(), ev); err != nil {
		t.Fatalf("Publish (safe payload) failed: %v", err)
	}
	got := drainN(t, sub, 1, time.Second)
	if len(got) != 1 {
		t.Fatalf("got %d, want 1", len(got))
	}
	if _, ok := got[0].Payload.(events.SubscriptionIdleClosedPayload); !ok {
		t.Errorf("safe payload was redacted; type=%T", got[0].Payload)
	}
}

// Concurrent-reuse contract test (D-025).
func TestBus_ConcurrentReuse_ReuseContract(t *testing.T) {
	bus := newBus(t)
	const publishers = 64
	const subscribers = 16
	const eventsPerProducer = 32

	// Subscribers spread across distinct identity tuples.
	ids := make([]identity.Quadruple, subscribers)
	subs := make([]events.Subscription, subscribers)
	for i := 0; i < subscribers; i++ {
		ids[i] = mkID(i)
		s, err := bus.Subscribe(context.Background(), events.Filter{
			Tenant: ids[i].TenantID, User: ids[i].UserID, Session: ids[i].SessionID,
		})
		if err != nil {
			t.Fatal(err)
		}
		subs[i] = s
	}

	// Drain goroutines per subscriber: count, count cross-talk.
	var wg sync.WaitGroup
	mismatches := atomic.Int64{}
	for i, s := range subs {
		wg.Add(1)
		go func(i int, s events.Subscription) {
			defer wg.Done()
			for ev := range s.Events() {
				if ev.Type == events.EventTypeBusDropped {
					continue
				}
				if ev.Identity.TenantID != ids[i].TenantID ||
					ev.Identity.UserID != ids[i].UserID ||
					ev.Identity.SessionID != ids[i].SessionID {
					mismatches.Add(1)
				}
			}
		}(i, s)
	}

	// Publishers each emit eventsPerProducer events with random identity.
	var pwg sync.WaitGroup
	for p := 0; p < publishers; p++ {
		pwg.Add(1)
		go func(p int) {
			defer pwg.Done()
			for j := 0; j < eventsPerProducer; j++ {
				ev := mkEvent(p % subscribers)
				_ = bus.Publish(context.Background(), ev)
			}
		}(p)
	}
	pwg.Wait()

	// Cancel all subs; their channels close; drain goroutines exit.
	for _, s := range subs {
		s.Cancel()
	}
	wg.Wait()

	if n := mismatches.Load(); n != 0 {
		t.Fatalf("%d cross-tenant deliveries observed", n)
	}
}

func TestBus_GoroutineLeak_AfterClose(t *testing.T) {
	baseline := runtime.NumGoroutine()

	cfg := defaultCfg()
	bus, err := inmem.New(cfg, auditpatterns.New())
	if err != nil {
		t.Fatal(err)
	}
	id := mkID(1)
	sub, err := bus.Subscribe(context.Background(), events.Filter{
		Tenant: id.TenantID, User: id.UserID, Session: id.SessionID,
	})
	if err != nil {
		t.Fatal(err)
	}

	// Saturate.
	for i := 0; i < 32; i++ {
		_ = bus.Publish(context.Background(), mkEvent(1))
	}

	// Drain so the channel is empty when we Close.
	go func() {
		for range sub.Events() {
		}
	}()

	if err := bus.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > baseline && time.Now().Before(deadline) {
		runtime.Gosched()
		time.Sleep(10 * time.Millisecond)
	}
	if delta := runtime.NumGoroutine() - baseline; delta > 0 {
		t.Errorf("goroutine leak: baseline=%d, after=%d", baseline, runtime.NumGoroutine())
	}
}

// drainN reads up to n events from sub, with a per-event timeout.
// Returns however many it got before timeout.
func drainN(t *testing.T, sub events.Subscription, n int, timeout time.Duration) []events.Event {
	t.Helper()
	out := make([]events.Event, 0, n)
	for len(out) < n {
		select {
		case ev, ok := <-sub.Events():
			if !ok {
				return out
			}
			out = append(out, ev)
		case <-time.After(timeout):
			return out
		}
	}
	return out
}
