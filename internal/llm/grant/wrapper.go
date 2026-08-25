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

const terminalPersistenceTimeout = 5 * time.Second

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
	grant := *req.ExternalGrant
	grantMode := llm.EffectiveExternalGrantRouteMode(grant.RouteMode)
	if c.deps.ExternalGrant.RouteMode != "" && grantMode != c.deps.ExternalGrant.RouteMode {
		return llm.CompleteResponse{}, fmt.Errorf("%w: grant route mode is not enabled", llm.ErrExternalGrantInvalid)
	}
	if grantMode == llm.ExternalGrantRouteCoordinatorBound && c.deps.ExternalGrant.Credentials == nil {
		return llm.CompleteResponse{}, fmt.Errorf("%w: credential resolver is not configured", llm.ErrExternalGrantInvalid)
	}
	if mode == llm.ExternalGrantRequired && c.deps.ExternalGrant.Reservations == nil {
		return llm.CompleteResponse{}, fmt.Errorf("%w: durable reservation manager is not configured", llm.ErrExternalGrantInvalid)
	}

	requestedModel := req.Model
	if grantMode == llm.ExternalGrantRouteRuntimeDefault {
		if c.cfg.Provider == "" || c.cfg.Model == "" || (requestedModel != "" && requestedModel != c.cfg.Model) {
			return llm.CompleteResponse{}, fmt.Errorf("%w: runtime-default grant requires the configured provider and model", llm.ErrExternalGrantInvalid)
		}
		if grant.Provider != "" || grant.ProviderModelID != "" || grant.ProviderConnectionID != "" || grant.ProviderConnectionGeneration != 0 || grant.RouteID != "" || grant.CredentialBindingHandle != "" || grant.CredentialAssetGeneration != 0 {
			return llm.CompleteResponse{}, fmt.Errorf("%w: runtime-default grant carries coordinator route claims", llm.ErrExternalGrantInvalid)
		}
	}
	var err error
	req, err = bindRequestDefaults(req, grant, c.cfg)
	if err != nil {
		return llm.CompleteResponse{}, err
	}
	callUnits, err := boundedCallUnits(req, grant, c.cfg)
	if err != nil {
		return llm.CompleteResponse{}, err
	}
	if err := c.verifyOrTopUp(ctx, &grant, req, callUnits); err != nil {
		return llm.CompleteResponse{}, err
	}
	req, err = capRequestToGrant(req, grant)
	if err != nil {
		return llm.CompleteResponse{}, err
	}
	callUnits, err = boundedCallUnits(req, grant, c.cfg)
	if err != nil {
		return llm.CompleteResponse{}, err
	}
	var scope *llm.AttemptScope
	ctx, scope, err = llm.EnsureGrantAttemptScope(ctx, grant)
	if err != nil {
		return llm.CompleteResponse{}, fmt.Errorf("%w: call identity: %w", llm.ErrExternalGrantInvalid, err)
	}
	if scope.FallbackHop > 0 {
		return llm.CompleteResponse{}, llm.ErrExternalGrantCrossProviderFallback
	}
	if grantMode == llm.ExternalGrantRouteCoordinatorBound && c.cfg.Provider != "" && grant.Provider != "" && c.cfg.Provider != grant.Provider {
		return llm.CompleteResponse{}, fmt.Errorf("%w: grant provider %q is not the configured provider", llm.ErrExternalGrantCrossProviderFallback, grant.Provider)
	}
	var reservation llm.LeaseReservation
	if c.deps.ExternalGrant.Reservations != nil {
		epoch := grant.Lease.Epoch
		if epoch == 0 {
			epoch = 1
		}
		reservation, err = c.deps.ExternalGrant.Reservations.Reserve(ctx, llm.LeaseReservationRequest{
			AttemptID: attemptID(grant, scope), LogicalCallID: scope.LogicalCallID, AttemptNonce: scope.AttemptNonce,
			GrantID: grant.GrantID, LeaseID: grant.Lease.LeaseID, OrganizationID: grant.OrganizationID,
			RuntimeID: grant.RuntimeID, AgentID: grant.AgentID,
			Epoch: epoch, Capacity: grant.Lease.RemainingTokens(), Units: callUnits, ExpiresAt: grant.Lease.ExpiresAt,
			Identity: identity.Quadruple{Identity: identity.Identity{TenantID: grant.TenantID, UserID: grant.UserID, SessionID: grant.SessionID}, RunID: grant.LogicalRunID},
		})
		if err != nil {
			return llm.CompleteResponse{}, fmt.Errorf("%w: reserve attempt: %w", llm.ErrExternalGrantLeaseInsufficient, err)
		}
		if reservation.Existing {
			switch reservation.Status {
			case "reserved":
				return llm.CompleteResponse{}, llm.ErrExternalGrantAttemptInFlight
			case "consumed", "released", "expired":
				return llm.CompleteResponse{}, llm.ErrExternalGrantAttemptSettled
			default:
				return llm.CompleteResponse{}, fmt.Errorf("%w: unknown persisted attempt status", llm.ErrExternalGrantAttemptSettled)
			}
		}
	}
	// Never let an unverified caller-provided request field reach the driver;
	// the driver receives the copy that is tied to the successful verification.
	req.ExternalGrant = &grant
	ctx = llm.WithVerifiedGrantContext(ctx, grant, func() llm.CredentialResolver {
		if grantMode == llm.ExternalGrantRouteRuntimeDefault {
			return nil
		}
		return c.deps.ExternalGrant.Credentials
	}())
	started := time.Now().UTC()
	resp, callErr := c.inner.Complete(ctx, req)
	terminalCtx, cancelTerminal := context.WithTimeout(context.WithoutCancel(ctx), terminalPersistenceTimeout)
	defer cancelTerminal()
	if c.deps.ExternalGrant.ReceiptSink == nil && c.deps.ExternalGrant.Reservations == nil {
		if c.deps.ExternalGrant.ReceiptRequired || mode == llm.ExternalGrantRequired {
			return resp, fmt.Errorf("%w: receipt sink is not configured", llm.ErrUsageReceiptUnavailable)
		}
		return resp, callErr
	}
	receipt := makeReceipt(grant, req, resp, callErr, started, time.Now().UTC(), scope, c.cfg)
	hash, err := llm.CanonicalAttemptUsageReceiptBodyHash(receipt)
	if err != nil {
		if c.deps.ExternalGrant.ReceiptRequired || mode == llm.ExternalGrantRequired || c.deps.ExternalGrant.Reservations != nil {
			return resp, fmt.Errorf("%w: canonical body: %w", llm.ErrUsageReceiptUnavailable, err)
		}
		return resp, callErr
	}
	receipt.CanonicalBodyHash = hash
	if err := llm.ValidateAttemptUsageReceiptAgainstGrant(receipt, grant); err != nil {
		if c.deps.ExternalGrant.ReceiptRequired || mode == llm.ExternalGrantRequired || c.deps.ExternalGrant.Reservations != nil {
			return resp, fmt.Errorf("%w: validate receipt: %w", llm.ErrUsageReceiptUnavailable, err)
		}
		return resp, callErr
	}
	if c.deps.ExternalGrant.Reservations != nil {
		used := int64(receipt.TotalTokens)
		if used < 0 {
			used = 0
		}
		if err := c.deps.ExternalGrant.Reservations.Settle(terminalCtx, llm.LeaseSettlement{AttemptID: reservation.AttemptID, LogicalCallID: scope.LogicalCallID, AttemptNonce: scope.AttemptNonce, Receipt: receipt, Units: used, Now: receipt.CompletedAt}); err != nil {
			return resp, fmt.Errorf("%w: settle attempt: %w", llm.ErrUsageReceiptUnavailable, err)
		}
	}
	if c.deps.ExternalGrant.ReceiptSink != nil {
		if err := c.deps.ExternalGrant.ReceiptSink.Enqueue(terminalCtx, receipt); err != nil {
			if c.deps.ExternalGrant.ReceiptRequired || mode == llm.ExternalGrantRequired {
				return resp, fmt.Errorf("%w: %w", llm.ErrUsageReceiptUnavailable, err)
			}
		}
	}
	return resp, callErr
}

