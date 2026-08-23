package grant

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync/atomic"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
)

const receiptEnqueueTimeout = 5 * time.Second

func init() { llm.RegisterExternalGrantWrapper(Wrap) }

// Wrap inserts grant verification immediately inside the retry and downgrade
// layers. Consequently every provider attempt, including corrective retries,
// structured-output downgrades, and failover reissues, must present the same
// signed context-bound grant before the driver is reached.
func Wrap(inner llm.LLMClient, cfg llm.ConfigSnapshot, deps llm.Deps) llm.LLMClient {
	if inner == nil {
		panic("llm/grant: nil inner client")
	}
	if deps.ExternalGrant.Mode == "" || deps.ExternalGrant.Mode == llm.ExternalGrantDisabled {
		return inner
	}
	return &client{inner: inner, cfg: cfg, deps: deps}
}

type client struct {
	inner  llm.LLMClient
	cfg    llm.ConfigSnapshot
	deps   llm.Deps
	closed atomic.Bool
}

var _ llm.LLMClient = (*client)(nil)

func (c *client) Complete(ctx context.Context, req llm.CompleteRequest) (llm.CompleteResponse, error) {
	if c.closed.Load() {
		return llm.CompleteResponse{}, llm.ErrClientClosed
	}
	mode := c.deps.ExternalGrant.Mode
	if (mode == llm.ExternalGrantRequired || c.deps.ExternalGrant.ReceiptRequired) && c.deps.ExternalGrant.ReceiptSink == nil {
		return llm.CompleteResponse{}, fmt.Errorf("%w: receipt sink is not configured", llm.ErrUsageReceiptUnavailable)
	}
	if req.ExternalGrant == nil {
		if mode == llm.ExternalGrantRequired {
			return llm.CompleteResponse{}, llm.ErrExternalGrantRequired
		}
		return c.inner.Complete(ctx, req)
	}
	if c.deps.ExternalGrant.Verifier == nil {
		return llm.CompleteResponse{}, fmt.Errorf("%w: verifier is not configured", llm.ErrExternalGrantInvalid)
	}
	if c.deps.ExternalGrant.Credentials == nil {
		return llm.CompleteResponse{}, fmt.Errorf("%w: credential resolver is not configured", llm.ErrExternalGrantInvalid)
	}

	grant := *req.ExternalGrant
	req = bindRequestDefaults(req, grant, c.cfg)
	if err := c.verifyOrTopUp(ctx, &grant, req); err != nil {
		return llm.CompleteResponse{}, err
	}
	var err error
	req, err = capRequestToGrant(req, grant)
	if err != nil {
		return llm.CompleteResponse{}, err
	}
	var scope *llm.AttemptScope
	ctx, scope, err = llm.EnsureAttemptScope(ctx)
	if err != nil {
		return llm.CompleteResponse{}, fmt.Errorf("%w: call identity: %v", llm.ErrExternalGrantInvalid, err)
	}
	if scope.FallbackHop > 0 {
		return llm.CompleteResponse{}, llm.ErrExternalGrantCrossProviderFallback
	}
	if c.cfg.Provider != "" && grant.Provider != "" && c.cfg.Provider != grant.Provider {
		return llm.CompleteResponse{}, fmt.Errorf("%w: grant provider %q is not the configured provider", llm.ErrExternalGrantCrossProviderFallback, grant.Provider)
	}
	var reservation llm.LeaseReservation
	if c.deps.ExternalGrant.Reservations != nil {
		epoch := grant.Lease.Epoch
		if epoch == 0 {
			epoch = 1
		}
		units := grant.Lease.RemainingTokens()
		if units <= 0 {
			return llm.CompleteResponse{}, llm.ErrExternalGrantLeaseInsufficient
		}
		reservation, err = c.deps.ExternalGrant.Reservations.Reserve(ctx, llm.LeaseReservationRequest{
			AttemptID: attemptID(grant, scope), GrantID: grant.GrantID, LeaseID: grant.Lease.LeaseID,
			Epoch: epoch, Capacity: grant.Lease.TokenUnits, Units: units, ExpiresAt: grant.Lease.ExpiresAt,
			Identity: identity.Quadruple{Identity: identity.Identity{TenantID: grant.TenantID, UserID: grant.UserID, SessionID: grant.SessionID}, RunID: grant.LogicalRunID},
		})
		if err != nil {
			return llm.CompleteResponse{}, fmt.Errorf("%w: reserve attempt: %v", llm.ErrExternalGrantLeaseInsufficient, err)
		}
	}
	// Never let an unverified caller-provided request field reach the driver;
	// the driver receives the copy that is tied to the successful verification.
	req.ExternalGrant = &grant
	ctx = llm.WithVerifiedGrantContext(ctx, grant, c.deps.ExternalGrant.Credentials)
	started := time.Now().UTC()
	resp, callErr := c.inner.Complete(ctx, req)
	if c.deps.ExternalGrant.ReceiptSink == nil {
		if c.deps.ExternalGrant.ReceiptRequired || mode == llm.ExternalGrantRequired {
			return resp, fmt.Errorf("%w: receipt sink is not configured", llm.ErrUsageReceiptUnavailable)
		}
		return resp, callErr
	}
	receipt := makeReceipt(grant, req, resp, callErr, started, time.Now().UTC(), scope)
	hash, err := llm.CanonicalAttemptUsageReceiptBodyHash(receipt)
	if err != nil {
		if c.deps.ExternalGrant.ReceiptRequired || mode == llm.ExternalGrantRequired {
			return resp, fmt.Errorf("%w: canonical body: %v", llm.ErrUsageReceiptUnavailable, err)
		}
		return resp, callErr
	}
	receipt.CanonicalBodyHash = hash
	if c.deps.ExternalGrant.Reservations != nil {
		used := int64(receipt.TotalTokens)
		if used < 0 {
			used = 0
		}
		if err := c.deps.ExternalGrant.Reservations.Settle(ctx, llm.LeaseSettlement{AttemptID: reservation.AttemptID, Receipt: receipt, Units: used, Now: receipt.CompletedAt}); err != nil {
			return resp, fmt.Errorf("%w: settle attempt: %v", llm.ErrUsageReceiptUnavailable, err)
		}
	}
	receiptCtx, cancelReceipt := context.WithTimeout(context.WithoutCancel(ctx), receiptEnqueueTimeout)
	defer cancelReceipt()
	if err := c.deps.ExternalGrant.ReceiptSink.Enqueue(receiptCtx, receipt); err != nil {
		if c.deps.ExternalGrant.ReceiptRequired || mode == llm.ExternalGrantRequired {
			return resp, fmt.Errorf("%w: %v", llm.ErrUsageReceiptUnavailable, err)
		}
	}
	return resp, callErr
}

