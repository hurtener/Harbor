package artifacts_test

import (
	"context"
	"errors"
	"testing"

	"github.com/hurtener/Harbor/internal/artifacts"
	"github.com/hurtener/Harbor/internal/config"

	_ "github.com/hurtener/Harbor/internal/artifacts/drivers/inmem"
)

func TestScoped_AutoStampsScope(t *testing.T) {
	store := openInMem(t)
	defer func() { _ = store.Close(context.Background()) }()
	scope := artifacts.ArtifactScope{TenantID: "T", UserID: "U", SessionID: "S", TaskID: "K"}
	facade := artifacts.NewScoped(store, scope)
	ref, err := facade.PutBytes(context.Background(), []byte("hi"), artifacts.PutOpts{Namespace: "ns"})
	if err != nil {
		t.Fatalf("PutBytes: %v", err)
	}
	if !ref.Scope.Equal(scope) {
		t.Errorf("ref.Scope=%+v, want %+v", ref.Scope, scope)
	}
}

func TestScoped_PutText_DefaultsMime(t *testing.T) {
	store := openInMem(t)
	defer func() { _ = store.Close(context.Background()) }()
	scope := artifacts.ArtifactScope{TenantID: "T", UserID: "U", SessionID: "S"}
	facade := artifacts.NewScoped(store, scope)
	ref, err := facade.PutText(context.Background(), "lorem", artifacts.PutOpts{Namespace: "ns"})
	if err != nil {
		t.Fatalf("PutText: %v", err)
	}
	gotRef, found, err := facade.GetRef(context.Background(), ref.ID)
	if err != nil || !found {
		t.Fatalf("GetRef: err=%v found=%v", err, found)
	}
	if gotRef.MimeType == "" {
		t.Errorf("PutText did not default MimeType")
	}
}

