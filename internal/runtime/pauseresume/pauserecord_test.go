// Phase 51 — pause-state serialise contract (fail-loud) negative-test
// gate (RFC §6.3 + §3.4; master-plan Phase 51 detail block; D-069).
//
// The master plan is explicit: "Negative tests are the gate. CI fails
// on any silent-drop regression." These tests ARE the acceptance
// criterion. They live in-package (package pauseresume, not
// pauseresume_test) because the pause-record serialise contract
// operates on the unexported checkpointRecord envelope — the negative
// tests must construct a record with a non-encodable Payload leaf
// directly to prove SerializeRecord fails loud rather than dropping it.
package pauseresume

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/planner/trajectory"
)

// recordID is a documented dummy identity triple — no secrets
// (CLAUDE.md §13: fixtures carry documented dummy values).
var recordID = identity.Identity{TenantID: "t1", UserID: "u1", SessionID: "s1"}

// goodRecord builds a fully-encodable pause-record envelope for the
// happy-path / round-trip tests.
func goodRecord() checkpointRecord {
	return checkpointRecord{
		Token:    "tok-good",
		Reason:   ReasonApprovalRequired,
		State:    StatusPaused,
		Identity: recordID,
		RunID:    "run-1",
		Payload: map[string]any{
			"prompt": "approve the provision call?",
			"scopes": []any{"db:write", "db:read"},
			"count":  float64(3),
		},
		PausedAt: time.Date(2026, 5, 14, 12, 0, 0, 0, time.UTC),
	}
}

// --- The fail-loud negative gate ---------------------------------------

// TestSerializeRecord_FailLoud_FunctionInPayload is the load-bearing
// test: a func() in the pause record's Payload surfaces
// trajectory.ErrUnserializable naming the offending field path — never
// a silent drop, never a half-encoded record. This is the exact bug
// brief 02 §4 calls out (the predecessor's silent-drop-on-callback in
// the pause-state serialiser) applied to the pause record's OWN
// envelope.
func TestSerializeRecord_FailLoud_FunctionInPayload(t *testing.T) {
	t.Parallel()
	rec := goodRecord()
	rec.Payload = map[string]any{"callback": func() {}}

	out, err := SerializeRecord(rec)
	if out != nil {
		t.Fatalf("SerializeRecord returned non-nil bytes on non-encodable Payload — fail-loud violated, silent-drop regression")
	}
	var unserr trajectory.ErrUnserializable
	if !errors.As(err, &unserr) {
		t.Fatalf("err = %v, want trajectory.ErrUnserializable", err)
	}
	if !strings.Contains(unserr.Field, "PauseRecord") {
		t.Errorf("Field path %q is not rooted at the PauseRecord envelope vocabulary", unserr.Field)
	}
	if !strings.Contains(unserr.Field, "payload") {
		t.Errorf("Field path %q does not name the payload field", unserr.Field)
	}
	if !strings.Contains(unserr.Field, "callback") {
		t.Errorf("Field path %q does not name the offending 'callback' key", unserr.Field)
	}
}

// TestSerializeRecord_FailLoud_ChannelInPayload — a chan in Payload
// surfaces ErrUnserializable.
func TestSerializeRecord_FailLoud_ChannelInPayload(t *testing.T) {
	t.Parallel()
	rec := goodRecord()
	rec.Payload = map[string]any{"events": make(chan int)}

	_, err := SerializeRecord(rec)
	var unserr trajectory.ErrUnserializable
	if !errors.As(err, &unserr) {
		t.Fatalf("err = %v, want trajectory.ErrUnserializable on chan", err)
	}
	if !strings.Contains(unserr.Field, "events") {
		t.Errorf("Field path %q should name the offending 'events' key", unserr.Field)
	}
}

// TestSerializeRecord_FailLoud_NestedFunction — a func buried inside a
// nested map in Payload still surfaces with the dotted path.
func TestSerializeRecord_FailLoud_NestedFunction(t *testing.T) {
	t.Parallel()
	rec := goodRecord()
	rec.Payload = map[string]any{
		"oauth": map[string]any{
			"refresh": func() string { return "" },
		},
	}

	_, err := SerializeRecord(rec)
	var unserr trajectory.ErrUnserializable
	if !errors.As(err, &unserr) {
		t.Fatalf("err = %v, want trajectory.ErrUnserializable on nested func", err)
	}
	if !strings.Contains(unserr.Field, "oauth") || !strings.Contains(unserr.Field, "refresh") {
		t.Errorf("Field path %q should name the nested oauth.refresh path", unserr.Field)
	}
}

// TestSerializeRecord_FailLoud_ComplexInPayload — a complex number in
// Payload (json cannot encode it) surfaces ErrUnserializable.
func TestSerializeRecord_FailLoud_ComplexInPayload(t *testing.T) {
	t.Parallel()
	rec := goodRecord()
	rec.Payload = map[string]any{"phase": complex(1, 2)}

	_, err := SerializeRecord(rec)
	var unserr trajectory.ErrUnserializable
	if !errors.As(err, &unserr) {
		t.Fatalf("err = %v, want trajectory.ErrUnserializable on complex", err)
	}
}

