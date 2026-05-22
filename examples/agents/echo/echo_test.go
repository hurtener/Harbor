// echo_test.go — worked example of testing a Harbor agent through the
// public harbortest package. It is also the build/validate gate for
// the examples/ tree: `go test ./examples/...` exercises this file in
// CI (the `examples` job), so a drift in the harbortest surface that
// breaks the example fails the build.
package echo

import (
	"context"
	"testing"

	"github.com/hurtener/Harbor/harbortest"
)

// TestEchoAgent_RoundTrips drives EchoAgent through harbortest.RunOnce
// under the kit's canonical identity quadruple and asserts the output
// round-trips. AssertNoLeaks is the cross-session-isolation gate: it
// stays green for an event-free agent and turns red the moment a real
// agent emits an event under a foreign (tenant, user, session) triple.
func TestEchoAgent_RoundTrips(t *testing.T) {
	agent := &EchoAgent{}

	out, log, err := harbortest.RunOnce(context.Background(), agent, "hello")
	if err != nil {
		t.Fatalf("RunOnce: unexpected error: %v", err)
	}
	if out != "hello" {
		t.Errorf("RunOnce: output = %v, want %q", out, "hello")
	}

	harbortest.AssertNoLeaks(t, log)
}

// TestEchoAgent_HonoursCancellation proves the failure-mode path: a
// cancelled context surfaces a wrapped error instead of a silent
// success (CLAUDE.md §5 "Fail loudly").
func TestEchoAgent_HonoursCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := (&EchoAgent{}).Run(ctx, "hello"); err == nil {
		t.Fatal("Run: expected error on cancelled context, got nil")
	}
}