func attemptID(grant llm.ExternalGrant, scope *llm.AttemptScope) string {
	attempt, retry, downgrade, hop := 1, 0, 0, 0
	logicalCallID, attemptNonce := grant.LogicalCallID, grant.AttemptNonce
	if scope != nil {
		attempt, retry, downgrade, hop = scope.Attempt, scope.Retry, scope.Downgrade, scope.FallbackHop
		if scope.LogicalCallID != "" {
			logicalCallID = scope.LogicalCallID
		}
		if scope.AttemptNonce != "" {
			attemptNonce = scope.AttemptNonce
		}
		if attempt <= 0 {
			attempt = 1
		}
	}
	return llm.CanonicalAttemptID(grant.GrantID, logicalCallID, attemptNonce, attempt, retry, downgrade, hop)
}

// bindRequestDefaults closes the two ordinary omission paths before the
// signed claims are checked. A grant's model is authoritative when the
// caller relies on Harbor's configured default; likewise the configured
// profile's reasoning default is checked against the grant ceiling. An
// explicitly empty reasoning control remains an intentional provider-default
// request and is not rewritten.
func bindRequestDefaults(req llm.CompleteRequest, grant llm.ExternalGrant, cfg llm.ConfigSnapshot) (llm.CompleteRequest, error) {
	if req.Model == "" {
		if llm.EffectiveExternalGrantRouteMode(grant.RouteMode) == llm.ExternalGrantRouteRuntimeDefault {
			req.Model = cfg.Model
		} else {
			req.Model = grant.ProviderModelID
		}
	}
	if req.Model == "" {
		return llm.CompleteRequest{}, fmt.Errorf("%w: no configured model for grant", llm.ErrExternalGrantInvalid)
	}
	if req.ReasoningEffort == "" && !req.ReasoningEffortExplicit {
		if profile, ok := cfg.ModelProfiles[req.Model]; ok && profile.ReasoningEffort != "" {
			req.ReasoningEffort = profile.ReasoningEffort
		} else {
			req.ReasoningEffort = grant.MaxReasoning
		}
	}
	return req, nil
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
	maxTokens, err := boundedOutputUnits(req, grant, true)
	if err != nil {
		return llm.CompleteRequest{}, err
	}
	value := int(maxTokens)
	req.MaxTokens = &value
	return req, nil
}

