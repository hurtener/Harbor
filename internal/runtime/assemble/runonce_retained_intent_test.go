package assemble_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/hurtener/Harbor/internal/audit"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/runtime/assemble"
	"github.com/hurtener/Harbor/internal/runtime/runctx"
)

type ambiguousIntentRedactor struct{ inner audit.Redactor }

func (r ambiguousIntentRedactor) Redact(ctx context.Context, value any) (any, error) {
	redacted, err := r.inner.Redact(ctx, value)
	if err != nil {
		return nil, err
	}
	object, ok := redacted.(map[string]any)
	if !ok || object["action"] == nil {
		return redacted, nil
	}
	body, err := json.Marshal(object)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(string(body[:len(body)-1]) + `,"failure":{"code":"one","code":"two","message":"invalid host metadata","attempts":1}}`), nil
}

// A malformed redactor response must stop the real run loop before the tool
// runs, rather than discovering the invalid durable intent after its side effect.
func TestRunOnce_RetainedJournalRejectsAmbiguousIntentBeforeTool(t *testing.T) {
	stack, _, calls := retainedRecordingStack(t)
	original := stack.Redactor
	stack.Redactor = ambiguousIntentRedactor{inner: original}
	defer func() { stack.Redactor = original }()
	id := identity.Identity{TenantID: "tenant", UserID: "user", SessionID: "intent-validation"}
	_, err := stack.RunOnce(t.Context(), "read", id, assemble.WithRunID("first"), assemble.WithRetainedContext(2))
	if !errors.Is(err, runctx.ErrRetainedContextUnavailable) {
		t.Fatalf("malformed intent was not rejected: %v", err)
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("tool ran %d time(s) before malformed intent was rejected", got)
	}
}
