package protocol_test

import (
	"context"
	"encoding/json"
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

const (
	secretHeaderKey = "Authorization"
	secretHeaderVal = "Bearer super-secret-token-value-9f2c"
	allowedStdioBin = "/usr/local/bin/harbor-mcptest-stdio"
)

// fakeAttacher is a deterministic ConnectionAttacher: it records each
// AttachRequest verbatim (so a test can assert the secret headers DID flow to
// the live attach) and returns a scripted outcome (nil=online, an
// ErrAuthRequired-wrapped error, or a generic failure).
type fakeAttacher struct {
	mu     sync.Mutex
	calls  []agentcfgprotocol.AttachRequest
	result error
}

func (f *fakeAttacher) Attach(_ context.Context, req agentcfgprotocol.AttachRequest) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, req)
	return f.result
}

func (f *fakeAttacher) lastCall(t *testing.T) agentcfgprotocol.AttachRequest {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		t.Fatal("attacher was never called")
	}
	return f.calls[len(f.calls)-1]
}

// addHarness wires a Service with a fake attacher + a real coordinator + a
// shared bus + a real StateStore-backed registry, with a stdio allowlist that
// permits allowedStdioBin.
type addHarness struct {
	svc      *agentcfgprotocol.Service
	bus      events.EventBus
	reg      agentcfg.Registry
	coord    pauseresume.Coordinator
	attacher *fakeAttacher
}

func newAddHarness(t *testing.T, attachResult error) *addHarness {
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
	coord := pauseresume.New(pauseresume.WithBus(bus))
	attacher := &fakeAttacher{result: attachResult}
	svcOpts := []agentcfgprotocol.Option{
		agentcfgprotocol.WithBus(bus),
		agentcfgprotocol.WithConnectionAttacher(attacher),
		agentcfgprotocol.WithCoordinator(coord),
		agentcfgprotocol.WithStdioAllowlist([]string{allowedStdioBin}),
	}
	s, err := agentcfgprotocol.NewService(reg, svcOpts...)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	t.Cleanup(func() { _ = reg.Close(context.Background()); _ = bus.Close(context.Background()) })
	return &addHarness{svc: s, bus: bus, reg: reg, coord: coord, attacher: attacher}
}

func stdioConn() prototypes.AgentConfigMCPConnectionDescriptor {
	return prototypes.AgentConfigMCPConnectionDescriptor{
		Name:      "mcptest",
		Transport: "stdio",
		Command:   []string{allowedStdioBin, "--flag"},
	}
}

func addReq(conn prototypes.AgentConfigMCPConnectionDescriptor, headers map[string]string) prototypes.AgentConfigAddMCPConnectionRequest {
	return prototypes.AgentConfigAddMCPConnectionRequest{
		Identity: scope(), AgentID: testAgentID, Connection: conn, Headers: headers,
	}
}

