package engine_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	auditpatterns "github.com/hurtener/Harbor/internal/audit/drivers/patterns"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/tasks"
	"github.com/hurtener/Harbor/internal/tasks/engine"
)

// `caller_memory` is caller-controlled content that the whole-record marshal
// persists into the StateStore on disk, exactly like Description and Query.
// Those two go through the audit redactor; this one was copied RAW, so a
// secret in it landed on disk unredacted — and did so INCONSISTENTLY, which is
// the part that must not survive: an operator who sees `query` redacted will
// reasonably infer the same of `caller_memory`.

// cmRedactSecret is a dummy bearer token shaped like the real thing so the
// redactor's `bearer_in_value` rule fires. Not a credential — a fixture.
const cmRedactSecret = "Bearer sk-abcdefghijklmnopqrstuvwxyz0123456789"

func cmQuad() identity.Quadruple {
	return identity.Quadruple{Identity: identity.Identity{
		TenantID: "t-cm", UserID: "u-cm", SessionID: "s-cm",
	}}
}

// cmCtx is a context carrying the caller-memory test identity — Get is
// identity-mandatory.
func cmCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, err := identity.With(context.Background(), cmQuad().Identity)
	if err != nil {
		t.Fatalf("identity.With: %v", err)
	}
	return ctx
}

func newCMEngine(t *testing.T) *engine.Engine {
	t.Helper()
	bus := mkBus(t)
	eng, err := engine.New(bus, auditpatterns.New(), &memBackend{})
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		_ = eng.Close(ctx)
		_ = bus.Close(ctx)
	})
	return eng
}

