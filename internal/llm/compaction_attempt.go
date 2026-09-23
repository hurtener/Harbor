package llm

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
)

// DefaultCompactionCalls is the chronological maintenance allowance when the
// operator has not configured one. It is not a receipt-identity grammar limit.
const DefaultCompactionCalls = 16

type compactionAttemptKey struct{}

// CompactionAttemptContext starts a distinct maintenance invocation, retaining
// the host-owned planner step but not its retry/downgrade coordinates. A supplied
// grant is authenticated by the ordinary wrapper and deterministically derives
// a child identity. The summarizer enforces the configured call allowance;
// no model output can select this coordinate.
func CompactionAttemptContext(ctx context.Context, ordinal int) (context.Context, error) {
	if ordinal < 1 {
		return ctx, ErrExternalGrantInvalid
	}
	if err := ctx.Err(); err != nil {
		return ctx, err
	}
	ctx = context.WithValue(ctx, compactionAttemptKey{}, ordinal)
	ctx, _, err := EnsureAttemptScope(WithAttemptScope(ctx, nil))
	return ctx, err
}

func compactionAttemptIdentity(parentID, parentNonce string, ordinal int) (string, string) {
	digest := sha256.Sum256([]byte(fmt.Sprintf("%s\x00compaction\x00%d", parentNonce, ordinal)))
	return fmt.Sprintf("%s/compaction/%d", parentID, ordinal), hex.EncodeToString(digest[:])
}

// validCompactionAttempt admits only the exact positive-ordinal child grammar. Existing
// receipt fields and canonical bytes are unchanged; recipients must understand
// this derivation before enabling granted compaction. Older validators reject it.
func validCompactionAttempt(callID, nonce, parentID, parentNonce string) bool {
	suffix, ok := strings.CutPrefix(callID, parentID+"/compaction/")
	if !ok {
		return false
	}
	ordinal, err := strconv.Atoi(suffix)
	if err != nil || ordinal < 1 || strconv.Itoa(ordinal) != suffix {
		return false
	}
	wantID, wantNonce := compactionAttemptIdentity(parentID, parentNonce, ordinal)
	return callID == wantID && nonce == wantNonce
}
