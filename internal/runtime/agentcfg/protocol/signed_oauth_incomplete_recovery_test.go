package protocol_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	agentcfgprotocol "github.com/hurtener/Harbor/internal/runtime/agentcfg/protocol"
	stateinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
)

func TestSignedOAuthMCPIncompleteRecovery_CorrectedRegistrationAndExactRemoval(t *testing.T) {
	for _, prior := range []bool{false, true} {
		for _, remove := range []bool{false, true} {
			name := "corrected-registration"
			if remove {
				name = "remove-re-register"
			}
			if prior {
				name += "-with-prior"
			}
			t.Run(name, func(t *testing.T) {
				ctx := context.Background()
				now := time.Now().UTC()
				svc, key, reg, store, preparer := signedCapabilityServiceWithRegistry(t, now)
				q := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}}
				var before agentcfg.Revision
				if prior {
					var err error
					before, err = reg.SetRevision(ctx, q, testAgentID, agentcfg.ConfigScopeAgent,
						agentcfg.ConfigPayload{Skills: &agentcfg.SkillsSelection{Names: []string{"history"}}}, agentcfg.SetOptions{})
					if err != nil {
						t.Fatal(err)
					}
				}
				connectErr := errors.New("connect: unexpected HTTP status 404")
				preparer.failPrepare = connectErr
				failedReq := signedCapabilityRequest(t, key, now, "incomplete-old", "old-audience")
				if _, err := svc.RegisterOAuthMCPCapability(ctx, failedReq); !errors.Is(err, connectErr) {
					t.Fatalf("connection failure = %v", err)
				}
				history, err := reg.ListRevisions(ctx, q, testAgentID, agentcfg.ConfigScopeAgent, 0)
				if err != nil || len(history) == 0 {
					t.Fatalf("failed candidate history: %+v %v", history, err)
				}
				candidate := history[0]
				ops, _ := agentcfg.NewSignedOAuthMCPOperationStore(store)
				old, err := ops.LoadForPair(ctx, q.TenantID, candidate.Payload.SignedOAuthMCPPair)
				if err != nil || old.Phase != agentcfg.SignedOAuthMCPPhaseRevisionCommitted {
					t.Fatalf("persisted failed candidate = %+v %v", old, err)
				}
				// Reconstruct the service over existing state: recovery must cover
				// receipts persisted by older versions, not just new failures.
				svc = signedCapabilityServiceForExisting(t, now, reg, store, preparer, key)
				preparer.failPrepare = nil
				removalReq := prototypes.AgentConfigRemoveOAuthMCPCapabilityRequest{
					Identity: scope(), AgentID: testAgentID, ProviderName: failedReq.ProviderName, ExpectedContentHash: candidate.ContentHash,
				}
				if remove {
					wrong := removalReq
					wrong.ExpectedContentHash = "wrong-generation"
					if _, err := svc.RemoveOAuthMCPCapability(ctx, wrong); !errors.Is(err, agentcfg.ErrRevisionConflict) {
						t.Fatalf("wrong candidate hash accepted: %v", err)
					}
					for range 2 {
						response, err := svc.RemoveOAuthMCPCapability(ctx, removalReq)
						if err != nil || response.OperationPhase != string(agentcfg.SignedOAuthMCPPhasePreparationRejected) {
							t.Fatalf("exact incomplete removal = %+v %v", response, err)
						}
					}
					active, set, err := reg.Active(ctx, q, testAgentID, agentcfg.ConfigScopeAgent)
					if err != nil || set != prior || (prior && active.RevisionID != before.RevisionID) {
						t.Fatalf("restored authority = %+v set=%t err=%v", active, set, err)
					}
				}
				url, sink, _ := agentcfg.CanonicalOAuthMCPURL("https://example.test/corrected/mcp")
				connection := failedReq.Connection
				connection.URL = url
				corrected := signedCapabilityRequestWithConnection(t, key, now, scope(), testAgentID, "incomplete-corrected", "corrected-audience", connection, sink)
				corrected.ExpectedContentHash = before.ContentHash
				if !prior {
					corrected.ExpectedContentHash = agentcfg.ExpectNoActiveRevision
				}
				response, err := svc.RegisterOAuthMCPCapability(ctx, corrected)
				if err != nil {
					t.Fatalf("corrected registration: %v", err)
				}
				if prior && response.Revision.ParentRevisionID != before.RevisionID {
					t.Fatalf("corrected registration lost prior siblings: %+v", response.Revision)
				}
				old, err = ops.Load(ctx, old.ReplayKey)
				if err != nil || old.Phase != agentcfg.SignedOAuthMCPPhasePreparationRejected {
					t.Fatalf("old lifetime not terminal: %+v %v", old, err)
				}
				if _, err := svc.RegisterOAuthMCPCapability(ctx, failedReq); !errors.Is(err, agentcfg.ErrSignedCapabilityReplay) {
					t.Fatalf("retired JTI replay = %v", err)
				}
				if _, err := svc.RemoveOAuthMCPCapability(ctx, removalReq); !errors.Is(err, agentcfg.ErrRevisionConflict) {
					t.Fatalf("stale incomplete removal after replacement = %v", err)
				}
				preparer.mu.Lock()
				activations, live := preparer.activations, len(preparer.live)
				preparer.mu.Unlock()
				if activations != 1 || live != 1 {
					t.Fatalf("publication = %d activations, %d live", activations, live)
				}
			})
		}
	}
}

