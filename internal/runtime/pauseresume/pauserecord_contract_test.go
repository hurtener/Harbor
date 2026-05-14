// Phase 51 — pause-state serialise contract: end-to-end exercise
// through the Coordinator surface (RFC §6.3 + §3.4; D-069).
//
// pauserecord_test.go pins the SerializeRecord / DeserializeRecord
// primitive directly (in-package). This file proves the WIRING: that
// Coordinator.Request — the real call site — routes a non-encodable
// pause Payload through the fail-loudly contract and rejects the
// pause LOUD without a half-persisted checkpoint. These are black-box
// (package pauseresume_test) — they exercise only the exported
// surface, exactly as the runtime executor (Phase 53) will.
package pauseresume_test

import (
	"errors"
	"testing"

	"github.com/hurtener/Harbor/internal/planner/trajectory"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
)

// testID / runCtx / newStore are shared test fixtures defined in
// pauseresume_test.go (same package).

// TestRequest_FailsLoudlyOnUnserializablePayload is the §11-mandatory
// pause/resume serialisation negative test for Phase 51's surface: a
// PauseRequest whose Payload carries a non-JSON-encodable leaf, with a
// checkpoint store configured, makes Request fail LOUD with
// trajectory.ErrUnserializable — never a silent drop of the offending
// field, never a half-persisted checkpoint.
//
// Phase 50's equivalent test (TestRequest_FailsLoudlyOnUnserializable
// Trajectory) covered the *trajectory*; this covers the pause record's
// OWN envelope (the Payload field), which Phase 50 reached via a bare
// json.Marshal that produced no actionable field path. Phase 51 closes
// that gap.
func TestRequest_FailsLoudlyOnUnserializablePayload(t *testing.T) {
	t.Parallel()
	store := newStore(t)
	ctx := runCtx(t, testID, "run-1")

	c := pauseresume.New(pauseresume.WithCheckpointStore(store))
	p, err := c.Request(ctx, pauseresume.PauseRequest{
		Identity: testID,
		Reason:   pauseresume.ReasonApprovalRequired,
		Payload: map[string]any{
			"approval_context": "provision a database",
			"callback":         func() {}, // non-encodable leaf
		},
	})
	if p.Token != "" {
		t.Fatalf("Request returned a Token (%q) on a non-encodable Payload — half-persisted pause, silent-corruption regression", p.Token)
	}
	var unserr trajectory.ErrUnserializable
	if !errors.As(err, &unserr) {
		t.Fatalf("Request err = %v, want trajectory.ErrUnserializable (the fail-loudly contract)", err)
	}
	if unserr.Field == "" {
		t.Error("ErrUnserializable.Field is empty — the contract requires the offending field path be named")
	}

	// And nothing was persisted: Request rejected the pause before
	// minting a Token or touching the store, so a known-good Token from
	// a follow-up Request is the only thing the store ever sees.
	good, gerr := c.Request(ctx, pauseresume.PauseRequest{
		Identity: testID,
		Reason:   pauseresume.ReasonApprovalRequired,
		Payload:  map[string]any{"approval_context": "encodable this time"},
	})
	if gerr != nil {
		t.Fatalf("follow-up Request with an encodable Payload: %v", gerr)
	}
	c2 := pauseresume.New(pauseresume.WithCheckpointStore(store))
	if st, serr := c2.Status(ctx, good.Token); serr != nil || st.State != pauseresume.StatusPaused {
		t.Fatalf("follow-up pause did not checkpoint cleanly: st=%+v err=%v", st, serr)
	}
}

// TestRequest_NoStore_FailsLoudlyOnUnserializablePayload — even with NO
// checkpoint store configured, a non-encodable Payload still fails the
// Request loud. The serialise contract is enforced unconditionally:
// the Payload is the pause record's wire shape whether or not it is
// persisted, and a process-local-only pause must not silently carry a
// field that could never round-trip (RFC §3.4 — no silent degradation).
func TestRequest_NoStore_FailsLoudlyOnUnserializablePayload(t *testing.T) {
	t.Parallel()
	ctx := runCtx(t, testID, "run-1")

	c := pauseresume.New() // no checkpoint store
	p, err := c.Request(ctx, pauseresume.PauseRequest{
		Identity: testID,
		Reason:   pauseresume.ReasonAwaitInput,
		Payload:  map[string]any{"events": make(chan int)},
	})
	if p.Token != "" {
		t.Fatalf("Request returned a Token (%q) on a non-encodable Payload (no store) — silent-degradation regression", p.Token)
	}
	var unserr trajectory.ErrUnserializable
	if !errors.As(err, &unserr) {
		t.Fatalf("Request err = %v, want trajectory.ErrUnserializable even without a checkpoint store", err)
	}
}

// TestRequest_EncodablePayload_RoundTripsThroughCheckpoint — the happy
// path: a fully-encodable Payload survives Request → checkpoint →
// restart → Status with the format_version: 1 envelope intact.
func TestRequest_EncodablePayload_RoundTripsThroughCheckpoint(t *testing.T) {
	t.Parallel()
	store := newStore(t)
	ctx := runCtx(t, testID, "run-1")

	c1 := pauseresume.New(pauseresume.WithCheckpointStore(store))
	p, err := c1.Request(ctx, pauseresume.PauseRequest{
		Identity: testID,
		Reason:   pauseresume.ReasonExternalEvent,
		Payload: map[string]any{
			"auth_url": "https://example.test/oauth",
			"scopes":   []any{"read", "write"},
		},
	})
	if err != nil {
		t.Fatalf("Request with an encodable Payload: %v", err)
	}
	if p.Token == "" {
		t.Fatal("Request returned an empty Token on the happy path")
	}

	// Restarted Coordinator rehydrates via DeserializeRecord (the
	// format_version guard runs on this path).
	c2 := pauseresume.New(pauseresume.WithCheckpointStore(store))
	st, err := c2.Status(ctx, p.Token)
	if err != nil {
		t.Fatalf("Status on restarted coordinator: %v", err)
	}
	if st.State != pauseresume.StatusPaused {
		t.Fatalf("Status.State = %q, want paused (pause did not survive restart)", st.State)
	}
	if st.Reason != pauseresume.ReasonExternalEvent {
		t.Fatalf("Status.Reason = %q, want %q", st.Reason, pauseresume.ReasonExternalEvent)
	}
}

// TestFormatVersionConstant_IsOne pins the wire-format version at 1 —
// RFC §6.3 settles "JSON with format_version: 1". A bump is an RFC
// change; this test is the tripwire.
func TestFormatVersionConstant_IsOne(t *testing.T) {
	t.Parallel()
	if pauseresume.FormatVersion != 1 {
		t.Fatalf("pauseresume.FormatVersion = %d, want 1 (RFC §6.3 — bumping it is an RFC change)", pauseresume.FormatVersion)
	}
}
