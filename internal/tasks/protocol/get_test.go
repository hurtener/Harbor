package protocol_test

import (
	"context"
	"errors"
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
