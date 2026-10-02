package llm

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
)

// InferenceAllocation is an immutable, task-scoped cumulative token allowance.
// It does not authorize provider credentials or assert a monetary guarantee.
type InferenceAllocation struct {
	AllocationID    string `json:"allocation_id"`
	Revision        uint64 `json:"revision"`
	MaxTotalTokens  int64  `json:"max_total_tokens"`
	MaxCostMicroUSD *int64 `json:"max_cost_micro_usd,omitempty"`
}

var (
	// ErrAllocationInvalid rejects a malformed or changed allocation.
	ErrAllocationInvalid = errors.New("llm: inference allocation invalid")
	// ErrAllocationExhausted means no conservative provider reservation fits.
	ErrAllocationExhausted = errors.New("llm: inference allocation exhausted")
	// ErrAllocationUnavailable means the durable allocation seam is absent.
	ErrAllocationUnavailable = errors.New("llm: inference allocation unavailable")
	// ErrAllocationPricingUnavailable rejects an unsupported hard monetary cap.
	ErrAllocationPricingUnavailable = errors.New("llm: trusted allocation pricing unavailable")
	// ErrAllocationBoundUnavailable rejects a call without a finite trusted bound.
	ErrAllocationBoundUnavailable = errors.New("llm: inference allocation token bound unavailable")
	// ErrAllocationBoundViolated reports measured usage above the trusted envelope.
	ErrAllocationBoundViolated = errors.New("llm: provider violated inference allocation bound")
)

// ValidateInferenceAllocation validates the immutable funding input.
func ValidateInferenceAllocation(a *InferenceAllocation) error {
	if a == nil {
		return nil
	}
	if strings.TrimSpace(a.AllocationID) != a.AllocationID || a.AllocationID == "" || strings.ContainsAny(a.AllocationID, "\x00\r\n\t") || len(a.AllocationID) > 128 || a.Revision == 0 || a.MaxTotalTokens <= 0 {
		return ErrAllocationInvalid
	}
	if a.MaxCostMicroUSD != nil {
		return ErrAllocationPricingUnavailable
	}
	return nil
}

// CloneInferenceAllocation detaches the accepted funding input.
func CloneInferenceAllocation(a *InferenceAllocation) *InferenceAllocation {
	if a == nil {
		return nil
	}
	b := *a
	if a.MaxCostMicroUSD != nil {
		v := *a.MaxCostMicroUSD
		b.MaxCostMicroUSD = &v
	}
	return &b
}

type allocationContextKey struct{}
type allocationTaskKey struct{}

// WithInferenceAllocationTask binds a child to its server-derived funding root.
func WithInferenceAllocationTask(ctx context.Context, a *InferenceAllocation, taskID string) context.Context {
	ctx = WithInferenceAllocation(ctx, a)
	return context.WithValue(ctx, allocationTaskKey{}, taskID)
}

// WithInferenceAllocation binds accepted task funding to all descendant helpers.
func WithInferenceAllocation(ctx context.Context, a *InferenceAllocation) context.Context {
	return context.WithValue(ctx, allocationContextKey{}, CloneInferenceAllocation(a))
}

// InferenceAllocationFrom returns a detached task allocation.
func InferenceAllocationFrom(ctx context.Context) *InferenceAllocation {
	a, ok := ctx.Value(allocationContextKey{}).(*InferenceAllocation)
	if !ok {
		return nil
	}
	return CloneInferenceAllocation(a)
}

// AllocationSnapshot contains content-free cumulative accounting. Reserved
// includes unresolved provider liability and never expires on a wall clock.
type AllocationSnapshot struct {
	Receipts          []AllocationReceipt `json:"receipts"`
	ReceiptsTruncated bool                `json:"receipts_truncated"`
	BoundBreached     bool                `json:"bound_breached"`
	AllocationID      string              `json:"allocation_id"`
	Revision          uint64              `json:"revision"`
	MaxTotalTokens    int64               `json:"max_total_tokens"`
	SettledTokens     int64               `json:"settled_tokens"`
	ReservedTokens    int64               `json:"reserved_tokens"`
	UnknownTokens     int64               `json:"unknown_tokens"`
	AttemptCount      int64               `json:"attempt_count"`
	Guarantee         string              `json:"guarantee"`
	PricingStatus     string              `json:"pricing_status"`
}

// AllocationStore is the mandatory durable accounting seam for allocated calls.
// Every method is identity+canonical-task scoped and safe across processes.
type AllocationStore interface {
	Reserve(context.Context, identity.Quadruple, InferenceAllocation, string, int64) error
	Settle(context.Context, identity.Quadruple, InferenceAllocation, string, *int64, bool) error
	// ReportBoundViolation retains the attempt envelope and permanently closes the allocation.
	ReportBoundViolation(context.Context, identity.Quadruple, InferenceAllocation, string) error
	Snapshot(context.Context, identity.Quadruple, InferenceAllocation) (AllocationSnapshot, error)
}

