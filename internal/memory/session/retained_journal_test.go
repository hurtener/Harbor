package session_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	sessionmemory "github.com/hurtener/Harbor/internal/memory/session"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/state"
)

const journalHeadKind = state.InternalKindPrefix + "session-execution-journal"

func TestRetainedJournal_AtomicFramesAndTerminalCleanup(t *testing.T) {
	for _, driver := range []string{"inmem", "sqlite"} {
		t.Run(driver, func(t *testing.T) {
			store, redactor, _ := retainedStore(t, driver)
			base := retainedBase("first", "journal")
			r, err := sessionmemory.BeginRetainedRun(t.Context(), store, redactor, base.Quadruple, 2, time.Hour, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err = r.Apply(&base); err != nil {
				t.Fatal(err)
			}
			if err = r.Start(t.Context(), base); err != nil {
				t.Fatal(err)
			}
			intent := planner.Step{Action: planner.CallTool{Tool: "write", CallID: "write-1", Args: json.RawMessage(`{"id":"doc-a"}`)}}
			if err = r.BeforeDispatch(t.Context(), base, intent); err != nil {
				t.Fatal(err)
			}
			frame, err := store.Load(t.Context(), base.Quadruple, journalHeadKind+"/action/000")
			if err != nil || !strings.Contains(string(frame.Bytes), `"settled":false`) {
				t.Fatalf("no durable intent: %v", err)
			}
			result := intent
			result.LLMObservation = json.RawMessage(`{"id":"doc-a","version":9007199254740993,"more":false}`)
			result.Observation = "RAW-NOT-FOR-CONTEXT"
			result.ReasoningTrace = "PRIVATE-REASONING"
			if err = r.AfterDispatch(t.Context(), base, result); err != nil {
				t.Fatal(err)
			}
			settled, err := store.Load(t.Context(), base.Quadruple, journalHeadKind+"/action/000")
			if err != nil || settled.ID == frame.ID || !strings.Contains(string(settled.Bytes), `"settled":true`) ||
				!strings.Contains(string(settled.Bytes), `"version":9007199254740993`) ||
				strings.Contains(string(settled.Bytes), "RAW-NOT") || strings.Contains(string(settled.Bytes), "PRIVATE-REASONING") {
				t.Fatalf("settlement lost exact permitted evidence: %v", err)
			}
			// Sibling admission does not import even committed in-flight frames.
			sibling := retainedBase("sibling", "journal")
			other, err := sessionmemory.BeginRetainedRun(t.Context(), store, redactor, sibling.Quadruple, 2, time.Hour, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err = other.Apply(&sibling); err != nil {
				t.Fatal(err)
			}
			if body := encodeRetained(t, sibling); strings.Contains(body, "doc-a") || !strings.Contains(body, "unsettled_historical_runs") {
				t.Fatal("sibling imported active journal")
			}
			base.Trajectory.Steps = append(base.Trajectory.Steps, result)
			if err = r.Finish(t.Context(), base.Trajectory, base.Query, "done", "complete"); err != nil {
				t.Fatal(err)
			}
			for _, kind := range []string{journalHeadKind, journalHeadKind + "/action/000"} {
				if _, err = store.Load(t.Context(), base.Quadruple, kind); !errors.Is(err, state.ErrNotFound) {
					t.Fatal("journal grew beyond terminal window")
				}
			}
		})
	}
}

func TestRetainedJournal_RestartDoesNotReplayPendingWrite(t *testing.T) {
	store, redactor, cfg := retainedStore(t, "sqlite")
	base := retainedBase("crashed", "s")
	r, err := sessionmemory.BeginRetainedRun(t.Context(), store, redactor, base.Quadruple, 2, time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = r.Apply(&base); err != nil {
		t.Fatal(err)
	}
	if err = r.Start(t.Context(), base); err != nil {
		t.Fatal(err)
	}
	step := planner.Step{Action: planner.CallTool{Tool: "save", CallID: "ambiguous-save", Args: json.RawMessage(`{"id":"doc-a"}`)}}
	if err = r.BeforeDispatch(t.Context(), base, step); err != nil {
		t.Fatal(err)
	}
	// Simulate process loss after external dispatch but before a committed receipt.
	if err = store.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	reopened, err := state.Open(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close(context.Background()) }()
	head, err := reopened.Load(t.Context(), base.Quadruple, journalHeadKind)
	if err != nil || !strings.Contains(string(head.Bytes), `"pending":true`) {
		t.Fatalf("pending write lost on restart: %v", err)
	}
	if _, err = sessionmemory.BeginRetainedRun(t.Context(), reopened, redactor, base.Quadruple, 2, time.Hour, nil); !errors.Is(err, sessionmemory.ErrRetainedContextUnavailable) {
		t.Fatal("cold restart silently reacquired active execution")
	}
}

func TestRetainedJournal_FailClosedBoundsAndIdentity(t *testing.T) {
	for _, scenario := range []string{"wrong identity", "pending terminal", "oversize", "changed action", "expired prefix", "cancelled intent"} {
		t.Run(scenario, func(t *testing.T) {
			store, redactor, _ := retainedStore(t, "inmem")
			base := retainedBase("r", scenario)
			r, err := sessionmemory.BeginRetainedRun(t.Context(), store, redactor, base.Quadruple, 2, time.Hour, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err = r.Apply(&base); err != nil {
				t.Fatal(err)
			}
			if err = r.Start(t.Context(), base); err != nil {
				t.Fatal(err)
			}
			step := planner.Step{Action: planner.CallTool{Tool: "save", CallID: "save-1", Args: json.RawMessage(`{}`)}}
			switch scenario {
			case "wrong identity":
				other := base
				other.Quadruple.UserID = "other"
				err = r.BeforeDispatch(t.Context(), other, step)
			case "oversize":
				step.AssistantPreamble = strings.Repeat("x", 512*1024)
				err = r.BeforeDispatch(t.Context(), base, step)
			case "cancelled intent":
				ctx, cancel := context.WithCancel(t.Context())
				cancel()
				err = r.BeforeDispatch(ctx, base, step)
			case "expired prefix":
				// Session erasure is also a commit-time predicate, not just
				// a decision-boundary check.
				_, err = store.DeleteScope(t.Context(), base.Quadruple.Identity)
				if err != nil {
					t.Fatal(err)
				}
				err = r.BeforeDispatch(t.Context(), base, step)
			default:
				if err = r.BeforeDispatch(t.Context(), base, step); err != nil {
					t.Fatal(err)
				}
				if scenario == "changed action" {
					step.Action = planner.CallTool{Tool: "different", CallID: "other", Args: json.RawMessage(`{}`)}
					err = r.AfterDispatch(t.Context(), base, step)
				} else {
					err = r.Finish(t.Context(), base.Trajectory, base.Query, "done", "complete")
				}
			}
			if err == nil {
				t.Fatal("invalid dispatch checkpoint accepted")
			}
		})
	}
}

func TestRetainedJournal_SettledReceiptSurvivesStoreReopen(t *testing.T) {
	_, redactor, _ := retainedStore(t, "inmem")
	// Use the real SQLite store with an explicit fresh path for reopen.
	cfg := config.StateConfig{Driver: "sqlite", DSN: filepath.Join(t.TempDir(), "settled.sqlite")}
	durable, err := state.Open(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = durable.Close(context.Background()) }()
	base := retainedBase("r", "settled")
	r, err := sessionmemory.BeginRetainedRun(t.Context(), durable, redactor, base.Quadruple, 2, time.Hour, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = r.Apply(&base); err != nil {
		t.Fatal(err)
	}
	if err = r.Start(t.Context(), base); err != nil {
		t.Fatal(err)
	}
	step := planner.Step{Action: planner.CallTool{Tool: "read", CallID: "r", Args: json.RawMessage(`{}`)}}
	if err = r.BeforeDispatch(t.Context(), base, step); err != nil {
		t.Fatal(err)
	}
	step.LLMObservation = json.RawMessage(`{"version":18446744073709551615,"more":false}`)
	if err = r.AfterDispatch(t.Context(), base, step); err != nil {
		t.Fatal(err)
	}
	if err = durable.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	durable, err = state.Open(t.Context(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	rec, err := durable.Load(t.Context(), identity.Quadruple{Identity: base.Quadruple.Identity, RunID: "r"}, journalHeadKind+"/action/000")
	if err != nil || !strings.Contains(string(rec.Bytes), `"version":18446744073709551615`) || !strings.Contains(string(rec.Bytes), `"settled":true`) {
		t.Fatal("committed receipt lost on restart")
	}
}
