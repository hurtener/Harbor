package session_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/artifacts"
	artinmem "github.com/hurtener/Harbor/internal/artifacts/drivers/inmem"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	sessionmemory "github.com/hurtener/Harbor/internal/memory/session"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/state"
)

type cumulativeFailCommit struct {
	state.StateStore
	fail     bool
	failures int
}

func cumulativeArtifactStore(t *testing.T) artifacts.ArtifactStore {
	t.Helper()
	store, err := artinmem.New(config.ArtifactsConfig{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })
	return store
}

// ULID entropy is not an admission ordering contract. A prior ID that sorts
// after every subsequently generated ID must not prevent a legitimate rollover.
func TestRetainedCumulative_AdmissionOrderDoesNotDependOnEventID(t *testing.T) {
	for _, driver := range []string{"inmem", "sqlite"} {
		t.Run(driver, func(t *testing.T) {
			store, redactor, _ := retainedStore(t, driver)
			firstBase := retainedBase("first", "admission-order")
			first, err := sessionmemory.BeginRetainedRun(t.Context(), store, redactor, firstBase.Quadruple, 1, time.Hour, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := first.Apply(&firstBase); err != nil {
				t.Fatal(err)
			}
			if err := first.Finish(t.Context(), firstBase.Trajectory, firstBase.Query, "done", "complete"); err != nil {
				t.Fatal(err)
			}
			q := identity.Quadruple{Identity: firstBase.Quadruple.Identity}
			record := loadHostRecord(t, store, q, retainedKind)
			var window struct {
				Turns []struct {
					Admission struct {
						ID string `json:"id"`
					} `json:"admission"`
				} `json:"turns"`
			}
			if err := json.Unmarshal(record.Bytes, &window); err != nil {
				t.Fatal(err)
			}
			if len(window.Turns) != 1 {
				t.Fatal("missing seed turn")
			}
			record.Bytes = bytes.Replace(record.Bytes, []byte(window.Turns[0].Admission.ID), []byte("7ZZZZZZZZZZZZZZZZZZZZZZZZZ"), 1)
			replaceHostRecord(t, store, record)
			secondBase := retainedBase("second", "admission-order")
			second, err := sessionmemory.BeginRetainedRun(t.Context(), store, redactor, secondBase.Quadruple, 1, time.Hour, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := second.Apply(&secondBase); err != nil {
				t.Fatal(err)
			}
			secondBase.Budget.TokenBudget = 1
			if err := planner.NewCompressionRunner(&retainedSummaryRecorder{}).MaybeCompress(t.Context(), secondBase, secondBase.Trajectory); err != nil {
				t.Fatal(err)
			}
			if err := second.Finish(t.Context(), secondBase.Trajectory, secondBase.Query, "done", "complete"); err != nil {
				t.Fatalf("event ID ordering prevented rollover: %v", err)
			}
			stored := loadHostRecord(t, store, q, retainedKind)
			var committed struct {
				CompactedThrough struct {
					RunID    string `json:"run_id"`
					Sequence uint64 `json:"sequence"`
				} `json:"compacted_through"`
			}
			if err := json.Unmarshal(stored.Bytes, &committed); err != nil {
				t.Fatal(err)
			}
			if committed.CompactedThrough.RunID != "first" || committed.CompactedThrough.Sequence != 1 {
				t.Fatal("coverage skipped or reordered an admission")
			}
		})
	}
}

func TestRetainedCumulative_ExactReferencesSurviveRollover(t *testing.T) {
	for _, driver := range []string{"inmem", "sqlite"} {
		for _, deletion := range []string{"none", "before inference", "during inference"} {
			t.Run(driver+"/"+deletion, func(t *testing.T) {
				store, redactor, cfg := retainedStore(t, driver)
				blobs := cumulativeArtifactStore(t)
				firstBase := retainedBase("first", "exact-rollover")
				scope := artifacts.ArtifactScope{TenantID: "t", UserID: "u", SessionID: "exact-rollover"}
				receipt := []byte(`{"source":"` + strings.Repeat("x", 14585) + `","resource_id":"doc-a","version":9007199254740993,"more":false}`)
				if len(receipt) != 14660 {
					t.Fatal("wrong exact receipt fixture")
				}
				ref, err := blobs.PutBytes(t.Context(), scope, receipt, artifacts.PutOpts{MimeType: "application/json"})
				if err != nil {
					t.Fatal(err)
				}
				first, err := sessionmemory.BeginRetainedRun(t.Context(), store, redactor, firstBase.Quadruple, 1, time.Hour, nil)
				if err != nil {
					t.Fatal(err)
				}
				if err := first.Apply(&firstBase); err != nil {
					t.Fatal(err)
				}
				firstBase.Trajectory.Steps = append(firstBase.Trajectory.Steps, planner.Step{Action: planner.CallTool{Tool: "read", CallID: "receipt", Args: json.RawMessage(`{}`)}, LLMObservation: map[string]any{"tool": "read", "size_bytes": len(receipt), "truncated": true, "preview": "exact receipt", "artifact_ref": ref.ID}})
				if err := first.Finish(t.Context(), firstBase.Trajectory, firstBase.Query, "done", "complete"); err != nil {
					t.Fatal(err)
				}
				secondBase := retainedBase("second", "exact-rollover")
				second, err := sessionmemory.BeginRetainedRun(t.Context(), store, redactor, secondBase.Quadruple, 1, time.Hour, nil)
				if err != nil {
					t.Fatal(err)
				}
				if err := second.Apply(&secondBase); err != nil {
					t.Fatal(err)
				}
				secondBase.Budget.TokenBudget = 1
				if err := planner.NewCompressionRunner(&retainedSummaryRecorder{}).MaybeCompress(t.Context(), secondBase, secondBase.Trajectory); err != nil {
					t.Fatal(err)
				}
				if err := second.Finish(t.Context(), secondBase.Trajectory, secondBase.Query, "done", "complete"); err != nil {
					t.Fatal(err)
				}
				if driver == "sqlite" {
					if err := store.Close(t.Context()); err != nil {
						t.Fatal(err)
					}
					store, err = state.Open(t.Context(), cfg)
					if err != nil {
						t.Fatal(err)
					}
					defer func() { _ = store.Close(context.Background()) }()
				}
				base := retainedBase("third", "exact-rollover")
				third, err := sessionmemory.BeginRetainedRun(t.Context(), store, redactor, base.Quadruple, 1, time.Hour, nil)
				if err != nil {
					t.Fatal(err)
				}
				if err := third.Apply(&base); err != nil {
					t.Fatal(err)
				}
				if base.Trajectory.Summary == nil || strings.Contains(encodeRetained(t, base), ref.ID) {
					t.Fatal("rollover lost its checkpoint or kept replaying covered raw detail")
				}
				remove := func() {
					t.Helper()
					if found, err := blobs.Delete(t.Context(), scope, ref.ID); err != nil || !found {
						t.Fatalf("delete: %t %v", found, err)
					}
				}
				if deletion == "before inference" {
					remove()
				}
				called := false
				guarded := third.GuardPlanner(retainedDecisionFunc(func(ctx context.Context, rc planner.RunContext) (planner.Decision, error) {
					called = true
					if len(rc.RetainedResultRefs) != 1 || rc.RetainedResultRefs[0].Ref != ref.ID {
						t.Fatal("compaction lost the runtime-owned retrieval reference")
					}
					data, found, err := blobs.Get(ctx, scope, rc.RetainedResultRefs[0].Ref)
					if err != nil || !found || !bytes.Equal(data, receipt) {
						t.Fatal("rolled receipt was not byte exact")
					}
					if deletion == "during inference" {
						remove()
					}
					return planner.CallTool{Tool: "edit"}, nil
				}), blobs)
				decision, err := guarded.Next(t.Context(), base)
				if deletion == "none" {
					if err != nil || decision == nil || !called {
						t.Fatalf("authorized rolled evidence unavailable: %v", err)
					}
				} else if !errors.Is(err, sessionmemory.ErrRetainedContextUnavailable) || decision != nil || called != (deletion == "during inference") {
					t.Fatalf("deleted evidence remained actionable: called=%t decision=%v err=%v", called, decision, err)
				}
			})
		}
	}
}

func TestRetainedCumulative_RolloverAndRestartDoNotRenewExpiry(t *testing.T) {
	for _, driver := range []string{"inmem", "sqlite"} {
		t.Run(driver, func(t *testing.T) {
			store, redactor, cfg := retainedStore(t, driver)
			now := time.Date(2026, 9, 23, 0, 0, 0, 0, time.UTC)
			clock := func() time.Time { return now }
			for _, run := range []string{"first", "second", "third"} {
				base := retainedBase(run, "cumulative-expiry")
				retained, err := sessionmemory.BeginRetainedRun(t.Context(), store, redactor, base.Quadruple, 1, time.Hour, clock)
				if err != nil {
					t.Fatal(err)
				}
				if err := retained.Apply(&base); err != nil {
					t.Fatal(err)
				}
				base.Budget.TokenBudget = 1
				if err := planner.NewCompressionRunner(&retainedSummaryRecorder{}).MaybeCompress(t.Context(), base, base.Trajectory); err != nil {
					t.Fatal(err)
				}
				if err := retained.Finish(t.Context(), base.Trajectory, base.Query, "done", "complete"); err != nil {
					t.Fatal(err)
				}
				now = now.Add(20 * time.Minute)
			}
			// The first source expires at exactly this clock value, although the
			// last rollover happened only twenty minutes ago.
			if driver == "sqlite" {
				if err := store.Close(t.Context()); err != nil {
					t.Fatal(err)
				}
				var err error
				store, err = state.Open(t.Context(), cfg)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = store.Close(context.Background()) }()
			}
			base := retainedBase("after-expiry", "cumulative-expiry")
			retained, err := sessionmemory.BeginRetainedRun(t.Context(), store, redactor, base.Quadruple, 1, time.Hour, clock)
			if err != nil {
				t.Fatal(err)
			}
			if err := retained.Apply(&base); err != nil {
				t.Fatal(err)
			}
			body := encodeRetained(t, base)
			if base.Trajectory.Summary != nil || strings.Contains(body, "Keep the approved navigation") || !strings.Contains(body, `"historical_context_partial":true`) || !strings.Contains(body, `"source_run":"third"`) {
				t.Fatal("expiry was renewed by compaction/restart or removed unexpired tail")
			}
		})
	}
}

func TestRetainedCumulative_RolloverPreservesNewerSiblingTail(t *testing.T) {
	for _, driver := range []string{"inmem", "sqlite"} {
		t.Run(driver, func(t *testing.T) {
			store, redactor, _ := retainedStore(t, driver)
			admit := func(run string) (*sessionmemory.RetainedRun, planner.RunContext) {
				t.Helper()
				base := retainedBase(run, "cumulative-siblings")
				r, err := sessionmemory.BeginRetainedRun(t.Context(), store, redactor, base.Quadruple, 3, time.Hour, nil)
				if err != nil {
					t.Fatal(err)
				}
				if err := r.Apply(&base); err != nil {
					t.Fatal(err)
				}
				return r, base
			}
			for _, run := range []string{"seed-one", "seed-two"} {
				r, base := admit(run)
				if err := r.Finish(t.Context(), base.Trajectory, base.Query, run, "complete"); err != nil {
					t.Fatal(err)
				}
			}
			older, a := admit("older")
			a.Budget.TokenBudget = 1
			if err := planner.NewCompressionRunner(&retainedSummaryRecorder{}).MaybeCompress(t.Context(), a, a.Trajectory); err != nil {
				t.Fatal(err)
			}
			newer, b := admit("newer")
			b.Trajectory.Steps = append(b.Trajectory.Steps, planner.Step{LLMObservation: "NEWER-EXACT-OUTCOME"})
			if err := newer.Finish(t.Context(), b.Trajectory, b.Query, "newer finished first", "complete"); err != nil {
				t.Fatal(err)
			}
			if err := older.Finish(t.Context(), a.Trajectory, a.Query, "older finished last", "complete"); err != nil {
				t.Fatal(err)
			}
			_, next := admit("next")
			if next.Trajectory.Summary == nil {
				t.Fatal("eligible older prefix did not publish its checkpoint")
			}
			start, err := next.Trajectory.ReplayStart()
			if err != nil {
				t.Fatal(err)
			}
			tail, err := json.Marshal(next.Trajectory.Steps[start:])
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(tail), "NEWER-EXACT-OUTCOME") || !strings.Contains(string(tail), "newer finished first") {
				t.Fatal("rollover lost or falsely covered a newer sibling")
			}
		})
	}
}

