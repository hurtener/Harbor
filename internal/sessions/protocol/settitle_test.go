package protocol_test

import (
	"context"
	"errors"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/sessions"
	sessionsprotocol "github.com/hurtener/Harbor/internal/sessions/protocol"
)

// fakeTitleSetter is the deterministic stand-in for the registry's
// SetTitle the Service-level tests need (CLAUDE.md §17.4 — NOT a
// re-implementation of registry semantics; the integration test drives
// the real *sessions.Registry). It records every call so a test can
// assert exactly what target id / identity / title reached it.
type fakeTitleSetter struct {
	calledWithID    string
	calledWithIdent identity.Identity
	calledWithTitle string
	err             error
}

func (f *fakeTitleSetter) SetTitle(_ context.Context, id string, ident identity.Identity, title string) error {
	f.calledWithID = id
	f.calledWithIdent = ident
	f.calledWithTitle = title
	return f.err
}

// TestService_SetTitle_Success pins the happy path: a valid identity +
// non-empty title reaches the TitleSetter with exactly the caller's
// verified (tenant, user) identity and the target session id, and the
// response reports title_source = "manual".
func TestService_SetTitle_Success(t *testing.T) {
	t.Parallel()
	ts := &fakeTitleSetter{}
	svc, err := sessionsprotocol.NewService(&fakeProjector{}, sessionsprotocol.WithTitleSetter(ts))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	resp, err := svc.SetTitle(context.Background(), prototypes.SessionsSetTitleRequest{
		Identity: scope("t1", "u1", "s-caller"), SessionID: "s-target", Title: "  My Title  ",
	})
	if err != nil {
		t.Fatalf("SetTitle: %v", err)
	}
	if resp.SessionID != "s-target" || resp.Title != "My Title" || resp.TitleSource != "manual" {
		t.Errorf("response = %+v, want {s-target, \"My Title\", manual}", resp)
	}
	if ts.calledWithID != "s-target" {
		t.Errorf("TitleSetter called with id=%q, want s-target", ts.calledWithID)
	}
	if ts.calledWithIdent != (identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s-caller"}) {
		t.Errorf("TitleSetter called with ident=%+v, want the caller's verified triple", ts.calledWithIdent)
	}
	if ts.calledWithTitle != "  My Title  " {
		t.Errorf("TitleSetter called with title=%q, want the RAW (untrimmed) title — trimming is the registry's job", ts.calledWithTitle)
	}
}

// TestService_SetTitle_Clear pins the clear path: an empty title reaches
// the TitleSetter and the response reports title_source = "" (unset).
func TestService_SetTitle_Clear(t *testing.T) {
	t.Parallel()
	ts := &fakeTitleSetter{}
	svc, err := sessionsprotocol.NewService(&fakeProjector{}, sessionsprotocol.WithTitleSetter(ts))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	resp, err := svc.SetTitle(context.Background(), prototypes.SessionsSetTitleRequest{
		Identity: scope("t1", "u1", "s-caller"), SessionID: "s-target", Title: "",
	})
	if err != nil {
		t.Fatalf("SetTitle: %v", err)
	}
	if resp.Title != "" || resp.TitleSource != "" {
		t.Errorf("response = %+v, want an empty title and unset source", resp)
	}
}

// TestService_SetTitle_IncompleteIdentity_Rejected pins the
// identity-mandatory gate: an incomplete triple fails closed with
// ErrIdentityRequired and the TitleSetter is NEVER reached.
func TestService_SetTitle_IncompleteIdentity_Rejected(t *testing.T) {
	t.Parallel()
	ts := &fakeTitleSetter{}
	svc, err := sessionsprotocol.NewService(&fakeProjector{}, sessionsprotocol.WithTitleSetter(ts))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	_, serr := svc.SetTitle(context.Background(), prototypes.SessionsSetTitleRequest{
		Identity: scope("t1", "", "s-caller"), SessionID: "s-target", Title: "x",
	})
	if !errors.Is(serr, sessionsprotocol.ErrIdentityRequired) {
		t.Fatalf("SetTitle err=%v, want ErrIdentityRequired", serr)
	}
	if ts.calledWithID != "" {
		t.Errorf("TitleSetter was reached on an incomplete identity: id=%q", ts.calledWithID)
	}
}