func TestSignedOAuthMCPIncompleteRecovery_StalePreconditionPreservesOldCandidate(t *testing.T) {
	for _, prior := range []bool{false, true} {
		for _, checkpointLoss := range []bool{false, true} {
			name := "connect-failure"
			if checkpointLoss {
				name = "publication-checkpoint-loss"
			}
			if prior {
				name += "-with-prior"
			}
			t.Run(name, func(t *testing.T) {
				ctx := context.Background()
				now := time.Now().UTC()
				base, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
				if err != nil {
					t.Fatal(err)
				}
				injected := errors.New("preparation or publication checkpoint failure")
				store := &capabilityOperationPhaseStore{StateStore: base, phase: agentcfg.SignedOAuthMCPPhasePublished}
				if checkpointLoss {
					store.failErr = injected
				}
				svc, key, reg, _, preparer := signedCapabilityServiceWithStore(t, now, store)
				q := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}}
				if prior {
					if _, err := reg.SetRevision(ctx, q, testAgentID, agentcfg.ConfigScopeAgent, agentcfg.ConfigPayload{}, agentcfg.SetOptions{}); err != nil {
						t.Fatal(err)
					}
				}
				if !checkpointLoss {
					preparer.failPrepare = injected
				}
				oldReq := signedCapabilityRequest(t, key, now, "stale-precondition-old", "old-audience")
				if _, err := svc.RegisterOAuthMCPCapability(ctx, oldReq); !errors.Is(err, injected) {
					t.Fatalf("fixture failure = %v", err)
				}
				physical := reg.(interface {
					PhysicalActive(context.Context, identity.Quadruple, string, agentcfg.ConfigScope) (agentcfg.Revision, bool, error)
				})
				beforePhysical, beforeSet, err := physical.PhysicalActive(ctx, q, testAgentID, agentcfg.ConfigScopeAgent)
				if err != nil || !beforeSet {
					t.Fatalf("physical fixture = %+v %t %v", beforePhysical, beforeSet, err)
				}
				ops, _ := agentcfg.NewSignedOAuthMCPOperationStore(store)
				beforeOp, err := ops.LoadForPair(ctx, q.TenantID, beforePhysical.Payload.SignedOAuthMCPPair)
				if err != nil {
					t.Fatal(err)
				}
				fences, _ := agentcfg.NewSignedOAuthMCPActivationFenceStore(store)
				beforeFence, err := fences.LoadForOperation(ctx, q.TenantID, testAgentID, mustOperationKind(t, ops, beforeOp.ReplayKey))
				if err != nil {
					t.Fatal(err)
				}
				preparer.mu.Lock()
				beforeLive, beforeDetaches := len(preparer.live), preparer.detaches
				preparer.failPrepare = nil
				preparer.mu.Unlock()
				for _, hash := range []string{"wrong-content-hash", agentcfg.ExpectNoActiveRevision} {
					if hash == agentcfg.ExpectNoActiveRevision && !prior {
						continue // this is a valid precondition without prior authority
					}
					corrected := signedCapabilityRequest(t, key, now, "stale-precondition-corrected", "new-audience")
					corrected.ExpectedContentHash = hash
					if _, err := svc.RegisterOAuthMCPCapability(ctx, corrected); !errors.Is(err, agentcfg.ErrRevisionConflict) {
						t.Fatalf("stale precondition %q = %v", hash, err)
					}
					afterOp, err := ops.Load(ctx, beforeOp.ReplayKey)
					if err != nil || !reflect.DeepEqual(afterOp, beforeOp) {
						t.Fatalf("stale precondition changed receipt/epoch = %+v %v", afterOp, err)
					}
					afterFence, err := fences.LoadForOperation(ctx, q.TenantID, testAgentID, mustOperationKind(t, ops, beforeOp.ReplayKey))
					if err != nil || !reflect.DeepEqual(afterFence, beforeFence) {
						t.Fatalf("stale precondition changed activation fence = %+v %v", afterFence, err)
					}
					afterPhysical, set, err := physical.PhysicalActive(ctx, q, testAgentID, agentcfg.ConfigScopeAgent)
					if err != nil || !set || !reflect.DeepEqual(afterPhysical, beforePhysical) {
						t.Fatalf("stale precondition changed physical authority = %+v %t %v", afterPhysical, set, err)
					}
					preparer.mu.Lock()
					live, detaches := len(preparer.live), preparer.detaches
					preparer.mu.Unlock()
					if live != beforeLive || detaches != beforeDetaches {
						t.Fatalf("stale precondition changed local publication: live=%d detaches=%d", live, detaches)
					}
				}
			})
		}
	}
}

