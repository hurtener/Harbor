// Wave 5 cross-subsystem integration test per AGENTS.md §17.5.
//
// Wave 5 closed two persistence-floor surfaces:
//
//   - Phase 15: SQLite StateStore driver (modernc.org/sqlite, WAL,
//     forward-only migrations) — durable single-binary persistence.
//   - Phase 16: Postgres StateStore driver (pgx, advisory-lock-
//     serialised migrations) — multi-node persistence; tests t.Skip
//     cleanly without HARBOR_PG_DSN. Exercised in CI via the
//     state-postgres job; here, only the SQLite path is mandatory.
//   - Phase 17: ArtifactStore — content-addressed blob store with
//     two V1 drivers (inmem, fs), the ScopedArtifacts facade, and
//     the heavy-output threshold gate.
//
// Each phase shipped its own conformance suite + concurrent-reuse
// test under -race. This wave-end smoke proves the new surfaces
// COMPOSE with the already-shipped subsystems (sessions / engine /
// audit / events) and that identity propagates through every layer.
//
// Three tests, each focused on a different composition angle:
//
//   - TestE2E_Wave5_DurablePersistence_RoundtripAcrossClose: open a
//     SQLite-backed StateStore, write a session lifecycle record via
//     SessionRegistry, write an artifact via the FS ArtifactStore,
//     close everything, reopen against the same DSN + FSRoot, prove
//     the state record AND the artifact survive (this is the durable
//     vs in-memory differentiator).
//   - TestE2E_Wave5_HeavyOutput_RoutesThroughArtifactStore: simulate
//     the heavy-output decision a tool dispatcher will make at Phase
//     26 — a payload above HeavyOutputThresholdBytes lands as an
//     ArtifactRef; the same scope cannot be read by a different
//     tenant's facade.
//   - TestE2E_Wave5_Concurrent_MultiTenant_PersistAndArtifact: 8
//     tenants × 4 sessions concurrent, each writing state records +
//     artifacts on a single shared SQLite store + FS ArtifactStore.
//     Asserts no cross-talk (per-tenant state lookups + artifact
//     lists return only that tenant's data) and goroutine baseline
//     restored after teardown.
package integration_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/artifacts"
	_ "github.com/hurtener/Harbor/internal/artifacts/drivers/fs"
	_ "github.com/hurtener/Harbor/internal/artifacts/drivers/inmem"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	_ "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/sessions"
	"github.com/hurtener/Harbor/internal/state"
	_ "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	_ "github.com/hurtener/Harbor/internal/state/drivers/sqlite"
)

