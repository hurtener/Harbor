package protocol_test

import (
	"context"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/agentcfg"
	"github.com/hurtener/Harbor/internal/identity"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	agentcfgprotocol "github.com/hurtener/Harbor/internal/runtime/agentcfg/protocol"
	toolauth "github.com/hurtener/Harbor/internal/tools/auth"
)

type scopeRecordingInstaller struct {
	capabilityInstaller
	bindings []toolauth.SignedCapabilityExchangeBinding
}

func (i *scopeRecordingInstaller) PrepareSignedCapabilityProvider(_ context.Context, _ string, binding toolauth.SignedCapabilityExchangeBinding, _ []string) (agentcfgprotocol.PreparedOAuthProvider, error) {
	i.bindings = append(i.bindings, binding)
	return capabilityPreparedProvider{}, nil
}

func TestSignedCapability_RegistrationAndRestartDeriveScope(t *testing.T) {
	for _, scope := range []agentcfg.ConfigScope{agentcfg.ConfigScopeAgent, agentcfg.ConfigScopeUser} {
		scopeName := "agent"
		if scope == agentcfg.ConfigScopeUser {
			scopeName = "user"
		}
		t.Run(scopeName, func(t *testing.T) {
			now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
			_, key, reg, st, preparer := signedCapabilityServiceWithRegistry(t, now)
			installer := &scopeRecordingInstaller{}
			svc, err := agentcfgprotocol.NewService(reg,
				agentcfgprotocol.WithClock(func() time.Time { return now }),
				agentcfgprotocol.WithConnectionPreparer(preparer),
				agentcfgprotocol.WithConnectionDetacher(preparer),
				agentcfgprotocol.WithProviderInstaller(installer),
				agentcfgprotocol.WithSignedOAuthMCPOperationState(st),
				agentcfgprotocol.WithSignedOAuthMCPCapabilityAuthorities(map[string]agentcfgprotocol.SignedOAuthMCPCapabilityAuthority{
					"broker": {Broker: "broker", Issuer: "issuer", Keys: capabilityKeySet{key: &key.PublicKey}, ScopeCeiling: []string{"read"}, MaxAuthorityLifetime: time.Hour},
				}),
			)
			if err != nil {
				t.Fatal(err)
			}
			registrar := identity.Identity{TenantID: "t", UserID: "service-user", SessionID: "registration-session"}
			req := signedCapabilityRequestFor(t, key, now, prototypes.IdentityScope{Tenant: registrar.TenantID, User: registrar.UserID, Session: registrar.SessionID}, testAgentID, "scope-jti", "aud", "cap")
			if scope == agentcfg.ConfigScopeUser {
				_, err = svc.RegisterUserOAuthMCPCapability(verifiedUserCapabilityContext(t, registrar, true, true), userRegisterRequest(req))
			} else {
				_, err = svc.RegisterOAuthMCPCapability(context.Background(), req)
			}
			if err != nil {
				t.Fatalf("register: %v", err)
			}
			if len(installer.bindings) != 1 {
				t.Fatalf("registration bindings = %d", len(installer.bindings))
			}
			// A new preparer has no live attachment. Reconcile must derive
			// the same local scope from durable authority after restart.
			restartPreparer := &capabilityPreparer{}
			restarted, err := agentcfgprotocol.NewSignedOAuthMCPReconciler(reg, st, restartPreparer, restartPreparer, installer)
			if err != nil {
				t.Fatal(err)
			}
			caller := registrar
			caller.SessionID = "later-session"
			if scope == agentcfg.ConfigScopeAgent {
				caller.UserID = "end-user"
			}
			if err := restarted.ReconcileSignedOAuthMCPCapabilityForScope(context.Background(), identity.Quadruple{Identity: caller}, testAgentID, scope); err != nil {
				t.Fatalf("restart reconcile: %v", err)
			}
			if len(installer.bindings) != 2 {
				t.Fatalf("registration and restart bindings = %d, want 2", len(installer.bindings))
			}
			for _, binding := range installer.bindings {
				if binding.AgentScoped != (scope == agentcfg.ConfigScopeAgent) || binding.UserID != registrar.UserID {
					t.Fatalf("binding lost trusted scope or registrar: %+v", binding)
				}
			}
			if scope == agentcfg.ConfigScopeAgent && installer.bindings[0].SessionID != installer.bindings[1].SessionID {
				t.Fatal("shared actor registration session changed on restart")
			}
		})
	}
}
