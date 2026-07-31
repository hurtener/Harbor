package serve

import (
	"context"
	"log/slog"
	"testing"

	"github.com/hurtener/Harbor/internal/governance"
	runsprotocol "github.com/hurtener/Harbor/internal/runtime/runs/protocol"
)

// TestResolveLLMOverrides_ExtraInstructionsJoin_TenantThenSession proves the
// run loop's run-start resolution — the ONE production path a real run takes —
// carries the additive-guidance JOIN, not just the composer function in
// isolation. The tenant-wide block comes first, the session block is appended
// below it, and the session cannot clear the tenant's.
//
// This closes the "the composition is reached through resolveLLMOverrides"
// claim mechanically: a join that lived only in a helper nobody wired would
// leave this test red.
func TestResolveLLMOverrides_ExtraInstructionsJoin_TenantThenSession(t *testing.T) {
	const (
		tenantBlock  = "TENANT-BLOCK: cite every source."
		sessionBlock = "SESSION-BLOCK: answer in the imperative mood."
	)

	t.Run("both producers: tenant first, session below", func(t *testing.T) {
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
		want := tenantBlock + "\n\n" + sessionBlock
		if ov == nil || ov.ExtraInstructions == nil {
			t.Fatal("nil additive guidance; want the two producers joined")
		}
		if *ov.ExtraInstructions != want {
			t.Fatalf("ExtraInstructions =\n%q\nwant\n%q", *ov.ExtraInstructions, want)
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
		if _, err := d.resolveLLMOverrides(context.Background(), d.agentConfigID, q); err != nil {
			t.Fatalf("resolveLLMOverrides 1: %v", err)
		}
		ov2, err := d.resolveLLMOverrides(context.Background(), d.agentConfigID, q)
		if err != nil {
			t.Fatalf("resolveLLMOverrides 2: %v", err)
		}
		if ov2 == nil || ov2.ExtraInstructions == nil || *ov2.ExtraInstructions != tenantBlock {
			t.Fatalf("second resolve ExtraInstructions = %v, want the tenant block only", ov2.ExtraInstructions)
		}
	})
}
