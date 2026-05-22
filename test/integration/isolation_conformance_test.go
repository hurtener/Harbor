// Phase 76 — the master cross-tenant + cross-session isolation
// conformance harness (RFC §4.3, D-001, D-025, D-134).
//
// # What this is
//
// The previous shipped phases each ship their OWN per-subsystem
// isolation tests (`state/conformancetest`, `memory/conformancetest`,
// `artifacts` isolation tests, …). This harness does NOT re-test each
// subsystem in isolation — it is the integrity gate that proves the
// SIX identity-scoped subsystems hold the multi-isolation invariant
// SIMULTANEOUSLY, under concurrent load, against ONE shared instance
// of each driver:
//
//   - StateStore   (Phase 07)
//   - ArtifactStore(Phase 17)
//   - MemoryStore  (Phase 23)
//   - SkillStore   (Phase 37)
//   - TaskRegistry (Phase 20)
//   - EventBus     (Phase 05)
//
// # The invariant (RFC §4.3, the binding acceptance criterion)
//
// Every read returns ONLY data whose identity tuple
// `(tenant, user, session)` exactly matches the caller's identity.
// Zero cross-tenant bleed. Zero cross-session bleed. A regression
// here is a security bug, not a test flake (master-plan Phase 76
// "Risks").
//
// # How it proves the invariant
//
// `runIsolationSoak` spins ~100 concurrent sessions spread across
// multiple tenants and users. Each session-worker, for its whole
// lifetime, loops over a randomized mix of writes-then-reads against
// all six subsystems — every write stamps the worker's identity into
// the VALUE (not just the key), so a cross-scope read that returned
// another worker's row surfaces as an identity mismatch in the value
// payload, not merely as a count discrepancy. After every read the
// worker asserts the recovered identity matches its own verbatim;
// any breach increments a shared failure counter and the harness
// fails loudly.
//
// # Real drivers at the seam (CLAUDE.md §17.3 #1, §17.4)
//
// Every subsystem is opened through its production registry factory
// (`state.Open`, `artifacts.Open`, `memory.Open`, `skills.OpenDriver`,
// `tasks.Open`, `events.Open`) against its real V1 in-memory driver.
// The SkillStore additionally runs against the real `localdb` SQLite
// driver (`:memory:` DSN) — it has no in-memory-only driver, and the
// SQLite path is the one operators ship. No mocks anywhere on the
// boundary.
//
// # CI-fast default vs. the 30 s soak (D-134)
//
// The master plan specifies "100 sessions × random ops × 30 s under
// -race". A 30 s soak on every PR would dominate CI wall-clock. The
// split (D-134):
//
//   - DEFAULT (every PR, `make test`): 100 sessions, a ~3 s soak
//     window. Fast enough to gate every PR; broad enough that a
//     cross-scope leak surfaces with overwhelming probability.
//   - FULL SOAK: set `HARBOR_ISOLATION_SOAK=30s` (any Go duration)
//     to run the master-plan-specified 30 s window. `-short` forces
//     the fast window regardless. The dedicated `isolation` CI job
//     runs the default fast window on every PR.
//
// Both windows exercise the identical code path — only the soak
// duration changes. There is no "with-flag / without-flag" parallel
// implementation (CLAUDE.md §13).
package integration_test

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/artifacts"
	_ "github.com/hurtener/Harbor/internal/artifacts/drivers/inmem"
	"github.com/hurtener/Harbor/internal/audit"
	_ "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	_ "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/memory"
	_ "github.com/hurtener/Harbor/internal/memory/drivers/inmem"
	"github.com/hurtener/Harbor/internal/skills"
	_ "github.com/hurtener/Harbor/internal/skills/drivers/localdb"
	"github.com/hurtener/Harbor/internal/state"
	_ "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	"github.com/hurtener/Harbor/internal/tasks"
	_ "github.com/hurtener/Harbor/internal/tasks/drivers/inprocess"
)

// isolationSessionCount is the number of concurrent session-workers
// the harness spins. The master plan pins "100 sessions"; the
// constant makes the intent explicit and greppable.
const isolationSessionCount = 100

// isolationFastWindow is the every-PR soak duration. Short enough to
// keep CI fast; with 100 workers each doing thousands of randomized
// op-cycles, a cross-scope leak surfaces with overwhelming
// probability inside it.
const isolationFastWindow = 3 * time.Second