func TestSignedOAuthMCPIncompleteRecovery_PublicationCheckpointLossFencesAndDetachesOldEpoch(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	base, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatal(err)
	}
	checkpointErr := errors.New("publication checkpoint unavailable")
	store := &capabilityOperationPhaseStore{StateStore: base, phase: agentcfg.SignedOAuthMCPPhasePublished, failErr: checkpointErr}
	svc, key, _, _, preparer := signedCapabilityServiceWithStore(t, now, store)
	req := signedCapabilityRequest(t, key, now, "published-checkpoint-lost", "old-audience")
	if _, err := svc.RegisterOAuthMCPCapability(ctx, req); !errors.Is(err, checkpointErr) {
		t.Fatalf("checkpoint error = %v", err)
	}
	ops, _ := agentcfg.NewSignedOAuthMCPOperationStore(store)
	oldKey := agentcfg.SignedOAuthMCPReplayKey{TenantID: "t", TrustAnchorName: "broker", Issuer: "issuer", KeyID: "kid", JTI: "published-checkpoint-lost"}
	old, err := ops.Load(ctx, oldKey)
	if err != nil || old.Phase != agentcfg.SignedOAuthMCPPhaseRevisionCommitted {
		t.Fatalf("unknown publication checkpoint = %+v %v", old, err)
	}
	preparer.mu.Lock()
	live := len(preparer.live)
	preparer.mu.Unlock()
	if live != 1 {
		t.Fatal("fixture did not publish before checkpoint loss")
	}
	corrected := signedCapabilityRequest(t, key, now, "checkpoint-corrected", "new-audience")
	if _, err := svc.RegisterOAuthMCPCapability(ctx, corrected); err != nil {
		t.Fatalf("same-owner explicit replacement of incomplete checkpoint: %v", err)
	}
	if err := ops.AuthorizeSignedCapabilityUse(ctx, "t", mustOperationKind(t, ops, oldKey), old.PublisherEpoch, false); !errors.Is(err, agentcfg.ErrSignedCapabilityPending) {
		t.Fatalf("old epoch remained authorized = %v", err)
	}
	preparer.mu.Lock()
	activations, detaches, live := preparer.activations, preparer.detaches, len(preparer.live)
	preparer.mu.Unlock()
	if activations != 2 || detaches != 1 || live != 1 {
		t.Fatalf("replacement teardown = activations:%d detaches:%d live:%d", activations, detaches, live)
	}
}

func mustOperationKind(t *testing.T, ops *agentcfg.SignedOAuthMCPOperationStore, key agentcfg.SignedOAuthMCPReplayKey) string {
	t.Helper()
	kind, err := ops.Kind(key)
	if err != nil {
		t.Fatal(err)
	}
	return kind
}

func TestSignedOAuthMCPIncompleteRecovery_CorrectedOwnerFencesDelayedPublisher(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	left, key, reg, store, preparer := signedCapabilityServiceWithRegistry(t, now)
	right := signedCapabilityServiceForExisting(t, now, reg, store, preparer, key)
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	preparer.prepareBlockTenant = "t"
	preparer.prepareEntered = entered
	preparer.prepareRelease = release
	oldReq := signedCapabilityRequest(t, key, now, "delayed-old", "old-audience")
	oldResult := make(chan error, 1)
	go func() {
		_, err := left.RegisterOAuthMCPCapability(ctx, oldReq)
		oldResult <- err
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		close(release)
		t.Fatal("old private preparation did not start")
	}
	preparer.mu.Lock()
	preparer.prepareBlockTenant = ""
	preparer.mu.Unlock()
	corrected := signedCapabilityRequest(t, key, now, "delayed-corrected", "new-audience")
	response, err := right.RegisterOAuthMCPCapability(ctx, corrected)
	close(release)
	if err != nil {
		t.Fatalf("corrected concurrent registration = %v", err)
	}
	select {
	case err := <-oldResult:
		if err == nil {
			t.Fatal("stale delayed publisher succeeded")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stale publisher did not finish")
	}
	q := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}}
	active, set, err := reg.Active(ctx, q, testAgentID, agentcfg.ConfigScopeAgent)
	if err != nil || !set || active.RevisionID != response.Revision.RevisionID {
		t.Fatalf("stale publisher changed corrected authority = %+v %t %v", active, set, err)
	}
	preparer.mu.Lock()
	activations, live := preparer.activations, len(preparer.live)
	preparer.mu.Unlock()
	if activations != 1 || live != 1 {
		t.Fatalf("delayed publication = %d activations, %d live", activations, live)
	}
}

