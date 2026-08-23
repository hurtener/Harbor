// internal/events/projection_source.go — the narrow INTERNAL forward
// projection source over successfully accepted/persisted canonical
// events.
//
// Runtime-owned projections (the durable conversation-turns projection
// and the observability rollup projection) consume canonical events
// through this seam instead of coupling to a bus driver's internals or
// subscribing to the live fan-out. The source is deliberately narrow:
//
//   - It pages a BOUNDED forward window strictly after a caller cursor,
//     globally ordered by the bus-assigned Sequence (the single-runtime
//     cursor). No generic cursor redesign, no outbox, no new canonical
//     event ID, no exactly-once delivery — the existing durable event
//     log and the in-memory ring are the substrate.
//   - It serves canonical persisted events ONLY: bus-internal notices
//     (bus.dropped, subscription_idle_closed, redaction_failed,
//     admin_scope_used) are excluded from pages exactly like the
//     windowed-history surface, so a projection's row space is the
//     conversation/telemetry stream, never bus bookkeeping.
//   - Each page reports the next cursor, the current watermark, an
//     explicit current / catching-up / unavailable quality, and an
//     honest retention-gap signal so a projector never mistakes a
//     truncated ring for a complete stream (the never-silently-lossy
//     rule applied to projection input).
//   - A bounded BEST-EFFORT wake notification fires after each
//     successful persistence of a canonical event, so an idle projector
//     catches up without polling and without any publication coupling:
//     the publish path never waits on, and can never be failed by, a
//     projector. A dropped wake is healed by the next forward page —
//     paging is idempotent from any cursor.
//
// The bus drivers implement ProjectionSource directly (the §4.4 seam
// shape: interface here, concrete in the driver); OpenProjectionSource
// is the discovery helper. A bus that does not implement the seam (or
// a driver whose retention substrate is configured off) reports
// unavailable rather than serving a silent empty stream.
package events

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// ErrProjectionUnavailable — the bus exposes no ProjectionSource
// (OpenProjectionSource type-assertion failed) or a driver that does
// implement the seam has its retention substrate configured off (an
// in-memory bus with replay disabled, a durable bus in best-effort
// mode with the fallback ring disabled). It is never returned for a
// closed bus (ErrBusClosed wins) or for a transient store failure
// (those surface wrapped, fail-loud).
var ErrProjectionUnavailable = errors.New("events: projection source unavailable")

// ProjectionQuality is the explicit catch-up state a projection page
// reports.
type ProjectionQuality int

const (
	// ProjectionCurrent — the page reached the watermark: no further
	// canonical events existed beyond the page at page time. The reader
	// is caught up with the canonical stream and should wait for a wake
	// (or its own schedule) before paging again.
	ProjectionCurrent ProjectionQuality = iota
	// ProjectionCatchingUp — at least one canonical event existed
	// beyond the page at page time (the page hit the caller's limit
	// with more to serve). The reader should page again with
	// ProjectionPage.Next as the new cursor.
	ProjectionCatchingUp
	// ProjectionUnavailable — the driver has no retained substrate to
	// page (retention disabled). The page is empty and the reader must
	// not treat the stream as complete; a projector on this deployment
	// cannot run.
	ProjectionUnavailable
)

// String renders the quality for logs.
func (q ProjectionQuality) String() string {
	switch q {
	case ProjectionCurrent:
		return "current"
	case ProjectionCatchingUp:
		return "catching_up"
	case ProjectionUnavailable:
		return "unavailable"
	default:
		return fmt.Sprintf("ProjectionQuality(%d)", int(q))
	}
}

