package postgres

import (
	"context"
	"database/sql"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/state"
)

func TestConditionalAdvisoryLockIDs_SortsActualKeysAndDeduplicatesCollisions(t *testing.T) {
	q := identity.Quadruple{Identity: identity.Identity{TenantID: "tenant", UserID: "user", SessionID: "session"}}
	first := state.SlotExpectation{Identity: q, Kind: "alpha"}
	second := state.SlotExpectation{Identity: q, Kind: "bravo"}
	third := state.SlotExpectation{Identity: q, Kind: "charlie"}
	lockIDs := map[string]int64{
		postgresSlotKey(first):  41,
		postgresSlotKey(second): -7,
		postgresSlotKey(third):  -7,
	}
	derive := func(_ context.Context, _ *sql.Tx, key string) (int64, error) {
		return lockIDs[key], nil
	}

	for _, expectations := range [][]state.SlotExpectation{{first, second, third}, {third, second, first}} {
		got, err := conditionalAdvisoryLockIDs(context.Background(), nil, expectations, derive)
		if err != nil {
			t.Fatalf("conditionalAdvisoryLockIDs: %v", err)
		}
		if len(got) != 2 || got[0] != -7 || got[1] != 41 {
			t.Fatalf("acquisition keys = %v, want [-7 41]", got)
		}
	}
}
