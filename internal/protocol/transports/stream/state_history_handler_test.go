package stream_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/artifacts"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/events/drivers/durable"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/transports/stream"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	stateinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
)

// stateHistID is a documented dummy identity triple (CLAUDE.md §13).
var stateHistID = identity.Identity{TenantID: "t-sh", UserID: "u-sh", SessionID: "s-sh"}

func stateHistCfg() config.EventsConfig {
	return config.EventsConfig{
		Driver:                   "durable",
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     256,
		IdleTimeout:              60 * time.Second,
		DropWindow:               time.Second,
		ReplayBufferSize:         64,
		LegacyWritersDrained:     true,
	}
}

func newDurableBusForHistory(t *testing.T) events.EventBus {
	t.Helper()
	s, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("stateinmem.New: %v", err)
	}
	bus, err := durable.New(context.Background(), stateHistCfg(), auditpatterns.New(), s)
	if err != nil {
		t.Fatalf("durable.New: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })
	return bus
}

// heavyRefPayload is a SafePayload that carries an offloaded-artifact
// reference in the REAL D-026 stamping shape: a STRING "artifact_ref" id
// with mime / size_bytes / hash siblings — byte-identical to what
// llm.ArtifactStub.MarshalJSON emits (the runtime's heavy-output edge).
// SafePayload keeps the test deterministic (no redactor remapping); the
// handler's ref-extractor walks the persisted map exactly as it walks a
// redacted one. The `fetch` hint (a sub-object that ALSO carries an `id`)
// is included so the test proves the extractor does NOT mistake fetch.id
// for a second ref.
type heavyRefPayload struct {
	events.SafeSealed
	Note      string         `json:"note"`
	Ref       string         `json:"artifact_ref"`
	MIME      string         `json:"mime"`
	SizeBytes int64          `json:"size_bytes"`
	Hash      string         `json:"hash,omitempty"`
	Fetch     map[string]any `json:"fetch,omitempty"`
}

// plainPayload is a SafePayload with no artifact ref.
type plainPayload struct {
	events.SafeSealed
	Note string `json:"note"`
}

func publishPlain(t *testing.T, bus events.EventBus, id identity.Identity, n int) {
	t.Helper()
	for i := range n {
		ev := events.Event{
			Type:     events.EventTypeRuntimeWarning,
			Identity: identity.Quadruple{Identity: id},
			Payload:  plainPayload{Note: fmt.Sprintf("ev-%d", i)},
		}
		if err := bus.Publish(context.Background(), ev); err != nil {
			t.Fatalf("Publish #%d: %v", i, err)
		}
	}
}

// seedHeavyArtifact stores bytes and returns the resulting ref id.
func seedHeavyArtifact(t *testing.T, store artifacts.ArtifactStore, id identity.Identity) artifacts.ArtifactRef {
	t.Helper()
	ref, err := store.PutBytes(context.Background(),
		artifacts.ArtifactScope{TenantID: id.TenantID, UserID: id.UserID, SessionID: id.SessionID},
		[]byte(strings.Repeat("x", 4096)),
		artifacts.PutOpts{MimeType: "application/json", Namespace: "default", Filename: "tool-result.json"})
	if err != nil {
		t.Fatalf("PutBytes: %v", err)
	}
	return ref
}

func newStateHistoryHandler(t *testing.T) (*stream.StateHistoryHandler, events.EventBus, artifacts.ArtifactStore) {
	t.Helper()
	bus := newDurableBusForHistory(t)
	store := newArtifactStore(t)
	h, err := stream.NewStateHistoryHandler(bus, store)
	if err != nil {
		t.Fatalf("NewStateHistoryHandler: %v", err)
	}
	return h, bus, store
}

func doStateHistory(t *testing.T, h http.Handler, body string, id *identity.Identity, scopes []auth.Scope) (int, []byte) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/state/history", strings.NewReader(body))
	if id != nil {
		req.Header.Set(stream.HeaderTenant, id.TenantID)
		req.Header.Set(stream.HeaderUser, id.UserID)
		req.Header.Set(stream.HeaderSession, id.SessionID)
	}
	if scopes != nil {
		req = req.WithContext(auth.WithScopes(req.Context(), scopes))
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes()
}

func TestStateHistory_TailFirstWindow(t *testing.T) {
	h, bus, _ := newStateHistoryHandler(t)
	publishPlain(t, bus, stateHistID, 10)

	body := `{"session_id":"s-sh","before":0,"limit":3}`
	status, resp := doStateHistory(t, h.Handler(), body, &stateHistID, nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", status, resp)
	}
	var got prototypes.StateHistoryResponse
	if err := json.Unmarshal(resp, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.HeadSequence != 1 || got.TailSequence != 10 {
		t.Fatalf("head/tail = %d/%d, want 1/10", got.HeadSequence, got.TailSequence)
	}
	if len(got.Events) != 3 {
		t.Fatalf("events len = %d, want 3", len(got.Events))
	}
	// Tail-first window, oldest-first within: 8,9,10.
	if got.Events[0].Sequence != 8 || got.Events[2].Sequence != 10 {
		t.Fatalf("events seqs = %d..%d, want 8..10", got.Events[0].Sequence, got.Events[2].Sequence)
	}
	// The last event's sequence equals the tail.
	if got.Events[len(got.Events)-1].Sequence != got.TailSequence {
		t.Errorf("last event seq %d != tail %d", got.Events[len(got.Events)-1].Sequence, got.TailSequence)
	}
	// NextCursor is the lowest sequence in the page (8) and older events
	// remain (head is 1).
	if got.NextCursor != 8 || !got.HasMore {
		t.Errorf("NextCursor/HasMore = %d/%v, want 8/true", got.NextCursor, got.HasMore)
	}
}

func TestStateHistory_ScrollUpReachesHead(t *testing.T) {
	h, bus, _ := newStateHistoryHandler(t)
	publishPlain(t, bus, stateHistID, 6)

	// First page: tail window of 3 ⇒ 4,5,6; next_cursor 4.
	_, b1 := doStateHistory(t, h.Handler(), `{"session_id":"s-sh","limit":3}`, &stateHistID, nil)
	var p1 prototypes.StateHistoryResponse
	if err := json.Unmarshal(b1, &p1); err != nil {
		t.Fatalf("decode p1: %v", err)
	}
	if p1.NextCursor != 4 {
		t.Fatalf("p1 next_cursor = %d, want 4", p1.NextCursor)
	}
	// Scroll up: before=4 ⇒ 1,2,3; head reached, NextCursor 0.
	body2 := fmt.Sprintf(`{"session_id":"s-sh","before":%d,"limit":3}`, p1.NextCursor)
	_, b2 := doStateHistory(t, h.Handler(), body2, &stateHistID, nil)
	var p2 prototypes.StateHistoryResponse
	if err := json.Unmarshal(b2, &p2); err != nil {
		t.Fatalf("decode p2: %v", err)
	}
	if len(p2.Events) != 3 || p2.Events[0].Sequence != 1 || p2.Events[2].Sequence != 3 {
		t.Fatalf("p2 events = %v, want seqs 1,2,3", p2.Events)
	}
	if p2.NextCursor != 0 || p2.HasMore {
		t.Errorf("p2 NextCursor/HasMore = %d/%v, want 0/false (head reached)", p2.NextCursor, p2.HasMore)
	}
}

func TestStateHistory_RoutableArtifactRef(t *testing.T) {
	h, bus, store := newStateHistoryHandler(t)
	// Seed the heavy artifact, then publish an event carrying its ref.
	ref := seedHeavyArtifact(t, store, stateHistID)
	ev := events.Event{
		Type:     events.EventTypeRuntimeWarning,
		Identity: identity.Quadruple{Identity: stateHistID},
		Payload: heavyRefPayload{
			Note:      "tool produced a heavy result",
			Ref:       ref.ID,
			MIME:      "application/json",
			SizeBytes: 4096,
			Hash:      "sha256:fixture",
			Fetch:     map[string]any{"tool": "artifact_fetch", "id": ref.ID},
		},
	}
	if err := bus.Publish(context.Background(), ev); err != nil {
		t.Fatalf("Publish heavy: %v", err)
	}

	_, resp := doStateHistory(t, h.Handler(), `{"session_id":"s-sh","limit":10}`, &stateHistID, nil)
	var got prototypes.StateHistoryResponse
	if err := json.Unmarshal(resp, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Events) != 1 {
		t.Fatalf("events len = %d, want 1", len(got.Events))
	}
	arts := got.Events[0].Artifacts
	if len(arts) != 1 {
		t.Fatalf("artifacts len = %d, want 1 (the routable ref)", len(arts))
	}
	if arts[0].ID != ref.ID {
		t.Fatalf("artifact ref id = %q, want %q (routable)", arts[0].ID, ref.ID)
	}
	// Best-effort enrichment filled the SHA256 + filename from the store.
	if arts[0].SHA256 == "" {
		t.Error("artifact ref SHA256 is empty — store enrichment did not fire")
	}
	if arts[0].SizeBytes != ref.SizeBytes {
		t.Errorf("artifact ref size = %d, want %d", arts[0].SizeBytes, ref.SizeBytes)
	}
}

func TestStateHistory_MissingIdentity401(t *testing.T) {
	h, _, _ := newStateHistoryHandler(t)
	status, body := doStateHistory(t, h.Handler(), `{}`, nil, nil)
	if status != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body=%s", status, body)
	}
	assertErrorCode(t, body, protoerrors.CodeIdentityRequired)
}

func TestStateHistory_CrossTenantWithoutAdmin404(t *testing.T) {
	h, bus, _ := newStateHistoryHandler(t)
	publishPlain(t, bus, stateHistID, 3)
	// A body naming a different tenant without the admin scope must be
	// 404 (existence is never revealed across identities), NEVER 403.
	body := `{"identity":{"tenant":"t-other","user":"u-sh","session":"s-sh"},"session_id":"s-sh"}`
	status, resp := doStateHistory(t, h.Handler(), body, &stateHistID, nil)
	if status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404 (no existence leak); body=%s", status, resp)
	}
	assertErrorCode(t, resp, protoerrors.CodeNotFound)
}