// ProjectionPage is one bounded forward page of canonical persisted
// events, globally ordered by Sequence (strictly ascending).
type ProjectionPage struct {
	// Events is the page: at most `limit` canonical events with
	// Sequence > the requested cursor, in ascending Sequence order.
	// Bus-internal notices are never present. The returned slice is
	// owned by the caller.
	Events []Event
	// Next is the cursor the reader passes to the next Page call: the
	// Sequence of the last returned event, or the requested cursor
	// unchanged when the page is empty. A consumer that owns a durable
	// projection checkpoint may advance through Watermark after a
	// successfully applied ProjectionCurrent page; that is how it
	// permanently skips excluded internal/fenced sequences without
	// changing this canonical-event cursor shape.
	Next uint64
	// Watermark is the highest sequence that had completed persistence
	// when the page was assembled — the source's high-water mark.
	// It may exceed Next when the intervening sequences belong to
	// excluded bus-internal notices or fenced (erased) sessions.
	Watermark uint64
	// Quality reports whether the page reached the watermark
	// (ProjectionCurrent), whether more canonical events follow it
	// (ProjectionCatchingUp), or whether the driver has no substrate to
	// serve (ProjectionUnavailable).
	Quality ProjectionQuality
	// RetentionGap is the honest signal that canonical events strictly
	// between the requested cursor and the page head may be missing —
	// the ring substrate wrapped and evicted older events. False on the
	// gap-free durable log and for a reader whose cursor already sits
	// inside the retained window. A projection that observes true must
	// treat its history as potentially incomplete (rebuild, or record
	// the gap) rather than assume a gapless stream.
	RetentionGap bool
}

// ProjectionSource is the narrow internal forward-paging source over
// successfully accepted/persisted canonical events. Implementations
// MUST be safe for concurrent use by N goroutines against a single
// shared instance: Page and Watermark never mutate the source, and
// Watch registers an independent best-effort notification sink.
//
// The Sequence cursor is single-runtime: it is the bus's own
// monotonically increasing counter and is only meaningful against the
// bus that issued it. A projector persists its own applied-through
// checkpoint and resumes from it after a Runtime restart; the durable
// driver rehydrates its counter from the persisted log so post-restart
// sequences stay strictly greater than any pre-restart token.
type ProjectionSource interface {
	// Page returns at most limit canonical events with Sequence > after,
	// globally ordered by Sequence. after==0 means "from the earliest
	// retained event" (a fresh projector). A non-positive limit returns
	// an empty page (Next unchanged, Quality Current) — the defensive
	// posture mirrored from the windowed-history surface. Page fails
	// loud: ErrBusClosed after Close, the caller's ctx error when
	// cancelled, and wrapped store errors on the durable driver — never
	// a silent empty page for a real failure.
	Page(ctx context.Context, after uint64, limit int) (ProjectionPage, error)

	// Watermark returns the highest sequence that has completed
	// persistence — the source's current high-water mark. A projector
	// that has not been woken (or lost a wake) compares its checkpoint
	// against the watermark to decide whether a forward page is
	// worthwhile.
	Watermark(ctx context.Context) (uint64, error)

	// Watch registers wake, a bounded best-effort notification sink,
	// and returns a handle that unsubscribes it. After registration the
	// source sends the CURRENT watermark once (when non-zero) so a
	// projector that missed wakes while busy catches up immediately, and
	// then sends the new watermark after every successful persistence of
	// a canonical event. Sends are non-blocking: a full or absent sink
	// is dropped and the publish path is never delayed or failed by a
	// projector (notification loss is healed by forward paging).
	//
	// The caller owns wake's lifecycle (buffer size, close) and MUST
	// Unsubscribe when it stops reading, so a dead projector's sink does
	// not accumulate dropped sends forever.
	Watch(ctx context.Context, wake chan<- uint64) (ProjectionWatch, error)
}

// ProjectionWatch is the handle returned by ProjectionSource.Watch.
type ProjectionWatch interface {
	// Unsubscribe removes the registered sink from the source's
	// notification set. Idempotent.
	Unsubscribe()
}

// ProjectionWatchFunc adapts a plain function to ProjectionWatch.
type ProjectionWatchFunc func()

// Unsubscribe implements ProjectionWatch.
func (f ProjectionWatchFunc) Unsubscribe() { f() }

// OpenProjectionSource returns the bus's ProjectionSource. It is the
// discovery helper for runtime-owned projections: type-asserts the
// bus to the capability (the §4.4 seam), exactly like Replayer and
// HistoryReplayer. A bus that does not implement the seam (or a nil
// bus) fails with ErrProjectionUnavailable so a projector can disable
// itself loudly instead of silently reading an empty stream.
func OpenProjectionSource(bus EventBus) (ProjectionSource, error) {
	if bus == nil {
		return nil, ErrProjectionUnavailable
	}
	ps, ok := bus.(ProjectionSource)
	if !ok {
		return nil, fmt.Errorf("%w: bus %T does not implement ProjectionSource", ErrProjectionUnavailable, bus)
	}
	return ps, nil
}

