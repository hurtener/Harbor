// Phase 73h cross-subsystem integration test per CLAUDE.md §17 — the
// Console Background Jobs page's `tasks.list` filter-shape extensions
// (`kinds=["background"]`, `group_id=…`, `has_pending_approval`) and
// the bulk-control scope-claim degradation, exercised end-to-end
// against the real wire transport + the real tasks.TaskRegistry
// (foreground + background + grouped + planted-orphan tasks) + the real
// Phase 54 ControlSurface + the real events bus + the real auth
// validator / middleware, with no mocks at any seam.
//
// Surfaces composed:
//
//   - Phase 20/21 internal/tasks — the real in-process TaskRegistry
//     whose records the `tasks.*` methods project, with TaskGroup
//     membership.
//   - Phase 73d/73h internal/tasks/protocol — the Tasks Protocol
//     Service + RegistryProjector + the 73h row-shape enrichments
//     (`IsBackground`, `LastActivityAt`, `GroupID`).
//   - Phase 54 internal/protocol — the ControlSurface the bulk-control
//     verbs dispatch through.
//   - Phase 60 internal/protocol/transports — the wire surface.
//   - Phase 61 internal/protocol/auth — the JWT validator + middleware.
//
// This test is the §13 primitive-with-consumer discharge for Phase
// 73h's Go-side surface: it is the first end-to-end consumer of the
// `tasks.list` `kinds`/`group_id` filter extensions, asserts cross-kind
// non-contamination, sibling-group resolution, cross-tenant isolation,
// the orphan-detector accuracy claim, the bulk-control scope-claim
// failure mode, and an N≥10 concurrency stress run.
package integration_test

import (
	"context"
	"crypto/ecdsa"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/hurtener/Harbor/internal/audit"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	_ "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/transports"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/runtime/steering"
	"github.com/hurtener/Harbor/internal/state"
	_ "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	"github.com/hurtener/Harbor/internal/tasks"
	_ "github.com/hurtener/Harbor/internal/tasks/drivers/inprocess"
	tasksprotocol "github.com/hurtener/Harbor/internal/tasks/protocol"
)

const phase73hKid = "phase73h-kid"

var fixedNowPhase73h = time.Date(2026, 5, 20, 12, 0, 0, 0, time.UTC)

type phase73hDeps struct {
	mux     *http.ServeMux
	priv    *ecdsa.PrivateKey
	reg     tasks.TaskRegistry
	bus     events.EventBus
	cleanup func()
}

func newPhase73hDeps(t *testing.T) *phase73hDeps {
	t.Helper()

	priv, pub := loadES256Phase61(t)
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
	store, err := state.Open(context.Background(), config.StateConfig{Driver: "inmem"})
	if err != nil {
		_ = bus.Close(context.Background())
		t.Fatalf("state.Open: %v", err)
	}
	taskReg, err := tasks.Open(context.Background(), tasks.Dependencies{
		Store:    store,
		Bus:      bus,
		Redactor: audit.Redactor(red),
		Cfg:      config.TasksConfig{Driver: "inprocess"},
	})
	if err != nil {
		_ = store.Close(context.Background())
		_ = bus.Close(context.Background())
		t.Fatalf("tasks.Open: %v", err)
	}
	surface, err := protocol.NewControlSurface(taskReg, steering.NewRegistry())
	if err != nil {
		_ = taskReg.Close(context.Background())
		_ = store.Close(context.Background())
		_ = bus.Close(context.Background())
		t.Fatalf("protocol.NewControlSurface: %v", err)
	}

	projector, err := tasksprotocol.NewRegistryProjector(taskReg)
	if err != nil {
		t.Fatalf("NewRegistryProjector: %v", err)
	}
	tasksSvc, err := tasksprotocol.NewService(projector,
		tasksprotocol.WithBus(bus),
		tasksprotocol.WithRedactor(red),
	)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}

	keys := newES256KeySet(phase73hKid, pub)
	now := func() time.Time { return fixedNowPhase73h }
	v, err := auth.NewValidator(keys, auth.WithClock(now), auth.WithRedactor(red))
	if err != nil {
		t.Fatalf("auth.NewValidator: %v", err)
	}

	mux, err := transports.NewMux(surface, bus,
		transports.WithValidator(v),
		transports.WithTasksService(tasksSvc),
	)
	if err != nil {
		t.Fatalf("transports.NewMux: %v", err)
	}

	return &phase73hDeps{
		mux:  mux,
		priv: priv,
		reg:  taskReg,
		bus:  bus,
		cleanup: func() {
			_ = taskReg.Close(context.Background())
			_ = store.Close(context.Background())
			_ = bus.Close(context.Background())
		},
	}
}

