package errors_test

import (
	"encoding/json"
	stderrors "errors"
	"testing"

	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
)

// The canonical Protocol error codes — the test's independent source
// of truth. The original seven landed in Phase 54; CodeAuthRejected
// landed in Phase 61 (D-079); CodeIdentityScopeRequired landed in
// Phase 72 (D-105). If errors.go drifts, the stability test fails.
var wantCodes = []protoerrors.Code{
	protoerrors.CodeInvalidRequest,
	protoerrors.CodeIdentityRequired,
	protoerrors.CodeScopeMismatch,
	protoerrors.CodePayloadInvalid,
	protoerrors.CodeUnknownMethod,
	protoerrors.CodeNotFound,
	protoerrors.CodeRuntimeError,
	protoerrors.CodeAuthRejected,
	protoerrors.CodeIdentityScopeRequired,
}

func TestErrorCodes_StableWireStrings(t *testing.T) {
	// The Code wire strings are part of the versioned Protocol surface
	// (RFC §5.3) — a Protocol client branches on them. A casual rename
	// is a breaking change; this test pins the strings.
	wire := map[protoerrors.Code]string{
		protoerrors.CodeInvalidRequest:        "invalid_request",
		protoerrors.CodeIdentityRequired:      "identity_required",
		protoerrors.CodeScopeMismatch:         "scope_mismatch",
		protoerrors.CodePayloadInvalid:        "payload_invalid",
		protoerrors.CodeUnknownMethod:         "unknown_method",
		protoerrors.CodeNotFound:              "not_found",
		protoerrors.CodeRuntimeError:          "runtime_error",
		protoerrors.CodeAuthRejected:          "auth_rejected",
		protoerrors.CodeIdentityScopeRequired: "identity_scope_required",
	}
	for code, want := range wire {
		if string(code) != want {
			t.Errorf("code wire string = %q, want %q", string(code), want)
		}
		if !protoerrors.IsValidCode(code) {
			t.Errorf("IsValidCode(%q) = false, want true", code)
		}
	}
	if len(wantCodes) != len(wire) {
		t.Fatalf("wantCodes count %d != wire-string map count %d", len(wantCodes), len(wire))
	}
}

func TestIsValidCode_RejectsUnknown(t *testing.T) {
	for _, bad := range []protoerrors.Code{
		"", "INVALID_REQUEST", "invalidRequest", "bad_request", "500",
	} {
		if protoerrors.IsValidCode(bad) {
			t.Errorf("IsValidCode(%q) = true, want false", bad)
		}
	}
}

func TestError_ImplementsErrorInterface(t *testing.T) {
	var err error = protoerrors.New(protoerrors.CodeScopeMismatch, "caller scope insufficient")
	if err.Error() == "" {
		t.Fatal("Error() returned empty string")
	}
	// The message must carry the code so a log line is self-describing.
	got := err.Error()
	if want := "protocol: scope_mismatch: caller scope insufficient"; got != want {
		t.Fatalf("Error() = %q, want %q", got, want)
	}
}

func TestError_ErrorsAsReachesCode(t *testing.T) {
	// A handler returns a *Error; a caller reaches the stable Code via
	// errors.As. This is the in-process contract until the Phase 60
	// transport adapter maps Code onto an HTTP status.
	err := error(protoerrors.Newf(protoerrors.CodeNotFound, "no live run for %q", "run-x"))
	var pe *protoerrors.Error
	if !stderrors.As(err, &pe) {
		t.Fatal("errors.As did not reach the *protoerrors.Error")
	}
	if pe.Code != protoerrors.CodeNotFound {
		t.Fatalf("Code = %q, want %q", pe.Code, protoerrors.CodeNotFound)
	}
}

func TestError_JSONRoundTrip(t *testing.T) {
	in := protoerrors.Error{Code: protoerrors.CodePayloadInvalid, Message: "control payload failed validation"}
	b, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var out protoerrors.Error
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if out != in {
		t.Fatalf("round-trip mismatch:\n got %+v\nwant %+v", out, in)
	}
}

// TestCodes_IdentityScopeRequired — pins the Phase 72 / D-105 code:
// IsValidCode reads back true, the wire string is exactly
// "identity_scope_required" (third-party Consoles branch on it), and
// Codes() returns it in the lexicographically-sorted snapshot.
func TestCodes_IdentityScopeRequired(t *testing.T) {
	if string(protoerrors.CodeIdentityScopeRequired) != "identity_scope_required" {
		t.Fatalf("CodeIdentityScopeRequired wire string = %q, want %q",
			string(protoerrors.CodeIdentityScopeRequired), "identity_scope_required")
	}
	if !protoerrors.IsValidCode(protoerrors.CodeIdentityScopeRequired) {
		t.Error("IsValidCode(identity_scope_required) = false, want true")
	}
	// String-form stability: a third-party Console computes the
	// canonical name as a literal and expects parity.
	if !protoerrors.IsValidCode(protoerrors.Code("identity_scope_required")) {
		t.Error(`IsValidCode(Code("identity_scope_required")) = false, want true — wire-string stability broken`)
	}
	// Lexicographic-order pin: the new code lands between
	// "auth_rejected" and "invalid_request" in the canonical set.
	got := protoerrors.Codes()
	idx := -1
	for i, c := range got {
		if c == protoerrors.CodeIdentityScopeRequired {
			idx = i
			break
		}
	}
	if idx < 0 {
		t.Fatal("Codes() snapshot missing identity_scope_required")
	}
	if idx > 0 && string(got[idx-1]) > string(got[idx]) {
		t.Errorf("Codes() not lex-sorted around identity_scope_required: %q before %q", got[idx-1], got[idx])
	}
}

// TestCodes_ReturnsCanonicalSetSorted — Codes() returns the canonical
// set in deterministic lexicographic order, matching wantCodes
// (modulo ordering). Added PR #91 / D-082 (Wave 10 audit WARN-4) so
// the conformance suite's exhaustiveness check can derive from
// Codes() rather than a hardcoded count.
func TestCodes_ReturnsCanonicalSetSorted(t *testing.T) {
	got := protoerrors.Codes()
	if len(got) != len(wantCodes) {
		t.Fatalf("Codes() length = %d, want %d (the canonical set size)", len(got), len(wantCodes))
	}
	// Every canonical code from wantCodes must appear in Codes().
	gotSet := make(map[protoerrors.Code]struct{}, len(got))
	for _, c := range got {
		gotSet[c] = struct{}{}
	}
	for _, want := range wantCodes {
		if _, ok := gotSet[want]; !ok {
			t.Errorf("Codes() missing canonical code %q", want)
		}
	}
	// Lexicographic order.
	for i := 1; i < len(got); i++ {
		if string(got[i-1]) > string(got[i]) {
			t.Errorf("Codes() not lex-sorted: %q before %q", got[i-1], got[i])
		}
	}
}
