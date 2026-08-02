package stream_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/agentcfg"
	_ "github.com/hurtener/Harbor/internal/agentcfg/drivers/statestore"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	eventsinmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	"github.com/hurtener/Harbor/internal/protocol/transports/stream"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	agentcfgprotocol "github.com/hurtener/Harbor/internal/runtime/agentcfg/protocol"
	"github.com/hurtener/Harbor/internal/state"
	stateinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
)

// historicalReadHarness wires the production signed-reach handler with a
// lifecycle ensurer that fails if a historical route attempts to invoke it.
// The counter makes the zero-write requirement directly observable.
type historicalReadHarness struct {
	handler  http.Handler
	state    *historicalReadState
	registry agentcfg.Registry
	calls    atomic.Int32
}

// historicalReadState observes the handler's persistence boundary while
// retaining every production StateStore method through embedding. Counters
// are reset after fixture setup so each assertion describes only one request.
type historicalReadState struct {
	state.StateStore
	loads  atomic.Int32
	writes atomic.Int32
}

func (s *historicalReadState) Load(ctx context.Context, id identity.Quadruple, kind string) (state.StateRecord, error) {
	s.loads.Add(1)
	return s.StateStore.Load(ctx, id, kind)
}

func (s *historicalReadState) Save(ctx context.Context, record state.StateRecord) error {
	s.writes.Add(1)
	return s.StateStore.Save(ctx, record)
}

func (s *historicalReadState) SaveIf(ctx context.Context, expectations []state.SlotExpectation, next state.StateRecord) error {
	s.writes.Add(1)
	return s.StateStore.SaveIf(ctx, expectations, next)
}

func (s *historicalReadState) reset() {
	s.loads.Store(0)
	s.writes.Store(0)
}

func newHistoricalReadHarness(t *testing.T) *historicalReadHarness {
	t.Helper()
	ctx := t.Context()
	base, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	st := &historicalReadState{StateStore: base}
	bus, err := eventsinmem.New(config.EventsConfig{
		Driver: "inmem", MaxSubscribersPerSession: 8, SubscriberBufferSize: 32,
		IdleTimeout: 30 * time.Second, DropWindow: time.Second,
	}, auditpatterns.New())
	if err != nil {
		t.Fatalf("events: %v", err)
	}
	reg, err := agentcfg.Open(ctx, agentcfg.Config{}, agentcfg.Deps{State: st, Bus: bus})
	if err != nil {
		t.Fatalf("agentcfg.Open: %v", err)
	}
	h := &historicalReadHarness{state: st, registry: reg}
	svc, err := agentcfgprotocol.NewService(reg,
		agentcfgprotocol.WithBootLifecycleEnsurer(acAgent, func(context.Context, identity.Identity, string) error {
			h.calls.Add(1)
			return errors.New("historical read must not invoke lifecycle bootstrap")
		}),
	)
	if err != nil {
		t.Fatalf("service: %v", err)
	}
	h.handler, err = stream.NewAgentConfigHandler(svc,
		stream.WithAgentConfigReachAuthorizer(auth.NewAgentReachAuthorizer()))
	if err != nil {
		t.Fatalf("handler: %v", err)
	}
	t.Cleanup(func() {
		_ = reg.Close(context.Background())
		_ = bus.Close(context.Background())
		_ = st.Close(context.Background())
	})
	return h
}

func (h *historicalReadHarness) seedUserHistory(t *testing.T) (agentcfg.Revision, agentcfg.Revision) {
	t.Helper()
	q := identity.Quadruple{Identity: *acID()}
	firstText, secondText := "first history", "second history"
	first, err := h.registry.SetRevision(t.Context(), q, acAgent, agentcfg.ConfigScopeUser,
		agentcfg.ConfigPayload{PromptLayers: &agentcfg.PromptLayers{User: &firstText}}, agentcfg.SetOptions{})
	if err != nil {
		t.Fatalf("seed first user revision: %v", err)
	}
	second, err := h.registry.SetRevision(t.Context(), q, acAgent, agentcfg.ConfigScopeUser,
		agentcfg.ConfigPayload{PromptLayers: &agentcfg.PromptLayers{User: &secondText}}, agentcfg.SetOptions{})
	if err != nil {
		t.Fatalf("seed second user revision: %v", err)
	}
	return first, second
}

func userListBody() string {
	return `{"identity":{"tenant":"t1","user":"u1","session":"s1"},"agent_id":"` + acAgent + `"}`
}

func userDiffBody(from, to string) string {
	return `{"identity":{"tenant":"t1","user":"u1","session":"s1"},"agent_id":"` + acAgent + `","from_revision":"` + from + `","to_revision":"` + to + `"}`
}

func decodeHistorical[T any](t *testing.T, raw []byte) T {
	t.Helper()
	var out T
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode response %q: %v", raw, err)
	}
	return out
}

