package protocol_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/identity"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	agentcfgprotocol "github.com/hurtener/Harbor/internal/runtime/agentcfg/protocol"
	"github.com/hurtener/Harbor/internal/runtime/agentcfg/runsnapshot"
)

type retirementReplayDetacher struct {
	mu    sync.Mutex
	errs  []error
	calls int
}

func (d *retirementReplayDetacher) DetachConnection(_ context.Context, _, _, _ string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.calls++
	if len(d.errs) == 0 {
		return nil
	}
	err := d.errs[0]
	d.errs = d.errs[1:]
	return err
}

func (d *retirementReplayDetacher) callCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.calls
}

type retirementReplayInstaller struct {
	mu    sync.Mutex
	errs  []error
	calls int
}

func (*retirementReplayInstaller) InstallProvider(context.Context, string, string, agentcfg.OAuthProviderDescriptor) error {
	return nil
}

func (i *retirementReplayInstaller) UninstallProvider(_ context.Context, _, _, _ string) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.calls++
	if len(i.errs) == 0 {
		return nil
	}
	err := i.errs[0]
	i.errs = i.errs[1:]
	return err
}

func (i *retirementReplayInstaller) callCount() int {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.calls
}

func TestRetire_TombstonesThenCanceledDrainRetryDefersCleanup(t *testing.T) {
	reg, _ := newRegistryWithState(t)
	const tenant = "run-drain-tenant"
	const agent = "run-drain-agent"
	admin := identity.Quadruple{Identity: identity.Identity{TenantID: tenant, UserID: "admin", SessionID: "admin-session"}}
	revision, err := reg.SetRevision(t.Context(), admin, agent, agentcfg.ConfigScopeAgent, agentcfg.ConfigPayload{
		Connections: &agentcfg.ConnectionsSection{Servers: []agentcfg.MCPConnectionDescriptor{{
			Name: "drain-mcp", Transport: agentcfg.MCPTransportHTTP, URL: "https://mcp.example.test",
		}}},
	}, agentcfg.SetOptions{})
	if err != nil {
		t.Fatal(err)
	}

	gate := runsnapshot.NewGate()
	lease, err := gate.Acquire(t.Context(), tenant, agent)
	if err != nil {
		t.Fatal(err)
	}
	detacher := &retirementReplayDetacher{}
	svc, err := agentcfgprotocol.NewService(reg,
		agentcfgprotocol.WithConnectionDetacher(detacher),
		agentcfgprotocol.WithRunSnapshotGate(gate))
	if err != nil {
		t.Fatal(err)
	}
	req := prototypes.AgentConfigRetireRequest{
		Identity: prototypes.IdentityScope{Tenant: tenant, User: admin.UserID, Session: admin.SessionID},
		AgentID:  agent, OperationID: "run-drain-retire", ExpectedContentHash: revision.ContentHash,
	}

	waitCtx, cancel := context.WithTimeout(t.Context(), 40*time.Millisecond)
	defer cancel()
	if _, err := svc.Retire(waitCtx, req); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("retire with held run = %v, want deadline", err)
	}
	status, found, err := reg.(agentcfg.RetirementRegistry).RetirementStatus(t.Context(), admin, agent)
	if err != nil || !found || status.OperationID != req.OperationID || status.Completed {
		t.Fatalf("durable tombstone after canceled drain = (%+v,%t,%v)", status, found, err)
	}
	if detacher.callCount() != 0 {
		t.Fatalf("cleanup ran before held run drained: calls=%d", detacher.callCount())
	}
	if _, err := gate.Acquire(t.Context(), tenant, agent); !errors.Is(err, runsnapshot.ErrAdmissionClosed) {
		t.Fatalf("new run after tombstone = %v, want ErrAdmissionClosed", err)
	}
	if other, err := gate.Acquire(t.Context(), tenant, "other-agent"); err != nil {
		t.Fatalf("other agent was blocked: %v", err)
	} else {
		other.Release()
	}

	lease.Release()
	response, err := svc.Retire(t.Context(), req)
	if err != nil || !response.Status.Completed {
		t.Fatalf("same-operation retry after drain = (%+v,%v)", response, err)
	}
	if detacher.callCount() != 1 {
		t.Fatalf("cleanup calls after drain = %d, want 1", detacher.callCount())
	}
}