func TestSignedOAuthMCPIncompleteRecovery_ExpiredCandidateRemovalDisablesRenewal(t *testing.T) {
	for _, admitted := range []bool{false, true} {
		name := "completed-expiry"
		if admitted {
			name = "admitted-expiry"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			now := time.Now().UTC().Add(-3 * time.Hour)
			svc, key, reg, store, preparer := signedCapabilityServiceWithRegistry(t, now)
			preparer.failPrepare = errors.New("connect unavailable")
			req := signedCapabilityRequest(t, key, now, "expired-clear", "aud")
			if _, err := svc.RegisterOAuthMCPCapability(ctx, req); err == nil {
				t.Fatal("fixture unexpectedly registered")
			}
			q := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}}
			history, err := reg.ListRevisions(ctx, q, testAgentID, agentcfg.ConfigScopeAgent, 0)
			if err != nil || len(history) != 1 {
				t.Fatalf("candidate history = %+v %v", history, err)
			}
			candidate := history[0]
			ops, _ := agentcfg.NewSignedOAuthMCPOperationStore(store)
			op, err := ops.LoadForPair(ctx, q.TenantID, candidate.Payload.SignedOAuthMCPPair)
			if err != nil {
				t.Fatal(err)
			}
			if admitted {
				if _, err := ops.AdmitExpiry(ctx, op, candidate.RevisionID, time.Now().UTC()); err != nil {
					t.Fatal(err)
				}
			} else {
				reconciler, err := agentcfgprotocol.NewSignedOAuthMCPReconciler(reg, store, preparer, preparer, capabilityInstaller{})
				if err != nil {
					t.Fatal(err)
				}
				if err := reconciler.ReconcileSignedOAuthMCPCapability(ctx, q, testAgentID); err != nil {
					t.Fatal(err)
				}
			}
			preparer.failPrepare = nil
			svc = signedCapabilityServiceForExisting(t, time.Now().UTC(), reg, store, preparer, key)
			response, err := svc.RemoveOAuthMCPCapability(ctx, prototypes.AgentConfigRemoveOAuthMCPCapabilityRequest{
				Identity: scope(), AgentID: testAgentID, ProviderName: req.ProviderName, ExpectedContentHash: candidate.ContentHash,
			})
			if err != nil || response.OperationPhase != string(agentcfg.SignedOAuthMCPPhasePreparationRejected) {
				t.Fatalf("expired removal = %+v %v", response, err)
			}
			renewed := signedCapabilityRequest(t, key, time.Now().UTC(), "expired-clear", "aud")
			if _, err := svc.RegisterOAuthMCPCapability(ctx, renewed); !errors.Is(err, agentcfg.ErrSignedCapabilityReplay) {
				t.Fatalf("explicitly removed expiry renewed = %v", err)
			}
		})
	}
}

