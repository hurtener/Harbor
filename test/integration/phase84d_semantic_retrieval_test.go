// Semantic skills retain their existing embedding path. Native session-memory
// semantic indexing was retired by the cumulative-memory consolidation.
package integration_test

import (
	"context"
	"fmt"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/audit"
	_ "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/embeddings/embeddingstest"
	"github.com/hurtener/Harbor/internal/events"
	_ "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/memory"
	_ "github.com/hurtener/Harbor/internal/memory/drivers/sqlite"
	"github.com/hurtener/Harbor/internal/skills"
	_ "github.com/hurtener/Harbor/internal/skills/drivers/localdb"
	"github.com/hurtener/Harbor/internal/state"
	_ "github.com/hurtener/Harbor/internal/state/drivers/sqlite"
)

func phase84dQuad(tenant, user, session string) identity.Quadruple {
	return identity.Quadruple{Identity: identity.Identity{
		TenantID: tenant, UserID: user, SessionID: session,
	}}
}

func phase84dBus(t *testing.T) events.EventBus {
	t.Helper()
	red, err := audit.Open(context.Background(), config.AuditConfig{})
	if err != nil {
		t.Fatalf("audit.Open: %v", err)
	}
	bus, err := events.Open(context.Background(), config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     64,
		IdleTimeout:              30 * time.Second,
		DropWindow:               time.Second,
	}, red)
	if err != nil {
		t.Fatalf("events.Open: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })
	return bus
}

func TestE2E_Phase84d_SemanticSkills_RankAndIsolate(t *testing.T) {
	bus := phase84dBus(t)
	store, err := skills.Open(context.Background(), skills.ConfigSnapshot{
		Driver:    "localdb",
		DSN:       filepath.Join(t.TempDir(), "skills.sqlite"),
		Retrieval: skills.RetrievalSemantic,
	}, skills.Deps{Bus: bus, Embedder: embeddingstest.New()})
	if err != nil {
		t.Fatalf("skills.Open: %v", err)
	}
	defer func() { _ = store.Close(context.Background()) }()

	ctx := context.Background()
	owner := phase84dQuad("acme", "ana", "sess-1")
	seed := func(id identity.Quadruple, name, trigger, desc string) {
		t.Helper()
		if err := store.Upsert(ctx, id, skills.Skill{
			Name: name, Title: name, Trigger: trigger, Description: desc,
			Steps: []string{"step"}, Origin: skills.OriginGenerated, Scope: skills.ScopeSession,
		}); err != nil {
			t.Fatalf("Upsert %s: %v", name, err)
		}
	}
	seed(owner, "moor-boat", "mooring a sailboat at the pier", "tie the sailboat to the harbor pier cleats")
	seed(owner, "bake-cake", "baking a chocolate cake", "mix flour sugar cocoa and bake")

	ranked, err := store.Search(ctx, owner, "how to moor a sailboat at the pier", 1)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(ranked) != 1 || ranked[0].Skill.Name != "moor-boat" {
		t.Fatalf("semantic skill ranking missed: %+v", ranked)
	}
	if ranked[0].Path != skills.PathSemantic {
		t.Errorf("Path=%q, want %q", ranked[0].Path, skills.PathSemantic)
	}

	// Identity isolation.
	stranger := phase84dQuad("other-tenant", "ana", "sess-1")
	leak, err := store.Search(ctx, stranger, "how to moor a sailboat at the pier", 5)
	if err != nil {
		t.Fatalf("Search stranger: %v", err)
	}
	if len(leak) != 0 {
		t.Fatalf("cross-tenant semantic search returned %d row(s)", len(leak))
	}
}

// TestE2E_Phase84d_FailureMode_SemanticWithoutEmbedder pins the §17.3
// failure-mode leg: skills fail loudly at construction when a
// semantic mode is enabled without an Embedder.
func TestE2E_Phase84d_FailureMode_SemanticWithoutEmbedder(t *testing.T) {
	bus := phase84dBus(t)
	st, err := state.Open(context.Background(), config.StateConfig{Driver: "sqlite", DSN: filepath.Join(t.TempDir(), "state.sqlite")})
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	defer func() { _ = st.Close(context.Background()) }()

	_, err = skills.Open(context.Background(), skills.ConfigSnapshot{
		Driver:    "localdb",
		DSN:       filepath.Join(t.TempDir(), "skills.sqlite"),
		Retrieval: skills.RetrievalSemantic,
	}, skills.Deps{Bus: bus})
	if err == nil || !strings.Contains(err.Error(), "Deps.Embedder") {
		t.Fatalf("skills.Open err=%v, want the fail-loud Deps.Embedder guard", err)
	}
}

// TestE2E_Phase84d_ConcurrentSessions_NoCrossTalk is the §17.3
// concurrency-stress leg: N concurrent sessions put + inspect against
// ONE shared memory store; each session retrieves only its own turn;
// goroutine baseline restored after teardown.
func TestE2E_Phase84d_ConcurrentSessions_NoCrossTalk(t *testing.T) {
	bus := phase84dBus(t)
	red, err := audit.Open(context.Background(), config.AuditConfig{})
	if err != nil {
		t.Fatal(err)
	}
	st, err := state.Open(context.Background(), config.StateConfig{Driver: "sqlite", DSN: filepath.Join(t.TempDir(), "state.sqlite")})
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	mem, err := memory.Open(context.Background(), memory.ConfigSnapshot{
		Driver:   "sqlite",
		DSN:      filepath.Join(t.TempDir(), "memory.sqlite"),
		Strategy: memory.StrategyRollingSummary,
	}, memory.Deps{State: st, Bus: bus, Redactor: red})
	if err != nil {
		t.Fatalf("memory.Open: %v", err)
	}

	baseline := runtime.NumGoroutine()
	const sessions = 128
	var wg sync.WaitGroup
	errCh := make(chan error, sessions)
	wg.Add(sessions)
	for i := range sessions {
		go func() {
			defer wg.Done()
			ctx := context.Background()
			q := phase84dQuad("acme", "ana", fmt.Sprintf("sess-%d", i))
			marker := fmt.Sprintf("topic-%d unique payload for session %d", i, i)
			if _, err := mem.Put(ctx, q, memory.ConversationTurn{
				UserMessage: marker, AssistantResponse: "noted",
			}); err != nil {
				errCh <- fmt.Errorf("session %d Put: %w", i, err)
				return
			}
			got, err := mem.Inspect(ctx, q)
			if err != nil {
				errCh <- fmt.Errorf("session %d Inspect: %w", i, err)
				return
			}
			if len(got.Items) != 1 || !strings.Contains(string(got.Items[0].Value), fmt.Sprintf("session %d", i)) {
				errCh <- fmt.Errorf("session %d cross-talk: %+v", i, got)
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Error(err)
	}

	if err := mem.Close(context.Background()); err != nil {
		t.Fatalf("mem.Close: %v", err)
	}
	if err := st.Close(context.Background()); err != nil {
		t.Fatalf("state.Close: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > baseline && time.Now().Before(deadline) {
		runtime.Gosched()
	}
	if delta := runtime.NumGoroutine() - baseline; delta > 0 {
		t.Errorf("goroutine leak: baseline=%d, after=%d", baseline, runtime.NumGoroutine())
	}
}
