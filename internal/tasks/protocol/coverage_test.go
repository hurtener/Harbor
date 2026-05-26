package protocol_test

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/identity"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/tasks"
	tasksprotocol "github.com/hurtener/Harbor/internal/tasks/protocol"
)

func TestNewService_NilProjector_FailsLoudly(t *testing.T) {
	if _, err := tasksprotocol.NewService(nil); !errors.Is(err, tasksprotocol.ErrMisconfigured) {
		t.Fatalf("NewService(nil): want ErrMisconfigured, got %v", err)
	}
}

func TestNewRegistryProjector_NilRegistry_FailsLoudly(t *testing.T) {
	if _, err := tasksprotocol.NewRegistryProjector(nil); !errors.Is(err, tasksprotocol.ErrMisconfigured) {
		t.Fatalf("NewRegistryProjector(nil): want ErrMisconfigured, got %v", err)
	}
}

func TestNewService_OptionsAreApplied(t *testing.T) {
	reg, bus := newTestRegistry(t)
	proj, err := tasksprotocol.NewRegistryProjector(reg)
	if err != nil {
		t.Fatalf("NewRegistryProjector: %v", err)
	}
	// Exercise WithRedactor + WithLogger + WithBus — a constructed
	// Service with every option set must still serve a request.
	svc, err := tasksprotocol.NewService(proj,
		tasksprotocol.WithBus(bus),
		tasksprotocol.WithRedactor(auditpatterns.New()),
		tasksprotocol.WithLogger(slog.Default()),
	)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	if _, err := svc.List(context.Background(),
		prototypes.TaskListRequest{Identity: scopeOf("t1", "u1", "s1")}, false); err != nil {
		t.Fatalf("List with all options set: %v", err)
	}
}

func TestList_ValidateFilter_RejectsBadInput(t *testing.T) {
	svc, _, _ := newListService(t)
	ctx := context.Background()
	scope := scopeOf("t1", "u1", "s1")

	for _, tc := range []struct {
		name   string
		filter prototypes.TaskFilter
	}{
		{"bad status", prototypes.TaskFilter{Statuses: []prototypes.TaskStatus{"teleporting"}}},
		{"bad kind", prototypes.TaskFilter{Kinds: []prototypes.TaskKind{"daemonic"}}},
		{"negative latency", prototypes.TaskFilter{LatencyAboveMS: -1}},
		{
			"since after until",
			prototypes.TaskFilter{
				Since: time.Now(),
				Until: time.Now().Add(-time.Hour),
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.List(ctx, prototypes.TaskListRequest{Identity: scope, Filter: tc.filter}, false)
			if !errors.Is(err, tasksprotocol.ErrInvalidRequest) {
				t.Fatalf("want ErrInvalidRequest, got %v", err)
			}
		})
	}
}

func TestList_IdentityFacetAndTimeWindowAndLatency(t *testing.T) {
	svc, reg, _ := newListService(t)
	id := idFor("t1", "u1", "s1")
	seedTask(t, reg, id, tasks.KindForeground, tasks.StatusRunning, "alpha", "q")
	seedTask(t, reg, id, tasks.KindForeground, tasks.StatusRunning, "beta", "q")
	ctx := context.Background()
	scope := scopeOf("t1", "u1", "s1")

	t.Run("identity facet matches a tenant-only entry", func(t *testing.T) {
		resp, err := svc.List(ctx, prototypes.TaskListRequest{
			Identity: scope,
			Filter:   prototypes.TaskFilter{Identities: []prototypes.IdentityScope{{Tenant: "t1"}}},
		}, false)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(resp.Rows) < 2 {
			t.Fatalf("tenant-only identity facet: want >=2 rows, got %d", len(resp.Rows))
		}
	})

	t.Run("identity facet excludes a non-matching session", func(t *testing.T) {
		resp, err := svc.List(ctx, prototypes.TaskListRequest{
			Identity: scope,
			Filter: prototypes.TaskFilter{
				Identities: []prototypes.IdentityScope{{Tenant: "t1", Session: "no-such-session"}},
			},
		}, false)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(resp.Rows) != 0 {
			t.Fatalf("non-matching session facet: want 0 rows, got %d", len(resp.Rows))
		}
	})

	t.Run("time-window filter is honored", func(t *testing.T) {
		// A window in the far future excludes every just-seeded task.
		future := time.Now().Add(24 * time.Hour)
		resp, err := svc.List(ctx, prototypes.TaskListRequest{
			Identity: scope,
			Filter:   prototypes.TaskFilter{Since: future},
		}, false)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(resp.Rows) != 0 {
			t.Fatalf("future since-window: want 0 rows, got %d", len(resp.Rows))
		}
	})

	t.Run("latency-above filter is honored", func(t *testing.T) {
		// A huge latency floor excludes every just-seeded (fast) task.
		resp, err := svc.List(ctx, prototypes.TaskListRequest{
			Identity: scope,
			Filter:   prototypes.TaskFilter{LatencyAboveMS: 1 << 40},
		}, false)
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		if len(resp.Rows) != 0 {
			t.Fatalf("huge latency floor: want 0 rows, got %d", len(resp.Rows))
		}
	})
}

// stubEnricher is a deterministic Enricher for the WithEnricher path —
// it is a test fixture, NOT a production default (CLAUDE.md §13: stubs
// live in _test.go files).
type stubEnricher struct{}

func (stubEnricher) ParentSession(_ context.Context, _ identity.Identity, _ string) prototypes.TaskParentSessionRef {
	return prototypes.TaskParentSessionRef{SessionID: "s1", AgentName: "agent-x", Status: "active"}
}

func (stubEnricher) Cost(_ context.Context, _ identity.Identity, _ string) prototypes.TaskCostRollup {
	return prototypes.TaskCostRollup{
		TotalTokens: 120,
		USD:         0.0042,
		PerStep:     []prototypes.TaskCostStep{{StepIndex: 0, Tokens: 120, USD: 0.0042}},
	}
}

func (stubEnricher) PlannerSnapshot(_ context.Context, _ identity.Identity, _ string) *prototypes.TaskPlannerSnapshotRef {
	return &prototypes.TaskPlannerSnapshotRef{CheckpointID: "ckpt-1", Summary: "step 0 plan"}
}

func (stubEnricher) Trajectory(_ context.Context, _ identity.Identity, _ string) *prototypes.TaskTrajectoryRef {
	return nil
}

func TestGet_WithEnricher_PopulatesEnrichmentCards(t *testing.T) {
	reg, _ := newTestRegistry(t)
	proj, err := tasksprotocol.NewRegistryProjector(reg, tasksprotocol.WithEnricher(stubEnricher{}))
	if err != nil {
		t.Fatalf("NewRegistryProjector: %v", err)
	}
	svc, err := tasksprotocol.NewService(proj)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	id := idFor("t1", "u1", "s1")
	taskID := seedTask(t, reg, id, tasks.KindForeground, tasks.StatusRunning, "enriched", "q")

	detail, err := svc.Get(context.Background(), prototypes.TaskGetRequest{
		Identity: scopeOf("t1", "u1", "s1"),
		ID:       string(taskID),
	})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if detail.ParentSession.AgentName != "agent-x" {
		t.Errorf("enricher parent-session not applied: %+v", detail.ParentSession)
	}
	if detail.Cost.TotalTokens != 120 {
		t.Errorf("enricher cost not applied: %+v", detail.Cost)
	}
	if detail.PlannerSnapshot == nil || detail.PlannerSnapshot.CheckpointID != "ckpt-1" {
		t.Errorf("enricher planner-snapshot not applied: %+v", detail.PlannerSnapshot)
	}
}
