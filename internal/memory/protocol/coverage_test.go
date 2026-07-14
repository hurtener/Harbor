package protocol_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/memory"
	memprotocol "github.com/hurtener/Harbor/internal/memory/protocol"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
)

// TestList_NilStoreFailsLoud — a nil Store is a misconfiguration; List
// fails loudly rather than nil-panicking.
func TestList_NilStoreFailsLoud(t *testing.T) {
	_, err := memprotocol.List(context.Background(),
		memprotocol.ListDeps{Store: nil}, prototypes.MemoryListRequest{}, testIdentity())
	if err == nil {
		t.Fatal("List with nil Store: err = nil, want a misconfiguration error")
	}
}

// TestGet_NilStoreFailsLoud — same for Get.
func TestGet_NilStoreFailsLoud(t *testing.T) {
	h := newMemHarness(t, memory.StrategyNone, 0)
	_, err := memprotocol.Get(context.Background(),
		memprotocol.GetDeps{Store: nil, Artifacts: h.artifacts, HeavyThreshold: heavyThreshold},
		prototypes.MemoryGetRequest{Key: "k"}, testIdentity())
	if err == nil {
		t.Fatal("Get with nil Store: err = nil, want a misconfiguration error")
	}
	_, err = memprotocol.Get(context.Background(),
		memprotocol.GetDeps{Store: h.store, Artifacts: nil, HeavyThreshold: heavyThreshold},
		prototypes.MemoryGetRequest{Key: "k"}, testIdentity())
	if err == nil {
		t.Fatal("Get with nil Artifacts: err = nil, want a misconfiguration error")
	}
}

// TestHealth_NilStoreFailsLoud — same for Health.
func TestHealth_NilStoreFailsLoud(t *testing.T) {
	_, err := memprotocol.Health(context.Background(),
		memprotocol.HealthDeps{Store: nil}, testIdentity())
	if err == nil {
		t.Fatal("Health with nil Store: err = nil, want a misconfiguration error")
	}
}

// TestList_AgentIDFacetLoudRejects — the AgentIDs facet operates over an
// unpopulated producer identity (a V1 memory turn carries no agent), so
// naming it loud-rejects with ErrInvalidFilter rather than returning a
// silent empty page (D-313). Pins the agent_ids loud-reject branch.
func TestList_AgentIDFacetLoudRejects(t *testing.T) {
	h := newMemHarness(t, memory.StrategyTruncation, 100000)
	id := testIdentity()
	seedTurns(t, h, id, 3)

	_, err := memprotocol.List(context.Background(),
		memprotocol.ListDeps{Store: h.store, DriverName: "inmem"},
		prototypes.MemoryListRequest{Filter: prototypes.MemoryFilter{
			AgentIDs: []string{"agent-x"},
		}}, id)
	if !errors.Is(err, memprotocol.ErrInvalidFilter) {
		t.Fatalf("agent_ids filter: err = %v, want ErrInvalidFilter (loud-reject, never a false-empty page)", err)
	}
}

