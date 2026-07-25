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

// stubEraser is the deterministic Eraser the delete-branch handler tests
// inject. The integration test drives the real cascade.
type stubEraser struct {
	resp prototypes.SessionsDeleteResponse
	err  error
}

func (s *stubEraser) Erase(_ context.Context, id identity.Identity) (prototypes.SessionsDeleteResponse, error) {
	if s.err != nil {
		return prototypes.SessionsDeleteResponse{}, s.err
	}
	resp := s.resp
	resp.SessionID = id.SessionID
	return resp, nil
}

func newSessionsDeleteHandler(t *testing.T, er sessionsprotocol.Eraser) *stream.SessionsHandler {
	t.Helper()
	opts := []sessionsprotocol.Option{}
	if er != nil {
		opts = append(opts, sessionsprotocol.WithEraser(er))
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

func TestSessionsHandler_Delete_OwnSession_200(t *testing.T) {
	er := &stubEraser{resp: prototypes.SessionsDeleteResponse{
		Deleted: true, StateRecordsDeleted: 4, ArtifactsDeleted: 2, MemoryPurged: true,
	}}
	h := newSessionsDeleteHandler(t, er)
	body := `{"identity":{"tenant":"t-sh","user":"u-sh","session":"s-sh"}}`
	code, raw := doSessionsRequest(t, h, "delete", body, &sessionsHandlerID, nil)
	if code != http.StatusOK {
		t.Fatalf("delete: code = %d, body = %s", code, raw)
	}
	var resp prototypes.SessionsDeleteResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.Deleted || resp.SessionID != "s-sh" || resp.StateRecordsDeleted != 4 {
		t.Fatalf("delete response = %+v", resp)
	}
}

func TestSessionsHandler_Delete_RunningTask_409(t *testing.T) {
	er := &stubEraser{err: sessions.ErrSessionRunning}
	h := newSessionsDeleteHandler(t, er)
	body := `{"identity":{"tenant":"t-sh","user":"u-sh","session":"s-sh"}}`
	code, raw := doSessionsRequest(t, h, "delete", body, &sessionsHandlerID, nil)
	if code != http.StatusConflict {
		t.Fatalf("running-task delete: code = %d, want 409, body = %s", code, raw)
	}
	var e protoerrors.Error
	if err := json.Unmarshal(raw, &e); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if e.Code != protoerrors.CodeSessionRunning {
		t.Fatalf("error code = %q, want %q", e.Code, protoerrors.CodeSessionRunning)
	}
}

func TestSessionsHandler_Delete_ForeignTarget_401(t *testing.T) {
	er := &stubEraser{resp: prototypes.SessionsDeleteResponse{Deleted: true}}
	h := newSessionsDeleteHandler(t, er)
	// Body names a tenant other than the verified identity — rejected
	// identity_required (401) by the body-identity gate, BEFORE the eraser.
	body := `{"identity":{"tenant":"t-OTHER","user":"u-sh","session":"s-sh"}}`
	code, raw := doSessionsRequest(t, h, "delete", body, &sessionsHandlerID, nil)
	if code != http.StatusUnauthorized {
		t.Fatalf("foreign-target delete: code = %d, want 401, body = %s", code, raw)
	}
	var e protoerrors.Error
	if err := json.Unmarshal(raw, &e); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if e.Code != protoerrors.CodeIdentityRequired {
		t.Fatalf("error code = %q, want %q", e.Code, protoerrors.CodeIdentityRequired)
	}
}

// TestSessionsHandler_Delete_ErasureRecordFailed_500 pins D-286's new
// mapping: a destructive-steps-done-but-record-incomplete failure
// (ErrErasureRecordFailed) surfaces as 500 with CodeRuntimeError — the
// existing generic failure code, but via an explicit branch naming the
// erasure-record failure (rather than a bland "request failed"), so an
// operator scanning logs/responses can tell this apart from an
// unrelated internal error and knows a retry converges.
func TestSessionsHandler_Delete_ErasureRecordFailed_500(t *testing.T) {
	er := &stubEraser{err: sessions.ErrErasureRecordFailed}
	h := newSessionsDeleteHandler(t, er)
	body := `{"identity":{"tenant":"t-sh","user":"u-sh","session":"s-sh"}}`
	code, raw := doSessionsRequest(t, h, "delete", body, &sessionsHandlerID, nil)
	if code != http.StatusInternalServerError {
		t.Fatalf("erasure-record-failed delete: code = %d, want 500, body = %s", code, raw)
	}
	var e protoerrors.Error
	if err := json.Unmarshal(raw, &e); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if e.Code != protoerrors.CodeRuntimeError {
		t.Fatalf("error code = %q, want %q", e.Code, protoerrors.CodeRuntimeError)
	}
}

func TestSessionsHandler_Delete_NoEraser_404(t *testing.T) {
	h := newSessionsDeleteHandler(t, nil) // no eraser wired
	body := `{"identity":{"tenant":"t-sh","user":"u-sh","session":"s-sh"}}`
	code, raw := doSessionsRequest(t, h, "delete", body, &sessionsHandlerID, nil)
	if code != http.StatusNotFound {
		t.Fatalf("no-eraser delete: code = %d, want 404, body = %s", code, raw)
	}
	var e protoerrors.Error
	if err := json.Unmarshal(raw, &e); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if e.Code != protoerrors.CodeUnknownMethod {
		t.Fatalf("error code = %q, want %q", e.Code, protoerrors.CodeUnknownMethod)
	}
}
