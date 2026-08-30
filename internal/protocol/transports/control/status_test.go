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
		protoerrors.CodeRestartUnavailable:    http.StatusConflict,
		protoerrors.CodeRuntimeError:          http.StatusInternalServerError,
		protoerrors.CodeAuthRejected:          http.StatusUnauthorized,
		protoerrors.CodeIdentityScopeRequired: http.StatusForbidden,
		protoerrors.CodePresignUnsupported:    http.StatusNotImplemented,
		protoerrors.CodeRequestTooLarge:       http.StatusRequestEntityTooLarge,
		protoerrors.CodeSessionRunning:        http.StatusConflict,
		// CodeSessionErased was registered in the exhaustiveness set below but
		// never VALUE-asserted here, so its arm's returned status was
		// unpinned — the exhaustiveness check only proves a row exists, not
		// that it returns the right number. Added alongside
		// CodeRevisionConflict, which would have shipped with the same gap.
		protoerrors.CodeSessionErased:                    http.StatusConflict,
		protoerrors.CodeRevisionConflict:                 http.StatusConflict,
		protoerrors.CodeAgentPackCopyConflict:            http.StatusConflict,
		protoerrors.CodeAgentPackCopyIdempotencyConflict: http.StatusConflict,
		protoerrors.CodeSessionSkillCutoverPending:       http.StatusConflict,
		protoerrors.CodeSessionSkillReadUnstable:         http.StatusConflict,
		protoerrors.CodeAgentRetired:                     http.StatusConflict,
		protoerrors.CodeAgentRetirementConflict:          http.StatusConflict,
		protoerrors.CodeRenderAdmissionMissing:           http.StatusBadRequest,
		protoerrors.CodeRenderAdmissionUnavailable:       http.StatusBadRequest,
		protoerrors.CodeRenderAdmissionInvalid:           http.StatusBadRequest,
		protoerrors.CodeRenderAdmissionExpired:           http.StatusBadRequest,
		protoerrors.CodeRenderAdmissionMismatch:          http.StatusBadRequest,
		protoerrors.CodeRenderAuthorityAmbiguous:         http.StatusBadRequest,
		protoerrors.CodeSkillImportProposalInvalid:       http.StatusBadRequest,
		protoerrors.CodeSkillImportProposalExpired:       http.StatusBadRequest,
		protoerrors.CodeSkillImportPackageInvalid:        http.StatusBadRequest,
		protoerrors.CodeSkillImportReplaceRequired:       http.StatusConflict,
		protoerrors.CodeQueryBudgetExceeded:              http.StatusBadRequest,
		protoerrors.CodeInvalidCursor:                    http.StatusBadRequest,

		protoerrors.CodeSkillPublicationConflict:            http.StatusConflict,
		protoerrors.CodeSkillPublicationNotFound:            http.StatusNotFound,
		protoerrors.CodeSkillPublicationRetired:             http.StatusConflict,
		protoerrors.CodeSkillPublicationRuntimeMismatch:     http.StatusConflict,
		protoerrors.CodeSkillPublicationIdempotencyConflict: http.StatusConflict,
	}
	for code, want := range cases {
		if got := HTTPStatus(code); got != want {
			t.Errorf("HTTPStatus(%q) = %d, want %d", code, got, want)
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
		protoerrors.CodeInvalidRequest:                   {},
		protoerrors.CodeIdentityRequired:                 {},
		protoerrors.CodeScopeMismatch:                    {},
		protoerrors.CodePayloadInvalid:                   {},
		protoerrors.CodeUnknownMethod:                    {},
		protoerrors.CodeNotFound:                         {},
		protoerrors.CodeRestartUnavailable:               {},
		protoerrors.CodeRuntimeError:                     {},
		protoerrors.CodeAuthRejected:                     {},
		protoerrors.CodeIdentityScopeRequired:            {},
		protoerrors.CodePresignUnsupported:               {},
		protoerrors.CodeRequestTooLarge:                  {},
		protoerrors.CodeSessionRunning:                   {},
		protoerrors.CodeSessionErased:                    {},
		protoerrors.CodeRevisionConflict:                 {},
		protoerrors.CodeAgentPackCopyConflict:            {},
		protoerrors.CodeAgentPackCopyIdempotencyConflict: {},
		protoerrors.CodeSessionSkillCutoverPending:       {},
		protoerrors.CodeSessionSkillReadUnstable:         {},
		protoerrors.CodeAgentRetired:                     {},
		protoerrors.CodeAgentRetirementConflict:          {},
		protoerrors.CodeRenderAdmissionMissing:           {},
		protoerrors.CodeRenderAdmissionUnavailable:       {},
		protoerrors.CodeRenderAdmissionInvalid:           {},
		protoerrors.CodeRenderAdmissionExpired:           {},
		protoerrors.CodeRenderAdmissionMismatch:          {},
		protoerrors.CodeRenderAuthorityAmbiguous:         {},
		protoerrors.CodeSkillImportProposalInvalid:       {},
		protoerrors.CodeSkillImportProposalExpired:       {},
		protoerrors.CodeSkillImportPackageInvalid:        {},
		protoerrors.CodeSkillImportReplaceRequired:       {},
		protoerrors.CodeQueryBudgetExceeded:              {},
		protoerrors.CodeInvalidCursor:                    {},

		protoerrors.CodeSkillPublicationConflict:            {},
		protoerrors.CodeSkillPublicationNotFound:            {},
		protoerrors.CodeSkillPublicationRetired:             {},
		protoerrors.CodeSkillPublicationRuntimeMismatch:     {},
		protoerrors.CodeSkillPublicationIdempotencyConflict: {},
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
	if got := HTTPStatus(protoerrors.CodeIdentityScopeRequired); got != http.StatusForbidden {
		t.Errorf("HTTPStatus(CodeIdentityScopeRequired) = %d, want 403 (authenticated but not authorized)", got)
	}
}

// TestHTTPStatus_UnmappedCode_FailsLoudAs500 — an unmapped / unknown
// Code falls through to 500 rather than masking as a misleading 2xx.
func TestHTTPStatus_UnmappedCode_FailsLoudAs500(t *testing.T) {
	if got := HTTPStatus(protoerrors.Code("not_a_real_code")); got != http.StatusInternalServerError {
		t.Errorf("HTTPStatus(unmapped) = %d, want 500", got)
	}
}
