package errors_test

import (
	"encoding/json"
	stderrors "errors"
	"testing"

	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
)

// The canonical Phase 54 error codes — the test's independent source of
// truth. If errors.go drifts, the stability test fails.
var wantCodes = []protoerrors.Code{
	protoerrors.CodeInvalidRequest,
	protoerrors.CodeIdentityRequired,
	protoerrors.CodeScopeMismatch,
	protoerrors.CodePayloadInvalid,
	protoerrors.CodeUnknownMethod,
	protoerrors.CodeNotFound,
	protoerrors.CodeRuntimeError,
}

func TestErrorCodes_StableWireStrings(t *testing.T) {
	// The Code wire strings are part of the versioned Protocol surface
	// (RFC §5.3) — a Protocol client branches on them. A casual rename
	// is a breaking change; this test pins the strings.
	wire := map[protoerrors.Code]string{
		protoerrors.CodeInvalidRequest:   "invalid_request",
		protoerrors.CodeIdentityRequired: "identity_required",
		protoerrors.CodeScopeMismatch:    "scope_mismatch",
		protoerrors.CodePayloadInvalid:   "payload_invalid",
		protoerrors.CodeUnknownMethod:    "unknown_method",
		protoerrors.CodeNotFound:         "not_found",
		protoerrors.CodeRuntimeError:     "runtime_error",
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