// TestGet_ValueCarriesTrajectoryDigest — a turn with a TrajectoryDigest
// round-trips the digest into the memory.get value (pins the
// trajectory-digest branch of turnValueBytes).
func TestGet_ValueCarriesTrajectoryDigest(t *testing.T) {
	h := newMemHarness(t, memory.StrategyTruncation, 100000)
	id := testIdentity()
	if err := h.store.AddTurn(context.Background(), id, memory.ConversationTurn{
		UserMessage:       "with digest",
		AssistantResponse: "answer",
		TrajectoryDigest: &memory.TrajectoryDigest{
			ToolsInvoked:        []string{"search", "fetch"},
			ObservationsSummary: "found two results",
			ReasoningSummary:    "narrowed the query",
			ArtifactsRefs:       []string{"art_1"},
		},
		ArtifactsHiddenRefs: []string{"hidden_1"},
	}); err != nil {
		t.Fatalf("AddTurn: %v", err)
	}

	listResp, err := memprotocol.List(context.Background(),
		memprotocol.ListDeps{Store: h.store, DriverName: "inmem"},
		prototypes.MemoryListRequest{}, id)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	getResp, err := memprotocol.Get(context.Background(),
		memprotocol.GetDeps{Store: h.store, Artifacts: h.artifacts, DriverName: "inmem", HeavyThreshold: heavyThreshold},
		prototypes.MemoryGetRequest{Key: listResp.Items[0].Key}, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !strings.Contains(string(getResp.Detail.Value), "trajectory_digest") {
		t.Error("Get value missing the trajectory_digest projection")
	}
	if !strings.Contains(string(getResp.Detail.Value), "artifacts_hidden_refs") {
		t.Error("Get value missing the artifacts_hidden_refs projection")
	}
}

// TestList_HonoursCtxCancellation — a cancelled ctx fails List loudly
// rather than returning a partial / empty result.
func TestList_HonoursCtxCancellation(t *testing.T) {
	h := newMemHarness(t, memory.StrategyNone, 0)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := memprotocol.List(ctx,
		memprotocol.ListDeps{Store: h.store, DriverName: "inmem"},
		prototypes.MemoryListRequest{}, testIdentity())
	if err == nil {
		t.Fatal("List with a cancelled ctx: err = nil, want a cancellation error")
	}
}

// TestHealth_HonoursCtxCancellation — same for Health.
func TestHealth_HonoursCtxCancellation(t *testing.T) {
	h := newMemHarness(t, memory.StrategyNone, 0)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := memprotocol.Health(ctx,
		memprotocol.HealthDeps{Store: h.store, DriverName: "inmem"},
		testIdentity())
	if err == nil {
		t.Fatal("Health with a cancelled ctx: err = nil, want a cancellation error")
	}
}

// TestGet_HonoursCtxCancellation — same for Get.
func TestGet_HonoursCtxCancellation(t *testing.T) {
	h := newMemHarness(t, memory.StrategyNone, 0)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := memprotocol.Get(ctx,
		memprotocol.GetDeps{Store: h.store, Artifacts: h.artifacts, DriverName: "inmem", HeavyThreshold: heavyThreshold},
		prototypes.MemoryGetRequest{Key: "mem_x"}, testIdentity())
	if err == nil {
		t.Fatal("Get with a cancelled ctx: err = nil, want a cancellation error")
	}
}

// TestHealth_DefaultDriverWhenUnset — Health with no DriverName +
// no DriverByScope reports the inmem default for the session scope.
func TestHealth_DefaultDriverWhenUnset(t *testing.T) {
	h := newMemHarness(t, memory.StrategyNone, 0)
	resp, err := memprotocol.Health(context.Background(),
		memprotocol.HealthDeps{Store: h.store}, testIdentity())
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if resp.Aggregate.DriverByScope[string(prototypes.MemoryScopeSession)] != string(prototypes.MemoryDriverInmem) {
		t.Errorf("DriverByScope[session] = %q, want inmem (default)",
			resp.Aggregate.DriverByScope[string(prototypes.MemoryScopeSession)])
	}
}

// TestList_UserAndTenantFacets — the UserIDs / TenantIDs facets narrow
// to the caller's own identity (the projected rows carry the caller's
// triple); a facet naming a foreign user / tenant matches no rows.
func TestList_UserAndTenantFacets(t *testing.T) {
	h := newMemHarness(t, memory.StrategyTruncation, 100000)
	id := testIdentity()
	seedTurns(t, h, id, 2)

	// Own user → all rows.
	resp, err := memprotocol.List(context.Background(),
		memprotocol.ListDeps{Store: h.store, DriverName: "inmem"},
		prototypes.MemoryListRequest{Filter: prototypes.MemoryFilter{
			UserIDs: []string{id.UserID}, TenantIDs: []string{id.TenantID},
		}}, id)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(resp.Items) != 2 {
		t.Errorf("own user/tenant facet returned %d, want 2", len(resp.Items))
	}

	// Foreign user → no rows.
	resp, err = memprotocol.List(context.Background(),
		memprotocol.ListDeps{Store: h.store, DriverName: "inmem"},
		prototypes.MemoryListRequest{Filter: prototypes.MemoryFilter{
			UserIDs: []string{"u-stranger"},
		}}, id)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(resp.Items) != 0 {
		t.Errorf("foreign-user facet returned %d, want 0", len(resp.Items))
	}
}
