package llm

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm/pricing"
)

// InferenceAllocation is immutable cumulative task token and optional monetary funding.
// It does not authorize provider credentials. Monetary mode requires an exact
// operator-installed all-charges pricing manifest, never caller-supplied tariffs.
type InferenceAllocation struct {
	AllocationID            string `json:"allocation_id"`
	Revision                uint64 `json:"revision"`
	MaxTotalTokens          int64  `json:"max_total_tokens"`
	MaxCostMicroUSD         *int64 `json:"max_cost_micro_usd,omitempty"`
	PricingManifestID       string `json:"pricing_manifest_id,omitempty"`
	PricingManifestRevision uint64 `json:"pricing_manifest_revision,omitempty"`
	PricingManifestSHA256   string `json:"pricing_manifest_sha256,omitempty"`
}

var (
	// ErrAllocationInvalid rejects a malformed or changed allocation.
	ErrAllocationInvalid = errors.New("llm: inference allocation invalid")
	// ErrAllocationExhausted means no conservative provider reservation fits.
	ErrAllocationExhausted = errors.New("llm: inference allocation exhausted")
	// ErrAllocationUnavailable means the durable allocation seam is absent.
	ErrAllocationUnavailable = errors.New("llm: inference allocation unavailable")
	// ErrAllocationPricingUnavailable rejects an unsupported hard monetary cap.
	ErrAllocationPricingUnavailable = pricing.ErrUnavailable
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
		if *a.MaxCostMicroUSD < 0 {
			return ErrAllocationInvalid
		}
		if !pricing.ValidReference(a.PricingReference()) {
			return ErrAllocationPricingUnavailable
		}
	} else if a.PricingManifestID != "" || a.PricingManifestRevision != 0 || a.PricingManifestSHA256 != "" {
		return ErrAllocationInvalid
	}
	return nil
}

// PricingReference returns the exact operator manifest identity pinned in the task.
func (a InferenceAllocation) PricingReference() pricing.Reference {
	return pricing.Reference{ID: a.PricingManifestID, Revision: a.PricingManifestRevision, SHA256: a.PricingManifestSHA256}
}

// EqualInferenceAllocation compares immutable values, not pointer addresses.
func EqualInferenceAllocation(a, b *InferenceAllocation) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	aa, bb := *a, *b
	aa.MaxCostMicroUSD, bb.MaxCostMicroUSD = nil, nil
	return aa == bb && (a.MaxCostMicroUSD == nil && b.MaxCostMicroUSD == nil || a.MaxCostMicroUSD != nil && b.MaxCostMicroUSD != nil && *a.MaxCostMicroUSD == *b.MaxCostMicroUSD)
}