func maxInt() int { return int(^uint(0) >> 1) }

func (c *client) verifyOrTopUp(ctx context.Context, grant *llm.ExternalGrant, req llm.CompleteRequest, needed int64) error {
	err := c.deps.ExternalGrant.Verifier.Verify(ctx, *grant, req)
	if err == nil && grant.Lease.RemainingTokens() >= needed {
		return nil
	}
	if err != nil && !isLeaseError(err) {
		return err
	}
	if c.deps.ExternalGrant.TopUpper == nil {
		if err != nil {
			return err
		}
		return fmt.Errorf("%w: total call bound exceeds remaining lease", llm.ErrExternalGrantLeaseInsufficient)
	}
	newGrant, topErr := c.deps.ExternalGrant.TopUpper.TopUp(ctx, *grant, needed)
	if topErr != nil {
		return fmt.Errorf("%w: top-up failed: %w", llm.ErrExternalGrantLeaseInsufficient, topErr)
	}
	if err := llm.ValidateExternalGrantTopUpSuccessor(*grant, newGrant, needed); err != nil {
		return err
	}
	*grant = newGrant
	if err := c.deps.ExternalGrant.Verifier.Verify(ctx, *grant, req); err != nil {
		return err
	}
	if grant.Lease.RemainingTokens() < needed {
		return fmt.Errorf("%w: top-up does not cover total call bound", llm.ErrExternalGrantLeaseInsufficient)
	}
	return nil
}

func boundedCallUnits(req llm.CompleteRequest, grant llm.ExternalGrant, cfg llm.ConfigSnapshot) (int64, error) {
	output, err := boundedOutputUnits(req, grant, false)
	if err != nil {
		return 0, err
	}
	prompt := int64(llm.EstimateRequestTokens(req, cfg.ModelProfiles[req.Model]))
	if prompt < 0 || output > math.MaxInt64-prompt {
		return 0, fmt.Errorf("%w: total call bound exceeds local integer range", llm.ErrExternalGrantInvalid)
	}
	total := prompt + output
	if total <= 0 {
		return 0, fmt.Errorf("%w: total call bound must be positive", llm.ErrExternalGrantInvalid)
	}
	return total, nil
}

