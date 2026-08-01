package protocol_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/identity"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	agentcfgprotocol "github.com/hurtener/Harbor/internal/runtime/agentcfg/protocol"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
	toolauth "github.com/hurtener/Harbor/internal/tools/auth"
)

type resumeSequencePreparer struct {
	mu       sync.Mutex
	calls    int
	prepared []*recordingPrepared
	cancel   context.CancelFunc
	closeErr error
}

type producerPausePreparer struct {
	coord    pauseresume.Coordinator
	requests int
	token    pauseresume.Token
	prepared recordingPrepared
}

func (p *producerPausePreparer) PrepareConnection(ctx context.Context, req agentcfgprotocol.AttachRequest) (agentcfgprotocol.PreparedConnection, error) {
	if p.requests > 0 {
		return &p.prepared, nil
	}
	p.requests++
	pause, err := p.coord.Request(ctx, pauseresume.PauseRequest{Identity: req.Identity, Reason: pauseresume.ReasonExternalEvent})
	if err != nil {
		return nil, err
	}
	p.token = pause.Token
	return nil, errors.Join(agentcfgprotocol.ErrAuthRequired,
		&toolauth.ErrAuthRequired{Source: "producer", PauseToken: string(pause.Token), Message: "authorization required"})
}

func (p *resumeSequencePreparer) PrepareConnection(_ context.Context, _ agentcfgprotocol.AttachRequest) (agentcfgprotocol.PreparedConnection, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls++
	if p.calls == 1 {
		return nil, fmt.Errorf("%w: structured challenge", agentcfgprotocol.ErrAuthRequired)
	}
	prepared := &recordingPrepared{closeErr: p.closeErr}
	p.prepared = append(p.prepared, prepared)
	if p.cancel != nil {
		p.cancel()
		p.cancel = nil
	}
	return prepared, nil
}

func (p *resumeSequencePreparer) callCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

