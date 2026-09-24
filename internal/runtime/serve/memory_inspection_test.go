package serve

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	artifactsinmem "github.com/hurtener/Harbor/internal/artifacts/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/memory"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
)

func TestBuildMux_MemoryInspectionReportsExecutionStore(t *testing.T) {
	for _, mode := range []memory.Strategy{"", memory.StrategyRollingSummary} {
		t.Run(string(mode), func(t *testing.T) {
			deps := buildProjWiringMux(t)
			in := deps.in
			in.Cfg.Memory.Driver = "sqlite"
			in.Cfg.Memory.Strategy = string(mode)
			in.Cfg.State.Driver = "inmem"
			store, err := memory.Open(t.Context(), memory.ConfigSnapshot{Driver: "sqlite", DSN: ":memory:", Strategy: mode, RecentTurns: 4, BudgetTokens: 100000}, memory.Deps{State: in.State, Bus: in.Bus, Redactor: in.Redactor, RetentionTTL: time.Hour})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close(context.Background()) })
			in.Memory = store
			in.Artifacts, err = artifactsinmem.New(in.Cfg.Artifacts)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = in.Artifacts.Close(context.Background()) })
			id := identity.Identity{TenantID: "t", UserID: "u", SessionID: "inspection"}
			if _, err := store.Put(t.Context(), identity.Quadruple{Identity: id}, memory.ConversationTurn{UserMessage: "note"}); err != nil {
				t.Fatal(err)
			}
			built, err := BuildMux(in)
			if err != nil {
				t.Fatal(err)
			}
			code, body := postMux(t, built.Mux, "/v1/memory/list", id, `{}`)
			if code != http.StatusOK {
				t.Fatalf("list=%d %s", code, body)
			}
			var response prototypes.MemoryListResponse
			if err := json.Unmarshal(body, &response); err != nil {
				t.Fatal(err)
			}
			want := "inmem"
			if len(response.Items) != 1 || response.Items[0].Driver != want {
				t.Fatalf("execution-store projection=%+v, want %s", response.Items, want)
			}
		})
	}
}
