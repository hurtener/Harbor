package grant

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/llm/leases"
	"github.com/hurtener/Harbor/internal/llm/retry"
	stateinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
)

type recordingClient struct {
	mu       sync.Mutex
	contexts []llm.VerifiedGrantContext
	requests []llm.CompleteRequest
	response llm.CompleteResponse
	err      error
}

func (c *recordingClient) Complete(ctx context.Context, req llm.CompleteRequest) (llm.CompleteResponse, error) {
	c.mu.Lock()
	if verified, ok := llm.VerifiedGrantContextFrom(ctx); ok {
		c.contexts = append(c.contexts, verified)
	}
	c.requests = append(c.requests, req)
	c.mu.Unlock()
	return c.response, c.err
}

func (*recordingClient) Close(context.Context) error { return nil }

type recordingSink struct {
	mu       sync.Mutex
	receipts []llm.AttemptUsageReceipt
	err      error
}

type blockingClient struct {
	started   chan struct{}
	release   chan struct{}
	startOnce sync.Once
	calls     atomic.Int32
	response  llm.CompleteResponse
}

func (c *blockingClient) Complete(ctx context.Context, _ llm.CompleteRequest) (llm.CompleteResponse, error) {
	c.calls.Add(1)
	c.startOnce.Do(func() { close(c.started) })
	select {
	case <-c.release:
		return c.response, nil
	case <-ctx.Done():
		return llm.CompleteResponse{}, ctx.Err()
	}
}

func (*blockingClient) Close(context.Context) error { return nil }

type topUpperFunc func(context.Context, llm.ExternalGrant, int64) (llm.ExternalGrant, error)

func newTestReservations(t *testing.T) *leases.Store {
	t.Helper()
	stateStore, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stateStore.Close(context.Background()) })
	reservations, err := leases.New(stateStore, testClock)
	if err != nil {
		t.Fatal(err)
	}
	return reservations
}

func (f topUpperFunc) TopUp(ctx context.Context, grant llm.ExternalGrant, needed int64) (llm.ExternalGrant, error) {
	return f(ctx, grant, needed)
}

func (s *recordingSink) Enqueue(_ context.Context, receipt llm.AttemptUsageReceipt) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	s.receipts = append(s.receipts, receipt)
	return nil
}

func TestWrap_RequiredGrantResolvesOnlyVerifiedContextAndEmitsContentFreeReceipt(t *testing.T) {
	signer, err := NewSigner("key-1", "harbor-runtime", nil, testClock)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := NewVerifier(VerifierConfig{Audience: "harbor-runtime", RuntimeID: "runtime-1", Keys: map[string]ed25519.PublicKey{"key-1": signer.PublicKey()}, Clock: testClock})
	if err != nil {
		t.Fatal(err)
	}
	binding := NewBindingStore()
	if err := binding.Put(Binding{Handle: "binding-a", OrganizationID: "org-a", RuntimeID: "runtime-1", Provider: "openai", ProviderConnectionID: "connection-a", ProviderConnectionGeneration: 1, Generation: 1, Secret: "credential-a"}); err != nil {
		t.Fatal(err)
	}
	signed, err := signer.Sign(testGrant())
	if err != nil {
		t.Fatal(err)
	}
	inner := &recordingClient{response: llm.CompleteResponse{Usage: llm.Usage{PromptTokens: 2, CompletionTokens: 3, TotalTokens: 5}}}
	sink := &recordingSink{}
	client := Wrap(inner, llm.ConfigSnapshot{}, llm.Deps{ExternalGrant: llm.ExternalGrantConfig{
		Mode: llm.ExternalGrantRequired, Verifier: verifier, Credentials: binding, Reservations: newTestReservations(t), ReceiptSink: sink, ReceiptRequired: true,
	}})
	ctx := testContext(t, "org-a")
	resp, err := client.Complete(ctx, llm.CompleteRequest{Model: "model-fast", ExternalGrant: &signed})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Usage.TotalTokens != 5 {
		t.Fatalf("response usage = %+v", resp.Usage)
	}
	inner.mu.Lock()
	if len(inner.contexts) != 1 || inner.contexts[0].Grant.CredentialBindingHandle != "binding-a" {
		t.Fatalf("provider context = %+v", inner.contexts)
	}
	if len(inner.requests) != 1 || inner.requests[0].MaxTokens == nil || *inner.requests[0].MaxTokens != 1000 {
		t.Fatalf("provider output ceiling = %+v", inner.requests)
	}
	if inner.requests[0].Model != "model-fast" || inner.requests[0].ReasoningEffort != llm.ReasoningMedium {
		t.Fatalf("provider grant defaults = %+v", inner.requests[0])
	}
	inner.mu.Unlock()
	sink.mu.Lock()
	if len(sink.receipts) != 1 {
		t.Fatalf("receipt count = %d, want 1", len(sink.receipts))
	}
	receipt := sink.receipts[0]
	sink.mu.Unlock()
	if receipt.CanonicalBodyHash == "" || receipt.TotalTokens != 5 || receipt.Status != "success" {
		t.Fatalf("receipt = %+v", receipt)
	}
	if receiptJSON, marshalErr := llm.CanonicalAttemptUsageReceiptBodyHash(receipt); marshalErr != nil || receiptJSON != receipt.CanonicalBodyHash {
		t.Fatalf("receipt hash = %q, recompute=%q err=%v", receipt.CanonicalBodyHash, receiptJSON, marshalErr)
	}
	if got := string(mustJSON(t, receipt)); containsSecret(got, "credential-a") {
		t.Fatalf("receipt contains provider secret: %s", got)
	}
}