// isolationSoakEnvVar overrides the soak window with a Go duration
// string. Documented in docs/plans/phase-76-*.md and D-134.
const isolationSoakEnvVar = "HARBOR_ISOLATION_SOAK"

// isolationTenants / isolationUsersPerTenant fan the 100 workers
// across a grid of identities so the harness exercises BOTH the
// cross-tenant boundary (different tenant) AND the cross-session
// boundary (same tenant+user, different session). 5 tenants × 4
// users × 5 sessions = 100 distinct (tenant, user, session) triples.
const (
	isolationTenants         = 5
	isolationUsersPerTenant  = 4
	isolationSessionsPerUser = 5
)

// isolationStores bundles the six shared subsystem instances the
// harness drives. Every worker shares ONE of each — that is the
// point: the harness proves a single shared driver instance keeps N
// concurrent identities isolated (D-025 + D-001 composed).
type isolationStores struct {
	state     state.StateStore
	artifacts artifacts.ArtifactStore
	memory    memory.MemoryStore
	skillsInM skills.SkillStore // localdb SQLite :memory: — the only V1 SkillStore driver
	tasks     tasks.TaskRegistry
	bus       events.EventBus
}

// closeAll tears down every store, honoring ctx. Errors are reported
// via t.Errorf — a Close failure is a real defect, never swallowed.
func (s *isolationStores) closeAll(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	if err := s.state.Close(ctx); err != nil {
		t.Errorf("state.Close: %v", err)
	}
	if err := s.artifacts.Close(ctx); err != nil {
		t.Errorf("artifacts.Close: %v", err)
	}
	if err := s.memory.Close(ctx); err != nil {
		t.Errorf("memory.Close: %v", err)
	}
	if err := s.skillsInM.Close(ctx); err != nil {
		t.Errorf("skills.Close: %v", err)
	}
	// TaskRegistry has no Close in its interface; the inprocess
	// driver holds no long-lived goroutines.
	if err := s.bus.Close(ctx); err != nil {
		t.Errorf("bus.Close: %v", err)
	}
}

// openIsolationStores builds one shared instance of every identity-
// scoped subsystem through its PRODUCTION registry factory. No mocks
// — CLAUDE.md §17.3 #1.
func openIsolationStores(t *testing.T) *isolationStores {
	t.Helper()
	ctx := context.Background()

	redactor, err := audit.Open(ctx, config.AuditConfig{})
	if err != nil {
		t.Fatalf("audit.Open: %v", err)
	}

	st, err := state.Open(ctx, config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}

	art, err := artifacts.Open(ctx, config.ArtifactsConfig{
		Driver:                    "inmem",
		HeavyOutputThresholdBytes: 32768,
	})
	if err != nil {
		t.Fatalf("artifacts.Open: %v", err)
	}

	bus, err := events.Open(ctx, config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 64,
		SubscriberBufferSize:     256,
		IdleTimeout:              60 * time.Second,
		DropWindow:               time.Second,
		ReplayBufferSize:         1024,
	}, redactor)
	if err != nil {
		t.Fatalf("events.Open: %v", err)
	}

	mem, err := memory.Open(ctx, memory.ConfigSnapshot{
		Driver:   "inmem",
		Strategy: memory.StrategyNone,
	}, memory.Deps{State: st, Bus: bus})
	if err != nil {
		t.Fatalf("memory.Open: %v", err)
	}

	// SkillStore has a single V1 driver — `localdb`, SQLite-backed.
	// `:memory:` keeps the harness filesystem-free; the isolation
	// logic is identical to a file-backed DB.
	sk, err := skills.OpenDriver("localdb", skills.ConfigSnapshot{
		Driver: "localdb",
		DSN:    ":memory:",
	}, skills.Deps{Bus: bus})
	if err != nil {
		t.Fatalf("skills.OpenDriver(localdb): %v", err)
	}

	tr, err := tasks.Open(ctx, tasks.Dependencies{
		Store:    st,
		Bus:      bus,
		Redactor: redactor,
		Cfg:      config.TasksConfig{Driver: "inprocess"},
	})
	if err != nil {
		t.Fatalf("tasks.Open: %v", err)
	}

	return &isolationStores{
		state:     st,
		artifacts: art,
		memory:    mem,
		skillsInM: sk,
		tasks:     tr,
		bus:       bus,
	}
}

