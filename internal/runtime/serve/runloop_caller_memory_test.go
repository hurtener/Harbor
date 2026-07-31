package serve

// runloop_caller_memory_test.go — the run loop's caller-memory composition
// branch (Phase 219 / D-364).
//
// The Protocol edge refuses an inadmissible `caller_memory` before a task
// exists, so the run loop's own refusal branch is reachable ONLY from an
// in-process caller that spawns a task directly — which is exactly why it
// exists and exactly why it needs a test. An untested guard is how an inert
// guard survives.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/memory"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/runtime/runctx"
	"github.com/hurtener/Harbor/internal/tasks"
)

// spawnWithCallerMemory spawns a foreground task carrying raw caller memory
// and returns its handle.
func spawnWithCallerMemory(t *testing.T, reg tasks.TaskRegistry, raw json.RawMessage) tasks.TaskHandle {
	t.Helper()
	ctx, err := identity.With(context.Background(), runLoopDriverTestID)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	h, err := reg.Spawn(ctx, tasks.SpawnRequest{
		Identity:     identity.Quadruple{Identity: runLoopDriverTestID},
		Kind:         tasks.KindForeground,
		Query:        "caller-memory goal",
		CallerMemory: raw,
	})
	if err != nil {
		t.Fatalf("reg.Spawn: %v", err)
	}
	return h
}

// TestRunOne_CallerMemoryCompositionError_FailsRunLoud — a task carrying an
// inadmissible caller-memory payload fails the run LOUD rather than running
// without the memory it was promised and reporting success (CLAUDE.md §13).
func TestRunOne_CallerMemoryCompositionError_FailsRunLoud(t *testing.T) {
	// An explicit JSON `null` is VALID JSON, so it persists cleanly and
	// reaches the composition step — which is what makes the run loop's own
	// refusal reachable at all. (A syntactically malformed document never
	// gets that far; see the sibling test below.)
	env := newFailDriverEnv(t)
	startFailDriver(t, env, nil)
	h := spawnWithCallerMemory(t, env.reg, json.RawMessage(`null`))
	if status := waitForTaskStatus(t, env.reg, h.ID, tasks.StatusFailed, 5*time.Second); status != tasks.StatusFailed {
		t.Fatalf("task status = %q, want %q — an inadmissible payload must fail the run, not run without it", status, tasks.StatusFailed)
	}
	ctx, err := identity.With(context.Background(), runLoopDriverTestID)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	got, gErr := env.reg.Get(ctx, h.ID)
	if gErr != nil {
		t.Fatalf("reg.Get: %v", gErr)
	}
	if got.Error == nil {
		t.Fatal("failed task carries no TaskError")
	}
	if got.Error.Code != planner.TaskErrorCodeRunLoopError {
		t.Errorf("TaskError.Code = %q, want %q (message: %s)", got.Error.Code, planner.TaskErrorCodeRunLoopError, got.Error.Message)
	}
	if want := "caller-memory composition failed"; !strings.Contains(got.Error.Message, want) {
		t.Errorf("TaskError.Message = %q, want it to contain %q", got.Error.Message, want)
	}
}

// TestSpawn_MalformedCallerMemory_RefusedAtPersist records where a
// syntactically malformed payload actually dies on the IN-PROCESS door,
// which is EARLIER than this phase's design assumed.
//
// The Protocol edge refuses it before `Spawn` (that path is covered in
// `internal/protocol`). An in-process caller bypasses that edge — but the
// task record's whole-record marshal then fails, because a malformed
// `json.RawMessage` cannot be marshalled at all, and `Spawn` returns the
// serialization error rather than persisting an unusable row. So the
// composition step is never reached for this shape.
//
// Pinned as a TEST rather than left as folklore: it is the difference
// between "the run loop guards this" and "nothing has to, because the row
// cannot exist" — and a future change that made the record marshal
// tolerant would silently move the failure to a place nobody is watching.
func TestSpawn_MalformedCallerMemory_RefusedAtPersist(t *testing.T) {
	env := newFailDriverEnv(t)
	ctx, err := identity.With(context.Background(), runLoopDriverTestID)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	_, sErr := env.reg.Spawn(ctx, tasks.SpawnRequest{
		Identity:     identity.Quadruple{Identity: runLoopDriverTestID},
		Kind:         tasks.KindForeground,
		Query:        "malformed caller memory",
		CallerMemory: json.RawMessage(`{"unterminated":`),
	})
	if sErr == nil {
		t.Fatal("Spawn accepted a malformed caller_memory payload — it must fail loud, never persist an unusable row")
	}
	if !strings.Contains(sErr.Error(), "not serializable") {
		t.Fatalf("Spawn error = %v, want a serialization refusal", sErr)
	}
}

