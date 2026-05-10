package inmem_test

import (
	"context"
	"errors"
	"testing"

	"github.com/hurtener/Harbor/internal/artifacts"
	"github.com/hurtener/Harbor/internal/artifacts/conformancetest"
	"github.com/hurtener/Harbor/internal/artifacts/drivers/inmem"
	"github.com/hurtener/Harbor/internal/config"
)

// TestInMem_Conformance drives the canonical conformance suite
// against the inmem driver. This is the gate Phase 18 (SQLite-blob,
// Postgres-blob) and Phase 19 (S3-style) drivers will inherit
// verbatim.
func TestInMem_Conformance(t *testing.T) {
	conformancetest.Run(t, func() (artifacts.ArtifactStore, func()) {
		s, err := inmem.New(config.ArtifactsConfig{Driver: "inmem"})
		if err != nil {
			t.Fatalf("inmem.New: %v", err)
		}
		return s, func() { _ = s.Close(context.Background()) }
	})
}

// TestInMem_DriverRegistered verifies the init() side-effect — the
// driver self-registers under "inmem" so OpenDriver can resolve.
func TestInMem_DriverRegistered(t *testing.T) {
	cfg := config.ArtifactsConfig{Driver: "inmem"}
	s, err := artifacts.OpenDriver("inmem", cfg)
	if err != nil {
		t.Fatalf("OpenDriver: %v", err)
	}
	defer func() { _ = s.Close(context.Background()) }()
}

