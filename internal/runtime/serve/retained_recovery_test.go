package serve

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/runtime/runctx"
	"github.com/hurtener/Harbor/internal/tasks"
)

// Seed real durable intent/settlement, never a fake reconciliation callback.
func servedRecoverySource(t *testing.T, d projWiringDeps, session string, settled bool) (*runctx.RetainedRun, planner.RunContext, planner.Step) {
	t.Helper()
	q := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: session}, RunID: "source"}
	base := planner.RunContext{Quadruple: q, Query: "edit the document", Trajectory: &planner.Trajectory{Query: "edit the document"}}
	old, err := runctx.BeginRetainedRun(t.Context(), d.in.State, d.in.Redactor, q, 4, time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := old.Apply(&base); err != nil {
		t.Fatal(err)
	}
	if err := old.Start(t.Context(), base); err != nil {
		t.Fatal(err)
	}
	step := planner.Step{Action: planner.CallTool{Tool: "save", CallID: "save-one", Args: json.RawMessage(`{"id":"doc-a"}`)}}
	if err := old.BeforeDispatch(t.Context(), base, step); err != nil {
		t.Fatal(err)
	}
	step.LLMObservation = json.RawMessage(`{"source":"` + strings.Repeat("x", 14585) + `","resource_id":"doc-a","version":9007199254740993,"more":false}`)
	if settled {
		if err := old.AfterDispatch(t.Context(), base, step); err != nil {
			t.Fatal(err)
		}
	}
	return old, base, step
}

func TestE2E_ServedContextRecovery_SealsAndContinues(t *testing.T) {
	d := buildProjWiringMux(t)
	d.in.Cfg.Sessions.RetainedContextTurns = 4
	mux, err := BuildMux(d.in)
	if err != nil {
		t.Fatal(err)
	}
	old, base, step := servedRecoverySource(t, d, "recovered", true)
	for range 2 {
		code, body := postMux(t, mux.Mux, "/v1/sessions/reconcile_context", base.Quadruple.Identity, `{"source_run_id":"source"}`)
		var response types.SessionsReconcileContextResponse
		if code != http.StatusOK || json.Unmarshal(body, &response) != nil || !response.Reconciled || response.SourceRunID != "source" || response.SessionID != "recovered" {
			t.Fatalf("reconcile %d %s", code, body)
		}
		if strings.Contains(string(body), "doc-a") {
			t.Fatal("private execution data leaked")
		}
	}
	if err := old.BeforeDispatch(t.Context(), base, step); !errors.Is(err, runctx.ErrRetainedContextUnavailable) {
		t.Fatalf("old dispatch not fenced: %v", err)
	}
	env, client, calls, memory := retainedServerHarness(t, func(o *RunLoopDriverOptions) { o.StateStore, o.Redactor = d.in.State, d.in.Redactor })
	next := retainedServerTurn(t, env, base.Quadruple.Identity, "continue the recovered document", nil)
	if next.Status != tasks.StatusComplete {
		t.Fatalf("next turn failed: %+v", next.Error)
	}
	body := client.body(next.ID)
	for _, want := range []string{"9007199254740993", "doc-a", `"more":false`, strings.Repeat("x", 14585)} {
		if !strings.Contains(body, want) {
			t.Fatalf("actual next served request missing %.60s", want)
		}
	}
	if calls.Load() != 0 || memory.calls.Load() != 0 {
		t.Fatal("recovery reran tools or consulted legacy memory")
	}
}

