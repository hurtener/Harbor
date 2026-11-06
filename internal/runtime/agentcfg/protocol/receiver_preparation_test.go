package protocol_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/agentcfg"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	agentcfgprotocol "github.com/hurtener/Harbor/internal/runtime/agentcfg/protocol"
)

func TestReceiverPreparation_UnsignedPersistenceDoorsReject(t *testing.T) {
	for _, door := range []string{"add", "set"} {
		t.Run(door, func(t *testing.T) {
			svc, _ := newInjectionHarness(t, true)
			injection := &prototypes.AgentConfigMCPCredentialInjectionDescriptor{
				Provider: "broker", Form: "header", Header: "X-Receiver-Api-Key", AuthenticatePreparation: true,
			}
			var err error
			if door == "add" {
				_, err = svc.AddMCPConnection(context.Background(), prototypes.AgentConfigAddMCPConnectionRequest{
					Identity: scope(), AgentID: testAgentID, Connection: injConn(injection),
				})
			} else {
				_, err = svc.SetRevision(context.Background(), setRevInjConn(injection))
			}
			if !errors.Is(err, agentcfgprotocol.ErrInvalidConnection) {
				t.Fatalf("unsigned preparation authority accepted: %v", err)
			}
			got, err := svc.Get(context.Background(), prototypes.AgentConfigGetRequest{Identity: scope(), AgentID: testAgentID})
			if err != nil || got.Set {
				t.Fatalf("rejected preparation persisted: set=%v err=%v", got.Set, err)
			}
		})
	}
}

func TestReceiverPreparation_SignedFlagTamperingRejected(t *testing.T) {
	for _, signed := range []bool{false, true} {
		t.Run(map[bool]string{false: "unsigned-enable", true: "signed-disable"}[signed], func(t *testing.T) {
			now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
			svc, key, _, _ := signedCapabilityService(t, now)
			connection := prototypes.SignedOAuthMCPConnectionDescriptor{
				Name: "receiver", URL: "https://receiver.example.test:443/mcp",
				Injection: &prototypes.AgentConfigMCPCredentialInjectionDescriptor{Provider: "provider", Form: "header", Header: "X-Receiver-Api-Key", AuthenticatePreparation: signed},
			}
			req := signedCapabilityRequestWithConnection(t, key, now, scope(), testAgentID, "preparation-tamper", "receiver", connection, "https://receiver.example.test:443")
			req.Connection.Injection.AuthenticatePreparation = !signed
			if _, err := svc.RegisterOAuthMCPCapability(context.Background(), req); !errors.Is(err, agentcfg.ErrSignedCapabilityBinding) {
				t.Fatalf("preparation tamper error = %v", err)
			}
		})
	}
}

func TestReceiverPreparation_SetRevisionCannotFlipImmutableSignedPair(t *testing.T) {
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	svc, key, _, _ := signedCapabilityService(t, now)
	connection := prototypes.SignedOAuthMCPConnectionDescriptor{
		Name: "receiver", URL: "https://receiver.example.test:443/mcp",
		Injection: &prototypes.AgentConfigMCPCredentialInjectionDescriptor{Provider: "provider", Form: "header", Header: "X-Receiver-Api-Key"},
	}
	req := signedCapabilityRequestWithConnection(t, key, now, scope(), testAgentID, "preparation-readonly", "receiver", connection, "https://receiver.example.test:443")
	if _, err := svc.RegisterOAuthMCPCapability(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	got, err := svc.Get(context.Background(), prototypes.AgentConfigGetRequest{Identity: scope(), AgentID: testAgentID})
	if err != nil || !got.Set || got.Revision == nil {
		t.Fatalf("read registered pair: %+v %v", got, err)
	}
	payload := got.Revision.Payload
	if payload.SignedOAuthMCPPair != nil {
		payload.SignedOAuthMCPPair.Connection.Injection.AuthenticatePreparation = true
	} else {
		pair, ok := payload.SignedOAuthMCPPairs["provider"]
		if !ok || pair.Connection.Injection == nil {
			t.Fatal("missing registered pair")
		}
		pair.Connection.Injection.AuthenticatePreparation = true
		payload.SignedOAuthMCPPairs["provider"] = pair
	}
	if _, err := svc.SetRevision(context.Background(), prototypes.AgentConfigSetRevisionRequest{Identity: scope(), AgentID: testAgentID, Payload: payload}); !errors.Is(err, agentcfgprotocol.ErrSignedCapabilityPairReadOnly) {
		t.Fatalf("unsigned revision changed signed preparation flag: %v", err)
	}
}
