package planner_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/hurtener/Harbor/internal/planner"
)

func TestCompressionDiagnostics_NoErrorContent(t *testing.T) {
	t.Parallel()
	for _, estimatorFailure := range []bool{false, true} {
		t.Run(fmt.Sprint(estimatorFailure), func(t *testing.T) {
			// Synthetic source text, not a real credential. An extension or
			// provider may include such content anywhere in an error message.
			canary := "PRIVATE-SOURCE-DO-NOT-EMIT"
			cause := errors.New(canary + "\n" + strings.Repeat("日", 300))
			recorder := &recordingEmit{}
			summariser := &errSummariser{err: cause}
			var options []planner.CompressionOption
			if estimatorFailure {
				options = append(options, planner.WithTokenEstimator(func(*planner.Trajectory) (int, error) { return 0, cause }))
			}
			runner := planner.NewCompressionRunner(summariser, options...)
			tr := bigTrajectory(5000)
			err := runner.MaybeCompress(context.Background(), rcWith(fixedQuadruple("private-error"), 10, recorder.emit), tr)
			if !errors.Is(err, cause) || tr.Summary != nil {
				t.Fatal("original failure or checkpoint preservation changed")
			}
			events := recorder.snapshot()
			if len(events) != 1 {
				t.Fatalf("failure events = %d", len(events))
			}
			data, err := json.Marshal(events[0].Payload)
			if err != nil || strings.Contains(string(data), canary) || strings.Contains(string(data), "日") {
				t.Fatal("arbitrary error content escaped through a safe telemetry payload")
			}
		})
	}
}

func TestCompressionDiagnostics_ConcurrentFailureIsolation(t *testing.T) {
	t.Parallel()
	cause := errors.New("PRIVATE-EXTENSION-ERROR")
	runner := planner.NewCompressionRunner(&errSummariser{err: cause})
	var wg sync.WaitGroup
	for i := range 128 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			recorder := &recordingEmit{}
			q := fixedQuadruple(fmt.Sprintf("failure-%d", i))
			q.SessionID = q.RunID
			tr := bigTrajectory(5000)
			err := runner.MaybeCompress(t.Context(), rcWith(q, 10, recorder.emit), tr)
			events := recorder.snapshot()
			if !errors.Is(err, cause) || len(events) != 1 || events[0].Identity != q || tr.Summary != nil {
				t.Error("failure changed scope, outcome or checkpoint")
				return
			}
			data, err := json.Marshal(events[0].Payload)
			if err != nil || strings.Contains(string(data), cause.Error()) {
				t.Error("failure diagnostics leaked extension content")
			}
		}()
	}
	wg.Wait()
}