func TestE2E_ServedContextRecovery_RefusesPendingAndForeignIdentity(t *testing.T) {
	d := buildProjWiringMux(t)
	d.in.Cfg.Sessions.RetainedContextTurns = 4
	mux, err := BuildMux(d.in)
	if err != nil {
		t.Fatal(err)
	}
	_, base, _ := servedRecoverySource(t, d, "pending", false)
	for _, tc := range []struct {
		name string
		id   identity.Identity
		want protoerrors.Code
	}{
		{"pending", base.Quadruple.Identity, protoerrors.CodeRetainedContextUnsettled},
		{"tenant", identity.Identity{TenantID: "other", UserID: "u", SessionID: "pending"}, protoerrors.CodeRetainedContextUnavailable},
		{"user", identity.Identity{TenantID: "t", UserID: "other", SessionID: "pending"}, protoerrors.CodeRetainedContextUnavailable},
		{"session", identity.Identity{TenantID: "t", UserID: "u", SessionID: "other"}, protoerrors.CodeRetainedContextUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := auth.WithScopes(t.Context(), []auth.Scope{auth.ScopeAdmin})
			code, body := postMuxWithContext(t, mux.Mux, "/v1/sessions/reconcile_context", tc.id, `{"source_run_id":"source"}`, ctx)
			var pe protoerrors.Error
			if code != http.StatusConflict || json.Unmarshal(body, &pe) != nil || pe.Code != tc.want {
				t.Fatalf("%d %s", code, body)
			}
		})
	}
	if err := runctx.ReconcileRetainedRun(t.Context(), d.in.State, d.in.Redactor, base.Quadruple, 4, nil); !errors.Is(err, runctx.ErrRetainedContextUnsettled) {
		t.Fatal("pending state changed")
	}
}

func TestE2E_ServedContextRecovery_DisabledAndInvalid(t *testing.T) {
	d := buildProjWiringMux(t)
	disabled, err := BuildMux(d.in)
	if err != nil {
		t.Fatal(err)
	}
	id := identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}
	if code, _ := postMux(t, disabled.Mux, "/v1/sessions/reconcile_context", id, `{"source_run_id":"source"}`); code != http.StatusNotFound {
		t.Fatal("disabled recovery enabled")
	}
	d.in.Cfg.Sessions.RetainedContextTurns = 4
	enabled, err := BuildMux(d.in)
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{`{}`, `null`, `{"source_run_id":""}`, `{"source_run_id":" source"}`, `{"source_run_id":"source","force":true}`, `{"source_run_id":"source"} {}`, `{"source_run_id":"` + strings.Repeat("x", 257) + `"}`} {
		if code, resp := postMux(t, enabled.Mux, "/v1/sessions/reconcile_context", id, body); code != http.StatusBadRequest {
			t.Fatalf("invalid %d %s", code, resp)
		}
	}
	body := `{"identity":{"tenant":"t","user":"u","session":"foreign"},"source_run_id":"source"}`
	if code, _ := postMuxWithContext(t, enabled.Mux, "/v1/sessions/reconcile_context", id, body, auth.WithScopes(t.Context(), []auth.Scope{auth.ScopeAdmin})); code != http.StatusUnauthorized {
		t.Fatalf("identity spoof status %d", code)
	}
}

func TestE2E_ServedContextRecovery_ConcurrentIsolation(t *testing.T) {
	d := buildProjWiringMux(t)
	d.in.Cfg.Sessions.RetainedContextTurns = 4
	mux, err := BuildMux(d.in)
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]identity.Identity, 128)
	for i := range ids {
		_, base, _ := servedRecoverySource(t, d, fmt.Sprintf("recovery-%03d", i), true)
		ids[i] = base.Quadruple.Identity
	}
	var wg sync.WaitGroup
	for i, id := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx := t.Context()
			if i%7 == 0 {
				cancelled, cancel := context.WithCancel(ctx)
				cancel()
				ctx = cancelled
			}
			code, body := postMuxWithContext(t, mux.Mux, "/v1/sessions/reconcile_context", id, `{"source_run_id":"source"}`, ctx)
			if i%7 == 0 {
				if code == http.StatusOK {
					t.Error("cancelled recovery accepted")
				}
				return
			}
			var response types.SessionsReconcileContextResponse
			if code != http.StatusOK || json.Unmarshal(body, &response) != nil || response.SessionID != id.SessionID {
				t.Errorf("scope %s: %d %.300s", id.SessionID, code, body)
			}
		}()
	}
	wg.Wait()
}