// TestSerializeRecord_NeverHalfEncodes — when one Payload leaf is
// non-encodable, SerializeRecord returns (nil, err): there is no
// partially-serialised byte slice. A half-encoded record persisted to
// the StateStore is exactly the silent-corruption shape the contract
// closes.
func TestSerializeRecord_NeverHalfEncodes(t *testing.T) {
	t.Parallel()
	rec := goodRecord()
	rec.Payload = map[string]any{
		"good_field": "this would encode fine",
		"bad_field":  make(chan struct{}),
	}

	out, err := SerializeRecord(rec)
	if out != nil {
		t.Fatalf("SerializeRecord returned %d bytes alongside an error — half-encoded record, silent-corruption regression", len(out))
	}
	if err == nil {
		t.Fatal("SerializeRecord returned (nil, nil) on a non-encodable Payload — the exact silent-drop bug the contract closes")
	}
}

// --- format_version guard (the load-side half of the contract) ---------

// TestSerializeRecord_StampsFormatVersion — SerializeRecord owns the
// format_version field: it stamps the current FormatVersion regardless
// of what the caller set, so "what version did we write" is
// single-sourced.
func TestSerializeRecord_StampsFormatVersion(t *testing.T) {
	t.Parallel()
	rec := goodRecord()
	rec.FormatVersion = 0 // caller left it unset

	out, err := SerializeRecord(rec)
	if err != nil {
		t.Fatalf("SerializeRecord: %v", err)
	}
	var probe struct {
		FormatVersion int `json:"format_version"`
	}
	if err := json.Unmarshal(out, &probe); err != nil {
		t.Fatalf("unmarshal probe: %v", err)
	}
	if probe.FormatVersion != FormatVersion {
		t.Fatalf("serialised format_version = %d, want %d (SerializeRecord must stamp it)", probe.FormatVersion, FormatVersion)
	}
}

// TestDeserializeRecord_RejectsUnknownFormatVersion — a record carrying
// a higher format_version (a forward-incompatible write from a newer
// Runtime) is rejected loud with ErrUnsupportedFormatVersion, not
// silently mis-decoded against the current schema.
func TestDeserializeRecord_RejectsUnknownFormatVersion(t *testing.T) {
	t.Parallel()
	// Build a record, serialise it, then tamper the version upward.
	rec := goodRecord()
	out, err := SerializeRecord(rec)
	if err != nil {
		t.Fatalf("SerializeRecord: %v", err)
	}
	var envelope map[string]any
	if err := json.Unmarshal(out, &envelope); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	envelope["format_version"] = FormatVersion + 99
	tampered, err := json.Marshal(envelope)
	if err != nil {
		t.Fatalf("re-marshal: %v", err)
	}

	_, err = DeserializeRecord(tampered)
	if !errors.Is(err, ErrUnsupportedFormatVersion) {
		t.Fatalf("DeserializeRecord on a future format_version: err=%v, want ErrUnsupportedFormatVersion", err)
	}
}

// TestDeserializeRecord_RejectsZeroFormatVersion — a record with a
// missing / zero format_version (a corrupt or pre-contract write) is
// also rejected loud.
func TestDeserializeRecord_RejectsZeroFormatVersion(t *testing.T) {
	t.Parallel()
	// A record body with no format_version field at all.
	body := []byte(`{"token":"tok-x","reason":"approval_required","state":"paused",` +
		`"identity":{"tenant_id":"t1","user_id":"u1","session_id":"s1"},"paused_at":"2026-05-14T12:00:00Z"}`)

	_, err := DeserializeRecord(body)
	if !errors.Is(err, ErrUnsupportedFormatVersion) {
		t.Fatalf("DeserializeRecord on a zero/absent format_version: err=%v, want ErrUnsupportedFormatVersion", err)
	}
}

// TestDeserializeRecord_RejectsCorruptBytes — malformed JSON surfaces
// ErrCheckpointCorrupt, never a half-decoded record.
func TestDeserializeRecord_RejectsCorruptBytes(t *testing.T) {
	t.Parallel()
	_, err := DeserializeRecord([]byte(`{"format_version": 1, "token": `)) // truncated
	if !errors.Is(err, ErrCheckpointCorrupt) {
		t.Fatalf("DeserializeRecord on truncated JSON: err=%v, want ErrCheckpointCorrupt", err)
	}
}

// TestDeserializeRecord_RejectsEmptyBytes — empty input surfaces
// ErrCheckpointCorrupt, never (zero-record, nil).
func TestDeserializeRecord_RejectsEmptyBytes(t *testing.T) {
	t.Parallel()
	_, err := DeserializeRecord(nil)
	if !errors.Is(err, ErrCheckpointCorrupt) {
		t.Fatalf("DeserializeRecord on empty bytes: err=%v, want ErrCheckpointCorrupt", err)
	}
}

