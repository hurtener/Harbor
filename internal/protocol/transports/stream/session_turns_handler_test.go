package stream_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	"github.com/hurtener/Harbor/internal/protocol/transports/stream"
	turns "github.com/hurtener/Harbor/internal/sessions/turns"
	turnsprotocol "github.com/hurtener/Harbor/internal/sessions/turns/protocol"
)

// turnsHandlerID is a documented dummy identity triple — no secrets.
var turnsHandlerID = identity.Identity{TenantID: "t-tr", UserID: "u-tr", SessionID: "s-tr"}

// turnsFakeProjector is the deterministic Projector stand-in for the
// handler tests (the integration tests drive the real projection).
type turnsFakeProjector struct {
	rows    []turns.TurnRow
	ops     *turns.OpsTurnRow
	listErr error
	getErr  error
	opsErr  error
}

func (f *turnsFakeProjector) List(_ context.Context, id identity.Identity, opts turns.ListOptions) (turns.Page, error) {
	if f.listErr != nil {
		return turns.Page{}, f.listErr
	}
	var rows []turns.TurnRow
	for _, r := range f.rows {
		if r.SessionID == id.SessionID {
			rows = append(rows, r)
		}
	}
	return turns.Page{
		Rows:       rows,
		HasMore:    false,
		AsOf:       time.Date(2026, 5, 19, 9, 0, 0, 0, time.UTC),
		Snapshot:   7,
		Remaining:  0,
		CountExact: true,
		Complete:   true,
	}, nil
}

func (f *turnsFakeProjector) Get(_ context.Context, id identity.Identity, turnID turns.TurnID) (turns.TurnRow, error) {
	if f.getErr != nil {
		return turns.TurnRow{}, f.getErr
	}
	for _, r := range f.rows {
		if r.TurnID == turnID && r.SessionID == id.SessionID {
			return r, nil
		}
	}
	return turns.TurnRow{}, turns.ErrTurnNotFound
}

func (f *turnsFakeProjector) OpsTurn(_ context.Context, id identity.Identity, turnID turns.TurnID) (turns.OpsTurnRow, error) {
	if f.opsErr != nil {
		return turns.OpsTurnRow{}, f.opsErr
	}
	if f.ops == nil {
		return turns.OpsTurnRow{}, turns.ErrTurnNotFound
	}
	if f.ops.TurnID != turnID || f.ops.SessionID != id.SessionID {
		return turns.OpsTurnRow{}, turns.ErrTurnNotFound
	}
	return *f.ops, nil
}

func newTurnsHandler(t *testing.T, p *turnsFakeProjector) *stream.SessionTurnsHandler {
	t.Helper()
	svc, err := turnsprotocol.NewService(p)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	h, err := stream.NewSessionTurnsHandler(svc)
	if err != nil {
		t.Fatalf("NewSessionTurnsHandler: %v", err)
	}
	return h
}

// doTurnsRequest issues a POST /v1/sessions/turns/{verb} against the
// handler with the documented identity triple on the carrier headers.
func doTurnsRequest(t *testing.T, h http.Handler, verb, body string, scopes []auth.Scope) (int, []byte) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/sessions/turns/"+verb, strings.NewReader(body))
	req.Header.Set(stream.HeaderTenant, turnsHandlerID.TenantID)
	req.Header.Set(stream.HeaderUser, turnsHandlerID.UserID)
	req.Header.Set(stream.HeaderSession, turnsHandlerID.SessionID)
	if scopes != nil {
		req = req.WithContext(auth.WithScopes(req.Context(), scopes))
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes()
}

func TestNewSessionTurnsHandler_NilService_FailsLoudly(t *testing.T) {
	if _, err := stream.NewSessionTurnsHandler(nil); err == nil {
		t.Fatal("NewSessionTurnsHandler(nil) succeeded, want ErrSessionTurnsMisconfigured")
	}
}

