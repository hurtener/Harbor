// Package acme_agent implements a Harbor agent named "acme-agent".
//
// This file is the entry point for your agent's logic. The scaffolded
// EchoAgent is a placeholder — replace its body with calls to your
// tools, planner, or LLM client.
//
// EchoAgent implements the public harbortest.Agent interface, so
// agent_test.go exercises it via harbortest.RunOnce out of the box.
// Once your real agent is wired, point the test at it the same way.
package acme_agent

import (
	"context"
	"fmt"

	"github.com/hurtener/Harbor/harbortest"
)

// EchoAgent is the scaffolded example. It echoes whatever input it
// receives. Use it as a template for your own Agent implementations.
type EchoAgent struct{}

// Compile-time check that EchoAgent satisfies harbortest.Agent.
var _ harbortest.Agent = (*EchoAgent)(nil)

// Run implements harbortest.Agent. Replace the body with your agent's
// real logic — typically a planner step, a flow invocation, or a
// hand-rolled call sequence over your tool catalog.
func (a *EchoAgent) Run(ctx context.Context, input any) (any, error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("echo agent: %w", err)
	}
	return input, nil
}
