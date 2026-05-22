package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"strings"
	"time"

	"github.com/hurtener/Harbor/internal/events"
)

// ValidateMode selects which side of an invocation the policy
// validates: input, output, both, or none. Mirrors engine.NodePolicy
// (RFC §6.1) so a developer who learned NodePolicy already knows
// ToolPolicy (D-024).
type ValidateMode string

const (
	// ValidateNone disables validation. Escape hatch for hot paths.
	ValidateNone ValidateMode = ""
	// ValidateBoth runs validation on input AND output. Default for
	// production tools.
	ValidateBoth ValidateMode = "both"
	// ValidateIn runs validation on the input only.
	ValidateIn ValidateMode = "in"
	// ValidateOut runs validation on the output only.
	ValidateOut ValidateMode = "out"
)

// ErrorClass categorises a tool invocation failure. The policy's
// RetryOn allowlist references these classes; the shell's
// classifier maps Go errors to a class on each failed attempt.
type ErrorClass string

const (
	// ErrClassTransient covers retryable infrastructure issues —
	// network blips, "connection reset", "EOF", etc. The shell's
	// classifier maps common error strings here.
	ErrClassTransient ErrorClass = "transient"
	// ErrClassTimeout — the per-attempt context.DeadlineExceeded
	// or the tool's own timeout signal.
	ErrClassTimeout ErrorClass = "timeout"
	// ErrClass5xx — HTTP / RPC server-side 5xx (transient-side
	// failures distinct from network errors).
	ErrClass5xx ErrorClass = "5xx"
	// ErrClassPermanent — non-retryable. A tool that returns a
	// 4xx, ErrToolInvalidArgs, or a context.Canceled gets this
	// class; the shell stops retrying immediately.
	ErrClassPermanent ErrorClass = "permanent"
)

// ToolPolicy is the per-tool reliability shell. Every invocation
// regardless of Transport wraps in ToolPolicy (RFC §6.4, D-024).
// Zero value is "use defaults": DefaultPolicy fires on a fresh
// Tool.Policy at dispatch time.
//
// Mirrors engine.NodePolicy (RFC §6.1): same backoff math, same
// validation modes. A flow registered as a tool gets BOTH layers —
// the outer ToolPolicy wraps the whole flow invocation; the
// per-node NodePolicy wraps each step inside the flow's engine.
// No double-wrapping at any single layer.
type ToolPolicy struct {
	Validate    ValidateMode
	RetryOn     []ErrorClass
	TimeoutMS   int
	MaxRetries  int
	BackoffBase time.Duration
	BackoffMult float64
	BackoffMax  time.Duration
}

// DefaultPolicy returns the policy applied when ToolPolicy is
// zero-valued. 3 retries / 100ms→30s exponential backoff (mult=2)
// / 30s timeout / Validate=ValidateBoth / RetryOn=[transient,
// timeout, 5xx]. Per D-024 acceptance criteria.
func DefaultPolicy() ToolPolicy {
	return ToolPolicy{
		TimeoutMS:   30000,
		MaxRetries:  3,
		BackoffBase: 100 * time.Millisecond,
		BackoffMult: 2,
		BackoffMax:  30 * time.Second,
		RetryOn:     []ErrorClass{ErrClassTransient, ErrClassTimeout, ErrClass5xx},
		Validate:    ValidateBoth,
	}
}

// resolved returns p with zero-valued fields replaced by
// DefaultPolicy values. Only zero fields are filled; an
// explicitly-zero TimeoutMS on a non-zero policy stays zero
// (operators who want "no timeout, no retry" set ALL fields).
//
// Per-field rule: a field is "zero-valued" if it equals Go's zero
// value (int=0, time.Duration=0, slice=nil, string=""). A `Validate:
// ValidateNone` on an otherwise default-shaped policy stays
// ValidateNone (ValidateNone is the empty string).
func (p ToolPolicy) resolved() ToolPolicy {
	def := DefaultPolicy()
	out := p
	// If EVERY field is zero, the operator did not set the policy
	// → apply defaults wholesale.
	if p.isZero() {
		return def
	}
	// Otherwise apply per-field defaults for unset fields.
	if out.TimeoutMS == 0 {
		out.TimeoutMS = def.TimeoutMS
	}
	if out.MaxRetries == 0 {
		out.MaxRetries = def.MaxRetries
	}
	if out.BackoffBase == 0 {
		out.BackoffBase = def.BackoffBase
	}
	if out.BackoffMult == 0 {
		out.BackoffMult = def.BackoffMult
	}
	if out.BackoffMax == 0 {
		out.BackoffMax = def.BackoffMax
	}
	if out.RetryOn == nil {
		out.RetryOn = def.RetryOn
	}
	if out.Validate == "" {
		out.Validate = def.Validate
	}
	return out
}