func TestStateHistory_UnknownSession404(t *testing.T) {
	h, bus, _ := newStateHistoryHandler(t)
	publishPlain(t, bus, stateHistID, 3)
	body := `{"session_id":"does-not-exist"}`
	status, resp := doStateHistory(t, h.Handler(), body, &stateHistID, nil)
	if status != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body=%s", status, resp)
	}
	assertErrorCode(t, resp, protoerrors.CodeNotFound)
}

func TestStateHistory_MissingSession400(t *testing.T) {
	h, _, _ := newStateHistoryHandler(t)
	status, resp := doStateHistory(t, h.Handler(), `{"limit":5}`, &stateHistID, nil)
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", status, resp)
	}
	assertErrorCode(t, resp, protoerrors.CodeInvalidRequest)
}

func TestStateHistory_NegativeLimit400(t *testing.T) {
	h, _, _ := newStateHistoryHandler(t)
	status, resp := doStateHistory(t, h.Handler(), `{"session_id":"s-sh","limit":-5}`, &stateHistID, nil)
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", status, resp)
	}
	assertErrorCode(t, resp, protoerrors.CodeInvalidRequest)
}

// busNoHistory implements events.EventBus but NOT events.HistoryReplayer.
type busNoHistory struct{}

func (busNoHistory) Publish(context.Context, events.Event) error { return nil }
func (busNoHistory) PublishLive(context.Context, events.Event) error { return nil }
func (busNoHistory) Subscribe(context.Context, events.Filter) (events.Subscription, error) {
	return nil, nil
}
func (busNoHistory) Close(context.Context) error { return nil }

