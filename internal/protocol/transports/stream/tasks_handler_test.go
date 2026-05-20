package stream_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	eventsinmem "github.com/hurtener/Harbor/internal/events/drivers/inmem"
	stateinmem "github.com/hurtener/Harbor/internal/state/drivers/inmem"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/transports/stream"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/tasks"
	_ "github.com/hurtener/Harbor/internal/tasks/drivers/inprocess"
	tasksprotocol "github.com/hurtener/Harbor/internal/tasks/protocol"
)

// tasksHandlerID is a documented dummy identity triple — no secrets.
var tasksHandlerID = identity.Identity{TenantID: "t-th", UserID: "u-th", SessionID: "s-th"}

// newTasksHandler builds a TasksHandler over an in-process registry
// seeded with the given task statuses.
func newTasksHandler(t *testing.T, seed ...tasks.TaskStatus) (*stream.TasksHandler, tasks.TaskRegistry) {
	t.Helper()
	store, err := stateinmem.New(config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state store: %v", err)
	}
	redactor := auditpatterns.New()
	bus, err := eventsinmem.New(config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     256,
		IdleTimeout:              60 * time.Second,
		DropWindow:               time.Second,
		ReplayBufferSize:         512,
	}, redactor)
	if err != nil {
		t.Fatalf("events inmem New: %v", err)
	}
	reg, err := tasks.OpenDriver("inprocess", tasks.Dependencies{
		Store:    store,
		Bus:      bus,
		Redactor: redactor,
		Cfg:      config.TasksConfig{Driver: "inprocess"},
	})
	if err != nil {
		t.Fatalf("OpenDriver: %v", err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		_ = reg.Close(ctx)
		_ = bus.Close(ctx)
		_ = store.Close(ctx)
	})

	idCtx, err := identity.With(context.Background(), tasksHandlerID)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	for _, st := range seed {
		h, serr := reg.Spawn(idCtx, tasks.SpawnRequest{
			Identity:    identity.Quadruple{Identity: tasksHandlerID},
			Kind:        tasks.KindForeground,
			Description: "seeded task",
			Query:       "q",
		})
		if serr != nil {
			t.Fatalf("Spawn: %v", serr)
		}
		switch st {
		case tasks.StatusRunning:
			if err := reg.MarkRunning(idCtx, h.ID); err != nil {
				t.Fatalf("MarkRunning: %v", err)
			}
		case tasks.StatusPending:
			// already pending
		}
	}

	proj, err := tasksprotocol.NewRegistryProjector(reg)
	if err != nil {
		t.Fatalf("NewRegistryProjector: %v", err)
	}
	svc, err := tasksprotocol.NewService(proj)
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	h, err := stream.NewTasksHandler(svc)
	if err != nil {
		t.Fatalf("NewTasksHandler: %v", err)
	}
	return h, reg
}

// doTasksRequest issues a POST /v1/tasks/{verb} against the handler.
func doTasksRequest(t *testing.T, h http.Handler, verb, body string, id *identity.Identity, scopes []auth.Scope) (int, []byte) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/tasks/"+verb, strings.NewReader(body))
	req.SetPathValue("method", verb)
	if id != nil {
		req.Header.Set(stream.HeaderTenant, id.TenantID)
		req.Header.Set(stream.HeaderUser, id.UserID)
		req.Header.Set(stream.HeaderSession, id.SessionID)
	}
	if scopes != nil {
		req = req.WithContext(auth.WithScopes(req.Context(), scopes))
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.Bytes()
}

func TestNewTasksHandler_NilService_FailsLoudly(t *testing.T) {
	if _, err := stream.NewTasksHandler(nil); err == nil {
		t.Fatal("NewTasksHandler(nil) did not fail")
	}
}

func TestTasksHandler_List_HappyPath(t *testing.T) {
	h, _ := newTasksHandler(t, tasks.StatusRunning, tasks.StatusPending)
	status, body := doTasksRequest(t, h, "list", "{}", &tasksHandlerID, nil)
	if status != http.StatusOK {
		t.Fatalf("tasks/list status = %d, want 200; body=%s", status, body)
	}
	var resp prototypes.TaskListResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(resp.Rows) != 2 {
		t.Errorf("tasks/list returned %d rows, want 2", len(resp.Rows))
	}
}