// isZero reports whether p is the zero value across every field.
func (p ToolPolicy) isZero() bool {
	return p.TimeoutMS == 0 &&
		p.MaxRetries == 0 &&
		p.BackoffBase == 0 &&
		p.BackoffMult == 0 &&
		p.BackoffMax == 0 &&
		len(p.RetryOn) == 0 &&
		p.Validate == ""
}

// retryAllowed reports whether the policy retries on class c.
func (p ToolPolicy) retryAllowed(c ErrorClass) bool {
	for _, cls := range p.RetryOn {
		if cls == c {
			return true
		}
	}
	return false
}

// shouldValidateIn reports whether the resolved policy validates
// input.
func (p ToolPolicy) shouldValidateIn() bool {
	return p.Validate == ValidateBoth || p.Validate == ValidateIn
}

// shouldValidateOut reports whether the resolved policy validates
// output.
func (p ToolPolicy) shouldValidateOut() bool {
	return p.Validate == ValidateBoth || p.Validate == ValidateOut
}

// DescriptorOption configures a ToolDescriptor at registration.
// Drivers (Phase 26 in-process, Phase 27+ HTTP / MCP / A2A) accept
// `opts ...DescriptorOption` so the same option surface is reused
// across transports.
type DescriptorOption func(*descriptorConfig)

// descriptorConfig accumulates option settings; drivers consume the
// resolved config when building their ToolDescriptor.
type descriptorConfig struct {
	bus         events.EventBus
	description string
	sideEffect  SideEffect
	loading     LoadingMode
	costHint    string
	safetyNotes string
	source      ToolSourceID
	tags        []string
	authScopes  []string
	examples    []ToolExample
	policy      ToolPolicy
	latencyHint time.Duration
}

// WithPolicy overrides the ToolPolicy applied to the registered
// Tool. Pass tools.ToolPolicy{} (the zero value) to opt back into
// DefaultPolicy().
func WithPolicy(p ToolPolicy) DescriptorOption {
	return func(c *descriptorConfig) { c.policy = p }
}

// WithDescription overrides the Tool's planner-facing description.
// Default is the function name when registering via RegisterFunc.
func WithDescription(s string) DescriptorOption {
	return func(c *descriptorConfig) { c.description = s }
}

// WithTags adds operator-facing tags.
func WithTags(tags ...string) DescriptorOption {
	return func(c *descriptorConfig) { c.tags = append(c.tags, tags...) }
}

// WithAuthScopes adds required auth scopes — the CatalogFilter
// MUST grant every scope listed here for the Tool to be visible.
func WithAuthScopes(scopes ...string) DescriptorOption {
	return func(c *descriptorConfig) { c.authScopes = append(c.authScopes, scopes...) }
}

// WithExamples attaches canonical argument-shape examples surfaced
// to the planner.
func WithExamples(examples ...ToolExample) DescriptorOption {
	return func(c *descriptorConfig) { c.examples = append(c.examples, examples...) }
}

// WithSideEffect declares the tool's side-effect class.
func WithSideEffect(s SideEffect) DescriptorOption {
	return func(c *descriptorConfig) { c.sideEffect = s }
}

// WithLoading overrides LoadingMode (default: LoadingAlways).
func WithLoading(m LoadingMode) DescriptorOption {
	return func(c *descriptorConfig) { c.loading = m }
}

// WithCostHint sets a free-form cost annotation.
func WithCostHint(s string) DescriptorOption {
	return func(c *descriptorConfig) { c.costHint = s }
}

// WithLatencyHint sets a non-authoritative latency annotation.
func WithLatencyHint(d time.Duration) DescriptorOption {
	return func(c *descriptorConfig) { c.latencyHint = d }
}