func TestWrap_StrictModeRejectsMissingAndStaleGrant(t *testing.T) {
	signer, err := NewSigner("key-1", "harbor-runtime", nil, testClock)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := NewVerifier(VerifierConfig{Audience: "harbor-runtime", RuntimeID: "runtime-1", Keys: map[string]ed25519.PublicKey{"key-1": signer.PublicKey()}, Clock: testClock})
	if err != nil {
		t.Fatal(err)
	}
	binding := NewBindingStore()
	if err := binding.Put(Binding{Handle: "binding-a", OrganizationID: "org-a", RuntimeID: "runtime-1", Provider: "openai", ProviderConnectionID: "connection-a", ProviderConnectionGeneration: 1, Generation: 1, Secret: "credential-a"}); err != nil {
		t.Fatal(err)
	}
	signed, err := signer.Sign(testGrant())
	if err != nil {
		t.Fatal(err)
	}
	client := Wrap(&recordingClient{}, llm.ConfigSnapshot{}, llm.Deps{ExternalGrant: llm.ExternalGrantConfig{Mode: llm.ExternalGrantRequired, Verifier: verifier, Credentials: binding, ReceiptSink: &recordingSink{}, ReceiptRequired: true}})
	if _, err := client.Complete(testContext(t, "org-a"), llm.CompleteRequest{Model: "model-fast"}); !errors.Is(err, llm.ErrExternalGrantRequired) {
		t.Fatalf("missing grant = %v, want ErrExternalGrantRequired", err)
	}
	if _, err := client.Complete(testContext(t, "org-b"), llm.CompleteRequest{Model: "model-fast", ExternalGrant: &signed}); !errors.Is(err, llm.ErrExternalGrantInvalid) {
		t.Fatalf("wrong organization = %v, want ErrExternalGrantInvalid", err)
	}
}

