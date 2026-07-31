package serve

// The run loop's wiring of the run-start ATTACH pass (phase 216, D-361).
//
// Three properties are pinned here, none of which the projection-level unit
// tests can see:
//
//  1. the leg is actually WIRED — the counterpart of the pre-phase fact that
//     `grep -c Attach internal/runtime/serve/runloop.go` returned 0;
//  2. the pass ORDER against the sibling reconcile legs: the OAuth-provider
//     reconcile runs BEFORE the connection legs (so a provider the same revision
//     declares is installed before a connection binds it by name), and the
//     discovery-allowance re-apply runs AFTER them (so a freshly attached
//     connection receives its allowance in the SAME run start);
//  3. the failure posture: a refused or unreachable declared connection is loud
//     but NON-FATAL — the run still completes, and the remaining legs still run.

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/agentcfg"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/planner"
	"github.com/hurtener/Harbor/internal/runtime/agentcfg/projection"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
	"github.com/hurtener/Harbor/internal/runtime/steering"
	"github.com/hurtener/Harbor/internal/tasks"
	"github.com/hurtener/Harbor/internal/tools/auth"
)

const rlReattachAgent = "rl-reattach-agent"

// rlLegRecorder is one shared recorder every reconcile seam writes its call
// order into, so the ORDER assertions read one sequence rather than correlating
// three.
type rlLegRecorder struct {
	mu    sync.Mutex
	order []string
	// reattachOwners / reattachIDs record what the attach pass threaded through.
	reattachOwners []auth.Owner
	reattachIDs    []identity.Quadruple
	// reattachErr, when set, is returned by every Reattach.
	reattachErr error
}

func (r *rlLegRecorder) record(leg string) {
	r.mu.Lock()
	r.order = append(r.order, leg)
	r.mu.Unlock()
}

func (r *rlLegRecorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.order...)
}

// --- the three seams, all writing into one recorder ---

type rlDetacher struct {
	rec *rlLegRecorder
	// attached is the owner view; it is EMPTY, so every declared connection is a
	// re-attach candidate.
	attached []string
}

func (d *rlDetacher) AttachedSources(context.Context, auth.Owner) []string {
	d.rec.record("connections.view")
	return append([]string(nil), d.attached...)
}

func (d *rlDetacher) Detach(context.Context, string, auth.Owner) error {
	d.rec.record("connections.detach")
	return nil
}

func (d *rlDetacher) SetOAuthDiscoveryOrigins(_ context.Context, _ auth.Owner, _ string, _ []string) ([]string, error) {
	d.rec.record("allowance.apply")
	return nil, nil
}

type rlReattacher struct{ rec *rlLegRecorder }

func (r *rlReattacher) Reattach(_ context.Context, owner auth.Owner, id identity.Quadruple, _ agentcfg.MCPConnectionDescriptor) error {
	r.rec.mu.Lock()
	r.rec.order = append(r.rec.order, "connections.reattach")
	r.rec.reattachOwners = append(r.rec.reattachOwners, owner)
	r.rec.reattachIDs = append(r.rec.reattachIDs, id)
	err := r.rec.reattachErr
	r.rec.mu.Unlock()
	if err != nil {
		return err
	}
	// A successful re-attach makes the connection live, so the allowance leg's
	// view carries it in the same run start.
	return nil
}

type rlProviderReconciler struct {
	rec *rlLegRecorder
}

func (p *rlProviderReconciler) InstalledFor(context.Context, auth.Owner) []string {
	p.rec.record("providers.view")
	return nil
}

func (p *rlProviderReconciler) InstallProvider(context.Context, string, string, agentcfg.OAuthProviderDescriptor) error {
	p.rec.record("providers.install")
	return nil
}

func (p *rlProviderReconciler) UninstallProvider(context.Context, string, string, string) error {
	p.rec.record("providers.uninstall")
	return nil
}

// rlFinishProbe finishes every run immediately and signals that it ran, so the
// "a re-attach failure never fails the run" assertion has something to observe.
type rlFinishProbe struct{ ran chan struct{} }

func (p *rlFinishProbe) Next(context.Context, planner.RunContext) (planner.Decision, error) {
	select {
	case p.ran <- struct{}{}:
	default:
	}
	return planner.Finish{Reason: planner.FinishGoal}, nil
}

// rlRegistryDeclaring writes an active revision declaring one connection (absent
// from the live view, so it is a re-attach candidate) AND one installed OAuth
// provider (so the provider leg has work and its ordering is observable).
func rlRegistryDeclaring(t *testing.T, q identity.Quadruple) agentcfg.Registry {
	t.Helper()
	reg := acTestRegistry(t)
	if _, err := reg.SetRevision(context.Background(), q, rlReattachAgent, agentcfg.ConfigScopeAgent, agentcfg.ConfigPayload{
		Connections: &agentcfg.ConnectionsSection{Servers: []agentcfg.MCPConnectionDescriptor{{
			Name:                         "declared-absent",
			Transport:                    agentcfg.MCPTransportHTTP,
			URL:                          "https://example.invalid/mcp",
			OAuthDiscoveryAllowedOrigins: []string{"https://auth.example.invalid"},
		}}},
		OAuthProviders: &agentcfg.OAuthProvidersSection{Providers: []agentcfg.OAuthProviderDescriptor{{
			Name:             "declared-provider",
			CredentialBroker: "some-broker",
		}}},
	}, agentcfg.SetOptions{}); err != nil {
		t.Fatalf("seed revision: %v", err)
	}
	return reg
}

