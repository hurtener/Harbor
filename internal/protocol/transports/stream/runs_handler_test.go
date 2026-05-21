package stream_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/transports/stream"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	runsprotocol "github.com/hurtener/Harbor/internal/runtime/runs/protocol"
)

// runsHandlerID is a documented dummy identity triple — no secrets.
var runsHandlerID = identity.Identity{TenantID: "t-rh", UserID: "u-rh", SessionID: "s-rh"}

func newRunsHandler(t *testing.T) (*stream.RunsHandler, *runsprotocol.Store) {
	t.Helper()
	store := runsprotocol.NewStore()
	svc, err := runsprotocol.NewService(store)
	if err != nil {
		t.Fatalf("runsprotocol.NewService: %v", err)
	}
	h, err := stream.NewRunsHandler(svc)
	if err != nil {
		t.Fatalf("NewRunsHandler: %v", err)
	}
	return h, store
}

// doRunsRequest issues a POST /v1/runs/{verb} against the handler. id
// (when non-nil) is carried via the X-Harbor-* headers.
func doRunsRequest(t *testing.T, h http.Handler, verb, body string, id *identity.Identity) (int, []byte) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/runs/"+verb, strings.NewReader(body))
	if id != nil {
		req.Header.Set(stream.HeaderTenant, id.TenantID)
		req.Header.Set(stream.HeaderUser, id.UserID)
		req.Header.Set(stream.HeaderSession, id.SessionID)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes()
}

func TestNewRunsHandler_NilService_FailsLoudly(t *testing.T) {
	if _, err := stream.NewRunsHandler(nil); err == nil {
		t.Fatal("NewRunsHandler(nil) did not fail")
	}
}

func TestRunsHandler_SetOverrides_HappyPath(t *testing.T) {
	h, store := newRunsHandler(t)
	code, body := doRunsRequest(t, h, "set_overrides",
		`{"overrides":{"session_id":"s-rh","reasoning_effort":"high","temperature":0.5}}`,
		&runsHandlerID)
	if code != http.StatusOK {
		t.Fatalf("set_overrides: code = %d, body = %s", code, body)
	}
	var resp prototypes.RunSetOverridesResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.AppliedAt.IsZero() {
		t.Error("AppliedAt is zero")
	}
	if resp.ProtocolVersion != prototypes.ProtocolVersion {
		t.Errorf("ProtocolVersion = %q, want %q", resp.ProtocolVersion, prototypes.ProtocolVersion)
	}
	po, ok := store.Peek(runsHandlerID)
	if !ok || po.ReasoningEffort == nil || *po.ReasoningEffort != "high" {
		t.Errorf("override not recorded in store: %+v ok=%v", po, ok)
	}
}

func TestRunsHandler_SetOverrides_DefaultsSessionIDFromIdentity(t *testing.T) {
	h, store := newRunsHandler(t)
	// No session_id in the body — the handler defaults it to the
	// verified session.
	code, body := doRunsRequest(t, h, "set_overrides",
		`{"overrides":{"reasoning_effort":"low"}}`, &runsHandlerID)
	if code != http.StatusOK {
		t.Fatalf("set_overrides: code = %d, body = %s", code, body)
	}
	if _, ok := store.Peek(runsHandlerID); !ok {
		t.Error("override not recorded — session_id default did not apply")
	}
}

func TestRunsHandler_SetOverrides_RejectsMissingIdentity(t *testing.T) {
	h, _ := newRunsHandler(t)
	code, body := doRunsRequest(t, h, "set_overrides",
		`{"overrides":{"session_id":"s-rh","reasoning_effort":"high"}}`, nil)
	if code != http.StatusUnauthorized {
		t.Fatalf("code = %d, want 401; body = %s", code, body)
	}
	assertRunsErrCode(t, body, protoerrors.CodeIdentityRequired)
}

func TestRunsHandler_SetOverrides_RejectsCrossSessionOverride(t *testing.T) {
	h, _ := newRunsHandler(t)
	// Verified session is s-rh; the override names a different session.
	code, body := doRunsRequest(t, h, "set_overrides",
		`{"overrides":{"session_id":"other-session","reasoning_effort":"high"}}`,
		&runsHandlerID)
	if code != http.StatusForbidden {
		t.Fatalf("code = %d, want 403; body = %s", code, body)
	}
	assertRunsErrCode(t, body, protoerrors.CodeScopeMismatch)
}

func TestRunsHandler_SetOverrides_RejectsBodyIdentityMismatch(t *testing.T) {
	h, _ := newRunsHandler(t)
	// The body claims a different tenant than the verified identity.
	code, body := doRunsRequest(t, h, "set_overrides",
		`{"identity":{"tenant":"other-tenant","user":"u-rh","session":"s-rh"},"overrides":{"session_id":"s-rh","reasoning_effort":"high"}}`,
		&runsHandlerID)
	if code != http.StatusUnauthorized {
		t.Fatalf("code = %d, want 401; body = %s", code, body)
	}
}

func TestRunsHandler_SetOverrides_RejectsInvalidOverride(t *testing.T) {
	h, _ := newRunsHandler(t)
	code, body := doRunsRequest(t, h, "set_overrides",
		`{"overrides":{"session_id":"s-rh","temperature":9.9}}`, &runsHandlerID)
	if code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400; body = %s", code, body)
	}
	assertRunsErrCode(t, body, protoerrors.CodeInvalidRequest)
}

func TestRunsHandler_RejectsGET(t *testing.T) {
	h, _ := newRunsHandler(t)
	req := httptest.NewRequest(http.MethodGet, "/v1/runs/set_overrides", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET code = %d, want 405", rec.Code)
	}
}

func TestRunsHandler_UnknownMethodRoute(t *testing.T) {
	h, _ := newRunsHandler(t)
	code, body := doRunsRequest(t, h, "bogus", `{}`, &runsHandlerID)
	if code != http.StatusNotFound {
		t.Fatalf("code = %d, want 404; body = %s", code, body)
	}
	assertRunsErrCode(t, body, protoerrors.CodeUnknownMethod)
}

func assertRunsErrCode(t *testing.T, body []byte, want protoerrors.Code) {
	t.Helper()
	var e protoerrors.Error
	if err := json.Unmarshal(body, &e); err != nil {
		t.Fatalf("decode error body: %v (body=%s)", err, body)
	}
	if e.Code != want {
		t.Errorf("error code = %q, want %q", e.Code, want)
	}
}
