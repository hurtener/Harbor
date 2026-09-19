package runctx_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/agentcfg/sessionfence"
	"github.com/hurtener/Harbor/internal/audit"
	"github.com/hurtener/Harbor/internal/config"
	_ "github.com/hurtener/Harbor/internal/drivers/prod" // Exercise the same stores and redactor as production assembly.
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/runtime/runctx"
	"github.com/hurtener/Harbor/internal/state"
)

const retainedKind = state.InternalKindPrefix + "session-execution-context"

func retainedStore(t *testing.T, driver string) (state.StateStore, audit.Redactor, config.StateConfig) {
	t.Helper()
	cfg := config.StateConfig{Driver: driver}
	if driver == "sqlite" {
		cfg.DSN = filepath.Join(t.TempDir(), "context.sqlite")
	}
	store, err := state.Open(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })
	redactor, err := audit.Open(t.Context(), config.AuditConfig{})
	if err != nil {
		t.Fatal(err)
	}
	return store, redactor, cfg
}

func retainedBase(run, session string) planner.RunContext {
	return planner.RunContext{Quadruple: identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: session}, RunID: run}, Query: "edit the document", Trajectory: &planner.Trajectory{Query: "edit the document"}}
}
func encodeRetained(t *testing.T, base planner.RunContext) string {
	t.Helper()
	data, err := base.Trajectory.Serialize()
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestRetainedContext_ExactEvidenceRestartAndNoRecursiveHistory(t *testing.T) {
	for _, driver := range []string{"inmem", "sqlite"} {
		t.Run(driver, func(t *testing.T) {
			store, redactor, cfg := retainedStore(t, driver)
			base := retainedBase("first", "s")
			r, err := runctx.BeginRetainedRun(t.Context(), store, redactor, base.Quadruple, 2, time.Hour, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err = r.Apply(&base); err != nil {
				t.Fatal(err)
			}
			receipt := json.RawMessage(`{"body":"` + strings.Repeat("x", 14564) + `","resource_id":"doc-a","version":9007199254740993,"more":false}`)
			base.Trajectory.Steps = append(base.Trajectory.Steps, planner.Step{Action: planner.CallTool{Tool: "read", CallID: "r1", Args: json.RawMessage(`{"id":"doc-a"}`)}, LLMObservation: receipt, Observation: "PRIVATE-RAW", ReasoningTrace: "PRIVATE-REASONING"})
			if err = r.Finish(t.Context(), base.Trajectory, base.Query, "done", "complete"); err != nil {
				t.Fatal(err)
			}
			if driver == "sqlite" {
				if err = store.Close(t.Context()); err != nil {
					t.Fatal(err)
				}
				store, err = state.Open(t.Context(), cfg)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = store.Close(context.Background()) }()
			}
			next := retainedBase("second", "s")
			second, err := runctx.BeginRetainedRun(t.Context(), store, redactor, next.Quadruple, 2, time.Hour, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err = second.Apply(&next); err != nil {
				t.Fatal(err)
			}
			data := encodeRetained(t, next)
			for _, want := range []string{strings.Repeat("x", 14564), `"version":9007199254740993`, `"more":false`, `"resource_id":"doc-a"`, `"historical":`} {
				if !strings.Contains(data, want) {
					t.Fatalf("lost exact evidence %s", want[:min(50, len(want))])
				}
			}
			if strings.Contains(data, "PRIVATE-") {
				t.Fatal("diagnostic or reasoning data entered retained context")
			}
			for _, s := range next.Trajectory.Steps {
				if s.Action != nil || s.AssistantPreamble != "" {
					t.Fatal("historical actions became executable or completion-hook entries")
				}
			}
			if err = second.Finish(t.Context(), next.Trajectory, next.Query, "second done", "complete"); err != nil {
				t.Fatal(err)
			}
			record, err := store.Load(t.Context(), identity.Quadruple{Identity: base.Quadruple.Identity}, retainedKind)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Count(string(record.Bytes), "9007199254740993") != 1 {
				t.Fatal("inherited history was persisted recursively")
			}
			if err = second.Finish(t.Context(), next.Trajectory, next.Query, "again", "complete"); !errors.Is(err, runctx.ErrRetainedContextUnavailable) {
				t.Fatalf("double finish accepted: %v", err)
			}
		})
	}
}

func TestRetainedContext_WindowExpiryAndConcurrentSiblings(t *testing.T) {
	store, redactor, _ := retainedStore(t, "inmem")
	now := time.Date(2026, 9, 19, 0, 0, 0, 0, time.UTC)
	clock := func() time.Time { return now }
	a, b := retainedBase("a", "s"), retainedBase("b", "s")
	first, err := runctx.BeginRetainedRun(t.Context(), store, redactor, a.Quadruple, 2, time.Minute, clock)
	if err != nil {
		t.Fatal(err)
	}
	if err = first.Apply(&a); err != nil {
		t.Fatal(err)
	}
	second, err := runctx.BeginRetainedRun(t.Context(), store, redactor, b.Quadruple, 2, time.Minute, clock)
	if err != nil {
		t.Fatal(err)
	}
	if err = second.Apply(&b); err != nil {
		t.Fatal(err)
	}
	a.Trajectory.Steps = append(a.Trajectory.Steps, planner.Step{LLMObservation: "FIRST-SAVE"})
	if strings.Contains(encodeRetained(t, b), "FIRST-SAVE") {
		t.Fatal("in-flight sibling context leaked")
	}
	if err = second.Finish(t.Context(), b.Trajectory, "second request", "SECOND-OUTCOME", "complete"); err != nil {
		t.Fatal(err)
	}
	if err = first.Finish(t.Context(), a.Trajectory, "first request", "FIRST-OUTCOME", "interrupted"); err != nil {
		t.Fatal(err)
	}
	c := retainedBase("c", "s")
	third, err := runctx.BeginRetainedRun(t.Context(), store, redactor, c.Quadruple, 2, time.Minute, clock)
	if err != nil {
		t.Fatal(err)
	}
	if err = third.Apply(&c); err != nil {
		t.Fatal(err)
	}
	text := encodeRetained(t, c)
	if !strings.Contains(text, "FIRST-SAVE") || !strings.Contains(text, "SECOND-OUTCOME") || !strings.Contains(text, `"unrecorded_outcomes_possible":true`) {
		t.Fatal("lost committed outcomes or invented complete execution")
	}
	if err = third.Finish(t.Context(), c.Trajectory, "third", "THIRD-OUTCOME", "complete"); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Minute)
	d := retainedBase("d", "s")
	fourth, err := runctx.BeginRetainedRun(t.Context(), store, redactor, d.Quadruple, 2, time.Minute, clock)
	if err != nil {
		t.Fatal(err)
	}
	if err = fourth.Apply(&d); err != nil {
		t.Fatal(err)
	}
	text = encodeRetained(t, d)
	if strings.Contains(text, "OUTCOME") || !strings.Contains(text, `"historical_context_partial":true`) {
		t.Fatal("expired evidence resurrected or omission not exposed")
	}
}

func TestRetainedContext_ErasureAndCorruptionFailClosed(t *testing.T) {
	for _, scenario := range []string{"deleted-admission", "pending", "tombstone", "invalid-version", "null", "trailing"} {
		t.Run(scenario, func(t *testing.T) {
			store, redactor, _ := retainedStore(t, "inmem")
			base := retainedBase("run", scenario)
			r, err := runctx.BeginRetainedRun(t.Context(), store, redactor, base.Quadruple, 2, time.Hour, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err = r.Apply(&base); err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "deleted-admission":
				if _, err = store.DeleteScope(t.Context(), base.Quadruple.Identity); err != nil {
					t.Fatal(err)
				}
			case "pending", "tombstone":
				q, k, e := sessionfence.PendingSlot(base.Quadruple)
				if scenario == "tombstone" {
					q, k, e = sessionfence.TombstoneSlot(base.Quadruple)
				}
				if e != nil {
					t.Fatal(e)
				}
				if err = store.Save(t.Context(), state.StateRecord{ID: state.NewEventID(), Identity: q, Kind: k, Bytes: []byte(`{}`)}); err != nil {
					t.Fatal(err)
				}
			default:
				q := identity.Quadruple{Identity: base.Quadruple.Identity}
				old, e := store.Load(t.Context(), q, retainedKind)
				if e != nil {
					t.Fatal(e)
				}
				body := `{"version":999}`
				if scenario == "null" {
					body = `null`
				}
				if scenario == "trailing" {
					body = `{} {}`
				}
				if err = store.SaveIf(t.Context(), []state.SlotExpectation{state.InternalSlotExpectation(q, retainedKind, old.ID)}, state.NewInternalRecord(state.NewEventID(), q, retainedKind, []byte(body))); err != nil {
					t.Fatal(err)
				}
			}
			if err = r.Finish(t.Context(), base.Trajectory, "request", "DONE", "complete"); !errors.Is(err, runctx.ErrRetainedContextUnavailable) {
				t.Fatalf("untrusted reconstruction accepted: %v", err)
			}
		})
	}
}

type retainedFailStore struct {
	state.StateStore
	fail bool
}

func (s *retainedFailStore) SaveIf(ctx context.Context, p []state.SlotExpectation, r state.StateRecord) error {
	if s.fail {
		return errors.New("write failed")
	}
	return s.StateStore.SaveIf(ctx, p, r)
}

type retainedFailRedactor struct{}

func (retainedFailRedactor) Redact(context.Context, any) (any, error) {
	return nil, audit.ErrRedactionFailed
}

func TestRetainedContext_RequiredWriteAndRedaction(t *testing.T) {
	store, redactor, _ := retainedStore(t, "inmem")
	f := &retainedFailStore{StateStore: store, fail: true}
	base := retainedBase("run", "s")
	if r, err := runctx.BeginRetainedRun(t.Context(), f, redactor, base.Quadruple, 2, time.Hour, nil); err == nil || r != nil {
		t.Fatal("admitted without required persistence")
	}
	f.fail = false
	r, err := runctx.BeginRetainedRun(t.Context(), f, retainedFailRedactor{}, base.Quadruple, 2, time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = r.Apply(&base); err != nil {
		t.Fatal(err)
	}
	if err = r.Finish(t.Context(), base.Trajectory, "SECRET", "", "complete"); !errors.Is(err, audit.ErrRedactionFailed) {
		t.Fatalf("redaction fail open: %v", err)
	}
	record, err := store.Load(t.Context(), identity.Quadruple{Identity: base.Quadruple.Identity}, retainedKind)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(record.Bytes), "SECRET") {
		t.Fatal("unredacted content retained")
	}
	for _, size := range []int{0, 33, -1} {
		if _, err = runctx.BeginRetainedRun(t.Context(), store, redactor, retainedBase("other", "s").Quadruple, size, time.Hour, nil); err == nil {
			t.Fatal("invalid capacity")
		}
	}
}

func TestRetainedContext_SharedStoreIsolation(t *testing.T) {
	store, redactor, _ := retainedStore(t, "inmem")
	var wg sync.WaitGroup
	for i := range 128 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			marker := fmt.Sprintf("session-%03d", i)
			base := retainedBase("run-1", marker)
			r, err := runctx.BeginRetainedRun(t.Context(), store, redactor, base.Quadruple, 2, time.Hour, nil)
			if err != nil {
				t.Error(err)
				return
			}
			if err = r.Apply(&base); err != nil {
				t.Error(err)
				return
			}
			base.Trajectory.Steps = append(base.Trajectory.Steps, planner.Step{LLMObservation: marker})
			if err = r.Finish(t.Context(), base.Trajectory, marker, "done", "complete"); err != nil {
				t.Error(err)
				return
			}
			next := retainedBase("run-2", marker)
			r, err = runctx.BeginRetainedRun(t.Context(), store, redactor, next.Quadruple, 2, time.Hour, nil)
			if err != nil {
				t.Error(err)
				return
			}
			if err = r.Apply(&next); err != nil {
				t.Error(err)
				return
			}
			body, err := next.Trajectory.Serialize()
			if err != nil || !strings.Contains(string(body), marker) || strings.Count(string(body), "historical_user_request") != 1 {
				t.Errorf("scope %d lost or mixed evidence: %v", i, err)
			}
		}()
	}
	wg.Wait()
}