func identityContext(t *testing.T, ctx context.Context) context.Context {
	t.Helper()
	withID, err := identity.With(ctx, identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"})
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	return withID
}

func newContinuationService(t *testing.T, reg agentcfg.Registry, coord pauseresume.Coordinator, preparer *resumeSequencePreparer) *agentcfgprotocol.Service {
	t.Helper()
	svc, err := agentcfgprotocol.NewService(reg,
		agentcfgprotocol.WithConnectionPreparer(preparer),
		agentcfgprotocol.WithCoordinator(coord),
	)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return svc
}

func TestAddMCPConnection_AuthContinuationSurvivesRestartAndActivates(t *testing.T) {
	reg := newRegistry(t)
	checkpointStore, err := newStateStore(t)
	if err != nil {
		t.Fatalf("checkpoint store: %v", err)
	}
	preparer := &resumeSequencePreparer{}
	c1 := pauseresume.New(pauseresume.WithCheckpointStore(checkpointStore))
	svc1 := newContinuationService(t, reg, c1, preparer)
	resp, err := svc1.AddMCPConnection(context.Background(), addReq(httpConnNamed("restartable"), nil))
	if err != nil {
		t.Fatalf("AddMCPConnection: %v", err)
	}
	if resp.State != string(agentcfgprotocol.ConnectionStateAuthRequired) || resp.PauseToken == "" {
		t.Fatalf("response = %+v, want auth_required with token", resp)
	}

	c2 := pauseresume.New(pauseresume.WithCheckpointStore(checkpointStore))
	_ = newContinuationService(t, reg, c2, preparer)
	ctx := identityContext(t, context.Background())
	if err := c2.Resume(ctx, pauseresume.Token(resp.PauseToken), pauseresume.DecisionApprove, nil); err != nil {
		t.Fatalf("Resume after restart: %v", err)
	}
	if got := preparer.callCount(); got != 2 {
		t.Fatalf("prepare calls = %d, want auth attempt + resumed attempt", got)
	}
	activated, closed := preparer.prepared[0].counts()
	if activated != 1 || closed != 0 {
		t.Fatalf("resumed prepared lifecycle = activate:%d close:%d", activated, closed)
	}
}

func TestAddMCPConnection_UsesProviderOwnedPauseWithoutDuplicate(t *testing.T) {
	reg := newRegistry(t)
	coord := pauseresume.New()
	preparer := &producerPausePreparer{coord: coord}
	svc, err := agentcfgprotocol.NewService(reg,
		agentcfgprotocol.WithConnectionPreparer(preparer), agentcfgprotocol.WithCoordinator(coord))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	resp, err := svc.AddMCPConnection(context.Background(), addReq(httpConnNamed("one-pause"), nil))
	if err != nil {
		t.Fatalf("AddMCPConnection: %v", err)
	}
	if resp.PauseToken != string(preparer.token) || preparer.requests != 1 {
		t.Fatalf("response token=%q producer token=%q requests=%d", resp.PauseToken, preparer.token, preparer.requests)
	}
	if err := coord.Resume(identityContext(t, context.Background()), preparer.token, pauseresume.DecisionApprove, nil); err != nil {
		t.Fatalf("Resume provider pause: %v", err)
	}
	activated, _ := preparer.prepared.counts()
	if activated != 1 {
		t.Fatalf("activation count = %d, want 1", activated)
	}
}

func TestAddMCPConnection_AuthRequiredWriteLandedThenErroredRetainsProducerPause(t *testing.T) {
	base := newRegistry(t)
	writeErr := errors.New("commit acknowledgement lost")
	reg := &landedThenErroredRegistry{Registry: base, err: writeErr}
	bus := newCollectingBus(t)
	coord := pauseresume.New(pauseresume.WithBus(bus))
	preparer := &producerPausePreparer{coord: coord}
	installer := newRecordingInstaller()
	svc, err := agentcfgprotocol.NewService(reg,
		agentcfgprotocol.WithConnectionPreparer(preparer),
		agentcfgprotocol.WithCoordinator(coord),
		agentcfgprotocol.WithBus(bus),
		agentcfgprotocol.WithProviderInstaller(installer),
		agentcfgprotocol.WithAllowWireOAuthDescriptor(true),
	)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	conn := httpConnNamed("landed-auth")
	conn.OAuth = &prototypes.AgentConfigOAuthProviderDescriptor{
		Name:             "landed-auth-provider",
		Driver:           "tokenexchange",
		CredentialSource: "remote",
		CredentialBroker: "broker",
		TokenURL:         "https://idp.example.test/token",
	}
	ctx := identityContext(t, context.Background())
	resp, err := svc.AddMCPConnection(ctx, addReq(conn, nil))
	if err != nil {
		t.Fatalf("confirmed landed auth-required add returned lost acknowledgement: %v", err)
	}
	if resp.State != string(agentcfgprotocol.ConnectionStateAuthRequired) || resp.Revision == nil {
		t.Fatalf("response = %+v, want durable auth_required revision", resp)
	}
	if resp.PauseToken == "" || resp.PauseToken != string(preparer.token) {
		t.Fatalf("response pause token=%q producer token=%q", resp.PauseToken, preparer.token)
	}
	authEvents := bus.eventsOfType(agentcfg.EventTypeMCPConnectionAuthRequired)
	if len(authEvents) != 1 {
		t.Fatalf("auth_required lifecycle events=%d, want 1", len(authEvents))
	}
	payload, ok := authEvents[0].Payload.(agentcfg.MCPConnectionLifecyclePayload)
	if !ok || payload.RevisionID != resp.Revision.RevisionID || payload.PauseToken != resp.PauseToken {
		t.Fatalf("auth_required lifecycle payload=%#v response=%+v", authEvents[0].Payload, resp)
	}
	if !installer.isInstalled("landed-auth-provider") {
		t.Fatal("exact landed provider descriptor was not published")
	}
	active, set, err := base.Active(ctx, qScope(), testAgentID, agentcfg.ConfigScopeAgent)
	if err != nil || !set || active.RevisionID != resp.Revision.RevisionID {
		t.Fatalf("active landed revision: set=%v id=%q response=%q err=%v", set, active.RevisionID, resp.Revision.RevisionID, err)
	}
	status, err := coord.Status(ctx, preparer.token)
	if err != nil || status.State != pauseresume.StatusPaused {
		t.Fatalf("producer pause was rejected or hidden: state=%q err=%v", status.State, err)
	}
	if err := coord.Resume(ctx, preparer.token, pauseresume.DecisionApprove, nil); err != nil {
		t.Fatalf("Resume retained producer pause: %v", err)
	}
	activated, closed := preparer.prepared.counts()
	if activated != 1 || closed != 0 {
		t.Fatalf("retained continuation lifecycle = activate:%d close:%d", activated, closed)
	}
}

func TestAddMCPConnection_ContinuationDescriptorDriftDoesNotPrepareOrPublish(t *testing.T) {
	reg := newRegistry(t)
	preparer := &resumeSequencePreparer{}
	coord := pauseresume.New()
	svc := newContinuationService(t, reg, coord, preparer)
	resp, err := svc.AddMCPConnection(context.Background(), addReq(httpConnNamed("drifts"), nil))
	if err != nil {
		t.Fatalf("AddMCPConnection: %v", err)
	}
	q := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}}
	if _, err := reg.SetRevision(context.Background(), q, testAgentID, agentcfg.ConfigScopeAgent,
		agentcfg.ConfigPayload{Connections: &agentcfg.ConnectionsSection{Servers: []agentcfg.MCPConnectionDescriptor{{Name: "drifts", Transport: agentcfg.MCPTransportHTTP, URL: "https://changed.invalid/mcp"}}}}, agentcfg.SetOptions{}); err != nil {
		t.Fatalf("move desired descriptor: %v", err)
	}
	if err := coord.Resume(identityContext(t, context.Background()), pauseresume.Token(resp.PauseToken), pauseresume.DecisionApprove, nil); err != nil {
		t.Fatalf("Resume drifted descriptor: %v", err)
	}
	if got := preparer.callCount(); got != 1 {
		t.Fatalf("prepare calls = %d, want initial auth attempt only", got)
	}
}

func TestAddMCPConnection_ContinuationCancellationClosesAndStaysPaused(t *testing.T) {
	reg := newRegistry(t)
	closeBoom := errors.New("close boom")
	preparer := &resumeSequencePreparer{closeErr: closeBoom}
	coord := pauseresume.New()
	svc := newContinuationService(t, reg, coord, preparer)
	resp, err := svc.AddMCPConnection(context.Background(), addReq(httpConnNamed("cancelled"), nil))
	if err != nil {
		t.Fatalf("AddMCPConnection: %v", err)
	}
	resumeBase, cancel := context.WithCancel(context.Background())
	preparer.cancel = cancel
	err = coord.Resume(identityContext(t, resumeBase), pauseresume.Token(resp.PauseToken), pauseresume.DecisionApprove, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Resume = %v, want context.Canceled", err)
	}
	if !errors.Is(err, closeBoom) {
		t.Fatalf("Resume = %v, want joined cleanup error", err)
	}
	activated, closed := preparer.prepared[0].counts()
	if activated != 0 || closed != 1 {
		t.Fatalf("cancelled prepared lifecycle = activate:%d close:%d", activated, closed)
	}
	st, err := coord.Status(identityContext(t, context.Background()), pauseresume.Token(resp.PauseToken))
	if err != nil || st.State != pauseresume.StatusPaused {
		t.Fatalf("pause after cancellation = state:%q err:%v", st.State, err)
	}
}
