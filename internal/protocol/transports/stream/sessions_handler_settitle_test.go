package stream_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/transports/stream"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/sessions"
	sessionsprotocol "github.com/hurtener/Harbor/internal/sessions/protocol"
)

// stubTitleSetter is the deterministic TitleSetter the set_title-branch
// handler tests inject. The integration test drives the real registry.
type stubTitleSetter struct {
	calledWithID string
	err          error
}

func (s *stubTitleSetter) SetTitle(_ context.Context, id string, _ identity.Identity, _ string) error {
	s.calledWithID = id
	return s.err
}

func newSessionsSetTitleHandler(t *testing.T, ts sessionsprotocol.TitleSetter) *stream.SessionsHandler {
	t.Helper()
	opts := []sessionsprotocol.Option{}
	if ts != nil {
		opts = append(opts, sessionsprotocol.WithTitleSetter(ts))
	}
	svc, err := sessionsprotocol.NewService(&sessionsFakeProjector{}, opts...)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	h, err := stream.NewSessionsHandler(svc)
	if err != nil {
		t.Fatalf("NewSessionsHandler: %v", err)
	}
	return h
}

func TestSessionsHandler_SetTitle_OwnSession_200(t *testing.T) {
	ts := &stubTitleSetter{}
	h := newSessionsSetTitleHandler(t, ts)
	body := `{"identity":{"tenant":"t-sh","user":"u-sh","session":"s-sh"},"session_id":"s-sh","title":"My title"}`
	code, raw := doSessionsRequest(t, h, "set_title", body, &sessionsHandlerID, nil)
	if code != http.StatusOK {
		t.Fatalf("set_title: code = %d, body = %s", code, raw)
	}
	var resp prototypes.SessionsSetTitleResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.SessionID != "s-sh" || resp.Title != "My title" || resp.TitleSource != "manual" {
		t.Fatalf("set_title response = %+v", resp)
	}
	if ts.calledWithID != "s-sh" {
		t.Errorf("TitleSetter called with id=%q, want s-sh", ts.calledWithID)
	}
}

// TestSessionsHandler_SetTitle_SiblingSession_200 pins the (tenant, user)
// write scope at the wire layer: a session_id DIFFERENT from the caller's
// own connecting session (carried via X-Harbor-Session) is still
// accepted — the body-identity gate only constrains the body Identity,
// never the separate session_id field (D-288).
func TestSessionsHandler_SetTitle_SiblingSession_200(t *testing.T) {
	ts := &stubTitleSetter{}
	h := newSessionsSetTitleHandler(t, ts)
	body := `{"identity":{"tenant":"t-sh","user":"u-sh","session":"s-sh"},"session_id":"s-sibling","title":"Sibling title"}`
	code, raw := doSessionsRequest(t, h, "set_title", body, &sessionsHandlerID, nil)
	if code != http.StatusOK {
		t.Fatalf("set_title sibling: code = %d, body = %s", code, raw)
	}
	if ts.calledWithID != "s-sibling" {
		t.Errorf("TitleSetter called with id=%q, want s-sibling", ts.calledWithID)
	}
}

func TestSessionsHandler_SetTitle_ForeignBodyIdentity_401(t *testing.T) {
	ts := &stubTitleSetter{}
	h := newSessionsSetTitleHandler(t, ts)
	// Body names a tenant other than the verified identity — rejected
	// identity_required (401) by the body-identity gate, BEFORE the
	// TitleSetter is ever reached.
	body := `{"identity":{"tenant":"t-OTHER","user":"u-sh","session":"s-sh"},"session_id":"s-sh","title":"x"}`
	code, raw := doSessionsRequest(t, h, "set_title", body, &sessionsHandlerID, nil)
	if code != http.StatusUnauthorized {
		t.Fatalf("foreign-body-identity set_title: code = %d, want 401, body = %s", code, raw)
	}
	var e protoerrors.Error
	if err := json.Unmarshal(raw, &e); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if e.Code != protoerrors.CodeIdentityRequired {
		t.Fatalf("error code = %q, want %q", e.Code, protoerrors.CodeIdentityRequired)
	}
	if ts.calledWithID != "" {
		t.Error("TitleSetter was reached despite a foreign body identity")
	}
}

func TestSessionsHandler_SetTitle_InvalidTitle_400(t *testing.T) {
	ts := &stubTitleSetter{err: sessions.ErrInvalidTitle}
	h := newSessionsSetTitleHandler(t, ts)
	body := `{"identity":{"tenant":"t-sh","user":"u-sh","session":"s-sh"},"session_id":"s-sh","title":"x"}`
	code, raw := doSessionsRequest(t, h, "set_title", body, &sessionsHandlerID, nil)
	if code != http.StatusBadRequest {
		t.Fatalf("invalid-title set_title: code = %d, want 400, body = %s", code, raw)
	}
	var e protoerrors.Error
	if err := json.Unmarshal(raw, &e); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if e.Code != protoerrors.CodeInvalidRequest {
		t.Fatalf("error code = %q, want %q", e.Code, protoerrors.CodeInvalidRequest)
	}
}

func TestSessionsHandler_SetTitle_NotFound_404(t *testing.T) {
	ts := &stubTitleSetter{err: sessions.ErrSessionNotFound}
	h := newSessionsSetTitleHandler(t, ts)
	body := `{"identity":{"tenant":"t-sh","user":"u-sh","session":"s-sh"},"session_id":"s-nope","title":"x"}`
	code, raw := doSessionsRequest(t, h, "set_title", body, &sessionsHandlerID, nil)
	if code != http.StatusNotFound {
		t.Fatalf("not-found set_title: code = %d, want 404, body = %s", code, raw)
	}
	var e protoerrors.Error
	if err := json.Unmarshal(raw, &e); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if e.Code != protoerrors.CodeNotFound {
		t.Fatalf("error code = %q, want %q", e.Code, protoerrors.CodeNotFound)
	}
}

func TestSessionsHandler_SetTitle_NoTitleSetter_404(t *testing.T) {
	h := newSessionsSetTitleHandler(t, nil) // no TitleSetter wired
	body := `{"identity":{"tenant":"t-sh","user":"u-sh","session":"s-sh"},"session_id":"s-sh","title":"x"}`
	code, raw := doSessionsRequest(t, h, "set_title", body, &sessionsHandlerID, nil)
	if code != http.StatusNotFound {
		t.Fatalf("no-title-setter set_title: code = %d, want 404, body = %s", code, raw)
	}
	var e protoerrors.Error
	if err := json.Unmarshal(raw, &e); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if e.Code != protoerrors.CodeUnknownMethod {
		t.Fatalf("error code = %q, want %q", e.Code, protoerrors.CodeUnknownMethod)
	}
}

func TestSessionsHandler_SetTitle_UnknownField_400(t *testing.T) {
	h := newSessionsSetTitleHandler(t, &stubTitleSetter{})
	code, _ := doSessionsRequest(t, h, "set_title", `{"bogus":true}`, &sessionsHandlerID, nil)
	if code != http.StatusBadRequest {
		t.Fatalf("unknown-field set_title: code = %d, want 400", code)
	}
}

func TestSessionsHandler_SetTitle_MissingIdentity_401(t *testing.T) {
	h := newSessionsSetTitleHandler(t, &stubTitleSetter{})
	code, body := doSessionsRequest(t, h, "set_title", `{"session_id":"s-sh","title":"x"}`, nil, nil)
	if code != http.StatusUnauthorized {
		t.Fatalf("missing-identity set_title: code = %d, want 401, body = %s", code, body)
	}
}