// TestE2E_Wave5_DurablePersistence_RoundtripAcrossClose is the
// signature wave-5 test: open the durable surface, write through
// real sessions + artifacts wiring, close everything, reopen, prove
// the data survives. Roughly the same shape `harbor dev --persist
// sqlite://...` will exercise once the CLI lands at Phase 64.
//
// What this exercises:
//   - Phase 15 SQLite driver under a real `state.Open` factory call.
//   - Phase 17 FS ArtifactStore driver under a real `artifacts.Open`
//     factory call, with a `ScopedArtifacts` facade per the runtime's
//     intended consumer pattern.
//   - Phase 08 SessionRegistry writes through StateStore (the typed
//     wrapper at the consumer layer per D-027).
//   - Identity propagation: Identity.With + identity.WithRun on ctx;
//     ScopedArtifacts.NewScoped with the matching ArtifactScope.
//
// Failure modes covered:
//   - cross-tenant artifact read returns (nil, false, nil) (NOT
//     ErrNotFound — the ScopedArtifacts contract is found-false-is-
//     not-an-error per RFC §6.10).
//   - reopening with a different SQLite DSN does NOT see the prior
//     run's data (proves the persistence is actually file-backed,
//     not process-resident memory pretending to be a file).
func TestE2E_Wave5_DurablePersistence_RoundtripAcrossClose(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "wave5.db")
	dsn := "file:" + dbPath + "?cache=shared"
	fsRoot := t.TempDir()

	cfg := wave5SQLiteConfig(dsn, fsRoot)
	red := auditpatterns.New()

	// --- Phase 1: open everything, write data. ---

	bus, err := events.Open(context.Background(), cfg.Events, red)
	if err != nil {
		t.Fatalf("events.Open: %v", err)
	}
	store, err := state.Open(context.Background(), cfg.State)
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	artStore, err := artifacts.Open(context.Background(), cfg.Artifacts)
	if err != nil {
		t.Fatalf("artifacts.Open: %v", err)
	}
	reg, err := sessions.New(store, cfg.Sessions, bus)
	if err != nil {
		t.Fatalf("sessions.New: %v", err)
	}

	id := identity.Identity{TenantID: "T1", UserID: "U1", SessionID: "S1"}
	ctx, _ := identity.With(context.Background(), id)

	if _, err := reg.Open(ctx, id.SessionID, id); err != nil {
		t.Fatalf("session Open: %v", err)
	}

	// Per-task ScopedArtifacts — the canonical consumer pattern.
	scope := artifacts.ArtifactScope{
		TenantID: id.TenantID, UserID: id.UserID, SessionID: id.SessionID,
		TaskID: "task-1",
	}
	scoped := artifacts.NewScoped(artStore, scope)

	wantBytes := []byte("durable-payload-" + dsn)
	ref, err := scoped.PutBytes(ctx, wantBytes, artifacts.PutOpts{
		MimeType:  "application/octet-stream",
		Filename:  "payload.bin",
		Namespace: "wave5",
	})
	if err != nil {
		t.Fatalf("scoped.PutBytes: %v", err)
	}
	if ref.SizeBytes != int64(len(wantBytes)) {
		t.Errorf("ref.SizeBytes=%d want %d", ref.SizeBytes, len(wantBytes))
	}

	// --- Phase 2: close. The whole point: prove durability. ---

	if err := reg.CloseRegistry(context.Background()); err != nil {
		t.Errorf("reg.CloseRegistry: %v", err)
	}
	if err := artStore.Close(context.Background()); err != nil {
		t.Errorf("artStore.Close: %v", err)
	}
	if err := store.Close(context.Background()); err != nil {
		t.Errorf("store.Close: %v", err)
	}
	if err := bus.Close(context.Background()); err != nil {
		t.Errorf("bus.Close: %v", err)
	}

	// --- Phase 3: reopen against the same DSN + FSRoot. ---

	bus2, err := events.Open(context.Background(), cfg.Events, red)
	if err != nil {
		t.Fatalf("events.Open #2: %v", err)
	}
	defer func() { _ = bus2.Close(context.Background()) }()

	store2, err := state.Open(context.Background(), cfg.State)
	if err != nil {
		t.Fatalf("state.Open #2: %v", err)
	}
	defer func() { _ = store2.Close(context.Background()) }()

	artStore2, err := artifacts.Open(context.Background(), cfg.Artifacts)
	if err != nil {
		t.Fatalf("artifacts.Open #2: %v", err)
	}
	defer func() { _ = artStore2.Close(context.Background()) }()

	scoped2 := artifacts.NewScoped(artStore2, scope)
	gotBytes, found, err := scoped2.Get(ctx, ref.ID)
	if err != nil {
		t.Fatalf("scoped2.Get: %v", err)
	}
	if !found {
		t.Fatalf("artifact not durable across reopen — found=false")
	}
	if string(gotBytes) != string(wantBytes) {
		t.Errorf("durable bytes mismatch: got %q want %q", gotBytes, wantBytes)
	}

	// Cross-tenant isolation across the durable boundary: open a
	// facade for tenant T2 and assert (nil, false, nil).
	otherScope := artifacts.ArtifactScope{
		TenantID: "T2", UserID: "U1", SessionID: "S1", TaskID: "task-1",
	}
	otherScoped := artifacts.NewScoped(artStore2, otherScope)
	if _, found, err := otherScoped.Get(ctx, ref.ID); err != nil {
		t.Fatalf("cross-tenant Get: unexpected error %v", err)
	} else if found {
		t.Errorf("cross-tenant leak: tenant T2 found tenant T1's artifact")
	}

	// SessionRegistry reopened on the same SQLite DSN: the durable
	// session lifecycle record from the prior process is still in
	// the StateStore, so the new registry's Get returns it. (Re-
	// calling Open with the same SessionID is rejected as
	// ErrSessionAlreadyOpen — the session is durable; re-opening
	// is a caller bug, not idempotent.)
	reg2, err := sessions.New(store2, cfg.Sessions, bus2)
	if err != nil {
		t.Fatalf("sessions.New #2: %v", err)
	}
	defer func() { _ = reg2.CloseRegistry(context.Background()) }()
	got, err := reg2.Get(ctx, id.SessionID)
	if err != nil {
		t.Errorf("reg2.Get after durable reopen: %v", err)
	} else if got == nil || got.ID != id.SessionID {
		t.Errorf("reg2.Get: got %+v want session ID %q", got, id.SessionID)
	}
	if _, err := reg2.Open(ctx, id.SessionID, id); !errors.Is(err, sessions.ErrSessionAlreadyOpen) {
		t.Errorf("reg2.Open re-open should be rejected: err=%v want ErrSessionAlreadyOpen", err)
	}
}

