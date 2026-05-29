package protocol_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/tasks"
	tasksprotocol "github.com/hurtener/Harbor/internal/tasks/protocol"
)

func TestGet_ReturnsEnrichedDetail(t *testing.T) {
	svc, reg, _ := newListService(t)
	id := idFor("t1", "u1", "s1")
	taskID := seedTask(t, reg, id, tasks.KindForeground, tasks.StatusComplete, "enrich me", "do the thing")

	resp, err := svc.Get(context.Background(), prototypes.TaskGetRequest{
		Identity: scopeOf("t1", "u1", "s1"),
		ID:       string(taskID),
	})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if resp.Task.ID != string(taskID) {
		t.Fatalf("want task id %q, got %q", taskID, resp.Task.ID)
	}
	if resp.Task.Status != prototypes.TaskStatusComplete {
		t.Fatalf("want complete status, got %q", resp.Task.Status)
	}
	// With no Enricher wired the parent-session card still carries the
	// session the task runs within.
	if resp.ParentSession.SessionID != "s1" {
		t.Fatalf("want parent session s1, got %q", resp.ParentSession.SessionID)
	}
	// The completed task's small result is inlined, not referenced.
	if resp.ResultInline == "" {
		t.Fatalf("want an inlined result, got empty")
	}
	if resp.ResultRef != nil {
		t.Fatalf("small result must not be artifact-referenced")
	}
}

func TestGet_IdentityMandatory(t *testing.T) {
	svc, _, _ := newListService(t)
	_, err := svc.Get(context.Background(), prototypes.TaskGetRequest{
		Identity: scopeOf("", "u1", "s1"),
		ID:       "anything",
	})
	if !errors.Is(err, tasksprotocol.ErrIdentityRequired) {
		t.Fatalf("want ErrIdentityRequired, got %v", err)
	}
}

func TestGet_EmptyIDRejected(t *testing.T) {
	svc, _, _ := newListService(t)
	_, err := svc.Get(context.Background(), prototypes.TaskGetRequest{
		Identity: scopeOf("t1", "u1", "s1"),
		ID:       "   ",
	})
	if !errors.Is(err, tasksprotocol.ErrInvalidRequest) {
		t.Fatalf("want ErrInvalidRequest, got %v", err)
	}
}

func TestGet_CrossTenant_ReturnsNotFound(t *testing.T) {
	svc, reg, _ := newListService(t)
	// A task lives in tenant t1.
	taskID := seedTask(t, reg, idFor("t1", "u1", "s1"), tasks.KindForeground, tasks.StatusRunning, "secret", "q")

	// Tenant t2 asks for it by ID — existence is never revealed.
	_, err := svc.Get(context.Background(), prototypes.TaskGetRequest{
		Identity: scopeOf("t2", "u2", "s2"),
		ID:       string(taskID),
	})
	if !errors.Is(err, tasksprotocol.ErrTaskNotFound) {
		t.Fatalf("cross-tenant get: want ErrTaskNotFound, got %v", err)
	}
}

func TestGet_UnknownIDReturnsNotFound(t *testing.T) {
	svc, _, _ := newListService(t)
	_, err := svc.Get(context.Background(), prototypes.TaskGetRequest{
		Identity: scopeOf("t1", "u1", "s1"),
		ID:       "task-does-not-exist",
	})
	if !errors.Is(err, tasksprotocol.ErrTaskNotFound) {
		t.Fatalf("unknown id: want ErrTaskNotFound, got %v", err)
	}
}

// TestGet_CostPerStepIsNeverNullOnWire — round-6 F5 fix. The TS contract
// declares TaskCostRollup.per_step as a non-null TaskCostStep[]. A Go
// nil slice JSON-marshals to `null`, which made the Console's
// RightRailCostBreakdown null-deref on `.length` when clicking a
// just-completed task with no cost data. The projector now normalizes
// an empty PerStep to `[]` before returning. This test pins the wire
// shape: the JSON-encoded response carries `"per_step":[]`, never
// `"per_step":null`.
func TestGet_CostPerStepIsNeverNullOnWire(t *testing.T) {
	svc, reg, _ := newListService(t)
	id := idFor("t1", "u1", "s1")
	taskID := seedTask(t, reg, id, tasks.KindForeground, tasks.StatusComplete, "no cost", "trivial")

	resp, err := svc.Get(context.Background(), prototypes.TaskGetRequest{
		Identity: scopeOf("t1", "u1", "s1"),
		ID:       string(taskID),
	})
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	// Structural assertion — PerStep must be non-nil even when empty.
	if resp.Cost.PerStep == nil {
		t.Fatalf("Cost.PerStep is nil; contract requires an empty slice for the wire")
	}
	if len(resp.Cost.PerStep) != 0 {
		t.Fatalf("Cost.PerStep len=%d, want 0 (no enricher wired)", len(resp.Cost.PerStep))
	}

	// Wire assertion — encode and confirm the JSON output uses `[]`.
	enc, err := json.Marshal(resp.Cost)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(enc), `"per_step":null`) {
		t.Fatalf("wire-shape regression: per_step encoded as null:\n%s", enc)
	}
	if !strings.Contains(string(enc), `"per_step":[]`) {
		t.Fatalf("wire-shape regression: expected per_step:[] in output:\n%s", enc)
	}
}