func TestStateHistory_BusWithoutCapability500(t *testing.T) {
	store := newArtifactStore(t)
	h, err := stream.NewStateHistoryHandler(busNoHistory{}, store)
	if err != nil {
		t.Fatalf("NewStateHistoryHandler: %v", err)
	}
	status, resp := doStateHistory(t, h.Handler(), `{"session_id":"s-sh"}`, &stateHistID, nil)
	if status != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500 (loud, not a silent empty page); body=%s", status, resp)
	}
	assertErrorCode(t, resp, protoerrors.CodeRuntimeError)
}

func TestStateHistory_AdminCrossTenantAllowed(t *testing.T) {
	h, bus, _ := newStateHistoryHandler(t)
	// Seed events under a DIFFERENT tenant.
	other := identity.Identity{TenantID: "t-admin-other", UserID: "u-x", SessionID: "s-x"}
	publishPlain(t, bus, other, 4)

	// Caller is stateHistID but carries the admin scope and names the other
	// tenant in the body — the cross-identity read is permitted.
	body := `{"identity":{"tenant":"t-admin-other","user":"u-x","session":"s-x"},"session_id":"s-x"}`
	status, resp := doStateHistory(t, h.Handler(), body, &stateHistID, []auth.Scope{auth.ScopeAdmin})
	if status != http.StatusOK {
		t.Fatalf("admin cross-tenant status = %d, want 200; body=%s", status, resp)
	}
	var got prototypes.StateHistoryResponse
	if err := json.Unmarshal(resp, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.TailSequence != 4 {
		t.Fatalf("tail = %d, want 4", got.TailSequence)
	}
}

// TestStateHistory_WithRedactorIsAccepted — the audit path's redactor is
// wired through a constructor option, so the option itself must work. A
// granted cross-identity read publishes its record through this sink; an
// option that silently dropped the redactor would put an unvetted payload
// on the bus.
func TestStateHistory_WithRedactorIsAccepted(t *testing.T) {
	t.Parallel()
	bus := newDurableBusForHistory(t)
	store := newArtifactStore(t)
	h, err := stream.NewStateHistoryHandler(bus, store,
		stream.WithStateHistoryRedactor(auditpatterns.New()))
	if err != nil {
		t.Fatalf("NewStateHistoryHandler with a redactor: %v", err)
	}
	if h == nil {
		t.Fatal("NewStateHistoryHandler returned nil")
	}
	// A nil redactor is treated as unsupplied, never as a construction
	// failure — the same posture every other optional dependency holds.
	if _, err := stream.NewStateHistoryHandler(bus, store,
		stream.WithStateHistoryRedactor(nil)); err != nil {
		t.Fatalf("NewStateHistoryHandler with a nil redactor: %v", err)
	}
}