// TestSpawn_CallerMemoryIsRedactedLikeItsSiblings is the load-bearing test:
// against the REAL patterns redactor, all three caller-controlled fields on a
// spawned task come back redacted, and the whole-record marshal — the bytes a
// durable driver writes to disk — contains no fragment of the secret.
//
// Mutation: restore the raw copy (`callerMemory = append([]byte(nil),
// req.CallerMemory...)`) and this fails on the caller-memory assertions while
// the description / query assertions stay green — which is precisely the
// inconsistency that shipped.
func TestSpawn_CallerMemoryIsRedactedLikeItsSiblings(t *testing.T) {
	ctx := cmCtx(t)
	eng := newCMEngine(t)

	mem, err := json.Marshal(map[string]any{
		"token":    cmRedactSecret,
		"recalled": []any{map[string]any{"role": "user", "text": "please use " + cmRedactSecret}},
	})
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}

	h, err := eng.Spawn(ctx, tasks.SpawnRequest{
		Identity:     cmQuad(),
		Kind:         tasks.KindForeground,
		Description:  "desc " + cmRedactSecret,
		Query:        "please use " + cmRedactSecret,
		CallerMemory: mem,
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	task, err := eng.Get(ctx, h.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	// The siblings — the baseline this field must match.
	if strings.Contains(task.Description, "sk-abcdefghijklmnopqrstuvwxyz0123456789") {
		t.Fatal("fixture broken: Description was not redacted, so the comparison is meaningless")
	}
	if strings.Contains(task.Query, "sk-abcdefghijklmnopqrstuvwxyz0123456789") {
		t.Fatal("fixture broken: Query was not redacted, so the comparison is meaningless")
	}

	// The field under test.
	if strings.Contains(string(task.CallerMemory), "sk-abcdefghijklmnopqrstuvwxyz0123456789") {
		t.Fatalf("CallerMemory persisted the secret VERBATIM while its siblings were redacted: %s", task.CallerMemory)
	}

	// The at-rest claim, made against the bytes the durable driver actually
	// writes: the driver whole-record-marshals the Task.
	record, err := json.Marshal(task)
	if err != nil {
		t.Fatalf("marshal task record: %v", err)
	}
	if strings.Contains(string(record), "sk-abcdefghijklmnopqrstuvwxyz0123456789") {
		t.Fatalf("the durable whole-record marshal contains the raw secret: %s", record)
	}
}

// TestSpawn_CallerMemoryRedactionPreservesStructure proves the redaction is
// not lossy in the way that would matter: it walks the DECODED value, so
// objects, arrays, numbers, booleans and null survive structurally intact and
// only secret-shaped keys and inline `Bearer …` values are replaced.
//
// This is the evidence behind choosing redaction over "document the raw
// behaviour instead": the alternative was on the table precisely because
// redacting structured JSON through a text redactor COULD have been lossy. It
// is not — the engine already takes this path for MarkComplete's result value.
func TestSpawn_CallerMemoryRedactionPreservesStructure(t *testing.T) {
	ctx := cmCtx(t)
	eng := newCMEngine(t)

	raw := json.RawMessage(`{"count":7,"flag":true,"nil":null,"list":[1,2,3],` +
		`"nested":{"ok":"value","note":"harmless prose"},"token":"` + cmRedactSecret + `"}`)

	h, err := eng.Spawn(ctx, tasks.SpawnRequest{
		Identity: cmQuad(), Kind: tasks.KindForeground, Query: "structure", CallerMemory: raw,
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	task, err := eng.Get(ctx, h.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(task.CallerMemory, &got); err != nil {
		t.Fatalf("redacted caller memory is not valid JSON any more: %v (%s)", err, task.CallerMemory)
	}
	if got["count"] != float64(7) {
		t.Errorf("number lost: %#v", got["count"])
	}
	if got["flag"] != true {
		t.Errorf("boolean lost: %#v", got["flag"])
	}
	if v, ok := got["nil"]; !ok || v != nil {
		t.Errorf("explicit null lost: present=%v %#v", ok, v)
	}
	list, ok := got["list"].([]any)
	if !ok || len(list) != 3 {
		t.Errorf("array lost: %#v", got["list"])
	}
	nested, ok := got["nested"].(map[string]any)
	if !ok || nested["ok"] != "value" || nested["note"] != "harmless prose" {
		t.Errorf("nested object lost or altered: %#v", got["nested"])
	}
	if got["token"] == cmRedactSecret {
		t.Error("the secret-shaped key survived redaction")
	}
}

// TestSpawn_CallerMemoryRedactionDoesNotBreakIdempotency proves the redaction
// does not turn an honest retry into a false ErrIdempotencyConflict.
//
// The stored field is post-redaction; the request's is raw. A direct byte
// compare between them (which the conflict check used to do) would now differ
// for any payload containing anything secret-shaped. The content identity is
// carried by the PRE-redaction hash instead — the same treatment Description
// and Query already get.
func TestSpawn_CallerMemoryRedactionDoesNotBreakIdempotency(t *testing.T) {
	ctx := cmCtx(t)
	eng := newCMEngine(t)

	mem := json.RawMessage(`{"token":"` + cmRedactSecret + `"}`)
	req := tasks.SpawnRequest{
		Identity: cmQuad(), Kind: tasks.KindForeground, Query: "retry me",
		IdempotencyKey: "same-key", CallerMemory: mem,
	}
	first, err := eng.Spawn(ctx, req)
	if err != nil {
		t.Fatalf("first Spawn: %v", err)
	}
	second, err := eng.Spawn(ctx, req)
	if err != nil {
		t.Fatalf("an IDENTICAL retry was refused: %v — the redacted stored field was compared against the raw request", err)
	}
	if !second.Reused || second.ID != first.ID {
		t.Fatalf("retry did not reuse the task: %+v vs %+v", second, first)
	}

	// The conflict half still works: DIFFERENT caller memory under the same
	// key is still loud, because the content hash folds the pre-redaction
	// bytes.
	diff := req
	diff.CallerMemory = json.RawMessage(`{"token":"Bearer sk-zyxwvutsrqponmlkjihgfedcba9876543210"}`)
	if _, err := eng.Spawn(ctx, diff); !errors.Is(err, tasks.ErrIdempotencyConflict) {
		t.Fatalf("same key + DIFFERENT caller memory = %v, want ErrIdempotencyConflict — two payloads whose redacted forms collide must still conflict", err)
	}
}

// TestSpawn_MalformedCallerMemoryRefusedLoud pins the boundary the redaction
// moved. The redactor's raw-JSON path is deliberately tolerant of non-JSON
// bytes (it re-quotes them as a string), which is right for a tool result and
// wrong here: it would have turned a malformed document into a valid one, the
// whole-record marshal would then have SUCCEEDED, and an unusable row would
// have persisted with the failure silently relocated to whatever read it.
//
// So the refusal is explicit and runs BEFORE the redactor.
func TestSpawn_MalformedCallerMemoryRefusedLoud(t *testing.T) {
	ctx := cmCtx(t)
	eng := newCMEngine(t)

	_, err := eng.Spawn(ctx, tasks.SpawnRequest{
		Identity: cmQuad(), Kind: tasks.KindForeground, Query: "malformed",
		CallerMemory: json.RawMessage(`{"unterminated":`),
	})
	if err == nil {
		t.Fatal("Spawn accepted a malformed caller_memory payload — it must fail loud, never persist an unusable row")
	}
	if !errors.Is(err, tasks.ErrInvalidRequest) {
		t.Fatalf("Spawn error = %v, want tasks.ErrInvalidRequest", err)
	}
	if !strings.Contains(err.Error(), "caller_memory") {
		t.Fatalf("the refusal does not name the offending field: %v", err)
	}
}

// TestSpawn_AbsentCallerMemoryStaysNil is the additivity guard: a spawn that
// carried no caller memory must still round-trip as nil, so the `omitempty`
// marshal elides it and a durable-backed task does not hydrate with a
// `caller_supplied` key holding JSON null.
func TestSpawn_AbsentCallerMemoryStaysNil(t *testing.T) {
	ctx := cmCtx(t)
	eng := newCMEngine(t)

	h, err := eng.Spawn(ctx, tasks.SpawnRequest{
		Identity: cmQuad(), Kind: tasks.KindForeground, Query: "no memory",
	})
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	task, err := eng.Get(ctx, h.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if task.CallerMemory != nil {
		t.Fatalf("absent caller memory hydrated as %q, want nil", task.CallerMemory)
	}
	record, err := json.Marshal(task)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(record), "CallerMemory") {
		t.Fatalf("the omitempty elision broke: %s", record)
	}
}
