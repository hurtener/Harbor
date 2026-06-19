// Package integration_test — cross-subsystem E2E for Console-driven LLM
// key rotation (the RFC §6.15 / D-019 seam). Real drivers on the seam: a
// real inmem EventBus + a real audit Redactor back the
// governance/protocol KeyRotateService; a real llm.LiveKey + ProviderKey
// Rotator are the swap artifact bifrost reads through. The test proves the
// rotate path end-to-end, the secret-free audit event, secret hygiene (the
// key never reaches a log line), the immediate swap, and concurrency.
//
// The bifrost account's per-call read of the SAME holder after a rotate is
// proven in-package (internal/llm/drivers/bifrost account test); here the
// seam under test is the real service → real holder + the real bus audit.
package integration_test

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
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

const rotationSecret = "sk-live-rotation-SECRET-9f3a"

func keyRotateBus(t *testing.T) events.EventBus {
	t.Helper()
	bus, err := events.Open(context.Background(), config.EventsConfig{
		Driver: "inmem", MaxSubscribersPerSession: 16, SubscriberBufferSize: 64,
		IdleTimeout: 60 * time.Second, DropWindow: time.Second,
	}, auditpatterns.New())
	if err != nil {
		t.Fatalf("events.Open: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })
	return bus
}

// TestE2E_KeyRotation_SwapAndSecretFreeAudit proves a rotate swaps the
// shared holder (the next read uses the new key), emits a secret-free
// `governance.key_rotated` event, and never writes the key to a log.
func TestE2E_KeyRotation_SwapAndSecretFreeAudit(t *testing.T) {
	bus := keyRotateBus(t)
	holder := llm.NewLiveKey()
	holder.Init("sk-boot")
	rotator := llm.NewProviderKeyRotator("openrouter", holder)

	// A logger writing into a buffer so we can assert the key never logs.
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	svc, err := governanceprotocol.NewKeyRotateService(rotator,
		governanceprotocol.WithKeyRotateBus(bus),
		governanceprotocol.WithKeyRotateRedactor(auditpatterns.New()),
		governanceprotocol.WithKeyRotateLogger(logger))
	if err != nil {
		t.Fatalf("NewKeyRotateService: %v", err)
	}

	sub, err := bus.Subscribe(context.Background(), events.Filter{
		Tenant: "dev", User: "admin", Session: "s",
		Types: []events.EventType{governance.EventTypeKeyRotated},
	})
	if err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	defer sub.Cancel()

	resp, err := svc.RotateKey(context.Background(), prototypes.GovernanceRotateKeyRequest{
		Identity: prototypes.IdentityScope{Tenant: "dev", User: "admin", Session: "s"},
		Provider: "openrouter",
		Key:      rotationSecret,
	})
	if err != nil {
		t.Fatalf("RotateKey: %v", err)
	}

	// Immediate swap: the next read returns the new key.
	if holder.Get() != rotationSecret {
		t.Errorf("holder not swapped: %q", holder.Get())
	}
	// Response carries a fingerprint, never the key.
	if resp.Fingerprint != llm.Fingerprint(rotationSecret) {
		t.Errorf("response fingerprint mismatch")
	}
	if strings.Contains(resp.Fingerprint, rotationSecret) || resp.Provider == rotationSecret {
		t.Fatal("response leaked the secret")
	}

	// Audit event: secret-free.
	select {
	case ev := <-sub.Events():
		p, ok := ev.Payload.(governance.KeyRotatedPayload)
		if !ok {
			t.Fatalf("payload type %T", ev.Payload)
		}
		if p.Provider != "openrouter" || p.Fingerprint != llm.Fingerprint(rotationSecret) {
			t.Errorf("payload = %+v", p)
		}
		if strings.Contains(p.Fingerprint, rotationSecret) {
			t.Fatal("audit payload leaked the secret")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no key_rotated event observed")
	}

	// Secret hygiene: the key value appears in NO log line.
	if strings.Contains(logBuf.String(), rotationSecret) {
		t.Fatalf("the rotation secret leaked into the log:\n%s", logBuf.String())
	}
}

// TestE2E_KeyRotation_Concurrency runs N≥10 concurrent rotations + reads
// against one shared holder via the service under -race, asserting every
// read returns a valid key from the rotated set and no race fires.
func TestE2E_KeyRotation_Concurrency(t *testing.T) {
	bus := keyRotateBus(t)
	holder := llm.NewLiveKey()
	holder.Init("k0")
	rotator := llm.NewProviderKeyRotator("p", holder)
	svc, err := governanceprotocol.NewKeyRotateService(rotator, governanceprotocol.WithKeyRotateBus(bus))
	if err != nil {
		t.Fatalf("NewKeyRotateService: %v", err)
	}

	const n = 16
	allowed := map[string]struct{}{"k0": {}, "kA": {}, "kB": {}}
	var wg sync.WaitGroup
	wg.Add(n * 2)
	for i := range n {
		go func(i int) {
			defer wg.Done()
			key := "kA"
			if i%2 == 0 {
				key = "kB"
			}
			_, err := svc.RotateKey(context.Background(), prototypes.GovernanceRotateKeyRequest{
				Identity: prototypes.IdentityScope{Tenant: "t", User: "u", Session: "s"},
				Provider: "p", Key: key,
			})
			if err != nil {
				t.Errorf("rotate %d: %v", i, err)
			}
		}(i)
		go func() {
			defer wg.Done()
			if _, ok := allowed[holder.Get()]; !ok {
				t.Errorf("torn read: %q", holder.Get())
			}
		}()
	}
	wg.Wait()
}
