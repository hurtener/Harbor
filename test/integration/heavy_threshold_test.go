// heavy_threshold_test.go — phase 213 (D-358), the §17.1 integration
// gate for the heavy-content threshold split by purpose.
//
// The phase raises the LLM-CONTEXT arm to 128 KiB and PINS every
// Console-facing arm at 32 KiB behind its own constant. Both halves are
// only real if they hold through the actual runtime WIRING, so this
// test drives the surfaces through `serve.BuildMux` — the same builder
// `cmd/harbor` and the test kit call — rather than constructing the
// handlers by hand.
//
// Surfaces composed, all with REAL drivers (CLAUDE.md §17.3):
//
//   - internal/runtime/dispatch — the promote-to-stub boundary that
//     FOLLOWS the raise.
//   - internal/protocol/transports/stream — `pause.list` + `memory.get`
//     / `memory.list`, the Console reads that PIN.
//   - internal/artifacts (inmem), internal/state (inmem),
//     internal/events (inmem), internal/memory (inmem),
//     internal/audit/drivers/patterns.
//
// Three arms: the default configuration, the DECOUPLING arm (an
// operator who raises `heavy_output_threshold_bytes` gets a wider
// prompt budget and UNCHANGED Console bounds), and a forced
// artifact-store failure. Identity propagation is asserted on every
// arm; a concurrency stress across four tenants closes it.
package integration_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"

	"github.com/hurtener/Harbor/internal/artifacts"
	_ "github.com/hurtener/Harbor/internal/artifacts/drivers/inmem"
	"github.com/hurtener/Harbor/internal/audit"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	_ "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/memory"
	memoryinmem "github.com/hurtener/Harbor/internal/memory/drivers/inmem"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/protocol"
	"github.com/hurtener/Harbor/internal/protocol/transports/stream"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/runtime/dispatch"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
	"github.com/hurtener/Harbor/internal/runtime/serve"
	"github.com/hurtener/Harbor/internal/runtime/steering"
	"github.com/hurtener/Harbor/internal/state"
	_ "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	"github.com/hurtener/Harbor/internal/tasks"
	_ "github.com/hurtener/Harbor/internal/tasks/drivers/inprocess"
	"github.com/hurtener/Harbor/internal/telemetry"
	"github.com/hurtener/Harbor/internal/tools"
)

// htConsoleBand is the size that DISTINGUISHES the two arms: above the
// pinned 32 KiB Console bound, below the raised 128 KiB prompt bound.
const htConsoleBand = 64 * 1024

type htRig struct {
	mux      http.Handler
	memory   memory.MemoryStore
	coord    pauseresume.Coordinator
	artifact artifacts.ArtifactStore
	cfg      *config.Config
}

