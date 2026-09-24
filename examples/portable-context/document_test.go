package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/hurtener/Harbor/sdk/config"
	"github.com/hurtener/Harbor/sdk/identity"
	"github.com/hurtener/Harbor/sdk/state"
	"github.com/hurtener/Harbor/sdk/tools"
)

func testStore(t *testing.T, dsn string) state.StateStore {
	t.Helper()
	store, err := state.Open(t.Context(), config.StateConfig{Driver: "sqlite", DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })
	return store
}

func testIdentity(t *testing.T, session string) context.Context {
	t.Helper()
	ctx, err := identity.With(t.Context(), identity.Identity{TenantID: "sample", UserID: "tester", SessionID: session})
	if err != nil {
		t.Fatal(err)
	}
	return ctx
}

func TestDocumentExactEditAndReopen(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "state.db")
	store := testStore(t, dsn)
	ctx := testIdentity(t, "edit")
	if err := seedDocument(ctx, store); err != nil {
		t.Fatal(err)
	}
	doc, _, err := loadDocument(ctx, store)
	if err != nil || len(doc.Source) != 14660 || doc.Version != 7 || doc.More {
		t.Fatalf("invalid complete seed: bytes=%d version=%d err=%v", len(doc.Source), doc.Version, err)
	}
	receipt, err := replaceDocument(ctx, store, replaceInput{documentID, 7, "<h1>Draft</h1>", "<h1>Approved</h1>"})
	if err != nil || !receipt.Saved || receipt.Version != 8 || receipt.RenderingVerified || receipt.InteractionVerified {
		t.Fatalf("invalid save receipt: %+v %v", receipt, err)
	}
	if err := store.Close(ctx); err != nil {
		t.Fatal(err)
	}
	reopened := testStore(t, dsn)
	if err := seedDocument(ctx, reopened); err != nil {
		t.Fatal(err)
	}
	doc, _, err = loadDocument(ctx, reopened)
	if err != nil || !strings.Contains(doc.Source, "<h1>Approved</h1>") || !strings.Contains(doc.Source, "<footer>Keep this footer</footer>") {
		t.Fatal("restart lost an edit or reseeded the document")
	}
	for _, bad := range []replaceInput{
		{documentID, 7, "Approved", "Lost"},
		{"wrong-id", 8, "Approved", "Lost"},
		{documentID, 8, "", "Lost"},
		{documentID, 8, "x", "Lost"},
		{documentID, 8, "missing", "Lost"},
		{documentID, 8, "Approved", strings.Repeat("z", maxSourceBytes)},
	} {
		if _, err := replaceDocument(ctx, reopened, bad); err == nil {
			t.Fatalf("invalid write succeeded: version=%d id=%s", bad.ExpectedVersion, bad.ResourceID)
		}
	}
	after, _, err := loadDocument(ctx, reopened)
	if err != nil || after != doc {
		t.Fatal("refused edit changed stored content")
	}
	if _, _, err := loadDocument(t.Context(), reopened); !errors.Is(err, identity.ErrIdentityMissing) {
		t.Fatal("missing identity accepted")
	}
}

func TestDocumentConcurrentCompareAndSwap(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "shared.db")
	first, second := testStore(t, dsn), testStore(t, dsn)
	var wg sync.WaitGroup
	for i := range 128 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx := testIdentity(t, fmt.Sprintf("session-%d", i))
			if err := seedDocument(ctx, first); err != nil {
				t.Error(err)
				return
			}
			gate := make(chan struct{})
			results := make(chan error, 2)
			for _, store := range []state.StateStore{first, second} {
				go func() {
					<-gate
					_, err := replaceDocument(ctx, store, replaceInput{documentID, 7, "Draft", fmt.Sprintf("Approved-%d", i)})
					results <- err
				}()
			}
			close(gate)
			errs := []error{<-results, <-results}
			success, conflict := 0, 0
			for _, err := range errs {
				switch {
				case err == nil:
					success++
				case errors.Is(err, errVersionConflict):
					conflict++
				default:
					t.Error(err)
				}
			}
			doc, _, err := loadDocument(ctx, second)
			if err != nil || success != 1 || conflict != 1 || doc.Version != 8 || !strings.Contains(doc.Source, fmt.Sprintf("Approved-%d", i)) {
				t.Errorf("session %d: success=%d conflict=%d version=%d err=%v", i, success, conflict, doc.Version, err)
			}
		}()
	}
	wg.Wait()
}

// Simulate losing an acknowledgement after the durable write succeeded.
type uncertainStore struct {
	state.StateStore
	loads, saves int
}

func (s *uncertainStore) Load(ctx context.Context, q identity.Quadruple, kind string) (state.StateRecord, error) {
	s.loads++
	return s.StateStore.Load(ctx, q, kind)
}
func (s *uncertainStore) SaveIf(ctx context.Context, p []state.SlotExpectation, record state.StateRecord) error {
	s.saves++
	if err := s.StateStore.SaveIf(ctx, p, record); err != nil {
		return err
	}
	return context.DeadlineExceeded
}
func TestDocumentUncertainWriteIsNotRetried(t *testing.T) {
	store := testStore(t, filepath.Join(t.TempDir(), "state.db"))
	ctx := testIdentity(t, "uncertain")
	if err := seedDocument(ctx, store); err != nil {
		t.Fatal(err)
	}
	uncertain := &uncertainStore{StateStore: store}
	cat := tools.NewCatalog()
	if err := registerDocuments(uncertain, cat); err != nil {
		t.Fatal(err)
	}
	descriptor, ok := cat.Resolve("document_replace")
	if !ok {
		t.Fatal("sample write tool not registered")
	}
	_, err := descriptor.Invoke(ctx, []byte(`{"resource_id":"sample-document","expected_version":7,"old":"Draft","new":"Approved"}`))
	if err == nil || uncertain.loads != 1 || uncertain.saves != 1 {
		t.Fatalf("uncertain effect retried or hidden: loads=%d saves=%d err=%v", uncertain.loads, uncertain.saves, err)
	}
	doc, _, err := loadDocument(ctx, store)
	if err != nil || doc.Version != 8 || !strings.Contains(doc.Source, "Approved") {
		t.Fatal("fixture did not durably save before losing acknowledgement")
	}
}
