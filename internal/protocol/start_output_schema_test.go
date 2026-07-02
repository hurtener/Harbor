package protocol_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/tasks"
)

// TestDispatchStart_OutputSchema_EdgeRejectionTable pins the D-276
// reject-early contract: a present-but-empty, non-compiling, or over-cap
// output_schema is rejected with CodeInvalidRequest BEFORE any task
// spawns; a valid schema spawns a task carrying the field.
func TestDispatchStart_OutputSchema_EdgeRejectionTable(t *testing.T) {
	fx := newSurfaceFixture(t)

	scope := types.IdentityScope{Tenant: "tenant-a", User: "user-1", Session: "session-x"}
	idCtx, err := identity.With(context.Background(), identity.Identity{
		TenantID: scope.Tenant, UserID: scope.User, SessionID: scope.Session,
	})
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}

	// A 64 KiB+ blob to trip the size cap without depending on schema
	// semantics.
	overCap := json.RawMessage(`{"x":"` + strings.Repeat("a", 64*1024) + `"}`)

	cases := []struct {
		name       string
		schema     json.RawMessage
		wantReject bool
	}{
		{name: "present_but_empty", schema: json.RawMessage(``), wantReject: true},
		{name: "whitespace_only", schema: json.RawMessage("   \n"), wantReject: true},
		{name: "non_compiling_garbage", schema: json.RawMessage(`{"type":`), wantReject: true},
		{name: "over_cap", schema: overCap, wantReject: true},
		{name: "valid_schema_spawns", schema: json.RawMessage(`{"type":"object","required":["answer"]}`), wantReject: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, dErr := fx.surface.Dispatch(context.Background(), methods.MethodStart, &types.StartRequest{
				Identity:     scope,
				Query:        "classify",
				OutputSchema: tc.schema,
			})
			if tc.wantReject {
				if dErr == nil {
					t.Fatalf("want rejection, got a spawned task: %+v", resp)
				}
				var perr *protoerrors.Error
				if !errors.As(dErr, &perr) {
					t.Fatalf("want *protoerrors.Error, got %T: %v", dErr, dErr)
				}
				if perr.Code != protoerrors.CodeInvalidRequest {
					t.Fatalf("want CodeInvalidRequest, got %s: %v", perr.Code, perr)
				}
				return
			}
			if dErr != nil {
				t.Fatalf("valid schema must spawn, got error: %v", dErr)
			}
			sr, ok := resp.(*types.StartResponse)
			if !ok || sr.TaskID == "" {
				t.Fatalf("valid schema: want a spawned task with a TaskID, got %+v", resp)
			}
			task, gErr := fx.tasks.Get(idCtx, tasks.TaskID(sr.TaskID))
			if gErr != nil {
				t.Fatalf("tasks.Get: %v", gErr)
			}
			if string(task.OutputSchema) != string(tc.schema) {
				t.Fatalf("persisted OutputSchema = %s, want %s", task.OutputSchema, tc.schema)
			}
		})
	}
}

// TestDispatchStart_NoOutputSchema_SpawnsSchemaless pins zero-default:
// an absent output_schema spawns a task whose OutputSchema is nil.
func TestDispatchStart_NoOutputSchema_SpawnsSchemaless(t *testing.T) {
	fx := newSurfaceFixture(t)
	scope := types.IdentityScope{Tenant: "tenant-a", User: "user-1", Session: "session-y"}
	idCtx, err := identity.With(context.Background(), identity.Identity{
		TenantID: scope.Tenant, UserID: scope.User, SessionID: scope.Session,
	})
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	resp, dErr := fx.surface.Dispatch(context.Background(), methods.MethodStart, &types.StartRequest{
		Identity: scope,
		Query:    "hello",
	})
	if dErr != nil {
		t.Fatalf("start: %v", dErr)
	}
	sr := resp.(*types.StartResponse)
	task, gErr := fx.tasks.Get(idCtx, tasks.TaskID(sr.TaskID))
	if gErr != nil {
		t.Fatalf("tasks.Get: %v", gErr)
	}
	if len(task.OutputSchema) != 0 {
		t.Fatalf("schemaless spawn must carry a nil OutputSchema, got %s", task.OutputSchema)
	}
}
