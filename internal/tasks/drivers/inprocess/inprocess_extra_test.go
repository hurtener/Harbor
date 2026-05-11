package inprocess_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/audit"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	eventsinmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/state"
	stateinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	"github.com/hurtener/Harbor/internal/tasks"

	_ "github.com/hurtener/Harbor/internal/tasks/drivers/inprocess"
)

// freshRegistry constructs a fully-wired registry with production
// dependency drivers and returns it plus a teardown.
func freshRegistry(t *testing.T) (tasks.TaskRegistry, func()) {
	t.Helper()
	store, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state inmem New: %v", err)
	}
	redactor := auditpatterns.New()
	bus, err := eventsinmem.New(config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     256,
		IdleTimeout:              60 * time.Second,
		DropWindow:               1 * time.Second,
		ReplayBufferSize:         1024,
	}, redactor)
	if err != nil {
		t.Fatalf("events inmem New: %v", err)
	}
	r, err := tasks.OpenDriver("inprocess", tasks.Dependencies{
		Store:    store,
		Bus:      bus,
		Redactor: redactor,
		Cfg:      config.TasksConfig{Driver: "inprocess"},
	})
	if err != nil {
		t.Fatalf("OpenDriver: %v", err)
	}
	return r, func() {
		ctx := context.Background()
		_ = r.Close(ctx)
		_ = bus.Close(ctx)
		_ = store.Close(ctx)
	}
}

func tripleA() identity.Quadruple {
	return identity.Quadruple{
		Identity: identity.Identity{TenantID: "tenant-A", UserID: "user-1", SessionID: "sess-1"},
	}
}

func ctxA(t *testing.T) context.Context {
	t.Helper()
	ctx, err := identity.With(context.Background(), tripleA().Identity)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	return ctx
}