// isolationIdentities returns the 100 distinct (tenant, user,
// session) triples the harness fans workers across.
func isolationIdentities() []identity.Identity {
	ids := make([]identity.Identity, 0, isolationSessionCount)
	for ti := range isolationTenants {
		for ui := range isolationUsersPerTenant {
			for si := range isolationSessionsPerUser {
				ids = append(ids, identity.Identity{
					TenantID:  fmt.Sprintf("tenant-%02d", ti),
					UserID:    fmt.Sprintf("tenant-%02d-user-%02d", ti, ui),
					SessionID: fmt.Sprintf("tenant-%02d-user-%02d-sess-%02d", ti, ui, si),
				})
			}
		}
	}
	return ids
}

// identityStamp is the canonical string baked into every value the
// harness writes. A read whose recovered stamp does not equal the
// caller's stamp is a cross-scope leak.
func identityStamp(id identity.Identity) string {
	return id.TenantID + "|" + id.UserID + "|" + id.SessionID
}

// soakWindow resolves the soak duration: HARBOR_ISOLATION_SOAK when
// set and valid, the fast window otherwise. `-short` forces the fast
// window regardless of the env var (D-134).
func soakWindow(t *testing.T) time.Duration {
	t.Helper()
	if testing.Short() {
		return isolationFastWindow
	}
	raw := os.Getenv(isolationSoakEnvVar)
	if raw == "" {
		return isolationFastWindow
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		t.Fatalf("%s=%q is not a valid Go duration: %v", isolationSoakEnvVar, raw, err)
	}
	if d <= 0 {
		t.Fatalf("%s=%q must be a positive duration", isolationSoakEnvVar, raw)
	}
	return d
}

// leakReport accumulates every cross-scope breach the harness
// detects. A breach is a security bug; the report carries enough
// context (subsystem, expected vs. observed identity) to triage it.
type leakReport struct {
	mu       sync.Mutex
	breaches []string
	count    atomic.Int64
}

func (lr *leakReport) record(subsystem string, caller identity.Identity, detail string) {
	lr.count.Add(1)
	lr.mu.Lock()
	defer lr.mu.Unlock()
	if len(lr.breaches) < 64 { // cap the report; the count is authoritative
		lr.breaches = append(lr.breaches, fmt.Sprintf(
			"[%s] caller=%s: %s", subsystem, identityStamp(caller), detail))
	}
}

func (lr *leakReport) fail(t *testing.T) {
	t.Helper()
	n := lr.count.Load()
	if n == 0 {
		return
	}
	lr.mu.Lock()
	defer lr.mu.Unlock()
	for _, b := range lr.breaches {
		t.Errorf("ISOLATION BREACH %s", b)
	}
	t.Fatalf("cross-scope isolation breached %d time(s) — RFC §4.3 invariant violated (security bug)", n)
}

// runIsolationSoak is the harness body. It spins isolationSessionCount
// concurrent session-workers against the shared stores; each worker
// loops a randomized op-mix until `window` elapses, asserting after
// every read that the recovered identity matches its own verbatim.
func runIsolationSoak(t *testing.T, stores *isolationStores, window time.Duration) {
	t.Helper()
	ids := isolationIdentities()
	if len(ids) != isolationSessionCount {
		t.Fatalf("identity grid produced %d triples, want %d", len(ids), isolationSessionCount)
	}

	ctx, cancel := context.WithTimeout(context.Background(), window)
	defer cancel()

	report := &leakReport{}
	var totalOps atomic.Int64
	var wg sync.WaitGroup

	for wi, id := range ids {
		wg.Add(1)
		go func(workerIdx int, self identity.Identity) {
			defer wg.Done()
			// Deterministic per-worker seed → reproducible op order
			// when a breach is investigated; still well-mixed
			// across workers.
			rng := rand.New(rand.NewSource(int64(workerIdx) + 1))
			w := &isolationWorker{
				self:    self,
				stores:  stores,
				report:  report,
				rng:     rng,
				ops:     &totalOps,
				allTids: ids,
			}
			for ctx.Err() == nil {
				w.step(ctx)
			}
		}(wi, id)
	}
	wg.Wait()

	report.fail(t)
	if totalOps.Load() == 0 {
		t.Fatal("soak ran zero ops — harness did no work")
	}
	t.Logf("isolation soak: %d workers, %d total op-cycles in %s, 0 breaches",
		isolationSessionCount, totalOps.Load(), window)
}