func attemptID(grant llm.ExternalGrant, scope *llm.AttemptScope) string {
	attempt, retry, downgrade, hop := 1, 0, 0, 0
	if scope != nil {
		attempt, retry, downgrade, hop = scope.Attempt, scope.Retry, scope.Downgrade, scope.FallbackHop
		if attempt <= 0 {
			attempt = 1
		}
	}
	return fmt.Sprintf("%s/%d/%d/%d/%d", grant.GrantID, retry, downgrade, hop, attempt)
}

// bindRequestDefaults closes the two ordinary omission paths before the
// signed claims are checked. A grant's model is authoritative when the
// caller relies on Harbor's configured default; likewise the configured
// profile's reasoning default is checked against the grant ceiling. An
// explicitly empty reasoning control remains an intentional provider-default
// request and is not rewritten.
func bindRequestDefaults(req llm.CompleteRequest, grant llm.ExternalGrant, cfg llm.ConfigSnapshot) llm.CompleteRequest {
	if req.Model == "" {
		req.Model = grant.ProviderModelID
	}
	if req.ReasoningEffort == "" && !req.ReasoningEffortExplicit {
		if profile, ok := cfg.ModelProfiles[req.Model]; ok && profile.ReasoningEffort != "" {
			req.ReasoningEffort = profile.ReasoningEffort
		} else {
			req.ReasoningEffort = grant.MaxReasoning
		}
	}
	return req
}

// capRequestToGrant makes an omitted output limit explicit at the provider
// boundary. An absent MaxTokens cannot mean "use an unbounded provider
// default" once an external grant has supplied a finite output ceiling or
// lease; the narrower of those two bounds is the safe default. An explicit
// caller value is validated by the verifier and is never silently reduced.
func capRequestToGrant(req llm.CompleteRequest, grant llm.ExternalGrant) (llm.CompleteRequest, error) {
	if req.MaxTokens != nil {
		return req, nil
	}
	remaining := grant.Lease.RemainingTokens()
	if remaining <= 0 {
		return llm.CompleteRequest{}, fmt.Errorf("%w: no output lease remains", llm.ErrExternalGrantLeaseInsufficient)
	}
	maxTokens := int64(grant.MaxOutputTokens)
	if remaining < maxTokens {
		maxTokens = remaining
	}
	if maxTokens <= 0 || maxTokens > int64(maxInt()) {
		return llm.CompleteRequest{}, fmt.Errorf("%w: output lease exceeds local integer range", llm.ErrExternalGrantInvalid)
	}
	value := int(maxTokens)
	req.MaxTokens = &value
	return req, nil
}

