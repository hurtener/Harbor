package serve

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	llmgrant "github.com/hurtener/Harbor/internal/llm/grant"
	"github.com/hurtener/Harbor/internal/llm/leases"
	llmreceipts "github.com/hurtener/Harbor/internal/llm/receipts"
	"github.com/hurtener/Harbor/internal/state"
	"github.com/hurtener/Harbor/internal/state/drivers/inmem"
	llmtopup "github.com/hurtener/Harbor/sdk/llm/topup"
)

const testCoordinatorToken = "fixture-coordinator-service-token"

type testReceiptDelivery struct{}

func (testReceiptDelivery) Deliver(context.Context, llm.AttemptUsageReceipt) error { return nil }

type testReceiptSink struct{}

func (testReceiptSink) Enqueue(context.Context, llm.AttemptUsageReceipt) error { return nil }

type testGrantVerifier struct{}

func (testGrantVerifier) Verify(context.Context, llm.ExternalGrant, llm.CompleteRequest) error {
	return nil
}

type testCredentialResolver struct{}

func (testCredentialResolver) Resolve(context.Context, llm.ExternalGrant) (llm.ResolvedCredential, error) {
	return llm.ResolvedCredential{}, nil
}

type testTopUpper struct{}

func (testTopUpper) TopUp(context.Context, llm.ExternalGrant, int64) (llm.ExternalGrant, error) {
	return llm.ExternalGrant{}, nil
}

type transportTestProvider struct{}

func (transportTestProvider) Complete(context.Context, llm.CompleteRequest) (llm.CompleteResponse, error) {
	return llm.CompleteResponse{Usage: llm.Usage{TotalTokens: 1}}, nil
}

func (transportTestProvider) Close(context.Context) error { return nil }

// transientDueLoadStore fails one post-startup due-index read. It models a
// StateStore interruption after serve has already advertised strict readiness.
type transientDueLoadStore struct {
	state.StateStore
	mu       sync.Mutex
	failures int
}

func (s *transientDueLoadStore) Load(ctx context.Context, q identity.Quadruple, kind string) (state.StateRecord, error) {
	if q == identity.InternalCoordinationQuadruple() && kind == state.InternalKindPrefix+"inference.receipt.due" {
		s.mu.Lock()
		if s.failures > 0 {
			s.failures--
			s.mu.Unlock()
			return state.StateRecord{}, errors.New("transient durable due-index failure")
		}
		s.mu.Unlock()
	}
	return s.StateStore.Load(ctx, q, kind)
}

func TestConfigureStockExternalGrant_DisabledDoesNoWork(t *testing.T) {
	lookups := 0
	opts := Options{}
	client, err := configureStockExternalGrant(config.ExternalGrantCoordinatorConfig{}, &opts, func(string) (string, bool) {
		lookups++
		return "", false
	})
	if err != nil || client != nil || lookups != 0 || opts.ExternalGrantDelivery != nil || opts.ExternalGrant.TopUpper != nil {
		t.Fatalf("disabled stock transport client=%v lookups=%d opts=%+v err=%v", client, lookups, opts, err)
	}
}

func TestConfigureStockExternalGrant_WiresReceiptOnly(t *testing.T) {
	cfg := config.ExternalGrantCoordinatorConfig{
		ReceiptURL:        "https://coordinator.example.test/v1/receipts",
		AuthTokenEnv:      "HARBOR_COORDINATOR_TOKEN",
		Timeout:           2 * time.Second,
		MaxBatch:          17,
		ReconcileInterval: 3 * time.Minute,
	}
	lookups := 0
	opts := Options{}
	client, err := configureStockExternalGrant(cfg, &opts, func(name string) (string, bool) {
		lookups++
		if name != cfg.AuthTokenEnv {
			t.Fatalf("env lookup=%q", name)
		}
		return testCoordinatorToken, true
	})
	if err != nil {
		t.Fatal(err)
	}
	if client == nil || opts.ExternalGrantDelivery != client || opts.ExternalGrant.TopUpper != nil {
		t.Fatalf("stock seams not wired: client=%v delivery=%T topup=%T", client, opts.ExternalGrantDelivery, opts.ExternalGrant.TopUpper)
	}
	if lookups != 1 || opts.ExternalGrantMaxBatch != 17 || opts.ExternalGrantReconcile != 3*time.Minute {
		t.Fatalf("lookups=%d batch=%d reconcile=%s", lookups, opts.ExternalGrantMaxBatch, opts.ExternalGrantReconcile)
	}
}