func boundedOutputUnits(req llm.CompleteRequest, grant llm.ExternalGrant, constrainToRemaining bool) (int64, error) {
	if req.MaxTokens != nil {
		if *req.MaxTokens <= 0 {
			return 0, fmt.Errorf("%w: output limit must be positive", llm.ErrExternalGrantInvalid)
		}
		return int64(*req.MaxTokens), nil
	}
	units := int64(grant.MaxOutputTokens)
	if constrainToRemaining {
		remaining := grant.Lease.RemainingTokens()
		if remaining <= 0 {
			return 0, fmt.Errorf("%w: no output lease remains", llm.ErrExternalGrantLeaseInsufficient)
		}
		if remaining < units {
			units = remaining
		}
	}
	if units <= 0 || units > int64(maxInt()) {
		return 0, fmt.Errorf("%w: output lease exceeds local integer range", llm.ErrExternalGrantInvalid)
	}
	return units, nil
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

func makeReceipt(grant llm.ExternalGrant, req llm.CompleteRequest, resp llm.CompleteResponse, callErr error, started, completed time.Time, scope *llm.AttemptScope, cfg llm.ConfigSnapshot) llm.AttemptUsageReceipt {
	status := "success"
	if callErr != nil {
		status = "error"
		if errors.Is(callErr, context.Canceled) || errors.Is(callErr, context.DeadlineExceeded) {
			status = "canceled"
		}
	}
	attempt, retry, downgrade, hop := 1, 0, 0, 0
	logicalCallID, attemptNonce := grant.LogicalCallID, grant.AttemptNonce
	parentLogicalCallID, parentAttemptNonce, plannerStep := grant.LogicalCallID, grant.AttemptNonce, 0
	// The signed logical-call id and nonce anchor receipt identity. A true
	// response-loss retry carries the same grant and therefore reuses the same
	// durable identity; planner steps derive distinct child identities before
	// reaching this boundary.
	if scope != nil {
		attempt, retry, downgrade, hop = scope.Attempt, scope.Retry, scope.Downgrade, scope.FallbackHop
		if scope.LogicalCallID != "" {
			logicalCallID = scope.LogicalCallID
		}
		if scope.AttemptNonce != "" {
			attemptNonce = scope.AttemptNonce
		}
		if scope.ParentLogicalCallID != "" {
			parentLogicalCallID = scope.ParentLogicalCallID
		}
		if scope.ParentAttemptNonce != "" {
			parentAttemptNonce = scope.ParentAttemptNonce
		}
		plannerStep = scope.PlannerStep
		if attempt <= 0 {
			attempt = 1
		}
	}
	idempotency := attemptID(grant, scope)
	provider := grant.Provider
	model := grant.ProviderModelID
	if llm.EffectiveExternalGrantRouteMode(grant.RouteMode) == llm.ExternalGrantRouteRuntimeDefault {
		provider = cfg.Provider
		model = req.Model
	}
	receipt := llm.AttemptUsageReceipt{
		ReceiptID: idempotency, GrantID: grant.GrantID, RouteMode: grant.RouteMode, LogicalCallID: logicalCallID, AttemptNonce: attemptNonce,
		ParentLogicalCallID: parentLogicalCallID, ParentAttemptNonce: parentAttemptNonce, PlannerStep: plannerStep, OrganizationID: grant.OrganizationID,
		RuntimeID: grant.RuntimeID, AgentID: grant.AgentID, TenantID: grant.TenantID, UserID: grant.UserID,
		SessionID: grant.SessionID, LogicalRunID: grant.LogicalRunID, Provider: provider,
		ProviderModelID: model, ProviderConnectionID: grant.ProviderConnectionID,
		ProviderConnectionGeneration: grant.ProviderConnectionGeneration,
		RouteID:                      grant.RouteID, CredentialAssetGeneration: grant.CredentialAssetGeneration,
		PolicyGeneration: grant.PolicyGeneration, AttemptNumber: attempt, RetryNumber: retry, DowngradeNumber: downgrade,
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