// WithSafetyNotes attaches operator-supplied safety text surfaced
// to the planner.
func WithSafetyNotes(s string) DescriptorOption {
	return func(c *descriptorConfig) { c.safetyNotes = s }
}

// WithSource overrides the descriptor's ToolSourceID (for
// provider-driven registrations).
func WithSource(id ToolSourceID) DescriptorOption {
	return func(c *descriptorConfig) { c.source = id }
}

// WithBus wires a canonical event bus into a tool registration so
// the driver's Invoke wrapper publishes tool.invoked / tool.completed
// / tool.failed around each invocation. Identity in the published
// payload is read from the invocation ctx via identity.From; if no
// identity is present the publish is skipped (a tool boundary that
// rejects missing identity already errored before this point).
//
// Drivers (inproc, http, mcp, a2a) honour this option in their
// Invoke closures so the Phase 26 event surface fires regardless of
// transport. Zero value (nil bus) keeps the previous no-op
// behaviour, so legacy registrations continue to work.
func WithBus(bus events.EventBus) DescriptorOption {
	return func(c *descriptorConfig) { c.bus = bus }
}

// ResolvedDescriptorConfig is the resolved option set after
// applying DescriptorOptions. Drivers consume this when building
// their ToolDescriptor — they read what the operator declared at
// registration time + the package-set defaults.
//
// Exported so transports (Phase 27 HTTP, Phase 28 MCP, Phase 29
// A2A) can reuse the option-resolution pipeline without
// re-implementing it. The fields mirror the unexported
// descriptorConfig in this file.
type ResolvedDescriptorConfig struct {
	Bus         events.EventBus
	Description string
	SideEffect  SideEffect
	Loading     LoadingMode
	CostHint    string
	SafetyNotes string
	Source      ToolSourceID
	Tags        []string
	AuthScopes  []string
	Examples    []ToolExample
	Policy      ToolPolicy
	LatencyHint time.Duration
}

// ResolveOptions applies opts to a fresh ResolvedDescriptorConfig
// + sets per-driver defaults the inproc registrar (and future
// drivers) consume.
func ResolveOptions(opts ...DescriptorOption) ResolvedDescriptorConfig {
	cfg := descriptorConfig{
		loading:    LoadingAlways,
		sideEffect: SideEffectStateful,
	}
	for _, opt := range opts {
		opt(&cfg)
	}
	return ResolvedDescriptorConfig{
		Policy:      cfg.policy,
		Description: cfg.description,
		Tags:        cfg.tags,
		AuthScopes:  cfg.authScopes,
		Examples:    cfg.examples,
		SideEffect:  cfg.sideEffect,
		Loading:     cfg.loading,
		CostHint:    cfg.costHint,
		LatencyHint: cfg.latencyHint,
		SafetyNotes: cfg.safetyNotes,
		Source:      cfg.source,
		Bus:         cfg.bus,
	}
}

// invokeHooks is the optional callback surface the dispatcher
// exposes to drivers for observability — emits per-attempt events
// without coupling the policy executor to the events package
// directly. Drivers wire these in their Invoke closures.
type invokeHooks struct {
	OnAttempt func(attempt int, err error)
}

