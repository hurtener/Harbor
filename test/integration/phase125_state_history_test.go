// Phase 125 integration test — the `state.history` windowed event-replay
// surface exercised end-to-end against the REAL durable EventBus (over an
// inmem StateStore) + the REAL ArtifactStore behind the REAL
// StateHistoryHandler over httptest.
//
// This is the binding §17 integration test for Phase 125: its plan's Deps
// span the events subsystem (the durable driver), the artifacts subsystem
// (the routable-ref resolver), and the protocol transport (the handler).
// Real drivers everywhere on the seam — no mocks. Identity propagation is
// asserted through every layer; the tail-first windowed page + scroll-up
// to the head is the acceptance criterion; a returned StateArtifactRef.ID
// is routed to artifacts.get_ref (accepting the typed
// CodePresignUnsupported/501 on the default inmem store as proof the id
// reached the resolver well-formed); the missing-identity 401 +
// cross-tenant 404 failure modes are covered; an N≥10 concurrency stress
// runs under -race.
package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/artifacts"
	_ "github.com/hurtener/Harbor/internal/artifacts/drivers/inmem"
	"github.com/hurtener/Harbor/internal/audit"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/events/drivers/durable"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/transports"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/runtime/steering"
	statinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	"github.com/hurtener/Harbor/internal/tasks"
	_ "github.com/hurtener/Harbor/internal/tasks/drivers/inprocess"
)

type stateHistoryStack struct {
	srv   *httptest.Server
	bus   events.EventBus
	store artifacts.ArtifactStore
}

func newStateHistoryStack(t *testing.T) *stateHistoryStack {
	t.Helper()
	red := auditpatterns.New()
	st, err := statinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state inmem: %v", err)
	}
	bus, err := durable.New(context.Background(), config.EventsConfig{
		Driver:                   "durable",
		MaxSubscribersPerSession: 32,
		SubscriberBufferSize:     256,
		IdleTimeout:              time.Minute,
		DropWindow:               time.Second,
		ReplayBufferSize:         64,
		LegacyWritersDrained:     true,
	}, red, st)
	if err != nil {
		t.Fatalf("durable.New: %v", err)
	}
	store, err := artifacts.Open(context.Background(), config.ArtifactsConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("artifacts.Open: %v", err)
	}
	taskReg, err := tasks.Open(context.Background(), tasks.Dependencies{
		Store: st, Bus: bus, Redactor: audit.Redactor(red),
		Cfg: config.TasksConfig{Driver: "inprocess"},
	})
	if err != nil {
		t.Fatalf("tasks.Open: %v", err)
	}
	surface, err := protocol.NewControlSurface(taskReg, steering.NewRegistry())
	if err != nil {
		t.Fatalf("NewControlSurface: %v", err)
	}
	// artifacts.get_ref lives on the control surface; wire it so the
	// returned ref id can be routed to the resolver (the routable-ref gate).
	artSurface, err := protocol.NewArtifactsSurface(protocol.ArtifactsDeps{
		Store:                store,
		Redactor:             audit.Redactor(red),
		Bus:                  bus,
		Clock:                time.Now,
		DriverName:           "inmem",
		MaxBodyBytes:         1 << 20,
		FetchDefaultMaxBytes: config.DefaultArtifactFetchMaxBytes,
		FetchHardMaxBytes:    config.DefaultArtifactFetchHardMaxBytes,
	})
	if err != nil {
		t.Fatalf("NewArtifactsSurface: %v", err)
	}
	mux, err := transports.NewMux(surface, bus,
		transports.WithoutValidator(),
		transports.WithStateHistory(bus, store),
		transports.WithArtifactsSurface(artSurface),
	)
	if err != nil {
		t.Fatalf("NewMux: %v", err)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(func() {
		srv.Close()
		_ = taskReg.Close(context.Background())
		_ = store.Close(context.Background())
		_ = bus.Close(context.Background())
		_ = st.Close(context.Background())
	})
	return &stateHistoryStack{srv: srv, bus: bus, store: store}
}

func (s *stateHistoryStack) post(t *testing.T, path, body string, id identity.Identity) (int, []byte) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, s.srv.URL+path, strings.NewReader(body))
	if id.TenantID != "" {
		req.Header.Set("X-Harbor-Tenant", id.TenantID)
		req.Header.Set("X-Harbor-User", id.UserID)
		req.Header.Set("X-Harbor-Session", id.SessionID)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", path, err)
	}
	raw, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode, raw
}

