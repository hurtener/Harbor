package llm

import (
	"context"
	"sync/atomic"
)

type runUsageKey struct{}

// RunUsage accumulates the prompt and completion tokens of every model call
// made under one run's context, including context maintenance and any tool
// that calls the composed client with the run context. It is created per run
// and carried on that run's ctx, never stored on a reusable client.
type RunUsage struct {
	prompt     atomic.Int64
	completion atomic.Int64
	calls      atomic.Int64
}

// WithRunUsage returns a child context carrying a fresh run usage
// accumulator. An existing accumulator is reused so nested helpers do not
// split one run's total.
func WithRunUsage(ctx context.Context) (context.Context, *RunUsage) {
	if u := RunUsageFrom(ctx); u != nil {
		return ctx, u
	}
	u := &RunUsage{}
	return context.WithValue(ctx, runUsageKey{}, u), u
}

// RunUsageFrom returns the run usage accumulator on ctx, or nil.
func RunUsageFrom(ctx context.Context) *RunUsage {
	if u, ok := ctx.Value(runUsageKey{}).(*RunUsage); ok {
		return u
	}
	return nil
}

// Add records one completed call's usage.
func (u *RunUsage) Add(usage Usage) {
	if u == nil {
		return
	}
	u.prompt.Add(int64(max(usage.PromptTokens, 0)))
	u.completion.Add(int64(max(usage.CompletionTokens, 0)))
	u.calls.Add(1)
}

// Total returns cumulative prompt plus completion tokens.
func (u *RunUsage) Total() int64 {
	if u == nil {
		return 0
	}
	return u.prompt.Load() + u.completion.Load()
}

// Calls returns how many completed calls were recorded.
func (u *RunUsage) Calls() int64 {
	if u == nil {
		return 0
	}
	return u.calls.Load()
}

// runUsageClient is the outermost composed layer: it records the usage of
// every response that reaches the caller under a run's context.
type runUsageClient struct{ inner LLMClient }

func (c *runUsageClient) Complete(ctx context.Context, req CompleteRequest) (CompleteResponse, error) {
	resp, err := c.inner.Complete(ctx, req)
	if err == nil {
		RunUsageFrom(ctx).Add(resp.Usage)
	}
	return resp, err
}

func (c *runUsageClient) Close(ctx context.Context) error { return c.inner.Close(ctx) }