// runWithPolicy executes invoke under the policy's reliability
// shell. Mirrors engine.runWithReliability (RFC §6.1) — same
// backoff math, same retry-on-cancel semantics, same per-attempt
// timeout. The only delta is the input is `json.RawMessage` (not a
// runtime envelope) and the output is `ToolResult`.
//
// Validation contract (D-033 candidate): the input-validation pass
// runs ONCE BEFORE the retry loop. Retrying on invalid args would
// waste budget and never converge. Output-validation runs on each
// successful attempt's result (a retry that fixes a transient
// error still gets its output checked).
//
// Concurrent reuse (D-025): all per-invocation state lives on the
// goroutine stack (attempt counter, last err, deadline timer).
// `policy` is a value-typed parameter; no shared mutable state.
func runWithPolicy(
	ctx context.Context,
	args json.RawMessage,
	invoke func(ctx context.Context, args json.RawMessage) (ToolResult, error),
	validateIn func(args json.RawMessage) error,
	validateOut func(result ToolResult) error,
	policy ToolPolicy,
	jitter func() float64,
	hooks *invokeHooks,
) (ToolResult, error) {
	resolved := policy.resolved()

	// 1. Input validation ONCE, BEFORE the retry loop. Invalid args
	// are a typed failure the planner is expected to fix via
	// reformulation — retrying with the same args never converges.
	if resolved.shouldValidateIn() && validateIn != nil {
		if err := validateIn(args); err != nil {
			return ToolResult{}, wrap(ErrToolInvalidArgs, "%v", err)
		}
	}

	if jitter == nil {
		jitter = rand.Float64
	}

	totalAttempts := resolved.MaxRetries + 1
	if totalAttempts <= 0 {
		totalAttempts = 1
	}

	var lastErr error
	var lastClass ErrorClass

	for attempt := range totalAttempts {
		// Honor ctx cancellation between attempts.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ToolResult{}, ctxErr
		}

		// Sleep before retries (attempt > 0).
		if attempt > 0 {
			delay := nextBackoff(attempt, resolved.BackoffBase, resolved.BackoffMax, resolved.BackoffMult, jitter)
			if delay > 0 {
				timer := time.NewTimer(delay)
				select {
				case <-ctx.Done():
					timer.Stop()
					return ToolResult{}, ctx.Err()
				case <-timer.C:
				}
			}
		}

		// Per-attempt invocation context with timeout.
		attemptCtx := ctx
		var cancel context.CancelFunc
		if resolved.TimeoutMS > 0 {
			attemptCtx, cancel = context.WithTimeout(ctx, time.Duration(resolved.TimeoutMS)*time.Millisecond)
		}

		result, invokeErr := safeInvoke(attemptCtx, invoke, args)
		if cancel != nil {
			cancel()
		}

		if hooks != nil && hooks.OnAttempt != nil {
			hooks.OnAttempt(attempt, invokeErr)
		}

		if invokeErr == nil {
			// 5. Output validation on success.
			if resolved.shouldValidateOut() && validateOut != nil {
				if vErr := validateOut(result); vErr != nil {
					// Output validation failure is NOT retryable
					// (a permanent class). Return the result-shape
					// error wrapped against the original sentinel
					// so callers can compare with errors.Is.
					return ToolResult{}, wrap(ErrToolInvalidArgs, "output: %v", vErr)
				}
			}
			return result, nil
		}

		lastErr = invokeErr
		lastClass = ClassifyError(invokeErr, resolved.TimeoutMS > 0)

		// Permanent + ctx-cancellation classes terminate the loop.
		if lastClass == ErrClassPermanent {
			return ToolResult{}, invokeErr
		}
		// If the parent ctx died (not just the per-attempt one),
		// terminate.
		if parentErr := ctx.Err(); parentErr != nil {
			return ToolResult{}, parentErr
		}
		// If the class isn't in RetryOn, terminate.
		if !resolved.retryAllowed(lastClass) {
			return ToolResult{}, invokeErr
		}
	}

	// Terminal failure: wrap with ErrToolPolicyExhausted.
	return ToolResult{}, fmt.Errorf("%w: %d attempts, last class=%s: %w",
		ErrToolPolicyExhausted, totalAttempts, lastClass, lastErr)
}

// safeInvoke calls invoke under a panic recovery so a misbehaving
// tool function doesn't bring down the worker. Mirrors the engine
// shell's safeInvoke.
func safeInvoke(
	ctx context.Context,
	invoke func(ctx context.Context, args json.RawMessage) (ToolResult, error),
	args json.RawMessage,
) (result ToolResult, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("tools: panic during invoke: %v", r)
		}
	}()
	return invoke(ctx, args)
}

// nextBackoff computes the sleep duration before retry attempt
// `attempt` (1-indexed). Formula: min(base * mult^attempt +
// jitter, max). Jitter is uniform on [0, base*0.1). Mirrors
// engine.nextBackoff (RFC §6.1).
func nextBackoff(attempt int, base, max time.Duration, mult float64, rand func() float64) time.Duration {
	if attempt <= 0 || base <= 0 {
		return 0
	}
	if mult <= 0 {
		mult = 1
	}

	growth := float64(base)
	for range attempt {
		growth *= mult
	}
	if growth > float64(time.Duration(1<<62)) {
		growth = float64(time.Duration(1 << 62))
	}
	d := time.Duration(growth)

	if rand != nil {
		jitter := time.Duration(float64(base) * 0.1 * rand())
		d += jitter
	}

	if max > 0 && d > max {
		return max
	}
	return d
}

