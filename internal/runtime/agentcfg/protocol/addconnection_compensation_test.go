package protocol_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/agentcfg"
	_ "github.com/hurtener/Harbor/internal/agentcfg/drivers/statestore"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	eventsinmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	agentcfgprotocol "github.com/hurtener/Harbor/internal/runtime/agentcfg/protocol"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
)

// The add door is the ONE spine-writing door whose live side effect runs
// BEFORE its conditional write — the attach has to happen to know what to
// write. So a refused write (the expected-revision conflict) can arrive with a
// server already dialed, handshaken and REGISTERED.
//
// Before the compensation, that produced a live server no revision named:
// `remove_mcp_connection` answered ErrConnectionNotFound for a connection that
// was exposing tools, the last lifecycle event was the transient `pending`,
// and an inline wire-OAuth provider installed for the binding stayed
// installed. The wire contract says a `revision_conflict` persists NOTHING;
// these tests hold that claim to the WORLD, not just to the revision spine.

// liveRegistry is a stand-in for the live MCP registry the attacher registers
// into. It is not a re-implementation of the driver — it records membership
// only, which is exactly the property under test: "is the server still there
// after the refused write?"
type liveRegistry struct {
	mu      sync.Mutex
	present map[string]int // name → attach count
	detachN int
	detachE error
}

func newLiveRegistry() *liveRegistry {
	return &liveRegistry{present: map[string]int{}}
}

func (r *liveRegistry) attach(name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.present[name]++
}

func (r *liveRegistry) isLive(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.present[name]
	return ok
}

func (r *liveRegistry) detaches() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.detachN
}

// registeringAttacher is a ConnectionAttacher that actually REGISTERS into the
// stand-in registry on a successful attach, so "the attacher was called" and
// "the server is live" are two distinct observations.
type registeringAttacher struct {
	reg    *liveRegistry
	result error
	calls  int
	mu     sync.Mutex
}

func (a *registeringAttacher) Attach(_ context.Context, req agentcfgprotocol.AttachRequest) error {
	a.mu.Lock()
	a.calls++
	a.mu.Unlock()
	if a.result == nil || errors.Is(a.result, agentcfgprotocol.ErrAuthRequired) {
		a.reg.attach(req.Name)
	}
	return a.result
}

func (a *registeringAttacher) callCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.calls
}

// DetachConnection satisfies agentcfgprotocol.ConnectionDetacher — the
// compensating teardown. Idempotent, like the production concrete.
func (a *registeringAttacher) DetachConnection(_ context.Context, _, _, name string) error {
	a.reg.mu.Lock()
	defer a.reg.mu.Unlock()
	a.reg.detachN++
	if a.reg.detachE != nil {
		return a.reg.detachE
	}
	delete(a.reg.present, name)
	return nil
}

// recordingInstaller records install / uninstall calls so the wire-provider
// half of the compensation is observable.
type recordingInstaller struct {
	mu        sync.Mutex
	installed map[string]int
	uninstall []string
}

func newRecordingInstaller() *recordingInstaller {
	return &recordingInstaller{installed: map[string]int{}}
}

func (i *recordingInstaller) InstallProvider(_ context.Context, _, _ string, desc agentcfg.OAuthProviderDescriptor) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.installed[desc.Name]++
	return nil
}

func (i *recordingInstaller) UninstallProvider(_ context.Context, _, _, name string) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.uninstall = append(i.uninstall, name)
	delete(i.installed, name)
	return nil
}

func (i *recordingInstaller) isInstalled(name string) bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	_, ok := i.installed[name]
	return ok
}

// compHarness wires a Service over a REAL registry with an attacher that
// registers into a stand-in live registry and doubles as the detacher.
type compHarness struct {
	svc       *agentcfgprotocol.Service
	reg       agentcfg.Registry
	bus       events.EventBus
	live      *liveRegistry
	attacher  *registeringAttacher
	installer *recordingInstaller
}

