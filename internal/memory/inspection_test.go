package memory_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/audit"
	_ "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	_ "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/memory"
	_ "github.com/hurtener/Harbor/internal/memory/drivers/inmem"
	_ "github.com/hurtener/Harbor/internal/memory/drivers/postgres"
	_ "github.com/hurtener/Harbor/internal/memory/drivers/sqlite"
	"github.com/hurtener/Harbor/internal/memory/strategy"
	"github.com/hurtener/Harbor/internal/state"
	_ "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	_ "github.com/hurtener/Harbor/internal/state/drivers/postgres"
	_ "github.com/hurtener/Harbor/internal/state/drivers/sqlite"
)

func TestInspection_DriverMutationRoundTrip(t *testing.T) {
	for _, driver := range []string{"inmem", "sqlite", "postgres"} {
		for _, mode := range []memory.Strategy{memory.StrategyNone, memory.StrategyTruncation, memory.StrategyRollingSummary} {
			t.Run(driver+"/"+string(mode), func(t *testing.T) {
				dsn := ""
				switch driver {
				case "postgres":
					dsn = os.Getenv("HARBOR_PG_DSN")
					if dsn == "" {
						t.Skip("HARBOR_PG_DSN required for driver inspection")
					}
				case "sqlite":
					dsn = filepath.Join(t.TempDir(), "inspection.db")
				}
				st, err := state.Open(t.Context(), config.StateConfig{Driver: driver, DSN: dsn})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = st.Close(context.Background()) })
				redactor, err := audit.Open(t.Context(), config.AuditConfig{})
				if err != nil {
					t.Fatal(err)
				}
				bus, err := events.Open(t.Context(), config.Defaults().Events, redactor)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = bus.Close(context.Background()) })
				store, err := memory.Open(t.Context(), memory.ConfigSnapshot{Driver: driver, DSN: dsn, Strategy: mode, RecentTurns: 4, BudgetTokens: 100000}, memory.Deps{State: st, Bus: bus, Redactor: redactor, Summarizer: strategy.EchoSummarizer{}, RetentionTTL: time.Hour})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = store.Close(context.Background()) })
				id := identity.Quadruple{Identity: identity.Identity{TenantID: "inspection-t", UserID: "inspection-u", SessionID: string(state.NewEventID())}}
				turn := memory.ConversationTurn{UserMessage: "recorded note", AssistantResponse: "noted", Timestamp: time.Now(), TrajectoryDigest: &memory.TrajectoryDigest{ObservationsSummary: "note provenance"}, ArtifactsHiddenRefs: []string{"opaque-ref"}}
				key, err := store.Put(t.Context(), id, turn)
				if err != nil {
					t.Fatal(err)
				}
				view, err := store.Inspect(t.Context(), id)
				if err != nil {
					t.Fatal(err)
				}
				if mode == memory.StrategyNone {
					if key != "" || len(view.Items) != 0 {
						t.Fatal("disabled memory retained a note")
					}
				} else {
					if len(view.Items) != 1 || view.Items[0].Key != key || !strings.Contains(string(view.Items[0].Value), "recorded note") {
						t.Fatalf("round trip=%+v", view)
					}
					if mode == memory.StrategyRollingSummary && view.Items[0].ExpiresAt.IsZero() {
						t.Fatal("expiry lost")
					}
					if n, err := store.Delete(t.Context(), id, key); err != nil || n != 0 {
						t.Fatalf("delete: %d %v", n, err)
					}
				}
				if _, err := store.Delete(t.Context(), id, key); !errors.Is(err, memory.ErrNotFound) {
					t.Fatalf("absent delete: %v", err)
				}
				ctx, cancel := context.WithCancel(t.Context())
				cancel()
				if _, err := store.Inspect(ctx, id); !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
				if _, err := store.Put(ctx, id, turn); !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
				if _, err := store.Delete(ctx, id, key); !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
			})
		}
	}
}
