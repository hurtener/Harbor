package protocol_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/identity"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	agentcfgprotocol "github.com/hurtener/Harbor/internal/runtime/agentcfg/protocol"
	"github.com/hurtener/Harbor/internal/runtime/agentcfg/runsnapshot"
	"github.com/hurtener/Harbor/internal/state"
)

type retirementExactDetacher struct {
	mu           sync.Mutex
	failExact    int
	exactCalls   int
	genericCalls int
	fenceSeals   int
	siblings     map[string]bool
	wantOwners   map[string]identity.Identity
}

func (d *retirementExactDetacher) DetachConnection(context.Context, string, string, string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.genericCalls++
	return nil
}

func (d *retirementExactDetacher) DetachExactConnection(ctx context.Context, tenant, agentID, name, fingerprint string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.exactCalls++
	if tenant == "" || agentID == "" || name == "" || fingerprint == "" {
		return errors.New("incomplete exact detach")
	}
	if want, ok := d.wantOwners[name]; ok {
		got, found := identity.From(ctx)
		if !found || got != want {
			return fmt.Errorf("exact detach owner=(%+v,%t), want %+v", got, found, want)
		}
	}
	if d.failExact > 0 {
		d.failExact--
		return errors.New("injected retryable close failure")
	}
	delete(d.siblings, tenant+"/"+agentID+"/"+name)
	return nil
}

type retirementExactFence struct{ parent *retirementExactDetacher }

func (f retirementExactFence) Seal() {
	f.parent.mu.Lock()
	f.parent.fenceSeals++
	f.parent.mu.Unlock()
}
func (retirementExactFence) Cancel(context.Context) error { return nil }

func (d *retirementExactDetacher) BeginExactConnectionTeardown(tenant, agentID, name, fingerprint string) (agentcfgprotocol.ExactConnectionTeardownFence, error) {
	if tenant == "" || agentID == "" || name == "" || fingerprint == "" {
		return nil, errors.New("incomplete exact teardown fence")
	}
	return retirementExactFence{parent: d}, nil
}