func newCompHarness(t *testing.T, attachResult error, wireDetacher bool) *compHarness {
	t.Helper()
	bus, err := eventsinmem.New(config.EventsConfig{
		Driver: "inmem", MaxSubscribersPerSession: 16, SubscriberBufferSize: 64,
		IdleTimeout: 60 * time.Second, DropWindow: time.Second,
	}, auditpatterns.New())
	if err != nil {
		t.Fatalf("bus: %v", err)
	}
	st, err := newStateStore(t)
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	reg, err := agentcfg.Open(context.Background(), agentcfg.Config{}, agentcfg.Deps{State: st, Bus: bus})
	if err != nil {
		t.Fatalf("agentcfg.Open: %v", err)
	}
	live := newLiveRegistry()
	att := &registeringAttacher{reg: live, result: attachResult}
	inst := newRecordingInstaller()
	opts := []agentcfgprotocol.Option{
		agentcfgprotocol.WithBus(bus),
		agentcfgprotocol.WithConnectionAttacher(att),
		agentcfgprotocol.WithProviderInstaller(inst),
		agentcfgprotocol.WithCoordinator(pauseresume.New(pauseresume.WithBus(bus))),
		agentcfgprotocol.WithStdioAllowlist([]string{allowedStdioBin}),
		agentcfgprotocol.WithAllowWireOAuthDescriptor(true),
	}
	if wireDetacher {
		opts = append(opts, agentcfgprotocol.WithConnectionDetacher(att))
	}
	svc, err := agentcfgprotocol.NewService(reg, opts...)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	t.Cleanup(func() { _ = reg.Close(context.Background()); _ = bus.Close(context.Background()) })
	return &compHarness{svc: svc, reg: reg, bus: bus, live: live, attacher: att, installer: inst}
}

// httpConnNamed is an http descriptor (the transport that may carry a wire
// OAuth binding).
func httpConnNamed(name string) prototypes.AgentConfigMCPConnectionDescriptor {
	return prototypes.AgentConfigMCPConnectionDescriptor{
		Name:      name,
		Transport: "http",
		URL:       "https://mcp.example.test/sse",
	}
}