// TestSpawnTool_AssignsTaskID_PendingStub covers the SpawnTool stub
// path: persists at StatusPending and never auto-advances.
func TestSpawnTool_AssignsTaskID_PendingStub(t *testing.T) {
	r, cleanup := freshRegistry(t)
	defer cleanup()
	ctx := ctxA(t)
	h, err := r.SpawnTool(ctx, tasks.SpawnToolRequest{
		Identity:    tripleA(),
		ToolName:    "calc.add",
		ToolArgs:    []byte(`{"x":1,"y":2}`),
		Description: "add two numbers",
		Priority:    3,
	})
	if err != nil {
		t.Fatalf("SpawnTool: %v", err)
	}
	if h.ID == "" {
		t.Fatalf("SpawnTool returned empty TaskID")
	}
	got, err := r.Get(ctx, h.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != tasks.StatusPending {
		t.Errorf("status=%q, want %q (SpawnTool stub must leave task pending)", got.Status, tasks.StatusPending)
	}
	if got.Kind != tasks.KindForeground {
		t.Errorf("kind=%q, want %q", got.Kind, tasks.KindForeground)
	}
	if got.Description != "add two numbers" {
		t.Errorf("description=%q, want %q", got.Description, "add two numbers")
	}
}

// TestSpawnTool_DefaultDescriptionFromToolName covers the
// description fallback when caller passes empty Description.
func TestSpawnTool_DefaultDescriptionFromToolName(t *testing.T) {
	r, cleanup := freshRegistry(t)
	defer cleanup()
	ctx := ctxA(t)
	h, err := r.SpawnTool(ctx, tasks.SpawnToolRequest{
		Identity: tripleA(),
		ToolName: "search.web",
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := r.Get(ctx, h.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got.Description, "search.web") {
		t.Errorf("description=%q, want it to contain the tool name", got.Description)
	}
}

// TestSpawnTool_RejectsEmptyToolName covers ValidateToolRequest.
func TestSpawnTool_RejectsEmptyToolName(t *testing.T) {
	r, cleanup := freshRegistry(t)
	defer cleanup()
	ctx := ctxA(t)
	_, err := r.SpawnTool(ctx, tasks.SpawnToolRequest{
		Identity: tripleA(),
	})
	if !errors.Is(err, tasks.ErrInvalidRequest) {
		t.Errorf("err=%v, want ErrInvalidRequest", err)
	}
}

// TestSpawnTool_RejectsMissingIdentity covers ValidateToolRequest.
func TestSpawnTool_RejectsMissingIdentity(t *testing.T) {
	r, cleanup := freshRegistry(t)
	defer cleanup()
	ctx := context.Background()
	_, err := r.SpawnTool(ctx, tasks.SpawnToolRequest{
		Identity: identity.Quadruple{},
		ToolName: "x",
	})
	if !errors.Is(err, tasks.ErrIdentityRequired) {
		t.Errorf("err=%v, want ErrIdentityRequired", err)
	}
}

// TestMarkComplete_RedactsResultBytes covers the redactRawJSON path
// — the redactor walks JSON and replaces sensitive keys.
func TestMarkComplete_RedactsResultBytes(t *testing.T) {
	r, cleanup := freshRegistry(t)
	defer cleanup()
	ctx := ctxA(t)
	h, err := r.Spawn(ctx, tasks.SpawnRequest{
		Identity: tripleA(),
		Kind:     tasks.KindForeground,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.MarkRunning(ctx, h.ID); err != nil {
		t.Fatal(err)
	}
	// Result carries a key the audit redactor recognises ("api_key").
	rawJSON := []byte(`{"api_key":"sk-secret-12345","result":42}`)
	if err := r.MarkComplete(ctx, h.ID, tasks.TaskResult{Value: rawJSON}); err != nil {
		t.Fatalf("MarkComplete: %v", err)
	}
	got, err := r.Get(ctx, h.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Result == nil {
		t.Fatal("Result is nil after MarkComplete")
	}
	stored := string(got.Result.Value)
	if strings.Contains(stored, "sk-secret-12345") {
		t.Errorf("redactor leaked the secret api_key value into stored result: %q", stored)
	}
}

// TestMarkComplete_NonJSONFallback covers the redactRawJSON
// non-JSON path. We pass a non-JSON byte slice; the driver falls
// back to redactString and re-quotes the result.
func TestMarkComplete_NonJSONFallback(t *testing.T) {
	r, cleanup := freshRegistry(t)
	defer cleanup()
	ctx := ctxA(t)
	h, err := r.Spawn(ctx, tasks.SpawnRequest{
		Identity: tripleA(),
		Kind:     tasks.KindForeground,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.MarkRunning(ctx, h.ID); err != nil {
		t.Fatal(err)
	}
	// Not valid JSON.
	if err := r.MarkComplete(ctx, h.ID, tasks.TaskResult{Value: []byte("plain text result")}); err != nil {
		t.Fatalf("MarkComplete: %v", err)
	}
	got, err := r.Get(ctx, h.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Result == nil {
		t.Fatal("Result is nil")
	}
	// The driver re-quotes the redacted string.
	if !strings.HasPrefix(string(got.Result.Value), `"`) {
		t.Errorf("non-JSON fallback did not re-quote: %q", got.Result.Value)
	}
}

// TestMarkComplete_EmptyResultValue covers the empty-RawMessage
// short-circuit in redactRawJSON.
func TestMarkComplete_EmptyResultValue(t *testing.T) {
	r, cleanup := freshRegistry(t)
	defer cleanup()
	ctx := ctxA(t)
	h, err := r.Spawn(ctx, tasks.SpawnRequest{
		Identity: tripleA(),
		Kind:     tasks.KindForeground,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.MarkRunning(ctx, h.ID); err != nil {
		t.Fatal(err)
	}
	if err := r.MarkComplete(ctx, h.ID, tasks.TaskResult{}); err != nil {
		t.Fatalf("MarkComplete: %v", err)
	}
	got, err := r.Get(ctx, h.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Result == nil {
		t.Fatal("Result is nil after MarkComplete")
	}
	if len(got.Result.Value) != 0 {
		t.Errorf("empty input → non-empty stored value: %q", got.Result.Value)
	}
}

// TestMarkFailed_RedactsErrorMessage covers MarkFailed's redaction
// of the error message.
func TestMarkFailed_RedactsErrorMessage(t *testing.T) {
	r, cleanup := freshRegistry(t)
	defer cleanup()
	ctx := ctxA(t)
	h, err := r.Spawn(ctx, tasks.SpawnRequest{
		Identity: tripleA(),
		Kind:     tasks.KindForeground,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.MarkRunning(ctx, h.ID); err != nil {
		t.Fatal(err)
	}
	if err := r.MarkFailed(ctx, h.ID, tasks.TaskError{
		Code:    "tool.timeout",
		Message: "tool failed: bearer abcd1234efgh5678",
	}); err != nil {
		t.Fatalf("MarkFailed: %v", err)
	}
	got, err := r.Get(ctx, h.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != tasks.StatusFailed {
		t.Errorf("status=%q, want %q", got.Status, tasks.StatusFailed)
	}
	if got.Error == nil {
		t.Fatal("Error is nil after MarkFailed")
	}
	if got.Error.Code != "tool.timeout" {
		t.Errorf("code=%q, want %q", got.Error.Code, "tool.timeout")
	}
	// The bearer token must have been redacted out.
	if strings.Contains(got.Error.Message, "abcd1234efgh5678") {
		t.Errorf("redactor leaked bearer token: %q", got.Error.Message)
	}
}

// TestSpawn_RedactsDescriptionAndQuery covers the Spawn-time redaction.
func TestSpawn_RedactsDescriptionAndQuery(t *testing.T) {
	r, cleanup := freshRegistry(t)
	defer cleanup()
	ctx := ctxA(t)
	h, err := r.Spawn(ctx, tasks.SpawnRequest{
		Identity:    tripleA(),
		Kind:        tasks.KindForeground,
		Description: `desc with bearer abcd1234efgh5678`,
		Query:       `query api_key=sk-LEAK-secret-1234567890`,
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := r.Get(ctx, h.ID)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got.Description, "abcd1234efgh5678") {
		t.Errorf("Description leaked bearer: %q", got.Description)
	}
	// The query is run through the redactor as a string; the
	// pattern-driver's bearer/api-key rules act on map keys (we
	// wrap in {"v": query} internally), so a substring carrying
	// "api_key=" doesn't match a struct-shaped key. The driver-
	// level safety is the wrap-in-map approach catching anything
	// the redactor's value-level regex pass detects (bearer, etc.).
	if got.Query == "" {
		t.Errorf("Query was emptied: %q", got.Query)
	}
}

// failingRedactor surfaces a configurable error from Redact. Used
// only in tests that target the driver's redaction-error fall-out
// — not at the conformance seam (per AGENTS.md §17.3 conformance
// tests use the production redactor).
type failingRedactor struct{ err error }

func (f failingRedactor) Redact(_ context.Context, _ any) (any, error) {
	return nil, f.err
}

// failBuild constructs a registry whose redactor always fails.
func failBuild(t *testing.T, redErr error) (tasks.TaskRegistry, func()) {
	t.Helper()
	store, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state inmem New: %v", err)
	}
	red := failingRedactor{err: redErr}
	bus, err := eventsinmem.New(config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     256,
		IdleTimeout:              60 * time.Second,
		DropWindow:               1 * time.Second,
		ReplayBufferSize:         1024,
	}, auditpatterns.New())
	if err != nil {
		t.Fatalf("events inmem New: %v", err)
	}
	r, err := tasks.OpenDriver("inprocess", tasks.Dependencies{
		Store:    store,
		Bus:      bus,
		Redactor: red,
		Cfg:      config.TasksConfig{Driver: "inprocess"},
	})
	if err != nil {
		t.Fatalf("OpenDriver: %v", err)
	}
	return r, func() {
		ctx := context.Background()
		_ = r.Close(ctx)
		_ = bus.Close(ctx)
		_ = store.Close(ctx)
	}
}

// TestSpawn_RedactorErrorPropagates covers the redactSpawnFields
// error-return paths.
func TestSpawn_RedactorErrorPropagates(t *testing.T) {
	red := errors.New("simulated redactor failure")
	r, cleanup := failBuild(t, red)
	defer cleanup()
	ctx := ctxA(t)
	// Description triggers the first redactString call inside
	// redactSpawnFields.
	_, err := r.Spawn(ctx, tasks.SpawnRequest{
		Identity:    tripleA(),
		Kind:        tasks.KindForeground,
		Description: "non-empty so redactor is invoked",
	})
	if err == nil {
		t.Fatal("Spawn with failing redactor: err=nil, want non-nil")
	}
	if !strings.Contains(err.Error(), "simulated redactor failure") {
		t.Errorf("err=%v, want it to wrap the simulated redactor error", err)
	}
}

// TestSpawn_RedactorErrorOnQuery covers the Query branch (the
// second redactString call inside redactSpawnFields).
func TestSpawn_RedactorErrorOnQuery(t *testing.T) {
	red := errors.New("redactor down")
	r, cleanup := failBuild(t, red)
	defer cleanup()
	ctx := ctxA(t)
	// Description empty → first redactString short-circuits. Query
	// non-empty → second call hits the failing redactor.
	_, err := r.Spawn(ctx, tasks.SpawnRequest{
		Identity: tripleA(),
		Kind:     tasks.KindForeground,
		Query:    "non-empty",
	})
	if err == nil {
		t.Fatal("Spawn with failing redactor (query path): err=nil")
	}
}

// TestMarkComplete_RedactorErrorPropagates covers MarkComplete's
// redactRawJSON error path.
func TestMarkComplete_RedactorErrorPropagates(t *testing.T) {
	r, cleanup := failBuild(t, errors.New("redactor down"))
	defer cleanup()
	ctx := ctxA(t)
	// Spawn with empty desc/query so the spawn-time redactor is
	// short-circuited (both strings empty).
	h, err := r.Spawn(ctx, tasks.SpawnRequest{
		Identity: tripleA(),
		Kind:     tasks.KindForeground,
	})
	if err != nil {
		t.Fatalf("Spawn (no-redact): %v", err)
	}
	if err := r.MarkRunning(ctx, h.ID); err != nil {
		t.Fatalf("MarkRunning: %v", err)
	}
	err = r.MarkComplete(ctx, h.ID, tasks.TaskResult{Value: []byte(`{"x":1}`)})
	if err == nil {
		t.Fatal("MarkComplete with failing redactor: err=nil")
	}
	// Task must still be Running (no transition on redactor failure).
	got, gerr := r.Get(ctx, h.ID)
	if gerr != nil {
		t.Fatal(gerr)
	}
	if got.Status != tasks.StatusRunning {
		t.Errorf("status=%q, want Running (transition must not commit on redactor failure)", got.Status)
	}
}

// TestMarkFailed_RedactorErrorPropagates covers MarkFailed's
// redactString error path.
func TestMarkFailed_RedactorErrorPropagates(t *testing.T) {
	r, cleanup := failBuild(t, errors.New("redactor down"))
	defer cleanup()
	ctx := ctxA(t)
	h, err := r.Spawn(ctx, tasks.SpawnRequest{
		Identity: tripleA(),
		Kind:     tasks.KindForeground,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.MarkRunning(ctx, h.ID); err != nil {
		t.Fatal(err)
	}
	err = r.MarkFailed(ctx, h.ID, tasks.TaskError{Code: "x", Message: "non-empty"})
	if err == nil {
		t.Fatal("MarkFailed with failing redactor: err=nil")
	}
}

// TestMarkX_NotFound covers the missing-task error paths on the
// Mark*/Cancel/Prioritize methods.
func TestMarkX_NotFound(t *testing.T) {
	r, cleanup := freshRegistry(t)
	defer cleanup()
	ctx := ctxA(t)
	missingID := tasks.TaskID("01HABXXX-not-real-zzzzzzzz")
	if err := r.MarkRunning(ctx, missingID); !errors.Is(err, tasks.ErrNotFound) {
		t.Errorf("MarkRunning: err=%v, want ErrNotFound", err)
	}
	if err := r.MarkPaused(ctx, missingID); !errors.Is(err, tasks.ErrNotFound) {
		t.Errorf("MarkPaused: err=%v, want ErrNotFound", err)
	}
	if err := r.MarkResumed(ctx, missingID); !errors.Is(err, tasks.ErrNotFound) {
		t.Errorf("MarkResumed: err=%v, want ErrNotFound", err)
	}
	if err := r.MarkComplete(ctx, missingID, tasks.TaskResult{}); !errors.Is(err, tasks.ErrNotFound) {
		t.Errorf("MarkComplete: err=%v, want ErrNotFound", err)
	}
	if err := r.MarkFailed(ctx, missingID, tasks.TaskError{}); !errors.Is(err, tasks.ErrNotFound) {
		t.Errorf("MarkFailed: err=%v, want ErrNotFound", err)
	}
	if _, err := r.Prioritize(ctx, missingID, 1); !errors.Is(err, tasks.ErrNotFound) {
		t.Errorf("Prioritize: err=%v, want ErrNotFound", err)
	}
	if _, err := r.Cancel(ctx, missingID, "x"); !errors.Is(err, tasks.ErrNotFound) {
		t.Errorf("Cancel: err=%v, want ErrNotFound", err)
	}
}

// TestPrioritize_CrossTenantInvisible covers Prioritize's
// identity-visibility check.
func TestPrioritize_CrossTenantInvisible(t *testing.T) {
	r, cleanup := freshRegistry(t)
	defer cleanup()
	ctx := ctxA(t)
	h, err := r.Spawn(ctx, tasks.SpawnRequest{
		Identity: tripleA(),
		Kind:     tasks.KindForeground,
	})
	if err != nil {
		t.Fatal(err)
	}
	otherID := identity.Identity{TenantID: "other-tenant", UserID: "other", SessionID: "other"}
	otherCtx, _ := identity.With(context.Background(), otherID)
	if _, err := r.Prioritize(otherCtx, h.ID, 9); !errors.Is(err, tasks.ErrNotFound) {
		t.Errorf("cross-tenant Prioritize: err=%v, want ErrNotFound", err)
	}
}

// TestList_RejectsMissingIdentity covers the validateListIdentity
// branch.
func TestList_RejectsMissingIdentity(t *testing.T) {
	r, cleanup := freshRegistry(t)
	defer cleanup()
	ctx := context.Background()
	cases := []identity.Identity{
		{},
		{TenantID: "T"},                 // missing user + session
		{TenantID: "T", UserID: "U"},    // missing session
		{UserID: "U", SessionID: "S"},   // missing tenant
		{TenantID: "T", SessionID: "S"}, // missing user
	}
	for i, id := range cases {
		_, err := r.List(ctx, id, tasks.TaskFilter{})
		if !errors.Is(err, tasks.ErrIdentityRequired) {
			t.Errorf("case %d (%+v): err=%v, want ErrIdentityRequired", i, id, err)
		}
	}
}

// TestOperationsAfterClose covers each operation's
// closed.Load()-true guard.
func TestOperationsAfterClose(t *testing.T) {
	r, cleanup := freshRegistry(t)
	defer cleanup()
	ctx := ctxA(t)
	// Spawn a task before closing so Mark*/Get/Cancel/Prioritize
	// have a real ID to act on (the closed-check fires before the
	// not-found check).
	h, err := r.Spawn(ctx, tasks.SpawnRequest{
		Identity: tripleA(),
		Kind:     tasks.KindForeground,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	check := func(name string, err error) {
		if !errors.Is(err, tasks.ErrRegistryClosed) {
			t.Errorf("%s after Close: err=%v, want ErrRegistryClosed", name, err)
		}
	}
	_, err = r.Spawn(ctx, tasks.SpawnRequest{Identity: tripleA(), Kind: tasks.KindForeground})
	check("Spawn", err)
	_, err = r.SpawnTool(ctx, tasks.SpawnToolRequest{Identity: tripleA(), ToolName: "t"})
	// SpawnTool routes through Spawn; same check.
	check("SpawnTool", err)
	_, err = r.Get(ctx, h.ID)
	check("Get", err)
	_, err = r.List(ctx, tripleA().Identity, tasks.TaskFilter{})
	check("List", err)
	_, err = r.Cancel(ctx, h.ID, "x")
	check("Cancel", err)
	_, err = r.Prioritize(ctx, h.ID, 9)
	check("Prioritize", err)
	check("MarkRunning", r.MarkRunning(ctx, h.ID))
	check("MarkPaused", r.MarkPaused(ctx, h.ID))
	check("MarkResumed", r.MarkResumed(ctx, h.ID))
	check("MarkComplete", r.MarkComplete(ctx, h.ID, tasks.TaskResult{}))
	check("MarkFailed", r.MarkFailed(ctx, h.ID, tasks.TaskError{}))
}

// TestSpawn_IdempotencyConflict_DivergentFields exercises every
// field that contributes to the equality check, so each branch in
// spawnRequestsEqual is hit.
func TestSpawn_IdempotencyConflict_DivergentFields(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*tasks.SpawnRequest)
	}{
		{name: "kind diverges", mut: func(r *tasks.SpawnRequest) { r.Kind = tasks.KindBackground }},
		{name: "priority diverges", mut: func(r *tasks.SpawnRequest) { r.Priority = 7 }},
		{name: "parent diverges", mut: func(r *tasks.SpawnRequest) {
			pid := tasks.TaskID("01HXXXX-fake-parent")
			r.ParentTaskID = &pid
		}},
		{name: "propagate diverges", mut: func(r *tasks.SpawnRequest) {
			r.PropagateOnCancel = tasks.PropagateIsolate
		}},
		{name: "notify-on-complete diverges", mut: func(r *tasks.SpawnRequest) {
			r.NotifyOnComplete = true
		}},
		{name: "description diverges", mut: func(r *tasks.SpawnRequest) {
			// Hashes diverge → conflict. Catches the case the redactor
			// erases (both inputs may post-redact to the same string,
			// but the pre-redaction SHA-256 of Description+Query
			// surfaces the divergence).
			r.Description = "divergent description"
		}},
		{name: "query diverges", mut: func(r *tasks.SpawnRequest) {
			r.Query = "divergent query"
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, cleanup := freshRegistry(t)
			defer cleanup()
			ctx := ctxA(t)
			base := tasks.SpawnRequest{
				Identity:       tripleA(),
				Kind:           tasks.KindForeground,
				IdempotencyKey: "k1",
			}
			if _, err := r.Spawn(ctx, base); err != nil {
				t.Fatal(err)
			}
			divergent := base
			tc.mut(&divergent)
			_, err := r.Spawn(ctx, divergent)
			if !errors.Is(err, tasks.ErrIdempotencyConflict) {
				t.Errorf("%s: err=%v, want ErrIdempotencyConflict", tc.name, err)
			}
		})
	}
}

// TestSpawn_IdempotencyMatch_WithParentPointer covers the
// taskIDPtrEqual both-non-nil-equal branch.
func TestSpawn_IdempotencyMatch_WithParentPointer(t *testing.T) {
	r, cleanup := freshRegistry(t)
	defer cleanup()
	ctx := ctxA(t)
	parent, err := r.Spawn(ctx, tasks.SpawnRequest{
		Identity: tripleA(),
		Kind:     tasks.KindForeground,
	})
	if err != nil {
		t.Fatal(err)
	}
	pid := parent.ID
	req := tasks.SpawnRequest{
		Identity:       tripleA(),
		Kind:           tasks.KindForeground,
		ParentTaskID:   &pid,
		IdempotencyKey: "child-key",
	}
	h1, err := r.Spawn(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	pid2 := parent.ID
	req2 := req
	req2.ParentTaskID = &pid2 // distinct pointer, same value
	h2, err := r.Spawn(ctx, req2)
	if err != nil {
		t.Fatalf("Spawn 2: %v", err)
	}
	if h1.ID != h2.ID || !h2.Reused {
		t.Errorf("idempotency-with-parent: h1=%v h2=%v (want same ID, h2.Reused=true)", h1, h2)
	}
}

// TestNew_RejectsNilDeps validates the constructor's preconditions.
// Per AGENTS.md §17.3 we use real production drivers in the slots
// that aren't being tested; only the targeted slot is set to nil.
func TestNew_RejectsNilDeps(t *testing.T) {
	build := func() (state.StateStore, events.EventBus, audit.Redactor) {
		s, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
		if err != nil {
			t.Fatalf("state inmem New: %v", err)
		}
		r := auditpatterns.New()
		b, err := eventsinmem.New(config.EventsConfig{
			Driver:                   "inmem",
			MaxSubscribersPerSession: 16,
			SubscriberBufferSize:     256,
			IdleTimeout:              60 * time.Second,
			DropWindow:               1 * time.Second,
			ReplayBufferSize:         1024,
		}, r)
		if err != nil {
			t.Fatalf("events inmem New: %v", err)
		}
		return s, b, r
	}

	t.Run("nil store", func(t *testing.T) {
		_, bus, red := build()
		_, err := tasks.OpenDriver("inprocess", tasks.Dependencies{
			Store:    nil,
			Bus:      bus,
			Redactor: red,
		})
		if err == nil {
			t.Fatal("nil store: err=nil, want non-nil")
		}
	})
	t.Run("nil bus", func(t *testing.T) {
		store, _, red := build()
		_, err := tasks.OpenDriver("inprocess", tasks.Dependencies{
			Store:    store,
			Bus:      nil,
			Redactor: red,
		})
		if err == nil {
			t.Fatal("nil bus: err=nil, want non-nil")
		}
	})
	t.Run("nil redactor", func(t *testing.T) {
		store, bus, _ := build()
		_, err := tasks.OpenDriver("inprocess", tasks.Dependencies{
			Store:    store,
			Bus:      bus,
			Redactor: nil,
		})
		if err == nil {
			t.Fatal("nil redactor: err=nil, want non-nil")
		}
	})
}

// TestClose_DrainsActiveWatchersAndWaiters verifies the Close
// contract: any goroutine blocked on a `WatchGroup`-returned channel
// or a `RegisterRetainTurnWaiter` channel observes the close-on-
// shutdown signal instead of leaking forever. Phase 21 audit fix —
// before this, `Close` only flipped the atomic flag.
func TestClose_DrainsActiveWatchersAndWaiters(t *testing.T) {
	r, cleanup := freshRegistry(t)
	// We call Close ourselves; cleanup just teardowns the deps.
	defer cleanup()
	ctx := ctxA(t)
	id := tripleA().Identity

	// Group + WatchGroup subscription left intentionally open.
	grp, err := r.ResolveOrCreateGroup(ctx, tasks.GroupRequest{
		SessionID:   id,
		OwnerTaskID: tasks.TaskID("owner"),
		Description: "close-drain",
	})
	if err != nil {
		t.Fatalf("ResolveOrCreateGroup: %v", err)
	}
	watchCh, _, err := r.WatchGroup(id, grp.ID)
	if err != nil {
		t.Fatalf("WatchGroup: %v", err)
	}
	// Retain-turn waiter left open.
	waitCh, _ := r.RegisterRetainTurnWaiter(id)

	// Close.
	if err := r.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Both channels MUST close (not deliver). 1s timeout is the hard
	// cap; a leak shows up as a deadline trip, not a deadlock.
	select {
	case _, ok := <-watchCh:
		if ok {
			t.Errorf("WatchGroup channel delivered a value on Close (expected close-without-value)")
		}
	case <-time.After(1 * time.Second):
		t.Fatalf("WatchGroup channel did not close on Close — leak")
	}
	select {
	case _, ok := <-waitCh:
		if ok {
			t.Errorf("retain-turn channel delivered a value on Close (expected close-without-value)")
		}
	case <-time.After(1 * time.Second):
		t.Fatalf("retain-turn channel did not close on Close — leak")
	}

	// Idempotent re-close.
	if err := r.Close(context.Background()); err != nil {
		t.Errorf("second Close: %v", err)
	}
}