// isolationWorker carries one session-worker's per-goroutine state.
// Per CLAUDE.md §5 / D-025 nothing run-specific lives on the shared
// stores — only on this worker value.
type isolationWorker struct {
	self    identity.Identity
	stores  *isolationStores
	report  *leakReport
	rng     *rand.Rand
	ops     *atomic.Int64
	allTids []identity.Identity
}

// step runs one randomized write-then-read cycle against one
// randomly-chosen subsystem.
func (w *isolationWorker) step(ctx context.Context) {
	w.ops.Add(1)
	switch w.rng.Intn(6) {
	case 0:
		w.stepState(ctx)
	case 1:
		w.stepArtifacts(ctx)
	case 2:
		w.stepMemory(ctx)
	case 3:
		w.stepSkills(ctx)
	case 4:
		w.stepTasks(ctx)
	case 5:
		w.stepBus(ctx)
	}
}

// quad is the worker's identity as a runtime Quadruple (empty RunID:
// the harness exercises session-scoped state).
func (w *isolationWorker) quad() identity.Quadruple {
	return identity.Quadruple{Identity: w.self}
}

// --- StateStore -------------------------------------------------------

func (w *isolationWorker) stepState(ctx context.Context) {
	kind := fmt.Sprintf("isolation.probe.%d", w.rng.Intn(8))
	stamp := identityStamp(w.self)
	rec := state.StateRecord{
		ID:       state.NewEventID(),
		Identity: w.quad(),
		Kind:     kind,
		Bytes:    []byte(stamp),
	}
	if err := w.stores.state.Save(ctx, rec); err != nil {
		if ctx.Err() != nil {
			return
		}
		w.report.record("state", w.self, "Save: "+err.Error())
		return
	}
	got, err := w.stores.state.Load(ctx, w.quad(), kind)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		w.report.record("state", w.self, "Load: "+err.Error())
		return
	}
	if string(got.Bytes) != stamp {
		w.report.record("state", w.self, fmt.Sprintf(
			"Load returned value stamped %q, want %q", got.Bytes, stamp))
	}
	if got.Identity.Identity != w.self {
		w.report.record("state", w.self, fmt.Sprintf(
			"Load returned record scoped to %s", identityStamp(got.Identity.Identity)))
	}
}

// --- ArtifactStore ----------------------------------------------------

func (w *isolationWorker) stepArtifacts(ctx context.Context) {
	scope := artifacts.ArtifactScope{
		TenantID:  w.self.TenantID,
		UserID:    w.self.UserID,
		SessionID: w.self.SessionID,
	}
	stamp := identityStamp(w.self)
	// Distinct content per worker so the content-addressed ID never
	// collides across scopes — a collision would mask a leak.
	payload := fmt.Sprintf("isolation-artifact:%s:%d", stamp, w.rng.Intn(8))
	ref, err := w.stores.artifacts.PutText(ctx, scope, payload, artifacts.PutOpts{
		Namespace: "isolation",
	})
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		w.report.record("artifacts", w.self, "PutText: "+err.Error())
		return
	}
	got, found, err := w.stores.artifacts.Get(ctx, scope, ref.ID)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		w.report.record("artifacts", w.self, "Get: "+err.Error())
		return
	}
	if !found {
		w.report.record("artifacts", w.self, "Get: own artifact not found")
		return
	}
	if string(got) != payload {
		w.report.record("artifacts", w.self, fmt.Sprintf(
			"Get returned %q, want %q", got, payload))
	}
	if !ref.Scope.Equal(scope) {
		w.report.record("artifacts", w.self, fmt.Sprintf(
			"ref scoped to (%s,%s,%s)", ref.Scope.TenantID, ref.Scope.UserID, ref.Scope.SessionID))
	}
	// List under the worker's own scope must return ONLY its own
	// artifacts — every recovered ref carries the worker's stamp.
	refs, err := w.stores.artifacts.List(ctx, scope)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		w.report.record("artifacts", w.self, "List: "+err.Error())
		return
	}
	for _, r := range refs {
		if !r.Scope.Equal(scope) {
			w.report.record("artifacts", w.self, fmt.Sprintf(
				"List leaked ref scoped to (%s,%s,%s)",
				r.Scope.TenantID, r.Scope.UserID, r.Scope.SessionID))
		}
	}
}

