package protocol_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

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