func TestConfigureStockExternalGrant_WiresOptionalTopUpWithoutIdleWork(t *testing.T) {
	cfg := config.ExternalGrantCoordinatorConfig{
		ReceiptURL:   "https://coordinator.example.test/v1/receipts",
		TopUpURL:     "https://coordinator.example.test/v1/grants/top-up",
		AuthTokenEnv: "HARBOR_COORDINATOR_TOKEN",
	}
	lookups := 0
	opts := Options{}
	client, err := configureStockExternalGrant(cfg, &opts, func(string) (string, bool) {
		lookups++
		return testCoordinatorToken, true
	})
	if err != nil {
		t.Fatal(err)
	}
	if client == nil || opts.ExternalGrantDelivery != client || opts.ExternalGrant.TopUpper != client || lookups != 1 {
		t.Fatalf("stock top-up wiring client=%v topup=%T lookups=%d", client, opts.ExternalGrant.TopUpper, lookups)
	}
	if ready := client.Readiness(); ready.Receipt != "wired" || ready.TopUp != "wired" {
		t.Fatalf("constructor readiness=%+v", ready)
	}
}

func TestStockTopUpTransport_AmpleValidLeaseMakesZeroCoordinatorCalls(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		calls.Add(1)
	}))
	defer server.Close()
	opts := Options{}
	stock, err := configureStockExternalGrant(config.ExternalGrantCoordinatorConfig{
		ReceiptURL: server.URL, TopUpURL: server.URL, AuthTokenEnv: "HARBOR_COORDINATOR_TOKEN",
	}, &opts, func(string) (string, bool) { return testCoordinatorToken, true })
	if err != nil {
		t.Fatal(err)
	}
	st, err := inmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close(context.Background()) }()
	durable, err := leases.New(st, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	grant := llm.ExternalGrant{
		Version: llm.ExternalGrantVersionLegacy, KeyID: "key-a", Audience: "harbor-runtime", GrantID: "grant-ample",
		RouteMode: llm.ExternalGrantRouteRuntimeDefault, OrganizationID: "org-a", RuntimeID: "runtime-a",
		TenantID: "tenant-a", UserID: "user-a", SessionID: "session-a", LogicalRunID: "run-a",
		LogicalCallID: "call-a", AttemptNonce: "nonce-a", PolicyGeneration: 1, MaxReasoning: llm.ReasoningLow, MaxOutputTokens: 10,
		Lease:    llm.ComputeLease{LeaseID: "lease-ample", Epoch: 1, TokenUnits: 100, ExpiresAt: now.Add(time.Minute)},
		IssuedAt: now, ExpiresAt: now.Add(time.Minute), Signature: "fixture-signature",
	}
	client := llmgrant.Wrap(transportTestProvider{}, llm.ConfigSnapshot{Provider: "mock", Model: "model-fast"}, llm.Deps{ExternalGrant: llm.ExternalGrantConfig{
		Mode: llm.ExternalGrantOptional, RouteMode: llm.ExternalGrantRouteRuntimeDefault,
		Verifier: testGrantVerifier{}, Reservations: durable, Successors: durable, SuccessorResolver: durable,
		TopUpper: stock, ReceiptSink: testReceiptSink{},
	}})
	ctx, err := identity.WithVerified(context.Background(), identity.Identity{TenantID: grant.TenantID, UserID: grant.UserID, SessionID: grant.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	ctx, err = identity.WithRun(ctx, identity.Identity{TenantID: grant.TenantID, UserID: grant.UserID, SessionID: grant.SessionID}, grant.LogicalRunID)
	if err != nil {
		t.Fatal(err)
	}
	ctx = llm.WithVerifiedOrganization(ctx, grant.OrganizationID)
	maxTokens := 10
	if _, err := client.Complete(ctx, llm.CompleteRequest{Model: "model-fast", MaxTokens: &maxTokens, ExternalGrant: &grant}); err != nil {
		t.Fatal(err)
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("ample valid lease made %d coordinator HTTP calls", got)
	}
}

func TestConfigureStockExternalGrant_FailsLoudWithoutCredentialAndOnConflicts(t *testing.T) {
	cfg := config.ExternalGrantCoordinatorConfig{ReceiptURL: "https://coordinator.example.test/receipts", AuthTokenEnv: "HARBOR_COORDINATOR_TOKEN"}
	if _, err := configureStockExternalGrant(cfg, &Options{}, func(string) (string, bool) { return "", false }); err == nil {
		t.Fatal("missing credential was accepted")
	} else if strings.Contains(err.Error(), testCoordinatorToken) || strings.Contains(err.Error(), cfg.ReceiptURL) {
		t.Fatalf("configuration error leaked transport material: %v", err)
	}
	opts := Options{ExternalGrantDelivery: testReceiptDelivery{}}
	if _, err := configureStockExternalGrant(cfg, &opts, func(string) (string, bool) { return testCoordinatorToken, true }); err == nil {
		t.Fatal("configured and injected receipt transports were both accepted")
	}
	opts = Options{ExternalGrant: llm.ExternalGrantConfig{ReceiptSink: testReceiptSink{}}}
	if _, err := configureStockExternalGrant(cfg, &opts, func(string) (string, bool) { return testCoordinatorToken, true }); err == nil {
		t.Fatal("configured transport and injected receipt sink were both accepted")
	}
	topUpCfg := cfg
	topUpCfg.TopUpURL = "https://coordinator.example.test/grants/top-up"
	opts = Options{ExternalGrant: llm.ExternalGrantConfig{TopUpper: testTopUpper{}}}
	if _, err := configureStockExternalGrant(topUpCfg, &opts, func(string) (string, bool) { return testCoordinatorToken, true }); err == nil {
		t.Fatal("configured and injected top-up transports were both accepted")
	}
}

func TestExternalGrantReadiness_ReportsModeRoutesAndConcreteWiring(t *testing.T) {
	disabled := externalGrantReadinessProvider(config.LLMExternalGrantConfig{}, llm.ExternalGrantConfig{}, nil, nil)()
	if !disabled.Supported || disabled.Configured || disabled.Mode != "disabled" || disabled.ReceiptTransportKind != "none" || disabled.ReceiptTransport != "disabled" || disabled.TopUpTransport != "unsupported" || disabled.StrictReady || disabled.AgentBinding != "required_v2" || len(disabled.SupportedGrantVersions) != 2 || disabled.SupportedGrantVersions[0] != llm.ExternalGrantVersionLegacy || disabled.SupportedGrantVersions[1] != llm.ExternalGrantVersionAgentBound {
		t.Fatalf("disabled readiness=%+v", disabled)
	}

	provided := llm.ExternalGrantConfig{
		Mode:        llm.ExternalGrantRequired,
		RouteMode:   llm.ExternalGrantRouteRuntimeDefault,
		Verifier:    testGrantVerifier{},
		ReceiptSink: nil,
	}
	runtimeDefault := externalGrantReadinessProvider(config.LLMExternalGrantConfig{}, provided, testReceiptDelivery{}, nil)()
	if !runtimeDefault.Configured || runtimeDefault.Mode != "required" || len(runtimeDefault.AcceptedRouteModes) != 1 || runtimeDefault.AcceptedRouteModes[0] != "runtime_default" || len(runtimeDefault.ReadyRouteModes) != 1 || runtimeDefault.ReadyRouteModes[0] != "runtime_default" || runtimeDefault.ReceiptTransportKind != "host_injected_delivery" || !runtimeDefault.StrictReady || runtimeDefault.CredentialResolverWired || runtimeDefault.AgentBinding != "required_v2" {
		t.Fatalf("runtime-default readiness=%+v", runtimeDefault)
	}

	provided.RouteMode = llm.ExternalGrantRouteCoordinatorBound
	coordinatorBound := externalGrantReadinessProvider(config.LLMExternalGrantConfig{}, provided, testReceiptDelivery{}, nil)()
	if coordinatorBound.StrictReady {
		t.Fatalf("coordinator-bound route reported strict-ready without credential resolver: %+v", coordinatorBound)
	}
	provided.Credentials = testCredentialResolver{}
	provided.TopUpper = testTopUpper{}
	coordinatorBound = externalGrantReadinessProvider(config.LLMExternalGrantConfig{}, provided, testReceiptDelivery{}, nil)()
	if !coordinatorBound.StrictReady || len(coordinatorBound.ReadyRouteModes) != 1 || coordinatorBound.ReadyRouteModes[0] != "coordinator_bound" || coordinatorBound.TopUpTransport != "host_injected" {
		t.Fatalf("fully wired coordinator-bound route not ready: %+v", coordinatorBound)
	}
}

func TestExternalGrantReadiness_StockOutboxDegradesAndRecovers(t *testing.T) {
	opts := Options{ExternalGrant: llm.ExternalGrantConfig{
		Mode:      llm.ExternalGrantRequired,
		RouteMode: llm.ExternalGrantRouteRuntimeDefault,
		Verifier:  testGrantVerifier{},
	}}
	stock, err := configureStockExternalGrant(config.ExternalGrantCoordinatorConfig{
		ReceiptURL:   "https://coordinator.example.test/v1/receipts",
		TopUpURL:     "https://coordinator.example.test/v1/grants/top-up",
		AuthTokenEnv: "HARBOR_COORDINATOR_TOKEN",
	}, &opts, func(string) (string, bool) { return testCoordinatorToken, true })
	if err != nil {
		t.Fatal(err)
	}
	readiness := externalGrantReadinessProvider(config.LLMExternalGrantConfig{}, opts.ExternalGrant, opts.ExternalGrantDelivery, stock)
	if initial := readiness(); !initial.StrictReady || initial.ReceiptTransport != "wired" || initial.TopUpTransport != "stock_authenticated_http" {
		t.Fatalf("initial readiness=%+v", initial)
	}
	stock.SetOutboxHealth(false)
	if degraded := readiness(); degraded.StrictReady || degraded.ReceiptTransport != "degraded" {
		t.Fatalf("degraded readiness=%+v", degraded)
	}
	stock.SetOutboxHealth(true)
	if recovered := readiness(); !recovered.StrictReady || recovered.ReceiptTransport != "wired" {
		t.Fatalf("recovered readiness=%+v", recovered)
	}
}

func TestExternalGrantReadiness_StockTopUpFailureDegradesAndSuccessRecovers(t *testing.T) {
	var fail atomic.Bool
	fail.Store(true)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request: %v", err)
			return
		}
		request, err := llmtopup.ParseCanonicalRequest(body)
		if err != nil {
			t.Errorf("parse request: %v", err)
			return
		}
		successor := request.Predecessor
		successor.KeyID, successor.Signature = "key-b", "signature-b"
		successor.Lease.Epoch++
		successor.Lease.TokenUnits += request.RequestedUnits
		successor.IssuedAt = successor.IssuedAt.Add(time.Second)
		successor.ExpiresAt = successor.ExpiresAt.Add(time.Second)
		successor.Lease.ExpiresAt = successor.Lease.ExpiresAt.Add(time.Second)
		response, err := llmtopup.NewResponse(request, successor)
		if err != nil {
			t.Errorf("response: %v", err)
			return
		}
		payload, err := llmtopup.MarshalCanonicalResponse(request, response)
		if err != nil {
			t.Errorf("marshal response: %v", err)
			return
		}
		w.Header().Set("Idempotency-Key", request.IdempotencyKey)
		_, _ = w.Write(payload)
	}))
	defer server.Close()

	opts := Options{ExternalGrant: llm.ExternalGrantConfig{
		Mode: llm.ExternalGrantRequired, RouteMode: llm.ExternalGrantRouteRuntimeDefault, Verifier: testGrantVerifier{},
	}}
	stock, err := configureStockExternalGrant(config.ExternalGrantCoordinatorConfig{
		ReceiptURL: server.URL, TopUpURL: server.URL, AuthTokenEnv: "HARBOR_COORDINATOR_TOKEN",
	}, &opts, func(string) (string, bool) { return testCoordinatorToken, true })
	if err != nil {
		t.Fatal(err)
	}
	readiness := externalGrantReadinessProvider(config.LLMExternalGrantConfig{}, opts.ExternalGrant, opts.ExternalGrantDelivery, stock)
	if initial := readiness(); !initial.StrictReady || initial.TopUpState != "wired" {
		t.Fatalf("initial readiness=%+v", initial)
	}
	issued := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	grant := llm.ExternalGrant{
		Version: llm.ExternalGrantVersionAgentBound, KeyID: "key-a", Audience: "harbor-runtime", GrantID: "grant-top-up",
		RouteMode: llm.ExternalGrantRouteRuntimeDefault, OrganizationID: "org-a", RuntimeID: "runtime-a", AgentID: "agent-a",
		TenantID: "tenant-a", UserID: "user-a", SessionID: "session-a", LogicalRunID: "run-a", LogicalCallID: "call-a", AttemptNonce: "nonce-a",
		PolicyGeneration: 1, MaxReasoning: llm.ReasoningMedium, MaxOutputTokens: 100,
		Lease:    llm.ComputeLease{LeaseID: "lease-a", Epoch: 1, TokenUnits: 200, ExpiresAt: issued.Add(10 * time.Minute)},
		IssuedAt: issued, ExpiresAt: issued.Add(5 * time.Minute), Signature: "signature-a",
	}
	if _, err := stock.Renew(context.Background(), grant, 100, llm.ExternalGrantRenewalLeaseInsufficient); err == nil {
		t.Fatal("failed top-up unexpectedly succeeded")
	}
	if degraded := readiness(); degraded.StrictReady || degraded.TopUpState != "degraded" {
		t.Fatalf("degraded readiness=%+v", degraded)
	}
	fail.Store(false)
	if _, err := stock.Renew(context.Background(), grant, 100, llm.ExternalGrantRenewalLeaseInsufficient); err != nil {
		t.Fatalf("recovery top-up: %v", err)
	}
	if recovered := readiness(); !recovered.StrictReady || recovered.TopUpState != "wired" {
		t.Fatalf("recovered readiness=%+v", recovered)
	}
}

