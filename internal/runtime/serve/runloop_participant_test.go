package serve

import (
	"context"
	"testing"
	"time"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
	"github.com/hurtener/Harbor/internal/runtime/steering"
	"github.com/hurtener/Harbor/internal/tasks"
)

type participantObservation struct {
	acting, partition identity.Identity
	run               identity.Quadruple
	speaker           string
}

type participantProbePlanner struct{ seen chan participantObservation }

func (p *participantProbePlanner) Next(ctx context.Context, rc planner.RunContext) (planner.Decision, error) {
	acting, partition, _ := identity.ActingFrom(ctx)
	q, _ := identity.QuadrupleFrom(ctx)
	select {
	case p.seen <- participantObservation{acting: acting, partition: partition, run: q, speaker: rc.Speaker}:
	default:
	}
	return planner.Finish{Reason: planner.FinishGoal}, nil
}

// A participant's turn runs in the owner's session partition (storage,
// run quadruple) while the run context acts as the writer and the planner
// sees the writer's label. An ordinary turn is unchanged.
func TestRunLoopDriver_ParticipantRunActsAsWriter(t *testing.T) {
	for _, tc := range []struct {
		name        string
		participant *identity.Participant
		wantActing  string
		wantSpeaker string
	}{
		{name: "participant", participant: &identity.Participant{UserID: "writer-b", Speaker: "Bruno"}, wantActing: "writer-b", wantSpeaker: "Bruno"},
		{name: "ordinary", wantActing: runLoopDriverTestID.UserID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			red := auditpatterns.New()
			bus := mkDriverTestBus(t, red)
			reg := mkDriverTestTaskRegistry(t, bus, red)
			coord := pauseresume.New(pauseresume.WithBus(bus))
			rl, err := steering.NewRunLoop(steering.NewRegistry(), coord, steering.WithRunLoopBus(bus))
			if err != nil {
				t.Fatal(err)
			}
			p := &participantProbePlanner{seen: make(chan participantObservation, 1)}
			driver, err := NewRunLoopDriver(RunLoopDriverOptions{SessionMemory: config.MemoryConfig{Strategy: "none"},
				Bus: bus, RunLoop: rl, Planner: p, Tasks: reg, AgentConfigID: "agent-participant"})
			if err != nil {
				t.Fatal(err)
			}
			if err := driver.Start(context.Background()); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = driver.Close(context.Background()) }()
			spawnCtx, err := identity.With(context.Background(), runLoopDriverTestID)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := reg.Spawn(spawnCtx, tasks.SpawnRequest{Identity: identity.Quadruple{Identity: runLoopDriverTestID},
				Kind: tasks.KindForeground, Query: "shared turn", Participant: tc.participant}); err != nil {
				t.Fatal(err)
			}
			select {
			case got := <-p.seen:
				if got.partition != runLoopDriverTestID || got.run.Identity != runLoopDriverTestID {
					t.Fatalf("storage identity moved: partition %+v run %+v", got.partition, got.run)
				}
				if got.acting.UserID != tc.wantActing || got.acting.SessionID != runLoopDriverTestID.SessionID {
					t.Fatalf("acting = %+v, want user %q", got.acting, tc.wantActing)
				}
				if got.speaker != tc.wantSpeaker {
					t.Fatalf("speaker = %q, want %q", got.speaker, tc.wantSpeaker)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("planner never ran")
			}
		})
	}
}
