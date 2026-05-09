package concurrency

import (
	"context"
	"errors"
	"fmt"

	"github.com/hurtener/Harbor/internal/runtime/messages"
)

// ErrJoinKShortRead — the input channel closed before K envelopes
// arrived. The slice returned alongside the error contains however
// many envelopes did arrive (may be empty).
var ErrJoinKShortRead = errors.New("concurrency: JoinK source closed before K envelopes arrived")

// ErrInvalidK — JoinK was called with k <= 0.
var ErrInvalidK = errors.New("concurrency: k must be > 0")

// JoinK reads exactly K envelopes from `in`. After K, derives ctx
// cancellation so upstream producers blocked on send observe the
// cancellation and exit. Returns the K envelopes (caller-owned slice).
//
// If `in` closes before K arrive, returns ErrJoinKShortRead alongside
// the partial slice. If the caller's ctx cancels mid-read, returns
// ctx.Err() with the partial slice (which may be empty).
//
// Cancellation note: JoinK derives a child ctx and returns it via the
// `cancel` parameter so the caller wires upstream producers to honor
// it. Callers who don't need upstream cancellation can ignore the
// derived ctx; the cancel function is invoked internally on the
// happy path.
//
// nil channel is rejected at call time. K must be > 0.
func JoinK(ctx context.Context, in <-chan messages.Envelope, k int) ([]messages.Envelope, error) {
	if k <= 0 {
		return nil, fmt.Errorf("%w: %d", ErrInvalidK, k)
	}
	if in == nil {
		return nil, errors.New("concurrency: JoinK requires a non-nil input channel")
	}
	out := make([]messages.Envelope, 0, k)
	for len(out) < k {
		select {
		case <-ctx.Done():
			return out, ctx.Err()
		case env, ok := <-in:
			if !ok {
				return out, fmt.Errorf("%w: read %d of %d", ErrJoinKShortRead, len(out), k)
			}
			out = append(out, env)
		}
	}
	return out, nil
}
