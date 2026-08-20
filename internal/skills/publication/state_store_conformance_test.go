package publication

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/state"
	stateinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	postgresdriver "github.com/hurtener/Harbor/internal/state/drivers/postgres"
	sqlitedriver "github.com/hurtener/Harbor/internal/state/drivers/sqlite"
)

type publicationStateFixture struct {
	open   func() (state.StateStore, error)
	close  func()
	shared state.StateStore
}

func TestStateStorePublication_Conformance_InMemoryAndSQLite(t *testing.T) {
	tests := []struct {
		name string
		new  func(t *testing.T) publicationStateFixture
	}{
		{"inmem", func(t *testing.T) publicationStateFixture {
			raw, err := stateinmem.New(config.StateConfig{})
			if err != nil {
				t.Fatal(err)
			}
			return publicationStateFixture{shared: raw, open: func() (state.StateStore, error) { return raw, nil }, close: func() { _ = raw.Close(context.Background()) }}
		}},
		{"sqlite", func(t *testing.T) publicationStateFixture {
			path := filepath.Join(t.TempDir(), "publication.sqlite")
			open := func() (state.StateStore, error) {
				return sqlitedriver.New(config.StateConfig{Driver: "sqlite", DSN: path})
			}
			raw, err := open()
			if err != nil {
				t.Fatal(err)
			}
			return publicationStateFixture{shared: raw, open: open, close: func() { _ = raw.Close(context.Background()) }}
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) { runPublicationStateContract(t, tc.new(t)) })
	}
}

func TestStateStorePublication_Conformance_Postgres(t *testing.T) {
	dsn := os.Getenv("HARBOR_PG_DSN")
	if dsn == "" {
		t.Skip("HARBOR_PG_DSN not set; skipping publication StateStore conformance")
	}
	open := func() (state.StateStore, error) {
		return postgresdriver.New(config.StateConfig{Driver: "postgres", DSN: dsn})
	}
	raw, err := open()
	if err != nil {
		t.Fatal(err)
	}
	runPublicationStateContract(t, publicationStateFixture{shared: raw, open: open, close: func() { _ = raw.Close(context.Background()) }})
}

func runPublicationStateContract(t *testing.T, f publicationStateFixture) {
	t.Helper()
	ctx := context.Background()
	id := publicationIdentity("contract-tenant", "contract-user", "contract-session")
	store, err := NewStateStore(f.shared, "contract-runtime")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close(ctx); f.close() }()
	first, receipt, err := store.Publish(ctx, id, PublishRequest{IdempotencyKey: "publish", Name: "ops", Skill: publicationSkill("ops", "one"), ExpectedAbsent: true})
	if err != nil {
		t.Fatal(err)
	}
	replayed, replayReceipt, err := store.Publish(ctx, id, PublishRequest{IdempotencyKey: "publish", Name: "ops", Skill: publicationSkill("ops", "one"), ExpectedAbsent: true})
	if err != nil || replayed != first || replayReceipt != receipt {
		t.Fatalf("publish replay=%+v/%+v err=%v", replayed, replayReceipt, err)
	}
	second, _, err := store.PublishSuccessor(ctx, id, SuccessorRequest{IdempotencyKey: "successor", PublicationID: first.PublicationID, ExpectedGeneration: first.Generation, ExpectedContentHash: first.ContentHash, Skill: publicationSkill("ops", "two")})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Install(ctx, id, InstallRequest{IdempotencyKey: "install", AgentID: "agent", PublicationID: second.PublicationID, RevisionID: first.RevisionID, ExpectedAbsent: true}); err != nil {
		t.Fatal(err)
	}
	body, meta, err := store.Resolve(ctx, id, "agent")
	if err != nil || body.Steps[0] != "one" || meta.RevisionID != first.RevisionID {
		t.Fatalf("old revision resolve body=%+v meta=%+v err=%v", body, meta, err)
	}
	other := publicationIdentity("other-tenant", "contract-user", "contract-session")
	if _, err := store.List(ctx, other); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Update(ctx, other, UpdateRequest{IdempotencyKey: "cross", AgentID: "agent", PublicationID: first.PublicationID, RevisionID: second.RevisionID, ExpectedGeneration: second.Generation, ExpectedContentHash: second.ContentHash}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-tenant update=%v, want not found", err)
	}
	if _, _, err := store.Retire(ctx, id, RetireRequest{IdempotencyKey: "retire", PublicationID: first.PublicationID, ExpectedGeneration: second.Generation, ExpectedContentHash: second.ContentHash}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.PublishSuccessor(ctx, id, SuccessorRequest{IdempotencyKey: "retired-successor", PublicationID: first.PublicationID, ExpectedGeneration: second.Generation + 1, ExpectedContentHash: second.ContentHash, Skill: publicationSkill("ops", "three")}); !errors.Is(err, ErrRetired) && !errors.Is(err, ErrConflict) {
		t.Fatalf("successor after retire=%v", err)
	}
	var wg sync.WaitGroup
	errCh := make(chan error, 128)
	for i := 0; i < 128; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, e := store.Resolve(ctx, id, "agent")
			if e != nil && !errors.Is(e, ErrRetired) {
				errCh <- e
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for e := range errCh {
		t.Fatal(e)
	}

	if err := store.Close(ctx); err != nil {
		t.Fatal(err)
	}
	reopenedRaw, err := f.open()
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := NewStateStore(reopenedRaw, "contract-runtime")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = reopened.Close(ctx)
		if reopenedRaw != f.shared {
			_ = reopenedRaw.Close(ctx)
		}
	}()
	if _, err := reopened.List(ctx, id); err != nil {
		t.Fatalf("list after restart: %v", err)
	}
}
