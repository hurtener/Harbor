package benchmarks

import (
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/runtime/steering"
)

// steeringBenchRun is the run quadruple the steering benchmark targets.
// Documented dummy identity — no secrets (CLAUDE.md §13).
func steeringBenchRun() identity.Quadruple {
	return identity.Quadruple{
		Identity: identity.Identity{TenantID: "bench-tenant", UserID: "bench-user", SessionID: "bench-session"},
		RunID:    "bench-run",
	}
}

// BenchmarkSteeringApply_EnqueueDrain measures the steering ingress hot
// path against the REAL per-run Inbox (no mocks — CLAUDE.md §13): one
// CANCEL control submitted through Inbox.Enqueue (which runs the full
// identity gate + scope check + payload validation) and immediately
// drained, the round-trip the Protocol edge and the run loop perform per
// steering instruction. A single shared Inbox is reused across
// iterations — the Inbox is a compiled artifact, so per-iteration state
// stays on the stack (the rebuilt event) and never on the Inbox.
func BenchmarkSteeringApply_EnqueueDrain(b *testing.B) {
	q := steeringBenchRun()
	reg := steering.NewRegistry()
	inbox, err := reg.Open(q)
	if err != nil {
		b.Fatalf("Registry.Open: %v", err)
	}

	// A valid CANCEL: owner_user scope from the run's own tenant, soft
	// mode. EnqueuedAt stays zero (the Inbox owns the timeline); Enqueue
	// takes the event by value, so reuse across iterations is safe.
	ev := steering.ControlEvent{
		Type:         steering.ControlCancel,
		Identity:     q,
		CallerScope:  steering.ScopeOwnerUser,
		CallerTenant: q.TenantID,
		Payload:      map[string]any{"mode": "soft"},
	}

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if err := inbox.Enqueue(ev); err != nil {
			b.Fatalf("Enqueue: %v", err)
		}
		if _, err := inbox.Drain(); err != nil {
			b.Fatalf("Drain: %v", err)
		}
	}
}
