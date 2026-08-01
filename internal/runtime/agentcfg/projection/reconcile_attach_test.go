package projection_test

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sort"
	"sync"
	"testing"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/runtime/agentcfg/projection"
	"github.com/hurtener/Harbor/internal/tools/auth"
)

// reconcile_attach_test.go — unit coverage for the ATTACH pass of
// ReconcileConnections (phase 216, D-361): the declared-but-absent diff, the
// detach-before-attach ordering, owner scoping (cross-owner AND cross-tenant),
// the nil-reattacher backward-compatible path, the boot-declared skip, and the
// "one refusal does not strand the rest, and never fails the run" posture.

// mutableDetacher is a ConnectionDetacher whose attached view CHANGES as the
// reconcile mutates it — the shape the real registry has. Detach removes a name
// from the view; the paired reattacher adds one. This is what lets the test
// observe the pass ORDER (the attach pass re-reads the view after the detach
// pass) rather than asserting on a frozen snapshot.
type mutableDetacher struct {
	mu       sync.Mutex
	attached map[string]struct{}
	// calls records every AttachedSources / Detach / (paired) Reattach in order,
	// so the detach-before-attach ordering is asserted rather than assumed.
	calls []string
	// detachErr, when set, fails every Detach.
	detachErr error
}

func newMutableDetacher(attached ...string) *mutableDetacher {
	set := make(map[string]struct{}, len(attached))
	for _, a := range attached {
		set[a] = struct{}{}
	}
	return &mutableDetacher{attached: set}
}

func (m *mutableDetacher) AttachedSources(_ context.Context, _ auth.Owner) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, 0, len(m.attached))
	for a := range m.attached {
		out = append(out, a)
	}
	sort.Strings(out)
	m.calls = append(m.calls, "view:"+fmt.Sprint(out))
	return out
}

func (m *mutableDetacher) Detach(_ context.Context, source string, _ auth.Owner) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, "detach:"+source)
	if m.detachErr != nil {
		return m.detachErr
	}
	delete(m.attached, source)
	return nil
}

// markAttached is how the paired fakeReattacher reports a successful attach into
// the shared live view.
func (m *mutableDetacher) markAttached(name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, "attach:"+name)
	m.attached[name] = struct{}{}
}

func (m *mutableDetacher) isAttached(name string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.attached[name]
	return ok
}

func (m *mutableDetacher) callLog() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.calls...)
}

// fakeReattacher records every Reattach and, on success, reports the name into
// the shared live view so the pair behaves like the real registry.
type fakeReattacher struct {
	live *mutableDetacher
	mu   sync.Mutex
	// seen records (name → count) so a double-attach is visible.
	seen map[string]int
	// owners / ids record what the pass threaded through, so owner scoping and
	// identity propagation are asserted on the real arguments.
	owners []auth.Owner
	ids    []identity.Quadruple
	// descs records the DESCRIPTOR each call carried — the attach pass must hand
	// over the full declared descriptor, not just a name.
	descs []agentcfg.MCPConnectionDescriptor
	// failFor names connections whose Reattach fails.
	failFor map[string]error
}

func newFakeReattacher(live *mutableDetacher) *fakeReattacher {
	return &fakeReattacher{live: live, seen: map[string]int{}, failFor: map[string]error{}}
}

func (f *fakeReattacher) Reattach(_ context.Context, owner auth.Owner, id identity.Quadruple, desc agentcfg.MCPConnectionDescriptor) (bool, error) {
	alreadyLive := f.live != nil && f.live.isAttached(desc.Name)
	f.mu.Lock()
	err := f.failFor[desc.Name]
	if !alreadyLive {
		f.seen[desc.Name]++
		f.owners = append(f.owners, owner)
		f.ids = append(f.ids, id)
		f.descs = append(f.descs, desc)
	}
	f.mu.Unlock()
	if err != nil {
		return false, err
	}
	if alreadyLive {
		return false, nil
	}
	if f.live != nil {
		f.live.markAttached(desc.Name)
	}
	return true, nil
}

