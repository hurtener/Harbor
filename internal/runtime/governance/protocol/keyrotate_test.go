package protocol_test

import (
	"context"
	"errors"
	"testing"
	"time"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	_ "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/governance"
	"github.com/hurtener/Harbor/internal/llm"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	governanceprotocol "github.com/hurtener/Harbor/internal/runtime/governance/protocol"
)

// fakeRotator records the last rotate and returns canned results / errors.
type fakeRotator struct {
	gotProvider string
	gotKey      string
	retProvider string
	retFP       string
	err         error
}

func (f *fakeRotator) RotateKey(provider, newKey string) (string, string, error) {
	f.gotProvider = provider
	f.gotKey = newKey
	if f.err != nil {
		return "", "", f.err
	}
	return f.retProvider, f.retFP, nil
}

func TestKeyRotateService_NilRotator_FailLoud(t *testing.T) {
	if _, err := governanceprotocol.NewKeyRotateService(nil); !errors.Is(err, governanceprotocol.ErrKeyRotateMisconfigured) {
		t.Errorf("nil rotator: want ErrKeyRotateMisconfigured, got %v", err)
	}
}

func TestKeyRotateService_IdentityRequired(t *testing.T) {
	svc, err := governanceprotocol.NewKeyRotateService(&fakeRotator{})
	if err != nil {
		t.Fatalf("NewKeyRotateService: %v", err)
	}
	_, err = svc.RotateKey(context.Background(), prototypes.GovernanceRotateKeyRequest{Key: "sk-x"})
	if !errors.Is(err, governanceprotocol.ErrIdentityRequired) {
		t.Errorf("want ErrIdentityRequired, got %v", err)
	}
}

func TestKeyRotateService_ErrorPropagates(t *testing.T) {
	svc, err := governanceprotocol.NewKeyRotateService(&fakeRotator{err: llm.ErrEmptyKey})
	if err != nil {
		t.Fatalf("NewKeyRotateService: %v", err)
	}
	_, err = svc.RotateKey(context.Background(), prototypes.GovernanceRotateKeyRequest{
		Identity: prototypes.IdentityScope{Tenant: "t", User: "u", Session: "s"}, Key: "",
	})
	if !errors.Is(err, llm.ErrEmptyKey) {
		t.Errorf("want ErrEmptyKey propagated, got %v", err)
	}
}

// TestKeyRotateService_Success_EmitsSecretFreeEvent asserts a successful
// rotate returns the provider + fingerprint (NO key) and emits a
// `governance.key_rotated` event whose payload carries the fingerprint,
// never the key.
func TestKeyRotateService_Success_EmitsSecretFreeEvent(t *testing.T) {
	bus, err := events.Open(context.Background(), config.EventsConfig{
		Driver: "inmem", MaxSubscribersPerSession: 16, SubscriberBufferSize: 64,
		IdleTimeout: 60 * time.Second, DropWindow: time.Second,
	}, auditpatterns.New())
	if err != nil {
		t.Fatalf("events.Open: %v", err)
	}
	defer bus.Close(context.Background())

	const secret = "sk-super-secret-value"
	fp := llm.Fingerprint(secret)
	rot := &fakeRotator{retProvider: "openrouter", retFP: fp}
	svc, err := governanceprotocol.NewKeyRotateService(rot,
		governanceprotocol.WithKeyRotateBus(bus),
		governanceprotocol.WithKeyRotateRedactor(auditpatterns.New()))
	if err != nil {
		t.Fatalf("NewKeyRotateService: %v", err)
	}

	sub, err := bus.Subscribe(context.Background(), events.Filter{
		Tenant: "t", User: "admin", Session: "s",
		Types: []events.EventType{governance.EventTypeKeyRotated},
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Cancel()

	resp, err := svc.RotateKey(context.Background(), prototypes.GovernanceRotateKeyRequest{
		Identity: prototypes.IdentityScope{Tenant: "t", User: "admin", Session: "s"},
		Provider: "openrouter",
		Key:      secret,
	})
	if err != nil {
		t.Fatalf("RotateKey: %v", err)
	}
	if rot.gotKey != secret {
		t.Errorf("rotator did not receive the key")
	}
	if resp.Provider != "openrouter" || resp.Fingerprint != fp {
		t.Errorf("resp = %+v, want provider openrouter + fingerprint %s", resp, fp)
	}
	if resp.Fingerprint == secret || resp.Provider == secret {
		t.Fatal("response leaked the key")
	}

	select {
	case ev := <-sub.Events():
		p, ok := ev.Payload.(governance.KeyRotatedPayload)
		if !ok {
			t.Fatalf("payload type %T", ev.Payload)
		}
		if p.Provider != "openrouter" || p.Fingerprint != fp {
			t.Errorf("payload = %+v", p)
		}
		if p.Fingerprint == secret {
			t.Fatal("audit payload leaked the key")
		}
		if p.Actor.UserID != "admin" {
			t.Errorf("actor = %q, want admin", p.Actor.UserID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no key_rotated event observed")
	}
}
