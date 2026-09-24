package serve

import (
	"context"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/artifacts"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/tasks"
)

func TestRetainedServer_InputReferencesAndDeletion(t *testing.T) {
	store, err := artifacts.Open(t.Context(), config.ArtifactsConfig{Driver: "inmem"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close(context.Background()) }()
	env, client, toolsCalled := retainedServerHarness(t, func(opts *RunLoopDriverOptions) { opts.ArtifactStore = store })
	id := identity.Identity{TenantID: "t", UserID: "u", SessionID: "served-input"}
	scope := artifacts.ArtifactScope{TenantID: id.TenantID, UserID: id.UserID, SessionID: id.SessionID}
	ref, err := store.PutText(t.Context(), scope, "SOURCE-DO-NOT-DUPLICATE", artifacts.PutOpts{MimeType: "text/plain", Filename: "source.txt"})
	if err != nil {
		t.Fatal(err)
	}
	first := retainedServerTurn(t, env, id, "Use this supplied attachment", nil, ref.ID)
	if first.Status != tasks.StatusComplete {
		t.Fatalf("first: %+v", first.Error)
	}
	second := retainedServerTurn(t, env, id, "Refer to the previous attachment", nil)
	body := client.body(second.ID)
	if second.Status != tasks.StatusComplete || !strings.Contains(body, ref.ID) || !strings.Contains(body, "retained input attachment") || strings.Contains(body, "SOURCE-DO-NOT-DUPLICATE") {
		t.Fatalf("attachment continuity failed: %+v", second.Error)
	}
	if _, err := store.Delete(t.Context(), scope, ref.ID); err != nil {
		t.Fatal(err)
	}
	third := retainedServerTurn(t, env, id, "Attachment was erased", nil)
	if third.Status != tasks.StatusFailed || client.body(third.ID) != "" || toolsCalled.Load() != 0 {
		t.Fatal("erased attachment reached inference")
	}
	missing := retainedServerTurn(t, env, identity.Identity{TenantID: "t", UserID: "u", SessionID: "missing"}, "Inspect missing", nil, "unknown")
	if missing.Status != tasks.StatusFailed || client.body(missing.ID) != "" {
		t.Fatal("missing supplied input silently omitted")
	}
}
