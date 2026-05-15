package auth_test

import (
	"context"
	"testing"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/state"
	"github.com/hurtener/Harbor/internal/state/drivers/inmem"
	"github.com/hurtener/Harbor/internal/tools/auth"
	"github.com/hurtener/Harbor/internal/tools/auth/conformancetest"
)

// TestConformance_InMemDriver runs the cross-driver TokenStore +
// Sealer conformance suite against the in-mem state.StateStore
// driver. The SQLite + Postgres legs run in
// test/integration/phase30_tool_oauth_test.go where the build tag
// gates the live database availability.
func TestConformance_InMemDriver(t *testing.T) {
	t.Parallel()
	conformancetest.Run(t, func(t *testing.T) (auth.TokenStore, state.StateStore, auth.Sealer) {
		t.Helper()
		raw, err := inmem.New(config.StateConfig{})
		if err != nil {
			t.Fatalf("inmem.New: %v", err)
		}
		t.Cleanup(func() { _ = raw.Close(context.Background()) })

		// 32 known bytes — dummy KEK for conformance, never a real
		// credential per §7 rule 2.
		kek := make([]byte, auth.KEKSizeBytes)
		for i := range kek {
			kek[i] = byte(i*7 + 3)
		}
		sealer, err := auth.NewAESGCMSealer(kek)
		if err != nil {
			t.Fatalf("NewAESGCMSealer: %v", err)
		}
		store, err := auth.NewTokenStore(raw, sealer)
		if err != nil {
			t.Fatalf("NewTokenStore: %v", err)
		}
		return store, raw, sealer
	})
}