// TestAddMCPConnection_OnlineRecordsRevision_SecretHygiene proves a successful
// add records the NON-SECRET descriptor as a revision, the secret header flows
// to the live attach, and the secret appears NOWHERE in the persisted revision
// payload, the diff, or any emitted event (the load-bearing §7 invariant).
func TestAddMCPConnection_OnlineRecordsRevision_SecretHygiene(t *testing.T) {
	ctx := context.Background()
	h := newAddHarness(t, nil)

	sub, err := h.bus.Subscribe(ctx, events.Filter{Admin: true, Types: []events.EventType{
		agentcfg.EventTypeMCPConnectionPending, agentcfg.EventTypeMCPConnectionAdded,
		agentcfg.EventTypeConfigRevised,
	}})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	collected := make(chan events.Event, 32)
	go func() {
		for ev := range sub.Events() {
			collected <- ev
		}
	}()

	resp, err := h.svc.AddMCPConnection(ctx, addReq(stdioConn(), map[string]string{secretHeaderKey: secretHeaderVal}))
	if err != nil {
		t.Fatalf("AddMCPConnection: %v", err)
	}
	if resp.State != string(agentcfgprotocol.ConnectionStateOnline) {
		t.Fatalf("state = %q, want online", resp.State)
	}
	if resp.Revision == nil {
		t.Fatal("online add recorded no revision")
	}
	if resp.Connection.Name != "mcptest" || resp.Connection.Transport != "stdio" {
		t.Errorf("descriptor mismatch: %+v", resp.Connection)
	}

	// The secret header MUST have flowed to the live attach.
	call := h.attacher.lastCall(t)
	if call.Headers[secretHeaderKey] != secretHeaderVal {
		t.Errorf("attach did not receive the secret header: %+v", call.Headers)
	}

	// Secret hygiene #1: the persisted revision payload carries no secret.
	active, set, err := h.reg.Active(ctx, qScope(), testAgentID)
	if err != nil || !set {
		t.Fatalf("Active: %v set=%v", err, set)
	}
	payloadBytes, _ := json.Marshal(active.Payload)
	assertNoSecret(t, "persisted revision payload", string(payloadBytes))
	// The descriptor IS present (non-secret).
	if len(active.Payload.ConnectionDescriptors()) != 1 || active.Payload.ConnectionDescriptors()[0].Name != "mcptest" {
		t.Fatalf("connection not recorded in revision: %+v", active.Payload.ConnectionDescriptors())
	}

	// Secret hygiene #2: a fresh second add → diff carries no secret.
	conn2 := stdioConn()
	conn2.Name = "mcptest2"
	resp2, err := h.svc.AddMCPConnection(ctx, addReq(conn2, map[string]string{secretHeaderKey: secretHeaderVal}))
	if err != nil {
		t.Fatalf("second add: %v", err)
	}
	diff, err := h.svc.Diff(ctx, prototypes.AgentConfigDiffRequest{
		Identity: scope(), AgentID: testAgentID,
		FromRevision: resp.Revision.RevisionID, ToRevision: resp2.Revision.RevisionID,
	})
	if err != nil {
		t.Fatalf("Diff: %v", err)
	}
	diffBytes, _ := json.Marshal(diff)
	assertNoSecret(t, "diff", string(diffBytes))
	if len(diff.Diff.Connections.Added) != 1 || diff.Diff.Connections.Added[0] != "mcptest2" {
		t.Errorf("connection diff wrong: %+v", diff.Diff.Connections)
	}

	// Secret hygiene #3: no emitted event carries the secret; the added +
	// pending lifecycle events fired.
	deadline := time.After(2 * time.Second)
	var sawPending, sawAdded bool
	for !sawPending || !sawAdded {
		select {
		case ev := <-collected:
			b, _ := json.Marshal(ev.Payload)
			assertNoSecret(t, "event "+string(ev.Type), string(b))
			switch ev.Type {
			case agentcfg.EventTypeMCPConnectionPending:
				sawPending = true
			case agentcfg.EventTypeMCPConnectionAdded:
				sawAdded = true
			}
		case <-deadline:
			t.Fatalf("did not observe pending+added events (pending=%v added=%v)", sawPending, sawAdded)
		}
	}
}

