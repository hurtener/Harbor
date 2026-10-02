package bifrost

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/llm/allocation"
	"github.com/hurtener/Harbor/internal/state/drivers/inmem"
)

func TestAllocation_HiddenTransportRetriesRemainFunded(t *testing.T) {
	var hits atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(500)
		_, _ = w.Write([]byte(`{"error":{"message":"synthetic unavailable"}}`))
	}))
	defer server.Close()
	const env = "HARBOR_TEST_ALLOCATION_SYNTHETIC_KEY"
	t.Setenv(env, "test-only")
	cfg := llm.ConfigSnapshot{Driver: "bifrost", Provider: "allocation-local", Model: "model", DisableCorrections: true, DisableRetry: true, DisableDowngrade: true, DisableGovernance: true, ModelProfiles: map[string]llm.ModelProfile{"model": {ContextWindowTokens: 1000}}, CustomProviders: []llm.CustomProviderSpec{{Name: "allocation-local", BaseURL: server.URL + "/v1", APIKeyEnvVar: env, Models: []string{"model"}, MaxRetries: 1, Timeout: 5 * time.Second, RetryBackoffInitial: time.Millisecond, RetryBackoffMax: time.Millisecond}}}
	st, err := inmem.New(config.StateConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close(context.Background()) }()
	mgr := allocation.New(st)
	deps, closeDeps := makeCustomProviderTestDeps(t)
	defer closeDeps()
	deps.Allocations = mgr
	client, err := llm.Open(t.Context(), cfg, deps)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close(context.Background()) }()
	q := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}, RunID: "task"}
	ctx, _ := identity.WithRun(t.Context(), q.Identity, q.RunID)
	a := llm.InferenceAllocation{AllocationID: "f", Revision: 1, MaxTotalTokens: 3300}
	ctx = llm.WithInferenceAllocation(ctx, &a)
	max := 100
	req := llm.CompleteRequest{Model: "model", MaxTokens: &max, Messages: []llm.ChatMessage{{Role: llm.RoleUser, Content: llm.Content{Text: new("hello")}}}}
	if _, err = client.Complete(ctx, req); err == nil {
		t.Fatal("expected synthetic provider failure")
	}
	if hits.Load() != 2 {
		t.Fatalf("physical requests=%d want2", hits.Load())
	}
	snap, err := mgr.Snapshot(ctx, q, a)
	if err != nil || snap.ReservedTokens != 3300 || snap.UnknownTokens != 3300 {
		t.Fatalf("snapshot %+v %v", snap, err)
	}
	if _, err = client.Complete(ctx, req); !errors.Is(err, llm.ErrAllocationExhausted) {
		t.Fatalf("refusal %v", err)
	}
	if hits.Load() != 2 {
		t.Fatal("exhausted allocation reached transport")
	}
}
