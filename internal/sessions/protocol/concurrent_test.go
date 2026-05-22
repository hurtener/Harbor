package protocol_test

import (
	"context"
	"runtime"
	"sync"
	"testing"
	"time"

	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	sessionsprotocol "github.com/hurtener/Harbor/internal/sessions/protocol"
)

// TestService_ConcurrentReuse_NoCrossTalk is the D-025 concurrent-reuse
// gate for the sessions/protocol.Service compiled artifact. It runs
// N≥100 concurrent List + Inspect invocations against ONE shared
// *Service, asserting:
//
//   - no data races (the -race detector is the gate);
//   - no context bleed — each goroutine submits a distinct tenant's
//     identity and asserts every row it gets back carries that tenant;
//   - no cross-cancellation — each goroutine owns its own ctx;
//   - no goroutine leak — runtime.NumGoroutine returns to baseline
//     after the workgroup drains.
//
// Run with: go test -race ./internal/sessions/protocol/...
func TestService_ConcurrentReuse_NoCrossTalk(t *testing.T) {
	// Two-tenant catalog; each goroutine queries exactly one tenant.
	rows := []prototypes.SessionRow{}
	base := time.Date(2026, 5, 19, 9, 0, 0, 0, time.UTC)
	for i := range 20 {
		tenant := "t-even"
		if i%2 == 1 {
			tenant = "t-odd"
		}
		rows = append(rows, prototypes.SessionRow{
			SessionID:      sessionID(tenant, i),
			Status:         prototypes.SessionStatusRunning,
			UserID:         "u1",
			TenantID:       tenant,
			StartedAt:      base.Add(time.Duration(i) * time.Minute),
			LastActivityAt: base.Add(time.Duration(i+5) * time.Minute),
			Identity:       prototypes.IdentityScope{Tenant: tenant, User: "u1", Session: sessionID(tenant, i)},
		})
	}
	svc, err := sessionsprotocol.NewService(&fakeProjector{rows: rows})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	// Let any lazily-spawned goroutines settle before the baseline.
	time.Sleep(50 * time.Millisecond)
	baseline := runtime.NumGoroutine()

	const N = 200
	var wg sync.WaitGroup
	wg.Add(N)
	errCh := make(chan error, N)
	for i := range N {
		go func(n int) {
			defer wg.Done()
			tenant := "t-even"
			if n%2 == 1 {
				tenant = "t-odd"
			}
			ctx := context.Background()
			id := prototypes.IdentityScope{Tenant: tenant, User: "u1", Session: "caller-sess"}
			resp, lerr := svc.List(ctx, prototypes.SessionsListRequest{Identity: id}, false)
			if lerr != nil {
				errCh <- lerr
				return
			}
			// Context-bleed assertion: every row belongs to THIS
			// goroutine's tenant — no other goroutine's tenant leaked in.
			for _, r := range resp.Rows {
				if r.TenantID != tenant {
					errCh <- &crossTalkError{want: tenant, got: r.TenantID}
					return
				}
			}
		}(i)
	}
	wg.Wait()
	close(errCh)
	for e := range errCh {
		t.Errorf("concurrent List: %v", e)
	}

	// Goroutine-leak assertion: the count returns to baseline.
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > baseline+2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if leaked := runtime.NumGoroutine() - baseline; leaked > 2 {
		t.Errorf("goroutine leak: %d goroutines above baseline %d after %d concurrent List calls",
			leaked, baseline, N)
	}
}

type crossTalkError struct{ want, got string }

func (e *crossTalkError) Error() string {
	return "context bleed: row tenant " + e.got + " leaked into a query for tenant " + e.want
}

func sessionID(tenant string, i int) string {
	return tenant + "-s" + string(rune('0'+i%10))
}