func TestSignedOAuthMCPIncompleteRecovery_UserOwnerSurvivesSessionChange(t *testing.T) {
	now := time.Now().UTC()
	svc, key, reg, store, preparer := signedCapabilityServiceWithRegistry(t, now)
	id := identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}
	preparer.failPrepare = errors.New("connect unavailable")
	req := signedCapabilityRequest(t, key, now, "user-incomplete", "old-audience")
	if _, err := svc.RegisterUserOAuthMCPCapability(verifiedUserCapabilityContext(t, id, true, true), userRegisterRequest(req)); err == nil {
		t.Fatal("fixture unexpectedly registered")
	}
	preparer.failPrepare = nil
	foreign := identity.Identity{TenantID: "t", UserID: "foreign", SessionID: "foreign-session"}
	foreignScope := prototypes.IdentityScope{Tenant: foreign.TenantID, User: foreign.UserID, Session: foreign.SessionID}
	foreignReq := signedCapabilityRequestFor(t, key, now, foreignScope, testAgentID, "foreign-live", "aud", req.Connection.Name)
	if _, err := svc.RegisterUserOAuthMCPCapability(verifiedUserCapabilityContext(t, foreign, true, true), userRegisterRequest(foreignReq)); err != nil {
		t.Fatalf("independent user registration = %v", err)
	}
	id.SessionID = "new-session"
	ctx := verifiedUserCapabilityContext(t, id, true, true)
	svc = signedCapabilityServiceForExisting(t, now, reg, store, preparer, key)
	correctedScope := prototypes.IdentityScope{Tenant: id.TenantID, User: id.UserID, Session: id.SessionID}
	corrected := signedCapabilityRequestFor(t, key, now, correctedScope, testAgentID, "user-corrected", "corrected-audience", req.Connection.Name)
	if _, err := svc.RegisterUserOAuthMCPCapability(ctx, userRegisterRequest(corrected)); err != nil {
		t.Fatalf("user correction across sessions = %v", err)
	}
	preparer.mu.Lock()
	live := len(preparer.userLive)
	preparer.mu.Unlock()
	if live != 2 {
		t.Fatalf("cross-user physical attachments = %d, want 2", live)
	}
	active, set, err := reg.Active(context.Background(), identity.Quadruple{Identity: foreign}, testAgentID, agentcfg.ConfigScopeUser)
	if err != nil || !set || active.Payload.SignedOAuthMCPPair == nil || active.Payload.SignedOAuthMCPPair.Audience != "aud" {
		t.Fatalf("correction changed foreign user = %+v %t %v", active, set, err)
	}
}

func TestSignedOAuthMCPIncompleteRecovery_SameJTIResumesAndForeignOwnerCannotRetire(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	svc, key, reg, store, preparer := signedCapabilityServiceWithRegistry(t, now)
	connectErr := errors.New("connect failed")
	preparer.failPrepare = connectErr
	req := signedCapabilityRequest(t, key, now, "recover-same-jti", "aud")
	if _, err := svc.RegisterOAuthMCPCapability(ctx, req); !errors.Is(err, connectErr) {
		t.Fatal(err)
	}
	foreign := req.Identity
	foreign.User = "foreign-user"
	foreign.Session = "foreign-session"
	foreignReq := signedCapabilityRequestFor(t, key, now, foreign, testAgentID, "foreign-attempt", "foreign-aud", req.Connection.Name)
	if _, err := svc.RegisterOAuthMCPCapability(ctx, foreignReq); !errors.Is(err, agentcfg.ErrSignedCapabilityPending) || !strings.Contains(err.Error(), "another owner") {
		t.Fatalf("foreign pending replacement = %v", err)
	}
	q := identity.Quadruple{Identity: identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}}
	history, err := reg.ListRevisions(ctx, q, testAgentID, agentcfg.ConfigScopeAgent, 0)
	if err != nil || len(history) != 1 {
		t.Fatalf("foreign write changed history: %+v %v", history, err)
	}
	if _, err := svc.RemoveOAuthMCPCapability(ctx, prototypes.AgentConfigRemoveOAuthMCPCapabilityRequest{
		Identity: foreign, AgentID: testAgentID, ProviderName: req.ProviderName, ExpectedContentHash: history[0].ContentHash,
	}); !errors.Is(err, agentcfg.ErrSignedCapabilityReplay) {
		t.Fatalf("foreign incomplete removal = %v", err)
	}
	preparer.failPrepare = nil
	svc = signedCapabilityServiceForExisting(t, now, reg, store, preparer, key)
	for range 2 {
		if _, err := svc.RegisterOAuthMCPCapability(ctx, req); err != nil {
			t.Fatalf("same-JTI retry = %v", err)
		}
	}
	for _, replacement := range []prototypes.AgentConfigRegisterOAuthMCPCapabilityRequest{
		foreignReq, signedCapabilityRequest(t, key, now, "owner-new-live-jti", "changed-audience"),
	} {
		if _, err := svc.RegisterOAuthMCPCapability(ctx, replacement); !errors.Is(err, agentcfgprotocol.ErrSignedCapabilityPairExists) && !errors.Is(err, agentcfg.ErrSignedCapabilityPending) {
			t.Fatalf("published lifetime replacement = %v", err)
		}
	}
	preparer.mu.Lock()
	activations, detaches, live := preparer.activations, preparer.detaches, len(preparer.live)
	preparer.mu.Unlock()
	if activations != 1 || detaches != 0 || live != 1 {
		t.Fatalf("same-JTI/live-owner fences: activations=%d detaches=%d live=%d", activations, detaches, live)
	}
}
