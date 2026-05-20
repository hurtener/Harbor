package protocol_test

import (
	"context"
	"errors"
	"testing"

	"github.com/hurtener/Harbor/internal/artifacts"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/memory"
	memprotocol "github.com/hurtener/Harbor/internal/memory/protocol"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
)

// heavyThreshold is the test heavy-content threshold — small so a
// modest seeded turn crosses it without a megabyte of padding.
const heavyThreshold = 4096

func TestGet_LightValueInlined(t *testing.T) {
	h := newMemHarness(t, memory.StrategyTruncation, 100000)
	id := testIdentity()
	seedTurns(t, h, id, 2)

	listResp, err := memprotocol.List(context.Background(),
		memprotocol.ListDeps{Store: h.store, DriverName: "inmem"},
		prototypes.MemoryListRequest{}, id)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	key := listResp.Items[0].Key

	resp, err := memprotocol.Get(context.Background(),
		memprotocol.GetDeps{Store: h.store, Artifacts: h.artifacts, DriverName: "inmem", HeavyThreshold: heavyThreshold},
		prototypes.MemoryGetRequest{Key: key}, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(resp.Detail.Value) == 0 {
		t.Error("Get(light value): Value is empty, want inline bytes")
	}
	if resp.Detail.ValueArtifact != nil {
		t.Error("Get(light value): ValueArtifact populated, want nil — exactly one of Value/ValueArtifact (D-026)")
	}
	if resp.Detail.Item.HeavyContent {
		t.Error("Get(light value): HeavyContent = true, want false")
	}
	if resp.Detail.Item.Key != key {
		t.Errorf("Get: detail key = %q, want %q", resp.Detail.Item.Key, key)
	}
}

func TestGet_HeavyValueRoutesToArtifact(t *testing.T) {
	h := newMemHarness(t, memory.StrategyTruncation, 100000)
	id := testIdentity()
	seedHeavyTurn(t, h, id, heavyThreshold*2)

	listResp, err := memprotocol.List(context.Background(),
		memprotocol.ListDeps{Store: h.store, DriverName: "inmem"},
		prototypes.MemoryListRequest{}, id)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	key := listResp.Items[0].Key

	resp, err := memprotocol.Get(context.Background(),
		memprotocol.GetDeps{Store: h.store, Artifacts: h.artifacts, DriverName: "inmem", HeavyThreshold: heavyThreshold},
		prototypes.MemoryGetRequest{Key: key}, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if resp.Detail.ValueArtifact == nil {
		t.Fatal("Get(heavy value): ValueArtifact is nil, want by-reference stub (D-026)")
	}
	if len(resp.Detail.Value) != 0 {
		t.Error("Get(heavy value): Value populated, want empty — heavy value MUST NOT inline (D-026)")
	}
	if !resp.Detail.Item.HeavyContent {
		t.Error("Get(heavy value): HeavyContent = false, want true")
	}
	if resp.Detail.ValueArtifact.ID == "" {
		t.Error("Get(heavy value): ValueArtifact.ID is empty")
	}

	// The stub MUST resolve through the artifact store — round-trip
	// the bytes.
	scope := artifacts.ArtifactScope{TenantID: id.TenantID, UserID: id.UserID, SessionID: id.SessionID}
	got, found, err := h.artifacts.Get(context.Background(), scope, resp.Detail.ValueArtifact.ID)
	if err != nil {
		t.Fatalf("artifacts.Get: %v", err)
	}
	if !found {
		t.Fatal("artifacts.Get: ValueArtifact ref does not resolve in the artifact store")
	}
	if len(got) < heavyThreshold {
		t.Errorf("artifacts.Get: round-tripped %d bytes, want >= %d", len(got), heavyThreshold)
	}
}

func TestGet_FailsLoudlyOnIncompleteIdentity(t *testing.T) {
	h := newMemHarness(t, memory.StrategyNone, 0)
	_, err := memprotocol.Get(context.Background(),
		memprotocol.GetDeps{Store: h.store, Artifacts: h.artifacts, DriverName: "inmem", HeavyThreshold: heavyThreshold},
		prototypes.MemoryGetRequest{Key: "mem_whatever"},
		identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u"}})
	if !errors.Is(err, memory.ErrIdentityRequired) {
		t.Fatalf("Get with incomplete identity: err = %v, want ErrIdentityRequired", err)
	}
}

func TestGet_UnknownKeyIsNotFound(t *testing.T) {
	h := newMemHarness(t, memory.StrategyTruncation, 100000)
	id := testIdentity()
	seedTurns(t, h, id, 1)

	_, err := memprotocol.Get(context.Background(),
		memprotocol.GetDeps{Store: h.store, Artifacts: h.artifacts, DriverName: "inmem", HeavyThreshold: heavyThreshold},
		prototypes.MemoryGetRequest{Key: "mem_does_not_exist"}, id)
	if !errors.Is(err, memory.ErrNotFound) {
		t.Fatalf("Get with unknown key: err = %v, want ErrNotFound", err)
	}
}

func TestGet_EmptyKeyIsInvalid(t *testing.T) {
	h := newMemHarness(t, memory.StrategyNone, 0)
	_, err := memprotocol.Get(context.Background(),
		memprotocol.GetDeps{Store: h.store, Artifacts: h.artifacts, DriverName: "inmem", HeavyThreshold: heavyThreshold},
		prototypes.MemoryGetRequest{Key: ""}, testIdentity())
	if !errors.Is(err, memprotocol.ErrInvalidFilter) {
		t.Fatalf("Get with empty key: err = %v, want ErrInvalidFilter", err)
	}
}

func TestGet_NonPositiveThresholdFailsLoud(t *testing.T) {
	h := newMemHarness(t, memory.StrategyNone, 0)
	_, err := memprotocol.Get(context.Background(),
		memprotocol.GetDeps{Store: h.store, Artifacts: h.artifacts, DriverName: "inmem", HeavyThreshold: 0},
		prototypes.MemoryGetRequest{Key: "mem_x"}, testIdentity())
	if err == nil {
		t.Fatal("Get with zero HeavyThreshold: err = nil, want a misconfiguration error (a zero threshold would route every value)")
	}
}

// The D-026 leak negative test — buildDetail must fail loudly with
// ErrContextLeak when heavy bytes reach the inline path — lives in the
// same-package leak_internal_test.go (it needs the internal test seam
// BuildDetailLeakProbe to construct the mis-classified row that the
// public Get path cannot produce).
