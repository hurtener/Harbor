package protocol_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	agentcfgprotocol "github.com/hurtener/Harbor/internal/runtime/agentcfg/protocol"
	"github.com/hurtener/Harbor/internal/state"
	stateinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
)

func TestSignedOAuthMCPRecoveryFaults_AbortedLegacyFenceSurvivesReuse(t *testing.T) {
	for _, action := range []string{"remove", "corrected-register", "reconcile"} {
		t.Run(action, func(t *testing.T) {
			ctx := context.Background()
			now := time.Now().UTC()
			base, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
			if err != nil {
				t.Fatal(err)
			}
			checkpointErr := errors.New("terminal rejection checkpoint unavailable")
			store := &capabilityOperationPhaseStore{StateStore: base, phase: agentcfg.SignedOAuthMCPPhasePreparationRejected, failErr: checkpointErr}
			svc, key, reg, _, preparer := signedCapabilityServiceWithStore(t, now, store)
			preparer.failPrepare = errors.New("connect unavailable")
			oldReq := signedCapabilityRequest(t, key, now, "legacy-x", "aud-x")
			if _, err := svc.RegisterOAuthMCPCapability(ctx, oldReq); err == nil {
				t.Fatal("fixture connected")
			}
			q := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}}
			history, err := reg.ListRevisions(ctx, q, testAgentID, agentcfg.ConfigScopeAgent, 0)
			if err != nil || len(history) != 1 {
				t.Fatalf("failed candidate = %+v %v", history, err)
			}
			candidate := history[0]
			remove := prototypes.AgentConfigRemoveOAuthMCPCapabilityRequest{Identity: scope(), AgentID: testAgentID, ProviderName: oldReq.ProviderName, ExpectedContentHash: candidate.ContentHash}
			if _, err := svc.RemoveOAuthMCPCapability(ctx, remove); !errors.Is(err, checkpointErr) {
				t.Fatalf("expected second-write failure after abort: %v", err)
			}
			ops, _ := agentcfg.NewSignedOAuthMCPOperationStore(store)
			old, err := ops.LoadForPair(ctx, q.TenantID, candidate.Payload.SignedOAuthMCPPair)
			if err != nil || old.Phase != agentcfg.SignedOAuthMCPPhasePreparationRejectionAdmitted {
				t.Fatalf("incomplete rejection receipt = %+v %v", old, err)
			}
			fences, _ := agentcfg.NewSignedOAuthMCPActivationFenceStore(store)
			oldKind := mustOperationKind(t, ops, old.ReplayKey)
			beforeFence, err := fences.LoadForOperation(ctx, q.TenantID, testAgentID, oldKind)
			if err != nil {
				t.Fatal(err)
			}
			preparer.failPrepare = nil
			url, sink, _ := agentcfg.CanonicalOAuthMCPURL("https://example.test/mcp")
			other := signedCapabilityRequestNamed(t, key, now, scope(), testAgentID, "legacy-y", "aud-y", "provider-y", prototypes.SignedOAuthMCPConnectionDescriptor{Name: "connection-y", URL: url}, sink)
			if _, err := svc.RegisterOAuthMCPCapability(ctx, other); !errors.Is(err, agentcfg.ErrSignedCapabilityPending) {
				t.Fatalf("legacy slot reused before X terminal checkpoint = %v", err)
			}
			if afterFence, err := fences.LoadForOperation(ctx, q.TenantID, testAgentID, oldKind); err != nil || !reflect.DeepEqual(afterFence, beforeFence) {
				t.Fatalf("blocked Y changed X abort proof: %+v %v", afterFence, err)
			}
			svc = signedCapabilityServiceForExisting(t, now, reg, store, preparer, key)
			switch action {
			case "remove":
				if _, err := svc.RemoveOAuthMCPCapability(ctx, remove); err != nil {
					t.Fatalf("X removal after Y reused legacy fence: %v", err)
				}
			case "corrected-register":
				corrected := signedCapabilityRequest(t, key, now, "legacy-x-corrected", "aud-x-corrected")
				corrected.ExpectedContentHash = agentcfg.ExpectNoActiveRevision
				if _, err := svc.RegisterOAuthMCPCapability(ctx, corrected); err != nil {
					t.Fatalf("X correction after Y reused legacy fence: %v", err)
				}
			case "reconcile":
				reconciler, err := agentcfgprotocol.NewSignedOAuthMCPReconciler(reg, store, preparer, preparer, capabilityInstaller{})
				if err != nil {
					t.Fatal(err)
				}
				if err := reconciler.ReconcileSignedOAuthMCPCapability(ctx, q, testAgentID); err != nil {
					t.Fatalf("X reconciliation after Y reused legacy fence: %v", err)
				}
			}
			old, err = ops.Load(ctx, old.ReplayKey)
			if err != nil || old.Phase != agentcfg.SignedOAuthMCPPhasePreparationRejected {
				t.Fatalf("X receipt did not finish = %+v %v", old, err)
			}
			if _, err := svc.RegisterOAuthMCPCapability(ctx, other); err != nil {
				t.Fatalf("Y retry after X terminal checkpoint = %v", err)
			}
			active, set, err := reg.Active(ctx, q, testAgentID, agentcfg.ConfigScopeAgent)
			if err != nil || !set {
				t.Fatalf("Y authority lost = %+v %t %v", active, set, err)
			}
			if _, ok, err := active.Payload.SignedOAuthMCPPairByProvider(other.ProviderName); err != nil || !ok {
				t.Fatalf("X recovery altered Y = %+v %v", active.Payload, err)
			}
		})
	}
}