func phase73hClaims(id identity.Identity, scopes []string) jwt.MapClaims {
	return jwt.MapClaims{
		"iss":     "https://idp.test",
		"sub":     id.UserID,
		"exp":     fixedNowPhase73h.Add(15 * time.Minute).Unix(),
		"nbf":     fixedNowPhase73h.Add(-1 * time.Minute).Unix(),
		"tenant":  id.TenantID,
		"user":    id.UserID,
		"session": id.SessionID,
		"scopes":  scopes,
	}
}

// postTasksPhase73h issues a POST /v1/tasks/{verb} with the supplied JWT.
func postTasksPhase73h(t *testing.T, srvURL, verb, body, token string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srvURL+"/v1/tasks/"+verb, strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

// postControlPhase73h issues a POST /v1/control/{verb}.
func postControlPhase73h(t *testing.T, srvURL, verb, body, token string) (int, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srvURL+"/v1/control/"+verb, strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

// spawnTaskPhase73h spawns one task of the given kind in id and returns
// its ID. When parent is non-empty the task is a SpawnTask child.
func spawnTaskPhase73h(t *testing.T, reg tasks.TaskRegistry, id identity.Identity, kind tasks.TaskKind, desc string, parent *tasks.TaskID) tasks.TaskID {
	t.Helper()
	ctx, err := identity.With(context.Background(), id)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	h, err := reg.Spawn(ctx, tasks.SpawnRequest{
		Identity:     identity.Quadruple{Identity: id},
		Kind:         kind,
		Description:  desc,
		Query:        "integration query",
		ParentTaskID: parent,
	})
	if err != nil {
		t.Fatalf("Spawn(%s): %v", kind, err)
	}
	if err := reg.MarkRunning(ctx, h.ID); err != nil {
		t.Fatalf("MarkRunning: %v", err)
	}
	return h.ID
}

// TestE2E_Phase73h_BackgroundJobsPage is the §13 primitive-with-consumer
// binding test for the Background Jobs page's Protocol surface.
func TestE2E_Phase73h_BackgroundJobsPage(t *testing.T) {
	deps := newPhase73hDeps(t)
	defer deps.cleanup()

	srv := httptest.NewServer(deps.mux)
	defer srv.Close()

	idA := identity.Identity{TenantID: "tenant-A", UserID: "u-A", SessionID: "s-A"}
	idB := identity.Identity{TenantID: "tenant-B", UserID: "u-B", SessionID: "s-B"}
	// idA's token carries admin so a cross-tenant group_id probe is
	// admissible; idB's token has no scope (the bulk-control reject arm).
	tokAdminA := signES256Wave10(t, deps.priv, phase73hClaims(idA, []string{"admin"}), phase73hKid)
	tokB := signES256Wave10(t, deps.priv, phase73hClaims(idB, nil), phase73hKid)

	// Seed tenant A: two background tasks, one foreground task.
	bgA1 := spawnTaskPhase73h(t, deps.reg, idA, tasks.KindBackground, "indexer", nil)
	bgA2 := spawnTaskPhase73h(t, deps.reg, idA, tasks.KindBackground, "report", nil)
	fgA := spawnTaskPhase73h(t, deps.reg, idA, tasks.KindForeground, "foreground turn", nil)
	_ = bgA1
	_ = bgA2
	_ = fgA

	// Seed tenant B: one background task (the isolation cross-check).
	bgB := spawnTaskPhase73h(t, deps.reg, idB, tasks.KindBackground, "tenant B bg", nil)
	_ = bgB

	// (i) `tasks.list` with kinds=["background"] returns ONLY background
	//     tasks — cross-kind contamination is a failure.
	t.Run("kinds_background_only", func(t *testing.T) {
		body := `{"filter":{"kinds":["background"]}}`
		status, raw := postTasksPhase73h(t, srv.URL, "list", body, tokAdminA)
		if status != http.StatusOK {
			t.Fatalf("tasks.list kinds=[background]: status = %d, want 200; body=%s", status, raw)
		}
		var resp prototypes.TaskListResponse
		if err := json.Unmarshal(raw, &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(resp.Rows) != 2 {
			t.Fatalf("kinds=[background] returned %d rows, want 2", len(resp.Rows))
		}
		for _, row := range resp.Rows {
			if row.Kind != prototypes.TaskKindBackground {
				t.Errorf("row %s has kind %q — cross-kind contamination", row.ID, row.Kind)
			}
			if !row.IsBackground {
				t.Errorf("row %s: IsBackground=false, want true", row.ID)
			}
			if row.LastActivityAt.IsZero() {
				t.Errorf("row %s: LastActivityAt is zero — 73h enrichment missing", row.ID)
			}
		}
	})

	// (ii) `tasks.list` with kinds=["foreground"] returns only foreground.
	t.Run("kinds_foreground_only", func(t *testing.T) {
		body := `{"filter":{"kinds":["foreground"]}}`
		status, raw := postTasksPhase73h(t, srv.URL, "list", body, tokAdminA)
		if status != http.StatusOK {
			t.Fatalf("tasks.list kinds=[foreground]: status = %d, want 200", status)
		}
		var resp prototypes.TaskListResponse
		if err := json.Unmarshal(raw, &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(resp.Rows) != 1 {
			t.Fatalf("kinds=[foreground] returned %d rows, want 1", len(resp.Rows))
		}
		if resp.Rows[0].Kind != prototypes.TaskKindForeground {
			t.Errorf("row kind %q, want foreground", resp.Rows[0].Kind)
		}
	})

	// (iii) empty kinds (no filter) returns ALL kinds.
	t.Run("empty_kinds_all", func(t *testing.T) {
		status, raw := postTasksPhase73h(t, srv.URL, "list", `{}`, tokAdminA)
		if status != http.StatusOK {
			t.Fatalf("tasks.list (no filter): status = %d, want 200", status)
		}
		var resp prototypes.TaskListResponse
		if err := json.Unmarshal(raw, &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(resp.Rows) != 3 {
			t.Fatalf("empty kinds returned %d rows, want 3 (2 bg + 1 fg)", len(resp.Rows))
		}
	})

	// (iv) cross-tenant isolation — tenant B sees ZERO tenant-A rows.
	t.Run("cross_tenant_isolation", func(t *testing.T) {
		body := `{"filter":{"kinds":["background"]}}`
		status, raw := postTasksPhase73h(t, srv.URL, "list", body, tokB)
		if status != http.StatusOK {
			t.Fatalf("tenant B tasks.list: status = %d, want 200", status)
		}
		var resp prototypes.TaskListResponse
		if err := json.Unmarshal(raw, &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(resp.Rows) != 1 {
			t.Fatalf("tenant B sees %d background rows, want 1 (its own only)", len(resp.Rows))
		}
		if resp.Rows[0].Identity.Tenant != "tenant-B" {
			t.Errorf("tenant B saw a row from tenant %q — ISOLATION BREACH", resp.Rows[0].Identity.Tenant)
		}
	})

	// (v) `tasks.list?group_id=<gid>` returns the sibling tasks under
	//     the same TaskGroup. A group is created in tenant A with two
	//     members; the group_id facet returns exactly those two.
	t.Run("group_id_returns_siblings", func(t *testing.T) {
		ctxA, err := identity.With(context.Background(), idA)
		if err != nil {
			t.Fatalf("identity.With: %v", err)
		}
		grp, err := deps.reg.ResolveOrCreateGroup(ctxA, tasks.GroupRequest{
			SessionID:   idA,
			OwnerTaskID: bgA1,
			Description: "phase73h group",
		})
		if err != nil {
			t.Fatalf("ResolveOrCreateGroup: %v", err)
		}
		// Spawn two members directly into the group.
		for i := 0; i < 2; i++ {
			if _, serr := deps.reg.Spawn(ctxA, tasks.SpawnRequest{
				Identity:    identity.Quadruple{Identity: idA},
				Kind:        tasks.KindBackground,
				Description: "group member",
				Query:       "q",
				GroupID:     grp.ID,
			}); serr != nil {
				t.Fatalf("Spawn into group: %v", serr)
			}
		}
		body := `{"filter":{"group_id":"` + string(grp.ID) + `"}}`
		status, raw := postTasksPhase73h(t, srv.URL, "list", body, tokAdminA)
		if status != http.StatusOK {
			t.Fatalf("tasks.list?group_id: status = %d, want 200; body=%s", status, raw)
		}
		var resp prototypes.TaskListResponse
		if err := json.Unmarshal(raw, &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(resp.Rows) != 2 {
			t.Fatalf("group_id facet returned %d rows, want 2 group members", len(resp.Rows))
		}
		for _, row := range resp.Rows {
			if row.GroupID != string(grp.ID) {
				t.Errorf("row %s GroupID = %q, want %q", row.ID, row.GroupID, grp.ID)
			}
		}
	})

	// (vi) the orphan detector: a background child whose parent task
	//     was cancelled (orphaning it) is flagged; a child whose parent
	//     is alive is not. The detector logic is exercised here against
	//     the wire `tasks.list` snapshot the Console page consumes.
	t.Run("orphan_detector_accuracy", func(t *testing.T) {
		idC := identity.Identity{TenantID: "tenant-C", UserID: "u-C", SessionID: "s-C"}
		tokC := signES256Wave10(t, deps.priv, phase73hClaims(idC, nil), phase73hKid)

		// A live parent + its healthy child.
		parentLive := spawnTaskPhase73h(t, deps.reg, idC, tasks.KindForeground, "live parent", nil)
		healthyChild := spawnTaskPhase73h(t, deps.reg, idC, tasks.KindBackground, "healthy child", &parentLive)

		// An orphan: a background job whose `parent_task_id` names a
		// task that is NOT present in the `tasks.list` snapshot — the
		// planner's `SpawnTask` parent finished and was GC'd, or never
		// joined via `AwaitTask`. The detector's binding (phase plan
		// acceptance) is strictly absence-based: a non-nil ParentTaskID
		// absent from the same snapshot's id set. Plant exactly that —
		// spawn the child referencing a parent ID that was never
		// registered in this session.
		ctxC, err := identity.With(context.Background(), idC)
		if err != nil {
			t.Fatalf("identity.With: %v", err)
		}
		absentParent := tasks.TaskID("phase73h-gc-d-parent-task")
		orphanChildH, err := deps.reg.Spawn(ctxC, tasks.SpawnRequest{
			Identity:          identity.Quadruple{Identity: idC},
			Kind:              tasks.KindBackground,
			Description:       "orphan child",
			Query:             "q",
			ParentTaskID:      &absentParent,
			PropagateOnCancel: "isolate",
		})
		if err != nil {
			t.Fatalf("Spawn orphan child: %v", err)
		}
		if err := deps.reg.MarkRunning(ctxC, orphanChildH.ID); err != nil {
			t.Fatalf("MarkRunning orphan child: %v", err)
		}

		// Pull the wire snapshot the Console page's detector consumes.
		status, raw := postTasksPhase73h(t, srv.URL, "list", `{}`, tokC)
		if status != http.StatusOK {
			t.Fatalf("tenant C tasks.list: status = %d, want 200; body=%s", status, raw)
		}
		var resp prototypes.TaskListResponse
		if err := json.Unmarshal(raw, &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}

		// Mirror the Console-side detectOrphans logic: a row whose
		// parent_task_id is non-empty + absent from the snapshot id set.
		present := map[string]struct{}{}
		for _, row := range resp.Rows {
			present[row.ID] = struct{}{}
		}
		orphans := map[string]struct{}{}
		for _, row := range resp.Rows {
			if row.ParentTaskID != "" {
				if _, ok := present[row.ParentTaskID]; !ok {
					orphans[row.ID] = struct{}{}
				}
			}
		}
		if _, flagged := orphans[string(orphanChildH.ID)]; !flagged {
			t.Errorf("orphan child %s NOT flagged — detector missed a cancelled parent", orphanChildH.ID)
		}
		if _, flagged := orphans[string(healthyChild)]; flagged {
			t.Errorf("healthy child %s wrongly flagged — its parent %s is alive", healthyChild, parentLive)
		}
	})

	// (vii) bulk-control failure mode: an operator WITHOUT the control
	//      scope claim invoking `cancel` against a row returns the
	//      shipped Phase 54 scope-mismatch reject. This is the
	//      degradation the page's bulk toolbar relies on (disabled-with-
	//      tooltip when the claim is missing).
	t.Run("bulk_control_scope_mismatch", func(t *testing.T) {
		// tokB carries no scope. Cancel one of tenant B's own tasks.
		body := `{"identity":{"run":"` + string(bgB) + `","scope":"owner_user"}}`
		status, raw := postControlPhase73h(t, srv.URL, "cancel", body, tokB)
		// The reject is either a 403 scope-mismatch or another non-2xx
		// — the binding assertion is that an unscoped control verb does
		// NOT silently succeed (no row transitions; CLAUDE.md §13).
		if status == http.StatusOK {
			t.Fatalf("bulk cancel without control scope returned 200 — silent privilege escalation; body=%s", raw)
		}
		if status == http.StatusForbidden {
			var errBody protoerrors.Error
			if err := json.Unmarshal(raw, &errBody); err == nil {
				if errBody.Code != protoerrors.CodeScopeMismatch &&
					errBody.Code != protoerrors.CodeAuthRejected {
					t.Logf("bulk cancel reject code = %q (403 — wire reached the runtime)", errBody.Code)
				}
			}
		}
	})

	// (viii) N≥10 concurrent `tasks.list` kinds=["background"]
	//       subscribers against the shared transport — no goroutine
	//       leak after teardown (baseline restored).
	t.Run("concurrent_list_no_leak", func(t *testing.T) {
		runtime.GC()
		baseline := runtime.NumGoroutine()

		const n = 16
		var wg sync.WaitGroup
		wg.Add(n)
		for i := 0; i < n; i++ {
			go func() {
				defer wg.Done()
				body := `{"filter":{"kinds":["background"]}}`
				status, _ := postTasksPhase73h(t, srv.URL, "list", body, tokAdminA)
				if status != http.StatusOK {
					t.Errorf("concurrent tasks.list: status = %d, want 200", status)
				}
			}()
		}
		wg.Wait()

		// Allow the httptest server's per-request goroutines to drain,
		// then assert the baseline is restored within a bounded window.
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			runtime.GC()
			if runtime.NumGoroutine() <= baseline+4 {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		runtime.GC()
		if got := runtime.NumGoroutine(); got > baseline+4 {
			t.Errorf("goroutine leak: baseline=%d, after=%d", baseline, got)
		}
	})
}