// newHeavyThresholdRig assembles the Protocol surface through the REAL
// builder. `operatorThreshold` of 0 leaves the default in place; a
// non-zero value is the operator's explicit
// `artifacts.heavy_output_threshold_bytes`.
func newHeavyThresholdRig(t *testing.T, operatorThreshold int) htRig {
	t.Helper()
	red := auditpatterns.New()

	bus, err := events.Open(context.Background(), config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 64,
		SubscriberBufferSize:     512,
		IdleTimeout:              60 * time.Second,
		DropWindow:               time.Second,
		ReplayBufferSize:         512,
	}, red)
	if err != nil {
		t.Fatalf("events.Open: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })

	st, err := state.Open(context.Background(), config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close(context.Background()) })

	memStore, err := memoryinmem.New(memory.ConfigSnapshot{
		Driver:       "inmem",
		Strategy:     memory.StrategyTruncation,
		BudgetTokens: 100_000_000,
	}, memory.Deps{State: st, Bus: bus}, memoryinmem.Options{})
	if err != nil {
		t.Fatalf("memoryinmem.New: %v", err)
	}
	t.Cleanup(func() { _ = memStore.Close(context.Background()) })

	artStore, err := artifacts.Open(context.Background(), config.ArtifactsConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("artifacts.Open: %v", err)
	}
	t.Cleanup(func() { _ = artStore.Close(context.Background()) })

	taskReg, err := tasks.Open(context.Background(), tasks.Dependencies{
		Store:    st,
		Bus:      bus,
		Redactor: audit.Redactor(red),
		Cfg:      config.TasksConfig{Driver: "inprocess"},
	})
	if err != nil {
		t.Fatalf("tasks.Open: %v", err)
	}
	t.Cleanup(func() { _ = taskReg.Close(context.Background()) })

	surface, err := protocol.NewControlSurface(taskReg, steering.NewRegistry())
	if err != nil {
		t.Fatalf("NewControlSurface: %v", err)
	}
	metricsReg, metricsShutdown, err := telemetry.NewMetricsRegistry(
		config.TelemetryConfig{ServiceName: "heavy-threshold-test"},
		telemetry.WithMetricReader(sdkmetric.NewManualReader()))
	if err != nil {
		t.Fatalf("NewMetricsRegistry: %v", err)
	}
	t.Cleanup(func() { _ = metricsShutdown(context.Background()) })

	cfg := config.Defaults()
	if operatorThreshold > 0 {
		cfg.Artifacts.HeavyOutputThresholdBytes = operatorThreshold
	}
	coord := pauseresume.New(pauseresume.WithBus(bus))

	built, err := serve.BuildMux(serve.MuxInput{
		Cfg:          cfg,
		Surface:      surface,
		Bus:          bus,
		Redactor:     red,
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		Metrics:      metricsReg,
		Tasks:        taskReg,
		State:        st,
		Memory:       memStore,
		Artifacts:    artStore,
		Coordinator:  coord,
		Validator:    nil, // WithoutValidator — identity rides the carrier headers.
		DisplayName:  "heavy-threshold",
		InstanceID:   "heavy-threshold",
		BuildVersion: "test",
		BuildCommit:  "test",
	})
	if err != nil {
		t.Fatalf("BuildMux: %v", err)
	}
	return htRig{mux: built.Mux, memory: memStore, coord: coord, artifact: artStore, cfg: cfg}
}

