package serve

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/llm"
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
	if !disabled.Supported || disabled.Configured || disabled.Mode != "disabled" || disabled.ReceiptTransportKind != "none" || disabled.ReceiptTransport != "disabled" || disabled.TopUpTransport != "unsupported" || disabled.StrictReady {
		t.Fatalf("disabled readiness=%+v", disabled)
	}

	provided := llm.ExternalGrantConfig{
		Mode:        llm.ExternalGrantRequired,
		RouteMode:   llm.ExternalGrantRouteRuntimeDefault,
		Verifier:    testGrantVerifier{},
		ReceiptSink: nil,
	}
	runtimeDefault := externalGrantReadinessProvider(config.LLMExternalGrantConfig{}, provided, testReceiptDelivery{}, nil)()
	if !runtimeDefault.Configured || runtimeDefault.Mode != "required" || len(runtimeDefault.AcceptedRouteModes) != 1 || runtimeDefault.AcceptedRouteModes[0] != "runtime_default" || len(runtimeDefault.ReadyRouteModes) != 1 || runtimeDefault.ReadyRouteModes[0] != "runtime_default" || runtimeDefault.ReceiptTransportKind != "host_injected_delivery" || !runtimeDefault.StrictReady || runtimeDefault.CredentialResolverWired {
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
