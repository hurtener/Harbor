package grant

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"path/filepath"
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
	statesqlite "github.com/hurtener/Harbor/internal/state/drivers/sqlite"
	"github.com/hurtener/Harbor/internal/tools"
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

type recordingReservations struct {
	inner                   *leases.Store
	mu                      sync.Mutex
	requests                []llm.LeaseReservationRequest
	settlementContextErrors []error
}

func (r *recordingReservations) Reserve(ctx context.Context, req llm.LeaseReservationRequest) (llm.LeaseReservation, error) {
	r.mu.Lock()
	r.requests = append(r.requests, req)
	r.mu.Unlock()
	return r.inner.Reserve(ctx, req)
}

func (r *recordingReservations) Settle(ctx context.Context, settlement llm.LeaseSettlement) error {
	r.mu.Lock()
	r.settlementContextErrors = append(r.settlementContextErrors, ctx.Err())
	r.mu.Unlock()
	return r.inner.Settle(ctx, settlement)
}

type completeFunc func(context.Context, llm.CompleteRequest) (llm.CompleteResponse, error)

func (f completeFunc) Complete(ctx context.Context, req llm.CompleteRequest) (llm.CompleteResponse, error) {
	return f(ctx, req)
}

func (completeFunc) Close(context.Context) error { return nil }

type contextRecordingSink struct {
	mu            sync.Mutex
	receipts      []llm.AttemptUsageReceipt
	contextErrors []error
}

