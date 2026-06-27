// example_test.go — runnable godoc example for the sdk/steering facade:
// the per-run control inbox registry and the scoped, validated control
// instructions an operator submits to steer a live run. The example
// body imports only the public sdk/steering + sdk/identity facades —
// the copyable code never reaches into internal/.
package steering_test

import (
	"fmt"
	"log"

	"github.com/hurtener/Harbor/sdk/identity"
	"github.com/hurtener/Harbor/sdk/steering"
)

// Example shows the steering facade's primary entry surface: a Registry
// hands out one Inbox per run (keyed by the run's identity Quadruple);
// a caller Enqueues a scoped, validated ControlEvent; the run loop
// Drains it. Here a CANCEL — which requires at least the owning-user
// scope (RFC §6.3) — is submitted and drained back. Enqueue fails closed
// on an identity mismatch, an insufficient scope, or an invalid payload,
// so a misrouted or under-privileged control never reaches a run.
func Example() {
	reg := steering.NewRegistry()

	run := identity.Quadruple{
		Identity: identity.Identity{TenantID: "acme", UserID: "u-1", SessionID: "s-1"},
		RunID:    "run-1",
	}

	inbox, err := reg.Open(run)
	if err != nil {
		log.Fatalf("open inbox: %v", err)
	}
	defer func() { _ = reg.Retire(run) }()

	// A bare CANCEL carries no payload. CallerScope/CallerTenant are what
	// the Protocol edge derived from the submitting caller's JWT.
	err = inbox.Enqueue(steering.ControlEvent{
		Type:         steering.ControlCancel,
		Identity:     run,
		CallerScope:  steering.ScopeOwnerUser,
		CallerTenant: "acme",
	})
	if err != nil {
		fmt.Println("enqueue:", err)
		return
	}

	events, err := inbox.Drain()
	if err != nil {
		fmt.Println("drain:", err)
		return
	}
	fmt.Println(events[0].Type)
	// Output: CANCEL
}
