package integration_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	stderrors "errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/artifacts"
	"github.com/hurtener/Harbor/internal/audit"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/events/drivers/durable"
	"github.com/hurtener/Harbor/internal/governance"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/protocol"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/protocol/transports"
	"github.com/hurtener/Harbor/internal/protocol/types"
	runtimeposture "github.com/hurtener/Harbor/internal/runtime/posture"
	"github.com/hurtener/Harbor/internal/runtime/steering"
	"github.com/hurtener/Harbor/internal/sessions"
	"github.com/hurtener/Harbor/internal/state"
	statepostgres "github.com/hurtener/Harbor/internal/state/drivers/postgres"
	"github.com/hurtener/Harbor/internal/tasks"
	tasksinproc "github.com/hurtener/Harbor/internal/tasks/drivers/inprocess"
)

// h1CountingStore forwards every StateStore method, so a newly introduced
// health scan cannot hide behind the embedded interface. Calls are internally
// synchronized. The one-shot stall begins inside a real durable commit while
// the publisher owns publishMu; no production hook or database lock is needed.
type h1CountingStore struct {
	state.StateStore
	calls   atomic.Int64
	stall   atomic.Bool
	entered chan struct{}
	release chan struct{}
}

func (s *h1CountingStore) SaveBatchIf(ctx context.Context, e []state.SlotExpectation, r []state.StateRecord) error {
	s.calls.Add(1)
	if s.stall.CompareAndSwap(true, false) {
		close(s.entered)
		select {
		case <-s.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return s.StateStore.SaveBatchIf(ctx, e, r)
}

type h1CountingBus struct {
	events.EventBus
	publishes atomic.Int64
}

func (b *h1CountingBus) Publish(ctx context.Context, ev events.Event) error {
	b.publishes.Add(1)
	return b.EventBus.Publish(ctx, ev)
}
func (b *h1CountingBus) OldestRetainedAt(ctx context.Context) (time.Time, bool, error) {
	return b.EventBus.(events.RetentionReporter).OldestRetainedAt(ctx)
}

// h1PostgresStore uses an isolated schema, cleaned after the bus has closed.
// HARBOR_PG_DSN must name an explicitly provisioned integration database.
func h1PostgresStore(t *testing.T) *h1CountingStore {
	t.Helper()
	dsn := os.Getenv("HARBOR_PG_DSN")
	if dsn == "" {
		t.Skip("reason: HARBOR_PG_DSN not set for real-Postgres H1 acceptance")
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(2)
	t.Cleanup(func() { _ = db.Close() })
	schema := "harbor_h1_" + strings.ToLower(string(state.NewEventID()))
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	if _, err := db.ExecContext(ctx, `CREATE SCHEMA "`+schema+`"`); err != nil {
		t.Fatalf("create H1 schema: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if _, err := db.ExecContext(ctx, `DROP SCHEMA "`+schema+`" CASCADE`); err != nil {
			t.Errorf("drop H1 schema: %v", err)
		}
	})
	q := u.Query()
	q.Set("options", "-c search_path="+schema)
	u.RawQuery = q.Encode()
	st, err := statepostgres.New(config.StateConfig{Driver: "postgres", DSN: u.String()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close(context.Background()) })
	return &h1CountingStore{StateStore: st, entered: make(chan struct{}), release: make(chan struct{})}
}

func h1PostureSurface(t *testing.T, bus *h1CountingBus, taskReg tasks.TaskRegistry, lister sessions.SessionLister, red audit.Redactor) *protocol.PostureSurface {
	t.Helper()
	cfg := &config.Config{}
	s, err := protocol.NewPostureSurface(protocol.PostureDeps{
		Clock:            time.Now,
		Health:           func(context.Context) []types.SubsystemHealth { return runtimeposture.HealthFromConfig(cfg) },
		EventsHorizon:    runtimeposture.EventsHorizonProvider(bus),
		RuntimeRetention: runtimeposture.RuntimeRetentionProvider(bus, taskReg, lister),
		Counters:         runtimeposture.CountersProvider(taskReg, lister, nil),
		Drivers:          func() []types.SubsystemDriver { return runtimeposture.DriversFromConfig(cfg) },
		Metrics:          func(context.Context) types.MetricsSnapshot { return types.MetricsSnapshot{} },
		Governance:       governance.NewPostureProvider(governance.Config{DefaultTier: "free", IdentityTiers: map[string]governance.TierConfig{"free": {BudgetCeilingUSD: 5}}}),
		LLM:              llm.NewPostureProvider(llm.ConfigSnapshot{Driver: "bifrost", Provider: "fixture", Model: "fixture"}),
		Redactor:         red, Bus: bus, InstanceID: "h1-real-postgres",
	})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// TestE2E_H1_HealthPostgres proves the liveness contract over the real durable
// bus, Postgres store, runtime horizon adapters, ES256 validator and HTTP mux.
// It never opens a network listener. A 50,000-entry head is built through
// bounded StateStore batches of canonical event fixtures, then recovered by
// a fresh durable bus, rather than 50,000 growing-head appends.
func TestE2E_H1_HealthPostgres(t *testing.T) {
	ctx := t.Context()
	st := h1PostgresStore(t)
	bus, event := h1SeedDurableHead(t, st)
	t.Cleanup(func() { _ = bus.Close(context.Background()) })
	seedID, seedAt := event.Identity.Identity, event.OccurredAt
	counted := &h1CountingBus{EventBus: bus}
	taskReg, err := tasksinproc.New(tasks.Dependencies{Store: st, Bus: bus, Redactor: auditpatterns.New()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = taskReg.Close(context.Background()) })
	lister, err := sessions.New(st, config.SessionsConfig{}, bus)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lister.CloseRegistry(context.Background()) })
	surface := h1PostureSurface(t, counted, taskReg, lister, auditpatterns.New())
	control, err := protocol.NewControlSurface(taskReg, steering.NewRegistry())
	if err != nil {
		t.Fatal(err)
	}
	artifactStore, err := artifacts.Open(ctx, config.ArtifactsConfig{Driver: "inmem"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = artifactStore.Close(context.Background()) })
	priv, pub := loadES256Phase61(t)
	validator, err := auth.NewValidator(newES256KeySet(phase162Kid, pub), auth.WithClock(func() time.Time { return fixedNowPhase162 }), auth.WithRedactor(auditpatterns.New()))
	if err != nil {
		t.Fatal(err)
	}
	mux, err := transports.NewMux(control, bus, transports.WithValidator(validator), transports.WithPostureSurface(surface), transports.WithEventsList(artifactStore))
	if err != nil {
		t.Fatal(err)
	}
	actor := identity.Identity{TenantID: "h1-actor-tenant", UserID: "h1-actor-user", SessionID: "h1-actor-session"}
	tokenFixture := &phase162Stack{priv: priv}
	tokens := []string{tokenFixture.token(t, actor, nil), tokenFixture.token(t, actor, []string{string(auth.ScopeConsoleFleet)}), tokenFixture.token(t, actor, []string{string(auth.ScopeAdmin)})}
	body := fmt.Sprintf(`{"identity":{"tenant":%q,"user":%q,"session":%q}}`, actor.TenantID, actor.UserID, actor.SessionID)
	call := func(path, token, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		out := httptest.NewRecorder()
		mux.ServeHTTP(out, req)
		return out
	}
	head, err := st.Load(ctx, identity.Quadruple{Identity: seedID}, "events.durable.head")
	if err != nil {
		t.Fatal(err)
	}
	var persisted struct {
		Sequences []uint64 `json:"sequences"`
	}
	if err := json.Unmarshal(head.Bytes, &persisted); err != nil {
		t.Fatal(err)
	}
	if len(persisted.Sequences) != 50000 {
		t.Fatalf("durable head contains %d entries, want 50000", len(persisted.Sequences))
	}
	// No foreground writer is active here: exact counters cover every health
	// identity class independently, including an elevated fleet service token.
	for i, token := range tokens {
		before, published := st.calls.Load(), counted.publishes.Load()
		out := call("/v1/control/"+string(methods.MethodRuntimeHealth), token, body)
		if out.Code != http.StatusOK {
			t.Fatalf("health scope %d: %d %s", i, out.Code, out.Body.String())
		}
		if delta := st.calls.Load() - before; delta != 0 {
			t.Fatalf("health scope %d made %d StateStore calls", i, delta)
		}
		if delta := counted.publishes.Load() - published; delta != 0 {
			t.Fatalf("health scope %d published %d events", i, delta)
		}
		var h types.RuntimeHealth
		if err := json.Unmarshal(out.Body.Bytes(), &h); err != nil {
			t.Fatal(err)
		}
		if len(h.Retention) != 1 || h.Retention[0].Surface != "events" || h.Retention[0].Scope != types.RetentionScopeRuntime || h.Retention[0].OldestRetainedAt == nil || !h.Retention[0].OldestRetainedAt.Equal(seedAt) {
			t.Fatalf("health scope %d retention=%+v", i, h.Retention)
		}
	}
	// Liveness cannot be turned into an elevated cross-tenant read by a
	// body override, even for a verified elevated bearer.
	forgedBody := `{"identity":{"tenant":"h1-foreign","user":"h1-foreign","session":"h1-foreign"}}`
	for i, token := range tokens {
		before, published := st.calls.Load(), counted.publishes.Load()
		out := call("/v1/control/"+string(methods.MethodRuntimeHealth), token, forgedBody)
		if out.Code != http.StatusUnauthorized {
			t.Fatalf("cross-tenant health scope %d: %d %s", i, out.Code, out.Body.String())
		}
		if st.calls.Load() != before || counted.publishes.Load() != published {
			t.Fatalf("cross-tenant health scope %d touched store or published", i)
		}
	}
	// Own-scope events.list does not itself widen or emit an audit. Query the
	// verified actor's session after elevated health, so any health audit is
	// visible without an admin list introducing a fresh audit of its own.
	out := call("/v1/events/list", tokens[2], `{"filter":{}}`)
	if out.Code != http.StatusOK {
		t.Fatalf("events.list: %d %s", out.Code, out.Body.String())
	}
	var page types.EventsListResponse
	if err := json.Unmarshal(out.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != 0 {
		t.Fatalf("elevated health created %d actor events, want none", len(page.Events))
	}
	// Wait for SaveBatchIf admission, which is reached only after publishMu
	// acquisition. Keep it stalled a full five seconds even after the readers
	// finish; each of 50 simultaneous authenticated requests must beat 50 ms.
	st.stall.Store(true)
	publishDone := make(chan error, 1)
	go func() { publishDone <- bus.Publish(ctx, event) }()
	select {
	case <-st.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("publish did not reach blocked SaveBatchIf")
	}
	stalledAt := time.Now()
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(st.release) }) }
	t.Cleanup(release)
	timer := time.AfterFunc(5*time.Second, release)
	defer timer.Stop()
	before, published := st.calls.Load(), counted.publishes.Load()
	const readers = 50
	results := make(chan string, readers)
	var slowest atomic.Int64
	startGate := make(chan struct{})
	var wg sync.WaitGroup
	for i := range readers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-startGate
			start := time.Now()
			out := call("/v1/control/"+string(methods.MethodRuntimeHealth), tokens[i%len(tokens)], body)
			elapsed := time.Since(start)
			for previous := slowest.Load(); int64(elapsed) > previous; previous = slowest.Load() {
				if slowest.CompareAndSwap(previous, int64(elapsed)) {
					break
				}
			}
			if out.Code != http.StatusOK || elapsed >= 50*time.Millisecond {
				results <- fmt.Sprintf("reader %d: status=%d elapsed=%s body=%s", i, out.Code, elapsed, out.Body.String())
			}
		}(i)
	}
	close(startGate)
	wg.Wait()
	close(results)
	t.Logf("50 concurrent health requests: slowest %s during a five-second stalled publish", time.Duration(slowest.Load()))
	for result := range results {
		t.Error(result)
	}
	if delta := st.calls.Load() - before; delta != 0 {
		t.Errorf("stalled health made %d StateStore calls", delta)
	}
	if delta := counted.publishes.Load() - published; delta != 0 {
		t.Errorf("stalled health published %d events", delta)
	}
	if err := <-publishDone; err != nil {
		t.Fatalf("stalled publish: %v", err)
	}
	stallElapsed := time.Since(stalledAt)
	t.Logf("durable publish stall elapsed: %s", stallElapsed)
	if stallElapsed < 5*time.Second {
		t.Fatalf("publish was stalled only %s", stallElapsed)
	}
	// Denials must not scan or audit. Admin success must return all three
	// horizons and exactly one persisted event under the verified actor.
	for i := range 2 {
		before, published := st.calls.Load(), counted.publishes.Load()
		out := call("/v1/control/"+string(methods.MethodRuntimeRetention), tokens[i], body)
		var pe protoerrors.Error
		_ = json.Unmarshal(out.Body.Bytes(), &pe)
		if out.Code != http.StatusForbidden || pe.Code != protoerrors.CodeScopeMismatch {
			t.Fatalf("retention scope %d: %d %s", i, out.Code, out.Body.String())
		}
		if st.calls.Load() != before || counted.publishes.Load() != published {
			t.Fatalf("denied retention scope %d touched store or published", i)
		}
	}
	before, published = st.calls.Load(), counted.publishes.Load()
	_, trustErr := surface.Dispatch(context.Background(), methods.MethodRuntimeRetention, &types.RuntimeInfoRequest{Identity: types.IdentityScope{Tenant: actor.TenantID, User: actor.UserID, Session: actor.SessionID}})
	var trustProtocolErr *protoerrors.Error
	if !stderrors.As(trustErr, &trustProtocolErr) || trustProtocolErr.Code != protoerrors.CodeScopeMismatch {
		t.Fatalf("trust-mode retention: %v", trustErr)
	}
	if st.calls.Load() != before || counted.publishes.Load() != published {
		t.Fatal("trust-mode retention touched store or published")
	}
	published = counted.publishes.Load()
	out = call("/v1/control/"+string(methods.MethodRuntimeRetention), tokens[2], body)
	if out.Code != http.StatusOK {
		t.Fatalf("admin retention: %d %s", out.Code, out.Body.String())
	}
	var retention types.RuntimeRetention
	if err := json.Unmarshal(out.Body.Bytes(), &retention); err != nil {
		t.Fatal(err)
	}
	surfaces := map[string]bool{}
	for _, h := range retention.Retention {
		if h.Scope != types.RetentionScopeRuntime {
			t.Errorf("retention scope=%q", h.Scope)
		}
		surfaces[h.Surface] = true
	}
	if len(retention.Retention) != 3 || !surfaces["events"] || !surfaces["tasks"] || !surfaces["sessions"] {
		t.Fatalf("admin retention=%+v", retention.Retention)
	}
	if delta := counted.publishes.Load() - published; delta != 1 {
		t.Fatalf("admin retention published %d events, want one", delta)
	}
	audits, err := bus.(events.Replayer).Replay(ctx, events.Cursor{SessionID: actor.SessionID}, events.Filter{Tenant: actor.TenantID, User: actor.UserID, Session: actor.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	if len(audits) != 1 || audits[0].Type != events.EventTypeAdminScopeUsed || audits[0].Identity.Identity != actor {
		t.Fatalf("retention actor audit=%+v", audits)
	}
	// The decoded audit must carry the method so operators can distinguish
	// runtime retention fan-in from other elevated reads.
	payload, ok := audits[0].Payload.(events.RedactedMap)
	if !ok {
		t.Fatalf("audit payload=%T", audits[0].Payload)
	}
	if payload.Data["Tenant"] != actor.TenantID || payload.Data["User"] != actor.UserID || payload.Data["Session"] != actor.SessionID {
		t.Fatalf("audit actor payload=%v", payload.Data)
	}
	if payload.Data["method"] != string(methods.MethodRuntimeRetention) {
		t.Fatalf("audit method=%v", payload.Data["method"])
	}
}

func (s *h1CountingStore) Save(ctx context.Context, r state.StateRecord) error {
	s.calls.Add(1)
	return s.StateStore.Save(ctx, r)
}

func (s *h1CountingStore) SaveIf(ctx context.Context, e []state.SlotExpectation, r state.StateRecord) error {
	s.calls.Add(1)
	return s.StateStore.SaveIf(ctx, e, r)
}

func (s *h1CountingStore) DeleteIf(ctx context.Context, e state.SlotExpectation) (bool, error) {
	s.calls.Add(1)
	return s.StateStore.DeleteIf(ctx, e)
}

func (s *h1CountingStore) FenceIf(ctx context.Context, e state.SlotExpectation, fn func() error) error {
	s.calls.Add(1)
	return s.StateStore.FenceIf(ctx, e, fn)
}

func (s *h1CountingStore) Load(ctx context.Context, id identity.Quadruple, kind string) (state.StateRecord, error) {
	s.calls.Add(1)
	return s.StateStore.Load(ctx, id, kind)
}

func (s *h1CountingStore) LoadByEventID(ctx context.Context, id state.EventID) (state.StateRecord, error) {
	s.calls.Add(1)
	return s.StateStore.LoadByEventID(ctx, id)
}

func (s *h1CountingStore) Delete(ctx context.Context, id identity.Quadruple, kind string) error {
	s.calls.Add(1)
	return s.StateStore.Delete(ctx, id, kind)
}

func (s *h1CountingStore) DeleteScope(ctx context.Context, id identity.Identity) (int, error) {
	s.calls.Add(1)
	return s.StateStore.DeleteScope(ctx, id)
}

func (s *h1CountingStore) ListKind(ctx context.Context, scope state.ListScope, prefix string) ([]state.StateRecord, error) {
	s.calls.Add(1)
	return s.StateStore.ListKind(ctx, scope, prefix)
}

func (s *h1CountingStore) ListKindBounded(ctx context.Context, scope state.ListScope, prefix string, limit int) ([]state.StateRecord, error) {
	s.calls.Add(1)
	return s.StateStore.ListKindBounded(ctx, scope, prefix, limit)
}

func (s *h1CountingStore) ListKindForIdentity(ctx context.Context, id identity.Quadruple, prefix string) ([]state.StateRecord, error) {
	s.calls.Add(1)
	return s.StateStore.ListKindForIdentity(ctx, id, prefix)
}

func (s *h1CountingStore) ListKindForIdentityBounded(ctx context.Context, id identity.Quadruple, prefix string, limit int) ([]state.StateRecord, error) {
	s.calls.Add(1)
	return s.StateStore.ListKindForIdentityBounded(ctx, id, prefix, limit)
}

func (s *h1CountingStore) ScanKindForTenant(ctx context.Context, scope state.ListScope, tenant, prefix string, limit int, cursor string) (state.StateScanPage, error) {
	s.calls.Add(1)
	return s.StateStore.ScanKindForTenant(ctx, scope, tenant, prefix, limit, cursor)
}

func (s *h1CountingStore) Close(ctx context.Context) error {
	s.calls.Add(1)
	return s.StateStore.Close(ctx)
}

// h1SeedDurableHead expands a canonical one-event fixture without making
// thousands of growing-head appends. It persists all bodies in bounded atomic
// store batches, authenticates the matching metadata projection, then boots a
// new real bus so production recovery validates and adopts the 50k head.
func h1SeedDurableHead(t *testing.T, st state.StateStore) (events.EventBus, events.Event) {
	t.Helper()
	ctx := t.Context()
	cfg := phase266EventsConfig()
	bus, err := durable.New(ctx, cfg, auditpatterns.New(), st)
	if err != nil {
		t.Fatal(err)
	}
	event := events.Event{Type: events.EventTypeRuntimeWarning, Identity: identity.Quadruple{Identity: identity.Identity{TenantID: "h1-seed", UserID: "h1-seed", SessionID: "h1-50000"}}, OccurredAt: time.Now().UTC().Add(-time.Hour), Payload: events.SubscriptionIdleClosedPayload{SubscriberID: 1}}
	if err := bus.Publish(ctx, event); err != nil {
		_ = bus.Close(context.Background())
		t.Fatal(err)
	}
	if err := bus.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	headRecord, err := st.Load(ctx, event.Identity, "events.durable.head")
	if err != nil {
		t.Fatal(err)
	}
	entries, err := st.ListKindForIdentity(ctx, event.Identity, "events.durable.entry/")
	if err != nil || len(entries) != 1 {
		t.Fatalf("canonical entry fixture: %d entries, %v", len(entries), err)
	}
	var head struct {
		Sequences                 []uint64          `json:"sequences"`
		Metadata                  []json.RawMessage `json:"metadata,omitempty"`
		MetadataReady             bool              `json:"metadata_ready,omitempty"`
		MetadataValidatedCount    int               `json:"metadata_validated_count,omitempty"`
		MetadataIntegrityChecksum string            `json:"metadata_integrity_checksum,omitempty"`
	}
	if err := json.Unmarshal(headRecord.Bytes, &head); err != nil {
		t.Fatal(err)
	}
	if len(head.Metadata) != 1 || !head.MetadataReady {
		t.Fatalf("canonical metadata fixture not ready: %+v", head)
	}
	start := time.Now()
	metadataTemplate := head.Metadata[0]
	head.Sequences = make([]uint64, 50000)
	head.Metadata = make([]json.RawMessage, 50000)
	for i := range head.Sequences {
		seq := uint64(i + 1)
		head.Sequences[i] = seq
		head.Metadata[i] = bytes.Replace(metadataTemplate, []byte(`"sequence":1`), fmt.Appendf(nil, `"sequence":%d`, seq), 1)
	}
	// Bodies preserve the canonical template's identity, time, type and
	// payload. Only their sequence, storage slot and idempotency ID differ.
	const batchSize = 1000
	prefix := entries[0].Kind[:strings.LastIndexByte(entries[0].Kind, '/')+1]
	for first := uint64(2); first <= 50000; first += batchSize {
		last := min(first+batchSize, 50001)
		writes := make([]state.StateRecord, 0, last-first)
		expected := make([]state.SlotExpectation, 0, last-first)
		for seq := first; seq < last; seq++ {
			entry := entries[0]
			entry.ID = state.NewEventID()
			entry.Kind = prefix + fmt.Sprintf("%020d", seq)
			entry.Bytes = bytes.Replace(entries[0].Bytes, []byte(`"sequence":1`), fmt.Appendf(nil, `"sequence":%d`, seq), 1)
			writes = append(writes, entry)
			expected = append(expected, state.SlotExpectation{Identity: entry.Identity, Kind: entry.Kind})
		}
		if err := st.SaveBatchIf(ctx, expected, writes); err != nil {
			t.Fatalf("seed body batch %d: %v", first, err)
		}
	}
	digest, err := json.Marshal(struct {
		Sequences []uint64          `json:"sequences"`
		Metadata  []json.RawMessage `json:"metadata"`
	}{head.Sequences, head.Metadata})
	if err != nil {
		t.Fatal(err)
	}
	head.MetadataIntegrityChecksum = fmt.Sprintf("%x", sha256.Sum256(digest))
	head.MetadataValidatedCount = len(head.Sequences)
	headRecord.Bytes, err = json.Marshal(head)
	if err != nil {
		t.Fatal(err)
	}
	previousID := headRecord.ID
	headRecord.ID = state.NewEventID()
	if err := st.SaveIf(ctx, []state.SlotExpectation{{Identity: headRecord.Identity, Kind: headRecord.Kind, ExpectedEventID: previousID}}, headRecord); err != nil {
		t.Fatal(err)
	}
	reopened, err := durable.New(ctx, cfg, auditpatterns.New(), st)
	if err != nil {
		t.Fatalf("recover seeded 50k head: %v", err)
	}
	lo, hi, truncated, err := reopened.(events.HistoryReplayer).Bounds(ctx, events.Filter{Tenant: event.Identity.TenantID, User: event.Identity.UserID, Session: event.Identity.SessionID})
	if err != nil || lo != 1 || hi != 50000 || truncated {
		_ = reopened.Close(context.Background())
		t.Fatalf("seed bounds=(%d,%d,%v), want (1,50000,nil)", lo, hi, err)
	}
	t.Logf("seeded and recovered 50,000 durable entries in %s", time.Since(start))
	return reopened, event
}