func TestWrap_TopsUpBoundedLeaseBeforeProviderCall(t *testing.T) {
	signer, err := NewSigner("key-1", "harbor-runtime", nil, testClock)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := NewVerifier(VerifierConfig{Audience: "harbor-runtime", RuntimeID: "runtime-1", Keys: map[string]ed25519.PublicKey{"key-1": signer.PublicKey()}, Clock: testClock})
	if err != nil {
		t.Fatal(err)
	}
	binding := NewBindingStore()
	if err := binding.Put(Binding{Handle: "binding-a", OrganizationID: "org-a", RuntimeID: "runtime-1", Provider: "openai", ProviderConnectionID: "connection-a", ProviderConnectionGeneration: 1, Generation: 1, Secret: "credential-a"}); err != nil {
		t.Fatal(err)
	}
	grant := testGrant()
	grant.Lease.TokenUnits = 10
	grant.Lease.LeaseID = "lease-small"
	signed, err := signer.Sign(grant)
	if err != nil {
		t.Fatal(err)
	}
	var requested int64
	topUpper := topUpperFunc(func(_ context.Context, old llm.ExternalGrant, needed int64) (llm.ExternalGrant, error) {
		requested = needed
		old.Lease.TokenUnits = 200
		old.Lease.LeaseID = "lease-topup"
		return signer.Sign(old)
	})
	inner := &recordingClient{response: llm.CompleteResponse{Usage: llm.Usage{TotalTokens: 4}}}
	client := Wrap(inner, llm.ConfigSnapshot{}, llm.Deps{ExternalGrant: llm.ExternalGrantConfig{
		Mode: llm.ExternalGrantRequired, Verifier: verifier, Credentials: binding,
		Reservations: newTestReservations(t), TopUpper: topUpper, ReceiptSink: &recordingSink{}, ReceiptRequired: true,
	}})
	maxTokens := 100
	if _, err := client.Complete(testContext(t, "org-a"), llm.CompleteRequest{Model: "model-fast", MaxTokens: &maxTokens, ExternalGrant: &signed}); err != nil {
		t.Fatalf("Complete with top-up: %v", err)
	}
	if requested != 100 || len(inner.requests) != 1 || inner.requests[0].MaxTokens == nil || *inner.requests[0].MaxTokens != 100 {
		t.Fatalf("top-up requested=%d requests=%+v", requested, inner.requests)
	}
}

func TestWrap_GrantIsRecheckedForEveryRetryAttempt(t *testing.T) {
	signer, err := NewSigner("key-1", "harbor-runtime", nil, testClock)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := NewVerifier(VerifierConfig{Audience: "harbor-runtime", RuntimeID: "runtime-1", Keys: map[string]ed25519.PublicKey{"key-1": signer.PublicKey()}, Clock: testClock})
	if err != nil {
		t.Fatal(err)
	}
	binding := NewBindingStore()
	if err := binding.Put(Binding{Handle: "binding-a", OrganizationID: "org-a", RuntimeID: "runtime-1", Provider: "openai", ProviderConnectionID: "connection-a", ProviderConnectionGeneration: 1, Generation: 1, Secret: "credential-a"}); err != nil {
		t.Fatal(err)
	}
	signed, err := signer.Sign(testGrant())
	if err != nil {
		t.Fatal(err)
	}
	sink := &recordingSink{}
	inner := &recordingClient{response: llm.CompleteResponse{Usage: llm.Usage{TotalTokens: 1}}}
	granted := Wrap(inner, llm.ConfigSnapshot{}, llm.Deps{ExternalGrant: llm.ExternalGrantConfig{Mode: llm.ExternalGrantRequired, Verifier: verifier, Credentials: binding, Reservations: newTestReservations(t), ReceiptSink: sink, ReceiptRequired: true}})
	wrapped := retry.Wrap(granted, llm.ConfigSnapshot{ModelProfiles: map[string]llm.ModelProfile{"model-fast": {MaxRetries: 1}}}, llm.Deps{})
	validatorCalls := 0
	_, err = wrapped.Complete(testContext(t, "org-a"), llm.CompleteRequest{Model: "model-fast", ExternalGrant: &signed, Validator: func(llm.CompleteResponse) error {
		validatorCalls++
		if validatorCalls == 1 {
			return errors.New("retry once")
		}
		return nil
	}})
	if err != nil {
		t.Fatalf("retry Complete: %v", err)
	}
	if validatorCalls != 2 || len(sink.receipts) != 2 || len(inner.contexts) != 2 {
		t.Fatalf("validator=%d receipts=%d contexts=%d", validatorCalls, len(sink.receipts), len(inner.contexts))
	}
	if sink.receipts[0].ReceiptID == sink.receipts[1].ReceiptID || sink.receipts[0].AttemptNumber == sink.receipts[1].AttemptNumber {
		t.Fatalf("retry receipt coordinates did not advance: %+v", sink.receipts)
	}
}