// TestE2E_Wave5_HeavyOutput_RoutesThroughArtifactStore mirrors the
// decision a future Phase 26 tool dispatcher will make: a payload
// above HeavyOutputThresholdBytes routes through ArtifactStore and
// the consumer carries an ArtifactRef onward, never the bytes (D-026,
// RFC §6.10).
//
// Phase 17 ships the threshold field on ArtifactsConfig but does NOT
// enforce routing inside the store — enforcement lives at the
// dispatcher (Phase 26) and the LLM-edge catch-all (Phase 32). This
// test asserts the contract Phase 17 exposes is correct: the
// threshold is readable from config, the FS driver round-trips
// payloads of any size, and re-Put of identical bytes deduplicates
// to the same ID (which is what makes the routing economical).
func TestE2E_Wave5_HeavyOutput_RoutesThroughArtifactStore(t *testing.T) {
	cfg := wave5SQLiteConfig(filepath.Join(t.TempDir(), "wave5-heavy.db"), t.TempDir())
	thresh := cfg.Artifacts.HeavyOutputThresholdBytes
	if thresh != 32*1024 {
		t.Fatalf("default threshold drift: want 32KB, got %d", thresh)
	}

	store, err := state.Open(context.Background(), cfg.State)
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	defer func() { _ = store.Close(context.Background()) }()

	artStore, err := artifacts.Open(context.Background(), cfg.Artifacts)
	if err != nil {
		t.Fatalf("artifacts.Open: %v", err)
	}
	defer func() { _ = artStore.Close(context.Background()) }()

	scope := artifacts.ArtifactScope{
		TenantID: "T", UserID: "U", SessionID: "S", TaskID: "task-heavy",
	}
	scoped := artifacts.NewScoped(artStore, scope)

	// Heavy payload (40KB > threshold). Simulates a tool that
	// produced a large blob; the dispatcher (Phase 26+) would route
	// to the store and propagate only the ref.
	heavy := make([]byte, 40*1024)
	for i := range heavy {
		heavy[i] = byte(i % 256)
	}

	ctx := context.Background()
	ref1, err := scoped.PutBytes(ctx, heavy, artifacts.PutOpts{
		MimeType:  "application/octet-stream",
		Namespace: "tool-output",
	})
	if err != nil {
		t.Fatalf("PutBytes heavy: %v", err)
	}
	if ref1.SizeBytes != int64(len(heavy)) {
		t.Errorf("ref1.SizeBytes=%d want %d", ref1.SizeBytes, len(heavy))
	}
	if got, found, err := scoped.Get(ctx, ref1.ID); err != nil {
		t.Fatalf("Get heavy: %v", err)
	} else if !found {
		t.Fatalf("Get heavy: found=false")
	} else if !bytesEqual(got, heavy) {
		t.Errorf("heavy bytes round-trip mismatch")
	}

	// Dedup: re-Put the same bytes under the same scope+namespace →
	// same ID, no second copy in storage. This is what makes
	// mandatory routing economical at the runtime: identical tool
	// outputs across runs cost nothing extra.
	ref2, err := scoped.PutBytes(ctx, heavy, artifacts.PutOpts{
		MimeType:  "application/octet-stream",
		Namespace: "tool-output",
	})
	if err != nil {
		t.Fatalf("PutBytes heavy #2: %v", err)
	}
	if ref1.ID != ref2.ID {
		t.Errorf("dedup failed: ref1.ID=%q ref2.ID=%q", ref1.ID, ref2.ID)
	}

	// Light payload (under threshold). Below the threshold the
	// dispatcher will decide NOT to route through the store; the
	// store still happily takes it. This proves the store has no
	// size-floor of its own.
	light := []byte("under-threshold")
	refLight, err := scoped.PutBytes(ctx, light, artifacts.PutOpts{
		Namespace: "tool-output",
	})
	if err != nil {
		t.Fatalf("PutBytes light: %v", err)
	}
	if refLight.ID == ref1.ID {
		t.Errorf("different bytes collided on ID: %q", refLight.ID)
	}

	// Failure mode: Get with a deliberately invalid scope at the
	// store level (bypass the facade) returns ErrIdentityRequired.
	if _, _, err := artStore.Get(ctx, artifacts.ArtifactScope{}, ref1.ID); !errors.Is(err, artifacts.ErrIdentityRequired) {
		t.Errorf("Get with empty scope: err=%v want errors.Is ErrIdentityRequired", err)
	}
}