// TestInMem_DefendsAgainstCallerMutation pins the deep-copy contract
// noted in the inmem package godoc — callers that mutate a slice
// they passed in (or got back) MUST NOT see the change reflected in
// the store.
func TestInMem_DefendsAgainstCallerMutation(t *testing.T) {
	s, err := inmem.New(config.ArtifactsConfig{Driver: "inmem"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close(context.Background()) }()

	scope := artifacts.ArtifactScope{TenantID: "T", UserID: "U", SessionID: "S"}
	data := []byte("original")
	ref, err := s.PutBytes(context.Background(), scope, data, artifacts.PutOpts{Namespace: "ns"})
	if err != nil {
		t.Fatal(err)
	}

	// Mutate the slice the caller passed in.
	data[0] = 'X'

	got, found, err := s.Get(context.Background(), scope, ref.ID)
	if err != nil || !found {
		t.Fatalf("Get: err=%v found=%v", err, found)
	}
	if string(got) != "original" {
		t.Errorf("inmem did not deep-copy on Put: %q", got)
	}

	// Mutate the loaded slice; a second Get must still see the
	// pristine value.
	got[0] = 'Y'
	got2, found, err := s.Get(context.Background(), scope, ref.ID)
	if err != nil || !found {
		t.Fatalf("Get 2: err=%v found=%v", err, found)
	}
	if string(got2) != "original" {
		t.Errorf("inmem did not deep-copy on Get: %q", got2)
	}
}

// TestInMem_DedupReturnsSameRef pins the dedup contract: same scope +
// namespace + bytes → same ID, same SHA, single storage row.
func TestInMem_DedupReturnsSameRef(t *testing.T) {
	s, err := inmem.New(config.ArtifactsConfig{Driver: "inmem"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close(context.Background()) }()

	scope := artifacts.ArtifactScope{TenantID: "T", UserID: "U", SessionID: "S"}
	opts := artifacts.PutOpts{Namespace: "ns"}
	ref1, err := s.PutBytes(context.Background(), scope, []byte("payload"), opts)
	if err != nil {
		t.Fatal(err)
	}
	ref2, err := s.PutBytes(context.Background(), scope, []byte("payload"), opts)
	if err != nil {
		t.Fatal(err)
	}
	if ref1.ID != ref2.ID || ref1.SHA256 != ref2.SHA256 {
		t.Errorf("dedup failed: ref1=%+v ref2=%+v", ref1, ref2)
	}
}

// TestInMem_PreservesAndClonesSource verifies Source map propagation
// — Put accepts metadata, GetRef returns a clone (caller can mutate
// without affecting the stored ref).
func TestInMem_PreservesAndClonesSource(t *testing.T) {
	s, err := inmem.New(config.ArtifactsConfig{Driver: "inmem"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close(context.Background()) }()
	scope := artifacts.ArtifactScope{TenantID: "T", UserID: "U", SessionID: "S"}
	src := map[string]any{"tool": "echo", "n": 42}
	ref, err := s.PutBytes(context.Background(), scope, []byte("x"),
		artifacts.PutOpts{Namespace: "ns", Source: src})
	if err != nil {
		t.Fatal(err)
	}
	if ref.Source["tool"] != "echo" {
		t.Errorf("Source not preserved on Put: %+v", ref.Source)
	}
	// Mutate caller's source; stored ref should be unaffected.
	src["tool"] = "MUTATED"
	got, found, err := s.GetRef(context.Background(), scope, ref.ID)
	if err != nil || !found {
		t.Fatalf("GetRef: err=%v found=%v", err, found)
	}
	if got.Source["tool"] != "echo" {
		t.Errorf("inmem did not clone Source: %+v", got.Source)
	}
	// Mutate returned source; subsequent GetRef should be unaffected.
	got.Source["tool"] = "MUTATED-AGAIN"
	got2, found, err := s.GetRef(context.Background(), scope, ref.ID)
	if err != nil || !found {
		t.Fatalf("GetRef 2: err=%v found=%v", err, found)
	}
	if got2.Source["tool"] != "echo" {
		t.Errorf("inmem did not clone on Read: %+v", got2.Source)
	}
}

// TestInMem_EmptyID_ReturnsFoundFalse — explicit edge: empty id is
// treated as "no such artifact" rather than panicking.
func TestInMem_EmptyID_ReturnsFoundFalse(t *testing.T) {
	s, err := inmem.New(config.ArtifactsConfig{Driver: "inmem"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Close(context.Background()) }()
	scope := artifacts.ArtifactScope{TenantID: "T", UserID: "U", SessionID: "S"}
	got, found, err := s.Get(context.Background(), scope, "")
	if err != nil {
		t.Errorf("Get empty id: err=%v", err)
	}
	if found {
		t.Errorf("Get empty id: found=true")
	}
	if got != nil {
		t.Errorf("Get empty id: bytes=%q", got)
	}
	exists, err := s.Exists(context.Background(), scope, "")
	if err != nil {
		t.Errorf("Exists empty id: err=%v", err)
	}
	if exists {
		t.Errorf("Exists empty id: true")
	}
	existed, err := s.Delete(context.Background(), scope, "")
	if err != nil {
		t.Errorf("Delete empty id: err=%v", err)
	}
	if existed {
		t.Errorf("Delete empty id: existed=true")
	}
}

// TestInMem_ClosedRejectsAllOps verifies every method returns
// ErrStoreClosed after Close — defends against the per-method
// closed-flag check by exercising each path.
func TestInMem_ClosedRejectsAllOps(t *testing.T) {
	s, err := inmem.New(config.ArtifactsConfig{Driver: "inmem"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	scope := artifacts.ArtifactScope{TenantID: "T", UserID: "U", SessionID: "S"}
	ctx := context.Background()
	if _, err := s.PutBytes(ctx, scope, []byte("x"), artifacts.PutOpts{}); !errors.Is(err, artifacts.ErrStoreClosed) {
		t.Errorf("PutBytes: err=%v", err)
	}
	if _, err := s.PutText(ctx, scope, "x", artifacts.PutOpts{}); !errors.Is(err, artifacts.ErrStoreClosed) {
		t.Errorf("PutText: err=%v", err)
	}
	if _, _, err := s.Get(ctx, scope, "id"); !errors.Is(err, artifacts.ErrStoreClosed) {
		t.Errorf("Get: err=%v", err)
	}
	if _, _, err := s.GetRef(ctx, scope, "id"); !errors.Is(err, artifacts.ErrStoreClosed) {
		t.Errorf("GetRef: err=%v", err)
	}
	if _, err := s.Exists(ctx, scope, "id"); !errors.Is(err, artifacts.ErrStoreClosed) {
		t.Errorf("Exists: err=%v", err)
	}
	if _, err := s.Delete(ctx, scope, "id"); !errors.Is(err, artifacts.ErrStoreClosed) {
		t.Errorf("Delete: err=%v", err)
	}
	if _, err := s.List(ctx, scope); !errors.Is(err, artifacts.ErrStoreClosed) {
		t.Errorf("List: err=%v", err)
	}
}