func (f *fakeReattacher) attachedNames() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, 0, len(f.seen))
	for k := range f.seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func (f *fakeReattacher) count(name string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.seen[name]
}

func (f *fakeReattacher) ownersSeen() []auth.Owner {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]auth.Owner(nil), f.owners...)
}

func (f *fakeReattacher) idsSeen() []identity.Quadruple {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]identity.Quadruple(nil), f.ids...)
}

func (f *fakeReattacher) descsSeen() []agentcfg.MCPConnectionDescriptor {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]agentcfg.MCPConnectionDescriptor(nil), f.descs...)
}

// TestReconcileConnections_AttachesDeclaredButAbsent is the core leg: a
// connection the active revision DECLARES but the live owner view does NOT carry
// is re-established at run start.
func TestReconcileConnections_AttachesDeclaredButAbsent(t *testing.T) {
	ctx := context.Background()
	reg := newRegistry(t)
	seedConnections(t, reg, "back", "already-live")
	live := newMutableDetacher("already-live") // "back" is declared but absent.
	re := newFakeReattacher(live)

	detached, attached, err := projection.ReconcileConnections(ctx, reg, projAgent, projID(), live, re, nil)
	if err != nil {
		t.Fatalf("ReconcileConnections: %v", err)
	}
	if detached != 0 {
		t.Fatalf("detached = %d, want 0 (nothing undeclared)", detached)
	}
	if attached != 1 {
		t.Fatalf("attached = %d, want 1", attached)
	}
	if got := re.attachedNames(); len(got) != 1 || got[0] != "back" {
		t.Fatalf("reattached = %v, want [back] (an already-live connection must not be re-dialled)", got)
	}
	// The pass hands over the whole descriptor, not a bare name: the attach
	// lifecycle re-validates transport/url/bindings from it.
	descs := re.descsSeen()
	if len(descs) != 1 || descs[0].Transport != agentcfg.MCPTransportHTTP || descs[0].URL == "" {
		t.Fatalf("descriptor handed to Reattach = %+v, want the full declared descriptor", descs)
	}
}

// TestReconcileConnections_NilReattacherIsDetachOnly pins the
// backward-compatible path: with no attach concrete the leg behaves exactly as
// the shipped detach-only reconcile did — a declared-but-absent connection is
// left absent and nothing is attached.
func TestReconcileConnections_NilReattacherIsDetachOnly(t *testing.T) {
	ctx := context.Background()
	reg := newRegistry(t)
	seedConnections(t, reg, "declared-absent", "keep")
	live := newMutableDetacher("keep", "drop")

	detached, attached, err := projection.ReconcileConnections(ctx, reg, projAgent, projID(), live, nil, nil)
	if err != nil {
		t.Fatalf("ReconcileConnections: %v", err)
	}
	if detached != 1 {
		t.Fatalf("detached = %d, want 1", detached)
	}
	if attached != 0 {
		t.Fatalf("attached = %d, want 0 (nil reattacher must attach nothing)", attached)
	}
	// Exactly ONE view read: the detach-only path must not take the attach pass's
	// second (post-detach) read.
	views := 0
	for _, c := range live.callLog() {
		if len(c) >= 5 && c[:5] == "view:" {
			views++
		}
	}
	if views != 1 {
		t.Fatalf("AttachedSources called %d times on the nil-reattacher path, want 1 (byte-for-byte the detach-only behaviour)", views)
	}
}