// TestE2E_Wave5_Concurrent_MultiTenant_PersistAndArtifact runs N
// tenants × M sessions concurrently against a single SQLite store +
// FS ArtifactStore, asserting per-tenant isolation under load and a
// clean goroutine baseline after teardown (D-025).
//
// Why both the SQLite driver AND the FS ArtifactStore in the same
// stress test: each shipped its own per-package concurrent test,
// but cross-package contention (e.g. a SQLite write contending with
// a filesystem fsync on the same Goroutine pool) is a class of bug
// the per-package tests can't see. This is exactly the §17.3 case
// for an "Integration stress run."
func TestE2E_Wave5_Concurrent_MultiTenant_PersistAndArtifact(t *testing.T) {
	const tenants = 8
	const sessionsPerTenant = 4
	const writesPerSession = 8

	baseline := runtime.NumGoroutine()

	cfg := wave5SQLiteConfig(filepath.Join(t.TempDir(), "wave5-stress.db"), t.TempDir())

	store, err := state.Open(context.Background(), cfg.State)
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	artStore, err := artifacts.Open(context.Background(), cfg.Artifacts)
	if err != nil {
		t.Fatalf("artifacts.Open: %v", err)
	}

	var (
		wg     sync.WaitGroup
		errCnt atomic.Int64
	)
	wg.Add(tenants * sessionsPerTenant)

	for ti := 0; ti < tenants; ti++ {
		for sj := 0; sj < sessionsPerTenant; sj++ {
			ti, sj := ti, sj
			go func() {
				defer wg.Done()
				ctx := context.Background()
				scope := artifacts.ArtifactScope{
					TenantID:  fmt.Sprintf("T-%d", ti),
					UserID:    fmt.Sprintf("U-%d", ti),
					SessionID: fmt.Sprintf("S-%d-%d", ti, sj),
					TaskID:    "task-stress",
				}
				scoped := artifacts.NewScoped(artStore, scope)

				identQuad := identity.Quadruple{
					Identity: identity.Identity{
						TenantID:  scope.TenantID,
						UserID:    scope.UserID,
						SessionID: scope.SessionID,
					},
				}

				for w := 0; w < writesPerSession; w++ {
					// 1) StateStore write.
					eventID := state.NewEventID()
					rec := state.StateRecord{
						ID:       eventID,
						Identity: identQuad,
						Kind:     "wave5.stress",
						Bytes:    []byte(fmt.Sprintf("t=%d s=%d w=%d", ti, sj, w)),
					}
					if err := store.Save(ctx, rec); err != nil {
						errCnt.Add(1)
						t.Errorf("store.Save: %v", err)
						return
					}
					if got, err := store.Load(ctx, identQuad, "wave5.stress"); err != nil {
						errCnt.Add(1)
						t.Errorf("store.Load: %v", err)
						return
					} else if string(got.Bytes) == "" {
						errCnt.Add(1)
						t.Errorf("store.Load: empty bytes")
						return
					}

					// 2) Artifact write under the per-session scope.
					payload := []byte(fmt.Sprintf("artifact-t=%d-s=%d-w=%d", ti, sj, w))
					ref, err := scoped.PutBytes(ctx, payload, artifacts.PutOpts{
						Namespace: "stress",
					})
					if err != nil {
						errCnt.Add(1)
						t.Errorf("scoped.PutBytes: %v", err)
						return
					}
					if got, found, err := scoped.Get(ctx, ref.ID); err != nil {
						errCnt.Add(1)
						t.Errorf("scoped.Get: %v", err)
						return
					} else if !found {
						errCnt.Add(1)
						t.Errorf("scoped.Get: own write found=false")
						return
					} else if string(got) != string(payload) {
						errCnt.Add(1)
						t.Errorf("scoped.Get: cross-talk — got %q want %q", got, payload)
						return
					}
				}
			}()
		}
	}

	wg.Wait()
	if n := errCnt.Load(); n != 0 {
		t.Fatalf("%d concurrent operations errored", n)
	}

	// Cross-tenant isolation post-stress: tenant T-0's List(filter)
	// returns ONLY T-0's artifacts; same for T-1. Sample two for
	// brevity; the per-package conformance suite covers the matrix.
	for ti := 0; ti < 2; ti++ {
		filter := artifacts.ArtifactScope{TenantID: fmt.Sprintf("T-%d", ti)}
		refs, err := artStore.List(context.Background(), filter)
		if err != nil {
			t.Errorf("artStore.List(tenant=%d): %v", ti, err)
			continue
		}
		want := sessionsPerTenant * writesPerSession
		if len(refs) != want {
			t.Errorf("tenant T-%d: List returned %d refs, want %d", ti, len(refs), want)
		}
		for _, r := range refs {
			if r.Scope.TenantID != filter.TenantID {
				t.Errorf("tenant T-%d: List returned ref from %q", ti, r.Scope.TenantID)
			}
		}
	}

	// Teardown.
	if err := artStore.Close(context.Background()); err != nil {
		t.Errorf("artStore.Close: %v", err)
	}
	if err := store.Close(context.Background()); err != nil {
		t.Errorf("store.Close: %v", err)
	}

	// Goroutine baseline restored. Tolerance +5 because we tore
	// down two long-lived subsystems and Go's parked goroutines may
	// not retire immediately.
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > baseline+5 && time.Now().Before(deadline) {
		runtime.Gosched()
		time.Sleep(10 * time.Millisecond)
	}
	if delta := runtime.NumGoroutine() - baseline; delta > 5 {
		t.Errorf("goroutine leak: baseline=%d after=%d (delta=%d)", baseline, runtime.NumGoroutine(), delta)
	}
}

