// Package stream is the Harbor Protocol SSE event transport — the
// server→client half of the wire binding RFC §5.4 resolves to (SSE for
// events + REST/JSON for control). It is a thin adapter over the Phase
// 05 events.EventBus: a Handler opens a triple-scoped events.Subscription
// and frames each events.Event as an SSE block (frame.go). The SSE
// transport adds the wire framing and the connection lifecycle; it adds
// NO second event channel — brief 06's "one bus, no parallel
// observability channel" is load-bearing.
//
// # The route shape
//
//	GET /v1/events
//
// The identity triple is carried in request headers (X-Harbor-Tenant /
// X-Harbor-User / X-Harbor-Session) — a header carrier, not a query
// string, so the triple is not logged in access logs by default and
// Phase 61's JWT validation slots in at the same choke point. An
// optional X-Harbor-Event-Type header (repeatable) narrows the
// subscription's event-type selector.
//
// # Identity at the edge (RFC §5.5, CLAUDE.md §6)
//
// The handler resolves the triple from the headers and rejects a request
// with any missing component closed — HTTP 401, before any subscription
// is opened. The SSE stream is ALWAYS triple-scoped: events.Filter.Admin
// (cross-tenant fan-in) is NOT exposed on the wire in Phase 60 — it needs
// the cryptographic scope claim Phase 61 adds. resolveIdentity is the
// single choke point Phase 61 slots JWT validation into.
//
// # Reconnect cursor
//
// SSE's native reconnect mechanism is the Last-Event-ID header: a client
// that drops echoes back the `id:` of the last frame it saw. The handler
// maps that onto an events.Cursor and, when the bus driver implements
// events.Replayer, replays the events strictly newer than the cursor
// before live-tailing — so a reconnecting client does not miss the gap.
// When the driver does not support replay (or replay is configured off),
// the handler live-tails from the reconnect point and emits a single
// explicit `stream.replay_unavailable` comment frame so the gap is
// SURFACED, never silently masked (CLAUDE.md §5).
//
// # Concurrent reuse (D-025)
//
// Handler is a compiled artifact: the bus, the logger, the keepalive
// interval and the clock are set once at construction and never mutated.
// ServeHTTP holds no per-request state on the Handler — each request
// gets its own events.Subscription, its own keepalive ticker, and its
// own goroutine, all torn down before ServeHTTP returns. One Handler
// serves N concurrent streams safely; the goroutine for a stream is
// cancelled by that request's ctx, never by a shared handler-level ctx
// (CLAUDE.md §5 "Concurrency"). internal/protocol/transports/
// concurrent_test.go pins it under -race.
package stream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
)

// minKeepalive is the floor WithKeepalive enforces. A keepalive interval
// shorter than this is almost certainly a test driving the keepalive
// path deterministically — which is fine — but a sub-millisecond
// interval would busy-spin the ticker, so the floor keeps the loop
// well-behaved.
const minKeepalive = time.Millisecond

// RoutePattern is the http.ServeMux pattern the SSE transport registers
// under. Exported so internal/protocol/transports can mount the handler
// under the same pattern it documents.
const RoutePattern = "GET /v1/events"

// Identity-carrier header names. The triple travels in headers, not a
// query string, so it does not land in access logs by default and so
// Phase 61's JWT validation replaces a single resolve step.
//
// HeaderRun is optional — when present, the subscription is
// run-scoped (events.Filter.Run is set, so only events with a
// matching RunID flow through). When absent, the subscription is
// session-scoped (the Phase 60 default; every run in the session is
// observed). Added in PR #91 / D-082 per Wave 10 audit WARN-5.
const (
	HeaderTenant    = "X-Harbor-Tenant"
	HeaderUser      = "X-Harbor-User"
	HeaderSession   = "X-Harbor-Session"
	HeaderRun       = "X-Harbor-Run"
	HeaderEventType = "X-Harbor-Event-Type"
)

