package pauseresume

import (
	"context"
	"fmt"
	"strings"

	"github.com/hurtener/Harbor/internal/identity"
)

// Continuation is the durable, non-secret identity of work that must complete
// before an accepted Resume can make a pause terminal. Kind selects one
// construction-time registered handler; Data contains only stable identifiers,
// never credentials or transient handles.
type Continuation struct {
	Kind string            `json:"kind"`
	Data map[string]string `json:"data,omitempty"`
}

// ContinuationInvocation is the immutable per-resume input delivered to a
// registered continuation handler outside the Coordinator's mutex.
type ContinuationInvocation struct {
	Token         Token
	Identity      identity.Identity
	RunID         string
	Continuation  Continuation
	Decision      Decision
	ResumePayload map[string]any
}

// ContinuationHandler completes one durable continuation. Returning an error
// leaves the pause paused and its checkpoint intact so the same token can be
// retried after cancellation or a transient dependency failure.
type ContinuationHandler func(context.Context, ContinuationInvocation) error

// ContinuationRegistrar is the construction-time registration surface exposed
// by the canonical Coordinator. Registrations are internally synchronized and
// one handler may own each kind.
type ContinuationRegistrar interface {
	RegisterContinuation(kind string, handler ContinuationHandler) error
}

type continuationContextKey struct{}

// WithContinuation carries a durable continuation into nested users of the
// unified pause primitive. This lets a tool-auth provider that owns the ONE
// OAuth pause persist the calling operation's continuation without minting a
// second pause record.
func WithContinuation(ctx context.Context, continuation Continuation) context.Context {
	return context.WithValue(ctx, continuationContextKey{}, cloneContinuation(continuation))
}

func continuationFromContext(ctx context.Context) *Continuation {
	continuation, ok := ctx.Value(continuationContextKey{}).(Continuation)
	if !ok || strings.TrimSpace(continuation.Kind) == "" {
		return nil
	}
	cloned := cloneContinuation(continuation)
	return &cloned
}

func validateContinuation(c *Continuation) error {
	if c == nil {
		return nil
	}
	if strings.TrimSpace(c.Kind) == "" {
		return fmt.Errorf("%w: kind is empty", ErrInvalidContinuation)
	}
	for key := range c.Data {
		if strings.TrimSpace(key) == "" {
			return fmt.Errorf("%w: data key is empty", ErrInvalidContinuation)
		}
	}
	return nil
}

func cloneContinuation(c Continuation) Continuation {
	out := Continuation{Kind: strings.TrimSpace(c.Kind)}
	if len(c.Data) > 0 {
		out.Data = make(map[string]string, len(c.Data))
		for key, value := range c.Data {
			out.Data[key] = value
		}
	}
	return out
}
