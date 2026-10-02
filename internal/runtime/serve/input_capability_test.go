package serve

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/types"
)

func TestInputReceiptCapability_PersistentConfigurationOnly(t *testing.T) {
	for _, taskDriver := range []string{"inprocess", "durable"} {
		for _, stateDriver := range []string{"inmem", "sqlite", "postgres"} {
			t.Run(fmt.Sprintf("%s/%s", taskDriver, stateDriver), func(t *testing.T) {
				deps := buildProjWiringMux(t)
				// BuildMux consumes the boot-validated driver configuration. This table
				// pins its advertisement gate; the storage triad fixtures separately
				// execute actual durable close/reopen against each configured backend.
				deps.in.Cfg.Tasks.Driver = taskDriver
				deps.in.Cfg.State.Driver = stateDriver
				built, err := BuildMux(deps.in)
				if err != nil {
					t.Fatal(err)
				}
				id := identity.Identity{TenantID: "t", UserID: "u", SessionID: "s"}
				code, body := postMux(t, built.Mux, "/v1/control/runtime.info", id, `{}`)
				if code != http.StatusOK {
					t.Fatalf("runtime info status=%d body=%s", code, body)
				}
				var info types.RuntimeInfo
				if err = json.Unmarshal(body, &info); err != nil {
					t.Fatal(err)
				}
				want := taskDriver == "durable" && stateDriver != "inmem"
				if got := slices.Contains(info.Capabilities, types.CapDurableTaskInputReceipts); got != want {
					t.Fatalf("receipt capability=%v want=%v", got, want)
				}
			})
		}
	}
}
