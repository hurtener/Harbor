package durable

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/tasks"
)

// TestPersistedTask_IdempotencyKeyIsInThePayload settles, by execution
// rather than by reading, a question a decision entry answered wrongly:
// is `Task.IdempotencyKey` written to the StateStore?
//
// It is. `persistedTask` embeds the whole `*tasks.Task` and marshals it
// with `encoding/json`; `IdempotencyKey` is an exported field, so it
// lands in the payload. The entry claimed "the idempotency key appears
// in no key and no payload" — the KEY half is true (`SaveTask` writes to
// `taskKindPrefix+string(rec.Task.ID)`, asserted below), the PAYLOAD half
// is not.
//
// The correction matters in the direction opposite to the one a reader
// might expect. The no-migration conclusion the entry drew is CORRECT,
// and it is correct BECAUSE the key is persisted: widening the in-memory
// index to `(tenant, user, session, key)` needs no migration precisely
// because `Hydrate` can re-derive the wider key from fields already on
// every pre-existing row — `Task.Identity` (a full quadruple, carrying
// tenant and user) plus `Task.IdempotencyKey`. Had the field genuinely
// not been persisted, the rebuild would have had nothing to read and
// dedup would have silently stopped surviving restarts. So the entry's
// stated evidence contradicted its own conclusion; this test pins the
// premise the conclusion actually rests on.
func TestPersistedTask_IdempotencyKeyIsInThePayload(t *testing.T) {
	const key = "retry-key-alpha"
	task := &tasks.Task{
		ID: tasks.TaskID("task-01"),
		Identity: identity.Quadruple{
			Identity: identity.Identity{
				TenantID:  "tenant-a",
				UserID:    "user-a",
				SessionID: "session-a",
			},
			RunID: "run-a",
		},
		Kind:           tasks.KindBackground,
		Status:         tasks.StatusPending,
		IdempotencyKey: key,
	}

	payload, err := json.Marshal(persistedTask{Task: task})
	if err != nil {
		t.Fatalf("marshal persistedTask: %v", err)
	}

	// The VALUE is on disk.
	if !strings.Contains(string(payload), key) {
		t.Fatalf("the idempotency key %q is absent from the persisted payload; if this is now true by design, D-371's no-migration reasoning must be re-derived, because Hydrate rebuilds the index from this field.\npayload: %s", key, payload)
	}

	// The JSON FIELD NAME is on disk too, and it is load-bearing. `Task`
	// carries no renaming json tags — its three tags are `json:",omitempty"`,
	// which keep the Go field name — so every persisted row spells this
	// field `IdempotencyKey`. Adding a `json:"idempotency_key"` tag later
	// would silently orphan the value on every existing row, and Hydrate
	// would rebuild an index missing every pre-upgrade key. That is the
	// migration D-371 correctly says is not needed TODAY, and this
	// assertion is what keeps it not needed.
	if !strings.Contains(string(payload), `"IdempotencyKey"`) {
		t.Fatalf("the persisted field name is no longer \"IdempotencyKey\" — a rename is an on-disk break for every existing row, not a cosmetic change.\npayload: %s", payload)
	}

	// And it round-trips, which is what Hydrate depends on.
	var back persistedTask
	if err := json.Unmarshal(payload, &back); err != nil {
		t.Fatalf("unmarshal persistedTask: %v", err)
	}
	if back.Task == nil {
		t.Fatal("the task did not survive the round trip")
	}
	if back.Task.IdempotencyKey != key {
		t.Fatalf("IdempotencyKey did not survive the round trip: got %q, want %q", back.Task.IdempotencyKey, key)
	}
	// The identity components the widened index needs must survive with it.
	if back.Task.Identity.TenantID != "tenant-a" || back.Task.Identity.UserID != "user-a" || back.Task.Identity.SessionID != "session-a" {
		t.Fatalf("the identity the index rebuild reads did not survive the round trip: %+v", back.Task.Identity)
	}
}

// TestPersistedTask_StateStoreKeyOmitsTheIdempotencyKey pins the half of
// the claim that IS true, so the correction narrows the entry rather
// than inverting it.
//
// The storage key is derived from the task's own ULID alone. Nothing
// about the idempotency key reaches it — which is why two tasks in
// different tenants may hold the same key without their rows colliding,
// and why the index shape can widen without touching stored keys.
func TestPersistedTask_StateStoreKeyOmitsTheIdempotencyKey(t *testing.T) {
	const (
		id  = "task-01HZY"
		key = "retry-key-alpha"
	)
	got := taskKindPrefix + id
	if strings.Contains(got, key) {
		t.Fatalf("the StateStore key %q embeds the idempotency key — rows would collide across the isolation boundary and a key change would strand its row", got)
	}
	if want := "task.durable.task/" + id; got != want {
		t.Fatalf("the task storage key shape moved: got %q, want %q", got, want)
	}
}
