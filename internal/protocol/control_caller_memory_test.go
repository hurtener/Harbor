// control_caller_memory_test.go — the `start` request's caller-memory
// edge validation (Phase 219 / D-364).
//
// The suite drives the REAL ControlSurface over the REAL task registry
// (inprocess over a real in-mem StateStore, a real in-mem bus, a real
// audit redactor) — nothing on the seam is faked, so the no-task-on-
// refusal property is asserted against the registry that would actually
// have created the task (CLAUDE.md §17.3 #1).
package protocol_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/config"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/protocol/types"
	"github.com/hurtener/Harbor/internal/runtime/steering"
	"github.com/hurtener/Harbor/internal/state"
	"github.com/hurtener/Harbor/internal/tasks"
)

// callerMemoryCap mirrors the unexported protocol.maxCallerMemoryBytes.
// Redeclaring it is deliberate: the tests below assert behaviour AT the
// boundary from outside the package, and a test that read the constant
// through the package under test could not catch a constant that moved.
const callerMemoryCap = 32 * 1024

// callerMemoryFixture is the caller-memory rig.
type callerMemoryFixture struct {
	surface *protocol.ControlSurface
	tasks   tasks.TaskRegistry
}

type callerMemoryAgentResolver struct{}

func (callerMemoryAgentResolver) ResolveAgent(context.Context, identity.Identity, string) (bool, error) {
	return true, nil
}

func (callerMemoryAgentResolver) EffectiveAgentID(requested string) (string, error) {
	if requested != "" {
		return requested, nil
	}
	return "caller-memory-default", nil
}

func newCallerMemoryFixture(t *testing.T) *callerMemoryFixture {
	t.Helper()

	red := auditpatterns.New()
	bus, err := events.Open(context.Background(), config.EventsConfig{
		Driver:                   "inmem",
		MaxSubscribersPerSession: 16,
		SubscriberBufferSize:     256,
		IdleTimeout:              60 * time.Second,
		DropWindow:               time.Second,
		ReplayBufferSize:         256,
	}, red)
	if err != nil {
		t.Fatalf("events.Open: %v", err)
	}
	t.Cleanup(func() { _ = bus.Close(context.Background()) })

	store, err := state.Open(context.Background(), config.StateConfig{Driver: "inmem"})
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}
	t.Cleanup(func() { _ = store.Close(context.Background()) })

	taskReg, err := tasks.Open(context.Background(), tasks.Dependencies{
		Store: store, Bus: bus, Redactor: red,
		Cfg: config.TasksConfig{Driver: "inprocess"},
	})
	if err != nil {
		t.Fatalf("tasks.Open: %v", err)
	}
	t.Cleanup(func() { _ = taskReg.Close(context.Background()) })

	surface, err := protocol.NewControlSurface(taskReg, steering.NewRegistry(),
		protocol.WithAgentResolver(callerMemoryAgentResolver{}))
	if err != nil {
		t.Fatalf("protocol.NewControlSurface: %v", err)
	}
	return &callerMemoryFixture{surface: surface, tasks: taskReg}
}

// start dispatches a `start` carrying the supplied raw caller_memory
// bytes. A nil raw omits the field entirely.
func (f *callerMemoryFixture) start(t *testing.T, id identity.Identity, raw json.RawMessage, key string) (*types.StartResponse, error) {
	t.Helper()
	ctx := auth.WithAgentReach(authCtx(t, id), []string{"caller-memory-default"})
	resp, err := f.surface.Dispatch(ctx, methods.MethodStart, &types.StartRequest{
		Identity:       types.IdentityScope{Tenant: id.TenantID, User: id.UserID, Session: id.SessionID},
		Query:          "hello",
		IdempotencyKey: key,
		CallerMemory:   raw,
	})
	if err != nil {
		return nil, err
	}
	sr, ok := resp.(*types.StartResponse)
	if !ok {
		t.Fatalf("start returned %T, want *types.StartResponse", resp)
	}
	return sr, nil
}