// defaultKeepalive is the interval between SSE keepalive comment frames
// when WithKeepalive is not supplied. 15s is comfortably under the
// common 30–60s idle-timeout of intermediary proxies.
const defaultKeepalive = 15 * time.Second

// reconnectRetryMS is the `retry:` value sent once at stream open — the
// backoff (in ms) a client waits before reconnecting after a drop.
const reconnectRetryMS = 3000

// Handler is the Protocol SSE event transport. It is built once per
// Runtime process via NewHandler and shared across every stream request;
// ServeHTTP is safe for concurrent use by N goroutines (D-025).
type Handler struct {
	bus       events.EventBus
	logger    *slog.Logger
	keepalive time.Duration
}

// Option configures a Handler at construction time.
type Option func(*Handler)

// WithLogger sets the slog.Logger the handler logs subscription /
// streaming failures to. A nil logger (the default) routes to
// slog.Default().
func WithLogger(l *slog.Logger) Option {
	return func(h *Handler) {
		if l != nil {
			h.logger = l
		}
	}
}

// WithKeepalive overrides the interval between SSE keepalive comment
// frames. A value below minKeepalive is clamped up to the floor; a
// non-positive value is ignored (the default is kept). Tests drive the
// keepalive path deterministically by supplying a short interval — the
// keepalive frame is observable on the wire, so a short interval makes
// the path testable without a time.Sleep-as-synchronisation antipattern.
func WithKeepalive(d time.Duration) Option {
	return func(h *Handler) {
		if d <= 0 {
			return
		}
		if d < minKeepalive {
			d = minKeepalive
		}
		h.keepalive = d
	}
}

// NewHandler builds the Protocol SSE event transport over the Phase 05
// events.EventBus. The bus is mandatory — a nil fails loud with
// ErrMisconfigured rather than building a handler that would nil-panic
// on the first request (CLAUDE.md §5).
//
// The returned *Handler is immutable after construction (D-025) and safe
// for concurrent use by N goroutines.
func NewHandler(bus events.EventBus, opts ...Option) (*Handler, error) {
	if bus == nil {
		return nil, fmt.Errorf("%w: events.EventBus is nil", ErrMisconfigured)
	}
	h := &Handler{
		bus:       bus,
		logger:    slog.Default(),
		keepalive: defaultKeepalive,
	}
	for _, opt := range opts {
		opt(h)
	}
	return h, nil
}

// ErrMisconfigured — NewHandler was called with a nil EventBus.
var ErrMisconfigured = errors.New("stream: SSE transport missing a mandatory dependency")

