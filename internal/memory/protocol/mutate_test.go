package protocol_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/memory"
	memprotocol "github.com/hurtener/Harbor/internal/memory/protocol"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
)

// A generous token budget so the strategy keeps every seeded turn verbatim
// in its recent window (no truncation away), which keeps the snapshot turn
// keys stable for the delete round-trip.
const traceBudget = 1 << 20

func TestStrategyTrace_ProjectsLiveStrategyState(t *testing.T) {
	h := newMemHarness(t, memory.StrategyTruncation, traceBudget)
	id := testIdentity()
	seedTurns(t, h, id, 3)

	resp, err := memprotocol.StrategyTrace(context.Background(),
		memprotocol.StrategyTraceDeps{Store: h.store}, id)
	if err != nil {
		t.Fatalf("StrategyTrace: %v", err)
	}
	if resp.Trace.Strategy != string(memory.StrategyTruncation) {
		t.Errorf("Strategy = %q, want truncation", resp.Trace.Strategy)
	}
	if resp.Trace.RecentTurnCount != 3 {
		t.Errorf("RecentTurnCount = %d, want 3", resp.Trace.RecentTurnCount)
	}
	if resp.Trace.Health != string(memory.HealthHealthy) {
		t.Errorf("Health = %q, want healthy", resp.Trace.Health)
	}
	if resp.Trace.EstimatedTokens < 0 {
		t.Errorf("EstimatedTokens = %d, want >= 0", resp.Trace.EstimatedTokens)
	}
	if resp.ProtocolVersion != prototypes.ProtocolVersion {
		t.Errorf("ProtocolVersion = %q, want %q", resp.ProtocolVersion, prototypes.ProtocolVersion)
	}
}

func TestStrategyTrace_FailsLoudlyOnIncompleteIdentity(t *testing.T) {
	h := newMemHarness(t, memory.StrategyTruncation, traceBudget)
	_, err := memprotocol.StrategyTrace(context.Background(),
		memprotocol.StrategyTraceDeps{Store: h.store},
		identity.Quadruple{Identity: identity.Identity{UserID: "u", SessionID: "s"}})
	if !errors.Is(err, memory.ErrIdentityRequired) {
		t.Fatalf("StrategyTrace err = %v, want ErrIdentityRequired", err)
	}
}

