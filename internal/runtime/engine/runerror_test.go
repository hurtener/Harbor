package engine

import (
	"errors"
	"testing"
)

func TestRunError_ToPayload_CarriesIdentityAndCode(t *testing.T) {
	t.Parallel()
	cause := errors.New("synthetic")
	re := &RunError{
		RunID:     "R-1",
		TenantID:  "T",
		UserID:    "U",
		SessionID: "S",
		NodeName:  "n",
		NodeID:    "n",
		Code:      CodeNodeException,
		Message:   "boom",
		Cause:     cause,
		Metadata:  map[string]any{"attempts": 3},
	}
	got := re.ToPayload()
	for _, key := range []string{"tenant_id", "user_id", "session_id", "run_id", "node", "code", "error", "cause"} {
		if _, ok := got[key]; !ok {
			t.Errorf("payload missing %q: %+v", key, got)
		}
	}
	if got["code"] != string(CodeNodeException) {
		t.Errorf("code=%v, want %v", got["code"], CodeNodeException)
	}
	if got["attempts"] != 3 {
		t.Errorf("metadata not flattened: %+v", got)
	}
}

func TestRunError_ToPayload_MetadataDoesNotOverwriteCanonical(t *testing.T) {
	t.Parallel()
	re := &RunError{
		RunID:    "R-1",
		NodeName: "n",
		Code:     CodeNodeException,
		Metadata: map[string]any{"node": "ATTACKER", "tenant_id": "X"},
	}
	got := re.ToPayload()
	if got["node"] != "n" {
		t.Errorf("metadata clobbered canonical node: %v", got["node"])
	}
	if got["meta_node"] != "ATTACKER" {
		t.Errorf("clobbered metadata not preserved under meta_ prefix: %+v", got)
	}
}

func TestRunError_Unwrap_OneLevel(t *testing.T) {
	t.Parallel()
	innermost := errors.New("inner")
	mid := errors.New("mid")
	re := &RunError{Cause: mid}
	if !errors.Is(re, mid) {
		t.Error("errors.Is failed for direct Cause")
	}
	// Deeper wrapping is the caller's responsibility — we don't
	// re-wrap. Plain Cause carries one level only.
	if errors.Is(re, innermost) {
		t.Error("RunError unexpectedly walked deeper than one level")
	}
}

func TestRunError_Error_NilSafe(t *testing.T) {
	t.Parallel()
	var re *RunError
	if re.Error() != "" {
		t.Error("nil RunError.Error must return empty string")
	}
	if re.Unwrap() != nil {
		t.Error("nil RunError.Unwrap must return nil")
	}
	if re.ToPayload() != nil {
		t.Error("nil RunError.ToPayload must return nil")
	}
}

func TestRunError_ErrorString_IncludesCode(t *testing.T) {
	t.Parallel()
	re := &RunError{
		Code:     CodeNodeTimeout,
		NodeName: "fetcher",
		RunID:    "R-7",
		Message:  "exceeded 200ms",
	}
	got := re.Error()
	if !contains(got, "node_timeout") || !contains(got, "fetcher") || !contains(got, "R-7") {
		t.Errorf("Error string missing fields: %q", got)
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