// ServeHTTP implements http.Handler. It resolves the identity triple at
// the edge, opens a triple-scoped events.Subscription, optionally
// replays from a Last-Event-ID reconnect cursor, and live-tails events
// as SSE frames until the client disconnects or the bus closes.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writePlainError(w, http.StatusMethodNotAllowed, "event stream accepts GET only")
		return
	}

	// The SSE stream is a long-lived chunked response; it requires an
	// http.Flusher to push each frame to the client. A ResponseWriter
	// that cannot flush cannot serve SSE — fail loud rather than
	// buffering the whole stream into oblivion.
	flusher, ok := w.(http.Flusher)
	if !ok {
		writePlainError(w, http.StatusInternalServerError, "response writer does not support streaming")
		return
	}

	// Identity at the edge — resolve the triple. Phase 61: when the
	// auth.Middleware ran before us, the verified identity is in
	// r.Context() (via identity.With) and we prefer it. Phase 60
	// fallback: when no middleware ran, resolve from the X-Harbor-*
	// carrier headers via resolveIdentity. A missing component on
	// either path fails the request closed (401) before any
	// subscription is opened (RFC §5.5, CLAUDE.md §6 rule 9).
	id, err := resolveIdentity(r)
	if err != nil {
		writeProtocolError(w, http.StatusUnauthorized,
			protoerrors.Newf(protoerrors.CodeIdentityRequired,
				"identity scope incomplete: %v", err))
		return
	}

	// Phase 61: an `?admin=1` query param requests cross-tenant fan-in
	// (events.Filter.Admin = true). This is gated on a verified scope
	// claim — ScopeAdmin OR ScopeConsoleFleet (D-079). A request that
	// asks for admin without the scope is rejected 403 with the
	// canonical Phase 72 / D-105 wire code CodeIdentityScopeRequired
	// (distinct from CodeScopeMismatch, which is reserved for the
	// steering-control scope-claim path per RFC §6.3). A request that
	// does NOT ask for admin is the default triple-scoped stream.
	wantAdmin := r.URL.Query().Get("admin") == "1"
	if wantAdmin && !(auth.HasScope(r.Context(), auth.ScopeAdmin) || auth.HasScope(r.Context(), auth.ScopeConsoleFleet)) {
		writeProtocolError(w, http.StatusForbidden,
			protoerrors.Newf(protoerrors.CodeIdentityScopeRequired,
				"admin fan-in requires a verified `admin` or `console:fleet` scope (D-079)"))
		return
	}

	filter := events.Filter{
		Tenant:  id.TenantID,
		User:    id.UserID,
		Session: id.SessionID,
		Run:     r.Header.Get(HeaderRun),
		Types:   parseEventTypes(r),
		Admin:   wantAdmin,
	}

	sub, err := h.bus.Subscribe(r.Context(), filter)
	if err != nil {
		// A rejected Subscribe (identity gate, subscriber-limit, closed
		// bus) — surface it; do not silently 200 with an empty stream.
		// Phase 72 / D-105: both ErrIdentityScopeRequired (filter
		// elided the triple and admin was false) and
		// ErrAdminScopeRequired (admin requested without the scope
		// claim) collapse onto the canonical wire code
		// CodeIdentityScopeRequired (403). Phase 61's `?admin=1` gate
		// normally short-circuits BEFORE Subscribe, but the mapping
		// must hold if a future filter variant lets the bus return
		// either sentinel.
		switch {
		case errors.Is(err, events.ErrIdentityScopeRequired),
			errors.Is(err, events.ErrAdminScopeRequired):
			writeProtocolError(w, http.StatusForbidden,
				protoerrors.Newf(protoerrors.CodeIdentityScopeRequired,
					"event subscription scope insufficient: %v", err))
			return
		case errors.Is(err, events.ErrSubscriberLimitReached):
			writeProtocolError(w, http.StatusTooManyRequests,
				protoerrors.Newf(protoerrors.CodeRuntimeError,
					"event subscriber limit reached: %v", err))
			return
		case errors.Is(err, events.ErrBusClosed):
			writeProtocolError(w, http.StatusServiceUnavailable,
				protoerrors.Newf(protoerrors.CodeRuntimeError,
					"event bus closed: %v", err))
			return
		default:
			writeProtocolError(w, http.StatusInternalServerError,
				protoerrors.Newf(protoerrors.CodeRuntimeError,
					"event stream could not be opened: %v", err))
			return
		}
	}
	defer sub.Cancel()

	// SSE response headers. text/event-stream + no-cache + the
	// connection-keep-alive hint. Once these are written the response
	// is committed — every later failure is logged, not surfaced as an
	// HTTP status.
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no") // disable proxy buffering (nginx)
	w.WriteHeader(http.StatusOK)

	// Tell the client how long to back off before reconnecting.
	if _, err := w.Write(retryFrame(reconnectRetryMS)); err != nil {
		return
	}
	flusher.Flush()

	logger := h.logger.With(
		slog.String("tenant_id", id.TenantID),
		slog.String("user_id", id.UserID),
		slog.String("session_id", id.SessionID),
	)

	// Reconnect: if the client echoed a Last-Event-ID, replay everything
	// strictly newer than that cursor before live-tailing.
	if cursor, ok := parseLastEventID(r, id.SessionID); ok {
		h.replayFromCursor(r.Context(), w, flusher, filter, cursor, logger)
	}

	h.streamLoop(r.Context(), w, flusher, sub, logger)
}

