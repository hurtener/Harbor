package mcp

import (
	"errors"
	"testing"
)

// deregister_test.go — coverage for the detach primitives the run-start
// reconcile leans on: Registry.Deregister (delete map entry + close transport)
// and Registry.SourceIDs (the attached-set enumeration).

func TestRegistry_SourceIDs_ListsAttached(t *testing.T) {
	r := newTestRegistry(t)
	ids := r.SourceIDs()
	if len(ids) != 2 || ids[0] != "github-server" || ids[1] != "slack-server" {
		t.Fatalf("SourceIDs = %v, want sorted [github-server slack-server]", ids)
	}
}

func TestRegistry_Deregister_RemovesAndClosesTransport(t *testing.T) {
	r := NewRegistry()
	prov := &stubProvider{id: "srv", toolNames: []string{"t"}}
	if err := r.Register(ServerRegistration{Provider: prov, Transport: "stdio", InitialState: ServerStateOnline}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if got := r.SourceIDs(); len(got) != 1 {
		t.Fatalf("pre-deregister SourceIDs = %v, want 1", got)
	}

	if err := r.Deregister(idCtx(t), "srv"); err != nil {
		t.Fatalf("Deregister: %v", err)
	}
	// The server is gone from the registry.
	if got := r.SourceIDs(); len(got) != 0 {
		t.Fatalf("post-deregister SourceIDs = %v, want empty", got)
	}
	if _, err := r.GetServer(idCtx(t), "srv"); !errors.Is(err, ErrServerNotFound) {
		t.Fatalf("GetServer after deregister: err = %v, want ErrServerNotFound", err)
	}
	// The provider's transport was closed exactly once.
	prov.mu.Lock()
	closed := prov.closed
	prov.mu.Unlock()
	if closed != 1 {
		t.Fatalf("provider Close called %d times, want 1", closed)
	}
}

func TestRegistry_Deregister_UnknownName_NotFound(t *testing.T) {
	r := newTestRegistry(t)
	if err := r.Deregister(idCtx(t), "ghost"); !errors.Is(err, ErrServerNotFound) {
		t.Fatalf("Deregister(ghost) = %v, want ErrServerNotFound", err)
	}
}

func TestRegistry_Deregister_PropagatesCloseError(t *testing.T) {
	r := NewRegistry()
	prov := &stubProvider{id: "srv", closeErr: errors.New("close boom")}
	if err := r.Register(ServerRegistration{Provider: prov, Transport: "stdio", InitialState: ServerStateOnline}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	// The entry is still removed (visible immediately) but the close error is
	// surfaced loud (never swallowed).
	err := r.Deregister(idCtx(t), "srv")
	if err == nil {
		t.Fatal("want a close error surfaced")
	}
	if got := r.SourceIDs(); len(got) != 0 {
		t.Fatalf("entry not removed despite close error: %v", got)
	}
}
