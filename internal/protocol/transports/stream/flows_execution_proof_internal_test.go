package stream

import (
	"net/http"
	"testing"

	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	flowprotocol "github.com/hurtener/Harbor/internal/runtime/flow/protocol"
)

// D-513: flows.run refused because runs require a signed execution proof
// answers 403 scope_mismatch, the same refusal start gets.
func TestClassifyFlowsError_ExecutionProofRequired(t *testing.T) {
	code, status, msg := classifyFlowsError(flowprotocol.ErrRunExecutionProofRequired)
	if code != protoerrors.CodeScopeMismatch || status != http.StatusForbidden || msg == "" {
		t.Fatalf("classifyFlowsError = %q %d %q, want scope_mismatch 403", code, status, msg)
	}
}
