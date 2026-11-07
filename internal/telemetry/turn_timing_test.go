package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
)

func TestTurnTiming_FirstDispatch_ExactlyOnce(t *testing.T) {
	var output bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&output, nil))
	start := time.Unix(100, 0)
	ctx := BeginTurnTiming(t.Context(), logger, start)
	timing := ctx.Value(turnTimingKey{}).(*turnTiming)
	timing.now = func() time.Time { return start.Add(25 * time.Millisecond) }
	RecordTurnStage(ctx, TurnStageToolAttach, 4*time.Millisecond)
	RecordTurnStage(ctx, TurnStageToolAttach, 2*time.Millisecond)
	RecordTurnStage(ctx, TurnStage(255), time.Second)
	RecordTurnStage(ctx, TurnStageProviderRoute, -time.Second)
	if output.Len() != 0 {
		t.Fatal("turn without model dispatch emitted a record")
	}
	var wg sync.WaitGroup
	for range 128 {
		wg.Go(func() { ObserveFirstModelRequest(ctx) })
	}
	wg.Wait()
	RecordTurnStage(ctx, TurnStageProviderRoute, time.Second)
	ObserveFirstModelRequest(ctx)
	if strings.Count(output.String(), "\n") != 1 {
		t.Fatalf("expected exactly one dispatch record: %s", output.String())
	}
	var record map[string]any
	if err := json.Unmarshal(output.Bytes(), &record); err != nil {
		t.Fatal(err)
	}
	if record["msg"] != "turn.pre_model" || record["duration_ms"] != float64(25) || record["tool_attach_ms"] != float64(6) {
		t.Fatalf("unexpected timing fields: %v", record)
	}
	if _, ok := record["provider_route_ms"]; ok {
		t.Fatal("negative or post-dispatch stage was recorded")
	}
}

func TestTurnTiming_ConcurrentTurns_IsolatedAndContentFree(t *testing.T) {
	var output bytes.Buffer
	// One handler serializes its writer across concurrent turn loggers.
	logger := slog.New(slog.NewJSONHandler(&output, nil))
	var wg sync.WaitGroup
	for i := range 128 {
		wg.Go(func() {
			id := identity.Identity{TenantID: fmt.Sprintf("tenant-%d", i), UserID: fmt.Sprintf("user-%d", i), SessionID: fmt.Sprintf("session-%d", i)}
			ctx, err := identity.WithRun(t.Context(), id, fmt.Sprintf("run-%d", i))
			if err != nil {
				t.Error(err)
				return
			}
			// Documented dummy sensitive context: only explicit timing fields
			// and identity may be observed, never arbitrary context values.
			ctx = context.WithValue(ctx, struct{ payload bool }{true}, "dummy-private-prompt https://dummy.invalid/private dummy-bearer-value")
			start := time.Unix(100, 0)
			ctx = BeginTurnTiming(ctx, logger, start)
			ctx.Value(turnTimingKey{}).(*turnTiming).now = func() time.Time { return start.Add(time.Duration(i+1) * time.Millisecond) }
			RecordTurnStage(ctx, TurnStageProviderRoute, time.Duration(i+1)*time.Microsecond)
			ObserveFirstModelRequest(ctx)
			ObserveFirstModelRequest(ctx)
		})
	}
	wg.Wait()
	seen := make(map[string]bool)
	for _, line := range strings.Split(strings.TrimSpace(output.String()), "\n") {
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatal(err)
		}
		duration := record["duration_ms"].(float64)
		i := int(duration) - 1
		if record["tenant_id"] != fmt.Sprintf("tenant-%d", i) || record["user_id"] != fmt.Sprintf("user-%d", i) || record["session_id"] != fmt.Sprintf("session-%d", i) || record["run_id"] != fmt.Sprintf("run-%d", i) || record["provider_route_ms"] != float64(i+1)/1000 {
			t.Fatalf("cross-turn timing/identity bleed: %v", record)
		}
		key := record["run_id"].(string)
		if seen[key] {
			t.Fatalf("duplicate turn %s", key)
		}
		seen[key] = true
	}
	if len(seen) != 128 {
		t.Fatalf("got %d turns, want 128", len(seen))
	}
	for _, forbidden := range []string{"dummy-private-prompt", "https://", "dummy-bearer-value", "payload"} {
		if strings.Contains(output.String(), forbidden) {
			t.Fatalf("timing record contains content %q", forbidden)
		}
	}
}

func TestTurnTiming_EnsureAndNewTurn_PreserveThenReset(t *testing.T) {
	var output bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&output, nil))
	ctx := BeginTurnTiming(t.Context(), logger, time.Now())
	if got := EnsureTurnTiming(ctx, logger, time.Now()); got != ctx {
		t.Fatal("inner runtime layer replaced outer timing")
	}
	ObserveFirstModelRequest(ctx)
	ctx = BeginTurnTiming(ctx, logger, time.Now())
	ObserveFirstModelRequest(ctx)
	ObserveFirstModelRequest(t.Context())
	RecordTurnStage(t.Context(), TurnStageToolAttach, time.Second)
	if strings.Count(output.String(), "\n") != 2 {
		t.Fatalf("expected one record for each new turn: %s", output.String())
	}
}