// --- MemoryStore ------------------------------------------------------

func (w *isolationWorker) stepMemory(ctx context.Context) {
	// Strategy=none: AddTurn / GetLLMContext are no-ops, but the
	// identity gate still fires. The load-bearing assertion here is
	// the fail-CLOSED behaviour — every method validates identity.
	if err := w.stores.memory.AddTurn(ctx, w.quad(), memory.ConversationTurn{
		UserMessage:       "isolation-probe:" + identityStamp(w.self),
		AssistantResponse: "ack:" + identityStamp(w.self),
	}); err != nil {
		if ctx.Err() != nil {
			return
		}
		w.report.record("memory", w.self, "AddTurn: "+err.Error())
		return
	}
	if _, err := w.stores.memory.GetLLMContext(ctx, w.quad()); err != nil {
		if ctx.Err() != nil {
			return
		}
		w.report.record("memory", w.self, "GetLLMContext: "+err.Error())
		return
	}
	if _, err := w.stores.memory.Health(ctx, w.quad()); err != nil {
		if ctx.Err() != nil {
			return
		}
		w.report.record("memory", w.self, "Health: "+err.Error())
	}
}

// --- SkillStore -------------------------------------------------------

func (w *isolationWorker) stepSkills(ctx context.Context) {
	stamp := identityStamp(w.self)
	name := fmt.Sprintf("isolation-skill-%d", w.rng.Intn(8))
	sk := skills.Skill{
		Name:        name,
		Title:       "Isolation probe " + stamp,
		Description: "probe skill stamped " + stamp,
		Trigger:     "when isolation probe runs for " + stamp,
		Steps:       []string{"step bound to " + stamp},
		Origin:      skills.OriginGenerated,
		Scope:       skills.ScopeSession,
		ContentHash: stamp + ":" + name,
	}
	if err := w.stores.skillsInM.Upsert(ctx, w.quad(), sk); err != nil {
		if ctx.Err() != nil {
			return
		}
		w.report.record("skills", w.self, "Upsert: "+err.Error())
		return
	}
	got, err := w.stores.skillsInM.Get(ctx, w.quad(), name)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		w.report.record("skills", w.self, "Get: "+err.Error())
		return
	}
	if got.Description != "probe skill stamped "+stamp {
		w.report.record("skills", w.self, fmt.Sprintf(
			"Get returned skill described %q, want stamp %q", got.Description, stamp))
	}
	// List under the worker's identity must return only its own
	// skills — every row's description must carry the worker stamp.
	list, err := w.stores.skillsInM.List(ctx, w.quad(), skills.ListFilter{})
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		w.report.record("skills", w.self, "List: "+err.Error())
		return
	}
	want := "probe skill stamped " + stamp
	for _, s := range list {
		if s.Description != want {
			w.report.record("skills", w.self, fmt.Sprintf(
				"List leaked skill %q described %q", s.Name, s.Description))
		}
	}
}

// --- TaskRegistry -----------------------------------------------------