// TestService_SetTitle_EmptySessionID_Rejected pins the empty-target
// guard: a blank (or whitespace-only) session_id is ErrInvalidRequest,
// never forwarded to the TitleSetter.
func TestService_SetTitle_EmptySessionID_Rejected(t *testing.T) {
	t.Parallel()
	ts := &fakeTitleSetter{}
	svc, err := sessionsprotocol.NewService(&fakeProjector{}, sessionsprotocol.WithTitleSetter(ts))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	_, serr := svc.SetTitle(context.Background(), prototypes.SessionsSetTitleRequest{
		Identity: scope("t1", "u1", "s-caller"), SessionID: "   ", Title: "x",
	})
	if !errors.Is(serr, sessionsprotocol.ErrInvalidRequest) {
		t.Fatalf("SetTitle err=%v, want ErrInvalidRequest", serr)
	}
	if ts.calledWithID != "" {
		t.Error("TitleSetter was reached with an empty session_id")
	}
}

// TestService_SetTitle_InvalidTitle_Mapped pins the oversize/control-char
// mapping: sessions.ErrInvalidTitle is mapped to the Service's
// ErrInvalidRequest (the handler turns it into a 400).
func TestService_SetTitle_InvalidTitle_Mapped(t *testing.T) {
	t.Parallel()
	ts := &fakeTitleSetter{err: sessions.ErrInvalidTitle}
	svc, err := sessionsprotocol.NewService(&fakeProjector{}, sessionsprotocol.WithTitleSetter(ts))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	_, serr := svc.SetTitle(context.Background(), prototypes.SessionsSetTitleRequest{
		Identity: scope("t1", "u1", "s-caller"), SessionID: "s-target", Title: "x",
	})
	if !errors.Is(serr, sessionsprotocol.ErrInvalidRequest) {
		t.Fatalf("SetTitle err=%v, want ErrInvalidRequest", serr)
	}
}

// TestService_SetTitle_NotFound_Mapped pins the absent/foreign-session
// mapping (covers both an unknown id and a cross-user/cross-tenant
// target — the registry reports both as sessions.ErrSessionNotFound).
func TestService_SetTitle_NotFound_Mapped(t *testing.T) {
	t.Parallel()
	ts := &fakeTitleSetter{err: sessions.ErrSessionNotFound}
	svc, err := sessionsprotocol.NewService(&fakeProjector{}, sessionsprotocol.WithTitleSetter(ts))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	_, serr := svc.SetTitle(context.Background(), prototypes.SessionsSetTitleRequest{
		Identity: scope("t1", "u1", "s-caller"), SessionID: "s-foreign", Title: "x",
	})
	if !errors.Is(serr, sessionsprotocol.ErrSessionNotFound) {
		t.Fatalf("SetTitle err=%v, want ErrSessionNotFound", serr)
	}
}

// TestService_SetTitle_IdentityMismatch_Mapped pins the defensive
// stored-identity mismatch mapping onto ErrIdentityRequired.
func TestService_SetTitle_IdentityMismatch_Mapped(t *testing.T) {
	t.Parallel()
	ts := &fakeTitleSetter{err: sessions.ErrIdentityMismatch}
	svc, err := sessionsprotocol.NewService(&fakeProjector{}, sessionsprotocol.WithTitleSetter(ts))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	_, serr := svc.SetTitle(context.Background(), prototypes.SessionsSetTitleRequest{
		Identity: scope("t1", "u1", "s-caller"), SessionID: "s-target", Title: "x",
	})
	if !errors.Is(serr, sessionsprotocol.ErrIdentityRequired) {
		t.Fatalf("SetTitle err=%v, want ErrIdentityRequired", serr)
	}
}

// TestService_SetTitle_NoTitleSetter_Unsupported pins capability gating:
// a Service built without a TitleSetter answers ErrTitleSetUnsupported
// (the handler maps it to a 404) and HasTitleSetter reports false.
func TestService_SetTitle_NoTitleSetter_Unsupported(t *testing.T) {
	t.Parallel()
	svc, err := sessionsprotocol.NewService(&fakeProjector{})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	if svc.HasTitleSetter() {
		t.Error("HasTitleSetter() = true on a Service built without one")
	}
	_, serr := svc.SetTitle(context.Background(), prototypes.SessionsSetTitleRequest{
		Identity: scope("t1", "u1", "s-caller"), SessionID: "s-target", Title: "x",
	})
	if !errors.Is(serr, sessionsprotocol.ErrTitleSetUnsupported) {
		t.Fatalf("SetTitle err=%v, want ErrTitleSetUnsupported", serr)
	}
}