// TestReconcileConnections_DetachRunsBeforeAttach pins the pass ORDER. A name
// dropped and re-declared across one revision transition must be torn down
// before the attach pass considers it, or the attach would meet its own stale
// live registration.
func TestReconcileConnections_DetachRunsBeforeAttach(t *testing.T) {
	ctx := context.Background()
	reg := newRegistry(t)
	// "shared" is attached AND declared; "gone" is attached but undeclared;
	// "fresh" is declared but absent.
	seedConnections(t, reg, "shared", "fresh")
	live := newMutableDetacher("shared", "gone")
	re := newFakeReattacher(live)

	detached, attached, err := projection.ReconcileConnections(ctx, reg, projAgent, projID(), live, re, nil)
	if err != nil {
		t.Fatalf("ReconcileConnections: %v", err)
	}
	if detached != 1 || attached != 1 {
		t.Fatalf("detached=%d attached=%d, want 1/1", detached, attached)
	}
	log := live.callLog()
	detachIdx, attachIdx := -1, -1
	for i, c := range log {
		if c == "detach:gone" {
			detachIdx = i
		}
		if c == "attach:fresh" {
			attachIdx = i
		}
	}
	if detachIdx < 0 || attachIdx < 0 {
		t.Fatalf("call log missing a leg: %v", log)
	}
	if detachIdx > attachIdx {
		t.Fatalf("detach ran AFTER attach (log=%v); detach must run first", log)
	}
	// The descriptor-aware Reattach owns the exact live comparison, so the
	// projection takes one owner-scoped snapshot for the detach pass.
	views := 0
	for _, c := range log {
		if len(c) >= 5 && c[:5] == "view:" {
			views++
		}
	}
	if views != 1 {
		t.Fatalf("AttachedSources called %d times, want 1", views)
	}
}

// TestReconcileConnections_AttachPassIsOwnerScoped asserts the owner boundary on
// the attach pass: the owner threaded to Reattach is the RECONCILING
// (tenant, agent), cross-owner within one tenant AND across tenants, and a
// boot-declared name is never a candidate.
func TestReconcileConnections_AttachPassIsOwnerScoped(t *testing.T) {
	ctx := context.Background()
	reg := newRegistry(t)
	seedConnections(t, reg, "owned", "yaml-boot-srv")
	live := newMutableDetacher()
	re := newFakeReattacher(live)

	if _, attached, err := projection.ReconcileConnections(ctx, reg, projAgent, projID(), live, re,
		map[string]struct{}{"yaml-boot-srv": {}}); err != nil || attached != 1 {
		t.Fatalf("reconcile: attached=%d err=%v, want 1,nil", attached, err)
	}
	if got := re.attachedNames(); len(got) != 1 || got[0] != "owned" {
		t.Fatalf("reattached = %v, want [owned] — a boot-declared name is never attached by the reconcile", got)
	}
	owners := re.ownersSeen()
	want := auth.Owner{Tenant: projTenant, Agent: projAgent}
	if len(owners) != 1 || owners[0] != want {
		t.Fatalf("owner threaded to Reattach = %+v, want %+v", owners, want)
	}
	// Identity propagation: the reconciling run's quadruple reaches the concrete
	// (it stamps the run on its lifecycle events).
	ids := re.idsSeen()
	if len(ids) != 1 || ids[0].TenantID != projTenant || ids[0].UserID != projUser || ids[0].SessionID != projSess {
		t.Fatalf("quadruple threaded to Reattach = %+v, want the reconciling run's triple", ids)
	}

	// A DIFFERENT tenant reconciling the same agent name gets its own owner tag —
	// never the first tenant's.
	otherTenant := identity.Quadruple{Identity: identity.Identity{
		TenantID: "tenant-other", UserID: projUser, SessionID: projSess,
	}}
	otherLive := newMutableDetacher()
	otherRe := newFakeReattacher(otherLive)
	if _, _, err := projection.ReconcileConnections(ctx, reg, projAgent, otherTenant, otherLive, otherRe, nil); err != nil {
		t.Fatalf("cross-tenant reconcile: %v", err)
	}
	// The other tenant has no revision for this agent, so it declares nothing and
	// attaches nothing — critically, it never attaches tenant-proj's "owned".
	if got := otherRe.attachedNames(); len(got) != 0 {
		t.Fatalf("cross-tenant sweep attached %v, want nothing (owner isolation)", got)
	}

	// A different AGENT under the same tenant likewise carries its own owner.
	otherAgentLive := newMutableDetacher()
	otherAgentRe := newFakeReattacher(otherAgentLive)
	seedConnectionsFor(t, reg, "agent-other", projID(), "other-owned")
	if _, _, err := projection.ReconcileConnections(ctx, reg, "agent-other", projID(), otherAgentLive, otherAgentRe, nil); err != nil {
		t.Fatalf("cross-agent reconcile: %v", err)
	}
	gotOwners := otherAgentRe.ownersSeen()
	wantOther := auth.Owner{Tenant: projTenant, Agent: "agent-other"}
	if len(gotOwners) != 1 || gotOwners[0] != wantOther {
		t.Fatalf("cross-agent owner = %+v, want %+v", gotOwners, wantOther)
	}
	if got := otherAgentRe.attachedNames(); len(got) != 1 || got[0] != "other-owned" {
		t.Fatalf("cross-agent sweep attached %v, want [other-owned] only", got)
	}
}

