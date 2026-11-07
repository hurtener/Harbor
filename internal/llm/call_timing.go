package llm

import (
	"context"
	"errors"
	"sync/atomic"
	"time"

	"github.com/hurtener/Harbor/internal/telemetry"
)

// CallRole identifies the bounded purpose of an LLM invocation in timing logs.
// It never carries operator text, tool names, or provider output.
type CallRole string

// Call roles distinguish planner reasoning from auxiliary model work.
const (
	CallRoleCompletion    CallRole = "completion"
	CallRolePlanner       CallRole = "planner"
	CallRoleRepair        CallRole = "repair"
	CallRoleCompaction    CallRole = "compaction"
	CallRoleTitle         CallRole = "title"
	CallRoleSkillDraft    CallRole = "skill_draft"
	CallRoleAgentProposal CallRole = "agent_proposal"
)

type callRoleKey struct{}
type streamTimingKey struct{}

// WithCallRole labels this invocation and its retry/downgrade attempts.
// Unknown values normalize to completion so the log vocabulary stays bounded.
func WithCallRole(ctx context.Context, role CallRole) context.Context {
	return context.WithValue(ctx, callRoleKey{}, boundedCallRole(role))
}

func boundedCallRole(role CallRole) CallRole {
	switch role {
	case CallRolePlanner, CallRoleRepair, CallRoleCompaction, CallRoleTitle, CallRoleSkillDraft, CallRoleAgentProposal:
		return role
	default:
		return CallRoleCompletion
	}
}

// callTiming belongs to exactly one driver invocation. Its first-delta slot is
// internally synchronized because providers may invoke callbacks concurrently.
type callTiming struct {
	started    time.Time
	dispatched bool
	firstDelta atomic.Int64          // elapsed nanoseconds + 1; zero means unobserved
	usage      atomic.Pointer[Usage] // reported scalar counts only; never provider extras
}

// ObserveProviderRequest starts timing at a driver's actual model dispatch.
// A ProviderRequestTiming driver calls it once on the dispatch goroutine,
// before any stream callback can run. Credential lookup and local validation
// therefore remain pre-model work, and rejected requests emit no model call.
func ObserveProviderRequest(ctx context.Context) {
	if timing, ok := ctx.Value(streamTimingKey{}).(*callTiming); ok {
		timing.started = time.Now()
		timing.dispatched = true
	}
	telemetry.ObserveFirstModelRequest(ctx)
}

// ObserveStreamDelta records an actual streamed content, reasoning, or tool
// delta. Drivers use this for channels without an OnContent/OnReasoning callback
// (for example tool-call arguments). Role-only, usage, and terminal chunks must
// not call it. It is a no-op outside a timed streaming invocation.
func ObserveStreamDelta(ctx context.Context) {
	if timing, ok := ctx.Value(streamTimingKey{}).(*callTiming); ok {
		timing.firstDelta.CompareAndSwap(0, time.Since(timing.started).Nanoseconds()+1)
	}
}

// ObserveProviderUsage preserves reported counts for timing when an interrupted
// stream returns no response. This is observability-only: it does not change
// the response, cost accounting, or governance, and retains no provider extras.
func ObserveProviderUsage(ctx context.Context, usage Usage) {
	if !usage.ReportPresent || usage.Estimated {
		return
	}
	if timing, ok := ctx.Value(streamTimingKey{}).(*callTiming); ok {
		timing.usage.Store(&Usage{ReportPresent: true, PromptTokens: usage.PromptTokens, CompletionTokens: usage.CompletionTokens})
	}
}

func beginCallTiming(ctx context.Context, req CompleteRequest) (context.Context, CompleteRequest, *callTiming) {
	timing := &callTiming{started: time.Now()}
	ctx = context.WithValue(ctx, streamTimingKey{}, timing)
	if !req.Stream {
		return ctx, req, timing
	}
	wrap := func(next func(string, bool)) func(string, bool) {
		if next == nil {
			return nil
		}
		return func(delta string, done bool) {
			if delta != "" {
				ObserveStreamDelta(ctx)
			}
			if next != nil {
				next(delta, done)
			}
		}
	}
	req.OnContent = wrap(req.OnContent)
	req.OnReasoning = wrap(req.OnReasoning)
	return ctx, req, timing
}

func (c *safetyClient) logCallTiming(ctx context.Context, req CompleteRequest, resp CompleteResponse, callErr error, timing *callTiming) {
	if _, ownsBoundary := c.driver.(ProviderRequestTiming); ownsBoundary && !timing.dispatched {
		return
	}
	provider := c.cfg.Provider
	if selected, ok := SelectedProviderRouteFrom(ctx); ok {
		provider = selected.Provider
	}
	effort, _ := EffectiveReasoningEffort(req) // Invalid values are never logged.
	role, _ := ctx.Value(callRoleKey{}).(CallRole)
	call := telemetry.ModelCallTiming{
		Provider: provider, Model: req.Model, ReasoningEffort: string(effort), Role: string(boundedCallRole(role)),
		Total: time.Since(timing.started), Failed: callErr != nil,
		Canceled: callErr != nil && (ctx.Err() != nil || errors.Is(callErr, context.Canceled) || errors.Is(callErr, context.DeadlineExceeded)),
	}
	if scope, ok := AttemptScopeFrom(ctx); ok && scope != nil {
		call.Attempt = &telemetry.ModelAttemptCoordinates{Attempt: scope.Attempt, Retry: scope.Retry, Downgrade: scope.Downgrade, FallbackHop: scope.FallbackHop}
	}
	if elapsed := timing.firstDelta.Load(); req.Stream && elapsed > 0 {
		firstDelta := time.Duration(elapsed - 1)
		call.FirstDelta = &firstDelta
	}
	usage := resp.Usage
	if !usage.ReportPresent {
		if observed := timing.usage.Load(); observed != nil {
			usage = *observed
		}
	}
	if usage.ReportPresent && !usage.Estimated {
		call.InputTokens = &usage.PromptTokens
		call.OutputTokens = &usage.CompletionTokens
	}
	telemetry.RecordModelCall(ctx, c.deps.Logger, call)
}
