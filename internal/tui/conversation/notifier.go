package conversation

import (
	"context"
	"sync"

	"github.com/hurtener/Harbor/internal/tui/projection"
)

// UpdateSource yields controller updates to the terminal event loop.
type UpdateSource interface {
	Next(context.Context) (Update, bool)
}

// Notifier preserves every non-batchable update in a bounded queue and keeps
// only the newest batchable projection. Producers backpressure rather than
// dropping lifecycle, intervention, session, or reconciliation updates.
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
		update.Projection.ReconciliationRequired = true
		if update.Projection.ReplayGap == nil {
			update.Projection.ReplayGap = &projection.ReplayGap{Reason: "tui_notifier_coalesced", LastSequence: n.latest.Projection.LastSequence, SeenSequence: update.Projection.LastSequence}
		}
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
func (n *Notifier) Next(ctx context.Context) (Update, bool) {
	for {
		select {
		case update := <-n.critical:
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
			return update, true
		case <-n.wake:
		case <-ctx.Done():
			return Update{}, false
		case <-n.closed:
			return Update{}, false
		}
	}
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