func maxInt() int { return int(^uint(0) >> 1) }

func (c *client) verifyOrTopUp(ctx context.Context, grant *llm.ExternalGrant, req llm.CompleteRequest) error {
	err := c.deps.ExternalGrant.Verifier.Verify(ctx, *grant, req)
	if err == nil {
		return nil
	}
	if c.deps.ExternalGrant.TopUpper == nil || !isLeaseError(err) {
		return err
	}
	needed := int64(1)
	if req.MaxTokens != nil && *req.MaxTokens > 0 {
		needed = int64(*req.MaxTokens)
	}
	newGrant, topErr := c.deps.ExternalGrant.TopUpper.TopUp(ctx, *grant, needed)
	if topErr != nil {
		return fmt.Errorf("%w: top-up failed: %v", llm.ErrExternalGrantLeaseInsufficient, topErr)
	}
	*grant = newGrant
	if err := c.deps.ExternalGrant.Verifier.Verify(ctx, *grant, req); err != nil {
		return err
	}
	return nil
}

func isLeaseError(err error) bool {
	return errors.Is(err, llm.ErrExternalGrantLeaseInsufficient)
}

func (c *client) Close(ctx context.Context) error {
	if !c.closed.CompareAndSwap(false, true) {
		return nil
	}
	return c.inner.Close(ctx)
}

func makeReceipt(grant llm.ExternalGrant, req llm.CompleteRequest, resp llm.CompleteResponse, callErr error, started, completed time.Time, scope *llm.AttemptScope) llm.AttemptUsageReceipt {
	status := "success"
	if callErr != nil {
		status = "error"
		if errors.Is(callErr, context.Canceled) || errors.Is(callErr, context.DeadlineExceeded) {
			status = "canceled"
		}
	}
	attempt, retry, hop := 1, 0, 0
	// GrantID is the coordinator's stable logical-call id. It deliberately
	// anchors receipt identity instead of the locally generated AttemptScope
	// id, so a caller retry after response loss re-enqueues the same receipt
	// and the StateStore outbox/delivery idempotency key converges.
	if scope != nil {
		attempt, retry, hop = scope.Attempt, scope.Retry, scope.FallbackHop
		if attempt <= 0 {
			attempt = 1
		}
	}
	idempotency := attemptID(grant, scope)
	receipt := llm.AttemptUsageReceipt{
		ReceiptID: idempotency, GrantID: grant.GrantID, OrganizationID: grant.OrganizationID,
		RuntimeID: grant.RuntimeID, TenantID: grant.TenantID, UserID: grant.UserID,
		SessionID: grant.SessionID, LogicalRunID: grant.LogicalRunID, Provider: grant.Provider,
		ProviderModelID: grant.ProviderModelID, ProviderConnectionID: grant.ProviderConnectionID,
		ProviderConnectionGeneration: grant.ProviderConnectionGeneration,
		RouteID:                      grant.RouteID, CredentialAssetGeneration: grant.CredentialAssetGeneration,
		PolicyGeneration: grant.PolicyGeneration, AttemptNumber: attempt, RetryNumber: retry,
		FallbackHop: hop, RequestedReasoning: req.ReasoningEffort, EffectiveReasoning: req.ReasoningEffort,
		PromptTokens: resp.Usage.PromptTokens, CompletionTokens: resp.Usage.CompletionTokens,
		ReasoningTokens: resp.Usage.ReasoningTokens, TotalTokens: resp.Usage.TotalTokens,
		CacheReadTokens: resp.Usage.CacheReadTokens, CacheWriteTokens: resp.Usage.CacheWriteTokens,
		InputCostMicros: micros(resp.Cost.InputTokensCost), OutputCostMicros: micros(resp.Cost.OutputTokensCost),
		ReasoningCostMicros: micros(resp.Cost.ReasoningTokensCost), TotalCostMicros: micros(resp.Cost.TotalCost),
		Currency: resp.Cost.Currency, LatencyMS: resp.Usage.LatencyMS, Status: status,
		StartedAt: started, CompletedAt: completed, IdempotencyKey: idempotency,
	}
	return receipt
}

func micros(value float64) int64 {
	if value <= 0 || math.IsNaN(value) || math.IsInf(value, 0) {
		return 0
	}
	return int64(math.Round(value * 1_000_000))
}