// TestAddMCPConnection_SectionMergeBothDirections proves an add REPLACES only
// the connections section (preserving skills + tool-exposure + prompt-layers),
// and a subsequent non-connection edit preserves the connection.
func TestAddMCPConnection_SectionMergeBothDirections(t *testing.T) {
	ctx := context.Background()
	h := newAddHarness(t, nil)

	// Seed all three sibling sections.
	if _, err := h.svc.SetRevision(ctx, prototypes.AgentConfigSetRevisionRequest{
		Identity: scope(), AgentID: testAgentID,
		Payload: prototypes.AgentConfigPayload{
			Skills:       &prototypes.AgentConfigSkillsSelection{Names: []string{"skA"}},
			ToolExposure: &prototypes.AgentConfigToolExposure{PausedServers: []string{"srvP"}},
			PromptLayers: &prototypes.AgentConfigPromptLayers{Base: ptr("base prompt")},
		},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Direction 1: add a connection — siblings preserved.
	if _, err := h.svc.AddMCPConnection(ctx, addReq(stdioConn(), nil)); err != nil {
		t.Fatalf("add: %v", err)
	}
	active, _, _ := h.reg.Active(ctx, qScope(), testAgentID)
	if len(active.Payload.SkillNames()) != 1 || active.Payload.SkillNames()[0] != "skA" {
		t.Errorf("skills not preserved by add: %+v", active.Payload.SkillNames())
	}
	if len(active.Payload.PausedServers()) != 1 || active.Payload.PausedServers()[0] != "srvP" {
		t.Errorf("tool-exposure not preserved by add: %+v", active.Payload.PausedServers())
	}
	if b, ok := active.Payload.BasePrompt(); !ok || b != "base prompt" {
		t.Errorf("prompt layer not preserved by add: %q ok=%v", b, ok)
	}
	if len(active.Payload.ConnectionDescriptors()) != 1 {
		t.Errorf("connection not recorded: %+v", active.Payload.ConnectionDescriptors())
	}

	// Direction 2: a skills edit preserves the connection.
	if _, err := h.svc.SetToolExposure(ctx, prototypes.AgentConfigSetToolExposureRequest{
		Identity: scope(), AgentID: testAgentID,
		ToolExposure: prototypes.AgentConfigToolExposure{PausedServers: []string{"srvP", "srvQ"}},
	}); err != nil {
		t.Fatalf("tool-exposure edit: %v", err)
	}
	active2, _, _ := h.reg.Active(ctx, qScope(), testAgentID)
	if len(active2.Payload.ConnectionDescriptors()) != 1 {
		t.Errorf("connection dropped by a tool-exposure edit: %+v", active2.Payload.ConnectionDescriptors())
	}
}

// TestAddMCPConnection_FailedDial_LoudEvent_NoRevision proves a dial/handshake
// failure records NO revision, registers NO server, and emits a loud failed
// event — never a silent drop.
func TestAddMCPConnection_FailedDial_LoudEvent_NoRevision(t *testing.T) {
	ctx := context.Background()
	h := newAddHarness(t, errors.New("provider.Connect: dial tcp: connection refused"))

	sub, err := h.bus.Subscribe(ctx, events.Filter{Admin: true, Types: []events.EventType{agentcfg.EventTypeMCPConnectionFailed}})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	failed := make(chan events.Event, 4)
	go func() {
		for ev := range sub.Events() {
			failed <- ev
		}
	}()

	resp, err := h.svc.AddMCPConnection(ctx, addReq(stdioConn(), nil))
	if err != nil {
		t.Fatalf("AddMCPConnection returned a transport error (should record failed state instead): %v", err)
	}
	if resp.State != string(agentcfgprotocol.ConnectionStateFailed) {
		t.Fatalf("state = %q, want failed", resp.State)
	}
	if resp.Reason == "" {
		t.Error("failed add carried no reason (silent drop)")
	}
	if resp.Revision != nil {
		t.Error("failed add recorded a revision (broken connection must not be desired state)")
	}
	// No active revision recorded.
	if _, set, _ := h.reg.Active(ctx, qScope(), testAgentID); set {
		t.Error("failed add left an active revision")
	}
	select {
	case ev := <-failed:
		if ev.Type != agentcfg.EventTypeMCPConnectionFailed {
			t.Errorf("event type = %q", ev.Type)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no failed event emitted (silent drop)")
	}
}

// TestAddMCPConnection_StdioAllowlist_FailClosed proves the stdio gate is
// fail-closed: an empty allowlist rejects every stdio add, and a command
// absent from the allowlist is rejected (argv-form is matched on argv[0]).
func TestAddMCPConnection_StdioAllowlist_FailClosed(t *testing.T) {
	ctx := context.Background()

	// Bespoke fail-closed service with NO allowlist (the secure default).
	bus, err := eventsinmem.New(config.EventsConfig{
		Driver: "inmem", MaxSubscribersPerSession: 8, SubscriberBufferSize: 32,
		IdleTimeout: 30 * time.Second, DropWindow: time.Second,
	}, auditpatterns.New())
	if err != nil {
		t.Fatalf("bus: %v", err)
	}
	st, err := newStateStore(t)
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	reg, err := agentcfg.Open(ctx, agentcfg.Config{}, agentcfg.Deps{State: st, Bus: bus})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = reg.Close(ctx); _ = bus.Close(ctx) })
	attacher := &fakeAttacher{}
	s, err := agentcfgprotocol.NewService(reg,
		agentcfgprotocol.WithBus(bus),
		agentcfgprotocol.WithConnectionAttacher(attacher),
		agentcfgprotocol.WithCoordinator(pauseresume.New()),
		// NO WithStdioAllowlist → fail-closed.
	)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	if _, err := s.AddMCPConnection(ctx, addReq(stdioConn(), nil)); !errors.Is(err, agentcfgprotocol.ErrStdioNotAllowed) {
		t.Fatalf("empty allowlist: want ErrStdioNotAllowed, got %v", err)
	}
	// The attacher must NOT have been called (rejected before any dial).
	if len(attacher.calls) != 0 {
		t.Errorf("stdio gate fired AFTER the attach (attacher called %d times)", len(attacher.calls))
	}

	// A command NOT in the allowlist is rejected.
	h := newAddHarness(t, nil)
	badConn := stdioConn()
	badConn.Command = []string{"/bin/evil", "--pwn"}
	if _, err := h.svc.AddMCPConnection(ctx, addReq(badConn, nil)); !errors.Is(err, agentcfgprotocol.ErrStdioNotAllowed) {
		t.Fatalf("un-allowlisted command: want ErrStdioNotAllowed, got %v", err)
	}
}

// TestAddMCPConnection_HTTPNotGatedByStdioAllowlist proves an http add is not
// subject to the stdio allowlist gate (it is still admin-scoped at the wire).
func TestAddMCPConnection_HTTPNotGatedByStdioAllowlist(t *testing.T) {
	ctx := context.Background()
	// Bespoke service with NO stdio allowlist — an http add still works.
	h := newAddHarness(t, nil)
	httpConn := prototypes.AgentConfigMCPConnectionDescriptor{
		Name: "remote", Transport: "http", URL: "https://mcp.example.com/sse",
	}
	resp, err := h.svc.AddMCPConnection(ctx, addReq(httpConn, map[string]string{secretHeaderKey: secretHeaderVal}))
	if err != nil {
		t.Fatalf("http add: %v", err)
	}
	if resp.State != string(agentcfgprotocol.ConnectionStateOnline) {
		t.Fatalf("http add state = %q, want online", resp.State)
	}
}

// TestAddMCPConnection_AuthRequired_ParksOnPauseResume proves an auth-required
// attach parks on the unified pause/resume primitive (NOT a new mechanism),
// records a revision, and surfaces the pause token — with no secret in the
// pause payload.
func TestAddMCPConnection_AuthRequired_ParksOnPauseResume(t *testing.T) {
	ctx := context.Background()
	h := newAddHarness(t, agentcfgprotocol.ErrAuthRequired)

	resp, err := h.svc.AddMCPConnection(ctx, addReq(stdioConn(), map[string]string{secretHeaderKey: secretHeaderVal}))
	if err != nil {
		t.Fatalf("AddMCPConnection: %v", err)
	}
	if resp.State != string(agentcfgprotocol.ConnectionStateAuthRequired) {
		t.Fatalf("state = %q, want auth_required", resp.State)
	}
	if resp.PauseToken == "" {
		t.Fatal("auth_required add returned no pause token")
	}
	if resp.Revision == nil {
		t.Fatal("auth_required add recorded no revision")
	}
	// The pause is parked on the SAME coordinator.
	status, err := h.coord.Status(ctx, pauseresume.Token(resp.PauseToken))
	if err != nil {
		t.Fatalf("coordinator.Status: %v", err)
	}
	if status.State != pauseresume.StatusPaused {
		t.Errorf("pause state = %q, want paused", status.State)
	}
	if status.Reason != pauseresume.ReasonExternalEvent {
		t.Errorf("pause reason = %q, want external_event", status.Reason)
	}
}

// TestAddMCPConnection_AuthRequired_NoCoordinator_FailsLoud proves an
// auth-required attach with no coordinator wired fails loud (never silently
// drops the auth requirement).
func TestAddMCPConnection_AuthRequired_NoCoordinator_FailsLoud(t *testing.T) {
	ctx := context.Background()
	bus, err := eventsinmem.New(config.EventsConfig{
		Driver: "inmem", MaxSubscribersPerSession: 8, SubscriberBufferSize: 32,
		IdleTimeout: 30 * time.Second, DropWindow: time.Second,
	}, auditpatterns.New())
	if err != nil {
		t.Fatalf("bus: %v", err)
	}
	st, err := newStateStore(t)
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	reg, err := agentcfg.Open(ctx, agentcfg.Config{}, agentcfg.Deps{State: st, Bus: bus})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = reg.Close(ctx); _ = bus.Close(ctx) })
	s, err := agentcfgprotocol.NewService(reg,
		agentcfgprotocol.WithBus(bus),
		agentcfgprotocol.WithConnectionAttacher(&fakeAttacher{result: agentcfgprotocol.ErrAuthRequired}),
		agentcfgprotocol.WithStdioAllowlist([]string{allowedStdioBin}),
		// NO coordinator.
	)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	if _, err := s.AddMCPConnection(ctx, addReq(stdioConn(), nil)); !errors.Is(err, agentcfgprotocol.ErrCoordinatorUnavailable) {
		t.Fatalf("want ErrCoordinatorUnavailable, got %v", err)
	}
}