func (w *isolationWorker) stepTasks(ctx context.Context) {
	stamp := identityStamp(w.self)
	handle, err := w.stores.tasks.Spawn(ctx, tasks.SpawnRequest{
		Identity:    w.quad(),
		Kind:        tasks.KindForeground,
		Description: "isolation task " + stamp,
		Query:       "probe:" + stamp,
	})
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		w.report.record("tasks", w.self, "Spawn: "+err.Error())
		return
	}
	// Get must resolve the worker's own task and carry its identity.
	task, err := w.stores.tasks.Get(identityScopedCtx(ctx, w.self), handle.ID)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		w.report.record("tasks", w.self, "Get: "+err.Error())
		return
	}
	if task.Identity.Identity != w.self {
		w.report.record("tasks", w.self, fmt.Sprintf(
			"Get returned task scoped to %s", identityStamp(task.Identity.Identity)))
	}
	// List under the worker's session must return only its own tasks.
	summaries, err := w.stores.tasks.List(identityScopedCtx(ctx, w.self), w.self, tasks.TaskFilter{})
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		w.report.record("tasks", w.self, "List: "+err.Error())
		return
	}
	// Every summarised task must be Get-resolvable under this
	// identity (proves they belong to this scope); a task from
	// another scope would fail Get with ErrNotFound.
	for _, s := range summaries {
		owned, gErr := w.stores.tasks.Get(identityScopedCtx(ctx, w.self), s.ID)
		if gErr != nil {
			if ctx.Err() != nil {
				return
			}
			w.report.record("tasks", w.self, fmt.Sprintf(
				"List surfaced task %s not Get-resolvable under caller scope: %v", s.ID, gErr))
			continue
		}
		if owned.Identity.Identity != w.self {
			w.report.record("tasks", w.self, fmt.Sprintf(
				"List leaked task %s scoped to %s", s.ID, identityStamp(owned.Identity.Identity)))
		}
	}
}

// identityScopedCtx attaches the worker's identity to ctx. TaskRegistry
// Get / List read the caller identity from ctx for the cross-scope
// visibility check.
func identityScopedCtx(ctx context.Context, id identity.Identity) context.Context {
	scoped, err := identity.With(ctx, id)
	if err != nil {
		// Identity is constructed from a valid grid; With cannot
		// fail here. Returning the unscoped ctx would make the
		// TaskRegistry reject the call loudly, which still surfaces
		// as a recorded breach rather than a silent pass.
		return ctx
	}
	return scoped
}

// --- EventBus ---------------------------------------------------------

func (w *isolationWorker) stepBus(ctx context.Context) {
	// Subscribe scoped to the worker's own (tenant, user, session).
	sub, err := w.stores.bus.Subscribe(ctx, events.Filter{
		Tenant:  w.self.TenantID,
		User:    w.self.UserID,
		Session: w.self.SessionID,
	})
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		w.report.record("events", w.self, "Subscribe: "+err.Error())
		return
	}
	defer sub.Cancel()

	stamp := identityStamp(w.self)
	ev := events.Event{
		Type:     events.EventTypeRuntimeWarning,
		Identity: w.quad(),
		Payload: events.RuntimeErrorPayload{
			Message: "isolation-probe:" + stamp,
			Fields:  map[string]any{"stamp": stamp},
		},
	}
	if err := w.stores.bus.Publish(ctx, ev); err != nil {
		if ctx.Err() != nil {
			return
		}
		w.report.record("events", w.self, "Publish: "+err.Error())
		return
	}

	// Drain whatever the subscription delivers within a bounded
	// window; assert every delivered event is identity-matched. We
	// do NOT require our own event to arrive (other workers may
	// have filled the buffer) — the invariant under test is "no
	// OTHER scope's event is ever delivered", not "my event always
	// arrives".
	deadline := time.After(50 * time.Millisecond)
	for {
		select {
		case got, ok := <-sub.Events():
			if !ok {
				return
			}
			if got.Identity.TenantID != w.self.TenantID ||
				got.Identity.UserID != w.self.UserID ||
				got.Identity.SessionID != w.self.SessionID {
				w.report.record("events", w.self, fmt.Sprintf(
					"Subscribe delivered event scoped to %s", identityStamp(got.Identity.Identity)))
			}
		case <-deadline:
			return
		case <-ctx.Done():
			return
		}
	}
}

// ----------------------------------------------------------------------
// Tests
// ----------------------------------------------------------------------

// TestE2E_Isolation_ConformanceHarness is the master gate: 100
// concurrent sessions across 5 tenants × 4 users randomly hammer all
// six identity-scoped subsystems for the soak window; the harness
// asserts every read returns ONLY the caller's own identity-stamped
// data. Zero cross-tenant bleed, zero cross-session bleed (RFC §4.3).
func TestE2E_Isolation_ConformanceHarness(t *testing.T) {
	stores := openIsolationStores(t)
	defer stores.closeAll(t)

	window := soakWindow(t)
	t.Logf("isolation soak window: %s (set %s to override; -short forces %s)",
		window, isolationSoakEnvVar, isolationFastWindow)

	// Goroutine baseline BEFORE the soak — settle the scheduler
	// without time.Sleep (§17.4).
	runtime.GC()
	for range 100 {
		runtime.Gosched()
	}
	baseline := runtime.NumGoroutine()

	runIsolationSoak(t, stores, window)

	// No goroutine leak: every worker joined; subscriptions all
	// cancelled. Bounded eventually-poll, never a fixed sleep.
	if got, ok := waitForGoroutineFloor(2*time.Second, baseline+8); !ok {
		t.Errorf("goroutine leak after soak: baseline=%d after=%d (delta=%d)",
			baseline, got, got-baseline)
	}
}