func TestSignedOAuthMCPRecoveryFaults_UserPreconditionMatchesVisibleAuthority(t *testing.T) {
	for _, prior := range []bool{false, true} {
		for _, expected := range []string{"prior", "candidate", "stale", "generic-write"} {
			name := expected
			if prior {
				name += "-with-prior"
			}
			t.Run(name, func(t *testing.T) {
				now := time.Now().UTC()
				svc, key, reg, store, preparer := signedCapabilityServiceWithRegistry(t, now)
				id := identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}
				q := identity.Quadruple{Identity: id}
				ctx := verifiedUserCapabilityContext(t, id, true, true)
				var before agentcfg.Revision
				if prior {
					var err error
					before, err = reg.SetRevision(ctx, q, testAgentID, agentcfg.ConfigScopeUser, agentcfg.ConfigPayload{}, agentcfg.SetOptions{})
					if err != nil {
						t.Fatal(err)
					}
				}
				preparer.failPrepare = errors.New("connect unavailable")
				oldReq := signedCapabilityRequest(t, key, now, "user-precondition-old", "old-audience")
				if _, err := svc.RegisterUserOAuthMCPCapability(ctx, userRegisterRequest(oldReq)); err == nil {
					t.Fatal("fixture connected")
				}
				history, err := reg.ListRevisions(ctx, q, testAgentID, agentcfg.ConfigScopeUser, 0)
				if err != nil || len(history) == 0 {
					t.Fatalf("failed user candidate = %+v %v", history, err)
				}
				candidate := history[0]
				ops, _ := agentcfg.NewSignedOAuthMCPOperationStore(store)
				old, err := ops.LoadForPair(ctx, id.TenantID, candidate.Payload.SignedOAuthMCPPair)
				if err != nil {
					t.Fatal(err)
				}
				preparer.failPrepare = nil
				if expected == "generic-write" {
					if _, err := svc.UserSetRevision(ctx, prototypes.AgentConfigUserSetRevisionRequest{Identity: scope(), AgentID: testAgentID, Payload: prototypes.AgentConfigUserPayload{UserPrompt: "ordinary edit"}}); !errors.Is(err, agentcfg.ErrSignedCapabilityPending) {
						t.Fatalf("generic user writer bypassed pending candidate = %v", err)
					}
					latest, err := ops.Load(ctx, old.ReplayKey)
					if err != nil || !reflect.DeepEqual(latest, old) {
						t.Fatalf("generic user edit changed receipt = %+v %v", latest, err)
					}
					return
				}
				corrected := signedCapabilityRequest(t, key, now, "user-precondition-corrected", "corrected-audience")
				corrected.ExpectedContentHash = agentcfg.ExpectNoActiveRevision
				if prior {
					corrected.ExpectedContentHash = before.ContentHash
				}
				switch expected {
				case "candidate":
					corrected.ExpectedContentHash = candidate.ContentHash
				case "stale":
					corrected.ExpectedContentHash = "stale-hash"
				case "prior":
					get, err := svc.UserGet(ctx, prototypes.AgentConfigUserGetRequest{Identity: scope(), AgentID: testAgentID})
					if err != nil || get.Set != prior || (prior && (get.Revision == nil || get.Revision.ContentHash != before.ContentHash)) {
						t.Errorf("GET exposes hidden user candidate: %+v %v", get, err)
					}
				}
				response, err := svc.RegisterUserOAuthMCPCapability(ctx, userRegisterRequest(corrected))
				if expected == "prior" {
					if err != nil {
						t.Fatalf("user correction with visible precondition = %v", err)
					}
					if prior && response.Revision.ParentRevisionID != before.RevisionID {
						t.Fatalf("user correction lost restored prior = %+v", response.Revision)
					}
				} else {
					if !errors.Is(err, agentcfg.ErrRevisionConflict) {
						t.Fatalf("stale user correction = %v", err)
					}
					latest, err := ops.Load(ctx, old.ReplayKey)
					if err != nil || !reflect.DeepEqual(latest, old) {
						t.Fatalf("stale user correction retired candidate: %+v %v", latest, err)
					}
					physical := reg.(interface {
						PhysicalActive(context.Context, identity.Quadruple, string, agentcfg.ConfigScope) (agentcfg.Revision, bool, error)
					})
					active, set, err := physical.PhysicalActive(ctx, q, testAgentID, agentcfg.ConfigScopeUser)
					if err != nil || !set || active.RevisionID != candidate.RevisionID {
						t.Fatalf("stale user correction moved physical pointer = %+v %t %v", active, set, err)
					}
				}
			})
		}
	}
}

