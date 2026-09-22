package serve

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/planner"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/tasks"
)

// Inject the retirement transition at an exact run-start read. All earlier
// reads and task/event operations use the existing real registry/driver fixture.
// This is deterministic; it does not depend on winning a scheduler race.
type retirementAtConfigRead struct {
	agentcfg.Registry
	calls  atomic.Int32
	at     int32
	reason error
}

func (r *retirementAtConfigRead) Active(ctx context.Context, q identity.Quadruple, agent string, scope agentcfg.ConfigScope) (agentcfg.Revision, bool, error) {
	if r.calls.Add(1) == r.at {
		return agentcfg.Revision{}, false, fmt.Errorf("config read: %w", r.reason)
	}
	return r.Registry.Active(ctx, q, agent, scope)
}

func TestRunOne_RetirementDuringConfigProjectionKeepsTypedRefusal(t *testing.T) {
	for _, stage := range []struct {
		name string
		read int32
		text string
	}{
		{"llm overrides", 2, "tenant-override resolution failed"},
		{"agent prompt", 3, "prompt-layer projection failed"},
		{"user prompt", 4, "prompt-layer projection failed"},
		{"prompt blocks", 5, "prompt-layer projection failed"},
		{"completion hook", 6, "run-completion-hook projection failed"},
	} {
		for _, retired := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/retired=%t", stage.name, retired), func(t *testing.T) {
				reason, code, text := errInjected, planner.TaskErrorCodeRunLoopError, stage.text
				if retired {
					reason, code, text = agentcfg.ErrAgentRetired, string(protoerrors.CodeAgentRetired), "agent is retired"
				}
				reg := &retirementAtConfigRead{Registry: acTestRegistry(t), at: stage.read, reason: reason}
				probe := &driverTestPlanner{finishGoalImmediately: true}
				env := newFailDriverEnv(t)
				startFailDriver(t, env, func(o *RunLoopDriverOptions) {
					o.AgentConfig, o.AgentConfigID, o.Planner = reg, "retiring-config-agent", probe
				})
				spawnAndAwaitFailure(t, env.reg, nil, code, text)
				if got := reg.calls.Load(); got != stage.read {
					t.Fatalf("config reads=%d, want stop at failing read %d", got, stage.read)
				}
				probe.mu.Lock()
				defer probe.mu.Unlock()
				if probe.steps != 0 {
					t.Fatal("failed config projection reached planner")
				}
			})
		}
	}
}

func TestRunConfigTaskError_OnlyTypedRetirementChangesClassification(t *testing.T) {
	t.Parallel()
	fallback := tasks.TaskError{Code: "runtime_fetch_error", Message: "original projection context"}
	for _, err := range []error{errInjected, context.Canceled, context.DeadlineExceeded, errors.New(agentcfg.ErrAgentRetired.Error())} {
		if got := runConfigTaskError(err, fallback); got != fallback {
			t.Fatalf("unrelated error %v changed classification: %+v", err, got)
		}
	}
	for _, err := range []error{agentcfg.ErrAgentRetired, fmt.Errorf("wrapped: %w", agentcfg.ErrAgentRetired), errors.Join(errInjected, agentcfg.ErrAgentRetired)} {
		if got := runConfigTaskError(err, fallback); got.Code != string(protoerrors.CodeAgentRetired) || got.Message != "agent is retired" {
			t.Fatalf("typed retirement not preserved: %+v", got)
		}
	}
}