// TestE2E_Isolation_FailClosedOnMissingIdentity is the named §17.3
// failure mode: every one of the six subsystems MUST reject a request
// whose identity triple is incomplete — fail closed, loudly, never
// silently degrade (CLAUDE.md §6 rule 9, §13, D-001). A subsystem
// that accepted a tenant-less write would be the exact silent-leak
// bug Harbor's isolation contract closes.
func TestE2E_Isolation_FailClosedOnMissingIdentity(t *testing.T) {
	stores := openIsolationStores(t)
	defer stores.closeAll(t)
	ctx := context.Background()

	// A deliberately-incomplete identity: tenant present, user +
	// session empty.
	bad := identity.Identity{TenantID: "tenant-only"}
	badQuad := identity.Quadruple{Identity: bad}

	t.Run("StateStore", func(t *testing.T) {
		err := stores.state.Save(ctx, state.StateRecord{
			ID:       state.NewEventID(),
			Identity: badQuad,
			Kind:     "isolation.failclosed",
			Bytes:    []byte("x"),
		})
		if err == nil {
			t.Fatal("StateStore.Save accepted an incomplete identity — fail-closed breach")
		}
	})

	t.Run("ArtifactStore", func(t *testing.T) {
		_, err := stores.artifacts.PutText(ctx, artifacts.ArtifactScope{
			TenantID: "tenant-only",
		}, "x", artifacts.PutOpts{Namespace: "isolation"})
		if err == nil {
			t.Fatal("ArtifactStore.PutText accepted an incomplete scope — fail-closed breach")
		}
	})

	t.Run("MemoryStore", func(t *testing.T) {
		err := stores.memory.AddTurn(ctx, badQuad, memory.ConversationTurn{
			UserMessage: "x",
		})
		if err == nil {
			t.Fatal("MemoryStore.AddTurn accepted an incomplete identity — fail-closed breach")
		}
	})

	t.Run("SkillStore", func(t *testing.T) {
		err := stores.skillsInM.Upsert(ctx, badQuad, skills.Skill{
			Name:    "isolation-failclosed",
			Trigger: "x",
			Steps:   []string{"x"},
			Origin:  skills.OriginGenerated,
			Scope:   skills.ScopeSession,
		})
		if err == nil {
			t.Fatal("SkillStore.Upsert accepted an incomplete identity — fail-closed breach")
		}
	})

	t.Run("TaskRegistry", func(t *testing.T) {
		_, err := stores.tasks.Spawn(ctx, tasks.SpawnRequest{
			Identity: badQuad,
			Kind:     tasks.KindForeground,
		})
		if err == nil {
			t.Fatal("TaskRegistry.Spawn accepted an incomplete identity — fail-closed breach")
		}
	})

	t.Run("EventBus", func(t *testing.T) {
		// Subscribe with a partial triple and no admin scope must be
		// rejected (events §6.13 / brief 06 §124).
		_, err := stores.bus.Subscribe(ctx, events.Filter{Tenant: "tenant-only"})
		if err == nil {
			t.Fatal("EventBus.Subscribe accepted a partial-triple filter without admin scope — fail-closed breach")
		}
	})
}