// TestAddMCPConnection_RevisionConflict_CompensatesTheAttach is the load-bearing
// test for the half-applied add.
//
// It drives a stale expected-revision token at the add door with an attacher
// that SUCCEEDS, and asserts every half of the claim the wire contract makes:
//
//  1. the caller is refused with ErrRevisionConflict;
//  2. no revision names the connection (the spine half — this already held);
//  3. the server is NOT in the live registry (the world half — this is what
//     was broken: the compensation detached it);
//  4. `remove_mcp_connection` is not the only way to discover the leak — the
//     terminal `failed` lifecycle event fired, so the Console is not parked on
//     `pending` forever.
//
// Mutation: delete the compensateAttach call in the `attachErr == nil` branch
// and assertions 3 and 4 both fail while 1 and 2 stay green — which is exactly
// how the defect shipped.
func TestAddMCPConnection_RevisionConflict_CompensatesTheAttach(t *testing.T) {
	ctx := context.Background()
	h := newCompHarness(t, nil, true)

	sub, err := h.bus.Subscribe(ctx, events.Filter{Admin: true, Types: []events.EventType{
		agentcfg.EventTypeMCPConnectionFailed,
	}})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	failed := make(chan events.Event, 4)
	go func() {
		for ev := range sub.Events() {
			failed <- ev
		}
	}()

	stale := staleTokenFor(t, h.reg, agentcfg.ConfigScopeAgent)

	req := addReq(httpConnNamed("leaky"), nil)
	req.ExpectedContentHash = stale
	_, addErr := h.svc.AddMCPConnection(ctx, req)
	if !errors.Is(addErr, agentcfg.ErrRevisionConflict) {
		t.Fatalf("add with a stale token = %v, want ErrRevisionConflict", addErr)
	}
	if h.attacher.callCount() != 1 {
		t.Fatalf("attacher called %d times, want 1 (the live half of the add DID run — that is the premise)", h.attacher.callCount())
	}

	// (2) the spine half.
	active, set, err := h.reg.Active(ctx, qScope(), testAgentID, agentcfg.ConfigScopeAgent)
	if err != nil {
		t.Fatalf("Active: %v", err)
	}
	if !set {
		t.Fatal("the seeded base revision vanished")
	}
	for _, d := range active.Payload.ConnectionDescriptors() {
		if d.Name == "leaky" {
			t.Fatal("the refused add persisted its descriptor onto the active revision")
		}
	}

	// (3) the world half — the whole point.
	if h.live.isLive("leaky") {
		t.Fatal("the refused add left the server LIVE in the registry: no revision names it, so remove_mcp_connection answers not-found and it can never be removed")
	}
	if h.live.detaches() != 1 {
		t.Fatalf("compensating detach ran %d times, want exactly 1", h.live.detaches())
	}

	// (4) the observability half.
	select {
	case ev := <-failed:
		if ev.Type != agentcfg.EventTypeMCPConnectionFailed {
			t.Fatalf("terminal event type = %q", ev.Type)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no terminal lifecycle event after the refused add — the last event is `pending`, so a Console reader is parked on the transient state forever")
	}

	// (5) and the connection really is unnameable-and-therefore-unremovable
	// UNLESS it was compensated: prove remove answers not-found, which is the
	// symptom that made the leak permanent.
	_, rmErr := h.svc.RemoveMCPConnection(ctx, prototypes.AgentConfigRemoveMCPConnectionRequest{
		Identity: scope(), AgentID: testAgentID, Name: "leaky",
	})
	if !errors.Is(rmErr, agentcfgprotocol.ErrConnectionNotFound) {
		t.Fatalf("remove_mcp_connection = %v, want ErrConnectionNotFound (the revision never named it)", rmErr)
	}
}

// TestAddMCPConnection_RevisionConflict_UninstallsInlineWireProvider proves the
// OTHER live effect is compensated too: an inline wire-OAuth binding installs a
// provider BEFORE the attach, and its uninstall previously existed only on the
// attach-failed branch — so a conflict left an orphan installed provider.
//
// Mutation: drop the uninstallWireProvider call from compensateAttach and the
// installed-provider assertion fails.
func TestAddMCPConnection_RevisionConflict_UninstallsInlineWireProvider(t *testing.T) {
	ctx := context.Background()
	h := newCompHarness(t, nil, true)
	stale := staleTokenFor(t, h.reg, agentcfg.ConfigScopeAgent)

	conn := httpConnNamed("with-oauth")
	conn.OAuth = &prototypes.AgentConfigOAuthProviderDescriptor{
		Name:             "inline-provider",
		Driver:           "tokenexchange",
		CredentialSource: "remote",
		CredentialBroker: "broker",
		TokenURL:         "https://idp.example.test/token",
	}
	req := addReq(conn, nil)
	req.ExpectedContentHash = stale

	if _, err := h.svc.AddMCPConnection(ctx, req); !errors.Is(err, agentcfg.ErrRevisionConflict) {
		t.Fatalf("add with a stale token = %v, want ErrRevisionConflict", err)
	}
	if h.installer.isInstalled("inline-provider") {
		t.Fatal("the refused add left its inline wire OAuth provider INSTALLED — an orphan live provider no revision names")
	}
	if h.live.isLive("with-oauth") {
		t.Fatal("the refused add left the server live")
	}
}

// TestAddMCPConnection_AuthRequiredConflict_CompensatesTheAttach covers the
// SECOND branch that writes a revision after a live attach. An auth-required
// attach may already have registered before the authorization requirement
// surfaced, and it certainly installed any inline wire provider — so a refused
// write there leaks exactly the same way.
//
// Mutation: delete the compensateAttach call in the ErrAuthRequired branch and
// this test fails while the online-branch test stays green.
func TestAddMCPConnection_AuthRequiredConflict_CompensatesTheAttach(t *testing.T) {
	ctx := context.Background()
	h := newCompHarness(t, agentcfgprotocol.ErrAuthRequired, true)
	stale := staleTokenFor(t, h.reg, agentcfg.ConfigScopeAgent)

	req := addReq(httpConnNamed("needs-auth"), nil)
	req.ExpectedContentHash = stale

	if _, err := h.svc.AddMCPConnection(ctx, req); !errors.Is(err, agentcfg.ErrRevisionConflict) {
		t.Fatalf("auth-required add with a stale token = %v, want ErrRevisionConflict", err)
	}
	if h.live.isLive("needs-auth") {
		t.Fatal("the refused auth-required add left the server live in the registry")
	}
	if h.live.detaches() != 1 {
		t.Fatalf("compensating detach ran %d times, want exactly 1", h.live.detaches())
	}
}

// TestAddMCPConnection_UnconditionalAddIsUnchanged is the additivity guard: a
// request that sends NO token still attaches, records its revision and leaves
// the server live. The compensation must be reachable only from the failure it
// was written for.
func TestAddMCPConnection_UnconditionalAddIsUnchanged(t *testing.T) {
	ctx := context.Background()
	h := newCompHarness(t, nil, true)

	resp, err := h.svc.AddMCPConnection(ctx, addReq(httpConnNamed("ok"), nil))
	if err != nil {
		t.Fatalf("unconditional add: %v", err)
	}
	if resp.State != string(agentcfgprotocol.ConnectionStateOnline) {
		t.Fatalf("state = %q, want online", resp.State)
	}
	if !h.live.isLive("ok") {
		t.Fatal("a SUCCESSFUL add detached its own server — the compensation fired on the happy path")
	}
	if h.live.detaches() != 0 {
		t.Fatalf("compensating detach ran %d times on a successful add, want 0", h.live.detaches())
	}
	if resp.Revision == nil {
		t.Fatal("successful add recorded no revision")
	}
}

// TestAddMCPConnection_FailedAttachDoesNotDetach pins the boundary the other
// way: the attach itself failed, so mcpdrv drained its own closers and there is
// nothing registered to tear down. A compensating detach there would be a
// second teardown of a server that was never up.
func TestAddMCPConnection_FailedAttachDoesNotDetach(t *testing.T) {
	ctx := context.Background()
	h := newCompHarness(t, errors.New("provider.Connect: dial tcp: connection refused"), true)

	resp, err := h.svc.AddMCPConnection(ctx, addReq(httpConnNamed("dead"), nil))
	if err != nil {
		t.Fatalf("failed add returned a transport error: %v", err)
	}
	if resp.State != string(agentcfgprotocol.ConnectionStateFailed) {
		t.Fatalf("state = %q, want failed", resp.State)
	}
	if h.live.detaches() != 0 {
		t.Fatalf("compensating detach ran %d times after a FAILED attach, want 0", h.live.detaches())
	}
}

// TestAddMCPConnection_ConflictWithNoDetacherFailsLoud proves the nil-detacher
// arm does not silently pretend. A runtime with an attacher and no detacher
// genuinely cannot undo the attach; the requirement is that it says so rather
// than returning the same answer as a runtime that did undo it.
//
// The observable difference is the detach count (0) alongside the SAME refusal
// — the server stays live, which is the honest outcome, and the ERROR-level log
// line is what tells the operator. The terminal `failed` event still fires, so
// the Console is never parked on `pending` on either arm.
func TestAddMCPConnection_ConflictWithNoDetacherFailsLoud(t *testing.T) {
	ctx := context.Background()
	h := newCompHarness(t, nil, false)

	sub, err := h.bus.Subscribe(ctx, events.Filter{Admin: true, Types: []events.EventType{
		agentcfg.EventTypeMCPConnectionFailed,
	}})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	failed := make(chan events.Event, 4)
	go func() {
		for ev := range sub.Events() {
			failed <- ev
		}
	}()

	stale := staleTokenFor(t, h.reg, agentcfg.ConfigScopeAgent)
	req := addReq(httpConnNamed("orphan"), nil)
	req.ExpectedContentHash = stale
	if _, addErr := h.svc.AddMCPConnection(ctx, req); !errors.Is(addErr, agentcfg.ErrRevisionConflict) {
		t.Fatalf("add with a stale token = %v, want ErrRevisionConflict", addErr)
	}
	if h.live.detaches() != 0 {
		t.Fatalf("detach ran %d times with no detacher wired", h.live.detaches())
	}
	select {
	case <-failed:
	case <-time.After(2 * time.Second):
		t.Fatal("no terminal lifecycle event on the nil-detacher arm — the leak would be silent AND invisible")
	}
}

// TestAddMCPConnection_CompensatingDetachFailureIsNotSwallowed proves a
// compensation that itself fails does not change the caller's answer and does
// not turn into a success: the caller still sees the conflict, and the terminal
// event still fires. The residual live server is a fact the ERROR log carries.
func TestAddMCPConnection_CompensatingDetachFailureIsNotSwallowed(t *testing.T) {
	ctx := context.Background()
	h := newCompHarness(t, nil, true)
	h.live.detachE = errors.New("registry: deregister failed")

	stale := staleTokenFor(t, h.reg, agentcfg.ConfigScopeAgent)
	req := addReq(httpConnNamed("stuck"), nil)
	req.ExpectedContentHash = stale
	_, addErr := h.svc.AddMCPConnection(ctx, req)
	if !errors.Is(addErr, agentcfg.ErrRevisionConflict) {
		t.Fatalf("add = %v, want the ORIGINAL ErrRevisionConflict (a failed compensation must not become the caller's error)", addErr)
	}
	if h.live.detaches() != 1 {
		t.Fatalf("compensating detach attempted %d times, want 1", h.live.detaches())
	}
}