func TestTasksHandler_List_StatusFilter(t *testing.T) {
	h, _ := newTasksHandler(t, tasks.StatusRunning, tasks.StatusPending, tasks.StatusPending)
	status, body := doTasksRequest(t, h, "list",
		`{"filter":{"statuses":["running"]}}`, &tasksHandlerID, nil)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", status, body)
	}
	var resp prototypes.TaskListResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, row := range resp.Rows {
		if row.Status != prototypes.TaskStatusRunning {
			t.Errorf("status filter leaked %q", row.Status)
		}
	}
}

func TestTasksHandler_List_MissingIdentity_401(t *testing.T) {
	h, _ := newTasksHandler(t)
	status, body := doTasksRequest(t, h, "list", "{}", nil, nil)
	if status != http.StatusUnauthorized {
		t.Fatalf("tasks/list without identity status = %d, want 401", status)
	}
	var errBody protoerrors.Error
	if err := json.Unmarshal(body, &errBody); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if errBody.Code != protoerrors.CodeIdentityRequired {
		t.Errorf("code = %q, want %q", errBody.Code, protoerrors.CodeIdentityRequired)
	}
}

func TestTasksHandler_List_CrossTenant_NoAdmin_403(t *testing.T) {
	h, _ := newTasksHandler(t, tasks.StatusRunning)
	body := `{"filter":{"identities":[{"tenant":"t-th"},{"tenant":"other"}]}}`
	status, raw := doTasksRequest(t, h, "list", body, &tasksHandlerID, nil)
	if status != http.StatusForbidden {
		t.Fatalf("cross-tenant without admin: status = %d, want 403; body=%s", status, raw)
	}
	var errBody protoerrors.Error
	if err := json.Unmarshal(raw, &errBody); err != nil {
		t.Fatalf("decode error body: %v", err)
	}
	if errBody.Code != protoerrors.CodeScopeMismatch {
		t.Errorf("code = %q, want %q", errBody.Code, protoerrors.CodeScopeMismatch)
	}
}

func TestTasksHandler_List_CrossTenant_WithAdmin_200(t *testing.T) {
	h, _ := newTasksHandler(t, tasks.StatusRunning)
	body := `{"filter":{"identities":[{"tenant":"t-th"},{"tenant":"other"}]}}`
	status, raw := doTasksRequest(t, h, "list", body, &tasksHandlerID,
		[]auth.Scope{auth.ScopeAdmin})
	if status != http.StatusOK {
		t.Fatalf("cross-tenant with admin: status = %d, want 200; body=%s", status, raw)
	}
}

func TestTasksHandler_Get_HappyPath(t *testing.T) {
	h, reg := newTasksHandler(t)
	idCtx, _ := identity.With(context.Background(), tasksHandlerID)
	handle, err := reg.Spawn(idCtx, tasks.SpawnRequest{
		Identity:    identity.Quadruple{Identity: tasksHandlerID},
		Kind:        tasks.KindForeground,
		Description: "get me",
		Query:       "q",
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	status, raw := doTasksRequest(t, h, "get",
		`{"id":"`+string(handle.ID)+`"}`, &tasksHandlerID, nil)
	if status != http.StatusOK {
		t.Fatalf("tasks/get status = %d, want 200; body=%s", status, raw)
	}
	var detail prototypes.TaskDetail
	if err := json.Unmarshal(raw, &detail); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if detail.Task.ID != string(handle.ID) {
		t.Errorf("task id = %q, want %q", detail.Task.ID, handle.ID)
	}
}

func TestTasksHandler_Get_UnknownID_404(t *testing.T) {
	h, _ := newTasksHandler(t)
	status, raw := doTasksRequest(t, h, "get", `{"id":"no-such-task"}`, &tasksHandlerID, nil)
	if status != http.StatusNotFound {
		t.Fatalf("unknown task: status = %d, want 404; body=%s", status, raw)
	}
	var errBody protoerrors.Error
	if err := json.Unmarshal(raw, &errBody); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if errBody.Code != protoerrors.CodeNotFound {
		t.Errorf("code = %q, want %q", errBody.Code, protoerrors.CodeNotFound)
	}
}

func TestTasksHandler_UnknownVerb_404(t *testing.T) {
	h, _ := newTasksHandler(t)
	status, _ := doTasksRequest(t, h, "teleport", "{}", &tasksHandlerID, nil)
	if status != http.StatusNotFound {
		t.Fatalf("unknown verb: status = %d, want 404", status)
	}
}

func TestTasksHandler_GET_405(t *testing.T) {
	h, _ := newTasksHandler(t)
	req := httptest.NewRequest(http.MethodGet, "/v1/tasks/list", nil)
	req.SetPathValue("method", "list")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /v1/tasks/list status = %d, want 405", rec.Code)
	}
}
