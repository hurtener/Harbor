package session_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	sessionmemory "github.com/hurtener/Harbor/internal/memory/session"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/state"
	stateinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
)

// Measure terminal preparation, the conditional publication and exact cleanup
// together. Admission and durable settlement have already succeeded, as in the
// runtime's finalization path. The receipt matches the concurrent-reuse fixture.
func BenchmarkRetainedContext_Terminal(b *testing.B) {
	inner, err := stateinmem.New(config.StateConfig{})
	if err != nil {
		b.Fatal(err)
	}
	store := &terminalTimedStore{StateStore: inner}
	defer func() { _ = store.Close(context.Background()) }()
	redactor := auditpatterns.New()
	result := json.RawMessage(`{"source":"` + strings.Repeat("x", 14585) + `","resource_id":"doc-a","version":9007199254740993,"more":false}`)
	b.ReportAllocs()
	var preparation, publication, cleanup time.Duration
	for i := 0; b.Loop(); i++ {
		b.StopTimer()
		base := retainedBase(fmt.Sprint(i), "terminal-benchmark")
		run, err := sessionmemory.BeginRetainedRun(b.Context(), store, redactor, base.Quadruple, 2, time.Hour, nil)
		if err != nil {
			b.Fatal(err)
		}
		if err = run.Apply(&base); err != nil {
			b.Fatal(err)
		}
		if err = run.Start(b.Context(), base); err != nil {
			b.Fatal(err)
		}
		step := planner.Step{Action: planner.CallTool{Tool: "read", CallID: "r", Args: json.RawMessage(`{}`)}}
		if err = run.BeforeDispatch(b.Context(), base, step); err != nil {
			b.Fatal(err)
		}
		step.LLMObservation = result
		if err = run.AfterDispatch(b.Context(), base, step); err != nil {
			b.Fatal(err)
		}
		base.Trajectory.Steps = append(base.Trajectory.Steps, step)
		b.StartTimer()
		store.started = time.Now()
		err = run.Finish(b.Context(), base.Trajectory, base.Query, "done", "complete")
		finished := time.Now()
		b.StopTimer()
		if err != nil {
			b.Fatal(err)
		}
		preparation += store.prepared.Sub(store.started)
		publication += store.published.Sub(store.prepared)
		cleanup += finished.Sub(store.published)
		store.started, store.prepared, store.published = time.Time{}, time.Time{}, time.Time{}
		if _, err = store.DeleteScope(b.Context(), base.Quadruple.Identity); err != nil {
			b.Fatal(err)
		}
		b.StartTimer()
	}
	b.ReportMetric(float64(preparation.Nanoseconds())/float64(b.N), "prepare-ns/op")
	b.ReportMetric(float64(publication.Nanoseconds())/float64(b.N), "publish-ns/op")
	b.ReportMetric(float64(cleanup.Nanoseconds())/float64(b.N), "cleanup-ns/op")
}

type terminalTimedStore struct {
	state.StateStore
	started, prepared, published time.Time
}

func (s *terminalTimedStore) Load(ctx context.Context, q identity.Quadruple, kind string) (state.StateRecord, error) {
	if !s.started.IsZero() && s.prepared.IsZero() {
		s.prepared = time.Now()
	}
	return s.StateStore.Load(ctx, q, kind)
}

func (s *terminalTimedStore) SaveBatchIf(ctx context.Context, checks []state.SlotExpectation, records []state.StateRecord) error {
	err := s.StateStore.SaveBatchIf(ctx, checks, records)
	if !s.started.IsZero() {
		s.published = time.Now()
	}
	return err
}