func (s *cumulativeFailCommit) SaveBatchIf(ctx context.Context, predicates []state.SlotExpectation, writes []state.StateRecord) error {
	for _, record := range writes {
		if s.fail && record.Kind == retainedKind {
			s.failures++
			return errors.New("injected cumulative publication failure")
		}
	}
	return s.StateStore.SaveBatchIf(ctx, predicates, writes)
}

func TestRetainedCumulative_FailedRolloverPreservesCommittedState(t *testing.T) {
	for _, driver := range []string{"inmem", "sqlite"} {
		for _, failure := range []string{"no summary", "write failure"} {
			t.Run(driver+"/"+failure, func(t *testing.T) {
				store, redactor, _ := retainedStore(t, driver)
				wrapped := &cumulativeFailCommit{StateStore: store}
				firstBase := retainedBase("first", "failed-rollover")
				first, err := sessionmemory.BeginRetainedRun(t.Context(), wrapped, redactor, firstBase.Quadruple, 1, time.Hour, nil)
				if err != nil {
					t.Fatal(err)
				}
				if err := first.Apply(&firstBase); err != nil {
					t.Fatal(err)
				}
				if err := first.Finish(t.Context(), firstBase.Trajectory, firstBase.Query, "done", "complete"); err != nil {
					t.Fatal(err)
				}
				base := retainedBase("second", "failed-rollover")
				second, err := sessionmemory.BeginRetainedRun(t.Context(), wrapped, redactor, base.Quadruple, 1, time.Hour, nil)
				if err != nil {
					t.Fatal(err)
				}
				if err := second.Apply(&base); err != nil {
					t.Fatal(err)
				}
				if err := second.Start(t.Context(), base); err != nil {
					t.Fatal(err)
				}
				if !second.CompactionRequired() {
					t.Fatal("full detailed window did not request compaction")
				}
				if failure == "write failure" {
					base.Budget.TokenBudget = 1
					if err := planner.NewCompressionRunner(&retainedSummaryRecorder{}).MaybeCompress(t.Context(), base, base.Trajectory); err != nil {
						t.Fatal(err)
					}
					if base.Trajectory.Summary == nil {
						t.Fatal("fixture did not generate a candidate")
					}
					wrapped.fail = true
				}
				q := identity.Quadruple{Identity: base.Quadruple.Identity}
				before, err := store.Load(t.Context(), q, retainedKind)
				if err != nil {
					t.Fatal(err)
				}
				err = second.Finish(t.Context(), base.Trajectory, base.Query, "done", "complete")
				if err == nil || (failure == "no summary" && !errors.Is(err, sessionmemory.ErrRetainedContextCapacity)) {
					t.Fatalf("failed rollover silently evicted prior context: %v", err)
				}
				if failure == "write failure" && wrapped.failures != 1 {
					t.Fatalf("fixture did not fail at the atomic publication seam: %d failures, %v", wrapped.failures, err)
				}
				after, err := store.Load(t.Context(), q, retainedKind)
				if err != nil {
					t.Fatal(err)
				}
				if after.ID != before.ID || string(after.Bytes) != string(before.Bytes) {
					t.Fatal("failure changed checkpoint, tail, coverage or admission")
				}
				journal, err := store.Load(t.Context(), base.Quadruple, state.InternalKindPrefix+"session-execution-journal")
				if err != nil || len(journal.Bytes) == 0 {
					t.Fatalf("failure deleted recoverable admitted evidence: %v", err)
				}
			})
		}
	}
}
