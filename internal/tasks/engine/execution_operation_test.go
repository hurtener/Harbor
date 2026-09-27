package engine_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/tasks"
	"github.com/hurtener/Harbor/internal/tasks/engine"
)

func TestEngine_ExecutionOperationSurvivesTaskEncodingAndFencesRetry(t *testing.T) {
	bus := mkBus(t)
	t.Cleanup(func() { _ = bus.Close(context.Background()) })
	eng, err := engine.New(bus, auditpatterns.New(), &memBackend{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = eng.Close(context.Background()) })
	id := identity.Identity{TenantID: "tenant", UserID: "user", SessionID: "session"}
	ctx, err := identity.With(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	req := tasks.SpawnRequest{
		Identity: identity.Quadruple{Identity: id}, Kind: tasks.KindForeground,
		Query: "answer", IdempotencyKey: "start-key", VerifiedExecutionOperationID: "operation-one",
	}
	first, err := eng.Spawn(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := eng.Get(ctx, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(stored)
	if err != nil {
		t.Fatal(err)
	}
	var recovered tasks.Task
	if err := json.Unmarshal(raw, &recovered); err != nil || recovered.VerifiedExecutionOperationID != "operation-one" {
		t.Fatalf("recovered operation = %q, %v", recovered.VerifiedExecutionOperationID, err)
	}
	reused, err := eng.Spawn(ctx, req)
	if err != nil || !reused.Reused || reused.ID != first.ID {
		t.Fatalf("exact retry = %+v, %v", reused, err)
	}
	req.VerifiedExecutionOperationID = "operation-two"
	if _, err := eng.Spawn(ctx, req); !errors.Is(err, tasks.ErrIdempotencyConflict) {
		t.Fatalf("foreign operation reused start key: %v", err)
	}
}