func (s *contextRecordingSink) Enqueue(ctx context.Context, receipt llm.AttemptUsageReceipt) error {
	s.mu.Lock()
	s.receipts = append(s.receipts, receipt)
	s.contextErrors = append(s.contextErrors, ctx.Err())
	s.mu.Unlock()
	return nil
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

type successorApplierFunc func(context.Context, llm.ExternalGrant, llm.ExternalGrant) error

type signalingSuccessorApplier struct {
	store        *leases.Store
	smallApplied chan struct{}
	once         sync.Once
	threshold    int64
}

func (a *signalingSuccessorApplier) ApplySuccessor(ctx context.Context, predecessor, successor llm.ExternalGrant) error {
	err := a.store.ApplySuccessor(ctx, predecessor, successor)
	if err == nil && successor.Lease.TokenUnits-predecessor.Lease.TokenUnits <= a.threshold {
		a.once.Do(func() { close(a.smallApplied) })
	}
	return err
}

func (a *signalingSuccessorApplier) ResolveSuccessor(ctx context.Context, root llm.ExternalGrant) (llm.ExternalGrant, bool, error) {
	return a.store.ResolveSuccessor(ctx, root)
}

type verifierFunc func(context.Context, llm.ExternalGrant, llm.CompleteRequest) error

type renewalVerifierFunc func(context.Context, llm.ExternalGrant, llm.CompleteRequest) error

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

type successorResolverFunc func(context.Context, llm.ExternalGrant) (llm.ExternalGrant, bool, error)

func (f successorResolverFunc) ResolveSuccessor(ctx context.Context, root llm.ExternalGrant) (llm.ExternalGrant, bool, error) {
	return f(ctx, root)
}

type reasonedTopUpperFunc func(context.Context, llm.ExternalGrant, int64, llm.ExternalGrantRenewalReason) (llm.ExternalGrant, error)

func (f reasonedTopUpperFunc) TopUp(ctx context.Context, grant llm.ExternalGrant, units int64) (llm.ExternalGrant, error) {
	reason := llm.ExternalGrantRenewalExpired
	if grant.Lease.RemainingTokens() < units {
		reason = llm.ExternalGrantRenewalLeaseInsufficient
	}
	return f(ctx, grant, units, reason)
}

func (f reasonedTopUpperFunc) Renew(ctx context.Context, grant llm.ExternalGrant, units int64, reason llm.ExternalGrantRenewalReason) (llm.ExternalGrant, error) {
	return f(ctx, grant, units, reason)
}

func newTestBinding(t *testing.T) *BindingStore {
	t.Helper()
	bindings := NewBindingStore()
	if err := bindings.Put(Binding{Handle: "binding-a", OrganizationID: "org-a", RuntimeID: "runtime-1", Provider: "openai", ProviderConnectionID: "connection-a", ProviderConnectionGeneration: 1, Generation: 1, Secret: "credential-a"}); err != nil {
		t.Fatal(err)
	}
	return bindings
}

func (f topUpperFunc) TopUp(ctx context.Context, grant llm.ExternalGrant, needed int64) (llm.ExternalGrant, error) {
	return f(ctx, grant, needed)
}

func (f successorApplierFunc) ApplySuccessor(ctx context.Context, predecessor, successor llm.ExternalGrant) error {
	return f(ctx, predecessor, successor)
}

func (f verifierFunc) Verify(ctx context.Context, grant llm.ExternalGrant, req llm.CompleteRequest) error {
	return f(ctx, grant, req)
}

func (f renewalVerifierFunc) VerifyRenewalPredecessor(ctx context.Context, grant llm.ExternalGrant, req llm.CompleteRequest) error {
	return f(ctx, grant, req)
}

func signLegacyBlankRoute(t *testing.T, signer *Signer, grant llm.ExternalGrant) llm.ExternalGrant {
	t.Helper()
	signed, err := signer.Sign(grant)
	if err != nil {
		t.Fatal(err)
	}
	signed.RouteMode = ""
	document, err := canonicalDocument(signed)
	if err != nil {
		t.Fatal(err)
	}
	signed.Signature = base64.RawURLEncoding.EncodeToString(ed25519.Sign(signer.private, document))
	return signed
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

func TestWrap_ReservesCanonicalTotalCallBoundForPromptHeavySuccess(t *testing.T) {
	signer, err := NewSigner("key-total", "harbor-runtime", nil, testClock)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := NewVerifier(VerifierConfig{Audience: "harbor-runtime", RuntimeID: "runtime-1", Keys: map[string]ed25519.PublicKey{"key-total": signer.PublicKey()}, Clock: testClock})
	if err != nil {
		t.Fatal(err)
	}
	binding := NewBindingStore()
	if err := binding.Put(Binding{Handle: "binding-a", OrganizationID: "org-a", RuntimeID: "runtime-1", Provider: "openai", ProviderConnectionID: "connection-a", ProviderConnectionGeneration: 1, Generation: 1, Secret: "credential-a"}); err != nil {
		t.Fatal(err)
	}
	grant := testGrant()
	grant.Lease.LeaseID = "lease-prompt-heavy"
	signed, err := signer.Sign(grant)
	if err != nil {
		t.Fatal(err)
	}
	reservations := &recordingReservations{inner: newTestReservations(t)}
	sink := &recordingSink{}
	inner := &recordingClient{response: llm.CompleteResponse{Usage: llm.Usage{PromptTokens: 520, CompletionTokens: 980, TotalTokens: 1500}}}
	client := Wrap(inner, llm.ConfigSnapshot{}, llm.Deps{ExternalGrant: llm.ExternalGrantConfig{
		Mode: llm.ExternalGrantRequired, Verifier: verifier, Credentials: binding,
		Reservations: reservations, ReceiptSink: sink, ReceiptRequired: true,
	}})
	textContent := strings.Repeat("abcdefgh", 250)
	req := llm.CompleteRequest{Model: "model-fast", Messages: []llm.ChatMessage{{Role: llm.RoleUser, Content: llm.Content{Text: &textContent}}}, ExternalGrant: &signed}
	if _, err := client.Complete(testContext(t, "org-a"), req); err != nil {
		t.Fatalf("prompt-heavy Complete: %v", err)
	}
	want := int64(llm.EstimateRequestTokens(req, llm.ModelProfile{}) + grant.MaxOutputTokens)
	reservations.mu.Lock()
	defer reservations.mu.Unlock()
	if len(reservations.requests) != 1 || reservations.requests[0].Units != want {
		t.Fatalf("reservation = %+v, want total-call units %d", reservations.requests, want)
	}
	if reservations.requests[0].Units <= int64(grant.MaxOutputTokens) {
		t.Fatalf("reservation omitted prompt estimate: %+v", reservations.requests[0])
	}
}

func TestWrap_PersistsCanceledProviderUsageAfterCallerCancellation(t *testing.T) {
	signer, err := NewSigner("key-cancel", "harbor-runtime", nil, testClock)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := NewVerifier(VerifierConfig{Audience: "harbor-runtime", RuntimeID: "runtime-1", Keys: map[string]ed25519.PublicKey{"key-cancel": signer.PublicKey()}, Clock: testClock})
	if err != nil {
		t.Fatal(err)
	}
	binding := NewBindingStore()
	if err := binding.Put(Binding{Handle: "binding-a", OrganizationID: "org-a", RuntimeID: "runtime-1", Provider: "openai", ProviderConnectionID: "connection-a", ProviderConnectionGeneration: 1, Generation: 1, Secret: "credential-a"}); err != nil {
		t.Fatal(err)
	}
	grant := testGrant()
	grant.Lease.LeaseID = "lease-canceled-usage"
	signed, err := signer.Sign(grant)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(testContext(t, "org-a"))
	reservations := &recordingReservations{inner: newTestReservations(t)}
	sink := &contextRecordingSink{}
	provider := completeFunc(func(context.Context, llm.CompleteRequest) (llm.CompleteResponse, error) {
		cancel()
		return llm.CompleteResponse{Usage: llm.Usage{PromptTokens: 3, CompletionTokens: 2, TotalTokens: 5}}, context.Canceled
	})
	client := Wrap(provider, llm.ConfigSnapshot{}, llm.Deps{ExternalGrant: llm.ExternalGrantConfig{
		Mode: llm.ExternalGrantRequired, Verifier: verifier, Credentials: binding,
		Reservations: reservations, ReceiptSink: sink, ReceiptRequired: true,
	}})
	if _, err := client.Complete(ctx, llm.CompleteRequest{Model: "model-fast", ExternalGrant: &signed}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Complete = %v, want context.Canceled", err)
	}
	reservations.mu.Lock()
	settleErrors := append([]error(nil), reservations.settlementContextErrors...)
	reservations.mu.Unlock()
	sink.mu.Lock()
	receipts := append([]llm.AttemptUsageReceipt(nil), sink.receipts...)
	sinkErrors := append([]error(nil), sink.contextErrors...)
	sink.mu.Unlock()
	if len(settleErrors) != 1 || settleErrors[0] != nil || len(sinkErrors) != 1 || sinkErrors[0] != nil {
		t.Fatalf("terminal contexts: settle=%v sink=%v", settleErrors, sinkErrors)
	}
	if len(receipts) != 1 || receipts[0].Status != "canceled" || receipts[0].TotalTokens != 5 {
		t.Fatalf("canceled receipt = %+v", receipts)
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

func TestWrap_V2AgentBindingRefusesBeforeProviderSideEffects(t *testing.T) {
	signer, err := NewSigner("key-v2", "harbor-runtime", nil, testClock)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := NewVerifier(VerifierConfig{Audience: "harbor-runtime", RuntimeID: "runtime-1", Keys: map[string]ed25519.PublicKey{"key-v2": signer.PublicKey()}, Clock: testClock})
	if err != nil {
		t.Fatal(err)
	}
	claims := testGrant()
	claims.Version, claims.AgentID = llm.ExternalGrantVersionAgentBound, "agent-a"
	signed, err := signer.Sign(claims)
	if err != nil {
		t.Fatal(err)
	}
	for name, ctx := range map[string]context.Context{
		"missing":  testContext(t, "org-a"),
		"mismatch": tools.WithEffectiveAgentConfig(testContext(t, "org-a"), "agent-b"),
	} {
		t.Run(name, func(t *testing.T) {
			inner := &recordingClient{}
			client := Wrap(inner, llm.ConfigSnapshot{}, llm.Deps{ExternalGrant: llm.ExternalGrantConfig{
				Mode: llm.ExternalGrantRequired, Verifier: verifier, Reservations: newTestReservations(t), ReceiptSink: &recordingSink{}, ReceiptRequired: true,
			}})
			_, err := client.Complete(ctx, llm.CompleteRequest{Model: "model-fast", ExternalGrant: &signed})
			if !errors.Is(err, llm.ErrExternalGrantInvalid) {
				t.Fatalf("Complete = %v, want ErrExternalGrantInvalid", err)
			}
			if len(inner.requests) != 0 {
				t.Fatalf("provider calls=%d, want 0", len(inner.requests))
			}
		})
	}
}

func TestWrap_TopsUpBoundedLeaseBeforeProviderCall(t *testing.T) {
	predecessorSigner, err := NewSigner("key-1", "harbor-runtime", nil, testClock)
	if err != nil {
		t.Fatal(err)
	}
	successorSigner, err := NewSigner("key-2", "harbor-runtime", nil, testClock)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := NewVerifier(VerifierConfig{Audience: "harbor-runtime", RuntimeID: "runtime-1", Keys: map[string]ed25519.PublicKey{
		"key-1": predecessorSigner.PublicKey(),
		"key-2": successorSigner.PublicKey(),
	}, Clock: testClock})
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
	signed, err := predecessorSigner.Sign(grant)
	if err != nil {
		t.Fatal(err)
	}
	var requested int64
	topUpper := topUpperFunc(func(_ context.Context, old llm.ExternalGrant, needed int64) (llm.ExternalGrant, error) {
		requested = needed
		old.Lease.Epoch++
		old.Lease.TokenUnits = 110
		old.IssuedAt = old.IssuedAt.Add(30 * time.Second)
		old.ExpiresAt = old.ExpiresAt.Add(30 * time.Second)
		old.Lease.ExpiresAt = old.Lease.ExpiresAt.Add(30 * time.Second)
		return successorSigner.Sign(old)
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

func TestWrap_ExpiredGrantRenewsDeadlinesWithoutWideningCapacityAndAppliesBeforeProvider(t *testing.T) {
	now := testClock()
	predecessorSigner, err := NewSigner("key-expired", "harbor-runtime", nil, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	successorNow := now.Add(20 * time.Minute)
	successorSigner, err := NewSigner("key-renewed", "harbor-runtime", nil, func() time.Time { return successorNow })
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := NewVerifier(VerifierConfig{Audience: "harbor-runtime", RuntimeID: "runtime-1", Keys: map[string]ed25519.PublicKey{
		"key-expired": predecessorSigner.PublicKey(), "key-renewed": successorSigner.PublicKey(),
	}, Clock: func() time.Time { return successorNow }})
	if err != nil {
		t.Fatal(err)
	}
	claims := testGrant()
	claims.RouteMode = llm.ExternalGrantRouteRuntimeDefault
	claims.Provider, claims.ProviderModelID, claims.ProviderConnectionID, claims.RouteID, claims.CredentialBindingHandle = "", "", "", "", ""
	claims.ProviderConnectionGeneration, claims.CredentialAssetGeneration = 0, 0
	claims.Lease.TokenUnits = 500
	claims.IssuedAt = now.Add(-20 * time.Minute)
	claims.ExpiresAt = now.Add(-15 * time.Minute)
	claims.Lease.ExpiresAt = now.Add(-10 * time.Minute)
	predecessor, err := predecessorSigner.Sign(claims)
	if err != nil {
		t.Fatal(err)
	}
	successor := predecessor
	successor.Lease.Epoch++
	successor.IssuedAt = successorNow
	successor.ExpiresAt = successorNow.Add(5 * time.Minute)
	successor.Lease.ExpiresAt = successorNow.Add(10 * time.Minute)
	successor, err = successorSigner.Sign(successor)
	if err != nil {
		t.Fatal(err)
	}

	var topUpCalls, applyCalls atomic.Int32
	applied := make(chan struct{}, 1)
	durable := newTestReservations(t)
	inner := &recordingClient{response: llm.CompleteResponse{Usage: llm.Usage{TotalTokens: 4}}}
	client := Wrap(completeFunc(func(ctx context.Context, req llm.CompleteRequest) (llm.CompleteResponse, error) {
		select {
		case <-applied:
		default:
			t.Fatal("provider called before durable successor application")
		}
		return inner.Complete(ctx, req)
	}), llm.ConfigSnapshot{Provider: "openai", Model: "model-fast"}, llm.Deps{ExternalGrant: llm.ExternalGrantConfig{
		Mode: llm.ExternalGrantOptional, Verifier: verifier, RouteMode: llm.ExternalGrantRouteRuntimeDefault,
		TopUpper: topUpperFunc(func(context.Context, llm.ExternalGrant, int64) (llm.ExternalGrant, error) {
			topUpCalls.Add(1)
			return successor, nil
		}),
		Reservations: durable,
		Successors: successorApplierFunc(func(ctx context.Context, old, next llm.ExternalGrant) error {
			applyCalls.Add(1)
			if old.Lease.TokenUnits != next.Lease.TokenUnits || old.Lease.ConsumedUnits != next.Lease.ConsumedUnits {
				t.Fatal("expiry-only renewal widened compute")
			}
			if err := durable.ApplySuccessor(ctx, old, next); err != nil {
				return err
			}
			applied <- struct{}{}
			return nil
		}),
	}})
	maxTokens := 100
	if _, err := client.Complete(testContext(t, "org-a"), llm.CompleteRequest{Model: "model-fast", MaxTokens: &maxTokens, ExternalGrant: &predecessor}); err != nil {
		t.Fatalf("expired renewal Complete: %v", err)
	}
	if topUpCalls.Load() != 1 || applyCalls.Load() != 1 || len(inner.requests) != 1 {
		t.Fatalf("topup=%d apply=%d provider=%d", topUpCalls.Load(), applyCalls.Load(), len(inner.requests))
	}
}

func TestWrap_ImmutableRootReusesDurableSuccessorThenRenewsActualExhaustion(t *testing.T) {
	now := testClock()
	signer, err := NewSigner("key-multicall", "harbor-runtime", nil, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := NewVerifier(VerifierConfig{
		Audience: "harbor-runtime", RuntimeID: "runtime-1",
		Keys: map[string]ed25519.PublicKey{"key-multicall": signer.PublicKey()}, Clock: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	claims := testGrant()
	claims.RouteMode = llm.ExternalGrantRouteRuntimeDefault
	claims.Provider, claims.ProviderModelID, claims.ProviderConnectionID, claims.RouteID, claims.CredentialBindingHandle = "", "", "", "", ""
	claims.ProviderConnectionGeneration, claims.CredentialAssetGeneration = 0, 0
	claims.Lease.TokenUnits, claims.Lease.ConsumedUnits = 5, 0
	root, err := signer.Sign(claims)
	if err != nil {
		t.Fatal(err)
	}
	durable := newTestReservations(t)
	var renewalCalls atomic.Int32
	var mu sync.Mutex
	var reasons []llm.ExternalGrantRenewalReason
	topUpper := reasonedTopUpperFunc(func(_ context.Context, current llm.ExternalGrant, needed int64, reason llm.ExternalGrantRenewalReason) (llm.ExternalGrant, error) {
		renewalCalls.Add(1)
		mu.Lock()
		reasons = append(reasons, reason)
		mu.Unlock()
		current.Lease.Epoch++
		if reason == llm.ExternalGrantRenewalLeaseInsufficient {
			current.Lease.TokenUnits += needed
		}
		current.IssuedAt = current.IssuedAt.Add(time.Second)
		current.ExpiresAt = current.ExpiresAt.Add(time.Second)
		current.Lease.ExpiresAt = current.Lease.ExpiresAt.Add(time.Second)
		return signer.Sign(current)
	})
	inner := &recordingClient{response: llm.CompleteResponse{Usage: llm.Usage{TotalTokens: 4}}}
	client := Wrap(inner, llm.ConfigSnapshot{Provider: "openai", Model: "model-fast"}, llm.Deps{ExternalGrant: llm.ExternalGrantConfig{
		Mode: llm.ExternalGrantOptional, RouteMode: llm.ExternalGrantRouteRuntimeDefault,
		Verifier: verifier, RenewalVerifier: verifier, Reservations: durable,
		Successors: durable, SuccessorResolver: durable, TopUpper: topUpper,
	}})
	maxTokens := 10
	for step := 1; step <= 3; step++ {
		ctx := llm.WithAttemptStep(testContext(t, "org-a"), step)
		if _, err := client.Complete(ctx, llm.CompleteRequest{Model: "model-fast", MaxTokens: &maxTokens, ExternalGrant: &root}); err != nil {
			t.Fatalf("step %d: %v", step, err)
		}
		if step == 2 && renewalCalls.Load() != 1 {
			t.Fatalf("step 2 did not reuse durable successor: renewal calls=%d", renewalCalls.Load())
		}
	}
	if renewalCalls.Load() != 2 || len(inner.requests) != 3 {
		t.Fatalf("renewal calls=%d provider calls=%d", renewalCalls.Load(), len(inner.requests))
	}
	mu.Lock()
	defer mu.Unlock()
	if len(reasons) != 2 || reasons[0] != llm.ExternalGrantRenewalLeaseInsufficient || reasons[1] != llm.ExternalGrantRenewalLeaseInsufficient {
		t.Fatalf("renewal reasons=%v", reasons)
	}
}

func TestWrap_ImmutableRootResolvesCurrentSuccessorAfterSQLiteRestart(t *testing.T) {
	now := testClock()
	signer, err := NewSigner("key-restart", "harbor-runtime", nil, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := NewVerifier(VerifierConfig{
		Audience: "harbor-runtime", RuntimeID: "runtime-1",
		Keys: map[string]ed25519.PublicKey{"key-restart": signer.PublicKey()}, Clock: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	claims := testGrant()
	claims.RouteMode = llm.ExternalGrantRouteRuntimeDefault
	claims.Provider, claims.ProviderModelID, claims.ProviderConnectionID, claims.RouteID, claims.CredentialBindingHandle = "", "", "", "", ""
	claims.ProviderConnectionGeneration, claims.CredentialAssetGeneration = 0, 0
	claims.Lease.TokenUnits, claims.Lease.ConsumedUnits = 5, 0
	root, err := signer.Sign(claims)
	if err != nil {
		t.Fatal(err)
	}
	var renewals atomic.Int32
	topUpper := reasonedTopUpperFunc(func(_ context.Context, current llm.ExternalGrant, needed int64, reason llm.ExternalGrantRenewalReason) (llm.ExternalGrant, error) {
		renewals.Add(1)
		current.Lease.Epoch++
		if reason == llm.ExternalGrantRenewalLeaseInsufficient {
			current.Lease.TokenUnits += needed
		}
		current.IssuedAt = current.IssuedAt.Add(time.Second)
		current.ExpiresAt = current.ExpiresAt.Add(time.Second)
		current.Lease.ExpiresAt = current.Lease.ExpiresAt.Add(time.Second)
		return signer.Sign(current)
	})
	dsn := filepath.Join(t.TempDir(), "complete-restart.sqlite")
	open := func() (*leases.Store, interface{ Close(context.Context) error }) {
		st, openErr := statesqlite.New(config.StateConfig{Driver: "sqlite", DSN: dsn})
		if openErr != nil {
			t.Fatal(openErr)
		}
		store, openErr := leases.New(st, func() time.Time { return now })
		if openErr != nil {
			t.Fatal(openErr)
		}
		return store, st
	}
	complete := func(store *leases.Store, step int) {
		inner := &recordingClient{response: llm.CompleteResponse{Usage: llm.Usage{TotalTokens: 4}}}
		client := Wrap(inner, llm.ConfigSnapshot{Provider: "openai", Model: "model-fast"}, llm.Deps{ExternalGrant: llm.ExternalGrantConfig{
			Mode: llm.ExternalGrantOptional, RouteMode: llm.ExternalGrantRouteRuntimeDefault,
			Verifier: verifier, RenewalVerifier: verifier, Reservations: store,
			Successors: store, SuccessorResolver: store, TopUpper: topUpper,
		}})
		maxTokens := 10
		ctx := llm.WithAttemptStep(testContext(t, "org-a"), step)
		if _, completeErr := client.Complete(ctx, llm.CompleteRequest{Model: "model-fast", MaxTokens: &maxTokens, ExternalGrant: &root}); completeErr != nil {
			t.Fatalf("step %d after restart boundary: %v", step, completeErr)
		}
		if len(inner.requests) != 1 {
			t.Fatalf("step %d provider calls=%d", step, len(inner.requests))
		}
	}

	store, st := open()
	complete(store, 1)
	if renewals.Load() != 1 {
		t.Fatalf("initial renewals=%d", renewals.Load())
	}
	if err := st.Close(context.Background()); err != nil {
		t.Fatal(err)
	}

	store, st = open()
	defer func() { _ = st.Close(context.Background()) }()
	complete(store, 2)
	if renewals.Load() != 1 {
		t.Fatalf("restart did not reuse current successor: renewals=%d", renewals.Load())
	}
	complete(store, 3)
	if renewals.Load() != 2 {
		t.Fatalf("actual durable exhaustion did not renew current successor: renewals=%d", renewals.Load())
	}
}

func TestWrap_MixedRequestedUnitRenewalsReloadWinnerBeforeProvider(t *testing.T) {
	now := testClock()
	signer, err := NewSigner("key-mixed", "harbor-runtime", nil, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := NewVerifier(VerifierConfig{Audience: "harbor-runtime", RuntimeID: "runtime-1", Keys: map[string]ed25519.PublicKey{"key-mixed": signer.PublicKey()}, Clock: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	claims := testGrant()
	claims.RouteMode = llm.ExternalGrantRouteRuntimeDefault
	claims.Provider, claims.ProviderModelID, claims.ProviderConnectionID, claims.RouteID, claims.CredentialBindingHandle = "", "", "", "", ""
	claims.ProviderConnectionGeneration, claims.CredentialAssetGeneration = 0, 0
	claims.Lease.TokenUnits, claims.Lease.ConsumedUnits = 1, 0
	root, err := signer.Sign(claims)
	if err != nil {
		t.Fatal(err)
	}
	durable := newTestReservations(t)
	applier := &signalingSuccessorApplier{store: durable, smallApplied: make(chan struct{}), threshold: 2}
	var entered atomic.Int32
	firstTwoEntered := make(chan struct{})
	var enteredOnce sync.Once
	var renewalCalls atomic.Int32
	topUpper := reasonedTopUpperFunc(func(ctx context.Context, current llm.ExternalGrant, needed int64, reason llm.ExternalGrantRenewalReason) (llm.ExternalGrant, error) {
		call := entered.Add(1)
		if call == 2 {
			enteredOnce.Do(func() { close(firstTwoEntered) })
		}
		if call <= 2 {
			select {
			case <-firstTwoEntered:
			case <-ctx.Done():
				return llm.ExternalGrant{}, ctx.Err()
			}
		}
		renewalCalls.Add(1)
		if needed > 2 && current.Lease.Epoch == root.Lease.Epoch {
			select {
			case <-applier.smallApplied:
			case <-ctx.Done():
				return llm.ExternalGrant{}, ctx.Err()
			}
		}
		current.Lease.Epoch++
		if reason == llm.ExternalGrantRenewalLeaseInsufficient {
			current.Lease.TokenUnits += needed
		}
		current.IssuedAt = current.IssuedAt.Add(time.Second)
		current.ExpiresAt = current.ExpiresAt.Add(time.Second)
		current.Lease.ExpiresAt = current.Lease.ExpiresAt.Add(time.Second)
		return signer.Sign(current)
	})
	inner := &recordingClient{response: llm.CompleteResponse{Usage: llm.Usage{TotalTokens: 1}}}
	client := Wrap(inner, llm.ConfigSnapshot{Provider: "openai", Model: "model-fast"}, llm.Deps{ExternalGrant: llm.ExternalGrantConfig{
		Mode: llm.ExternalGrantOptional, RouteMode: llm.ExternalGrantRouteRuntimeDefault,
		Verifier: verifier, RenewalVerifier: verifier, Reservations: durable,
		Successors: applier, SuccessorResolver: applier, TopUpper: topUpper,
	}})
	errCh := make(chan error, 2)
	contexts := []context.Context{testContext(t, "org-a"), testContext(t, "org-a")}
	for i, units := range []int{2, 10} {
		step := i + 1
		units := units
		baseCtx := contexts[i]
		go func() {
			_, completeErr := client.Complete(llm.WithAttemptStep(baseCtx, step), llm.CompleteRequest{Model: "model-fast", MaxTokens: &units, ExternalGrant: &root})
			errCh <- completeErr
		}()
	}
	for range 2 {
		if err := <-errCh; err != nil {
			t.Fatalf("mixed requested-unit Complete: %v", err)
		}
	}
	if got := len(inner.requests); got != 2 {
		t.Fatalf("provider calls=%d, want 2", got)
	}
	if got := renewalCalls.Load(); got != 3 {
		t.Fatalf("renewal calls=%d, want stale loser plus current retry (3)", got)
	}
}

func TestWrap_ResolvedSuccessorStillRequiresStrictSignatureBeforeProvider(t *testing.T) {
	signer, err := NewSigner("key-resolved", "harbor-runtime", nil, testClock)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := NewVerifier(VerifierConfig{Audience: "harbor-runtime", RuntimeID: "runtime-1", Keys: map[string]ed25519.PublicKey{"key-resolved": signer.PublicKey()}, Clock: testClock})
	if err != nil {
		t.Fatal(err)
	}
	rootClaims := testGrant()
	rootClaims.RouteMode = llm.ExternalGrantRouteRuntimeDefault
	rootClaims.Provider, rootClaims.ProviderModelID, rootClaims.ProviderConnectionID, rootClaims.RouteID, rootClaims.CredentialBindingHandle = "", "", "", "", ""
	rootClaims.ProviderConnectionGeneration, rootClaims.CredentialAssetGeneration = 0, 0
	rootClaims.Lease.Epoch = 1
	root, err := signer.Sign(rootClaims)
	if err != nil {
		t.Fatal(err)
	}
	resolved := root
	resolved.Lease.Epoch++
	resolved.IssuedAt, resolved.ExpiresAt, resolved.Lease.ExpiresAt = resolved.IssuedAt.Add(time.Second), resolved.ExpiresAt.Add(time.Second), resolved.Lease.ExpiresAt.Add(time.Second)
	resolved, err = signer.Sign(resolved)
	if err != nil {
		t.Fatal(err)
	}
	resolved.Signature = "corrupt-persisted-signature"
	inner := &recordingClient{}
	client := Wrap(inner, llm.ConfigSnapshot{Provider: "openai", Model: "model-fast"}, llm.Deps{ExternalGrant: llm.ExternalGrantConfig{
		Mode: llm.ExternalGrantOptional, RouteMode: llm.ExternalGrantRouteRuntimeDefault, Verifier: verifier,
		SuccessorResolver: successorResolverFunc(func(context.Context, llm.ExternalGrant) (llm.ExternalGrant, bool, error) { return resolved, true, nil }),
	}})
	n := 1
	if _, err := client.Complete(testContext(t, "org-a"), llm.CompleteRequest{Model: "model-fast", MaxTokens: &n, ExternalGrant: &root}); !errors.Is(err, llm.ErrExternalGrantSignature) {
		t.Fatalf("corrupt resolved signature=%v", err)
	}
	if len(inner.requests) != 0 {
		t.Fatalf("provider calls=%d", len(inner.requests))
	}
}

func TestWrap_ExpiredInvalidOrAgentMismatchedPredecessorMakesZeroRenewalCalls(t *testing.T) {
	now := testClock()
	signer, err := NewSigner("key-expired-v2", "harbor-runtime", nil, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	verifyNow := now.Add(20 * time.Minute)
	verifier, err := NewVerifier(VerifierConfig{Audience: "harbor-runtime", RuntimeID: "runtime-1", Keys: map[string]ed25519.PublicKey{"key-expired-v2": signer.PublicKey()}, Clock: func() time.Time { return verifyNow }})
	if err != nil {
		t.Fatal(err)
	}
	claims := testGrant()
	claims.Version, claims.AgentID = llm.ExternalGrantVersionAgentBound, "agent-a"
	claims.RouteMode = llm.ExternalGrantRouteRuntimeDefault
	claims.Provider, claims.ProviderModelID, claims.ProviderConnectionID, claims.RouteID, claims.CredentialBindingHandle = "", "", "", "", ""
	claims.ProviderConnectionGeneration, claims.CredentialAssetGeneration = 0, 0
	claims.IssuedAt, claims.ExpiresAt, claims.Lease.ExpiresAt = now.Add(-20*time.Minute), now.Add(-15*time.Minute), now.Add(-10*time.Minute)
	expired, err := signer.Sign(claims)
	if err != nil {
		t.Fatal(err)
	}
	badSignature := expired
	badSignature.Signature = "bad"
	for name, tc := range map[string]struct {
		grant llm.ExternalGrant
		agent string
	}{
		"signature": {grant: badSignature, agent: "agent-a"},
		"agent":     {grant: expired, agent: "agent-b"},
	} {
		t.Run(name, func(t *testing.T) {
			var calls atomic.Int32
			client := Wrap(&recordingClient{}, llm.ConfigSnapshot{Provider: "openai", Model: "model-fast"}, llm.Deps{ExternalGrant: llm.ExternalGrantConfig{
				Mode: llm.ExternalGrantOptional, Verifier: verifier, RouteMode: llm.ExternalGrantRouteRuntimeDefault,
				TopUpper: topUpperFunc(func(context.Context, llm.ExternalGrant, int64) (llm.ExternalGrant, error) {
					calls.Add(1)
					return llm.ExternalGrant{}, nil
				}),
				Successors: successorApplierFunc(func(context.Context, llm.ExternalGrant, llm.ExternalGrant) error { return nil }),
			}})
			n := 1
			if _, err := client.Complete(agentBoundTestContext(t, "org-a", tc.agent), llm.CompleteRequest{Model: "model-fast", MaxTokens: &n, ExternalGrant: &tc.grant}); err == nil {
				t.Fatal("unsafe expired predecessor accepted")
			}
			if calls.Load() != 0 {
				t.Fatalf("renewal calls=%d, want 0", calls.Load())
			}
		})
	}
}

func TestWrap_ExpiredRevokedCredentialMakesZeroRenewalCalls(t *testing.T) {
	now := testClock()
	signer, err := NewSigner("key-expired-revoked", "harbor-runtime", nil, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	verifyNow := now.Add(20 * time.Minute)
	verifier, err := NewVerifier(VerifierConfig{
		Audience: "harbor-runtime", RuntimeID: "runtime-1",
		Keys:  map[string]ed25519.PublicKey{"key-expired-revoked": signer.PublicKey()},
		Clock: func() time.Time { return verifyNow },
	})
	if err != nil {
		t.Fatal(err)
	}
	claims := testGrant()
	claims.IssuedAt, claims.ExpiresAt, claims.Lease.ExpiresAt = now.Add(-20*time.Minute), now.Add(-15*time.Minute), now.Add(-10*time.Minute)
	expired, err := signer.Sign(claims)
	if err != nil {
		t.Fatal(err)
	}
	binding := newTestBinding(t)
	if err := binding.Revoke(claims.CredentialBindingHandle); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	inner := &recordingClient{}
	client := Wrap(inner, llm.ConfigSnapshot{}, llm.Deps{ExternalGrant: llm.ExternalGrantConfig{
		Mode: llm.ExternalGrantOptional, Verifier: verifier, Credentials: binding,
		TopUpper: topUpperFunc(func(context.Context, llm.ExternalGrant, int64) (llm.ExternalGrant, error) {
			calls.Add(1)
			return llm.ExternalGrant{}, nil
		}),
		Successors: successorApplierFunc(func(context.Context, llm.ExternalGrant, llm.ExternalGrant) error { return nil }),
	}})
	maxTokens := 1
	if _, err := client.Complete(testContext(t, "org-a"), llm.CompleteRequest{Model: "model-fast", MaxTokens: &maxTokens, ExternalGrant: &expired}); !errors.Is(err, llm.ErrExternalGrantRevoked) {
		t.Fatalf("revoked renewal=%v, want ErrExternalGrantRevoked", err)
	}
	if calls.Load() != 0 || len(inner.requests) != 0 {
		t.Fatalf("renewal calls=%d provider calls=%d, want zero", calls.Load(), len(inner.requests))
	}
}

func TestWrap_OmittedMaxTokensTopUpUsesGrantedOutputCeiling(t *testing.T) {
	predecessorSigner, err := NewSigner("key-1", "harbor-runtime", nil, testClock)
	if err != nil {
		t.Fatal(err)
	}
	successorSigner, err := NewSigner("key-2", "harbor-runtime", nil, testClock)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := NewVerifier(VerifierConfig{Audience: "harbor-runtime", RuntimeID: "runtime-1", Keys: map[string]ed25519.PublicKey{
		"key-1": predecessorSigner.PublicKey(),
		"key-2": successorSigner.PublicKey(),
	}, Clock: testClock})
	if err != nil {
		t.Fatal(err)
	}
	binding := NewBindingStore()
	if err := binding.Put(Binding{Handle: "binding-a", OrganizationID: "org-a", RuntimeID: "runtime-1", Provider: "openai", ProviderConnectionID: "connection-a", ProviderConnectionGeneration: 1, Generation: 1, Secret: "credential-a"}); err != nil {
		t.Fatal(err)
	}
	grant := testGrant()
	grant.Lease.TokenUnits = 10
	grant.Lease.LeaseID = "lease-implicit-max"
	signed, err := predecessorSigner.Sign(grant)
	if err != nil {
		t.Fatal(err)
	}
	var requested int64
	topUpper := topUpperFunc(func(_ context.Context, old llm.ExternalGrant, needed int64) (llm.ExternalGrant, error) {
		requested = needed
		old.Lease.Epoch++
		old.Lease.TokenUnits += needed
		old.IssuedAt = old.IssuedAt.Add(30 * time.Second)
		old.ExpiresAt = old.ExpiresAt.Add(30 * time.Second)
		old.Lease.ExpiresAt = old.Lease.ExpiresAt.Add(30 * time.Second)
		return successorSigner.Sign(old)
	})
	inner := &recordingClient{response: llm.CompleteResponse{Usage: llm.Usage{TotalTokens: 4}}}
	leaseAwareVerifier := verifierFunc(func(ctx context.Context, candidate llm.ExternalGrant, req llm.CompleteRequest) error {
		if err := verifier.Verify(ctx, candidate, req); err != nil {
			return err
		}
		if candidate.Lease.RemainingTokens() < int64(candidate.MaxOutputTokens) {
			return llm.ErrExternalGrantLeaseInsufficient
		}
		return nil
	})
	client := Wrap(inner, llm.ConfigSnapshot{}, llm.Deps{ExternalGrant: llm.ExternalGrantConfig{
		Mode: llm.ExternalGrantRequired, Verifier: leaseAwareVerifier, RenewalVerifier: verifier, Credentials: binding,
		Reservations: newTestReservations(t), TopUpper: topUpper, ReceiptSink: &recordingSink{}, ReceiptRequired: true,
	}})
	if _, err := client.Complete(testContext(t, "org-a"), llm.CompleteRequest{Model: "model-fast", ExternalGrant: &signed}); err != nil {
		t.Fatalf("Complete with implicit output limit top-up: %v", err)
	}
	if requested != int64(grant.MaxOutputTokens) || len(inner.requests) != 1 || inner.requests[0].MaxTokens == nil || *inner.requests[0].MaxTokens != grant.MaxOutputTokens {
		t.Fatalf("top-up requested=%d requests=%+v", requested, inner.requests)
	}
}

func TestWrap_TopUpPreservesLegacyBlankRouteBeforeProviderCall(t *testing.T) {
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
	grant.Lease.LeaseID = "lease-legacy-blank"
	signed := signLegacyBlankRoute(t, signer, grant)
	topUpper := topUpperFunc(func(_ context.Context, old llm.ExternalGrant, _ int64) (llm.ExternalGrant, error) {
		old.Lease.Epoch++
		old.Lease.TokenUnits = 110
		old.IssuedAt = old.IssuedAt.Add(30 * time.Second)
		old.ExpiresAt = old.ExpiresAt.Add(30 * time.Second)
		old.Lease.ExpiresAt = old.Lease.ExpiresAt.Add(30 * time.Second)
		return signLegacyBlankRoute(t, signer, old), nil
	})
	inner := &recordingClient{response: llm.CompleteResponse{Usage: llm.Usage{TotalTokens: 4}}}
	client := Wrap(inner, llm.ConfigSnapshot{}, llm.Deps{ExternalGrant: llm.ExternalGrantConfig{
		Mode: llm.ExternalGrantRequired, Verifier: verifier, Credentials: binding,
		Reservations: newTestReservations(t), TopUpper: topUpper, ReceiptSink: &recordingSink{}, ReceiptRequired: true,
	}})
	maxTokens := 100
	if _, err := client.Complete(testContext(t, "org-a"), llm.CompleteRequest{Model: "model-fast", MaxTokens: &maxTokens, ExternalGrant: &signed}); err != nil {
		t.Fatalf("Complete with legacy blank route top-up: %v", err)
	}
	if len(inner.requests) != 1 || inner.requests[0].ExternalGrant == nil || inner.requests[0].ExternalGrant.RouteMode != "" {
		t.Fatalf("provider requests=%+v, want one preserved legacy blank route", inner.requests)
	}

	// A top-up service that re-signs through the modern defaulting boundary
	// changes blank to explicit coordinator_bound. Even though the resulting
	// grant has a valid signature, the relationship validator must reject that
	// raw signed-authority change before another provider call.
	normalizedInner := &recordingClient{}
	normalizedClient := Wrap(normalizedInner, llm.ConfigSnapshot{}, llm.Deps{ExternalGrant: llm.ExternalGrantConfig{
		Mode: llm.ExternalGrantOptional, Verifier: verifier, Credentials: binding,
		Successors: successorApplierFunc(func(context.Context, llm.ExternalGrant, llm.ExternalGrant) error { return nil }),
		TopUpper: topUpperFunc(func(_ context.Context, old llm.ExternalGrant, _ int64) (llm.ExternalGrant, error) {
			old.Lease.Epoch++
			old.Lease.TokenUnits = 110
			old.IssuedAt = old.IssuedAt.Add(30 * time.Second)
			old.ExpiresAt = old.ExpiresAt.Add(30 * time.Second)
			old.Lease.ExpiresAt = old.Lease.ExpiresAt.Add(30 * time.Second)
			return signer.Sign(old)
		}),
	}})
	_, err = normalizedClient.Complete(testContext(t, "org-a"), llm.CompleteRequest{Model: "model-fast", MaxTokens: &maxTokens, ExternalGrant: &signed})
	if !errors.Is(err, llm.ErrExternalGrantInvalid) {
		t.Fatalf("normalized legacy route Complete = %v, want ErrExternalGrantInvalid", err)
	}
	if len(normalizedInner.requests) != 0 {
		t.Fatalf("normalized legacy route provider calls=%d, want 0", len(normalizedInner.requests))
	}
}

func TestWrap_TopUpRejectsUntrustedOrInvalidRotatingSignatureBeforeProviderCall(t *testing.T) {
	predecessorSigner, err := NewSigner("key-1", "harbor-runtime", nil, testClock)
	if err != nil {
		t.Fatal(err)
	}
	trustedSuccessorSigner, err := NewSigner("key-2", "harbor-runtime", nil, testClock)
	if err != nil {
		t.Fatal(err)
	}
	unknownSigner, err := NewSigner("unknown-key", "harbor-runtime", nil, testClock)
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := NewVerifier(VerifierConfig{Audience: "harbor-runtime", RuntimeID: "runtime-1", Keys: map[string]ed25519.PublicKey{
		"key-1": predecessorSigner.PublicKey(),
		"key-2": trustedSuccessorSigner.PublicKey(),
	}, Clock: testClock})
	if err != nil {
		t.Fatal(err)
	}
	grant := testGrant()
	grant.Lease.TokenUnits = 10
	grant.Lease.LeaseID = "lease-signature-refusal"
	predecessor, err := predecessorSigner.Sign(grant)
	if err != nil {
		t.Fatal(err)
	}

	tests := map[string]func(llm.ExternalGrant) llm.ExternalGrant{
		"unknown rotating key": func(successor llm.ExternalGrant) llm.ExternalGrant {
			signed, signErr := unknownSigner.Sign(successor)
			if signErr != nil {
				t.Fatal(signErr)
			}
			return signed
		},
		"invalid rotating signature": func(successor llm.ExternalGrant) llm.ExternalGrant {
			signed, signErr := trustedSuccessorSigner.Sign(successor)
			if signErr != nil {
				t.Fatal(signErr)
			}
			signed.Signature = "invalid-signature"
			return signed
		},
	}
	for name, sign := range tests {
		t.Run(name, func(t *testing.T) {
			successor := predecessor
			successor.Lease.Epoch++
			successor.Lease.TokenUnits = 110
			successor.IssuedAt = successor.IssuedAt.Add(30 * time.Second)
			successor.ExpiresAt = successor.ExpiresAt.Add(30 * time.Second)
			successor.Lease.ExpiresAt = successor.Lease.ExpiresAt.Add(30 * time.Second)
			successor = sign(successor)
			if err := llm.ValidateExternalGrantTopUpSuccessor(predecessor, successor, 100); err != nil {
				t.Fatalf("relationship should be valid before signature verification: %v", err)
			}

			inner := &recordingClient{}
			client := Wrap(inner, llm.ConfigSnapshot{}, llm.Deps{ExternalGrant: llm.ExternalGrantConfig{
				Mode: llm.ExternalGrantOptional, Verifier: verifier, Credentials: newTestBinding(t),
				Successors: successorApplierFunc(func(context.Context, llm.ExternalGrant, llm.ExternalGrant) error { return nil }),
				TopUpper: topUpperFunc(func(context.Context, llm.ExternalGrant, int64) (llm.ExternalGrant, error) {
					return successor, nil
				}),
			}})
			maxTokens := 100
			_, completeErr := client.Complete(testContext(t, "org-a"), llm.CompleteRequest{Model: "model-fast", MaxTokens: &maxTokens, ExternalGrant: &predecessor})
			if !errors.Is(completeErr, llm.ErrExternalGrantSignature) {
				t.Fatalf("Complete = %v, want ErrExternalGrantSignature", completeErr)
			}
			if len(inner.requests) != 0 {
				t.Fatalf("provider calls=%d, want 0", len(inner.requests))
			}
		})
	}
}

func TestWrap_TopUpRejectsEveryImmutableSuccessorDriftBeforeProviderCall(t *testing.T) {
	signer, err := NewSigner("key-1", "harbor-runtime", nil, testClock)
	if err != nil {
		t.Fatal(err)
	}
	original := testGrant()
	original.Lease.TokenUnits = 10
	original.Lease.LeaseID = "lease-topup"
	original, err = signer.Sign(original)
	if err != nil {
		t.Fatal(err)
	}

	mutations := map[string]func(*llm.ExternalGrant){
		"version":                        func(g *llm.ExternalGrant) { g.Version++ },
		"audience":                       func(g *llm.ExternalGrant) { g.Audience = "other-audience" },
		"grant id":                       func(g *llm.ExternalGrant) { g.GrantID = "other-grant" },
		"route mode":                     func(g *llm.ExternalGrant) { g.RouteMode = llm.ExternalGrantRouteRuntimeDefault },
		"organization":                   func(g *llm.ExternalGrant) { g.OrganizationID = "other-org" },
		"runtime":                        func(g *llm.ExternalGrant) { g.RuntimeID = "other-runtime" },
		"tenant":                         func(g *llm.ExternalGrant) { g.TenantID = "other-tenant" },
		"user":                           func(g *llm.ExternalGrant) { g.UserID = "other-user" },
		"session":                        func(g *llm.ExternalGrant) { g.SessionID = "other-session" },
		"logical run":                    func(g *llm.ExternalGrant) { g.LogicalRunID = "other-run" },
		"logical call":                   func(g *llm.ExternalGrant) { g.LogicalCallID = "other-call" },
		"attempt nonce":                  func(g *llm.ExternalGrant) { g.AttemptNonce = "other-nonce" },
		"provider":                       func(g *llm.ExternalGrant) { g.Provider = "other-provider" },
		"provider model":                 func(g *llm.ExternalGrant) { g.ProviderModelID = "other-model" },
		"provider connection":            func(g *llm.ExternalGrant) { g.ProviderConnectionID = "other-connection" },
		"provider connection generation": func(g *llm.ExternalGrant) { g.ProviderConnectionGeneration++ },
		"route":                          func(g *llm.ExternalGrant) { g.RouteID = "other-route" },
		"credential binding":             func(g *llm.ExternalGrant) { g.CredentialBindingHandle = "other-binding" },
		"credential asset generation":    func(g *llm.ExternalGrant) { g.CredentialAssetGeneration++ },
		"policy generation":              func(g *llm.ExternalGrant) { g.PolicyGeneration++ },
		"reasoning ceiling":              func(g *llm.ExternalGrant) { g.MaxReasoning = llm.ReasoningHigh },
		"output ceiling":                 func(g *llm.ExternalGrant) { g.MaxOutputTokens++ },
		"lease id":                       func(g *llm.ExternalGrant) { g.Lease.LeaseID = "other-lease" },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			successor := original
			successor.Lease.Epoch++
			successor.Lease.TokenUnits = 110
			successor.IssuedAt = successor.IssuedAt.Add(30 * time.Second)
			successor.ExpiresAt = successor.ExpiresAt.Add(30 * time.Second)
			successor.Lease.ExpiresAt = successor.Lease.ExpiresAt.Add(30 * time.Second)
			successor.Signature = "successor-signature"
			mutate(&successor)

			verifierCalls := 0
			verifier := verifierFunc(func(context.Context, llm.ExternalGrant, llm.CompleteRequest) error {
				verifierCalls++
				if verifierCalls == 1 {
					return llm.ErrExternalGrantLeaseInsufficient
				}
				return nil
			})
			inner := &recordingClient{}
			client := Wrap(inner, llm.ConfigSnapshot{}, llm.Deps{ExternalGrant: llm.ExternalGrantConfig{
				Mode: llm.ExternalGrantOptional, Verifier: verifier, Credentials: newTestBinding(t),
				RenewalVerifier: renewalVerifierFunc(func(context.Context, llm.ExternalGrant, llm.CompleteRequest) error { return nil }),
				Successors:      successorApplierFunc(func(context.Context, llm.ExternalGrant, llm.ExternalGrant) error { return nil }),
				TopUpper: topUpperFunc(func(context.Context, llm.ExternalGrant, int64) (llm.ExternalGrant, error) {
					return successor, nil
				}),
			}})
			maxTokens := 100
			_, err := client.Complete(testContext(t, "org-a"), llm.CompleteRequest{
				Model: "model-fast", MaxTokens: &maxTokens, ExternalGrant: &original,
			})
			if !errors.Is(err, llm.ErrExternalGrantInvalid) {
				t.Fatalf("Complete = %v, want ErrExternalGrantInvalid", err)
			}
			if verifierCalls != 1 || len(inner.requests) != 0 {
				t.Fatalf("verifier calls=%d provider calls=%d, want 1/0", verifierCalls, len(inner.requests))
			}
		})
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