// rawOfSize builds a syntactically valid JSON object whose encoded byte
// length is exactly n.
func rawOfSize(t *testing.T, n int) json.RawMessage {
	t.Helper()
	const wrapper = `{"blob":""}`
	if n < len(wrapper) {
		t.Fatalf("rawOfSize(%d): below the minimum encodable size %d", n, len(wrapper))
	}
	raw := json.RawMessage(`{"blob":"` + strings.Repeat("x", n-len(wrapper)) + `"}`)
	if len(raw) != n {
		t.Fatalf("rawOfSize(%d): built %d bytes", n, len(raw))
	}
	if !json.Valid(raw) {
		t.Fatalf("rawOfSize(%d): built an invalid document", n)
	}
	return raw
}

// TestDispatchStart_CallerMemory_EdgeTable is the admission table.
func TestDispatchStart_CallerMemory_EdgeTable(t *testing.T) {
	caller := agentIdent("tenant-a", "user-1", "session-x")

	for _, tc := range []struct {
		name     string
		raw      json.RawMessage
		wantCode protoerrors.Code // "" ⇒ admitted
		// wantNamesField asserts the refusal text names `caller_memory`.
		// It is load-bearing: the control transport's whole-body cap
		// answers the SAME CodeInvalidRequest, so the field name is the
		// only thing that tells the two apart.
		wantNamesField bool
	}{
		{name: "AbsentIsTheUnchangedPath", raw: nil},
		{name: "ValidObject", raw: json.RawMessage(`{"recalled":[{"user":"hi"}]}`)},
		{name: "ValidArray", raw: json.RawMessage(`[{"user":"hi"},{"user":"bye"}]`)},
		{name: "ValidString", raw: json.RawMessage(`"a recalled note"`)},
		{name: "ExactlyAtCapIsAdmitted", raw: rawOfSize(t, callerMemoryCap)},
		{
			name: "OneByteOverCapIsRefused", raw: rawOfSize(t, callerMemoryCap+1),
			wantCode: protoerrors.CodeInvalidRequest, wantNamesField: true,
		},
		{
			name: "ExplicitNullIsRefused", raw: json.RawMessage(`null`),
			wantCode: protoerrors.CodeInvalidRequest, wantNamesField: true,
		},
		{
			name: "MalformedDocumentIsRefused", raw: json.RawMessage(`{"unterminated":`),
			wantCode: protoerrors.CodeInvalidRequest, wantNamesField: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCallerMemoryFixture(t)
			resp, err := f.start(t, caller, tc.raw, "")
			if tc.wantCode == "" {
				if err != nil {
					t.Fatalf("start: unexpected refusal: %v", err)
				}
				task, gErr := f.tasks.Get(mustIdentCtx(t, caller), tasks.TaskID(resp.TaskID))
				if gErr != nil {
					t.Fatalf("tasks.Get: %v", gErr)
				}
				if !bytes.Equal(task.CallerMemory, tc.raw) {
					t.Fatalf("task.CallerMemory = %q, want %q", task.CallerMemory, tc.raw)
				}
				return
			}
			if err == nil {
				t.Fatalf("start: want %s, got a spawned task %q", tc.wantCode, resp.TaskID)
			}
			if got := codeOf(t, err); got != tc.wantCode {
				t.Fatalf("code = %s, want %s", got, tc.wantCode)
			}
			if tc.wantNamesField && !strings.Contains(err.Error(), "caller_memory") {
				t.Fatalf("refusal %q does not name caller_memory — indistinguishable from the transport's identical envelope refusal", err.Error())
			}
		})
	}
}

// TestDispatchStart_CallerMemory_RefusedBeforeSpawn is the load-bearing
// half of the refusal contract: a status code alone would not catch a
// refusal that happened AFTER the task was created.
func TestDispatchStart_CallerMemory_RefusedBeforeSpawn(t *testing.T) {
	f := newCallerMemoryFixture(t)
	caller := agentIdent("tenant-refuse", "user-1", "session-x")

	if got := countTasks(t, f.tasks, caller); got != 0 {
		t.Fatalf("precondition: fresh identity owns %d tasks, want 0", got)
	}
	for i, raw := range []json.RawMessage{
		rawOfSize(t, callerMemoryCap+1),
		json.RawMessage(`null`),
		json.RawMessage(`{"unterminated":`),
	} {
		if _, err := f.start(t, caller, raw, fmt.Sprintf("refuse-%d", i)); err == nil {
			t.Fatalf("payload %d was admitted, want a refusal", i)
		}
	}
	if got := countTasks(t, f.tasks, caller); got != 0 {
		t.Fatalf("three refused starts created %d task(s) — a refused start must not reach Spawn", got)
	}
}