func TestScoped_GetReturnsBytes(t *testing.T) {
	store := openInMem(t)
	defer func() { _ = store.Close(context.Background()) }()
	scope := artifacts.ArtifactScope{TenantID: "T", UserID: "U", SessionID: "S"}
	facade := artifacts.NewScoped(store, scope)
	ref, err := facade.PutBytes(context.Background(), []byte("payload"), artifacts.PutOpts{Namespace: "ns"})
	if err != nil {
		t.Fatal(err)
	}
	got, found, err := facade.Get(context.Background(), ref.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !found {
		t.Fatal("Get found=false")
	}
	if string(got) != "payload" {
		t.Errorf("Get bytes=%q", got)
	}
}

func TestScoped_ExistsAndDelete(t *testing.T) {
	store := openInMem(t)
	defer func() { _ = store.Close(context.Background()) }()
	scope := artifacts.ArtifactScope{TenantID: "T", UserID: "U", SessionID: "S"}
	facade := artifacts.NewScoped(store, scope)
	ref, err := facade.PutBytes(context.Background(), []byte("x"), artifacts.PutOpts{Namespace: "ns"})
	if err != nil {
		t.Fatal(err)
	}
	exists, err := facade.Exists(context.Background(), ref.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !exists {
		t.Errorf("Exists=false")
	}
	existed, err := facade.Delete(context.Background(), ref.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !existed {
		t.Errorf("Delete existed=false")
	}
	exists, err = facade.Exists(context.Background(), ref.ID)
	if err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Errorf("Exists=true after Delete")
	}
}

func TestScoped_ListReturnsOnlyOwnScope(t *testing.T) {
	store := openInMem(t)
	defer func() { _ = store.Close(context.Background()) }()
	scopeA := artifacts.ArtifactScope{TenantID: "T", UserID: "U", SessionID: "S1", TaskID: "K1"}
	scopeB := artifacts.ArtifactScope{TenantID: "T", UserID: "U", SessionID: "S2", TaskID: "K2"}
	facadeA := artifacts.NewScoped(store, scopeA)
	facadeB := artifacts.NewScoped(store, scopeB)
	_, err := facadeA.PutBytes(context.Background(), []byte("a1"), artifacts.PutOpts{Namespace: "ns"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = facadeA.PutBytes(context.Background(), []byte("a2"), artifacts.PutOpts{Namespace: "ns"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = facadeB.PutBytes(context.Background(), []byte("b1"), artifacts.PutOpts{Namespace: "ns"})
	if err != nil {
		t.Fatal(err)
	}
	listA, err := facadeA.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(listA) != 2 {
		t.Errorf("facadeA list len=%d, want 2", len(listA))
	}
	listB, err := facadeB.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(listB) != 1 {
		t.Errorf("facadeB list len=%d, want 1", len(listB))
	}
}

func TestScoped_NewScoped_PanicsOnInvalidScope(t *testing.T) {
	store := openInMem(t)
	defer func() { _ = store.Close(context.Background()) }()
	defer func() {
		if r := recover(); r == nil {
			t.Errorf("expected panic on invalid scope")
		}
	}()
	_ = artifacts.NewScoped(store, artifacts.ArtifactScope{})
}

func TestScoped_NewScoped_PanicsOnNilStore(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Errorf("expected panic on nil store")
		}
	}()
	_ = artifacts.NewScoped(nil, artifacts.ArtifactScope{TenantID: "T", UserID: "U", SessionID: "S"})
}

func TestScoped_GetRef_ScopeMismatch_DefendsAgainstDriverBug(t *testing.T) {
	// Use a synthetic ArtifactStore that returns refs with the WRONG
	// scope — the facade must surface ErrScopeMismatch loudly.
	scope := artifacts.ArtifactScope{TenantID: "T", UserID: "U", SessionID: "S"}
	wrongScope := artifacts.ArtifactScope{TenantID: "OTHER", UserID: "U", SessionID: "S"}
	leaky := &leakyStore{
		ref: artifacts.ArtifactRef{
			ID:    "ns_deadbeef0000",
			Scope: wrongScope,
		},
	}
	facade := artifacts.NewScoped(leaky, scope)
	_, found, err := facade.GetRef(context.Background(), "ns_deadbeef0000")
	if !errors.Is(err, artifacts.ErrScopeMismatch) {
		t.Errorf("err=%v, want errors.Is ErrScopeMismatch", err)
	}
	if found {
		t.Errorf("found=true, want false on scope mismatch")
	}
}

// --- helpers ---

func openInMem(t *testing.T) artifacts.ArtifactStore {
	t.Helper()
	s, err := artifacts.OpenDriver("inmem", config.ArtifactsConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("OpenDriver inmem: %v", err)
	}
	return s
}

// leakyStore is a deliberately-broken ArtifactStore that returns refs
// whose Scope doesn't match the request. Used to exercise the
// ScopedArtifacts defensive check.
type leakyStore struct {
	ref artifacts.ArtifactRef
}

func (l *leakyStore) PutBytes(_ context.Context, _ artifacts.ArtifactScope, _ []byte, _ artifacts.PutOpts) (artifacts.ArtifactRef, error) {
	return l.ref, nil
}

func (l *leakyStore) PutText(_ context.Context, _ artifacts.ArtifactScope, _ string, _ artifacts.PutOpts) (artifacts.ArtifactRef, error) {
	return l.ref, nil
}

func (l *leakyStore) Get(_ context.Context, _ artifacts.ArtifactScope, _ string) ([]byte, bool, error) {
	return nil, true, nil
}

func (l *leakyStore) GetRef(_ context.Context, _ artifacts.ArtifactScope, _ string) (*artifacts.ArtifactRef, bool, error) {
	r := l.ref
	return &r, true, nil
}

func (l *leakyStore) Exists(_ context.Context, _ artifacts.ArtifactScope, _ string) (bool, error) {
	return true, nil
}

func (l *leakyStore) Delete(_ context.Context, _ artifacts.ArtifactScope, _ string) (bool, error) {
	return true, nil
}

func (l *leakyStore) List(_ context.Context, _ artifacts.ArtifactScope) ([]artifacts.ArtifactRef, error) {
	return []artifacts.ArtifactRef{l.ref}, nil
}

func (l *leakyStore) Close(_ context.Context) error { return nil }
