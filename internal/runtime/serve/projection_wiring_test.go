package serve

import (
	"context"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/audit"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	_ "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
	"github.com/hurtener/Harbor/internal/sessions"
	sessionsprotocol "github.com/hurtener/Harbor/internal/sessions/protocol"
	"github.com/hurtener/Harbor/internal/state"
	_ "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	"github.com/hurtener/Harbor/internal/tasks"
)

// TestProdWiring_TasksProjectorInstallsApprovalChecker is the tasks
// prod-wiring test named by the projection-completeness contract (Half B).
// It proves the PRODUCTION serve.ApprovalChecker over a real pause
// coordinator answers `has_pending_approval` truthfully — an open
// ApprovalRequired gate on a session reads true; a bare session reads false;
// a nil coordinator leaves the seam unwired (nil checker), so a forgotten
// WithApprovalChecker in mux.go would ship a false absence the gate catches.
func TestProdWiring_TasksProjectorInstallsApprovalChecker(t *testing.T) {
	if NewApprovalChecker(nil) != nil {
		t.Fatal("NewApprovalChecker(nil) must return nil (unwired seam)")
	}
	coord := pauseresume.New()
	id := identity.Identity{TenantID: "t", UserID: "u", SessionID: "s-gated"}
	ctx, err := identity.With(context.Background(), id)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	if _, err := coord.Request(ctx, pauseresume.PauseRequest{
		Identity: id,
		Reason:   pauseresume.ReasonApprovalRequired,
	}); err != nil {
		t.Fatalf("coord.Request: %v", err)
	}
	checker := NewApprovalChecker(coord)
	if checker == nil {
		t.Fatal("NewApprovalChecker(coord) returned nil")
	}
	if !checker.HasPendingApproval(context.Background(), id, "") {
		t.Fatal("gated session: HasPendingApproval = false, want true")
	}
	other := identity.Identity{TenantID: "t", UserID: "u", SessionID: "s-clean"}
	if checker.HasPendingApproval(context.Background(), other, "") {
		t.Fatal("clean session: HasPendingApproval = true, want false (isolation)")
	}
}

// TestProdWiring_SessionsProjectorInstallsEnricher is the sessions
// prod-wiring test named by the projection-completeness contract (Half B).
// It proves the PRODUCTION sessions CounterEnricher assembled the way mux.go
// assembles it (real event bus + task registry + pause coordinator) makes
// the ListerProjector report CountersAvailable()==true, so the counter
// facets operate on real data — and that a projector built WITHOUT the
// enricher (a forgotten WithEnricher) reports false, the never-wired variant
// the Service loud-rejects rather than shipping a false-empty counter page.
func TestProdWiring_SessionsProjectorInstallsEnricher(t *testing.T) {
	red, err := audit.Open(context.Background(), config.AuditConfig{})
	if err != nil {
		t.Fatalf("audit.Open: %v", err)
	}
	bus, err := events.Open(context.Background(), config.EventsConfig{Driver: "inmem", MaxSubscribersPerSession: 64, SubscriberBufferSize: 512, IdleTimeout: 60 * time.Second, DropWindow: time.Second, ReplayBufferSize: 512}, red)
	if err != nil {
		t.Fatalf("events.Open: %v", err)
	}
	defer func() { _ = bus.Close(context.Background()) }()
	store, err := state.Open(context.Background(), config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	defer func() { _ = store.Close(context.Background()) }()
	taskReg, err := tasks.Open(context.Background(), tasks.Dependencies{
		Store: store, Bus: bus, Redactor: red,
		Cfg: config.TasksConfig{Driver: "inprocess"},
	})
	if err != nil {
		t.Fatalf("tasks.Open: %v", err)
	}
	defer func() { _ = taskReg.Close(context.Background()) }()
	sessReg, err := sessions.New(store, config.SessionsConfig{}, bus)
	if err != nil {
		t.Fatalf("sessions.New: %v", err)
	}
	coord := pauseresume.New()

	// The exact CounterEnricher deps mux.go assembles.
	enricher, err := sessionsprotocol.NewCounterEnricher(sessionsprotocol.CounterEnricherDeps{
		Bus: bus, Tasks: taskReg, Pauses: coord,
	})
	if err != nil {
		t.Fatalf("NewCounterEnricher: %v", err)
	}
	wired, err := sessionsprotocol.NewListerProjector(sessReg, sessionsprotocol.WithEnricher(enricher))
	if err != nil {
		t.Fatalf("NewListerProjector(wired): %v", err)
	}
	if !wired.CountersAvailable() {
		t.Fatal("prod-assembled sessions projector reports CountersAvailable()=false — a forgotten WithEnricher")
	}

	unwired, err := sessionsprotocol.NewListerProjector(sessReg)
	if err != nil {
		t.Fatalf("NewListerProjector(unwired): %v", err)
	}
	if unwired.CountersAvailable() {
		t.Fatal("projector with no enricher reports CountersAvailable()=true — the never-wired variant would ship false absence")
	}
}