// TestDispatchStart_CallerMemory_IncompleteIdentityRefusedFirst pins the
// ordering: identity is validated before the field is inspected, so an
// over-cap payload from an unidentified caller is an identity refusal,
// never an invalid_request that leaks that the edge read the body.
func TestDispatchStart_CallerMemory_IncompleteIdentityRefusedFirst(t *testing.T) {
	f := newCallerMemoryFixture(t)
	// A verified ctx identity is present (the auth middleware's shape) but
	// the BODY's triple is incomplete — the edge validates the body triple.
	resp, err := f.surface.Dispatch(
		authCtx(t, agentIdent("tenant-a", "user-1", "session-x")),
		methods.MethodStart,
		&types.StartRequest{
			Identity:     types.IdentityScope{Tenant: "tenant-a", User: "user-1"},
			Query:        "hello",
			CallerMemory: rawOfSize(t, callerMemoryCap+1),
		})
	if err == nil {
		t.Fatalf("start with an incomplete triple was admitted (task %v)", resp)
	}
	if got := codeOf(t, err); got != protoerrors.CodeIdentityRequired {
		t.Fatalf("code = %s, want %s — the field must not be inspected before identity", got, protoerrors.CodeIdentityRequired)
	}
}

// TestDispatchStart_CallerMemory_FoldsIntoIdempotencyIdentity asserts a
// reused key carrying DIFFERENT caller memory is a loud conflict rather
// than a silent adoption of the first payload — the `output_schema` and
// `agent_id` precedent.
func TestDispatchStart_CallerMemory_FoldsIntoIdempotencyIdentity(t *testing.T) {
	f := newCallerMemoryFixture(t)
	caller := agentIdent("tenant-idem", "user-1", "session-x")

	first, err := f.start(t, caller, json.RawMessage(`{"note":"alpha"}`), "shared-key")
	if err != nil {
		t.Fatalf("first start: %v", err)
	}

	// Same key, same bytes → a genuine retry.
	retry, err := f.start(t, caller, json.RawMessage(`{"note":"alpha"}`), "shared-key")
	if err != nil {
		t.Fatalf("genuine retry was refused: %v", err)
	}
	if retry.TaskID != first.TaskID || !retry.Reused {
		t.Fatalf("retry returned task %q reused=%v, want %q reused=true", retry.TaskID, retry.Reused, first.TaskID)
	}

	// Same key, DIFFERENT bytes → a loud conflict.
	if _, err := f.start(t, caller, json.RawMessage(`{"note":"beta"}`), "shared-key"); err == nil {
		t.Fatal("a reused idempotency key with different caller_memory was silently adopted, want a conflict")
	}
}

// TestStartRequest_CallerMemory_AbsentIsByteIdenticalOnTheWire pins the
// additive-field contract: an omitted field produces the exact bytes a
// Runtime without the field produced. Golden-compared, not asserted.
func TestStartRequest_CallerMemory_AbsentIsByteIdenticalOnTheWire(t *testing.T) {
	req := types.StartRequest{
		Identity: types.IdentityScope{Tenant: "tenant-a", User: "user-1", Session: "session-x"},
		Query:    "hello",
	}
	got, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	const want = `{"identity":{"tenant":"tenant-a","user":"user-1","session":"session-x"},"query":"hello"}`
	if string(got) != want {
		t.Fatalf("omitted caller_memory changed the wire shape:\n got=%s\nwant=%s", got, want)
	}

	// And an explicit null on the wire decodes to a NON-nil RawMessage,
	// which is what makes "explicitly null" distinguishable from
	// "absent" at the edge. If this ever collapses to nil the null
	// refusal above becomes unreachable.
	var decoded types.StartRequest
	if err := json.Unmarshal([]byte(`{"caller_memory":null}`), &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.CallerMemory == nil {
		t.Fatal("an explicit caller_memory:null decoded to nil — indistinguishable from absent, so the null refusal is unreachable")
	}
}
