package conformancetest

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/tasks"
)

func runOutputArtifacts(t *testing.T, factory Factory) {
	t.Run("OutputManifest_ConcurrentAdmissionAndSeal", func(t *testing.T) {
		reg, close := factory()
		defer close()
		q := identity.Quadruple{Identity: identity.Identity{TenantID: "outputs-t", UserID: "outputs-u", SessionID: "outputs-s"}, RunID: "parent-run"}
		ctx, _ := identity.With(context.Background(), q.Identity)
		ctx, _ = identity.WithRun(ctx, q.Identity, q.RunID)
		h, err := reg.Spawn(ctx, tasks.SpawnRequest{Identity: q, Kind: tasks.KindForeground, AgentID: "agent"})
		if err != nil {
			t.Fatal(err)
		}
		ctx = tasks.WithOutputTask(ctx, h.ID)
		if err = reg.MarkRunning(ctx, h.ID); err != nil {
			t.Fatal(err)
		}
		intent := tasks.OutputInvocationIntent{Position: 0, ToolName: "native", RequestSHA256: strings.Repeat("a", 64)}
		var wins atomic.Int64
		var invocation string
		var mu sync.Mutex
		var wg sync.WaitGroup
		for i := 0; i < 100; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				id, err := reg.BeginOutputInvocation(ctx, h.ID, intent)
				if err == nil {
					wins.Add(1)
					mu.Lock()
					invocation = id
					mu.Unlock()
				} else if !errors.Is(err, tasks.ErrOutputInvocationUnknown) {
					t.Errorf("admission: %v", err)
				}
			}()
		}
		wg.Wait()
		if wins.Load() != 1 {
			t.Fatalf("admitted %d invocations", wins.Load())
		}
		if err = reg.MarkComplete(ctx, h.ID, tasks.TaskResult{Value: []byte(`"answer"`)}); !errors.Is(err, tasks.ErrOutputInvocationUnknown) {
			t.Fatalf("pending completion: %v", err)
		}
		changed := intent
		changed.RequestSHA256 = strings.Repeat("b", 64)
		if _, err = reg.BeginOutputInvocation(ctx, h.ID, changed); !errors.Is(err, tasks.ErrIdempotencyConflict) {
			t.Fatalf("changed request: %v", err)
		}
		ref := tasks.ProducedArtifact{ID: "exact-artifact", SHA256: strings.Repeat("c", 64), MIMEType: "application/octet-stream", SizeBytes: 4, InvocationID: invocation}
		if err = reg.FinishOutputInvocation(ctx, h.ID, invocation, []tasks.ProducedArtifact{ref}, true); err != nil {
			t.Fatal(err)
		}
		if err = reg.FinishOutputInvocation(ctx, h.ID, invocation, []tasks.ProducedArtifact{ref}, true); err != nil {
			t.Fatal(err)
		}
		if _, err = reg.BeginOutputInvocation(ctx, h.ID, intent); !errors.Is(err, tasks.ErrOutputInvocationSettled) {
			t.Fatalf("settled replay: %v", err)
		}
		wrong, _ := identity.With(context.Background(), identity.Identity{TenantID: "wrong", UserID: q.UserID, SessionID: q.SessionID})
		wrong, _ = identity.WithRun(wrong, identity.Identity{TenantID: "wrong", UserID: q.UserID, SessionID: q.SessionID}, "run")
		wrong = tasks.WithOutputTask(wrong, h.ID)
		if _, err = reg.BeginOutputInvocation(wrong, h.ID, intent); !errors.Is(err, tasks.ErrNotFound) {
			t.Fatalf("owner fence: %v", err)
		}
		if err = reg.MarkComplete(ctx, h.ID, tasks.TaskResult{Value: []byte(`"answer"`)}); err != nil {
			t.Fatal(err)
		}
		task, err := reg.Get(ctx, h.ID)
		if err != nil {
			t.Fatal(err)
		}
		if task.OutputManifest == nil || !task.OutputManifest.Sealed || len(task.OutputManifest.Artifacts) != 1 || tasks.ValidateOutputManifest(task) != nil {
			t.Fatalf("manifest: %+v", task.OutputManifest)
		}
		digest := task.OutputManifest.SHA256
		task.OutputManifest.Artifacts[0].ID = "mutated"
		task.OutputManifest.Invocations[0].ID = "mutated"
		again, _ := reg.Get(ctx, h.ID)
		if again.OutputManifest.SHA256 != digest || tasks.ValidateOutputManifest(again) != nil {
			t.Fatal("retained read mutated manifest")
		}
		if err = reg.FinishOutputInvocation(ctx, h.ID, invocation, nil, false); err == nil {
			t.Fatal("late mutation accepted")
		}
	})
	t.Run("OutputManifest_KnownEmptyAndBoundedSlots", func(t *testing.T) {
		reg, close := factory()
		defer close()
		q := identity.Quadruple{Identity: identity.Identity{TenantID: "ot", UserID: "ou", SessionID: "os"}, RunID: "run"}
		ctx, _ := identity.With(context.Background(), q.Identity)
		ctx, _ = identity.WithRun(ctx, q.Identity, q.RunID)
		h, err := reg.Spawn(ctx, tasks.SpawnRequest{Identity: q, Kind: tasks.KindForeground})
		if err != nil {
			t.Fatal(err)
		}
		ctx = tasks.WithOutputTask(ctx, h.ID)
		if err = reg.MarkRunning(ctx, h.ID); err != nil {
			t.Fatal(err)
		}
		for step := 0; step < 100; step++ {
			intent := tasks.OutputInvocationIntent{Position: int64(step), ToolName: "plain", RequestSHA256: strings.Repeat("a", 64)}
			id, err := reg.BeginOutputInvocation(ctx, h.ID, intent)
			if err != nil {
				t.Fatal(err)
			}
			if err = reg.FinishOutputInvocation(ctx, h.ID, id, nil, step%2 == 0); err != nil {
				t.Fatal(err)
			}
		}
		if err = reg.MarkComplete(ctx, h.ID, tasks.TaskResult{}); err != nil {
			t.Fatal(err)
		}
		task, _ := reg.Get(ctx, h.ID)
		if task.OutputManifest == nil || !task.OutputManifest.Sealed || len(task.OutputManifest.Artifacts) != 0 || len(task.OutputManifest.Invocations) != 1 {
			t.Fatal(fmt.Sprintf("unbounded or unknown empty: %+v", task.OutputManifest))
		}
	})
}