// TestRunOne_CallerMemory_AdmittedAndAnnounced — the happy branch: a valid
// payload reaches the planner's MemoryBlocks under the fixed key AND the
// admission event lands on the bus carrying a size, never content.
func TestRunOne_CallerMemory_AdmittedAndAnnounced(t *testing.T) {
	const marker = "runloop-caller-memory-marker"

	env := newFailDriverEnv(t)
	sub, err := env.bus.Subscribe(context.Background(), events.Filter{
		Tenant:  runLoopDriverTestID.TenantID,
		User:    runLoopDriverTestID.UserID,
		Session: runLoopDriverTestID.SessionID,
		Types:   []events.EventType{memory.EventTypeMemoryCallerBlockAdmitted},
	})
	if err != nil {
		t.Fatalf("bus.Subscribe: %v", err)
	}
	t.Cleanup(sub.Cancel)

	seen := make(chan *planner.MemoryBlocks, 4)
	startFailDriver(t, env, func(o *RunLoopDriverOptions) {
		o.Planner = plannerFunc(func(_ context.Context, rc planner.RunContext) (planner.Decision, error) {
			select {
			case seen <- rc.MemoryBlocks:
			default:
			}
			return planner.Finish{Reason: planner.FinishGoal}, nil
		})
	})

	raw := json.RawMessage(`{"note":"` + marker + `"}`)
	spawnWithCallerMemory(t, env.reg, raw)

	select {
	case mb := <-seen:
		if mb == nil {
			t.Fatal("the planner saw nil MemoryBlocks — caller memory did not reach the run")
		}
		ext, ok := mb.External.(map[string]any)
		if !ok {
			t.Fatalf("External is %T, want map[string]any", mb.External)
		}
		val, ok := ext[runctx.CallerSuppliedKey].(map[string]any)
		if !ok {
			t.Fatalf("the %q key is absent or wrongly shaped: %v", runctx.CallerSuppliedKey, ext)
		}
		if val["note"] != marker {
			t.Fatalf("caller value = %v, want note=%q", val, marker)
		}
		if mb.Conversation != nil {
			t.Fatalf("Conversation = %v, want nil — composition never writes the conversation tier", mb.Conversation)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the run never reached the planner")
	}

	select {
	case ev, ok := <-sub.Events():
		if !ok {
			t.Fatal("subscription closed before the admission event arrived")
		}
		payload, mErr := json.Marshal(ev.Payload)
		if mErr != nil {
			t.Fatalf("marshal payload: %v", mErr)
		}
		if strings.Contains(string(payload), marker) {
			t.Fatalf("the admission event carries the caller's content: %s", payload)
		}
		var admitted struct {
			Bytes int    `json:"bytes"`
			Tier  string `json:"tier"`
			Key   string `json:"key"`
		}
		if uErr := json.Unmarshal(payload, &admitted); uErr != nil {
			t.Fatalf("decode payload: %v (%s)", uErr, payload)
		}
		if admitted.Bytes != len(raw) {
			t.Errorf("bytes = %d, want %d (the wire length)", admitted.Bytes, len(raw))
		}
		if admitted.Key != runctx.CallerSuppliedKey || admitted.Tier != runctx.ExternalTierName {
			t.Errorf("tier/key = %q/%q, want %q/%q", admitted.Tier, admitted.Key, runctx.ExternalTierName, runctx.CallerSuppliedKey)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("memory.caller_block_admitted never arrived")
	}
}

// plannerFunc adapts a func to planner.Planner.
type plannerFunc func(context.Context, planner.RunContext) (planner.Decision, error)

func (f plannerFunc) Next(ctx context.Context, rc planner.RunContext) (planner.Decision, error) {
	return f(ctx, rc)
}
