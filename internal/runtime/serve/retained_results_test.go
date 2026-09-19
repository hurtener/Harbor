package serve

import (
	"context"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/artifacts"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/runtime/dispatch"
	"github.com/hurtener/Harbor/internal/tasks"
)

func TestRetainedServer_ResultReferencesAndDeletion(t *testing.T) {
	store, err := artifacts.Open(t.Context(), config.ArtifactsConfig{Driver: "inmem"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close(context.Background()) }()
	env, client, calls, _ := retainedServerHarness(t, func(opts *RunLoopDriverOptions) {
		opts.ArtifactStore = store
		opts.Executor = dispatch.NewToolExecutor(opts.Catalog, store, nil, dispatch.WithHeavyThreshold(4096))
	})
	id := identity.Identity{TenantID: "t", UserID: "u", SessionID: "large-served"}
	first := retainedServerTurn(t, env, id, "first root", nil)
	if first.Status != tasks.StatusComplete {
		t.Fatalf("first: %+v", first.Error)
	}
	scope := artifacts.ArtifactScope{TenantID: id.TenantID, UserID: id.UserID, SessionID: id.SessionID}
	refs, err := store.List(t.Context(), scope)
	if err != nil || len(refs) != 1 {
		t.Fatalf("offloaded refs=%d error=%v", len(refs), err)
	}
	second := retainedServerTurn(t, env, id, "continue exact source", nil)
	body := client.body(second.ID)
	if second.Status != tasks.StatusComplete || !strings.Contains(body, "Retained result references") || !strings.Contains(body, refs[0].ID) {
		t.Fatalf("served request lost resolvable reference: %+v", second.Error)
	}
	if _, err := store.Delete(t.Context(), scope, refs[0].ID); err != nil {
		t.Fatal(err)
	}
	third := retainedServerTurn(t, env, id, "source was erased", nil)
	if third.Status != tasks.StatusFailed || client.body(third.ID) != "" || calls.Load() != 1 {
		t.Fatal("deleted source reached inference or historical read was repeated")
	}
}