func htPost(t *testing.T, srvURL, route string, id identity.Identity, body string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srvURL+route, strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(stream.HeaderTenant, id.TenantID)
	req.Header.Set(stream.HeaderUser, id.UserID)
	req.Header.Set(stream.HeaderSession, id.SessionID)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

// htSeedHeavyTurn appends a conversation turn whose assistant response
// is `size` bytes, so the projected memory row lands above the Console
// inline-payload bound.
func htSeedHeavyTurn(t *testing.T, store memory.MemoryStore, id identity.Identity, size int) {
	t.Helper()
	ctx, err := identity.With(context.Background(), id)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	if err := store.AddTurn(ctx, identity.Quadruple{Identity: id}, memory.ConversationTurn{
		UserMessage:       "how big is it?",
		AssistantResponse: strings.Repeat("m", size),
		Timestamp:         time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("AddTurn: %v", err)
	}
}

func htSeedPause(t *testing.T, coord pauseresume.Coordinator, id identity.Identity, runID string, size int) {
	t.Helper()
	ctx, err := identity.WithRun(context.Background(), id, runID)
	if err != nil {
		t.Fatalf("identity.WithRun: %v", err)
	}
	if _, err := coord.Request(ctx, pauseresume.PauseRequest{
		Identity: id,
		Reason:   pauseresume.ReasonApprovalRequired,
		Payload:  map[string]any{"blob": strings.Repeat("p", size)},
	}); err != nil {
		t.Fatalf("coord.Request: %v", err)
	}
}

// htExecutor builds a dispatch executor over the rig's REAL artifact
// store, threaded exactly as `assemble.go` threads it (the operator
// field — the arm that FOLLOWS the raise).
func htExecutor(t *testing.T, rig htRig, cat tools.ToolCatalog, store artifacts.ArtifactStore) steering.ToolExecutor {
	t.Helper()
	return dispatch.NewToolExecutor(cat, store, nil,
		dispatch.WithHeavyThreshold(rig.cfg.Artifacts.HeavyOutputThresholdBytes),
		dispatch.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
}

func htCatalog(t *testing.T, size int) tools.ToolCatalog {
	t.Helper()
	cat := tools.NewCatalog()
	blob := strings.Repeat("d", size)
	if err := cat.Register(tools.ToolDescriptor{
		Tool: tools.Tool{Name: "bulk"},
		Invoke: func(_ context.Context, _ json.RawMessage) (tools.ToolResult, error) {
			return tools.ToolResult{Value: map[string]any{"blob": blob}}, nil
		},
	}); err != nil {
		t.Fatalf("Register bulk: %v", err)
	}
	return cat
}

func htRunTool(t *testing.T, exec steering.ToolExecutor, cat tools.ToolCatalog, id identity.Identity, runID string) map[string]any {
	t.Helper()
	q := identity.Quadruple{Identity: id, RunID: runID}
	ctx, err := identity.WithRun(context.Background(), id, runID)
	if err != nil {
		t.Fatalf("identity.WithRun: %v", err)
	}
	_, llmObs, err := exec.ExecuteDecision(ctx, planner.RunContext{Quadruple: q, Catalog: tools.NewPlannerView(cat, tools.CatalogFilter{TenantID: q.TenantID, UserID: q.UserID, SessionID: q.SessionID})},
		planner.CallTool{Tool: "bulk", Args: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatalf("ExecuteDecision: %v", err)
	}
	obs, ok := llmObs.(map[string]any)
	if !ok {
		t.Fatalf("llmObservation type = %T, want a map", llmObs)
	}
	return obs
}

// TestE2E_HeavyThreshold_DefaultConfiguration_RaiseAndPinsTogether —
// arm 1. On a stock configuration the prompt-bound arm inlines the
// 32–128 KiB band while every Console-facing read keeps shipping a
// reference at 32 KiB. Both properties are asserted in ONE stack, which
// is the only place their independence is observable.
func TestE2E_HeavyThreshold_DefaultConfiguration_RaiseAndPinsTogether(t *testing.T) {
	rig := newHeavyThresholdRig(t, 0)
	srv := httptest.NewServer(rig.mux)
	defer srv.Close()

	id := identity.Identity{TenantID: "tenant-ht-default", UserID: "u-d", SessionID: "s-d"}

	// --- the RAISE: a 64 KiB tool result reaches the planner inline and
	// writes NOTHING to the artifact store.
	inlineStore, err := artifacts.Open(context.Background(), config.ArtifactsConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("artifacts.Open: %v", err)
	}
	defer func() { _ = inlineStore.Close(context.Background()) }()
	cat := htCatalog(t, htConsoleBand)
	obs := htRunTool(t, htExecutor(t, rig, cat, inlineStore), cat, id, "run-inline")
	if obs["truncated"] == true {
		t.Fatalf("a %d-byte tool result was promoted on the default configuration", htConsoleBand)
	}
	refs, err := inlineStore.List(context.Background(), artifacts.ArtifactScope{TenantID: id.TenantID})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(refs) != 0 {
		t.Errorf("the inlined band wrote %d artifacts, want 0", len(refs))
	}

	// --- still promoted above the band, and the stub is resolvable
	// UNDER THE WRITING SCOPE ONLY (identity propagation).
	promoteStore, err := artifacts.Open(context.Background(), config.ArtifactsConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("artifacts.Open: %v", err)
	}
	defer func() { _ = promoteStore.Close(context.Background()) }()
	cat = htCatalog(t, 256*1024)
	obs = htRunTool(t, htExecutor(t, rig, cat, promoteStore), cat, id, "run-promote")
	if obs["truncated"] != true {
		t.Fatalf("a 256 KiB tool result was NOT promoted: %#v", obs)
	}
	refID, _ := obs["artifact_ref"].(string)
	if refID == "" {
		t.Fatalf("promoted summary carries no artifact_ref: %#v", obs)
	}
	ownScope := artifacts.ArtifactScope{TenantID: id.TenantID, UserID: id.UserID, SessionID: id.SessionID}
	if _, found, gErr := promoteStore.GetRef(context.Background(), ownScope, refID); gErr != nil || !found {
		t.Fatalf("GetRef under the writing scope = found=%v err=%v, want the stub resolvable", found, gErr)
	}
	otherScope := artifacts.ArtifactScope{TenantID: "tenant-ht-intruder", UserID: "u-x", SessionID: "s-x"}
	if _, found, _ := promoteStore.GetRef(context.Background(), otherScope, refID); found {
		t.Error("a promoted stub resolved across tenants — the isolation boundary leaked")
	}

	// --- the PINS: every Console read still ships a reference at 64 KiB.
	htAssertConsoleArmsOffload(t, srv.URL, rig, id, "run-pause-default")
}

// TestE2E_HeavyThreshold_DecouplingArm_OperatorRaiseDoesNotWidenTheConsole
// — arm 2, and the arm that FAILS if `mux.go` keeps threading
// `cfg.Artifacts.HeavyOutputThresholdBytes` into the pause-list /
// memory wiring. The operator asks for a 256 KiB prompt budget; they
// get it for the planner and NOT for the browser.
func TestE2E_HeavyThreshold_DecouplingArm_OperatorRaiseDoesNotWidenTheConsole(t *testing.T) {
	const operator = 256 * 1024
	rig := newHeavyThresholdRig(t, operator)
	srv := httptest.NewServer(rig.mux)
	defer srv.Close()

	id := identity.Identity{TenantID: "tenant-ht-decoupled", UserID: "u-x", SessionID: "s-x"}

	// The operator's value won on the prompt arm: a 200 KiB result —
	// far past the 128 KiB default — inlines.
	store, err := artifacts.Open(context.Background(), config.ArtifactsConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("artifacts.Open: %v", err)
	}
	defer func() { _ = store.Close(context.Background()) }()
	cat := htCatalog(t, 200*1024)
	obs := htRunTool(t, htExecutor(t, rig, cat, store), cat, id, "run-operator")
	if obs["truncated"] == true {
		t.Fatalf("a 200 KiB result was promoted under an operator threshold of %d", operator)
	}

	// The pins held: the Console reads are STILL bounded at 32 KiB.
	htAssertConsoleArmsOffload(t, srv.URL, rig, id, "run-pause-decoupled")
}

// htAssertConsoleArmsOffload drives `memory.get`, `memory.list` and
// `pause.list` with 64 KiB payloads and asserts every one selects the
// by-reference arm. memory.get and memory.list are asserted TOGETHER
// because the two are documented to agree.
func htAssertConsoleArmsOffload(t *testing.T, srvURL string, rig htRig, id identity.Identity, runID string) {
	t.Helper()

	htSeedHeavyTurn(t, rig.memory, id, htConsoleBand)

	status, body := htPost(t, srvURL, "/v1/memory/list", id, `{}`)
	if status != http.StatusOK {
		t.Fatalf("memory.list: status = %d, body = %s", status, body)
	}
	var listResp prototypes.MemoryListResponse
	if err := json.Unmarshal(body, &listResp); err != nil {
		t.Fatalf("decode memory.list: %v", err)
	}
	if len(listResp.Items) == 0 {
		t.Fatal("memory.list returned no rows")
	}
	var heavyKey string
	for _, item := range listResp.Items {
		if item.HeavyContent {
			heavyKey = item.Key
			break
		}
	}
	if heavyKey == "" {
		t.Fatalf("no memory row flagged HeavyContent at %d bytes — the Console inline bound (%d) "+
			"followed the LLM-context threshold (%d)",
			htConsoleBand, config.DefaultConsoleInlinePayloadBytes, rig.cfg.Artifacts.HeavyOutputThresholdBytes)
	}

	status, body = htPost(t, srvURL, "/v1/memory/get", id,
		fmt.Sprintf(`{"key":%q}`, heavyKey))
	if status != http.StatusOK {
		t.Fatalf("memory.get: status = %d, body = %s", status, body)
	}
	var getResp prototypes.MemoryGetResponse
	if err := json.Unmarshal(body, &getResp); err != nil {
		t.Fatalf("decode memory.get: %v", err)
	}
	if getResp.Detail.ValueArtifact == nil {
		t.Error("memory.get: ValueArtifact = nil, want the by-reference arm")
	}
	if len(getResp.Detail.Value) != 0 {
		t.Errorf("memory.get: inline Value carries %d bytes; EXACTLY ONE arm may be populated",
			len(getResp.Detail.Value))
	}

	htSeedPause(t, rig.coord, id, runID, htConsoleBand)
	status, body = htPost(t, srvURL, "/v1/pause/list", id, `{}`)
	if status != http.StatusOK {
		t.Fatalf("pause.list: status = %d, body = %s", status, body)
	}
	var pauseResp prototypes.PauseListResponse
	if err := json.Unmarshal(body, &pauseResp); err != nil {
		t.Fatalf("decode pause.list: %v", err)
	}
	if len(pauseResp.Snapshots) == 0 {
		t.Fatal("pause.list returned no snapshots")
	}
	snap := pauseResp.Snapshots[0]
	if snap.PayloadRef == nil {
		t.Errorf("pause.list: PayloadRef = nil at %d bytes — the pin did not hold", htConsoleBand)
	}
	if snap.Payload != nil {
		t.Errorf("pause.list: inline Payload populated alongside the ref: %+v", snap.Payload)
	}
}

// htFailingStore forces the PutText failure mode. It wraps the REAL
// store — the failure is injected, the behaviour under it is not
// re-implemented (CLAUDE.md §17.4).
type htFailingStore struct{ artifacts.ArtifactStore }

var errHTPutText = errors.New("heavy-threshold test: forced PutText failure")

func (htFailingStore) PutText(context.Context, artifacts.ArtifactScope, string, artifacts.PutOpts) (artifacts.ArtifactRef, error) {
	return artifacts.ArtifactRef{}, errHTPutText
}

// TestE2E_HeavyThreshold_ArtifactStoreFailure_DegradesLoudlyNeverInlines
// — arm 3. When the store cannot take the promoted bytes, dispatch
// degrades to the truncation summary WITHOUT an artifact ref and
// WITHOUT inlining the heavy value. Silent inlining would defeat the
// whole boundary (CLAUDE.md §13).
func TestE2E_HeavyThreshold_ArtifactStoreFailure_DegradesLoudlyNeverInlines(t *testing.T) {
	rig := newHeavyThresholdRig(t, 0)
	id := identity.Identity{TenantID: "tenant-ht-failure", UserID: "u-f", SessionID: "s-f"}

	real, err := artifacts.Open(context.Background(), config.ArtifactsConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("artifacts.Open: %v", err)
	}
	defer func() { _ = real.Close(context.Background()) }()

	cat := htCatalog(t, 256*1024)
	obs := htRunTool(t, htExecutor(t, rig, cat, htFailingStore{real}), cat, id, "run-failure")
	if obs["truncated"] != true {
		t.Fatalf("the store failure did not degrade to a truncation summary: %#v", obs)
	}
	if _, hasRef := obs["artifact_ref"]; hasRef {
		t.Error("a failed PutText still reported an artifact_ref")
	}
	preview, _ := obs["preview"].(string)
	if preview == "" {
		t.Error("the degraded summary carries no preview — the elision is invisible to the operator")
	}
	if len(preview) >= config.DefaultHeavyOutputThresholdBytes {
		t.Errorf("the degraded preview is %d bytes — the heavy value was inlined after all", len(preview))
	}
	refs, err := real.List(context.Background(), artifacts.ArtifactScope{TenantID: id.TenantID})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(refs) != 0 {
		t.Errorf("the failing store recorded %d artifacts, want 0", len(refs))
	}
}

// TestE2E_HeavyThreshold_ConcurrentTenants_NoCrossTalkNoLeak — the
// §17.3 concurrency requirement: N=16 runs across four tenants, half in
// the newly-inlined band and half above it, against ONE shared
// executor and ONE shared store. Asserts per-run correctness, per-tenant
// artifact isolation, and a restored goroutine baseline.
func TestE2E_HeavyThreshold_ConcurrentTenants_NoCrossTalkNoLeak(t *testing.T) {
	baseline := runtime.NumGoroutine()

	rig := newHeavyThresholdRig(t, 0)
	store, err := artifacts.Open(context.Background(), config.ArtifactsConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("artifacts.Open: %v", err)
	}
	defer func() { _ = store.Close(context.Background()) }()

	// Two shared executors — one per size band — exercised concurrently.
	inlineCat := htCatalog(t, htConsoleBand)
	promoteCat := htCatalog(t, 256*1024)
	inlineExec := htExecutor(t, rig, inlineCat, store)
	promoteExec := htExecutor(t, rig, promoteCat, store)

	const n = 16
	var wg sync.WaitGroup
	errCh := make(chan error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tenant := "tenant-ht-conc-" + strconv.Itoa(i%4)
			id := identity.Identity{
				TenantID:  tenant,
				UserID:    "u-" + strconv.Itoa(i),
				SessionID: "s-" + strconv.Itoa(i),
			}
			q := identity.Quadruple{Identity: id, RunID: "run-conc-" + strconv.Itoa(i)}
			ctx, cErr := identity.WithRun(context.Background(), id, q.RunID)
			if cErr != nil {
				errCh <- cErr
				return
			}
			exec := inlineExec
			wantPromoted := false
			if i%2 == 1 {
				exec, wantPromoted = promoteExec, true
			}
			cat := inlineCat
			if wantPromoted {
				cat = promoteCat
			}
			_, llmObs, eErr := exec.ExecuteDecision(ctx, planner.RunContext{Quadruple: q, Catalog: tools.NewPlannerView(cat, tools.CatalogFilter{TenantID: q.TenantID, UserID: q.UserID, SessionID: q.SessionID})},
				planner.CallTool{Tool: "bulk", Args: json.RawMessage(`{}`)})
			if eErr != nil {
				errCh <- eErr
				return
			}
			obs, ok := llmObs.(map[string]any)
			if !ok {
				errCh <- fmt.Errorf("run %d: observation type %T", i, llmObs)
				return
			}
			if got := obs["truncated"] == true; got != wantPromoted {
				errCh <- fmt.Errorf("run %d (tenant %s): promoted=%v, want %v — band cross-talk",
					i, tenant, got, wantPromoted)
				return
			}
			if !wantPromoted {
				// The inlined arm must carry ITS OWN payload, not a
				// sibling run's.
				if blob, _ := obs["blob"].(string); len(blob) != htConsoleBand {
					errCh <- fmt.Errorf("run %d: inlined blob is %d bytes, want %d",
						i, len(blob), htConsoleBand)
				}
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}

	// Per-tenant isolation: every artifact written landed under the
	// writing tenant only.
	for tenantIdx := range 4 {
		tenant := "tenant-ht-conc-" + strconv.Itoa(tenantIdx)
		refs, lErr := store.List(context.Background(), artifacts.ArtifactScope{TenantID: tenant})
		if lErr != nil {
			t.Fatalf("List(%s): %v", tenant, lErr)
		}
		for _, ref := range refs {
			if ref.Scope.TenantID != tenant {
				t.Errorf("artifact %q listed under %s but scoped to %s",
					ref.ID, tenant, ref.Scope.TenantID)
			}
		}
	}

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if runtime.NumGoroutine() <= baseline+10 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("goroutine leak: NumGoroutine=%d, baseline=%d", runtime.NumGoroutine(), baseline)
}
