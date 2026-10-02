package serve

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/sessions/turns"
	"github.com/hurtener/Harbor/internal/tasks"
)

type nativeOutputAuthorityPlanner struct{ observed chan tasks.TaskID }

func (p nativeOutputAuthorityPlanner) Next(ctx context.Context, _ planner.RunContext) (planner.Decision, error) {
	id, ok := tasks.OutputTaskFromContext(ctx)
	if !ok {
		return nil, errors.New("native task output authority is absent")
	}
	p.observed <- id
	return planner.Finish{Reason: planner.FinishGoal}, nil
}

func TestRunOne_BindsExactNativeOutputAuthority(t *testing.T) {
	env := newFailDriverEnv(t)
	observed := make(chan tasks.TaskID, 1)
	startFailDriver(t, env, func(o *RunLoopDriverOptions) {
		o.Planner = nativeOutputAuthorityPlanner{observed: observed}
	})
	ctx, err := identity.With(t.Context(), runLoopDriverTestID)
	if err != nil {
		t.Fatal(err)
	}
	h, err := env.reg.Spawn(ctx, tasks.SpawnRequest{
		Identity: identity.Quadruple{Identity: runLoopDriverTestID},
		Kind:     tasks.KindForeground,
		Query:    "native task output boundary",
	})
	if err != nil {
		t.Fatal(err)
	}
	if status := waitForTaskStatus(t, env.reg, h.ID, tasks.StatusComplete, 5*time.Second); status != tasks.StatusComplete {
		t.Fatalf("native task status = %q", status)
	}
	select {
	case id := <-observed:
		if id != h.ID {
			t.Fatalf("output task = %q, want %q", id, h.ID)
		}
	default:
		t.Fatal("native planner did not observe task authority")
	}
}

func TestTaskSnapshotOutputManifest_OnlySealedExactTask(t *testing.T) {
	deps := buildProjWiringMux(t)
	reg := deps.tasks
	id := identity.Identity{TenantID: "output-t", UserID: "output-u", SessionID: "output-s"}
	ctx, _ := identity.With(context.Background(), id)
	ctx, _ = identity.WithRun(ctx, id, "different-first-writer")
	q := identity.Quadruple{Identity: id, RunID: "different-first-writer"}
	h, err := reg.Spawn(ctx, tasks.SpawnRequest{Identity: q, Kind: tasks.KindForeground, AgentID: "exact-agent"})
	if err != nil {
		t.Fatal(err)
	}
	ctx = tasks.WithOutputTask(ctx, h.ID)
	if err = reg.MarkRunning(ctx, h.ID); err != nil {
		t.Fatal(err)
	}
	intent := tasks.OutputInvocationIntent{Position: 0, ToolName: "native", RequestSHA256: strings.Repeat("a", 64)}
	inv, err := reg.BeginOutputInvocation(ctx, h.ID, intent)
	if err != nil {
		t.Fatal(err)
	}
	refs := []tasks.ProducedArtifact{{ID: "z", SHA256: strings.Repeat("b", 64), MIMEType: "application/octet-stream", SizeBytes: 4, InvocationID: inv}, {ID: "a", SHA256: strings.Repeat("c", 64), MIMEType: "image/png", SizeBytes: 5, InvocationID: inv, ContentIndex: 1}}
	if err = reg.FinishOutputInvocation(ctx, h.ID, inv, refs, true); err != nil {
		t.Fatal(err)
	}
	adapter := &taskSnapshotAdapter{reg: reg}
	pending, err := adapter.Task(ctx, id, string(h.ID))
	if err != nil {
		t.Fatal(err)
	}
	if pending.OutputsPresent || pending.OutputManifest != nil {
		t.Fatal("unsealed capture became eligible")
	}
	if err = reg.MarkComplete(ctx, h.ID, tasks.TaskResult{}); err != nil {
		t.Fatal(err)
	}
	snap, err := adapter.Task(ctx, id, string(h.ID))
	if err != nil {
		t.Fatal(err)
	}
	if !snap.AnswerPresent || snap.Answer.State != turns.AnswerStateEmpty || !snap.OutputsPresent || snap.OutputManifest == nil || snap.OutputManifest.Version != 1 || len(snap.Outputs) != 2 || snap.Outputs[0].ID != "a" || snap.Outputs[1].ID != "z" {
		t.Fatalf("canonical output projection: %+v", snap)
	}
	foreign := id
	foreign.SessionID = "other"
	if _, err = adapter.Task(ctx, foreign, string(h.ID)); err == nil {
		t.Fatal("foreign task manifest visible")
	}
}

type legacyOutputSnapshotRegistry struct {
	tasks.TaskRegistry
	task *tasks.Task
}

func (r legacyOutputSnapshotRegistry) Get(context.Context, tasks.TaskID) (*tasks.Task, error) {
	return r.task, nil
}
func TestTaskSnapshotOutputManifest_LegacyMissingAnswerStaysUnknown(t *testing.T) {
	id := identity.Identity{TenantID: "legacy-t", UserID: "legacy-u", SessionID: "legacy-s"}
	for _, result := range []*tasks.TaskResult{nil, {}} {
		adapter := taskSnapshotAdapter{reg: legacyOutputSnapshotRegistry{task: &tasks.Task{ID: "legacy", Identity: identity.Quadruple{Identity: id}, Status: tasks.StatusComplete, Result: result}}}
		snap, err := adapter.Task(t.Context(), id, "legacy")
		if err != nil {
			t.Fatal(err)
		}
		if snap.AnswerPresent || snap.OutputsPresent || snap.OutputManifest != nil {
			t.Fatalf("invented legacy sources: %+v", snap)
		}
	}
}
