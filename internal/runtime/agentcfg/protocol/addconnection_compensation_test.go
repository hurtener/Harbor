package protocol_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/agentcfg"
	_ "github.com/hurtener/Harbor/internal/agentcfg/drivers/statestore"
	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	eventsinmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
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

// liveOwner is the `(tenant, agent)` owner tag the real MCP registry stamps on
// a runtime-added registration (`auth.Owner`, without the driver import). The
// double carries it because the owner is HALF the address of a registration:
// `Registry.Deregister` resolves `(name, owner)` and answers a foreign owner
// with ErrServerNotFound.
type liveOwner struct{ tenant, agent string }

// liveRegistry is a stand-in for the live MCP registry the attacher registers
// into. It is not a re-implementation of the driver — it records membership
// and its OWNER, which is exactly the property under test: "is the server still
// there after the refused write, and was it addressed as its own owner?"
//
// Owner scoping is modelled STRUCTURALLY rather than merely asserted, because
// the production consequence of getting it wrong is a SILENT no-op:
// `Registry.Deregister` answers another owner's registration as absent
// (ErrServerNotFound) and `detachSource` swallows that as idempotent, so a
// compensating detach addressed to the wrong owner returns nil, removes
// nothing, and re-opens the leak invisibly. A double that dropped the owner
// reported that mutation as a pass.
type liveRegistry struct {
	mu       sync.Mutex
	present  map[string]liveOwner // name → the owner whose attach registered it
	detachN  int
	detachTo []liveOwner // the (tenant, agent) each DetachConnection was handed
	detachE  error
}

func newLiveRegistry() *liveRegistry {
	return &liveRegistry{present: map[string]liveOwner{}}
}

func (r *liveRegistry) attach(name string, owner liveOwner) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.present[name] = owner
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

// detachOwners returns the owner each compensating detach was called with, in
// call order.
func (r *liveRegistry) detachOwners() []liveOwner {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]liveOwner(nil), r.detachTo...)
}

// registeringAttacher is a ConnectionAttacher that actually REGISTERS into the
// stand-in registry on a successful attach, so "the attacher was called" and
// "the server is live" are two distinct observations. It registers under the
// owner the AttachRequest carries, so the owner the detach must match is
// derived from production input rather than restated by the test.
type registeringAttacher struct {
	reg    *liveRegistry
	result error
	calls  int
	owners []liveOwner
	mu     sync.Mutex
}

func (a *registeringAttacher) Attach(_ context.Context, req agentcfgprotocol.AttachRequest) error {
	owner := liveOwner{tenant: req.Identity.TenantID, agent: req.AgentID}
	a.mu.Lock()
	a.calls++
	a.owners = append(a.owners, owner)
	result := a.result
	a.mu.Unlock()
	if result == nil || errors.Is(result, agentcfgprotocol.ErrAuthRequired) {
		a.reg.attach(req.Name, owner)
	}
	return result
}

// setResult re-scripts the attach outcome between calls, so one harness can
// drive a SUCCESSFUL first add and a FAILING re-add — the sequence that
// exercises the attach-failed rollback against state a successful add left
// behind.
func (a *registeringAttacher) setResult(err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.result = err
}

func (a *registeringAttacher) callCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.calls
}

// attachOwner returns the owner the LAST attach stamped on its registration.
func (a *registeringAttacher) attachOwner(t *testing.T) liveOwner {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.owners) == 0 {
		t.Fatal("attacher was never called")
	}
	return a.owners[len(a.owners)-1]
}