func TestSignedOAuthMCPRecoveryFaults_ClaimedPhysicalCandidateCanBeRetired(t *testing.T) {
	for _, action := range []string{"remove", "corrected-register", "same-jti-retry"} {
		t.Run(action, func(t *testing.T) {
			ctx := context.Background()
			now := time.Now().UTC()
			base, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
			if err != nil {
				t.Fatal(err)
			}
			checkpointErr := errors.New("revision checkpoint unavailable")
			store := &capabilityOperationPhaseStore{StateStore: base, phase: agentcfg.SignedOAuthMCPPhaseRevisionCommitted, failErr: checkpointErr}
			svc, key, reg, _, preparer := signedCapabilityServiceWithStore(t, now, store)
			oldReq := signedCapabilityRequest(t, key, now, "claimed-retire-old", "old-audience")
			if _, err := svc.RegisterOAuthMCPCapability(ctx, oldReq); !errors.Is(err, checkpointErr) {
				t.Fatalf("expected committed pointer / claimed receipt fault = %v", err)
			}
			q := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}}
			history, err := reg.ListRevisions(ctx, q, testAgentID, agentcfg.ConfigScopeAgent, 0)
			if err != nil || len(history) != 1 {
				t.Fatalf("claimed physical candidate = %+v %v", history, err)
			}
			candidate := history[0]
			ops, _ := agentcfg.NewSignedOAuthMCPOperationStore(store)
			old, err := ops.LoadForPair(ctx, q.TenantID, candidate.Payload.SignedOAuthMCPPair)
			if err != nil || old.Phase != agentcfg.SignedOAuthMCPPhaseClaimed {
				t.Fatalf("claimed receipt fixture = %+v %v", old, err)
			}
			if _, err := svc.RemoveOAuthMCPCapability(ctx, prototypes.AgentConfigRemoveOAuthMCPCapabilityRequest{Identity: scope(), AgentID: testAgentID, ProviderName: oldReq.ProviderName, ExpectedContentHash: "wrong-candidate-hash"}); !errors.Is(err, agentcfg.ErrRevisionConflict) {
				t.Fatalf("wrong claimed candidate hash = %v", err)
			}
			foreign := scope()
			foreign.User = "foreign-owner"
			foreign.Session = "foreign-session"
			foreignReq := signedCapabilityRequestFor(t, key, now, foreign, testAgentID, "claimed-foreign-attempt", "foreign-audience", oldReq.Connection.Name)
			if _, err := svc.RegisterOAuthMCPCapability(ctx, foreignReq); !errors.Is(err, agentcfg.ErrSignedCapabilityPending) {
				t.Fatalf("foreign claimed candidate replacement = %v", err)
			}
			if current, err := ops.Load(ctx, old.ReplayKey); err != nil || !reflect.DeepEqual(current, old) {
				t.Fatalf("refused claimed recovery changed receipt = %+v %v", current, err)
			}
			svc = signedCapabilityServiceForExisting(t, now, reg, store, preparer, key)
			if action == "same-jti-retry" {
				oldReq.ExpectedContentHash = agentcfg.ExpectNoActiveRevision
				if _, err := svc.RegisterOAuthMCPCapability(ctx, oldReq); err != nil {
					t.Fatalf("same JTI after claimed physical checkpoint = %v", err)
				}
			} else if action == "remove" {
				if _, err := svc.RemoveOAuthMCPCapability(ctx, prototypes.AgentConfigRemoveOAuthMCPCapabilityRequest{Identity: scope(), AgentID: testAgentID, ProviderName: oldReq.ProviderName, ExpectedContentHash: candidate.ContentHash}); err != nil {
					t.Fatalf("remove exact claimed physical candidate = %v", err)
				}
			} else {
				corrected := signedCapabilityRequest(t, key, now, "claimed-retire-corrected", "new-audience")
				corrected.ExpectedContentHash = agentcfg.ExpectNoActiveRevision
				if _, err := svc.RegisterOAuthMCPCapability(ctx, corrected); err != nil {
					t.Fatalf("correct claimed physical candidate = %v", err)
				}
			}
			old, err = ops.Load(ctx, old.ReplayKey)
			want := agentcfg.SignedOAuthMCPPhasePreparationRejected
			if action == "same-jti-retry" {
				want = agentcfg.SignedOAuthMCPPhasePublished
			}
			if err != nil || old.Phase != want {
				t.Fatalf("claimed candidate not retired = %+v %v", old, err)
			}
		})
	}
}

