package serve

import (
	"context"
	"log/slog"
	"testing"

	"github.com/hurtener/Harbor/internal/governance"
	runsprotocol "github.com/hurtener/Harbor/internal/runtime/runs/protocol"
)

// TestResolveLLMOverrides_ExtraInstructionsAuthoritySplit proves the
// run loop's run-start resolution — the ONE production path a real run takes —
// carries the authority split, not just the composer function in isolation.
// The tenant-wide block stays trusted while the session value becomes
// one-run user personalization and cannot clear the tenant's.
//
// This closes the "the composition is reached through resolveLLMOverrides"
// claim mechanically: a split that lived only in a helper nobody wired would
// leave this test red.
func TestResolveLLMOverrides_ExtraInstructionsAuthoritySplit(t *testing.T) {
	const (
		tenantBlock  = "TENANT-BLOCK: cite every source."
		sessionBlock = "SESSION-BLOCK: answer in the imperative mood."
	)

	t.Run("both producers remain separate", func(t *testing.T) {
		q := validQuadForOv()
		store := runsprotocol.NewStore()
		store.Set(q.Identity, runsprotocol.PendingOverride{ExtraInstructions: ovStr(sessionBlock)})
		d := &RunLoopDriver{
			logger:           slog.Default(),
			sessionOverrides: store,
			tenantOverrides: fakeTenantOverrides{set: true, spec: governance.TenantOverrideSpec{
				ExtraInstructions: ovStr(tenantBlock),
			}},
		}
		ov, err := d.resolveLLMOverrides(context.Background(), d.agentConfigID, q)
		if err != nil {
			t.Fatalf("resolveLLMOverrides: %v", err)
		}
		if ov == nil || ov.ExtraInstructions == nil || ov.UserPersonalization == nil {
			t.Fatal("nil guidance tier; want trusted tenant plus user personalization")
		}
		if *ov.ExtraInstructions != tenantBlock || *ov.UserPersonalization != sessionBlock {
			t.Fatalf("trusted=%q personalization=%q, want %q / %q",
				*ov.ExtraInstructions, *ov.UserPersonalization, tenantBlock, sessionBlock)
		}
	})

	t.Run("a session set cannot clear the admin-set tenant block", func(t *testing.T) {
		q := validQuadForOv()
		store := runsprotocol.NewStore()
		store.Set(q.Identity, runsprotocol.PendingOverride{ExtraInstructions: ovStr("")})
		d := &RunLoopDriver{
			logger:           slog.Default(),
			sessionOverrides: store,
			tenantOverrides: fakeTenantOverrides{set: true, spec: governance.TenantOverrideSpec{
				ExtraInstructions: ovStr(tenantBlock),
			}},
		}
		ov, err := d.resolveLLMOverrides(context.Background(), d.agentConfigID, q)
		if err != nil {
			t.Fatalf("resolveLLMOverrides: %v", err)
		}
		if ov == nil || ov.ExtraInstructions == nil || *ov.ExtraInstructions != tenantBlock {
			t.Fatalf("ExtraInstructions = %v, want the tenant block intact (there is no run-level clear)", ov.ExtraInstructions)
		}
	})

	t.Run("one-shot: the session block is gone on the next run, the tenant block remains", func(t *testing.T) {
		q := validQuadForOv()
		store := runsprotocol.NewStore()
		store.Set(q.Identity, runsprotocol.PendingOverride{ExtraInstructions: ovStr(sessionBlock)})
		d := &RunLoopDriver{
			logger:           slog.Default(),
			sessionOverrides: store,
			tenantOverrides: fakeTenantOverrides{set: true, spec: governance.TenantOverrideSpec{
				ExtraInstructions: ovStr(tenantBlock),
			}},
		}
		ov1, err := d.resolveLLMOverrides(context.Background(), d.agentConfigID, q)
		if err != nil {
			t.Fatalf("resolveLLMOverrides 1: %v", err)
		}
		if ov1.UserPersonalization == nil || *ov1.UserPersonalization != sessionBlock {
			t.Fatalf("first resolve personalization = %v, want %q", ov1.UserPersonalization, sessionBlock)
		}
		ov2, err := d.resolveLLMOverrides(context.Background(), d.agentConfigID, q)
		if err != nil {
			t.Fatalf("resolveLLMOverrides 2: %v", err)
		}
		if ov2 == nil || ov2.ExtraInstructions == nil || *ov2.ExtraInstructions != tenantBlock {
			t.Fatalf("second resolve ExtraInstructions = %v, want the tenant block only", ov2.ExtraInstructions)
		}
		if ov2.UserPersonalization != nil {
			t.Fatalf("second resolve personalization = %q, want nil after one-shot consumption", *ov2.UserPersonalization)
		}
	})
}