func TestExternalGrantReadiness_StockOutboxReplayFailureDegradesThenRecovers(t *testing.T) {
	base, err := inmem.New(config.StateConfig{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = base.Close(context.Background()) })
	store := &transientDueLoadStore{StateStore: base, failures: 1}
	opts := Options{ExternalGrant: llm.ExternalGrantConfig{
		Mode:      llm.ExternalGrantRequired,
		RouteMode: llm.ExternalGrantRouteRuntimeDefault,
		Verifier:  testGrantVerifier{},
	}}
	stock, err := configureStockExternalGrant(config.ExternalGrantCoordinatorConfig{
		ReceiptURL:   "https://coordinator.example.test/v1/receipts",
		AuthTokenEnv: "HARBOR_COORDINATOR_TOKEN",
	}, &opts, func(string) (string, bool) { return testCoordinatorToken, true })
	if err != nil {
		t.Fatal(err)
	}
	outbox, err := llmreceipts.New(llmreceipts.Config{
		Store: store, Delivery: stock, BaseBackoff: 5 * time.Millisecond,
		MaxBackoff: 5 * time.Millisecond, ReconcileInterval: time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := outbox.Prepare(context.Background()); err != nil {
		t.Fatal(err)
	}
	readiness := externalGrantReadinessProvider(config.LLMExternalGrantConfig{}, opts.ExternalGrant, opts.ExternalGrantDelivery, stock)
	if got := readiness(); !got.StrictReady || got.ReceiptTransport != "wired" {
		t.Fatalf("startup readiness=%+v", got)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- outbox.Run(ctx) }()
	deadline := time.Now().Add(2 * time.Second)
	for {
		got := readiness()
		if !got.StrictReady && got.ReceiptTransport == "degraded" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("replay failure did not degrade runtime readiness: %+v", got)
		}
		time.Sleep(time.Millisecond)
	}
	for {
		got := readiness()
		if got.StrictReady && got.ReceiptTransport == "wired" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("recovered replay did not restore runtime readiness: %+v", got)
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("outbox Run=%v, want context cancellation", err)
	}
}