func seedEvents(t *testing.T, bus events.EventBus, id identity.Identity, n int) {
	t.Helper()
	for i := range n {
		ev := events.Event{
			Type:     events.EventTypeRuntimeWarning,
			Identity: identity.Quadruple{Identity: id},
			Payload:  events.SubscriptionIdleClosedPayload{SubscriberID: uint64(i)},
		}
		if err := bus.Publish(context.Background(), ev); err != nil {
			t.Fatalf("publish #%d: %v", i, err)
		}
	}
}

// heavyEventPayload carries an offloaded-artifact reference in the REAL
// D-026 stamping shape — a STRING "artifact_ref" id with mime / size_bytes
// siblings, byte-identical to llm.ArtifactStub's JSON (the runtime's
// heavy-output edge). A `fetch` sub-object (which also carries an `id`) is
// included so the E2E proves the extractor surfaces exactly one ref, not a
// phantom second from fetch.id.
type heavyEventPayload struct {
	events.SafeSealed
	Ref       string         `json:"artifact_ref"`
	MIME      string         `json:"mime"`
	SizeBytes int64          `json:"size_bytes"`
	Fetch     map[string]any `json:"fetch,omitempty"`
}

func TestE2E_StateHistory_WindowedReplay(t *testing.T) {
	stack := newStateHistoryStack(t)
	id := identity.Identity{TenantID: "t-e2e", UserID: "u-e2e", SessionID: "s-e2e"}

	// Seed an ordered event set, then a heavy offloaded payload as the
	// newest event.
	seedEvents(t, stack.bus, id, 12)
	ref, err := stack.store.PutBytes(context.Background(),
		artifacts.ArtifactScope{TenantID: id.TenantID, UserID: id.UserID, SessionID: id.SessionID},
		bytes.Repeat([]byte("y"), 8192),
		artifacts.PutOpts{MimeType: "application/json", Namespace: "default", Filename: "heavy.json"})
	if err != nil {
		t.Fatalf("PutBytes: %v", err)
	}
	if pubErr := stack.bus.Publish(context.Background(), events.Event{
		Type:     events.EventTypeRuntimeWarning,
		Identity: identity.Quadruple{Identity: id},
		Payload:  heavyEventPayload{Ref: ref.ID, MIME: "application/json", SizeBytes: 8192, Fetch: map[string]any{"tool": "artifact_fetch", "id": ref.ID}},
	}); pubErr != nil {
		t.Fatalf("publish heavy: %v", pubErr)
	}
	// Sequences are now 1..13 (12 plain + 1 heavy at seq 13).

	// 1. Tail-first window page.
	status, raw := stack.post(t, "/v1/state/history", `{"session_id":"s-e2e","limit":5}`, id)
	if status != http.StatusOK {
		t.Fatalf("tail window status = %d, want 200; body=%s", status, raw)
	}
	var page1 prototypes.StateHistoryResponse
	mustJSON(t, raw, &page1)
	if page1.HeadSequence != 1 || page1.TailSequence != 13 {
		t.Fatalf("head/tail = %d/%d, want 1/13", page1.HeadSequence, page1.TailSequence)
	}
	if len(page1.Events) != 5 || page1.Events[4].Sequence != 13 {
		t.Fatalf("tail page = %d events ending at %d, want 5 ending at 13", len(page1.Events), page1.Events[len(page1.Events)-1].Sequence)
	}
	// The newest event carries the routable artifact ref.
	heavy := page1.Events[len(page1.Events)-1]
	if len(heavy.Artifacts) != 1 || heavy.Artifacts[0].ID == "" {
		t.Fatalf("newest event artifacts = %+v, want one routable ref", heavy.Artifacts)
	}
	refID := heavy.Artifacts[0].ID

	// 2. Scroll up by NextCursor until the head is reached.
	cursor := page1.NextCursor
	seen := len(page1.Events)
	guard := 0
	for cursor != 0 {
		guard++
		if guard > 20 {
			t.Fatal("scroll-up did not terminate")
		}
		st, b := stack.post(t, "/v1/state/history",
			fmt.Sprintf(`{"session_id":"s-e2e","before":%d,"limit":5}`, cursor), id)
		if st != http.StatusOK {
			t.Fatalf("scroll page status = %d; body=%s", st, b)
		}
		var p prototypes.StateHistoryResponse
		mustJSON(t, b, &p)
		seen += len(p.Events)
		cursor = p.NextCursor
	}
	if seen != 13 {
		t.Fatalf("scroll-up saw %d events, want all 13 (reached head)", seen)
	}

	// 3. The returned ref id ROUTES to artifacts.get_ref. On the default
	//    inmem store (NON-Presigner) the resolver returns the typed
	//    CodePresignUnsupported/501 — which proves the id reached the
	//    resolver well-formed. (The 200-resolves leg is gated behind a
	//    HARBOR_LIVE_* S3 env per §17.8.)
	// No `identity` member — the artifacts wire types scope by `scope`.
	// The control transport decodes strictly, so a stray `identity` is a
	// 400 rather than a silently discarded member.
	getRefBody := fmt.Sprintf(`{"id":%q,"scope":{"tenant":"t-e2e","user":"u-e2e","session":"s-e2e"}}`, refID)
	rst, rraw := stack.post(t, "/v1/control/artifacts.get_ref", getRefBody, id)
	switch rst {
	case http.StatusOK:
		// An S3-compat Presigner store would resolve to a presigned URL.
	case http.StatusNotImplemented:
		assertCodeIs(t, rraw, protoerrors.CodePresignUnsupported)
	default:
		t.Fatalf("artifacts.get_ref status = %d (want 200 or 501 — the id must ROUTE well-formed); body=%s", rst, rraw)
	}

	// 4. Optionally confirm the artifact exists via artifacts.list. The
	//    load-bearing gate (the ref routes to the resolver) is already
	//    asserted above; this is a soft existence confirmation.
	lst, lraw := stack.post(t, "/v1/control/artifacts.list",
		`{"scope":{"tenant":"t-e2e","user":"u-e2e","session":"s-e2e"}}`, id)
	if lst == http.StatusOK && !strings.Contains(string(lraw), refID) {
		t.Errorf("artifacts.list does not mention the seeded ref %q", refID)
	}
}

