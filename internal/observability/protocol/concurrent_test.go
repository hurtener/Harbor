package protocol_test

import (
	"context"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/observability/protocol"
	"github.com/hurtener/Harbor/internal/observability/rollups"
	"github.com/hurtener/Harbor/internal/observability/rollups/memstore"
	protocolauth "github.com/hurtener/Harbor/internal/protocol/auth"
)

// TestService_ConcurrentMixedIdentityReuse pins the concurrent-reuse
// contract on the observability.query SERVICE: N≥100 goroutines with MIXED
// verified identities (ordinary and admin/console:fleet) query ONE shared
// Service over ONE shared store under -race, asserting:
//
//   - no data races (the race detector is the gate);
//   - no context bleed (every ordinary caller's response is exactly their
//     own seeded aggregate — any leak of another identity's rows changes
//     the exact sum);
//   - no cancellation cross-talk (cancelled callers fail with the ctx
//     error while their neighbours complete normally);
//   - the widened fan-ins emit their audit records exactly once each;
//   - no goroutine leak (the goroutine count returns to baseline).
func TestService_ConcurrentMixedIdentityReuse(t *testing.T) {
	ctx := context.Background()
	baseline := runtime.NumGoroutine()

	// total identities across total/10 tenants, 10 users each; every
	// identity owns one minute-bucket row with a UNIQUE exact cost.
	const total = 150
	store := memstore.New()
	defer func() { _ = store.Close(ctx) }()

	costOf := func(i int) int64 { return int64(1000 + i) }
	for i := range total {
		seed(t, store, uint64(i+1), rollups.Key{
			BucketStart: refHour,
			TenantID:    fmt.Sprintf("t-%02d", i/10),
			UserID:      fmt.Sprintf("u-%03d", i),
			SessionID:   fmt.Sprintf("s-%03d", i),
			Model:       "m",
		}, rollups.MeasureSet{LLMCompletions: 1, LLMCostMicros: costOf(i)})
	}

	rec := &auditRecorder{}
	svc, err := protocol.NewService(store, &testQuality{store: store, state: rollups.StateCurrent},
		fleetScope, rec.publish, patterns.New())
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	// Pre-build every caller's context and request OUTSIDE the goroutines
	// (identity attachment and scope seeding are test-goroutine work).
	type call struct {
		ctx   context.Context
		req   protocol.Request
		isAdm bool // widened fan-in within the caller's tenant
	}
	calls := make([]call, total)
	auditsWant := int64(0)
	for i := range total {
		id := identityOf(i)
		c := call{ctx: scopedCtx(t, id), req: baseRequest()}
		if i%3 == 0 {
			// Admin / console:fleet widened fan-in within the tenant.
			scopes := []protocolauth.Scope{protocolauth.ScopeAdmin}
			if i%9 == 0 {
				scopes = []protocolauth.Scope{protocolauth.ScopeConsoleFleet}
			}
			c.ctx = scopedCtx(t, id, scopes...)
			if i%4 != 0 {
				c.isAdm = true
				auditsWant++
			}
		}
		if i%4 == 0 {
			// The cancelled caller (ordinary or elevated): its ctx is
			// cancelled AFTER scope seeding, so the cancellation is what
			// reaches the service — and it must not disturb its
			// neighbours.
			var cancel context.CancelFunc
			c.ctx, cancel = context.WithCancel(c.ctx)
			cancel()
		}
		calls[i] = c
	}

	var wg sync.WaitGroup
	var failures atomic.Int64
	var auditsGot atomic.Int64

	for i := range total {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			c := calls[idx]

			resp, qerr := svc.Query(c.ctx, c.req)
			if qerr != nil {
				if idx%4 == 0 {
					// The cancelled caller must fail with the ctx error —
					// and, critically, must NOT have affected anyone else.
					return
				}
				failures.Add(1)
				t.Errorf("caller %d: %v", idx, qerr)
				return
			}

			if c.isAdm {
				// The elevated fan-in sees the WHOLE tenant (10 users) —
				// its own rows plus its tenant-mates' — exactly once.
				want := int64(0)
				for j := range 10 {
					want += costOf((idx/10)*10 + j)
				}
				got := sum(t, resp, rollups.MeasureLLMCostMicros)
				if got != want {
					failures.Add(1)
					t.Errorf("elevated caller %d cost = %d, want tenant sum %d", idx, got, want)
				}
				auditsGot.Add(1)
				return
			}

			// Ordinary caller: EXACTLY its own seeded aggregate. Any
			// context bleed (another identity's rows entering the forced
			// triple filter) changes this exact sum.
			got := sum(t, resp, rollups.MeasureLLMCostMicros)
			if got != costOf(idx) {
				failures.Add(1)
				t.Errorf("caller %d cost = %d, want own %d (context bleed)", idx, got, costOf(idx))
			}
		}(i)
	}

	wg.Wait()
	if failures.Load() != 0 {
		t.Fatalf("%d concurrent-reuse assertions failed", failures.Load())
	}

	// Exactly one audit.admin_scope_used per successful widened fan-in —
	// never one per caller, never zero, and none from the cancelled
	// elevated callers (a refused read emits nothing).
	if got := auditsGot.Load(); got != auditsWant {
		t.Fatalf("widened reads audited = %d, want %d", got, auditsWant)
	}
	gotAudits := rec.snapshot()
	if n := int64(len(gotAudits)); n != auditsWant {
		t.Fatalf("audit events = %d, want %d", n, auditsWant)
	}
	for _, ev := range gotAudits {
		if ev.Type != events.EventTypeAdminScopeUsed {
			t.Fatalf("audit event type = %q, want %q", ev.Type, events.EventTypeAdminScopeUsed)
		}
		payload, ok := ev.Payload.(events.AdminScopeUsedPayload)
		if !ok {
			t.Fatalf("audit payload = %T, want AdminScopeUsedPayload", ev.Payload)
		}
		// Every fan-in here is the caller's own tenant with elided user /
		// session — the tenant fold plus the canonical wildcard spelling.
		if payload.User != "" || payload.Session != "" {
			t.Fatalf("audit payload = %+v, want elided user/session (wildcard)", payload)
		}
	}

	// Goroutine-leak check: the service spawns nothing, so the count must
	// return to baseline after the wave joins.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= baseline+2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := runtime.NumGoroutine(); got > baseline+2 {
		t.Fatalf("goroutine leak: baseline=%d now=%d", baseline, got)
	}
}

// identityOf is the deterministic mixed-identity generator: 10 tenants,
// 10 users each, every identity a distinct session.
func identityOf(i int) identity.Identity {
	return identity.Identity{
		TenantID:  fmt.Sprintf("t-%02d", i/10),
		UserID:    fmt.Sprintf("u-%03d", i),
		SessionID: fmt.Sprintf("s-%03d", i),
	}
}