// seedConnectionsFor writes an active revision for an arbitrary agent id.
func seedConnectionsFor(t *testing.T, reg agentcfg.Registry, agentID string, id identity.Quadruple, names ...string) {
	t.Helper()
	servers := make([]agentcfg.MCPConnectionDescriptor, 0, len(names))
	for _, n := range names {
		servers = append(servers, agentcfg.MCPConnectionDescriptor{
			Name: n, Transport: agentcfg.MCPTransportHTTP, URL: "https://example.invalid/" + n,
		})
	}
	payload := agentcfg.ConfigPayload{Connections: &agentcfg.ConnectionsSection{Servers: servers}}
	if _, err := reg.SetRevision(context.Background(), id, agentID, agentcfg.ConfigScopeAgent, payload, agentcfg.SetOptions{}); err != nil {
		t.Fatalf("seed connections for %q: %v", agentID, err)
	}
}

// TestReconcileConnections_AttachErrorsAreJoinedNotFatal — one refusing server
// does not strand the rest, every error is marked so the caller can classify it,
// and the fail-loud read error still fails loud.
func TestReconcileConnections_AttachErrorsAreJoinedNotFatal(t *testing.T) {
	ctx := context.Background()
	reg := newRegistry(t)
	seedConnections(t, reg, "bad-1", "good", "bad-2")
	live := newMutableDetacher()
	re := newFakeReattacher(live)
	re.failFor["bad-1"] = errors.New("unreachable")
	re.failFor["bad-2"] = errors.New("refused")

	detached, attached, err := projection.ReconcileConnections(ctx, reg, projAgent, projID(), live, re, nil)
	if err == nil {
		t.Fatal("want a joined re-attach error")
	}
	if !errors.Is(err, projection.ErrReconcileReattach) {
		t.Fatalf("err = %v, want it to carry ErrReconcileReattach so the caller can classify it", err)
	}
	if errors.Is(err, projection.ErrReconcileRead) {
		t.Fatal("an attach failure must never masquerade as a fail-loud read error")
	}
	if detached != 0 || attached != 1 {
		t.Fatalf("detached=%d attached=%d, want 0/1 (the good one still landed)", detached, attached)
	}
	// All three were attempted — no early abort.
	if got := re.attachedNames(); len(got) != 3 {
		t.Fatalf("attempted = %v, want all three attempted despite two failures", got)
	}

	// The fail-loud read error still fails loud, and attaches nothing.
	freshRe := newFakeReattacher(nil)
	if _, _, rerr := projection.ReconcileConnections(ctx, errReg{}, projAgent, projID(),
		newMutableDetacher(), freshRe, nil); !errors.Is(rerr, projection.ErrReconcileRead) {
		t.Fatalf("read error = %v, want ErrReconcileRead", rerr)
	}
	if len(freshRe.attachedNames()) != 0 {
		t.Fatal("a read error must not attach anything (no silent fall-through)")
	}
}

// cancellingReattacher cancels the sweep's ctx from inside its FIRST call, so the
// test observes the pass honouring cancellation BETWEEN connections — the
// mechanism an overall sweep budget relies on. (A ctx cancelled before the call
// never reaches the attach pass at all: the active-revision read fails loud
// first, which is the shipped, correct behaviour.)
type cancellingReattacher struct {
	cancel context.CancelFunc
	mu     sync.Mutex
	seen   []string
}

