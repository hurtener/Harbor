// Package echo is a worked, runnable Harbor agent example.
//
// It implements the public harbortest.Agent interface — the same
// interface `harbor scaffold`'s minimal-react template produces — so
// it can be exercised end-to-end with harbortest.RunOnce without
// constructing an engine graph by hand.
//
// EchoAgent is deliberately trivial (it returns its input unchanged):
// the value of this example is the SHAPE, not the behaviour. Copy this
// package, keep the harbortest.Agent satisfaction and the
// compile-time assertion, and replace Run's body with a planner step,
// a flow invocation, or a hand-rolled tool-call sequence.
//
// See docs/recipes/ for the companion how-to guides and
// examples/tools/ for a worked in-process tool.
package echo

import (
	"context"
	"fmt"

	"github.com/hurtener/Harbor/harbortest"
)

// EchoAgent is the worked example agent. It echoes whatever input it
// receives. Real agents replace Run's body with their own logic.
type EchoAgent struct{}

// Compile-time assertion that EchoAgent satisfies harbortest.Agent.
// Keep this line when you adapt the example — it turns an interface
// drift into a build failure rather than a runtime surprise.
var _ harbortest.Agent = (*EchoAgent)(nil)

// Run implements harbortest.Agent. It honours context cancellation
// (every Harbor agent must — CLAUDE.md §5 "Context") and returns the
// input unchanged.
func (a *EchoAgent) Run(ctx context.Context, input any) (any, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("echo agent: %w", err)
	}
	return input, nil
}