// TestGet_TrajectoryPopulatedFromEnricher — Phase 107a AC-13.
// When the enricher returns a *TaskTrajectoryRef, it lands on detail.Trajectory.
func TestGet_TrajectoryPopulatedFromEnricher(t *testing.T) {
	reg, _ := newTestRegistry(t)
	enr := &trajectoryStubEnricher{
		ref: &prototypes.TaskTrajectoryRef{
			Steps: []prototypes.TaskTrajectoryStep{
				{Index: 0, ReasoningTrace: "First, I need to understand what the user asked."},
				{Index: 2, ReasoningTrace: "Now I'll check using the provided tool."},
			},
		},
	}
	proj, err := tasksprotocol.NewRegistryProjector(reg, tasksprotocol.WithEnricher(enr))
	if err != nil {
		t.Fatalf("NewRegistryProjector: %v", err)
	}
	id := idFor("t1", "u1", "s1")
	taskID := seedTask(t, reg, id, tasks.KindForeground, tasks.StatusComplete, "reason", "think step by step")
	detail, err := proj.GetTask(context.Background(), id, string(taskID))
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if detail.Trajectory == nil {
		t.Fatal("expected detail.Trajectory to be non-nil")
	}
	if len(detail.Trajectory.Steps) != 2 {
		t.Fatalf("want 2 steps, got %d", len(detail.Trajectory.Steps))
	}
	if detail.Trajectory.Steps[0].ReasoningTrace == "" {
		t.Fatal("first step's ReasoningTrace must be non-empty")
	}
}

// TestGet_TrajectoryNilWhenEnricherReturnsNil — Phase 107a AC-13.
// Graceful absence: nil enricher result preserves a nil detail.Trajectory.
func TestGet_TrajectoryNilWhenEnricherReturnsNil(t *testing.T) {
	reg, _ := newTestRegistry(t)
	enr := &trajectoryStubEnricher{ref: nil}
	proj, err := tasksprotocol.NewRegistryProjector(reg, tasksprotocol.WithEnricher(enr))
	if err != nil {
		t.Fatalf("NewRegistryProjector: %v", err)
	}
	id := idFor("t1", "u1", "s1")
	taskID := seedTask(t, reg, id, tasks.KindForeground, tasks.StatusComplete, "no traj", "simple")
	detail, err := proj.GetTask(context.Background(), id, string(taskID))
	if err != nil {
		t.Fatalf("GetTask: %v", err)
	}
	if detail.Trajectory != nil {
		t.Fatal("expected detail.Trajectory to be nil when enricher returns nil")
	}
}

// trajectoryStubEnricher is a test-only Enricher for Phase 107a projector
// tests. It returns a stubbed parent-session, cost, and a configurable
// trajectory ref (nil = no trajectory).
type trajectoryStubEnricher struct {
	ref *prototypes.TaskTrajectoryRef
}

func (s *trajectoryStubEnricher) ParentSession(_ context.Context, _ identity.Identity, _ string) prototypes.TaskParentSessionRef {
	return prototypes.TaskParentSessionRef{SessionID: "s1", AgentName: "stub", Status: "active"}
}

func (s *trajectoryStubEnricher) Cost(_ context.Context, _ identity.Identity, _ string) prototypes.TaskCostRollup {
	return prototypes.TaskCostRollup{PerStep: []prototypes.TaskCostStep{}}
}

func (s *trajectoryStubEnricher) PlannerSnapshot(_ context.Context, _ identity.Identity, _ string) *prototypes.TaskPlannerSnapshotRef {
	return nil
}

func (s *trajectoryStubEnricher) Trajectory(_ context.Context, _ identity.Identity, _ string) *prototypes.TaskTrajectoryRef {
	return s.ref
}