func TestWrap_DurableAttemptIdentityBlocksConcurrentAndResponseLossReplay(t *testing.T) {
	signer, err := NewSigner("key-1", "harbor-runtime", nil, testClock)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := NewVerifier(VerifierConfig{Audience: "harbor-runtime", RuntimeID: "runtime-1", Keys: map[string]ed25519.PublicKey{"key-1": signer.PublicKey()}, Clock: testClock})
	if err != nil {
		t.Fatal(err)
	}
	binding := NewBindingStore()
	if err := binding.Put(Binding{Handle: "binding-a", OrganizationID: "org-a", RuntimeID: "runtime-1", Provider: "openai", ProviderConnectionID: "connection-a", ProviderConnectionGeneration: 1, Generation: 1, Secret: "credential-a"}); err != nil {
		t.Fatal(err)
	}
	signed, err := signer.Sign(testGrant())
	if err != nil {
		t.Fatal(err)
	}
	provider := &blockingClient{
		started:  make(chan struct{}),
		release:  make(chan struct{}),
		response: llm.CompleteResponse{Usage: llm.Usage{TotalTokens: 5}},
	}
	sink := &recordingSink{err: errors.New("receipt enqueue lost")}
	client := Wrap(provider, llm.ConfigSnapshot{}, llm.Deps{ExternalGrant: llm.ExternalGrantConfig{
		Mode: llm.ExternalGrantRequired, Verifier: verifier, Credentials: binding,
		Reservations: newTestReservations(t), ReceiptSink: sink, ReceiptRequired: true,
	}})
	ctx := testContext(t, "org-a")
	firstDone := make(chan error, 1)
	go func() {
		_, completeErr := client.Complete(ctx, llm.CompleteRequest{Model: "model-fast", ExternalGrant: &signed})
		firstDone <- completeErr
	}()
	select {
	case <-provider.started:
	case <-time.After(time.Second):
		t.Fatal("first provider call did not start")
	}
	secondDone := make(chan error, 1)
	go func() {
		_, completeErr := client.Complete(ctx, llm.CompleteRequest{Model: "model-fast", ExternalGrant: &signed})
		secondDone <- completeErr
	}()
	select {
	case secondErr := <-secondDone:
		if !errors.Is(secondErr, llm.ErrExternalGrantAttemptInFlight) {
			t.Fatalf("concurrent duplicate = %v, want ErrExternalGrantAttemptInFlight", secondErr)
		}
	case <-time.After(time.Second):
		t.Fatal("concurrent duplicate did not return")
	}
	close(provider.release)
	if firstErr := <-firstDone; !errors.Is(firstErr, llm.ErrUsageReceiptUnavailable) {
		t.Fatalf("response-loss provider call = %v, want receipt failure", firstErr)
	}
	if got := provider.calls.Load(); got != 1 {
		t.Fatalf("provider calls after first settlement = %d, want one", got)
	}
	if _, replayErr := client.Complete(ctx, llm.CompleteRequest{Model: "model-fast", ExternalGrant: &signed}); !errors.Is(replayErr, llm.ErrExternalGrantAttemptSettled) {
		t.Fatalf("response-loss replay = %v, want ErrExternalGrantAttemptSettled", replayErr)
	}
	if got := provider.calls.Load(); got != 1 {
		t.Fatalf("provider calls after replay = %d, want one", got)
	}
}

