package protocol_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/identity"
	agentcfgprotocol "github.com/hurtener/Harbor/internal/runtime/agentcfg/protocol"
)

type recordingPrepared struct {
	mu          sync.Mutex
	activations int
	closes      int
	activateErr error
	closeErr    error
}

func (p *recordingPrepared) Activate(context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.activations++
	return p.activateErr
}

func (p *recordingPrepared) Close(context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closes++
	return p.closeErr
}

func (p *recordingPrepared) counts() (int, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.activations, p.closes
}

type recordingPreparer struct {
	prepared *recordingPrepared
	calls    int
}

type providerVisibilityPreparer struct {
	prepared               *recordingPrepared
	installer              *fakeInstaller
	publishedDuringPrepare bool
}

func (p *providerVisibilityPreparer) PrepareConnection(_ context.Context, req agentcfgprotocol.AttachRequest) (agentcfgprotocol.PreparedConnection, error) {
	p.publishedDuringPrepare = p.installer.has(req.OAuthProvider)
	return p.prepared, nil
}

type siblingDescriptorThenErrorRegistry struct {
	agentcfg.Registry
	err error
}

func (r *siblingDescriptorThenErrorRegistry) SetRevision(ctx context.Context, id identity.Quadruple, agentID string, scope agentcfg.ConfigScope, _ agentcfg.ConfigPayload, opts agentcfg.SetOptions) (agentcfg.Revision, error) {
	sibling := agentcfg.MCPConnectionDescriptor{Name: "github", Transport: agentcfg.MCPTransportHTTP, URL: "https://sibling.invalid/mcp"}
	rev, err := r.Registry.SetRevision(ctx, id, agentID, scope, agentcfg.ConfigPayload{Connections: &agentcfg.ConnectionsSection{Servers: []agentcfg.MCPConnectionDescriptor{sibling}}}, opts)
	if err != nil {
		return rev, err
	}
	return rev, r.err
}

func (p *recordingPreparer) PrepareConnection(context.Context, agentcfgprotocol.AttachRequest) (agentcfgprotocol.PreparedConnection, error) {
	p.calls++
	return p.prepared, nil
}

func preparedService(t *testing.T, reg agentcfg.Registry, p *recordingPreparer) *agentcfgprotocol.Service {
	t.Helper()
	svc, err := agentcfgprotocol.NewService(reg, agentcfgprotocol.WithConnectionPreparer(p))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return svc
}

func TestAddMCPConnection_PreparedRefusalNeverActivates(t *testing.T) {
	boom := errors.New("injected revision failure")
	p := &recordingPreparer{prepared: &recordingPrepared{}}
	svc := preparedService(t, setFailureRegistry{Registry: newRegistry(t), err: boom}, p)
	if _, err := svc.AddMCPConnection(context.Background(), addReq(httpConnNamed("github"), nil)); !errors.Is(err, boom) {
		t.Fatalf("add = %v, want injected write error", err)
	}
	if activated, closed := p.prepared.counts(); activated != 0 || closed != 1 {
		t.Fatalf("prepared lifecycle after refusal: activate=%d close=%d, want 0/1", activated, closed)
	}
}

func TestAddMCPConnection_PreparedUnknownPointerClosesWithoutPublication(t *testing.T) {
	boom := errors.New("reported pointer failure")
	base := newRegistry(t)
	p := &recordingPreparer{prepared: &recordingPrepared{}}
	svc := preparedService(t, &blindAfterWriteRegistry{Registry: base, err: boom}, p)
	if _, err := svc.AddMCPConnection(context.Background(), addReq(httpConnNamed("github"), nil)); !errors.Is(err, boom) {
		t.Fatalf("add = %v, want reported write error", err)
	}
	if activated, closed := p.prepared.counts(); activated != 0 || closed != 1 {
		t.Fatalf("unknown pointer outcome published: activate=%d close=%d", activated, closed)
	}
}

func TestAddMCPConnection_PreparedConfirmedLandingConvergesThenReturnsStoreError(t *testing.T) {
	boom := errors.New("commit acknowledgement lost")
	base := newRegistry(t)
	p := &recordingPreparer{prepared: &recordingPrepared{}}
	svc := preparedService(t, &landedThenErroredRegistry{Registry: base, err: boom}, p)
	if _, err := svc.AddMCPConnection(context.Background(), addReq(httpConnNamed("github"), nil)); !errors.Is(err, boom) {
		t.Fatalf("add = %v, want original store error", err)
	}
	if activated, closed := p.prepared.counts(); activated != 1 || closed != 0 {
		t.Fatalf("confirmed landing did not converge: activate=%d close=%d", activated, closed)
	}
	active, set, err := base.Active(context.Background(), condQuad(), testAgentID, agentcfg.ConfigScopeAgent)
	if err != nil || !set || len(active.Payload.ConnectionDescriptors()) != 1 {
		t.Fatalf("landed revision missing: set=%v err=%v payload=%+v", set, err, active.Payload)
	}
}