// TestE2E_Isolation_CrossScopeReadIsBlind is the targeted positive
// proof of the RFC §4.3 invariant, separate from the probabilistic
// soak: two fixed identities — same tenant+user, different session
// (the cross-SESSION boundary) and a third in a different tenant (the
// cross-TENANT boundary) — write distinct data; each then reads under
// its OWN identity and must see only its own rows.
func TestE2E_Isolation_CrossScopeReadIsBlind(t *testing.T) {
	stores := openIsolationStores(t)
	defer stores.closeAll(t)
	ctx := context.Background()

	idA := identity.Identity{TenantID: "tA", UserID: "uShared", SessionID: "sessA"}
	idB := identity.Identity{TenantID: "tA", UserID: "uShared", SessionID: "sessB"} // cross-session vs A
	idC := identity.Identity{TenantID: "tC", UserID: "uC", SessionID: "sessC"}      // cross-tenant vs A

	all := []identity.Identity{idA, idB, idC}

	// Each identity writes one state record + one artifact + one
	// skill + one task, all stamped with its own identity.
	for _, id := range all {
		q := identity.Quadruple{Identity: id}
		stamp := identityStamp(id)

		if err := stores.state.Save(ctx, state.StateRecord{
			ID: state.NewEventID(), Identity: q, Kind: "xscope", Bytes: []byte(stamp),
		}); err != nil {
			t.Fatalf("state.Save(%s): %v", stamp, err)
		}
		if _, err := stores.artifacts.PutText(ctx, artifacts.ArtifactScope{
			TenantID: id.TenantID, UserID: id.UserID, SessionID: id.SessionID,
		}, "xscope:"+stamp, artifacts.PutOpts{Namespace: "xscope"}); err != nil {
			t.Fatalf("artifacts.PutText(%s): %v", stamp, err)
		}
		if err := stores.skillsInM.Upsert(ctx, q, skills.Skill{
			Name: "xscope", Trigger: "x", Steps: []string{"x"},
			Description: stamp, Origin: skills.OriginGenerated, Scope: skills.ScopeSession,
			ContentHash: stamp,
		}); err != nil {
			t.Fatalf("skills.Upsert(%s): %v", stamp, err)
		}
		if _, err := stores.tasks.Spawn(ctx, tasks.SpawnRequest{
			Identity: q, Kind: tasks.KindForeground, Query: stamp,
		}); err != nil {
			t.Fatalf("tasks.Spawn(%s): %v", stamp, err)
		}
	}

	// Each identity reads under its OWN scope and must recover only
	// its own stamp.
	for _, id := range all {
		q := identity.Quadruple{Identity: id}
		stamp := identityStamp(id)
		sctx := identityScopedCtx(ctx, id)

		rec, err := stores.state.Load(ctx, q, "xscope")
		if err != nil {
			t.Errorf("state.Load(%s): %v", stamp, err)
		} else if string(rec.Bytes) != stamp {
			t.Errorf("state cross-scope leak: %s read %q", stamp, rec.Bytes)
		}

		arts, err := stores.artifacts.List(ctx, artifacts.ArtifactScope{
			TenantID: id.TenantID, UserID: id.UserID, SessionID: id.SessionID,
		})
		if err != nil {
			t.Errorf("artifacts.List(%s): %v", stamp, err)
		}
		if len(arts) != 1 {
			t.Errorf("artifacts cross-scope leak: %s sees %d artifacts, want 1", stamp, len(arts))
		}

		sk, err := stores.skillsInM.Get(ctx, q, "xscope")
		if err != nil {
			t.Errorf("skills.Get(%s): %v", stamp, err)
		} else if sk.Description != stamp {
			t.Errorf("skills cross-scope leak: %s read skill described %q", stamp, sk.Description)
		}

		summaries, err := stores.tasks.List(sctx, id, tasks.TaskFilter{})
		if err != nil {
			t.Errorf("tasks.List(%s): %v", stamp, err)
		}
		if len(summaries) != 1 {
			t.Errorf("tasks cross-scope leak: %s sees %d tasks, want 1", stamp, len(summaries))
		}
	}
}

// waitForGoroutineFloor polls runtime.NumGoroutine until it is at or
// below `target`, or maxWait elapses. Bounded eventually-poll — never
// a fixed time.Sleep (§17.4).
func waitForGoroutineFloor(maxWait time.Duration, target int) (int, bool) {
	deadline := time.Now().Add(maxWait)
	for time.Now().Before(deadline) {
		runtime.GC()
		runtime.Gosched()
		if got := runtime.NumGoroutine(); got <= target {
			return got, true
		}
		for range 10 {
			runtime.Gosched()
		}
	}
	return runtime.NumGoroutine(), false
}