type userRecoveryCASRaceRegistry struct {
	agentcfg.Registry
	armed       bool
	replacement agentcfg.Revision
}

func (r *userRecoveryCASRaceRegistry) SetRevision(ctx context.Context, q identity.Quadruple, agentID string, scope agentcfg.ConfigScope, payload agentcfg.ConfigPayload, opts agentcfg.SetOptions) (agentcfg.Revision, error) {
	if r.armed && scope == agentcfg.ConfigScopeUser {
		r.armed = false
		var err error
		r.replacement, err = r.Registry.SetRevision(ctx, q, agentID, scope, agentcfg.ConfigPayload{Skills: &agentcfg.SkillsSelection{Names: []string{"concurrent-winner"}}}, agentcfg.SetOptions{})
		if err != nil {
			return agentcfg.Revision{}, err
		}
	}
	return r.Registry.SetRevision(ctx, q, agentID, scope, payload, opts)
}

func (r *userRecoveryCASRaceRegistry) PhysicalActive(ctx context.Context, q identity.Quadruple, agentID string, scope agentcfg.ConfigScope) (agentcfg.Revision, bool, error) {
	return r.Registry.(interface {
		PhysicalActive(context.Context, identity.Quadruple, string, agentcfg.ConfigScope) (agentcfg.Revision, bool, error)
	}).PhysicalActive(ctx, q, agentID, scope)
}

func TestSignedOAuthMCPRecoveryFaults_UserReplacementRetainsPhysicalCAS(t *testing.T) {
	now := time.Now().UTC()
	_, key, underlying, store, preparer := signedCapabilityServiceWithRegistry(t, now)
	reg := &userRecoveryCASRaceRegistry{Registry: underlying}
	svc := signedCapabilityServiceForExisting(t, now, reg, store, preparer, key)
	id := identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}
	q := identity.Quadruple{Identity: id}
	ctx := verifiedUserCapabilityContext(t, id, true, true)
	prior, err := reg.SetRevision(ctx, q, testAgentID, agentcfg.ConfigScopeUser, agentcfg.ConfigPayload{}, agentcfg.SetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	preparer.failPrepare = errors.New("connect unavailable")
	old := signedCapabilityRequest(t, key, now, "user-cas-old", "old-audience")
	if _, err := svc.RegisterUserOAuthMCPCapability(ctx, userRegisterRequest(old)); err == nil {
		t.Fatal("fixture connected")
	}
	preparer.failPrepare = nil
	reg.armed = true
	corrected := signedCapabilityRequest(t, key, now, "user-cas-corrected", "corrected-audience")
	corrected.ExpectedContentHash = prior.ContentHash
	if _, err := svc.RegisterUserOAuthMCPCapability(ctx, userRegisterRequest(corrected)); !errors.Is(err, agentcfg.ErrRevisionConflict) {
		t.Fatalf("post-compensation pointer race = %v", err)
	}
	active, set, err := reg.Active(ctx, q, testAgentID, agentcfg.ConfigScopeUser)
	if err != nil || !set || reg.replacement.RevisionID == "" || active.RevisionID != reg.replacement.RevisionID {
		t.Fatalf("replacement ignored final physical CAS: %+v %t %v", active, set, err)
	}
}

type userGuardReadScheduleStore struct {
	state.StateStore
	mu      sync.Mutex
	armed   bool
	reads   int
	entered chan struct{}
	release chan struct{}
}