// ProjectionWakeHub is the bounded best-effort watermark notification
// registry a projection-capable bus driver embeds. The driver calls
// NotifyWatermark after each successful persistence of a canonical
// event; every registered sink receives the new watermark via a
// non-blocking send (a full sink drops the wake — forward paging heals
// the loss). The zero value is ready to use.
//
// The hub deliberately never blocks and never calls back into
// projector code: Register/Unsubscribe only mutate the sink map under
// the mutex, and NotifyWatermark copies the sink set under the mutex
// before sending outside it. A projector failure can therefore never
// make a Publish fail or stall (the no-publication-coupling
// requirement).
type ProjectionWakeHub struct {
	mu    sync.Mutex
	next  uint64
	sinks map[uint64]chan<- uint64
}

// Register subscribes sink to watermark notifications and returns the
// unsubscribe function. The sink channel is caller-owned (bounded by
// the caller); the hub only ever sends to it non-blocking and never
// closes it.
func (h *ProjectionWakeHub) Register(sink chan<- uint64) (unsubscribe func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.sinks == nil {
		h.sinks = map[uint64]chan<- uint64{}
	}
	id := h.next
	h.next++
	h.sinks[id] = sink
	return func() {
		h.mu.Lock()
		delete(h.sinks, id)
		h.mu.Unlock()
	}
}

// NotifyWatermark best-effort sends watermark to every registered
// sink. The send is non-blocking: a sink whose channel is full (a
// slow projector) drops this wake and heals it on its next forward
// page. Safe to call concurrently with Register/Unsubscribe and from
// the driver's publish path.
func (h *ProjectionWakeHub) NotifyWatermark(watermark uint64) {
	h.mu.Lock()
	if len(h.sinks) == 0 {
		h.mu.Unlock()
		return
	}
	sinks := make([]chan<- uint64, 0, len(h.sinks))
	for _, s := range h.sinks {
		sinks = append(sinks, s)
	}
	h.mu.Unlock()
	for _, s := range sinks {
		select {
		case s <- watermark:
		default:
		}
	}
}

// ProjectionPageFromSnapshot selects the bounded forward projection
// page from a sequence-ordered (ascending) snapshot of the driver's
// retained substrate — the shared selection for ring-backed
// implementations (the in-memory driver and the durable driver's
// best-effort fallback ring).
//
// It returns at most limit canonical events with Sequence > after,
// excluding bus-internal notices and any event for which skip returns
// true (a driver's fence/erasure predicate), computes Next/Quality
// exactly (up to limit+1 matches are collected so CatchingUp is
// precise), and refines the driver's eviction flag into the honest
// per-reader RetentionGap: true only when the substrate has evicted
// AND the reader's cursor could actually be affected (asked from the
// beginning, or sitting before the oldest retained canonical event).
// Pure function — no package-level state, safe for concurrent use.
func ProjectionPageFromSnapshot(snapshot []Event, after uint64, limit int, watermark uint64, evicted bool, skip func(Event) bool) ProjectionPage {
	if limit <= 0 {
		return ProjectionPage{Next: after, Watermark: watermark, Quality: ProjectionCurrent}
	}
	matches := make([]Event, 0, limit+1)
	for _, ev := range snapshot {
		if ev.Sequence <= after {
			continue
		}
		if IsBusInternalNotice(ev.Type) {
			continue
		}
		if skip != nil && skip(ev) {
			continue
		}
		matches = append(matches, ev)
		if len(matches) > limit {
			break
		}
	}
	quality := ProjectionCurrent
	if len(matches) > limit {
		quality = ProjectionCatchingUp
		matches = matches[:limit]
	}
	next := after
	if len(matches) > 0 {
		next = matches[len(matches)-1].Sequence
	}
	// Refine the eviction flag into the per-reader gap signal: a
	// wrapped ring dropped events once, but a reader whose cursor sits
	// inside the retained window is unaffected by that history.
	retentionGap := false
	if evicted {
		var oldestCanonical uint64
		for _, ev := range snapshot {
			if IsBusInternalNotice(ev.Type) {
				continue
			}
			oldestCanonical = ev.Sequence
			break
		}
		if oldestCanonical != 0 && (after == 0 || after < oldestCanonical) {
			retentionGap = true
		}
	}
	return ProjectionPage{
		Events:       matches,
		Next:         next,
		Watermark:    watermark,
		Quality:      quality,
		RetentionGap: retentionGap,
	}
}