type retainedDecisionFunc func(context.Context, planner.RunContext) (planner.Decision, error)

func (f retainedDecisionFunc) Next(ctx context.Context, rc planner.RunContext) (planner.Decision, error) {
	return f(ctx, rc)
}
func TestRetainedContext_ErasureBlocksFollowingDecision(t *testing.T) {
	store, redactor, _ := retainedStore(t, "inmem")
	base := retainedBase("run", "erase-decision")
	r, err := runctx.BeginRetainedRun(t.Context(), store, redactor, base.Quadruple, 2, time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	called := false
	guarded := r.GuardPlanner(retainedDecisionFunc(func(context.Context, planner.RunContext) (planner.Decision, error) {
		called = true
		return planner.CallTool{Tool: "write"}, nil
	}))
	if _, err = store.DeleteScope(t.Context(), base.Quadruple.Identity); err != nil {
		t.Fatal(err)
	}
	if decision, err := guarded.Next(t.Context(), base); !errors.Is(err, runctx.ErrRetainedContextUnavailable) || decision != nil || called {
		t.Fatal("erasure did not block the next planner request")
	}
}

func TestRetainedContext_ErasureDuringDecisionBlocksDispatch(t *testing.T) {
	store, redactor, _ := retainedStore(t, "inmem")
	base := retainedBase("run", "erase-during")
	r, err := runctx.BeginRetainedRun(t.Context(), store, redactor, base.Quadruple, 2, time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	guarded := r.GuardPlanner(retainedDecisionFunc(func(context.Context, planner.RunContext) (planner.Decision, error) {
		if _, err := store.DeleteScope(t.Context(), base.Quadruple.Identity); err != nil {
			return nil, err
		}
		return planner.CallTool{Tool: "write"}, nil
	}))
	if decision, err := guarded.Next(t.Context(), base); !errors.Is(err, runctx.ErrRetainedContextUnavailable) || decision != nil {
		t.Fatal("dispatch accepted after source erasure during model call")
	}
}

func TestRetainedContext_CancelledAdmissionAndOversizedTerminal(t *testing.T) {
	store, redactor, _ := retainedStore(t, "inmem")
	base := retainedBase("run", "bounds")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if r, err := runctx.BeginRetainedRun(ctx, store, redactor, base.Quadruple, 2, time.Hour, nil); !errors.Is(err, context.Canceled) || r != nil {
		t.Fatal("cancelled admission accepted")
	}
	r, err := runctx.BeginRetainedRun(t.Context(), store, redactor, base.Quadruple, 2, time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = r.Apply(&base); err != nil {
		t.Fatal(err)
	}
	base.Trajectory.Steps = append(base.Trajectory.Steps, planner.Step{LLMObservation: strings.Repeat("x", 512*1024)})
	if err = r.Finish(t.Context(), base.Trajectory, "request", "done", "complete"); !errors.Is(err, runctx.ErrRetainedContextCapacity) {
		t.Fatalf("oversized evidence clipped: %v", err)
	}
}

func TestRetainedContext_LegacyWindowUpgradeIsExplicit(t *testing.T) {
	store, redactor, _ := retainedStore(t, "inmem")
	base := retainedBase("new", "legacy-migration")
	q := identity.Quadruple{Identity: base.Quadruple.Identity}
	data, err := json.Marshal(map[string]any{
		"version": 1,
		"turns": []any{map[string]any{
			"admission":  map[string]any{"id": state.NewEventID(), "run_id": "legacy"},
			"expires_at": time.Now().Add(time.Hour),
			"status":     "complete", "query": "old request",
			"steps": []any{map[string]any{
				"action":          map[string]any{"Tool": "read", "Args": map[string]any{"id": "legacy-doc"}},
				"llm_observation": map[string]any{"version": json.Number("9007199254740993")},
			}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = store.Save(t.Context(), state.NewInternalRecord(state.NewEventID(), q, retainedKind, data)); err != nil {
		t.Fatal(err)
	}
	run, err := runctx.BeginRetainedRun(t.Context(), store, redactor, base.Quadruple, 2, time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = run.Apply(&base); err != nil {
		t.Fatal(err)
	}
	for _, step := range base.Trajectory.Steps {
		if step.Historical != nil || step.Action != nil {
			t.Fatal("legacy action acquired an invented native kind")
		}
	}
	if body := encodeRetained(t, base); !strings.Contains(body, "legacy-doc") || !strings.Contains(body, "9007199254740993") {
		t.Fatal("migration lost legacy evidence")
	}
	record, err := store.Load(t.Context(), q, retainedKind)
	if err != nil {
		t.Fatal(err)
	}
	var window struct {
		Version int `json:"version"`
	}
	if err = json.Unmarshal(record.Bytes, &window); err != nil || window.Version != 2 {
		t.Fatal("new representation is not fenced from old v1 readers")
	}
}

// Corruption must be rejected before admission, not merely by the eventual
// native renderer after a summarizer could already have consumed the record.
func TestRetainedContext_InvalidHistoricalBodyBlocksAdmission(t *testing.T) {
	for _, driver := range []string{"inmem", "sqlite"} {
		t.Run(driver, func(t *testing.T) {
			store, redactor, _ := retainedStore(t, driver)
			base := retainedBase("first", "corrupt-history")
			first, err := runctx.BeginRetainedRun(t.Context(), store, redactor, base.Quadruple, 2, time.Hour, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err = first.Apply(&base); err != nil {
				t.Fatal(err)
			}
			base.Trajectory.Steps = append(base.Trajectory.Steps, planner.Step{Action: planner.CallTool{Tool: "read", Args: json.RawMessage(`{}`)}, LLMObservation: "good"})
			if err = first.Finish(t.Context(), base.Trajectory, "read", "done", "complete"); err != nil {
				t.Fatal(err)
			}
			q := identity.Quadruple{Identity: base.Quadruple.Identity}
			old, err := store.Load(t.Context(), q, retainedKind)
			if err != nil {
				t.Fatal(err)
			}
			var window map[string]json.RawMessage
			if err = json.Unmarshal(old.Bytes, &window); err != nil {
				t.Fatal(err)
			}
			var turns []map[string]json.RawMessage
			if err = json.Unmarshal(window["turns"], &turns); err != nil {
				t.Fatal(err)
			}
			badStep := planner.Step{Historical: &planner.HistoricalStep{Version: 1, SourceRun: "first", Kind: "call_tool", Body: json.RawMessage(`{"action":{"Tool":"read"},"reasoning_trace":"PRIVATE-CONTENT"}`)}}
			turns[0]["steps"], err = json.Marshal([]planner.Step{badStep})
			if err != nil {
				t.Fatal(err)
			}
			window["turns"], err = json.Marshal(turns)
			if err != nil {
				t.Fatal(err)
			}
			data, err := json.Marshal(window)
			if err != nil {
				t.Fatal(err)
			}
			if err = store.SaveIf(t.Context(), []state.SlotExpectation{state.InternalSlotExpectation(q, retainedKind, old.ID)}, state.NewInternalRecord(state.NewEventID(), q, retainedKind, data)); err != nil {
				t.Fatal(err)
			}
			next := retainedBase("second", "corrupt-history")
			if r, err := runctx.BeginRetainedRun(t.Context(), store, redactor, next.Quadruple, 2, time.Hour, nil); !errors.Is(err, runctx.ErrRetainedContextUnavailable) || r != nil {
				t.Fatalf("invalid history admitted: %v", err)
			}
		})
	}
}
