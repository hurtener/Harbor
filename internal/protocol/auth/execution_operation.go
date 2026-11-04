package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"unicode"
)

// ExecutionOperationClaim is an optional signed JWT claim that binds one Start
// to a previously admitted external operation. It never comes from the body.
const ExecutionOperationClaim = "execution_operation_id"

// ExecutionIdempotencyClaim binds the operation to its Start retry key.
const ExecutionIdempotencyClaim = "execution_idempotency_key"

// ExecutionStartDigestClaim binds exact Start wire bytes.
const ExecutionStartDigestClaim = "execution_start_sha256"

// ErrExecutionOperationMalformed rejects a present but unbounded claim.
var ErrExecutionOperationMalformed = errors.New("auth: execution operation claim malformed")

type executionOperationKey struct{}

// ExecutionStartProof is verified signed Start correlation. The HTTP middleware
// checks BodySHA256 against raw Start bytes before seating it on context.
type ExecutionStartProof struct {
	OperationID    string
	IdempotencyKey string
	BodySHA256     string
}

// WithExecutionOperation seats a verified claim on a request context.
func WithExecutionOperation(ctx context.Context, proof ExecutionStartProof) context.Context {
	if !validExecutionProof(proof) {
		return ctx
	}
	return context.WithValue(ctx, executionOperationKey{}, proof)
}

// ExecutionOperationFrom returns a verified request claim, if present.
func ExecutionOperationFrom(ctx context.Context) (ExecutionStartProof, bool) {
	proof, ok := ctx.Value(executionOperationKey{}).(ExecutionStartProof)
	return proof, ok && validExecutionProof(proof)
}

// MatchesExecutionStartBody compares raw wire bytes to the signed digest.
func MatchesExecutionStartBody(proof ExecutionStartProof, raw []byte) bool {
	if !validExecutionProof(proof) || len(raw) == 0 || len(raw) > 512<<10 {
		return false
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]) == proof.BodySHA256
}

func validExecutionProof(p ExecutionStartProof) bool {
	if !validExecutionOperation(p.OperationID) || !validExecutionOperation(p.IdempotencyKey) || len(p.BodySHA256) != 64 {
		return false
	}
	for _, c := range p.BodySHA256 {
		if c < '0' || c > '9' {
			if c < 'a' || c > 'f' {
				return false
			}
		}
	}
	return true
}

func validExecutionOperation(id string) bool {
	return len(id) > 0 && len(id) <= 128 && !strings.ContainsFunc(id, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r) || r == '*' || r > unicode.MaxASCII
	})
}