// DetachConnection satisfies agentcfgprotocol.ConnectionDetacher — the
// compensating teardown.
//
// It reproduces the production resolution rule rather than a name lookup: the
// entry is removed ONLY when the caller's `(tenant, agent)` equals the owner
// the attach stamped. A foreign owner returns nil (production's
// ErrServerNotFound, swallowed as idempotent by `detachSource`) and removes
// NOTHING — so a wrong-owner call is observable exactly the way it is
// observable in production: by the server still being live.
func (a *registeringAttacher) DetachConnection(_ context.Context, tenant, agentID, name string) error {
	caller := liveOwner{tenant: tenant, agent: agentID}
	a.reg.mu.Lock()
	defer a.reg.mu.Unlock()
	a.reg.detachN++
	a.reg.detachTo = append(a.reg.detachTo, caller)
	if a.reg.detachE != nil {
		return a.reg.detachE
	}
	owner, ok := a.reg.present[name]
	if !ok || owner != caller {
		return nil // ErrServerNotFound in production; swallowed as idempotent.
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
	// onInstall is a ONE-SHOT interleaving hook fired after an install is
	// recorded. It is the deterministic stand-in for a racing sibling writer:
	// the install sits exactly between this add's "is the provider already
	// declared?" read and its compensation, which is the window a sibling's
	// winning revision lands in. It disarms itself so the sibling's own install
	// cannot re-enter.
	onInstall func()
}

func newRecordingInstaller() *recordingInstaller {
	return &recordingInstaller{installed: map[string]int{}}
}

func (i *recordingInstaller) InstallProvider(_ context.Context, _, _ string, desc agentcfg.OAuthProviderDescriptor) error {
	i.mu.Lock()
	i.installed[desc.Name]++
	hook := i.onInstall
	i.onInstall = nil
	i.mu.Unlock()
	if hook != nil {
		hook()
	}
	return nil
}

// setOnInstall arms the one-shot interleaving hook.
func (i *recordingInstaller) setOnInstall(f func()) {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.onInstall = f
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
	return newCompHarnessWrapped(t, attachResult, wireDetacher, nil)
}

// newCompHarnessWrapped is newCompHarness with a fault decorator interposed
// between the Service and the REAL registry. `h.reg` stays the undecorated
// registry, so the test's own assertions read ground truth rather than the
// fault's story about it.
func newCompHarnessWrapped(t *testing.T, attachResult error, wireDetacher bool, wrap func(agentcfg.Registry) agentcfg.Registry) *compHarness {
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
	svcReg := reg
	if wrap != nil {
		svcReg = wrap(reg)
	}
	svc, err := agentcfgprotocol.NewService(svcReg, opts...)
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

// ---------------------------------------------------------------------------
// The compensation must undo only what THIS CALL created.
//
// The compensation shipped UNCONDITIONAL, and its boundary tests only ever
// covered the fresh-name case. A same-owner same-name re-attach is allowed and
// INTENDED (`internal/tools/drivers/mcp/registry.go` — "a re-attach that
// supersedes a still-live connection is the operator replacing their own"), so
// re-adding an ALREADY-LIVE connection with a stale token drove the
// compensation against a server the ACTIVE revision still names: catalog tools
// deregistered, transport closed, a terminal `failed` emitted for a healthy
// connection, in-flight runs stripped of their tools. A REFUSED write became
// DESTRUCTIVE — the same broken claim the compensation set out to fix, in the
// opposite direction.
// ---------------------------------------------------------------------------

// activeDeclaresConn reports whether the agent's ACTIVE agent-scope revision
// currently names the connection — the SAME predicate the run-start reconcile
// uses to decide "keep it attached".
func activeDeclaresConn(t *testing.T, h *compHarness, name string) bool {
	t.Helper()
	active, set, err := h.reg.Active(context.Background(), qScope(), testAgentID, agentcfg.ConfigScopeAgent)
	if err != nil {
		t.Fatalf("Active: %v", err)
	}
	if !set {
		return false
	}
	for _, d := range active.Payload.ConnectionDescriptors() {
		if d.Name == name {
			return true
		}
	}
	return false
}

// activeDeclaresProvider is the provider-plane twin of activeDeclaresConn.
func activeDeclaresProvider(t *testing.T, h *compHarness, name string) bool {
	t.Helper()
	active, set, err := h.reg.Active(context.Background(), qScope(), testAgentID, agentcfg.ConfigScopeAgent)
	if err != nil {
		t.Fatalf("Active: %v", err)
	}
	if !set {
		return false
	}
	for _, p := range active.Payload.OAuthProviderDescriptors() {
		if p.Name == name {
			return true
		}
	}
	return false
}

// TestAddMCPConnection_RevisionConflict_KeepsAStillDeclaredConnection is the
// regression probe.
//
// An operator's `github` connection is added and goes LIVE. A sibling write
// moves the base. The operator then re-adds `github` (the same-owner same-name
// replace the registry explicitly permits) holding the now-stale token and is
// answered `revision_conflict` — a REFUSAL, which the wire contract says
// changes nothing.
//
// Mutation: make compensateAttach detach unconditionally again and the two
// world-half assertions fail while the refusal and the still-declared
// assertions stay green — which is exactly how the regression shipped.
func TestAddMCPConnection_RevisionConflict_KeepsAStillDeclaredConnection(t *testing.T) {
	ctx := context.Background()
	h := newCompHarness(t, nil, true)

	// (1) The operator's connection is added and comes online.
	first, err := h.svc.AddMCPConnection(ctx, addReq(httpConnNamed("github"), nil))
	if err != nil {
		t.Fatalf("first add: %v", err)
	}
	if first.Revision == nil {
		t.Fatal("first add recorded no revision")
	}
	if !h.live.isLive("github") {
		t.Fatal("the first add did not bring the server live — the premise of this probe")
	}
	if h.live.detaches() != 0 {
		t.Fatalf("after a successful add: detaches = %d, want 0", h.live.detaches())
	}
	stale := first.Revision.ContentHash

	// (2) A sibling write moves the base, through the door itself so the
	//     connections section is PRESERVED (a raw registry write would drop it).
	if _, err := h.svc.AddMCPConnection(ctx, addReq(httpConnNamed("other"), nil)); err != nil {
		t.Fatalf("sibling add: %v", err)
	}
	if !activeDeclaresConn(t, h, "github") {
		t.Fatal("the sibling add dropped the first connection from the active revision")
	}

	// (3) The re-add is REFUSED on the stale token.
	req := addReq(httpConnNamed("github"), nil)
	req.ExpectedContentHash = stale
	if _, addErr := h.svc.AddMCPConnection(ctx, req); !errors.Is(addErr, agentcfg.ErrRevisionConflict) {
		t.Fatalf("re-add with a stale token = %v, want ErrRevisionConflict", addErr)
	}

	// (4) THE COLLATERAL. The active revision still names `github`, so tearing
	//     it down leaves a DECLARED connection dark: its catalog tools are
	//     deregistered and its transport closed, and in-flight runs lose them
	//     until the next run-start reconcile re-attaches it.
	if !activeDeclaresConn(t, h, "github") {
		t.Fatal("the refused re-add removed the connection from the active revision")
	}
	if !h.live.isLive("github") {
		t.Fatal("COLLATERAL: a REFUSED write tore down a pre-existing live server that the ACTIVE revision still names")
	}
	if h.live.detaches() != 0 {
		t.Fatalf("the refused re-add ran %d compensating detaches against a still-declared connection, want 0 — a refusal must undo only what THIS call created", h.live.detaches())
	}
}

// TestAddMCPConnection_RevisionConflict_KeepsAStillDeclaredWireProvider is the
// same defect one plane over, found by asking whether `wireProviderIsNew`
// guards the re-add case.
//
// It did not: the inline branch of prepareWireOAuthBinding returned
// `isNew = true` UNCONDITIONALLY, so a re-add carrying the same inline binding
// reported a provider the ACTIVE revision already names as "newly installed by
// this call" — and the refusal then UNINSTALLED it, breaking southbound bearer
// injection for the still-declared connection that binds it.
//
// Mutation: return an unconditional `true` from the inline branch again and the
// installed-provider assertion fails.
func TestAddMCPConnection_RevisionConflict_KeepsAStillDeclaredWireProvider(t *testing.T) {
	ctx := context.Background()
	h := newCompHarness(t, nil, true)

	inline := func() *prototypes.AgentConfigOAuthProviderDescriptor {
		return &prototypes.AgentConfigOAuthProviderDescriptor{
			Name:             "gh-provider",
			Driver:           "tokenexchange",
			CredentialSource: "remote",
			CredentialBroker: "broker",
			TokenURL:         "https://idp.example.test/token",
		}
	}

	// (1) The connection + its inline wire provider are installed and live.
	conn := httpConnNamed("github")
	conn.OAuth = inline()
	first, err := h.svc.AddMCPConnection(ctx, addReq(conn, nil))
	if err != nil {
		t.Fatalf("first add: %v", err)
	}
	if !h.installer.isInstalled("gh-provider") {
		t.Fatal("the first add did not install the inline wire provider — the premise of this probe")
	}
	if !activeDeclaresProvider(t, h, "gh-provider") {
		t.Fatal("the first add did not record the wire provider onto the revision")
	}
	stale := first.Revision.ContentHash

	// (2) A sibling write moves the base.
	if _, err := h.svc.AddMCPConnection(ctx, addReq(httpConnNamed("other"), nil)); err != nil {
		t.Fatalf("sibling add: %v", err)
	}

	// (3) The re-add carrying the SAME inline binding is refused.
	re := httpConnNamed("github")
	re.OAuth = inline()
	req := addReq(re, nil)
	req.ExpectedContentHash = stale
	if _, addErr := h.svc.AddMCPConnection(ctx, req); !errors.Is(addErr, agentcfg.ErrRevisionConflict) {
		t.Fatalf("re-add with a stale token = %v, want ErrRevisionConflict", addErr)
	}

	// (4) THE COLLATERAL, provider plane.
	if !activeDeclaresProvider(t, h, "gh-provider") {
		t.Fatal("the refused re-add removed the provider from the active revision")
	}
	if !h.installer.isInstalled("gh-provider") {
		t.Fatal("COLLATERAL: a REFUSED write UNINSTALLED a wire OAuth provider the ACTIVE revision still names — the still-declared connection that binds it loses southbound bearer injection")
	}
	if !h.live.isLive("github") {
		t.Fatal("COLLATERAL: the refused re-add also tore down the still-declared server")
	}
}

// TestAddMCPConnection_RevisionConflict_KeepsAProviderASiblingDeclared covers
// the window `wireProviderIsNew` structurally cannot: the provider was NOT
// declared when this add read the revision (so the install genuinely was new to
// this caller), and a racing sibling's revision — the very write that moves the
// base and produces this add's 409 — declares it before the compensation runs.
//
// A pre-attach flag cannot answer a post-attach question, which is why the
// compensation re-reads rather than trusting it. The interleaving is driven
// deterministically through the installer hook (the install is inside the
// window), never a sleep.
//
// Mutation: drop `&& !providerDeclared` from the compensation's uninstall call
// and this fails.
func TestAddMCPConnection_RevisionConflict_KeepsAProviderASiblingDeclared(t *testing.T) {
	ctx := context.Background()
	h := newCompHarness(t, nil, true)

	inline := func() *prototypes.AgentConfigOAuthProviderDescriptor {
		return &prototypes.AgentConfigOAuthProviderDescriptor{
			Name:             "shared-idp",
			Driver:           "tokenexchange",
			CredentialSource: "remote",
			CredentialBroker: "broker",
			TokenURL:         "https://idp.example.test/token",
		}
	}

	stale := staleTokenFor(t, h.reg, agentcfg.ConfigScopeAgent)

	// The racing sibling: a DIFFERENT connection binding the SAME provider,
	// written in the window between this add's declaration read and its
	// compensation. A different connection name isolates the provider guard from
	// the connection guard.
	h.installer.setOnInstall(func() {
		mirror := httpConnNamed("github-mirror")
		mirror.OAuth = inline()
		if _, err := h.svc.AddMCPConnection(ctx, addReq(mirror, nil)); err != nil {
			t.Errorf("sibling add: %v", err)
		}
	})

	conn := httpConnNamed("github")
	conn.OAuth = inline()
	req := addReq(conn, nil)
	req.ExpectedContentHash = stale
	if _, addErr := h.svc.AddMCPConnection(ctx, req); !errors.Is(addErr, agentcfg.ErrRevisionConflict) {
		t.Fatalf("add with a stale token = %v, want ErrRevisionConflict", addErr)
	}

	if !activeDeclaresProvider(t, h, "shared-idp") {
		t.Fatal("the sibling write did not land — the interleaving this test needs did not happen")
	}
	if activeDeclaresConn(t, h, "github") {
		t.Fatal("the refused add's own connection ended up declared — this test no longer isolates the provider guard")
	}
	// The connection guard correctly does NOT fire (nothing declares `github`),
	// so the server is torn down — the D-370 behaviour, unchanged.
	if h.live.isLive("github") {
		t.Fatal("the refused add left an UNDECLARED server live — the leak the compensation exists to close")
	}
	// The provider guard DOES fire: the sibling's still-declared `github-mirror`
	// binds it.
	if !h.installer.isInstalled("shared-idp") {
		t.Fatal("COLLATERAL: the refused add uninstalled a provider a racing sibling's WINNING revision declares — the sibling's still-declared connection loses southbound bearer injection")
	}
	if !h.live.isLive("github-mirror") {
		t.Fatal("the refused add tore down the sibling's connection")
	}
}

// TestAddMCPConnection_FailedAttach_KeepsAStillDeclaredWireProvider is the
// attach-FAILED twin of the test above, and it is the one that pins
// `wireProviderIsNew` itself rather than the compensation's own scoping.
//
// The rollback on a failed attach has no second guard to fall back on: it
// trusts `wireProviderIsNew` outright. So an inline install reported as "new"
// when it was really an UPSERT over an already-declared provider means a
// connection that merely fails to DIAL uninstalls a provider other
// still-declared connections bind — no conflict, no refusal, just a dead
// third-party endpoint taking a live credential binding with it.
//
// Mutation: return an unconditional `true` from the inline branch of
// prepareWireOAuthBinding and this fails.
func TestAddMCPConnection_FailedAttach_KeepsAStillDeclaredWireProvider(t *testing.T) {
	ctx := context.Background()
	h := newCompHarness(t, nil, true)

	inline := func() *prototypes.AgentConfigOAuthProviderDescriptor {
		return &prototypes.AgentConfigOAuthProviderDescriptor{
			Name:             "shared-provider",
			Driver:           "tokenexchange",
			CredentialSource: "remote",
			CredentialBroker: "broker",
			TokenURL:         "https://idp.example.test/token",
		}
	}

	// (1) The connection + its inline wire provider are installed and declared.
	conn := httpConnNamed("github")
	conn.OAuth = inline()
	if _, err := h.svc.AddMCPConnection(ctx, addReq(conn, nil)); err != nil {
		t.Fatalf("first add: %v", err)
	}
	if !activeDeclaresProvider(t, h, "shared-provider") {
		t.Fatal("the first add did not record the wire provider onto the revision")
	}

	// (2) The re-add's DIAL fails. Nothing is refused and nothing conflicts —
	//     the third party is simply down.
	h.attacher.setResult(errors.New("provider.Connect: dial tcp: connection refused"))
	re := httpConnNamed("github")
	re.OAuth = inline()
	resp, err := h.svc.AddMCPConnection(ctx, addReq(re, nil))
	if err != nil {
		t.Fatalf("failed re-add returned a transport error: %v", err)
	}
	if resp.State != string(agentcfgprotocol.ConnectionStateFailed) {
		t.Fatalf("state = %q, want failed", resp.State)
	}

	// (3) THE COLLATERAL. The rollback may only undo what THIS call created,
	//     and this call re-installed a provider that already existed.
	if !activeDeclaresProvider(t, h, "shared-provider") {
		t.Fatal("the failed re-add removed the provider from the active revision")
	}
	if !h.installer.isInstalled("shared-provider") {
		t.Fatal("COLLATERAL: a FAILED DIAL uninstalled a wire OAuth provider the ACTIVE revision still names — an unreachable third party took a live credential binding down with it")
	}
}

// TestAddMCPConnection_RevisionConflict_RetainedReasonNamesTheUntouchedServer
// pins the OBSERVABILITY half of the retained path. The terminal event must
// still fire (never leave a Console on the transient `pending`), and its reason
// must say plainly that nothing was torn down — a bare `failed` for a healthy,
// still-declared, still-serving connection is itself misleading.
//
// Mutation: drop the retained-reason prefix and this fails while the
// event-fired assertion stays green.
func TestAddMCPConnection_RevisionConflict_RetainedReasonNamesTheUntouchedServer(t *testing.T) {
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

	first, err := h.svc.AddMCPConnection(ctx, addReq(httpConnNamed("github"), nil))
	if err != nil {
		t.Fatalf("first add: %v", err)
	}
	stale := first.Revision.ContentHash
	if _, err := h.svc.AddMCPConnection(ctx, addReq(httpConnNamed("other"), nil)); err != nil {
		t.Fatalf("sibling add: %v", err)
	}

	req := addReq(httpConnNamed("github"), nil)
	req.ExpectedContentHash = stale
	if _, addErr := h.svc.AddMCPConnection(ctx, req); !errors.Is(addErr, agentcfg.ErrRevisionConflict) {
		t.Fatalf("re-add with a stale token = %v, want ErrRevisionConflict", addErr)
	}

	select {
	case ev := <-failed:
		payload, ok := ev.Payload.(agentcfg.MCPConnectionLifecyclePayload)
		if !ok {
			t.Fatalf("terminal event payload type = %T", ev.Payload)
		}
		if payload.ServerID != "github" {
			t.Fatalf("terminal event server_id = %q, want github", payload.ServerID)
		}
		if !strings.Contains(payload.Reason, "NOT torn down") {
			t.Fatalf("terminal reason = %q — it does not say the pre-existing connection was left in place, so a reader sees a bare `failed` for a healthy, still-declared, still-serving connection", payload.Reason)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no terminal lifecycle event on the retained path — a Console reader is parked on the transient `pending` forever")
	}
}

// ---------------------------------------------------------------------------
// "The write failed" is what the store SAID, not what the disk did.
//
// The sibling correction one layer down (the driver's compensating delete)
// names THIS door as carrying the same commit-then-error shape. It does not,
// and the reason is not a second mechanism: scoping the teardown by
// DECLARED-NESS, re-read after the failure, answers the commit-then-error
// question as a side effect, because "did the write land?" and "does the
// active pointer name it?" are THE SAME QUESTION asked of the same read.
//
// The three rows below hold that claim to execution rather than to reasoning,
// across the three disk states one identical error value can hide.
// ---------------------------------------------------------------------------

// landedThenErroredRegistry lands the write COMPLETELY — the revision record is
// written and the active pointer moves to it — and then reports it as FAILED.
// The commonest production shape: a deadline firing after commit, a dropped
// ack, a proxy timeout, a connection reset with the response in flight.
type landedThenErroredRegistry struct {
	agentcfg.Registry
	err error
}

func (r *landedThenErroredRegistry) SetRevision(ctx context.Context, id identity.Quadruple, agentID string, scope agentcfg.ConfigScope, payload agentcfg.ConfigPayload, opts agentcfg.SetOptions) (agentcfg.Revision, error) {
	rev, err := r.Registry.SetRevision(ctx, id, agentID, scope, payload, opts)
	if err != nil {
		return rev, err
	}
	return agentcfg.Revision{}, r.err
}

// recordLandedPointerStuckRegistry models the PARTIAL landing: the revision
// record is durably written but the active-pointer write does not move. The
// pointer is put back where it was, so history carries a record nothing
// references — which is precisely the orphan the driver's own compensation
// exists to remove.
type recordLandedPointerStuckRegistry struct {
	agentcfg.Registry
	err error
}

func (r *recordLandedPointerStuckRegistry) SetRevision(ctx context.Context, id identity.Quadruple, agentID string, scope agentcfg.ConfigScope, payload agentcfg.ConfigPayload, opts agentcfg.SetOptions) (agentcfg.Revision, error) {
	// Active and Rollback are the embedded registry's own (this type overrides
	// neither); only SetRevision needs the explicit selector, to reach past its
	// own override.
	prev, hadPrev, aErr := r.Active(ctx, id, agentID, scope)
	rev, err := r.Registry.SetRevision(ctx, id, agentID, scope, payload, opts)
	if err != nil {
		return rev, err
	}
	if aErr == nil && hadPrev {
		if _, rbErr := r.Rollback(ctx, id, agentID, prev.RevisionID, scope, agentcfg.SetOptions{}); rbErr != nil {
			return agentcfg.Revision{}, rbErr
		}
	}
	return agentcfg.Revision{}, r.err
}

// blindAfterWriteRegistry lands the write, reports it failed, and then REFUSES
// the read the compensation uses to scope itself — a store sick enough to fail
// a write is sick enough to fail the read after it. It is the unknown-answer
// arm, and the only one where this door's chosen fallback is observable.
type blindAfterWriteRegistry struct {
	agentcfg.Registry
	err   error
	mu    sync.Mutex
	blind bool
}

func (r *blindAfterWriteRegistry) SetRevision(ctx context.Context, id identity.Quadruple, agentID string, scope agentcfg.ConfigScope, payload agentcfg.ConfigPayload, opts agentcfg.SetOptions) (agentcfg.Revision, error) {
	rev, err := r.Registry.SetRevision(ctx, id, agentID, scope, payload, opts)
	if err != nil {
		return rev, err
	}
	r.mu.Lock()
	r.blind = true
	r.mu.Unlock()
	return agentcfg.Revision{}, r.err
}

func (r *blindAfterWriteRegistry) Active(ctx context.Context, id identity.Quadruple, agentID string, scope agentcfg.ConfigScope) (agentcfg.Revision, bool, error) {
	r.mu.Lock()
	blind := r.blind
	r.mu.Unlock()
	if blind {
		return agentcfg.Revision{}, false, errors.New("state store unavailable: read active pointer")
	}
	return r.Registry.Active(ctx, id, agentID, scope)
}

// TestAddMCPConnection_WriteLandedThenErrored_DoesNotDetach settles the
// commit-then-error question for this door BY EXECUTION.
//
// The write lands completely — the pointer moves and the new revision declares
// the connection — and the store then reports it failed. The pre-fix
// compensation detached on any error, which tore down a server the landed
// write had just declared: the config named a connection nothing served.
//
// The declared-ness re-read closes it with no second mechanism, because the
// pointer IS the answer to "did the write land?".
//
// Mutation: detach unconditionally and this fails on the live assertion while
// the declared assertion stays green — the config-names-an-absent-server shape.
func TestAddMCPConnection_WriteLandedThenErrored_DoesNotDetach(t *testing.T) {
	ctx := context.Background()
	boom := errors.New("state store unavailable: save active pointer: injected")
	h := newCompHarnessWrapped(t, nil, true, func(r agentcfg.Registry) agentcfg.Registry {
		return &landedThenErroredRegistry{Registry: r, err: boom}
	})

	_, addErr := h.svc.AddMCPConnection(ctx, addReq(httpConnNamed("github"), nil))
	if !errors.Is(addErr, boom) {
		t.Fatalf("add = %v, want the store's reported failure", addErr)
	}

	t.Logf("CASE 1 (pointer MOVED, store reported failure): activeRevisionNames(github)=%v liveRegistryHas(github)=%v detaches=%d",
		activeDeclaresConn(t, h, "github"), h.live.isLive("github"), h.live.detaches())

	// The premise: the write LANDED despite the error.
	if !activeDeclaresConn(t, h, "github") {
		t.Fatal("the write did not land — this row no longer models commit-then-error")
	}
	// The property.
	if h.live.detaches() != 0 {
		t.Fatalf("the compensation ran %d detaches against a write that LANDED, want 0 — the active revision declares this connection", h.live.detaches())
	}
	if !h.live.isLive("github") {
		t.Fatal("COLLATERAL: a reported-failed write that LANDED had its server torn down — the config now names a connection nothing serves")
	}
}

// TestAddMCPConnection_RecordLandedPointerStuck_DetachesAndIsCorrect is the
// PARTIAL landing, and it pins that detaching there is right rather than the
// same defect wearing a different hat.
//
// The revision record is durably written but the pointer never moves, so the
// ACTIVE config does not declare the connection. The pointer is the source of
// truth — an unreferenced revision is invisible to Active and to the run-start
// projection — so there is nothing declared to protect, and a live server no
// active revision names is exactly the leak the compensation exists to close.
// Detaching is the correct answer, not a regression.
func TestAddMCPConnection_RecordLandedPointerStuck_DetachesAndIsCorrect(t *testing.T) {
	ctx := context.Background()
	boom := errors.New("state store unavailable: save active pointer: injected")

	var stuck *recordLandedPointerStuckRegistry
	h := newCompHarnessWrapped(t, nil, true, func(r agentcfg.Registry) agentcfg.Registry {
		stuck = &recordLandedPointerStuckRegistry{Registry: r}
		return stuck
	})

	// Seed a base revision so there is a pointer position to be stuck AT.
	if _, err := h.svc.AddMCPConnection(ctx, addReq(httpConnNamed("base"), nil)); err != nil {
		t.Fatalf("seed add: %v", err)
	}
	stuck.err = boom

	_, addErr := h.svc.AddMCPConnection(ctx, addReq(httpConnNamed("github"), nil))
	if !errors.Is(addErr, boom) {
		t.Fatalf("add = %v, want the store's reported failure", addErr)
	}

	t.Logf("CASE 2 (record landed, pointer STUCK): activeRevisionNames(github)=%v liveRegistryHas(github)=%v detaches=%d",
		activeDeclaresConn(t, h, "github"), h.live.isLive("github"), h.live.detaches())

	// The premise: the pointer did NOT move to the new revision.
	if activeDeclaresConn(t, h, "github") {
		t.Fatal("the pointer moved — this row no longer models the partial landing")
	}
	// The property: nothing declares it, so the live server is the leak.
	if h.live.detaches() != 1 {
		t.Fatalf("the compensation ran %d detaches against a connection NO active revision names, want 1 — that is the leak it exists to close", h.live.detaches())
	}
	if h.live.isLive("github") {
		t.Fatal("a live server no active revision names survived — remove_mcp_connection answers not-found for it")
	}
	// And the seeded connection is untouched, in BOTH planes: the compensation
	// is scoped to the name this call attached, not to the agent.
	if !h.live.isLive("base") {
		t.Fatal("the compensation tore down a DIFFERENT connection")
	}
	if !activeDeclaresConn(t, h, "base") {
		t.Fatal("the refused add removed a SIBLING connection from the active revision")
	}
}

// TestAddMCPConnection_WriteLandedButPointerUnreadable_FallsBackLoudly is the
// UNKNOWN-answer arm, and the one place this door still tears down a landed
// write. It is pinned rather than left as a maybe.
//
// The write lands, the store reports it failed, and the re-read that would
// scope the compensation is itself refused. "The pointer does not name it" and
// "I cannot tell" are then indistinguishable, so the door falls back to the
// unrefined compensation and says so at ERROR.
//
// The fallback direction is deliberately the OPPOSITE of the driver's, and the
// asymmetry is the point: down there an unknown-answer delete strands a
// dangling pointer no later write can repair, so it retains. Here BOTH
// outcomes self-heal at the next run-start reconcile — a declared-but-dark
// connection is re-attached by the attach pass, an undeclared-but-live one is
// torn down by the detach pass — so the door keeps the guarantee it was given
// and reports the residual instead of guessing.
func TestAddMCPConnection_WriteLandedButPointerUnreadable_FallsBackLoudly(t *testing.T) {
	ctx := context.Background()
	boom := errors.New("state store unavailable: save active pointer: injected")
	h := newCompHarnessWrapped(t, nil, true, func(r agentcfg.Registry) agentcfg.Registry {
		return &blindAfterWriteRegistry{Registry: r, err: boom}
	})

	_, addErr := h.svc.AddMCPConnection(ctx, addReq(httpConnNamed("github"), nil))
	if !errors.Is(addErr, boom) {
		t.Fatalf("add = %v, want the store's reported failure", addErr)
	}

	t.Logf("CASE 3 (pointer MOVED, re-read REFUSED): activeRevisionNames(github)=%v liveRegistryHas(github)=%v detaches=%d",
		activeDeclaresConn(t, h, "github"), h.live.isLive("github"), h.live.detaches())

	// The premise: the write LANDED (read through the undecorated registry).
	if !activeDeclaresConn(t, h, "github") {
		t.Fatal("the write did not land — this row no longer models the unknown answer over a landed write")
	}
	// The documented fallback: unconditional compensation, loud.
	if h.live.detaches() != 1 {
		t.Fatalf("the unknown-answer arm ran %d detaches, want 1 — the fallback is the unrefined compensation, stated in the godoc and reported at ERROR", h.live.detaches())
	}
	if h.live.isLive("github") {
		t.Fatal("the unknown-answer arm neither scoped nor compensated — it must do one or the other, never neither")
	}
}

// TestAddMCPConnection_CompensatingDetach_IsOwnerScoped is the owner guard the
// seven tests above could not carry.
//
// The original double ignored the tenant and agent it was handed
// (`DetachConnection(_, _, _, name)`), so mutating the production call to pass
// a WRONG tenant left all seven compensation tests GREEN. In production that
// mutation is a SILENT NO-OP: `Registry.Deregister` answers a foreign owner
// with ErrServerNotFound and `detachSource` swallows it as idempotent — which
// restores invisibly the exact leak the compensation exists to close.
//
// The double now models that owner scoping STRUCTURALLY (a foreign-owner
// detach records the call, returns nil, and removes NOTHING, exactly as
// production does), so the mutation turns every compensation test red on its
// own liveness assertion. This test adds the precise diagnosis on top: the
// owner the detach was called with must be the owner the attach was stamped
// with.
func TestAddMCPConnection_CompensatingDetach_IsOwnerScoped(t *testing.T) {
	ctx := context.Background()
	h := newCompHarness(t, nil, true)

	stale := staleTokenFor(t, h.reg, agentcfg.ConfigScopeAgent)
	req := addReq(httpConnNamed("scoped"), nil)
	req.ExpectedContentHash = stale
	if _, addErr := h.svc.AddMCPConnection(ctx, req); !errors.Is(addErr, agentcfg.ErrRevisionConflict) {
		t.Fatalf("add with a stale token = %v, want ErrRevisionConflict", addErr)
	}

	want := liveOwner{tenant: scope().Tenant, agent: testAgentID}
	got := h.live.detachOwners()
	if len(got) != 1 {
		t.Fatalf("compensating detach called %d times, want exactly 1", len(got))
	}
	if got[0] != want {
		t.Fatalf("compensating detach owner = %+v, want %+v — a foreign owner is a SILENT no-op in production (Deregister answers ErrServerNotFound, detachSource swallows it as idempotent), so the leak returns invisibly", got[0], want)
	}
	if attached := h.attacher.attachOwner(t); got[0] != attached {
		t.Fatalf("compensating detach owner %+v != the owner the ATTACH stamped %+v — attach and compensating detach must address the same registration", got[0], attached)
	}
	if h.live.isLive("scoped") {
		t.Fatal("the compensating detach did not remove the registration — it addressed a different owner")
	}
}