func TestRetire_GenericCleanupFailureReplaysBeforeProgressAck(t *testing.T) {
	closeErr := errors.New("retryable generic close failure")
	for _, tc := range []struct {
		name       string
		payload    agentcfg.ConfigPayload
		options    func() ([]agentcfgprotocol.Option, func() int)
		wantClass  string
		wantSource string
	}{
		{
			name: "mcp_connection",
			payload: agentcfg.ConfigPayload{Connections: &agentcfg.ConnectionsSection{Servers: []agentcfg.MCPConnectionDescriptor{{
				Name: "replay-mcp", Transport: agentcfg.MCPTransportHTTP, URL: "https://replay.example.test",
			}}}},
			options: func() ([]agentcfgprotocol.Option, func() int) {
				d := &retirementReplayDetacher{errs: []error{closeErr, nil}}
				return []agentcfgprotocol.Option{agentcfgprotocol.WithConnectionDetacher(d)}, d.callCount
			},
			wantClass: "mcp_connection", wantSource: "replay-mcp",
		},
		{
			name: "oauth_provider",
			payload: agentcfg.ConfigPayload{OAuthProviders: &agentcfg.OAuthProvidersSection{Providers: []agentcfg.OAuthProviderDescriptor{{
				Name: "replay-oauth", Driver: "tokenexchange", CredentialSource: "remote", CredentialBroker: "broker",
			}}}},
			options: func() ([]agentcfgprotocol.Option, func() int) {
				i := &retirementReplayInstaller{errs: []error{closeErr, nil}}
				return []agentcfgprotocol.Option{agentcfgprotocol.WithProviderInstaller(i)}, i.callCount
			},
			wantClass: "oauth_provider", wantSource: "replay-oauth",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reg, _ := newRegistryWithState(t)
			tenant, agent := "replay-tenant-"+tc.name, "replay-agent"
			admin := identity.Quadruple{Identity: identity.Identity{TenantID: tenant, UserID: "admin", SessionID: "session"}}
			revision, err := reg.SetRevision(t.Context(), admin, agent, agentcfg.ConfigScopeAgent, tc.payload, agentcfg.SetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			opts, calls := tc.options()
			opts = append(opts, agentcfgprotocol.WithRunSnapshotGate(runsnapshot.NewGate()))
			svc, err := agentcfgprotocol.NewService(reg, opts...)
			if err != nil {
				t.Fatal(err)
			}
			req := prototypes.AgentConfigRetireRequest{
				Identity: prototypes.IdentityScope{Tenant: tenant, User: admin.UserID, Session: admin.SessionID},
				AgentID:  agent, OperationID: "generic-replay-" + tc.name, ExpectedContentHash: revision.ContentHash,
			}
			if _, err := svc.Retire(t.Context(), req); !errors.Is(err, closeErr) {
				t.Fatalf("first retirement = %v, want close failure", err)
			}
			status, found, err := reg.(agentcfg.RetirementRegistry).RetirementStatus(t.Context(), admin, agent)
			if err != nil || !found || len(status.Cleanup) != 1 || status.Cleanup[0].Class != tc.wantClass || status.Cleanup[0].Resource != tc.wantSource || status.Completed {
				t.Fatalf("pending cleanup = (%+v,%t,%v), want %s/%s", status, found, err, tc.wantClass, tc.wantSource)
			}
			response, err := svc.Retire(t.Context(), req)
			if err != nil || !response.Status.Completed {
				t.Fatalf("retirement retry = (%+v,%v)", response, err)
			}
			if calls() != 2 {
				t.Fatalf("cleanup calls = %d, want retry of same operation", calls())
			}
		})
	}
}