func (s *userGuardReadScheduleStore) Load(ctx context.Context, q identity.Quadruple, kind string) (state.StateRecord, error) {
	record, err := s.StateStore.Load(ctx, q, kind)
	s.mu.Lock()
	pause := false
	if err == nil && s.armed && q.UserID == "u" && strings.HasPrefix(kind, "agentcfg.user.revision.") {
		s.reads++
		// UserSetRevision reads logical Active first, then the driver's
		// physical fence guard resolves the same prior revision a second time.
		pause = s.reads == 2
	}
	s.mu.Unlock()
	if pause {
		close(s.entered)
		select {
		case <-s.release:
		case <-ctx.Done():
			return state.StateRecord{}, ctx.Err()
		}
	}
	return record, err
}

func TestSignedOAuthMCPRecoveryFaults_UserGuardKeepsItsPointerGenerationAcrossInstances(t *testing.T) {
	now := time.Now().UTC()
	_, key, signedRegistry, base, preparer := signedCapabilityServiceWithRegistry(t, now)
	schedule := &userGuardReadScheduleStore{StateStore: base, entered: make(chan struct{}), release: make(chan struct{})}
	_, _, ordinaryRegistry, _, _ := signedCapabilityServiceWithStore(t, now, schedule)
	ordinary := signedCapabilityServiceForExisting(t, now, ordinaryRegistry, schedule, preparer, key)
	signed := signedCapabilityServiceForExisting(t, now, signedRegistry, base, preparer, key)
	id := identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}
	q := identity.Quadruple{Identity: id}
	ctx := verifiedUserCapabilityContext(t, id, true, true)
	prior, err := signedRegistry.SetRevision(ctx, q, testAgentID, agentcfg.ConfigScopeUser, agentcfg.ConfigPayload{}, agentcfg.SetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(schedule.release) }) }
	t.Cleanup(release)
	schedule.mu.Lock()
	schedule.armed = true
	schedule.mu.Unlock()
	ordinaryResult := make(chan error, 1)
	go func() {
		_, err := ordinary.UserSetRevision(ctx, prototypes.AgentConfigUserSetRevisionRequest{
			Identity: scope(), AgentID: testAgentID, Payload: prototypes.AgentConfigUserPayload{UserPrompt: "ordinary edit"},
		})
		ordinaryResult <- err
	}()
	select {
	case <-schedule.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("ordinary writer did not pause after its physical guard read")
	}
	connectErr := errors.New("signed physical connection unavailable")
	preparer.failPrepare = connectErr
	req := signedCapabilityRequest(t, key, now, "user-guard-generation", "aud")
	if _, err := signed.RegisterUserOAuthMCPCapability(ctx, userRegisterRequest(req)); !errors.Is(err, connectErr) {
		t.Fatalf("signed candidate did not reach the expected connect fault: %v", err)
	}
	physical := signedRegistry.(interface {
		PhysicalActive(context.Context, identity.Quadruple, string, agentcfg.ConfigScope) (agentcfg.Revision, bool, error)
	})
	candidate, set, err := physical.PhysicalActive(ctx, q, testAgentID, agentcfg.ConfigScopeUser)
	if err != nil || !set || candidate.RevisionID == prior.RevisionID || candidate.Payload.SignedOAuthMCPPair == nil {
		t.Fatalf("failed signed candidate fixture = %+v %t %v", candidate, set, err)
	}
	ops, _ := agentcfg.NewSignedOAuthMCPOperationStore(base)
	before, err := ops.LoadForPair(ctx, id.TenantID, candidate.Payload.SignedOAuthMCPPair)
	if err != nil {
		t.Fatal(err)
	}
	release()
	select {
	case err := <-ordinaryResult:
		if !errors.Is(err, agentcfg.ErrRevisionConflict) && !errors.Is(err, agentcfg.ErrSignedCapabilityPending) {
			t.Fatalf("ordinary writer adopted a newer candidate generation: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ordinary writer did not finish")
	}
	current, set, err := physical.PhysicalActive(ctx, q, testAgentID, agentcfg.ConfigScopeUser)
	if err != nil || !set || current.RevisionID != candidate.RevisionID {
		t.Fatalf("ordinary writer overwrote pending signed candidate: %+v %t %v", current, set, err)
	}
	after, err := ops.Load(ctx, before.ReplayKey)
	if err != nil || !reflect.DeepEqual(after, before) {
		t.Fatalf("ordinary write changed signed operation: %+v %v", after, err)
	}
}