func seedRetirementSignedOperation(t *testing.T, st state.StateStore, tenant, user, session, agent, suffix string, phase agentcfg.SignedOAuthMCPOperationPhase) agentcfg.SignedOAuthMCPOperation {
	t.Helper()
	ops, err := agentcfg.NewSignedOAuthMCPOperationStore(st)
	if err != nil {
		t.Fatal(err)
	}
	binding := agentcfg.SignedOAuthMCPBinding{
		TenantID: tenant, UserID: user, SessionID: session, AgentID: agent,
		Broker: "broker-" + suffix, ProviderName: "provider-" + suffix,
		CapabilityRevision: "cap-" + suffix, URLDigest: strings.Repeat("a", 64), SinkDigest: strings.Repeat("b", 64),
		Audience:   "audience-" + suffix,
		Connection: agentcfg.SignedOAuthMCPConnectionDescriptor{Name: "connection-" + suffix, URL: "https://example.invalid/" + suffix},
	}
	key := agentcfg.SignedOAuthMCPReplayKey{TenantID: tenant, TrustAnchorName: "anchor", Issuer: "issuer", KeyID: "kid", JTI: "secret-jti-" + suffix}
	op, _, err := ops.Claim(context.Background(), key, binding, time.Now().Add(-24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if phase == agentcfg.SignedOAuthMCPPhaseClaimed {
		return op
	}
	op, err = ops.Advance(context.Background(), op, agentcfg.SignedOAuthMCPPhaseRevisionCommitted, "revision-"+suffix)
	if err != nil {
		t.Fatal(err)
	}
	if phase == agentcfg.SignedOAuthMCPPhaseRevisionCommitted {
		return op
	}
	op, err = ops.Advance(context.Background(), op, agentcfg.SignedOAuthMCPPhasePublished, op.RevisionID)
	if err != nil {
		t.Fatal(err)
	}
	for _, next := range []agentcfg.SignedOAuthMCPOperationPhase{
		agentcfg.SignedOAuthMCPPhaseRemovalAdmitted,
		agentcfg.SignedOAuthMCPPhaseRemovalRevisionCommitted,
		agentcfg.SignedOAuthMCPPhaseCatalogUnpublished,
		agentcfg.SignedOAuthMCPPhaseTeardownReceipted,
		agentcfg.SignedOAuthMCPPhaseRemoved,
	} {
		if op.Phase == phase {
			break
		}
		op, err = ops.Advance(context.Background(), op, next, op.RevisionID)
		if err != nil {
			t.Fatal(err)
		}
	}
	return op
}

func TestRetire_SignedOAuthMCPDiscoversCrossSessionExpiredPairsAndRetriesClose(t *testing.T) {
	ctx := context.Background()
	reg, st := newRegistryWithState(t)
	const tenant = "retirement-tenant"
	const agent = "retirement-agent"
	first := seedRetirementSignedOperation(t, st, tenant, "owner-one", "session-one", agent, "one", agentcfg.SignedOAuthMCPPhasePublished)
	second := seedRetirementSignedOperation(t, st, tenant, "owner-two", "session-two", agent, "two", agentcfg.SignedOAuthMCPPhasePublished)
	foreign := seedRetirementSignedOperation(t, st, "foreign-tenant", "owner-three", "session-three", agent, "foreign", agentcfg.SignedOAuthMCPPhasePublished)

	admin := identity.Quadruple{Identity: identity.Identity{TenantID: tenant, UserID: "retirement-admin", SessionID: "admin-session"}}
	revision, err := reg.SetRevision(ctx, admin, agent, agentcfg.ConfigScopeAgent, agentcfg.ConfigPayload{Skills: &agentcfg.SkillsSelection{Names: []string{"history"}}}, agentcfg.SetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	detacher := &retirementExactDetacher{
		failExact: 1,
		siblings:  map[string]bool{"foreign-tenant/" + agent + "/connection-foreign": true, tenant + "/sibling-agent/connection-one": true},
		wantOwners: map[string]identity.Identity{
			"connection-one": {TenantID: tenant, UserID: "owner-one", SessionID: "session-one"},
			"connection-two": {TenantID: tenant, UserID: "owner-two", SessionID: "session-two"},
		},
	}
	svc, err := agentcfgprotocol.NewService(reg, agentcfgprotocol.WithSignedOAuthMCPOperationState(st), agentcfgprotocol.WithConnectionDetacher(detacher), agentcfgprotocol.WithRunSnapshotGate(runsnapshot.NewGate()))
	if err != nil {
		t.Fatal(err)
	}
	req := prototypes.AgentConfigRetireRequest{
		Identity: prototypes.IdentityScope{Tenant: tenant, User: admin.UserID, Session: admin.SessionID},
		AgentID:  agent, OperationID: "retire-signed-pairs", ExpectedContentHash: revision.ContentHash,
	}
	if _, err := svc.Retire(ctx, req); err == nil || !strings.Contains(err.Error(), "retryable close failure") {
		t.Fatalf("first retirement error=%v, want close failure", err)
	}
	status, found, err := reg.(agentcfg.RetirementRegistry).RetirementStatus(ctx, admin, agent)
	if err != nil || !found || len(status.Cleanup) != 1 || status.Cleanup[0].Class != agentcfg.RetirementCleanupClassSignedOAuthMCPPair {
		t.Fatalf("pending signed cleanup=(%+v,%t,%v)", status, found, err)
	}
	resource := status.Cleanup[0].Resource
	if len(resource) != 129 || resource[64] != '.' || strings.Contains(resource, "owner") || strings.Contains(resource, "session") || strings.Contains(resource, "jti") || strings.Contains(resource, "example") {
		t.Fatalf("retirement manifest resource is not hash-only: %q", resource)
	}

	response, err := svc.Retire(ctx, req)
	if err != nil || !response.Status.Completed {
		t.Fatalf("retirement retry=(%+v,%v)", response, err)
	}
	ops, err := agentcfg.NewSignedOAuthMCPOperationStore(st)
	if err != nil {
		t.Fatal(err)
	}
	for _, seeded := range []agentcfg.SignedOAuthMCPOperation{first, second} {
		latest, loadErr := ops.Load(ctx, seeded.ReplayKey)
		if loadErr != nil || latest.Phase != agentcfg.SignedOAuthMCPPhaseRemoved {
			t.Fatalf("pair %s did not converge: phase=%s err=%v", seeded.Binding.SessionID, latest.Phase, loadErr)
		}
	}
	foreignLatest, err := ops.Load(ctx, foreign.ReplayKey)
	if err != nil || foreignLatest.Phase != agentcfg.SignedOAuthMCPPhasePublished {
		t.Fatalf("foreign tenant operation changed: phase=%s err=%v", foreignLatest.Phase, err)
	}
	detacher.mu.Lock()
	defer detacher.mu.Unlock()
	if detacher.genericCalls != 0 || detacher.exactCalls < 3 || detacher.fenceSeals < 2 {
		t.Fatalf("wrong teardown path: generic=%d exact=%d seals=%d", detacher.genericCalls, detacher.exactCalls, detacher.fenceSeals)
	}
	if !detacher.siblings["foreign-tenant/"+agent+"/connection-foreign"] || !detacher.siblings[tenant+"/sibling-agent/connection-one"] {
		t.Fatalf("sibling resources were touched: %+v", detacher.siblings)
	}
}

func TestRetire_SignedOAuthMCPResumesEveryRemovalPhase(t *testing.T) {
	for _, phase := range []agentcfg.SignedOAuthMCPOperationPhase{
		agentcfg.SignedOAuthMCPPhasePublished,
		agentcfg.SignedOAuthMCPPhaseRemovalAdmitted,
		agentcfg.SignedOAuthMCPPhaseRemovalRevisionCommitted,
		agentcfg.SignedOAuthMCPPhaseCatalogUnpublished,
		agentcfg.SignedOAuthMCPPhaseTeardownReceipted,
	} {
		t.Run(string(phase), func(t *testing.T) {
			reg, st := newRegistryWithState(t)
			suffix := strings.ReplaceAll(string(phase), "_", "-")
			seeded := seedRetirementSignedOperation(t, st, "phase-tenant", "phase-owner", "phase-session", "phase-agent", suffix, phase)
			admin := identity.Quadruple{Identity: identity.Identity{TenantID: "phase-tenant", UserID: "admin", SessionID: "admin-session"}}
			revision, err := reg.SetRevision(context.Background(), admin, "phase-agent", agentcfg.ConfigScopeAgent, agentcfg.ConfigPayload{}, agentcfg.SetOptions{})
			if err != nil {
				t.Fatal(err)
			}
			detacher := &retirementExactDetacher{siblings: make(map[string]bool)}
			svc, err := agentcfgprotocol.NewService(reg, agentcfgprotocol.WithSignedOAuthMCPOperationState(st), agentcfgprotocol.WithConnectionDetacher(detacher), agentcfgprotocol.WithRunSnapshotGate(runsnapshot.NewGate()))
			if err != nil {
				t.Fatal(err)
			}
			_, err = svc.Retire(context.Background(), prototypes.AgentConfigRetireRequest{Identity: prototypes.IdentityScope{Tenant: admin.TenantID, User: admin.UserID, Session: admin.SessionID}, AgentID: "phase-agent", OperationID: fmt.Sprintf("retire-%s", suffix), ExpectedContentHash: revision.ContentHash})
			if err != nil {
				t.Fatal(err)
			}
			operations, err := agentcfg.NewSignedOAuthMCPOperationStore(st)
			if err != nil {
				t.Fatal(err)
			}
			latest, err := operations.Load(context.Background(), seeded.ReplayKey)
			if err != nil || latest.Phase != agentcfg.SignedOAuthMCPPhaseRemoved {
				t.Fatalf("phase=%s latest=%s err=%v", phase, latest.Phase, err)
			}
		})
	}
}