// runReattachProbe boots a driver wired with all three reconcile seams, spawns
// one task, and returns the recorder plus whether the run completed.
func runReattachProbe(t *testing.T, reattachErr error) (*rlLegRecorder, bool) {
	t.Helper()
	red := auditpatterns.New()
	bus := mkDriverTestBus(t, red)
	taskReg := mkDriverTestTaskRegistry(t, bus, red)
	steerReg := steering.NewRegistry()
	coord := pauseresume.New(pauseresume.WithBus(bus))
	rl, err := steering.NewRunLoop(steerReg, coord, steering.WithRunLoopBus(bus))
	if err != nil {
		t.Fatalf("steering.NewRunLoop: %v", err)
	}

	q := identity.Quadruple{Identity: runLoopDriverTestID}
	cfgReg := rlRegistryDeclaring(t, q)
	rec := &rlLegRecorder{reattachErr: reattachErr}

	probe := &rlFinishProbe{ran: make(chan struct{}, 1)}
	driver, err := NewRunLoopDriver(RunLoopDriverOptions{
		Bus:                     bus,
		RunLoop:                 rl,
		Planner:                 probe,
		Tasks:                   taskReg,
		AgentConfig:             cfgReg,
		AgentConfigID:           rlReattachAgent,
		ConnectionDetacher:      &rlDetacher{rec: rec},
		ConnectionReattacher:    &rlReattacher{rec: rec},
		OAuthProviderReconciler: &rlProviderReconciler{rec: rec},
	})
	if err != nil {
		t.Fatalf("NewRunLoopDriver: %v", err)
	}
	if err := driver.Start(context.Background()); err != nil {
		t.Fatalf("driver.Start: %v", err)
	}
	t.Cleanup(func() { _ = driver.Close(context.Background()) })

	ctx, err := identity.With(context.Background(), runLoopDriverTestID)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	if _, err := taskReg.Spawn(ctx, tasks.SpawnRequest{
		Identity: q,
		Kind:     tasks.KindForeground,
		Query:    "reattach wiring probe",
	}); err != nil {
		t.Fatalf("Spawn: %v", err)
	}

	select {
	case <-probe.ran:
		return rec, true
	case <-time.After(10 * time.Second):
		return rec, false
	}
}

// TestRunLoopDriver_Reattach_IsWiredAndOwnerScoped — the leg runs at run start,
// under the reconciling (tenant, agent) owner, carrying the run's quadruple.
func TestRunLoopDriver_Reattach_IsWiredAndOwnerScoped(t *testing.T) {
	rec, ran := runReattachProbe(t, nil)
	if !ran {
		t.Fatal("the run never reached the planner")
	}
	order := rec.snapshot()
	found := false
	for _, leg := range order {
		if leg == "connections.reattach" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the run-start attach pass never ran (legs=%v) — the leg is not wired", order)
	}

	rec.mu.Lock()
	owners := append([]auth.Owner(nil), rec.reattachOwners...)
	ids := append([]identity.Quadruple(nil), rec.reattachIDs...)
	rec.mu.Unlock()

	want := auth.Owner{Tenant: runLoopDriverTestID.TenantID, Agent: rlReattachAgent}
	for i, o := range owners {
		if o != want {
			t.Fatalf("re-attach #%d ran under owner %+v, want %+v", i, o, want)
		}
	}
	for i, id := range ids {
		if id.TenantID != runLoopDriverTestID.TenantID || id.SessionID != runLoopDriverTestID.SessionID {
			t.Fatalf("re-attach #%d carried identity %+v, want the run's triple", i, id)
		}
		if id.RunID == "" {
			t.Fatalf("re-attach #%d carried no RunID — the reconcile/admin-add discriminator is empty", i)
		}
	}
}

