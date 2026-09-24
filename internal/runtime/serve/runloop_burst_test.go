package serve

import (
	"testing"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/tasks"
)

// The shared fixture must hold the settings/isolation workload even when its
// task-spawn consumer has not been scheduled yet. A deliberately undersized
// lossy queue tests overflow instead of per-task settings or context isolation.
func TestDriverFixture_HoldsAcceptedTaskBurstBeforeConsumerRuns(t *testing.T) {
	red := auditpatterns.New()
	bus := mkDriverTestBus(t, red)
	reg := mkDriverTestTaskRegistry(t, bus, red)
	sub, err := bus.Subscribe(t.Context(), events.Filter{Admin: true, Types: []events.EventType{tasks.EventTypeTaskSpawned}})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Cancel()
	const count = 128
	ids := make([]tasks.TaskID, count)
	for i := range count {
		ids[i] = spawnDriverTestTask(t, reg)
	}
	counter, ok := bus.(events.DroppedCounter)
	if !ok {
		t.Fatal("real fixture bus must expose drop accounting")
	}
	if dropped := counter.DroppedTotal(); dropped != 0 {
		t.Fatalf("fixture dropped %d events before the %d-task consumer ran", dropped, count)
	}
	for i, want := range ids {
		select {
		case event := <-sub.Events():
			payload, ok := event.Payload.(tasks.TaskSpawnedPayload)
			if !ok || event.Type != tasks.EventTypeTaskSpawned || payload.TaskID != want || event.Identity.Identity != runLoopDriverTestID {
				t.Fatalf("burst event %d lost identity/order: type=%s payload=%T", i, event.Type, event.Payload)
			}
		default:
			t.Fatalf("accepted task %d has no queued spawn event", i)
		}
	}
}
