package engine_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/tasks"
	"github.com/hurtener/Harbor/internal/tasks/engine"
)

func TestOutputWitness_PersistenceUncertaintyStopsCompletion(t *testing.T) {
	for _, stage := range []string{"admission", "capture"} {
		t.Run(stage, func(t *testing.T) {
			bus := mkBus(t)
			defer func() { _ = bus.Close(context.Background()) }()
			back := &memBackend{}
			reg, err := engine.New(bus, auditpatterns.New(), back)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = reg.Close(context.Background()) }()
			q := idQuad()
			ctx, _ := identity.With(context.Background(), q.Identity)
			ctx, _ = identity.WithRun(ctx, q.Identity, "run")
			h, err := reg.Spawn(ctx, tasks.SpawnRequest{Identity: q, Kind: tasks.KindForeground})
			if err != nil {
				t.Fatal(err)
			}
			ctx = tasks.WithOutputTask(ctx, h.ID)
			if err = reg.MarkRunning(ctx, h.ID); err != nil {
				t.Fatal(err)
			}
			intent := tasks.OutputInvocationIntent{Position: 0, ToolName: "tool", RequestSHA256: strings.Repeat("a", 64)}
			if stage == "admission" {
				back.saveErr = errors.New("ambiguous disk write")
			}
			inv, err := reg.BeginOutputInvocation(ctx, h.ID, intent)
			if stage == "admission" {
				if err == nil {
					t.Fatal("admission persistence error hidden")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				back.saveErr = errors.New("ambiguous outcome write")
				if err = reg.FinishOutputInvocation(ctx, h.ID, inv, nil, true); err == nil {
					t.Fatal("capture persistence error hidden")
				}
			}
			back.saveErr = nil
			if _, err = reg.BeginOutputInvocation(ctx, h.ID, intent); err == nil {
				t.Fatal("uncertain invocation re-admitted")
			}
			if err = reg.MarkComplete(ctx, h.ID, tasks.TaskResult{}); !errors.Is(err, tasks.ErrOutputInvocationUnknown) {
				t.Fatalf("uncertain task completed: %v", err)
			}
			if err = reg.MarkFailed(ctx, h.ID, tasks.TaskError{Code: "output_provenance_uncertain", Message: "Operator attention required"}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestOutputWitness_FailedCompleteDoesNotExposeCachedSeal(t *testing.T) {
	bus := mkBus(t)
	defer func() { _ = bus.Close(context.Background()) }()
	back := &memBackend{}
	reg, err := engine.New(bus, auditpatterns.New(), back)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reg.Close(context.Background()) }()
	q := idQuad()
	ctx, _ := identity.With(context.Background(), q.Identity)
	h, err := reg.Spawn(ctx, tasks.SpawnRequest{Identity: q, Kind: tasks.KindForeground})
	if err != nil {
		t.Fatal(err)
	}
	if err = reg.MarkRunning(ctx, h.ID); err != nil {
		t.Fatal(err)
	}
	back.saveErr = errors.New("uncertain completion CAS")
	if err = reg.MarkComplete(ctx, h.ID, tasks.TaskResult{Value: []byte(`"uncommitted"`)}); err == nil {
		t.Fatal("completion error hidden")
	}
	got, err := reg.Get(ctx, h.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != tasks.StatusRunning || got.Result != nil || got.OutputManifest.Sealed || got.OutputManifest.SHA256 != "" {
		t.Fatalf("uncommitted result visible: %+v", got)
	}
}