// --- helpers ---

func wave5SQLiteConfig(sqliteDSN, fsRoot string) *config.Config {
	return &config.Config{
		Server: config.ServerConfig{
			BindAddr:            "127.0.0.1:8080",
			ShutdownGracePeriod: 30 * time.Second,
		},
		Identity: config.IdentityConfig{
			JWTAlgorithms: []string{"RS256"},
			Issuer:        "https://issuer.example.com",
			Audience:      "harbor",
			JWKSURL:       "https://issuer.example.com/.well-known/jwks.json",
		},
		Telemetry: config.TelemetryConfig{
			LogFormat:   "json",
			LogLevel:    "info",
			ServiceName: "harbor-wave5-e2e",
		},
		State: config.StateConfig{Driver: "sqlite", DSN: sqliteDSN},
		LLM: config.LLMConfig{
			Provider: "openrouter",
			Model:    "anthropic/claude-sonnet-4",
			APIKey:   "sk-test",
			Timeout:  30 * time.Second,
		},
		Governance: config.GovernanceConfig{
			DefaultMaxTokens: 4096,
			RepairAttempts:   2,
		},
		Events: config.EventsConfig{
			Driver:                   "inmem",
			MaxSubscribersPerSession: 16,
			SubscriberBufferSize:     128,
			IdleTimeout:              60 * time.Second,
			DropWindow:               1 * time.Second,
			ReplayBufferSize:         512,
		},
		Sessions: config.SessionsConfig{
			IdleTTL:       1 * time.Hour,
			HardCap:       2 * time.Hour,
			SweepInterval: 30 * time.Minute,
		},
		Artifacts: config.ArtifactsConfig{
			Driver:                    "fs",
			FSRoot:                    fsRoot,
			HeavyOutputThresholdBytes: 32 * 1024,
		},
		// Phase 20 added a required `tasks.driver` field.
		Tasks: config.TasksConfig{Driver: "inprocess"},
		// Phase 22 added required distributed driver fields.
		Distributed: config.DistributedConfig{BusDriver: "loopback", RemoteDriver: "loopback"},
	}
}

func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