// TestAgentConfigHandler_UserHistoricalReads_AbsentBootLifecycleAreReadOnly
// proves list/diff do not materialise the configured default when its tenant
// lifecycle slot is absent, while still returning the caller's immutable user
// history through the signed-reach wire path.
func TestAgentConfigHandler_UserHistoricalReads_AbsentBootLifecycleAreReadOnly(t *testing.T) {
	h := newHistoricalReadHarness(t)
	first, second := h.seedUserHistory(t)
	slot, kind, err := agentcfg.LifecycleSlot(acID().TenantID, acAgent)
	if err != nil {
		t.Fatalf("LifecycleSlot: %v", err)
	}
	if _, err := h.state.Load(t.Context(), slot, kind); !errors.Is(err, state.ErrNotFound) {
		t.Fatalf("precondition lifecycle = %v, want absent", err)
	}
	h.state.reset()

	code, raw := acReq(t, h.handler, "user/list_revisions", userListBody(), acID(), []auth.Scope{auth.ScopeAgentConfigUser})
	if code != http.StatusOK {
		t.Fatalf("historical list status=%d body=%s", code, raw)
	}
	list := decodeHistorical[prototypes.AgentConfigUserListRevisionsResponse](t, raw)
	if len(list.Revisions) != 2 || list.Revisions[0].RevisionID != second.RevisionID || list.Revisions[1].RevisionID != first.RevisionID {
		t.Fatalf("historical list mismatch: %+v", list.Revisions)
	}

	code, raw = acReq(t, h.handler, "user/diff", userDiffBody(first.RevisionID, second.RevisionID), acID(), []auth.Scope{auth.ScopeAgentConfigUser})
	if code != http.StatusOK {
		t.Fatalf("historical diff status=%d body=%s", code, raw)
	}
	diff := decodeHistorical[prototypes.AgentConfigUserDiffResponse](t, raw)
	if !diff.Diff.PromptLayers.UserChanged {
		t.Fatal("historical diff omitted the user-layer change")
	}
	if got := h.calls.Load(); got != 0 {
		t.Fatalf("historical reads invoked lifecycle bootstrap %d times, want 0", got)
	}
	if got := h.state.writes.Load(); got != 0 {
		t.Fatalf("historical reads performed %d StateStore writes, want 0", got)
	}
	if _, err := h.state.Load(t.Context(), slot, kind); !errors.Is(err, state.ErrNotFound) {
		t.Fatalf("historical reads materialised lifecycle slot: %v", err)
	}

	// Denied reach remains before any registry lookup or lifecycle callback.
	h.state.reset()
	code, raw = acReqReach(t, h.handler, "user/list_revisions", userListBody(), acID(), []auth.Scope{auth.ScopeAgentConfigUser}, []string{"other-agent"})
	if code != http.StatusForbidden {
		t.Fatalf("denied historical list status=%d body=%s", code, raw)
	}
	if got := h.calls.Load(); got != 0 {
		t.Fatalf("denied reach invoked lifecycle bootstrap %d times, want 0", got)
	}
	if got := h.state.loads.Load(); got != 0 || h.state.writes.Load() != 0 {
		t.Fatalf("denied reach touched StateStore loads=%d writes=%d, want zero", got, h.state.writes.Load())
	}
}

// TestAgentConfigHandler_UserHistoricalReads_SurviveTerminalLifecycle proves
// retirement protects current/mutating doors without erasing immutable
// per-user history. List and diff remain signed-reach protected read paths.
func TestAgentConfigHandler_UserHistoricalReads_SurviveTerminalLifecycle(t *testing.T) {
	h := newHistoricalReadHarness(t)
	first, second := h.seedUserHistory(t)
	slot, kind, err := agentcfg.LifecycleSlot(acID().TenantID, acAgent)
	if err != nil {
		t.Fatalf("LifecycleSlot: %v", err)
	}
	terminal := []byte(`{"schema":1,"revision_id":"","updated_at":"2026-08-02T00:00:00Z"}`)
	if err := h.state.Save(t.Context(), state.StateRecord{ID: state.NewEventID(), Identity: slot, Kind: kind, Bytes: terminal}); err != nil {
		t.Fatalf("seed terminal lifecycle: %v", err)
	}
	h.state.reset()

	code, raw := acReq(t, h.handler, "user/list_revisions", userListBody(), acID(), []auth.Scope{auth.ScopeAgentConfigUser})
	if code != http.StatusOK {
		t.Fatalf("retired historical list status=%d body=%s", code, raw)
	}
	list := decodeHistorical[prototypes.AgentConfigUserListRevisionsResponse](t, raw)
	if len(list.Revisions) != 2 || list.Revisions[0].RevisionID != second.RevisionID || list.Revisions[1].RevisionID != first.RevisionID {
		t.Fatalf("retired historical list mismatch: %+v", list.Revisions)
	}
	code, raw = acReq(t, h.handler, "user/diff", userDiffBody(first.RevisionID, second.RevisionID), acID(), []auth.Scope{auth.ScopeAgentConfigUser})
	if code != http.StatusOK {
		t.Fatalf("retired historical diff status=%d body=%s", code, raw)
	}
	diff := decodeHistorical[prototypes.AgentConfigUserDiffResponse](t, raw)
	if !diff.Diff.PromptLayers.UserChanged {
		t.Fatal("retired historical diff omitted the user-layer change")
	}
	if got := h.calls.Load(); got != 0 {
		t.Fatalf("retired historical reads invoked lifecycle bootstrap %d times, want 0", got)
	}
	if got := h.state.writes.Load(); got != 0 {
		t.Fatalf("retired historical reads performed %d StateStore writes, want 0", got)
	}
	record, err := h.state.Load(t.Context(), slot, kind)
	if err != nil || string(record.Bytes) != string(terminal) {
		t.Fatalf("terminal lifecycle changed: record=%s err=%v", record.Bytes, err)
	}
}