func TestE2E_StateHistory_FailureModes(t *testing.T) {
	stack := newStateHistoryStack(t)
	id := identity.Identity{TenantID: "t-fm", UserID: "u-fm", SessionID: "s-fm"}
	seedEvents(t, stack.bus, id, 3)

	// Missing identity → 401.
	st, b := stack.post(t, "/v1/state/history", `{"session_id":"s-fm"}`, identity.Identity{})
	if st != http.StatusUnauthorized {
		t.Fatalf("missing-identity status = %d, want 401; body=%s", st, b)
	}
	assertCodeIs(t, b, protoerrors.CodeIdentityRequired)

	// Cross-tenant without admin → 404 EXACTLY (no existence leak).
	st, b = stack.post(t, "/v1/state/history",
		`{"identity":{"tenant":"t-other","user":"u-fm","session":"s-fm"},"session_id":"s-fm"}`, id)
	if st != http.StatusNotFound {
		t.Fatalf("cross-tenant status = %d, want 404 (no existence leak); body=%s", st, b)
	}
	assertCodeIs(t, b, protoerrors.CodeNotFound)
}

func TestE2E_StateHistory_ConcurrencyStress(t *testing.T) {
	stack := newStateHistoryStack(t)
	// Seed distinct identities.
	const ids = 10
	for k := range ids {
		id := identity.Identity{
			TenantID:  fmt.Sprintf("t-s%d", k),
			UserID:    fmt.Sprintf("u-s%d", k),
			SessionID: fmt.Sprintf("s-s%d", k),
		}
		seedEvents(t, stack.bus, id, 4+k)
	}

	const n = 60
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			k := i % ids
			id := identity.Identity{
				TenantID:  fmt.Sprintf("t-s%d", k),
				UserID:    fmt.Sprintf("u-s%d", k),
				SessionID: fmt.Sprintf("s-s%d", k),
			}
			st, raw := stack.post(t, "/v1/state/history",
				fmt.Sprintf(`{"session_id":"s-s%d","limit":50}`, k), id)
			if st != http.StatusOK {
				errs <- fmt.Errorf("k=%d status=%d body=%s", k, st, raw)
				return
			}
			var p prototypes.StateHistoryResponse
			if err := json.Unmarshal(raw, &p); err != nil {
				errs <- err
				return
			}
			if len(p.Events) != 4+k {
				errs <- fmt.Errorf("k=%d got %d events, want %d (cross-identity bleed?)", k, len(p.Events), 4+k)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Error(err)
		}
	}
}

func mustJSON(t *testing.T, raw []byte, dst any) {
	t.Helper()
	if err := json.Unmarshal(raw, dst); err != nil {
		t.Fatalf("decode: %v; body=%s", err, raw)
	}
}

func assertCodeIs(t *testing.T, raw []byte, want protoerrors.Code) {
	t.Helper()
	var e protoerrors.Error
	if err := json.Unmarshal(raw, &e); err != nil {
		t.Fatalf("decode error body: %v; body=%s", err, raw)
	}
	if e.Code != want {
		t.Fatalf("error code = %q, want %q; body=%s", e.Code, want, raw)
	}
}
