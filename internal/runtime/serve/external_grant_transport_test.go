package serve

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	llmreceipts "github.com/hurtener/Harbor/internal/llm/receipts"
	"github.com/hurtener/Harbor/internal/state"
	"github.com/hurtener/Harbor/internal/state/drivers/inmem"
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
		AuthTokenEnv: "HARBOR_COORDINATOR_TOKEN",
	}, &opts, func(string) (string, bool) { return testCoordinatorToken, true })
	if err != nil {
		t.Fatal(err)
	}
	readiness := externalGrantReadinessProvider(config.LLMExternalGrantConfig{}, opts.ExternalGrant, opts.ExternalGrantDelivery, stock)
	if initial := readiness(); !initial.StrictReady || initial.ReceiptTransport != "wired" {
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