// TestRunLoopDriver_Reattach_LegOrdering pins the two orderings the attach pass
// depends on, so neither can be reordered silently.
func TestRunLoopDriver_Reattach_LegOrdering(t *testing.T) {
	rec, ran := runReattachProbe(t, nil)
	if !ran {
		t.Fatal("the run never reached the planner")
	}
	order := rec.snapshot()

	idx := func(leg string) int {
		for i, l := range order {
			if l == leg {
				return i
			}
		}
		return -1
	}
	// lastIdx finds the LAST occurrence — the allowance leg's owner-view read is
	// the third "connections.view", after the detach and attach passes'.
	lastIdx := func(leg string) int {
		last := -1
		for i, l := range order {
			if l == leg {
				last = i
			}
		}
		return last
	}
	providerLeg := idx("providers.view")
	attachLeg := idx("connections.reattach")
	firstView := idx("connections.view")
	allowanceView := lastIdx("connections.view")

	if providerLeg < 0 || attachLeg < 0 {
		t.Fatalf("a leg never ran: legs=%v", order)
	}
	// (a) A provider the same revision declares is installed BEFORE a connection
	//     binds it by name.
	if providerLeg > attachLeg {
		t.Fatalf("the OAuth-provider reconcile ran AFTER the attach pass (legs=%v); a connection binding an "+
			"owner-installed provider would then fail to resolve on the run start that declares both", order)
	}
	// (b) A freshly attached connection receives its discovery allowance in the
	//     SAME run start: the allowance leg's view read comes AFTER the attach.
	if allowanceView <= attachLeg {
		t.Fatalf("the discovery-allowance re-apply did not run after the attach pass (legs=%v); a freshly "+
			"attached connection would then wait a whole extra run for its allowance", order)
	}
	// (c) Detach precedes attach inside the connections leg.
	if firstView > attachLeg {
		t.Fatalf("the connections leg's first view read came after the attach pass (legs=%v)", order)
	}
}

// TestRunLoopDriver_Reattach_FailureIsLoudButNeverFailsTheRun — a refused or
// unreachable declared connection does not fail the run, does not abort the
// sweep, and does not stop the sibling legs.
func TestRunLoopDriver_Reattach_FailureIsLoudButNeverFailsTheRun(t *testing.T) {
	rec, ran := runReattachProbe(t, errors.New("third party unreachable"))
	if !ran {
		t.Fatal("a re-attach failure must NOT prevent the run from reaching the planner")
	}
	order := rec.snapshot()
	sawAttach, views := false, 0
	for _, l := range order {
		switch l {
		case "connections.reattach":
			sawAttach = true
		case "connections.view":
			views++
		}
	}
	if !sawAttach {
		t.Fatalf("the attach pass never ran: legs=%v", order)
	}
	// Three owner-view reads means all three passes ran: detach, attach, and the
	// discovery-allowance re-apply. The allowance leg reaching its view AFTER a
	// failed re-attach is the property under test — one refused third party must
	// not stop every connection's allowance re-apply. (The apply itself does not
	// fire here because this fixture's live view is empty by construction; the
	// leg running at all is what a re-attach failure could have prevented.)
	if views != 3 {
		t.Fatalf("owner-view reads = %d after a failed re-attach (legs=%v), want 3 "+
			"(detach + attach + allowance); a missing third read means one unreachable "+
			"server stranded the sibling leg", views, order)
	}
}

// TestSplitReconcileErrors_PartitionsByOrigin pins the classification the run
// loop's log levels depend on: a suppressed re-attach is Debug, a loud re-attach
// is Error-but-continue, and anything else keeps the shipped abort behaviour.
func TestSplitReconcileErrors_PartitionsByOrigin(t *testing.T) {
	detachErr := errors.New("detach refused")
	// A NESTED join, because that is the shape the projection actually produces:
	// errors.Join over per-connection errors, each itself a %w chain.
	other, reLoud, suppressed := splitReconcileErrors(errors.Join(
		errors.Join(detachErr),
		errWrap(projection.ErrReconcileReattach, "unreachable"),
		errWrap(ErrReattachSuppressed, "backing off"),
	))
	if other == nil || !errors.Is(other, detachErr) {
		t.Fatalf("other = %v, want the detach error", other)
	}
	if reLoud == nil || !errors.Is(reLoud, projection.ErrReconcileReattach) {
		t.Fatalf("reattachLoud = %v, want the marked re-attach error", reLoud)
	}
	if suppressed == nil || !errors.Is(suppressed, ErrReattachSuppressed) {
		t.Fatalf("suppressed = %v, want the suppressed sentinel", suppressed)
	}
	// A suppressed leaf must NOT also land in the loud bucket, or a permanently
	// unreachable server writes an Error line on every run start.
	if errors.Is(reLoud, ErrReattachSuppressed) {
		t.Fatal("a suppressed re-attach leaked into the loud bucket")
	}
	// The detach bucket must not swallow a re-attach error either — that would
	// restore the abort-the-remaining-legs behaviour for an unreachable server.
	if errors.Is(other, projection.ErrReconcileReattach) {
		t.Fatal("a re-attach error leaked into the abort-the-run-legs bucket")
	}

	// A nil error partitions to three nils (no spurious log lines).
	if a, b, c := splitReconcileErrors(nil); a != nil || b != nil || c != nil {
		t.Fatalf("splitReconcileErrors(nil) = %v/%v/%v, want three nils", a, b, c)
	}
}

// errWrap wraps sentinel with a message, mirroring how the projection and the
// attacher wrap their own errors.
func errWrap(sentinel error, msg string) error {
	return errJoinWrapper{sentinel: sentinel, msg: msg}
}

type errJoinWrapper struct {
	sentinel error
	msg      string
}

func (e errJoinWrapper) Error() string { return e.msg + ": " + e.sentinel.Error() }
func (e errJoinWrapper) Unwrap() error { return e.sentinel }