func TestPut_AppendsTurnReturnsResolvableKeyAndAudits(t *testing.T) {
	h := newMemHarness(t, memory.StrategyTruncation, traceBudget)
	id := testIdentity()

	// Subscribe BEFORE the mutation so the audit event is observed.
	sub, err := h.bus.Subscribe(context.Background(), events.Filter{
		Tenant:  id.TenantID,
		User:    id.UserID,
		Session: id.SessionID,
		Types:   []events.EventType{memory.EventTypeMemoryItemPut},
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Cancel()

	resp, err := memprotocol.Put(context.Background(),
		memprotocol.PutDeps{Store: h.store, Bus: h.bus},
		prototypes.MemoryPutRequest{Turn: prototypes.MemoryTurnInput{
			UserMessage:       "operator added question",
			AssistantResponse: "operator added answer",
		}}, id)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if resp.Key == "" {
		t.Fatal("Put returned an empty key for the appended turn")
	}

	// The returned key resolves to the appended turn via memory.get.
	got, err := memprotocol.Get(context.Background(),
		memprotocol.GetDeps{Store: h.store, Artifacts: h.artifacts, DriverName: "inmem", HeavyThreshold: 1 << 20},
		prototypes.MemoryGetRequest{Key: resp.Key}, id)
	if err != nil {
		t.Fatalf("Get(put key): %v", err)
	}
	if got.Detail.Item.Key != resp.Key {
		t.Errorf("Get resolved key %q, want %q", got.Detail.Item.Key, resp.Key)
	}

	// The audit event fired with the operation + key (never the text).
	select {
	case ev := <-sub.Events():
		if ev.Type != memory.EventTypeMemoryItemPut {
			t.Errorf("event type = %q, want memory.item_put", ev.Type)
		}
		p, ok := ev.Payload.(memory.MemoryMutationPayload)
		if !ok {
			t.Fatalf("payload type = %T, want MemoryMutationPayload", ev.Payload)
		}
		if p.Operation != "put" || p.Key != resp.Key {
			t.Errorf("payload = %+v, want {put, %q}", p, resp.Key)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the memory.item_put audit event")
	}
}

func TestDelete_EvictsTurnByKeyAndAudits(t *testing.T) {
	h := newMemHarness(t, memory.StrategyTruncation, traceBudget)
	id := testIdentity()
	seedTurns(t, h, id, 4)

	// List to obtain a real turn key.
	list, err := memprotocol.List(context.Background(),
		memprotocol.ListDeps{Store: h.store, DriverName: "inmem", HeavyThreshold: 1 << 20},
		prototypes.MemoryListRequest{}, id)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list.Items) != 4 {
		t.Fatalf("seeded list = %d items, want 4", len(list.Items))
	}
	victim := list.Items[0].Key

	sub, err := h.bus.Subscribe(context.Background(), events.Filter{
		Tenant: id.TenantID, User: id.UserID, Session: id.SessionID,
		Types: []events.EventType{memory.EventTypeMemoryItemDeleted},
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Cancel()

	resp, err := memprotocol.Delete(context.Background(),
		memprotocol.DeleteDeps{Store: h.store, Bus: h.bus},
		prototypes.MemoryDeleteRequest{Key: victim}, id)
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if !resp.Deleted || resp.RemainingTurns != 3 {
		t.Errorf("Delete resp = %+v, want {deleted:true, remaining:3}", resp)
	}

	// The evicted turn is gone; the others survive (summary round-tripped).
	after, err := memprotocol.List(context.Background(),
		memprotocol.ListDeps{Store: h.store, DriverName: "inmem", HeavyThreshold: 1 << 20},
		prototypes.MemoryListRequest{}, id)
	if err != nil {
		t.Fatalf("List(after): %v", err)
	}
	if len(after.Items) != 3 {
		t.Errorf("post-delete list = %d items, want 3", len(after.Items))
	}
	for _, it := range after.Items {
		if it.Key == victim {
			t.Errorf("evicted key %q still present", victim)
		}
	}

	select {
	case ev := <-sub.Events():
		p, ok := ev.Payload.(memory.MemoryMutationPayload)
		if !ok || p.Operation != "delete" || p.Key != victim {
			t.Errorf("delete audit payload = %+v (ok=%v), want {delete, %q}", ev.Payload, ok, victim)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the memory.item_deleted audit event")
	}
}

func TestDelete_NotFoundOnUnknownKey(t *testing.T) {
	h := newMemHarness(t, memory.StrategyTruncation, traceBudget)
	id := testIdentity()
	seedTurns(t, h, id, 2)

	_, err := memprotocol.Delete(context.Background(),
		memprotocol.DeleteDeps{Store: h.store, Bus: h.bus},
		prototypes.MemoryDeleteRequest{Key: "mem_deadbeefdeadbeef"}, id)
	if !errors.Is(err, memory.ErrNotFound) {
		t.Fatalf("Delete(unknown key) err = %v, want ErrNotFound", err)
	}
}

// TestRecord_SummaryRoundTrips proves the Phase 108n (D-186) fix: the
// `memory.Record.Summary` field round-trips through a JSON marshal /
// unmarshal so the `memory.delete` read-modify-write (Snapshot → decode →
// drop turn → re-marshal → Restore) preserves the rolling-summary text
// LOSSLESSLY rather than dropping it. Before 108n, `memory.Record` had no
// Summary field, so re-marshalling a decoded snapshot would silently lose it.
func TestRecord_SummaryRoundTrips(t *testing.T) {
	orig := memory.Record{
		Strategy: memory.StrategyRollingSummary,
		Turns: []memory.ConversationTurn{
			{UserMessage: "u1", AssistantResponse: "a1", Timestamp: time.Unix(1, 0).UTC()},
			{UserMessage: "u2", AssistantResponse: "a2", Timestamp: time.Unix(2, 0).UTC()},
		},
		Summary: "ROLLING-SUMMARY-PRESERVED",
	}
	bytes, err := json.Marshal(orig)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	// Drop the first turn (the delete read-modify-write shape).
	var rec memory.Record
	if err := json.Unmarshal(bytes, &rec); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	rec.Turns = rec.Turns[1:]
	out, err := json.Marshal(rec)
	if err != nil {
		t.Fatalf("re-marshal: %v", err)
	}
	var got memory.Record
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if got.Summary != "ROLLING-SUMMARY-PRESERVED" {
		t.Errorf("Summary = %q, want it preserved across the delete round-trip", got.Summary)
	}
	if len(got.Turns) != 1 || got.Turns[0].UserMessage != "u2" {
		t.Errorf("Turns = %+v, want only the second turn surviving", got.Turns)
	}
}

func TestPutDelete_FailLoudlyOnIncompleteIdentity(t *testing.T) {
	h := newMemHarness(t, memory.StrategyTruncation, traceBudget)
	bad := identity.Quadruple{Identity: identity.Identity{TenantID: "t", SessionID: "s"}} // user empty

	if _, err := memprotocol.Put(context.Background(),
		memprotocol.PutDeps{Store: h.store, Bus: h.bus},
		prototypes.MemoryPutRequest{Turn: prototypes.MemoryTurnInput{UserMessage: "x"}}, bad); !errors.Is(err, memory.ErrIdentityRequired) {
		t.Errorf("Put err = %v, want ErrIdentityRequired", err)
	}
	if _, err := memprotocol.Delete(context.Background(),
		memprotocol.DeleteDeps{Store: h.store, Bus: h.bus},
		prototypes.MemoryDeleteRequest{Key: "mem_x"}, bad); !errors.Is(err, memory.ErrIdentityRequired) {
		t.Errorf("Delete err = %v, want ErrIdentityRequired", err)
	}
}