func TestAddMCPConnection_SameNameDifferentDescriptorNeverActivates(t *testing.T) {
	boom := errors.New("sibling won while acknowledgement was lost")
	base := newRegistry(t)
	p := &recordingPreparer{prepared: &recordingPrepared{}}
	svc := preparedService(t, &siblingDescriptorThenErrorRegistry{Registry: base, err: boom}, p)
	if _, err := svc.AddMCPConnection(context.Background(), addReq(httpConnNamed("github"), nil)); !errors.Is(err, boom) {
		t.Fatalf("add = %v, want original store error", err)
	}
	if activated, closed := p.prepared.counts(); activated != 0 || closed != 1 {
		t.Fatalf("same-name sibling authorized the wrong prepared transport: activate=%d close=%d", activated, closed)
	}
	active, set, err := base.Active(context.Background(), condQuad(), testAgentID, agentcfg.ConfigScopeAgent)
	if err != nil || !set {
		t.Fatalf("winning sibling state missing: set=%v err=%v", set, err)
	}
	got := active.Payload.ConnectionDescriptors()
	if len(got) != 1 || got[0].URL != "https://sibling.invalid/mcp" {
		t.Fatalf("winning sibling descriptor changed: %+v", got)
	}
}

func TestAddMCPConnection_ActivationFailureRollsBackDesiredState(t *testing.T) {
	boom := errors.New("activation failed")
	reg := newRegistry(t)
	p := &recordingPreparer{prepared: &recordingPrepared{activateErr: boom}}
	svc := preparedService(t, reg, p)
	if _, err := svc.AddMCPConnection(context.Background(), addReq(httpConnNamed("github"), nil)); !errors.Is(err, boom) {
		t.Fatalf("add = %v, want activation error", err)
	}
	active, set, err := reg.Active(context.Background(), condQuad(), testAgentID, agentcfg.ConfigScopeAgent)
	if err != nil || !set {
		t.Fatalf("neutralized active state: set=%v err=%v", set, err)
	}
	if got := len(active.Payload.ConnectionDescriptors()); got != 0 {
		t.Fatalf("failed activation remained desired: %d descriptors", got)
	}
	if activated, closed := p.prepared.counts(); activated != 1 || closed != 1 {
		t.Fatalf("failed activation cleanup: activate=%d close=%d, want 1/1", activated, closed)
	}
}

func TestAddMCPConnection_AmbiguousWriteLandingThenActivationFailureRollsBackDesiredState(t *testing.T) {
	writeErr := errors.New("commit acknowledgement lost")
	activateErr := errors.New("activation failed")
	base := newRegistry(t)
	p := &recordingPreparer{prepared: &recordingPrepared{activateErr: activateErr}}
	svc := preparedService(t, &landedThenErroredRegistry{Registry: base, err: writeErr}, p)
	_, err := svc.AddMCPConnection(context.Background(), addReq(httpConnNamed("github"), nil))
	if !errors.Is(err, writeErr) || !errors.Is(err, activateErr) {
		t.Fatalf("add = %v, want joined write and activation errors", err)
	}
	active, set, err := base.Active(context.Background(), condQuad(), testAgentID, agentcfg.ConfigScopeAgent)
	if err != nil || !set {
		t.Fatalf("neutralized active state: set=%v err=%v", set, err)
	}
	if got := len(active.Payload.ConnectionDescriptors()); got != 0 {
		t.Fatalf("failed activation after ambiguous landing remained desired: %d descriptors", got)
	}
	if activated, closed := p.prepared.counts(); activated != 1 || closed != 1 {
		t.Fatalf("failed activation cleanup: activate=%d close=%d, want 1/1", activated, closed)
	}
}

func TestAddMCPConnection_InlineOAuthRemainsUnpublishedThroughMCPPrepare(t *testing.T) {
	reg := newRegistry(t)
	installer := newFakeInstaller()
	preparer := &providerVisibilityPreparer{prepared: &recordingPrepared{}, installer: installer}
	svc, err := agentcfgprotocol.NewService(reg,
		agentcfgprotocol.WithProviderInstaller(installer),
		agentcfgprotocol.WithConnectionPreparer(preparer),
		agentcfgprotocol.WithAllowWireOAuthDescriptor(true),
	)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	conn := httpConnNamed("github")
	conn.OAuth = descPtr(wireProviderDesc("github-oauth"))
	resp, err := svc.AddMCPConnection(context.Background(), addReq(conn, nil))
	if err != nil || resp.State != string(agentcfgprotocol.ConnectionStateOnline) {
		t.Fatalf("AddMCPConnection: state=%q err=%v", resp.State, err)
	}
	if preparer.publishedDuringPrepare {
		t.Fatal("inline OAuth provider entered the shared set before MCP preparation completed")
	}
	if !installer.has("github-oauth") {
		t.Fatal("inline OAuth provider was not published after desired state and MCP activation succeeded")
	}
}