// streamLoop is the live-tail loop: it forwards each event the
// subscription delivers as an SSE frame, emits a keepalive comment on
// the keepalive interval, and returns when the client disconnects
// (ctx.Done), the bus closes the subscription channel, or a write
// fails. The loop owns no goroutine of its own — it runs on the request
// goroutine, so it is joined the moment ServeHTTP returns (no leak).
func (h *Handler) streamLoop(
	ctx context.Context,
	w http.ResponseWriter,
	flusher http.Flusher,
	sub events.Subscription,
	logger *slog.Logger,
) {
	ticker := time.NewTicker(h.keepalive)
	defer ticker.Stop()

	evCh := sub.Events()
	for {
		select {
		case <-ctx.Done():
			// Client disconnected or server shutting down. The deferred
			// sub.Cancel() in ServeHTTP releases the subscription.
			return

		case ev, open := <-evCh:
			if !open {
				// The bus closed the subscription (Close or idle-reap).
				// End the stream cleanly.
				logger.DebugContext(ctx, "stream: subscription channel closed by bus")
				return
			}
			frame, err := encodeEvent(ev)
			if err != nil {
				// A payload the SSE transport cannot encode — log loud
				// and skip this frame; do not tear the whole stream
				// down for one bad event.
				logger.ErrorContext(ctx, "stream: event encode failed",
					slog.String("event_type", string(ev.Type)),
					slog.Uint64("sequence", ev.Sequence))
				continue
			}
			if _, err := w.Write(frame); err != nil {
				logger.DebugContext(ctx, "stream: client write failed; closing stream")
				return
			}
			flusher.Flush()

		case <-ticker.C:
			if _, err := w.Write(keepaliveFrame); err != nil {
				logger.DebugContext(ctx, "stream: keepalive write failed; closing stream")
				return
			}
			flusher.Flush()
		}
	}
}

// replayFromCursor replays the events strictly newer than cursor before
// the live tail begins, when the bus driver implements events.Replayer.
// When replay is unavailable (the driver does not implement Replayer, or
// the configured ring is off, or the cursor is too old) the gap is
// SURFACED with an explicit comment frame — never silently masked
// (CLAUDE.md §5: fail loudly, no silent degradation).
func (h *Handler) replayFromCursor(
	ctx context.Context,
	w http.ResponseWriter,
	flusher http.Flusher,
	filter events.Filter,
	cursor events.Cursor,
	logger *slog.Logger,
) {
	replayer, ok := h.bus.(events.Replayer)
	if !ok {
		h.surfaceReplayGap(ctx, w, flusher, "driver does not support replay", logger)
		return
	}
	snapshot, err := replayer.Replay(ctx, cursor, filter)
	if err != nil {
		if errors.Is(err, events.ErrReplayUnavailable) {
			h.surfaceReplayGap(ctx, w, flusher, "replay not configured on this driver", logger)
			return
		}
		if errors.Is(err, events.ErrCursorTooOld) {
			h.surfaceReplayGap(ctx, w, flusher, "reconnect cursor older than the retained window", logger)
			return
		}
		logger.ErrorContext(ctx, "stream: replay failed", slog.String("error", err.Error()))
		h.surfaceReplayGap(ctx, w, flusher, "replay failed", logger)
		return
	}
	for _, ev := range snapshot {
		frame, encErr := encodeEvent(ev)
		if encErr != nil {
			logger.ErrorContext(ctx, "stream: replayed event encode failed",
				slog.String("event_type", string(ev.Type)),
				slog.Uint64("sequence", ev.Sequence))
			continue
		}
		if _, err := w.Write(frame); err != nil {
			logger.DebugContext(ctx, "stream: client write failed during replay")
			return
		}
	}
	flusher.Flush()
}

