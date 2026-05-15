package control

import (
	"net/http"
	"testing"

	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
)

// TestHTTPStatus_Mapping_EveryCanonicalCode pins the Code -> HTTP status
// table. Every canonical Protocol error code MUST map to an explicit,
// stable status — the mapping is part of the wire contract.
func TestHTTPStatus_Mapping_EveryCanonicalCode(t *testing.T) {
	cases := map[protoerrors.Code]int{
		protoerrors.CodeInvalidRequest:   http.StatusBadRequest,
		protoerrors.CodeIdentityRequired: http.StatusUnauthorized,
		protoerrors.CodeScopeMismatch:    http.StatusForbidden,
		protoerrors.CodePayloadInvalid:   http.StatusUnprocessableEntity,
		protoerrors.CodeUnknownMethod:    http.StatusNotFound,
		protoerrors.CodeNotFound:         http.StatusNotFound,
		protoerrors.CodeRuntimeError:     http.StatusInternalServerError,
	}
	for code, want := range cases {
		if got := httpStatus(code); got != want {
			t.Errorf("httpStatus(%q) = %d, want %d", code, got, want)
		}
	}
}

// TestHTTPStatus_Mapping_ExhaustiveOverCanonicalCodes asserts the table
// above covers every code internal/protocol/errors declares — a new
// canonical code without a status entry must surface as a test failure,
// not a silent 500.
func TestHTTPStatus_Mapping_ExhaustiveOverCanonicalCodes(t *testing.T) {
	known := map[protoerrors.Code]struct{}{
		protoerrors.CodeInvalidRequest:   {},
		protoerrors.CodeIdentityRequired: {},
		protoerrors.CodeScopeMismatch:    {},
		protoerrors.CodePayloadInvalid:   {},
		protoerrors.CodeUnknownMethod:    {},
		protoerrors.CodeNotFound:         {},
		protoerrors.CodeRuntimeError:     {},
	}
	for code := range known {
		if !protoerrors.IsValidCode(code) {
			t.Errorf("code %q is in the status table but not canonical", code)
		}
	}
}

// TestHTTPStatus_UnmappedCode_FailsLoudAs500 — an unmapped / unknown
// Code falls through to 500 rather than masking as a misleading 2xx.
func TestHTTPStatus_UnmappedCode_FailsLoudAs500(t *testing.T) {
	if got := httpStatus(protoerrors.Code("not_a_real_code")); got != http.StatusInternalServerError {
		t.Errorf("httpStatus(unmapped) = %d, want 500", got)
	}
}