func (c *safetyClient) completeAllocated(ctx context.Context, req CompleteRequest, profile ModelProfile) (CompleteResponse, error) {
	a := InferenceAllocationFrom(ctx)
	if a == nil {
		return c.driver.Complete(ctx, req)
	}
	if err := ValidateInferenceAllocation(a); err != nil {
		return CompleteResponse{}, err
	}
	if c.deps.Allocations == nil {
		return CompleteResponse{}, ErrAllocationUnavailable
	}
	q := identityQuad(ctx)
	if root, ok := ctx.Value(allocationTaskKey{}).(string); ok && root != "" {
		q.RunID = root
	}
	if q.RunID == "" {
		return CompleteResponse{}, ErrAllocationInvalid
	}
	if profile.ContextWindowTokens <= 0 || req.MaxTokens == nil || *req.MaxTokens <= 0 || profile.ContextWindowTokens > 1<<30 || *req.MaxTokens > 1<<30 {
		return CompleteResponse{}, ErrAllocationBoundUnavailable
	}
	// ContextWindowTokens bounds input; MaxTokens bounds output. The prompt
	// estimator is deliberately not used for hard admission.
	bound, ok := c.driver.(AllocationBoundedDriver)
	if !ok {
		return CompleteResponse{}, ErrAllocationBoundUnavailable
	}
	attempts, err := bound.ProviderAttemptBound(ctx, req)
	if err != nil {
		return CompleteResponse{}, err
	}
	if attempts <= 0 || attempts > 1000 {
		return CompleteResponse{}, ErrAllocationBoundUnavailable
	}
	units := (int64(profile.ContextWindowTokens) + int64(*req.MaxTokens)) * int64(attempts)
	_, scope, err := EnsureAttemptScope(WithAttemptScope(ctx, nil))
	if err != nil {
		return CompleteResponse{}, fmt.Errorf("allocation attempt identity: %w", err)
	}
	attempt := scope.CallID
	if err = c.deps.Allocations.Reserve(ctx, q, *a, attempt, units); err != nil {
		return CompleteResponse{}, err
	}
	if err = ctx.Err(); err != nil {
		zero := int64(0)
		terminal, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		settleErr := c.deps.Allocations.Settle(terminal, q, *a, attempt, &zero, false)
		return CompleteResponse{}, errors.Join(err, settleErr)
	}
	resp, callErr := c.driver.Complete(ctx, req)
	var used *int64
	u := resp.Usage
	invalidUsage := false
	// An errored stream may carry only partial cumulative usage. Without a
	// terminal-completeness witness it cannot release the unknown remainder.
	if callErr == nil && u.ReportPresent && !u.Estimated && u.PromptTokens >= 0 && u.CompletionTokens >= 0 && u.TotalTokens > 0 {
		// Accounting rejects an unrepresentable usage report without wrapping
		// the sum into a small number and refunding real provider liability.
		const maxAccounted = int64(1 << 62)
		prompt, completion, reported := int64(u.PromptTokens), int64(u.CompletionTokens), int64(u.TotalTokens)
		if completion > maxAccounted || prompt > maxAccounted-completion || reported > maxAccounted {
			invalidUsage = true
		} else {
			total := max(prompt+completion, reported)
			used = &total
		}
	}
	// Caller cancellation cannot erase accounting after a provider attempt.
	terminal, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if invalidUsage {
		breachErr := c.deps.Allocations.ReportBoundViolation(terminal, q, *a, attempt)
		return resp, errors.Join(callErr, ErrAllocationBoundViolated, breachErr)
	}
	if err = c.deps.Allocations.Settle(terminal, q, *a, attempt, used, attempts > 1); err != nil {
		return resp, errors.Join(callErr, fmt.Errorf("allocation settlement: %w", err))
	}
	if used != nil && *used > units {
		return resp, errors.Join(callErr, ErrAllocationBoundViolated)
	}
	return resp, callErr
}

// AllocationBoundedDriver is an additive contract for providers that can bound
// all hidden physical attempts. Allocated calls refuse legacy drivers without
// this guarantee; ordinary calls retain their existing Driver compatibility.
type AllocationBoundedDriver interface {
	ProviderAttemptBound(context.Context, CompleteRequest) (int, error)
}

// AllocationReceipt is a content-free terminal provider-envelope accounting receipt.
// Unknown tokens remain held; settled usage never implies unknown work is free.
type AllocationReceipt struct {
	AttemptID      string `json:"attempt_id"`
	ReservedTokens int64  `json:"reserved_tokens"`
	SettledTokens  int64  `json:"settled_tokens"`
	UnknownTokens  int64  `json:"unknown_tokens"`
	Status         string `json:"status"`
}
