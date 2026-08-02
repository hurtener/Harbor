package mcp

import (
	"errors"
	"testing"

	"github.com/hurtener/Harbor/internal/tools/auth"
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
	if err := r.Register(idCtx(t), ServerRegistration{Provider: prov, Transport: "stdio", InitialState: ServerStateOnline}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	if got := r.SourceIDs(); len(got) != 1 {
		t.Fatalf("pre-deregister SourceIDs = %v, want 1", got)
	}

	if err := r.Deregister(idCtx(t), "srv", auth.Owner{}); err != nil {
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
	if err := r.Deregister(idCtx(t), "ghost", auth.Owner{}); !errors.Is(err, ErrServerNotFound) {
		t.Fatalf("Deregister(ghost) = %v, want ErrServerNotFound", err)
	}
}

func TestRegistry_Deregister_CloseFailureRetainsRetryHandleAndBlocksReplacement(t *testing.T) {
	r := NewRegistry()
	closeErr := errors.New("close boom")
	prov := &stubProvider{id: "srv", closeErrs: []error{closeErr, nil}}
	if err := r.Register(idCtx(t), ServerRegistration{Provider: prov, Transport: "stdio", InitialState: ServerStateOnline}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	// The entry is still removed (visible immediately) but the close error is
	// surfaced loud (never swallowed).
	err := r.Deregister(idCtx(t), "srv", auth.Owner{})
	if !errors.Is(err, closeErr) {
		t.Fatalf("first close = %v, want injected error", err)
	}
	if got := r.SourceIDs(); len(got) != 0 {
		t.Fatalf("entry not removed despite close error: %v", got)
	}
	if err := r.Register(idCtx(t), ServerRegistration{Provider: &stubProvider{id: "srv"}, Transport: "stdio"}); err == nil {
		t.Fatal("replacement registered while prior generic handle was closing")
	}
	if err := r.Deregister(idCtx(t), "srv", auth.Owner{}); err != nil {
		t.Fatalf("retry close: %v", err)
	}
	prov.mu.Lock()
	closeCalls := prov.closed
	prov.mu.Unlock()
	if closeCalls != 2 {
		t.Fatalf("close calls = %d, want one failed attempt plus one retry", closeCalls)
	}
	if err := r.Register(idCtx(t), ServerRegistration{Provider: &stubProvider{id: "srv"}, Transport: "stdio"}); err != nil {
		t.Fatalf("replacement remained blocked after close receipt: %v", err)
	}
}

func TestRegistry_Deregister_PersistentCloseFailureNeverBecomesAbsentSuccess(t *testing.T) {
	r := NewRegistry()
	owner := auth.Owner{Tenant: "tenant", Agent: "agent"}
	closeErr := errors.New("persistent close boom")
	prov := &stubProvider{id: "srv", closeErr: closeErr}
	if err := r.Register(idCtx(t), ServerRegistration{Provider: prov, Transport: "stdio", Owner: owner}); err != nil {
		t.Fatal(err)
	}
	for attempt := range 3 {
		if err := r.Deregister(idCtx(t), "srv", owner); !errors.Is(err, closeErr) {
			t.Fatalf("attempt %d = %v, want persistent close error", attempt, err)
		}
		if err := r.Deregister(idCtx(t), "srv", auth.Owner{Tenant: "tenant", Agent: "other"}); !errors.Is(err, ErrServerNotFound) {
			t.Fatalf("cross-owner retry = %v, want ErrServerNotFound", err)
		}
		if err := r.Register(idCtx(t), ServerRegistration{Provider: &stubProvider{id: "srv"}, Transport: "stdio", Owner: owner}); err == nil {
			t.Fatalf("attempt %d allowed replacement before close receipt", attempt)
		}
	}
}