func TestSessionTurnsHandler_List_ProjectsPage(t *testing.T) {
	row := turns.TurnRow{
		TurnID: turns.TurnID("task-1"), TaskID: "task-1", SessionID: "s-tr",
		Sequence: 2, TieBreaker: "task-1", Status: turns.StatusComplete, Sealed: true, Version: 3,
		StartedAt: time.Date(2026, 5, 19, 9, 0, 0, 0, time.UTC),
	}
	h := newTurnsHandler(t, &turnsFakeProjector{rows: []turns.TurnRow{row}})
	code, body := doTurnsRequest(t, h, "list", `{"session_id":"s-tr","limit":20}`, nil)
	if code != http.StatusOK {
		t.Fatalf("list status = %d, body=%s", code, body)
	}
	var out struct {
		Header struct {
			SessionID  string `json:"session_id"`
			SnapshotID uint64 `json:"snapshot_id"`
		} `json:"header"`
		Turns            []map[string]any `json:"turns"`
		Order            string           `json:"order"`
		PageCompleteness string           `json:"page_completeness"`
		CountExact       bool             `json:"count_exact"`
		ProtocolVersion  string           `json:"protocol_version"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Header.SessionID != "s-tr" || out.Header.SnapshotID != 7 {
		t.Errorf("header = %+v, want session s-tr snapshot 7", out.Header)
	}
	if len(out.Turns) != 1 {
		t.Fatalf("turns = %d rows, want 1", len(out.Turns))
	}
	if out.Turns[0]["turn_id"] != "task-1" || out.Turns[0]["status"] != "complete" {
		t.Errorf("turn row projection wrong: %v", out.Turns[0])
	}
	if out.Order != "newest_first" {
		t.Errorf("order = %q, want newest_first", out.Order)
	}
	// The explicit completeness + counter-status contract rides verbatim.
	if out.PageCompleteness != "complete" {
		t.Errorf("page_completeness = %q, want complete", out.PageCompleteness)
	}
	if !out.CountExact {
		t.Error("count_exact = false, want true (the page's counter status is explicit)")
	}
	if out.ProtocolVersion == "" {
		t.Error("protocol_version is empty")
	}
}

func TestSessionTurnsHandler_List_OperationsProjectionRejected(t *testing.T) {
	h := newTurnsHandler(t, &turnsFakeProjector{})
	code, body := doTurnsRequest(t, h, "list", `{"session_id":"s-tr","projection":"operations"}`, nil)
	if code != http.StatusBadRequest {
		t.Fatalf("list(operations) status = %d, want 400; body=%s", code, body)
	}
}

func TestSessionTurnsHandler_List_ForeignSessionIsNotFound(t *testing.T) {
	h := newTurnsHandler(t, &turnsFakeProjector{})
	code, body := doTurnsRequest(t, h, "list", `{"session_id":"s-foreign"}`, nil)
	if code != http.StatusNotFound {
		t.Fatalf("foreign-session list status = %d, want 404 (non-oracular); body=%s", code, body)
	}
}

func TestSessionTurnsHandler_Get_ConsumerLane(t *testing.T) {
	row := turns.TurnRow{
		TurnID: turns.TurnID("task-1"), TaskID: "task-1", SessionID: "s-tr",
		Sequence: 2, TieBreaker: "task-1", Status: turns.StatusComplete, Sealed: true, Version: 3,
	}
	h := newTurnsHandler(t, &turnsFakeProjector{rows: []turns.TurnRow{row}})
	code, body := doTurnsRequest(t, h, "get", `{"session_id":"s-tr","task_id":"task-1"}`, nil)
	if code != http.StatusOK {
		t.Fatalf("get status = %d, body=%s", code, body)
	}
	var out struct {
		Turn    *map[string]any `json:"turn"`
		OpsTurn *map[string]any `json:"ops_turn"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Turn == nil || (*out.Turn)["turn_id"] != "task-1" {
		t.Errorf("consumer turn projection wrong: %+v", out.Turn)
	}
	if out.OpsTurn != nil {
		t.Error("consumer lane must not populate ops_turn")
	}
}

func TestSessionTurnsHandler_Get_OperationsLaneRequiresScope(t *testing.T) {
	ops := &turns.OpsTurnRow{
		TurnID: turns.TurnID("task-1"), TaskID: "task-1", SessionID: "s-tr",
		Status: turns.StatusComplete, Sealed: true, Version: 3,
	}
	h := newTurnsHandler(t, &turnsFakeProjector{ops: ops})
	// No scope claim → the operations lane is refused 403.
	code, _ := doTurnsRequest(t, h, "get", `{"session_id":"s-tr","task_id":"task-1","projection":"operations"}`, nil)
	if code != http.StatusForbidden {
		t.Fatalf("operations get without scope = %d, want 403", code)
	}
	// Admin claim → the structurally distinct ops DTO is served.
	code, body := doTurnsRequest(t, h, "get",
		`{"session_id":"s-tr","task_id":"task-1","projection":"operations"}`, []auth.Scope{auth.ScopeAdmin})
	if code != http.StatusOK {
		t.Fatalf("operations get with admin = %d, body=%s", code, body)
	}
	var out struct {
		Turn    *map[string]any `json:"turn"`
		OpsTurn *map[string]any `json:"ops_turn"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.OpsTurn == nil {
		t.Fatal("operations lane returned no ops_turn DTO")
	}
	if (*out.OpsTurn)["turn_id"] != "task-1" {
		t.Errorf("ops turn projection wrong: %+v", out.OpsTurn)
	}
	if out.Turn != nil {
		t.Error("operations lane must not populate the consumer turn")
	}
}

func TestSessionTurnsHandler_Get_UnknownTurnIsNotFound(t *testing.T) {
	h := newTurnsHandler(t, &turnsFakeProjector{})
	code, _ := doTurnsRequest(t, h, "get", `{"session_id":"s-tr","task_id":"nope"}`, nil)
	if code != http.StatusNotFound {
		t.Fatalf("unknown turn = %d, want 404", code)
	}
}