// --- byte-stable round-trip (conformance with phase 43) ----------------

// TestSerializeRecord_RoundTrip_ByteStable proves the pause-record
// envelope round-trips byte-identically:
// SerializeRecord → DeserializeRecord → SerializeRecord. This mirrors
// Phase 43's trajectory_test.go::TestRoundTrip_ByteStable — the
// master plan's "Conformance with phase 43 Trajectory.Serialize"
// requirement. The byte-stable property holds because the pause record
// reuses the same canonical-ordering discipline (declaration-order
// struct fields + alphabetised map keys) D-049 pins for the
// trajectory.
func TestSerializeRecord_RoundTrip_ByteStable(t *testing.T) {
	t.Parallel()
	rec := goodRecord()

	first, err := SerializeRecord(rec)
	if err != nil {
		t.Fatalf("SerializeRecord (first): %v", err)
	}
	decoded, err := DeserializeRecord(first)
	if err != nil {
		t.Fatalf("DeserializeRecord: %v", err)
	}
	second, err := SerializeRecord(decoded)
	if err != nil {
		t.Fatalf("SerializeRecord (second): %v", err)
	}
	if string(first) != string(second) {
		t.Fatalf("round-trip not byte-stable:\n first  = %s\n second = %s", first, second)
	}
}

// TestSerializeRecord_RoundTrip_WithTrajectoryBytes proves the
// envelope round-trips byte-stably even when it carries an embedded,
// already-canonical trajectory blob (the json.RawMessage TrajectoryBytes
// field) — the blob is preserved verbatim, not re-marshalled.
func TestSerializeRecord_RoundTrip_WithTrajectoryBytes(t *testing.T) {
	t.Parallel()
	tr := &trajectory.Trajectory{
		Query:      "checkpointed run",
		LLMContext: map[string]any{"note": "prior-turn-summary"},
		ToolContext: trajectory.ToolContext{
			Serializable: map[string]any{"region": "us-east-1"},
		},
	}
	trBytes, err := tr.Serialize()
	if err != nil {
		t.Fatalf("trajectory.Serialize: %v", err)
	}

	rec := goodRecord()
	rec.TrajectoryBytes = json.RawMessage(trBytes)

	first, err := SerializeRecord(rec)
	if err != nil {
		t.Fatalf("SerializeRecord (first): %v", err)
	}
	decoded, err := DeserializeRecord(first)
	if err != nil {
		t.Fatalf("DeserializeRecord: %v", err)
	}
	if string(decoded.TrajectoryBytes) != string(trBytes) {
		t.Fatalf("embedded trajectory bytes not preserved verbatim:\n got  = %s\n want = %s",
			decoded.TrajectoryBytes, trBytes)
	}
	second, err := SerializeRecord(decoded)
	if err != nil {
		t.Fatalf("SerializeRecord (second): %v", err)
	}
	if string(first) != string(second) {
		t.Fatalf("round-trip with embedded trajectory not byte-stable:\n first  = %s\n second = %s", first, second)
	}
}

// TestSerializeRecord_HappyPath_EmptyPayload — a record with a nil
// Payload encodes cleanly (the omitempty tag drops it from the wire).
func TestSerializeRecord_HappyPath_EmptyPayload(t *testing.T) {
	t.Parallel()
	rec := goodRecord()
	rec.Payload = nil

	out, err := SerializeRecord(rec)
	if err != nil {
		t.Fatalf("SerializeRecord on nil Payload: %v", err)
	}
	if strings.Contains(string(out), "payload") {
		t.Errorf("nil Payload should be omitted from the wire, got: %s", out)
	}
}

// TestSerializeRecord_SharesTrajectoryWalker is the §13 anti-parallel-
// implementation guard expressed as a test: a non-encodable leaf in the
// pause record's Payload produces a trajectory.ErrUnserializable —
// the SAME struct sentinel Phase 43's Trajectory.Serialize produces.
// If Phase 51 had forked a second fail-loudly serialiser with its own
// error type, this errors.As would fail. The shared error type is the
// observable proof the walker is shared, not copied (D-069).
func TestSerializeRecord_SharesTrajectoryWalker(t *testing.T) {
	t.Parallel()
	rec := goodRecord()
	rec.Payload = map[string]any{"sock": make(chan int)}
	_, recErr := SerializeRecord(rec)

	tr := &trajectory.Trajectory{HintState: map[string]any{"sock": make(chan int)}}
	_, trErr := tr.Serialize()

	var recUnser, trUnser trajectory.ErrUnserializable
	if !errors.As(recErr, &recUnser) {
		t.Fatalf("SerializeRecord err = %v, want trajectory.ErrUnserializable", recErr)
	}
	if !errors.As(trErr, &trUnser) {
		t.Fatalf("Trajectory.Serialize err = %v, want trajectory.ErrUnserializable", trErr)
	}
	// Both produce the same error TYPE — the contract is one shape,
	// shared, not two parallel implementations.
}