// TestAddMCPConnection_AttacherUnavailable proves the method fails loud when
// no attacher is wired.
func TestAddMCPConnection_AttacherUnavailable(t *testing.T) {
	ctx := context.Background()
	s, err := agentcfgprotocol.NewService(newRegistry(t),
		agentcfgprotocol.WithStdioAllowlist([]string{allowedStdioBin}))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	if _, err := s.AddMCPConnection(ctx, addReq(stdioConn(), nil)); !errors.Is(err, agentcfgprotocol.ErrConnectionAttachUnavailable) {
		t.Fatalf("want ErrConnectionAttachUnavailable, got %v", err)
	}
}

// TestAddMCPConnection_Rejections covers identity + descriptor validation.
func TestAddMCPConnection_Rejections(t *testing.T) {
	ctx := context.Background()
	h := newAddHarness(t, nil)

	cases := []struct {
		name string
		req  prototypes.AgentConfigAddMCPConnectionRequest
		want error
	}{
		{"incomplete identity", prototypes.AgentConfigAddMCPConnectionRequest{
			Identity: prototypes.IdentityScope{Tenant: "t"}, AgentID: testAgentID, Connection: stdioConn(),
		}, agentcfgprotocol.ErrIdentityRequired},
		{"empty agent id", prototypes.AgentConfigAddMCPConnectionRequest{
			Identity: scope(), AgentID: "", Connection: stdioConn(),
		}, agentcfgprotocol.ErrIdentityRequired},
		{"empty name", addReq(prototypes.AgentConfigMCPConnectionDescriptor{Transport: "stdio", Command: []string{allowedStdioBin}}, nil), agentcfgprotocol.ErrInvalidConnection},
		{"unknown transport", addReq(prototypes.AgentConfigMCPConnectionDescriptor{Name: "x", Transport: "grpc"}, nil), agentcfgprotocol.ErrInvalidConnection},
		{"stdio without command", addReq(prototypes.AgentConfigMCPConnectionDescriptor{Name: "x", Transport: "stdio"}, nil), agentcfgprotocol.ErrInvalidConnection},
		{"http without url", addReq(prototypes.AgentConfigMCPConnectionDescriptor{Name: "x", Transport: "http"}, nil), agentcfgprotocol.ErrInvalidConnection},
		{"stdio with url", addReq(prototypes.AgentConfigMCPConnectionDescriptor{Name: "x", Transport: "stdio", Command: []string{allowedStdioBin}, URL: "https://x"}, nil), agentcfgprotocol.ErrInvalidConnection},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := h.svc.AddMCPConnection(ctx, tc.req); !errors.Is(err, tc.want) {
				t.Fatalf("want %v, got %v", tc.want, err)
			}
		})
	}
}

