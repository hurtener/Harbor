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
		protoerrors.CodeInvalidRequest:        http.StatusBadRequest,
		protoerrors.CodeIdentityRequired:      http.StatusUnauthorized,
		protoerrors.CodeScopeMismatch:         http.StatusForbidden,
		protoerrors.CodePayloadInvalid:        http.StatusUnprocessableEntity,
		protoerrors.CodeUnknownMethod:         http.StatusNotFound,
		protoerrors.CodeNotFound:              http.StatusNotFound,
		protoerrors.CodeRuntimeError:          http.StatusInternalServerError,
		protoerrors.CodeAuthRejected:          http.StatusUnauthorized,
		protoerrors.CodeIdentityScopeRequired: http.StatusForbidden,
		protoerrors.CodePresignUnsupported:    http.StatusNotImplemented,
		protoerrors.CodeRequestTooLarge:       http.StatusRequestEntityTooLarge,
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
// not a silent 500. Derives the canonical set from protoerrors.Codes()
// (D-082 amendment) so a new code without a mapping surfaces by NAME.
func TestHTTPStatus_Mapping_ExhaustiveOverCanonicalCodes(t *testing.T) {
	mapped := map[protoerrors.Code]struct{}{
		protoerrors.CodeInvalidRequest:        {},
		protoerrors.CodeIdentityRequired:      {},
		protoerrors.CodeScopeMismatch:         {},
		protoerrors.CodePayloadInvalid:        {},
		protoerrors.CodeUnknownMethod:         {},
		protoerrors.CodeNotFound:              {},
		protoerrors.CodeRuntimeError:          {},
		protoerrors.CodeAuthRejected:          {},
		protoerrors.CodeIdentityScopeRequired: {},
		protoerrors.CodePresignUnsupported:    {},
		protoerrors.CodeRequestTooLarge:       {},
	}
	for code := range mapped {
		if !protoerrors.IsValidCode(code) {
			t.Errorf("code %q is in the status table but not canonical", code)
		}
	}
	for _, code := range protoerrors.Codes() {
		if _, ok := mapped[code]; !ok {
			t.Errorf("canonical code %q has no entry in the status-mapping table — add one to status.go and to this table", code)
		}
	}
}

// TestStatusFor_CodeIdentityScopeRequired_Returns403 — pins the
// Phase 72 / D-105 wire mapping: the new canonical code maps to HTTP
// 403 (the request is authenticated; the scope set does not authorize
// the operation). 401 would imply the request is unauthenticated,
// which would be wrong — the JWT verified, only the scope set was
// insufficient.
func TestStatusFor_CodeIdentityScopeRequired_Returns403(t *testing.T) {
	if got := httpStatus(protoerrors.CodeIdentityScopeRequired); got != http.StatusForbidden {
		t.Errorf("httpStatus(CodeIdentityScopeRequired) = %d, want 403 (authenticated but not authorized)", got)
	}
}

// TestHTTPStatus_UnmappedCode_FailsLoudAs500 — an unmapped / unknown
// Code falls through to 500 rather than masking as a misleading 2xx.
func TestHTTPStatus_UnmappedCode_FailsLoudAs500(t *testing.T) {
	if got := httpStatus(protoerrors.Code("not_a_real_code")); got != http.StatusInternalServerError {
		t.Errorf("httpStatus(unmapped) = %d, want 500", got)
	}
}