func (c *cancellingReattacher) Reattach(_ context.Context, _ auth.Owner, _ identity.Quadruple, desc agentcfg.MCPConnectionDescriptor) (bool, error) {
	c.mu.Lock()
	c.seen = append(c.seen, desc.Name)
	first := len(c.seen) == 1
	c.mu.Unlock()
	if first {
		c.cancel()
	}
	return true, nil
}

func (c *cancellingReattacher) names() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.seen...)
}

// TestReconcileConnections_AttachPassHonoursCancellation pins the sweep bound: an
// exhausted sweep budget ends the pass between connections rather than dialling
// every declared connection, and the early end is REPORTED (never a silent short
// sweep).
func TestReconcileConnections_AttachPassHonoursCancellation(t *testing.T) {
	reg := newRegistry(t)
	// Declared in sorted order a, b, c: the pass reaches "a", the budget expires,
	// and "b"/"c" are never dialled.
	seedConnections(t, reg, "a", "b", "c")
	live := newMutableDetacher()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	re := &cancellingReattacher{cancel: cancel}

	_, attached, err := projection.ReconcileConnections(ctx, reg, projAgent, projID(), live, re, nil)
	if attached != 1 {
		t.Fatalf("attached = %d, want 1 (only the connection reached before the budget expired)", attached)
	}
	if !errors.Is(err, context.Canceled) || !errors.Is(err, projection.ErrReconcileReattach) {
		t.Fatalf("err = %v, want a reported ErrReconcileReattach + context.Canceled (never a silent short sweep)", err)
	}
	if got := re.names(); len(got) != 1 || got[0] != "a" {
		t.Fatalf("dialled = %v, want only [a] — the pass must stop at the budget, not run to completion", got)
	}
}

// TestReconcileConnections_ConcurrentAttach_NoCrossTalk is the D-025
// concurrent-reuse run over the bidirectional leg: N concurrent reconciles for
// TWO owners against ONE shared reattacher under -race. The per-owner declared
// sets must never bleed, and the goroutine baseline must return.
func TestReconcileConnections_ConcurrentAttach_NoCrossTalk(t *testing.T) {
	ctx := context.Background()
	reg := newRegistry(t)
	seedConnectionsFor(t, reg, "agent-a", projID(), "a-conn")
	seedConnectionsFor(t, reg, "agent-b", projID(), "b-conn")

	liveA := newMutableDetacher()
	liveB := newMutableDetacher()
	// ONE shared reattacher serving both owners — the cross-talk surface.
	shared := newFakeReattacher(nil)

	base := runtime.NumGoroutine()
	const N = 128
	var wg sync.WaitGroup
	wg.Add(2 * N)
	for range N {
		go func() {
			defer wg.Done()
			if _, _, err := projection.ReconcileConnections(ctx, reg, "agent-a", projID(), liveA, shared, nil); err != nil {
				t.Errorf("owner A reconcile: %v", err)
			}
		}()
		go func() {
			defer wg.Done()
			if _, _, err := projection.ReconcileConnections(ctx, reg, "agent-b", projID(), liveB, shared, nil); err != nil {
				t.Errorf("owner B reconcile: %v", err)
			}
		}()
	}
	wg.Wait()

	// Each owner only ever saw its OWN declared connection.
	if got := shared.attachedNames(); len(got) != 2 || got[0] != "a-conn" || got[1] != "b-conn" {
		t.Fatalf("attempted = %v, want exactly [a-conn b-conn] (no cross-owner bleed)", got)
	}
	if shared.count("a-conn") != N || shared.count("b-conn") != N {
		t.Fatalf("per-owner attempt counts = a:%d b:%d, want %d each", shared.count("a-conn"), shared.count("b-conn"), N)
	}
	// Every call carried its own owner tag — no shared mutable owner state.
	byOwner := map[auth.Owner]int{}
	for _, o := range shared.ownersSeen() {
		byOwner[o]++
	}
	if byOwner[auth.Owner{Tenant: projTenant, Agent: "agent-a"}] != N ||
		byOwner[auth.Owner{Tenant: projTenant, Agent: "agent-b"}] != N {
		t.Fatalf("owner tags = %v, want %d per owner", byOwner, N)
	}
	assertGoroutineBaseline(t, base)
}