// qScope returns the identity Quadruple matching scope().
func qScope() identity.Quadruple {
	return identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}}
}

func ptr(s string) *string { return &s }

func assertNoSecret(t *testing.T, where, blob string) {
	t.Helper()
	if strings.Contains(blob, secretHeaderVal) {
		t.Fatalf("SECRET LEAK in %s: %q contains the secret header value", where, blob)
	}
	if strings.Contains(strings.ToLower(blob), "bearer ") {
		t.Fatalf("SECRET LEAK in %s: %q contains a bearer token", where, blob)
	}
}

// TestAddMCPConnection_FailedDial_ScrubsSecretsInReason proves the failure
// reason is secret-scrubbed before it reaches the response, the lifecycle
// event, or the bus/logs: a transport error that echoes a credential-bearing
// URL or an inline bearer token must surface with the secret redacted
// (CLAUDE.md §7 defence-in-depth — no secret on an event payload, even via an
// error string).
func TestAddMCPConnection_FailedDial_ScrubsSecretsInReason(t *testing.T) {
	ctx := context.Background()
	const urlSecret = "sup3r-s3cret-pw"
	const bearerSecret = "tok_ABC123XYZ"
	dialErr := errors.New("provider.Connect: get \"https://admin:" + urlSecret + "@mcp.example.com/sse\": dial failed; Authorization: Bearer " + bearerSecret)
	h := newAddHarness(t, dialErr)

	sub, err := h.bus.Subscribe(ctx, events.Filter{Admin: true, Types: []events.EventType{agentcfg.EventTypeMCPConnectionFailed}})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	failed := make(chan events.Event, 4)
	go func() {
		for ev := range sub.Events() {
			failed <- ev
		}
	}()

	resp, err := h.svc.AddMCPConnection(ctx, addReq(stdioConn(), nil))
	if err != nil {
		t.Fatalf("AddMCPConnection transport error: %v", err)
	}
	if resp.State != string(agentcfgprotocol.ConnectionStateFailed) {
		t.Fatalf("state = %q, want failed", resp.State)
	}
	// The response reason must carry NEITHER secret.
	for _, secret := range []string{urlSecret, bearerSecret} {
		if strings.Contains(resp.Reason, secret) {
			t.Fatalf("SECRET LEAK in response reason: %q contains %q", resp.Reason, secret)
		}
	}
	// And neither must the emitted event payload.
	select {
	case ev := <-failed:
		b, _ := json.Marshal(ev.Payload)
		for _, secret := range []string{urlSecret, bearerSecret} {
			if strings.Contains(string(b), secret) {
				t.Fatalf("SECRET LEAK in failed event payload: %q contains %q", string(b), secret)
			}
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no failed event emitted")
	}
}