func TestWrap_GrantAttemptIdentityIsStablePerPlannerStep(t *testing.T) {
	signer, err := NewSigner("key-1", "harbor-runtime", nil, testClock)
	if err != nil {
		t.Fatal(err)
	}
	grant, err := signer.Sign(testGrant())
	if err != nil {
		t.Fatal(err)
	}
	base := testContext(t, "org-a")
	stepOneCtx, stepOne, err := llm.EnsureGrantAttemptScope(llm.WithAttemptStep(base, 1), grant)
	if err != nil {
		t.Fatal(err)
	}
	_, stepOneReplay, err := llm.EnsureGrantAttemptScope(llm.WithAttemptStep(base, 1), grant)
	if err != nil {
		t.Fatal(err)
	}
	stepTwoCtx, stepTwo, err := llm.EnsureGrantAttemptScope(llm.WithAttemptStep(base, 2), grant)
	if err != nil {
		t.Fatal(err)
	}
	if stepOne.LogicalCallID != grant.LogicalCallID+"/step/1" || stepTwo.LogicalCallID != grant.LogicalCallID+"/step/2" {
		t.Fatalf("planner step logical IDs = %q, %q", stepOne.LogicalCallID, stepTwo.LogicalCallID)
	}
	if stepOne.AttemptNonce == stepTwo.AttemptNonce || stepOne.AttemptNonce != stepOneReplay.AttemptNonce {
		t.Fatalf("planner step nonces are not deterministic/distinct: %q %q %q", stepOne.AttemptNonce, stepTwo.AttemptNonce, stepOneReplay.AttemptNonce)
	}
	if scoped, ok := llm.AttemptScopeFrom(stepOneCtx); !ok || scoped.LogicalCallID != stepOne.LogicalCallID {
		t.Fatalf("step scope was not installed: %+v, %v", scoped, ok)
	}

	verifier, err := NewVerifier(VerifierConfig{
		Audience: "harbor-runtime", RuntimeID: "runtime-1",
		Keys: map[string]ed25519.PublicKey{"key-1": signer.PublicKey()}, Clock: testClock,
	})
	if err != nil {
		t.Fatal(err)
	}
	binding := NewBindingStore()
	if err := binding.Put(Binding{Handle: "binding-a", OrganizationID: "org-a", RuntimeID: "runtime-1", Provider: "openai", ProviderConnectionID: "connection-a", ProviderConnectionGeneration: 1, Generation: 1, Secret: "credential-a"}); err != nil {
		t.Fatal(err)
	}
	provider := &recordingClient{response: llm.CompleteResponse{Usage: llm.Usage{TotalTokens: 1}}}
	sink := &recordingSink{}
	client := Wrap(provider, llm.ConfigSnapshot{}, llm.Deps{ExternalGrant: llm.ExternalGrantConfig{
		Mode: llm.ExternalGrantRequired, Verifier: verifier, Credentials: binding,
		Reservations: newTestReservations(t), ReceiptSink: sink, ReceiptRequired: true,
	}})
	request := llm.CompleteRequest{Model: "model-fast", ExternalGrant: &grant}
	if _, err := client.Complete(stepOneCtx, request); err != nil {
		t.Fatalf("planner step one: %v", err)
	}
	if _, err := client.Complete(stepTwoCtx, request); err != nil {
		t.Fatalf("planner step two: %v", err)
	}
	if _, err := client.Complete(stepOneCtx, request); !errors.Is(err, llm.ErrExternalGrantAttemptSettled) {
		t.Fatalf("planner step replay = %v, want ErrExternalGrantAttemptSettled", err)
	}
	provider.mu.Lock()
	providerCalls := len(provider.requests)
	provider.mu.Unlock()
	sink.mu.Lock()
	receipts := append([]llm.AttemptUsageReceipt(nil), sink.receipts...)
	sink.mu.Unlock()
	if providerCalls != 2 || len(receipts) != 2 {
		t.Fatalf("provider calls=%d receipts=%d, want two distinct steps", providerCalls, len(receipts))
	}
	if receipts[0].PlannerStep != 1 || receipts[1].PlannerStep != 2 || receipts[0].ReceiptID == receipts[1].ReceiptID {
		t.Fatalf("planner-step receipts are not distinct/canonical: %+v", receipts)
	}
	for _, receipt := range receipts {
		if err := llm.ValidateAttemptUsageReceiptAgainstGrant(receipt, grant); err != nil {
			t.Fatalf("planner-step receipt validation: %v", err)
		}
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	b, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func containsSecret(value, secret string) bool {
	return len(secret) > 0 && strings.Contains(value, secret)
}