// ClassifyError maps a Go error to an ErrorClass. Used by the
// shell to decide retryability. Public so drivers can return a
// pre-classified error if they want to bypass the default
// heuristics.
//
// Heuristics (V1):
//   - context.DeadlineExceeded + perAttemptTimeout: ErrClassTimeout
//     (per-attempt timeout fired).
//   - context.DeadlineExceeded without perAttemptTimeout: parent
//     ctx died → ErrClassPermanent (caller-driven).
//   - context.Canceled: ErrClassPermanent.
//   - ErrToolInvalidArgs / ErrToolNotFound wrapped: ErrClassPermanent.
//   - HTTP-status-shaped error strings ("status 5xx", "500", "503"):
//     ErrClass5xx.
//   - "timeout" / "deadline exceeded" / "context canceled" string
//     match in error: ErrClassTimeout.
//   - everything else: ErrClassTransient (the conservative default
//     so tools opt OUT of retry rather than opting in).
func ClassifyError(err error, perAttemptTimeout bool) ErrorClass {
	if err == nil {
		return ErrClassPermanent // not used; defensive.
	}
	if errors.Is(err, context.DeadlineExceeded) {
		if perAttemptTimeout {
			return ErrClassTimeout
		}
		return ErrClassPermanent
	}
	if errors.Is(err, context.Canceled) {
		return ErrClassPermanent
	}
	if errors.Is(err, ErrToolInvalidArgs) || errors.Is(err, ErrToolNotFound) {
		return ErrClassPermanent
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "status 5") ||
		strings.Contains(msg, " 500 ") ||
		strings.Contains(msg, " 502 ") ||
		strings.Contains(msg, " 503 ") ||
		strings.Contains(msg, " 504 ") {
		return ErrClass5xx
	}
	if strings.Contains(msg, "timeout") ||
		strings.Contains(msg, "deadline exceeded") {
		return ErrClassTimeout
	}
	return ErrClassTransient
}

// RunWithPolicy is the externally-visible executor — drivers
// (in-process, HTTP, MCP, A2A, Flow) call this from their Invoke
// closures so every tool invocation regardless of Transport wraps
// in the same reliability shell.
//
// `validateIn` / `validateOut` may be nil; the shell uses the
// resolved policy's Validate mode to decide whether to call them.
// `policy` is the Tool's policy (per-Tool); zero-valued → defaults.
//
// Concurrent reuse (D-025): RunWithPolicy is stateless — no shared
// mutable state across calls. Safe for N concurrent invocations.
func RunWithPolicy(
	ctx context.Context,
	args json.RawMessage,
	invoke func(ctx context.Context, args json.RawMessage) (ToolResult, error),
	validateIn func(args json.RawMessage) error,
	validateOut func(result ToolResult) error,
	policy ToolPolicy,
) (ToolResult, error) {
	return runWithPolicy(ctx, args, invoke, validateIn, validateOut, policy, nil, nil)
}

// InvokeHooks lets drivers observe per-attempt outcomes (e.g.
// to emit per-attempt audit events) without coupling the policy
// shell to the events package.
type InvokeHooks struct {
	// OnAttempt fires after each invoke attempt with the attempt
	// index (0-based: 0 is the initial call, 1 is the first
	// retry) and the attempt's error (nil on success).
	OnAttempt func(attempt int, err error)
}

// RunWithPolicyHooked is the externally-visible executor with
// observability hooks. Equivalent to RunWithPolicy with an
// InvokeHooks pointer. Drivers (esp. those that publish
// per-attempt events) consume this surface.
func RunWithPolicyHooked(
	ctx context.Context,
	args json.RawMessage,
	invoke func(ctx context.Context, args json.RawMessage) (ToolResult, error),
	validateIn func(args json.RawMessage) error,
	validateOut func(result ToolResult) error,
	policy ToolPolicy,
	hooks ...InvokeHooks,
) (ToolResult, error) {
	var h *invokeHooks
	if len(hooks) > 0 {
		hh := hooks[0]
		h = &invokeHooks{OnAttempt: hh.OnAttempt}
	}
	return runWithPolicy(ctx, args, invoke, validateIn, validateOut, policy, nil, h)
}