// ValidateInferenceAllocationPricing establishes operator authority at acceptance.
func ValidateInferenceAllocationPricing(a *InferenceAllocation, catalog *pricing.Catalog) error {
	if err := ValidateInferenceAllocation(a); err != nil {
		return err
	}
	if a != nil && a.MaxCostMicroUSD != nil {
		return catalog.ValidateReference(a.PricingReference())
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
	MaxCostMicroUSD *int64 `json:"max_cost_micro_usd,omitempty"`
	// ChargedCostMicroUSD is consumed conservative ceiling capacity, not actual spend.
	ChargedCostMicroUSD int64 `json:"charged_cost_micro_usd"`
	// ReservedCostMicroUSD includes unresolved provider liability and never expires.
	ReservedCostMicroUSD int64 `json:"reserved_cost_micro_usd"`
	// UnknownCostMicroUSD is the unproven portion of reserved monetary capacity.
	UnknownCostMicroUSD     int64               `json:"unknown_cost_micro_usd"`
	PricingManifestID       string              `json:"pricing_manifest_id,omitempty"`
	PricingManifestRevision uint64              `json:"pricing_manifest_revision,omitempty"`
	PricingManifestSHA256   string              `json:"pricing_manifest_sha256,omitempty"`
	Receipts                []AllocationReceipt `json:"receipts"`
	ReceiptsTruncated       bool                `json:"receipts_truncated"`
	BoundBreached           bool                `json:"bound_breached"`
	AllocationID            string              `json:"allocation_id"`
	Revision                uint64              `json:"revision"`
	MaxTotalTokens          int64               `json:"max_total_tokens"`
	SettledTokens           int64               `json:"settled_tokens"`
	ReservedTokens          int64               `json:"reserved_tokens"`
	UnknownTokens           int64               `json:"unknown_tokens"`
	AttemptCount            int64               `json:"attempt_count"`
	Guarantee               string              `json:"guarantee"`
	PricingStatus           string              `json:"pricing_status"`
}

// AllocationStore is the mandatory durable accounting seam for allocated calls.
// Every method is identity+canonical-task scoped and safe across processes.
type AllocationStore interface {
	Reserve(context.Context, identity.Quadruple, InferenceAllocation, string, int64) error
	// ReserveMonetary atomically reserves token and inclusive monetary envelopes.
	ReserveMonetary(context.Context, identity.Quadruple, InferenceAllocation, string, int64, int64) error
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
	if a.MaxCostMicroUSD != nil {
		priced, ok := c.driver.(MonetaryBoundedDriver)
		if !ok {
			return CompleteResponse{}, ErrAllocationPricingUnavailable
		}
		target, targetErr := priced.MonetaryTarget(ctx, req, profile)
		if targetErr != nil {
			return CompleteResponse{}, targetErr
		}
		if target.InputTokens < int64(profile.ContextWindowTokens) || target.OutputTokens < int64(*req.MaxTokens) || target.InputTokens > 1<<31 || target.OutputTokens > 1<<31 {
			return CompleteResponse{}, ErrAllocationBoundUnavailable
		}
		units = (target.InputTokens + target.OutputTokens) * int64(attempts)
		cost, quoteErr := c.deps.PricingCatalog.Quote(a.PricingReference(), target.Provider, target.Model, target.EndpointBinding, target.InputTokens, target.OutputTokens, int64(attempts))
		if quoteErr != nil {
			return CompleteResponse{}, quoteErr
		}
		err = c.deps.Allocations.ReserveMonetary(ctx, q, *a, attempt, units, cost)
	} else {
		err = c.deps.Allocations.Reserve(ctx, q, *a, attempt, units)
	}
	if err != nil {
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
	// ReservedCostMicroUSD includes unresolved provider liability and never expires.
	ReservedCostMicroUSD int64 `json:"reserved_cost_micro_usd"`
	// ChargedCostMicroUSD is consumed conservative ceiling capacity, not actual spend.
	ChargedCostMicroUSD int64 `json:"charged_cost_micro_usd"`
	// UnknownCostMicroUSD is the unproven portion of reserved monetary capacity.
	UnknownCostMicroUSD int64 `json:"unknown_cost_micro_usd"`
	// MonetaryStatus is charged_ceiling, unknown, or released_before_dispatch.
	MonetaryStatus string `json:"monetary_status,omitempty"`
	AttemptID      string `json:"attempt_id"`
	ReservedTokens int64  `json:"reserved_tokens"`
	SettledTokens  int64  `json:"settled_tokens"`
	UnknownTokens  int64  `json:"unknown_tokens"`
	Status         string `json:"status"`
}

// MonetaryTarget is the trusted driver's actual immutable request selector and
// full physical input/output bounds, including provider-specific reasoning.
type MonetaryTarget struct {
	EndpointBinding string
	Provider        string
	Model           string
	InputTokens     int64
	OutputTokens    int64
}

// MonetaryBoundedDriver attests that this request has no unbounded auxiliary I/O,
// fallback model or route. Unsupported request shapes must fail before dispatch.
type MonetaryBoundedDriver interface {
	MonetaryTarget(context.Context, CompleteRequest, ModelProfile) (MonetaryTarget, error)
}
