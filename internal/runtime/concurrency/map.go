// Package concurrency ships Harbor's runtime concurrency primitives —
// Phase 14 of the runtime kernel chain (RFC §6.1).
//
// Two stateless helpers:
//
//   - MapConcurrent: runs fn over each input envelope with at most
//     maxConcurrency goroutines in flight; preserves output order.
//   - JoinK: reads exactly K envelopes from a channel; cancels the
//     remaining producers via ctx; short-read returns ErrJoinKShortRead.
//
// Both are pure functions — no shared state, no compiled artifacts,
// trivially safe to call from N concurrent runs (D-025 N/A by
// construction).
package concurrency

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/hurtener/Harbor/internal/runtime/messages"
)

// ErrInvalidConcurrency — MapConcurrent was called with maxConcurrency
// <= 0. Wraps the offending value for operator visibility.
var ErrInvalidConcurrency = errors.New("concurrency: maxConcurrency must be > 0")

// MapConcurrent runs fn over each envelope in `in` with at most
// maxConcurrency goroutines in flight. Output preserves input order.
// Returns the first error encountered; remaining work is cancelled.
//
// Cancellation: the function derives a child ctx from the caller's
// ctx. On the first fn error, the child ctx is cancelled, so any
// in-flight fn invocations that honor ctx will exit promptly. The
// caller's ctx is never cancelled.
//
// Order preservation: a pre-allocated output slice is indexed by input
// position; goroutines write to their own slot. No locks needed for
// the slice itself — distinct indices are written by distinct
// goroutines.
//
// nil fn is rejected at call time. Empty input returns nil, nil.
func MapConcurrent(
	ctx context.Context,
	in []messages.Envelope,
	fn func(context.Context, messages.Envelope) (messages.Envelope, error),
	maxConcurrency int,
) ([]messages.Envelope, error) {
	if maxConcurrency <= 0 {
		return nil, fmt.Errorf("%w: %d", ErrInvalidConcurrency, maxConcurrency)
	}
	if fn == nil {
		return nil, errors.New("concurrency: MapConcurrent requires a non-nil fn")
	}
	if len(in) == 0 {
		return nil, nil
	}

	out := make([]messages.Envelope, len(in))

	derivedCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	sem := make(chan struct{}, maxConcurrency)

	var (
		wg       sync.WaitGroup
		errMu    sync.Mutex
		firstErr error
	)
	captureErr := func(err error) {
		errMu.Lock()
		defer errMu.Unlock()
		if firstErr == nil {
			firstErr = err
			cancel()
		}
	}

	for i, env := range in {
		select {
		case <-derivedCtx.Done():
			// Either caller ctx cancelled or a prior fn errored.
			break
		default:
		}
		select {
		case sem <- struct{}{}:
		case <-derivedCtx.Done():
			// ctx cancelled before we could acquire a slot; bail.
			wg.Wait()
			if firstErr != nil {
				return nil, firstErr
			}
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			return out, nil
		}
		wg.Add(1)
		idx := i
		input := env
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			result, err := fn(derivedCtx, input)
			if err != nil {
				captureErr(err)
				return
			}
			out[idx] = result
		}()
	}
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