// surfaceReplayGap writes an explicit SSE comment frame announcing that
// a reconnecting client's gap could not be replayed. The client sees the
// gap and can fall through to a durable-log read (Phase 57) or accept
// the loss knowingly — what it does NOT get is a silent stream that
// looks complete but skipped events.
func (h *Handler) surfaceReplayGap(
	ctx context.Context,
	w http.ResponseWriter,
	flusher http.Flusher,
	reason string,
	logger *slog.Logger,
) {
	logger.WarnContext(ctx, "stream: reconnect replay gap surfaced to client", slog.String("reason", reason))
	frame := []byte(": stream.replay_unavailable " + reason + "\n\n")
	if _, err := w.Write(frame); err != nil {
		return
	}
	flusher.Flush()
}

// resolveIdentity reads the identity triple. Phase 61: prefer the
// verified identity attached to r.Context() by auth.Middleware
// (identity.With). Phase 60 fallback: when no middleware ran, read
// from the X-Harbor-* carrier headers and validate.
//
// This is the single identity choke point on the SSE transport — the
// Phase 61 ctx-first preference is additive and does not reshape
// ServeHTTP. When neither path produces a complete triple, the
// request fails closed (401).
func resolveIdentity(r *http.Request) (identity.Identity, error) {
	if id, ok := identity.From(r.Context()); ok {
		// auth.Middleware already ran identity.Validate against the
		// JWT claims — but defence in depth, re-validate here in case
		// some future caller attached an identity directly without
		// going through the middleware.
		if err := identity.Validate(id); err == nil {
			return id, nil
		}
	}
	id := identity.Identity{
		TenantID:  r.Header.Get(HeaderTenant),
		UserID:    r.Header.Get(HeaderUser),
		SessionID: r.Header.Get(HeaderSession),
	}
	if err := identity.Validate(id); err != nil {
		return identity.Identity{}, err
	}
	return id, nil
}

// parseEventTypes reads the optional repeatable X-Harbor-Event-Type
// header into an events.EventType selector. An empty selector matches
// every event type (events.Filter semantics).
func parseEventTypes(r *http.Request) []events.EventType {
	raw := r.Header.Values(HeaderEventType)
	if len(raw) == 0 {
		return nil
	}
	out := make([]events.EventType, 0, len(raw))
	for _, s := range raw {
		if s == "" {
			continue
		}
		out = append(out, events.EventType(s))
	}
	return out
}

// parseLastEventID maps the SSE Last-Event-ID reconnect header onto an
// events.Cursor scoped to the request's session. A missing or malformed
// header means "no reconnect cursor" — the stream starts fresh from the
// live tail (the false return), which is the correct first-connect
// behaviour.
func parseLastEventID(r *http.Request, sessionID string) (events.Cursor, bool) {
	raw := r.Header.Get("Last-Event-ID")
	if raw == "" {
		return events.Cursor{}, false
	}
	seq, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return events.Cursor{}, false
	}
	return events.Cursor{SessionID: sessionID, Sequence: seq}, true
}

// writePlainError writes a pre-stream error as a plain-text body with
// the given status. Used only before the SSE response is committed —
// once the text/event-stream headers are written the response status is
// fixed and later failures are logged, not surfaced. Reserved for
// pre-routing failures (non-GET method, missing Flusher) that aren't
// well-modelled by the canonical Protocol error envelope.
func writePlainError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(msg + "\n"))
}

// writeProtocolError writes a pre-stream rejection as the canonical
// Protocol error envelope (JSON `{code, message}`) with the given
// status. Phase 72 / D-105: the SSE transport's rejection paths
// (identity missing, scope insufficient, bus refused) return a typed
// wire envelope so third-party Consoles branch on the `code` field
// instead of parsing free-form prose. A marshal failure degrades to a
// bare status with no body — never a partial / truncated body.
func writeProtocolError(w http.ResponseWriter, status int, perr *protoerrors.Error) {
	buf, err := json.Marshal(perr)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(buf)
}
