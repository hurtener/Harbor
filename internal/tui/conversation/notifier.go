package conversation

import (
	"context"
	"sync"
)

// UpdateSource yields controller updates to the terminal event loop.
type UpdateSource interface {
	Next(context.Context) (Update, bool)
}

// Notifier preserves every non-batchable update in a bounded queue and keeps
// only the newest batchable projection. Producers backpressure rather than
// dropping lifecycle, intervention, session, or reconciliation updates.
//
// Coalescing a batchable update is loss-less: each Update carries the whole
// cumulative Projection, so the newest strictly supersedes any resident one.
// A coalesced update is therefore flagged only with Overflow (the honest
// "frames were merged" signal); it never fabricates ReconciliationRequired or
// a ReplayGap, because nothing was actually dropped.
type Notifier struct {
	critical chan Update
	wake     chan struct{}
	closed   chan struct{}
	once     sync.Once
	mu       sync.Mutex
	latest   *Update
}

// NewNotifier constructs a bounded controller-to-model notifier.
func NewNotifier(capacity int) *Notifier {
	if capacity < 1 {
		capacity = 1
	}
	return &Notifier{critical: make(chan Update, capacity), wake: make(chan struct{}, 1), closed: make(chan struct{})}
}

// Notify enqueues an update. Only explicitly batchable updates may coalesce.
//
// When a batchable update supersedes a resident one, it is marked Overflow so
// the UI can honestly report that intermediate frames were merged. Coalescing
// is loss-less (the cumulative Projection carries everything the merged frames
// carried), so Notify never sets ReconciliationRequired or a ReplayGap on the
// coalesced update.
func (n *Notifier) Notify(update Update) {
	if !update.Batchable {
		select {
		case n.critical <- update:
		case <-n.closed:
		}
		return
	}
	n.mu.Lock()
	if n.latest != nil {
		update.Overflow = true
	}
	copy := update
	n.latest = &copy
	n.mu.Unlock()
	select {
	case n.wake <- struct{}{}:
	default:
	}
}

// Next waits for the next preserved critical update or latest batchable state.
//
// Critical updates take priority. Because projections are cumulative, returning
// a critical frame while an older batchable frame is still resident would let a
// later Next hand back that stale frame and regress observable state (e.g. a
// completed tool back to "running"). To keep newest-wins semantics, Next drops
// any resident latest whose cumulative LastSequence is not newer than the
// critical update it is about to return; a strictly newer resident frame is
// preserved.
func (n *Notifier) Next(ctx context.Context) (Update, bool) {
	for {
		select {
		case update := <-n.critical:
			n.dropStaleLatest(update)
			return update, true
		default:
		}
		n.mu.Lock()
		if n.latest != nil {
			update := *n.latest
			n.latest = nil
			n.mu.Unlock()
			return update, true
		}
		n.mu.Unlock()
		select {
		case update := <-n.critical:
			n.dropStaleLatest(update)
			return update, true
		case <-n.wake:
		case <-ctx.Done():
			return Update{}, false
		case <-n.closed:
			return Update{}, false
		}
	}
}

// dropStaleLatest discards a resident batchable frame that a just-returned
// critical update has caught up to or overtaken. A resident frame that is
// strictly newer (higher cumulative LastSequence) is left in place so it can be
// delivered on the next call.
func (n *Notifier) dropStaleLatest(critical Update) {
	n.mu.Lock()
	if n.latest != nil && n.latest.Projection.LastSequence <= critical.Projection.LastSequence {
		n.latest = nil
	}
	n.mu.Unlock()
}

// Close releases blocked producers and consumers.
func (n *Notifier) Close() { n.once.Do(func() { close(n.closed) }) }

type channelSource struct{ updates <-chan Update }

// ChannelSource adapts test-owned channels without adding drop behavior.
func ChannelSource(updates <-chan Update) UpdateSource { return channelSource{updates: updates} }

func (s channelSource) Next(ctx context.Context) (Update, bool) {
	select {
	case update, ok := <-s.updates:
		return update, ok
	case <-ctx.Done():
		return Update{}, false
	}
}
