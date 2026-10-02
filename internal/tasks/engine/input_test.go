package engine_test

import (
	"context"
	"errors"
	"testing"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/tasks"
	"github.com/hurtener/Harbor/internal/tasks/engine"
)

func TestEngine_InputReceiptPersistenceFailureDoesNotAcknowledge(t *testing.T) {
	backend := &memBackend{}
	bus := mkBus(t)
	t.Cleanup(func() { _ = bus.Close(t.Context()) })
	r, err := engine.New(bus, auditpatterns.New(), backend)
	if err != nil {
		t.Fatal(err)
	}
	ctx, _ := identity.With(t.Context(), identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"})
	id, _ := identity.From(ctx)
	h, err := r.Spawn(ctx, tasks.SpawnRequest{Identity: identity.Quadruple{Identity: id}, Kind: tasks.KindForeground})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.MarkRunning(ctx, h.ID); err != nil {
		t.Fatal(err)
	}
	backend.saveErr = errors.New("injected save failure")
	if _, err := r.AcceptInput(ctx, h.ID, "event", "correction"); err == nil {
		t.Fatal("failed persist acknowledged")
	}
	got, _ := r.Get(ctx, h.ID)
	if got.InputRevision != 0 || len(got.InputReceipts) != 0 {
		t.Fatalf("failed acceptance leaked %+v", got)
	}
	backend.saveErr = nil
	if _, err := r.AcceptInput(ctx, h.ID, "event", "correction"); err != nil {
		t.Fatal(err)
	}
	backend.saveErr = errors.New("injected apply save failure")
	if _, err := r.MarkInputApplied(ctx, h.ID, "event", 1); err == nil {
		t.Fatal("failed application acknowledged")
	}
	got, _ = r.Get(ctx, h.ID)
	if got.AppliedInputRevision != 0 || got.InputReceipts[0].Receipt.Status != tasks.InputAccepted {
		t.Fatalf("failed apply leaked %+v", got)
	}
	if _, err := r.Cancel(ctx, h.ID, "stop"); err == nil {
		t.Fatal("failed cancellation hidden")
	}
	got, _ = r.Get(ctx, h.ID)
	if got.Status != tasks.StatusRunning || got.InputReceipts[0].Receipt.Status != tasks.InputAccepted {
		t.Fatalf("failed terminal write leaked %+v", got)
	}
}

type emptyInputRedactor struct{}

func (emptyInputRedactor) Redact(context.Context, any) (any, error) {
	return map[string]any{"v": ""}, nil
}
func TestEngine_InputReceiptEmptyCanonicalTextFailsClosed(t *testing.T) {
	backend := &memBackend{}
	bus := mkBus(t)
	t.Cleanup(func() { _ = bus.Close(t.Context()) })
	r, err := engine.New(bus, emptyInputRedactor{}, backend)
	if err != nil {
		t.Fatal(err)
	}
	id := identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}
	ctx, _ := identity.With(t.Context(), id)
	h, err := r.Spawn(ctx, tasks.SpawnRequest{Identity: identity.Quadruple{Identity: id}, Kind: tasks.KindForeground})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.MarkRunning(ctx, h.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := r.AcceptInput(ctx, h.ID, "event", "nonempty before redaction"); !errors.Is(err, tasks.ErrInvalidRequest) {
		t.Fatalf("empty canonical input=%v", err)
	}
	got, _ := r.Get(ctx, h.ID)
	if got.InputRevision != 0 || len(got.InputReceipts) != 0 {
		t.Fatalf("empty canonical input was accepted=%+v", got)
	}
}
