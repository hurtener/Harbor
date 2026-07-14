// Package stream — addition: the `events.list` durable, time-ranged,
// cross-session raw-event read handler. Like `state.history`, the
// `memory.*` reads, and `pause.list`, it is a one-shot request/response —
// POST JSON in, JSON out — and lives in the stream package because its
// identity edge is the same one Subscribe / Replay / state.history use, and
// it reads the same durable event-bus substrate the SSE stream tails.
//
// Route shape:
//
//	POST /v1/events/list
//
// It completes the events surface beside `events.subscribe` (the
// forward-only live tail) and `events.aggregate` (bucketed counts, no
// payloads): `events.list` returns a bounded, tail-first page of the flat
// types.StateEvent rows the SSE and state.history already project — the
// EXISTING EventFilter (identity axes + since/until + event-type selector)
// plus a sequence-based scroll-up cursor mirroring state.history's paging
// grammar.
//
// The handler resolves identity at the edge (an incomplete triple →
// CodeIdentityRequired / 401), gates a cross-tenant (fleet) filter on the
// verified `admin` OR `console:fleet` scope claim derived SOLELY from the
// verified ctx (the request body carries NO elevation knob — matching the
// sibling events.aggregate handler), folds the caller's triple into any
// elided identity axis, asserts the bus implements events.HistoryReplayer
// (a bus without the windowed capability → CodeRuntimeError, never a silent
// empty page), dispatches ListWindow (which emits one
// audit.admin_scope_used per widened request), projects events → the flat
// types.StateEvent wire shape (heavy content carried by a routable
// types.StateArtifactRef, never inline — the shared projectEvent helper),
// and encodes.
package stream

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"github.com/hurtener/Harbor/internal/artifacts"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
)

// EventsListRoutePattern is the route the events-list handler mounts under.
// Exported so internal/protocol/transports can mount it under the same
// pattern it documents.
const EventsListRoutePattern = "POST /v1/events/list"

// maxEventsListBodyBytes bounds an events.list request body. The wire
// payload is small (an identity + a filter + a cursor + a limit); 64 KiB is
// comfortably over the realistic ceiling and fails closed on a client that
// streams an unbounded body at the edge.
const maxEventsListBodyBytes = 64 << 10

// ErrEventsListMisconfigured — NewEventsListHandler was called with a nil
// mandatory dependency (EventBus or ArtifactStore).
var ErrEventsListMisconfigured = errors.New("stream: events-list handler missing a mandatory dependency")

// EventsListHandler serves the `events.list` windowed-read route. It is a
// safe-for-concurrent-reuse compiled artifact: every field is set once at
// construction; ServeHTTP holds no per-request state.
type EventsListHandler struct {
	bus       events.EventBus
	artifacts artifacts.ArtifactStore
	logger    *slog.Logger
}

// EventsListOption configures NewEventsListHandler at construction.
type EventsListOption func(*EventsListHandler)

// WithEventsListLogger sets the slog.Logger the handler logs decode /
// projection failures to. A nil logger (the default) routes to
// slog.Default().
func WithEventsListLogger(l *slog.Logger) EventsListOption {
	return func(h *EventsListHandler) {
		if l != nil {
			h.logger = l
		}
	}
}

// NewEventsListHandler builds the events-list handler over an
// events.EventBus + an artifacts.ArtifactStore. Both are mandatory — a nil
// fails loud with ErrEventsListMisconfigured rather than building a handler
// that would nil-panic on the first request (CLAUDE.md §5). The bus need
// not implement events.HistoryReplayer at construction; a bus without the
// windowed capability is surfaced per-request as CodeRuntimeError.
//
// The returned *EventsListHandler is immutable after construction and safe
// for concurrent use by N goroutines.
func NewEventsListHandler(bus events.EventBus, arts artifacts.ArtifactStore, opts ...EventsListOption) (*EventsListHandler, error) {
	if bus == nil {
		return nil, fmt.Errorf("%w: events.EventBus is nil", ErrEventsListMisconfigured)
	}
	if arts == nil {
		return nil, fmt.Errorf("%w: artifacts.ArtifactStore is nil", ErrEventsListMisconfigured)
	}
	h := &EventsListHandler{
		bus:       bus,
		artifacts: arts,
		logger:    slog.Default(),
	}
	for _, opt := range opts {
		opt(h)
	}
	return h, nil
}

// Handler returns the http.Handler for `POST /v1/events/list`.
func (h *EventsListHandler) Handler() http.Handler {
	return http.HandlerFunc(h.serve)
}

// serve answers `POST /v1/events/list`.
func (h *EventsListHandler) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeEventsListError(w, protoerrors.CodeInvalidRequest, http.StatusMethodNotAllowed,
			"events.list accepts POST only")
		return
	}

	// Identity edge — a missing / incomplete triple fails closed with
	// CodeIdentityRequired (401).
	verified, err := resolveIdentity(r)
	if err != nil {
		writeEventsListError(w, protoerrors.CodeIdentityRequired, http.StatusUnauthorized,
			"identity scope incomplete: "+err.Error())
		return
	}

	var req prototypes.EventsListRequest
	if perr := decodeEventsListBody(w, r, &req); perr != nil {
		writeEventsListError(w, perr.code, perr.status, perr.message)
		return
	}

	if req.Limit < 0 {
		writeEventsListError(w, protoerrors.CodeInvalidRequest, http.StatusBadRequest,
			"events.list limit must be >= 0 (zero ⇒ default)")
		return
	}
	// A structurally-invalid time range (Until <= Since, both set) is
	// rejected — the posture the EventFilter godoc names.
	if !req.Filter.Since.IsZero() && !req.Filter.Until.IsZero() &&
		!req.Filter.Until.After(req.Filter.Since) {
		writeEventsListError(w, protoerrors.CodeInvalidRequest, http.StatusBadRequest,
			"events.list: filter.until must be strictly after filter.since")
		return
	}
	limit := clampEventsListLimit(req.Limit)

	// Cross-tenant (fleet) gate — the wire filter may name a tenant other
	// than the caller's, or multiple tenants. FilterFromWire flags this via
	// RequiresAdminScope; we consult the verified ctx scope (never the
	// request body) before widening. A request that asks for cross-tenant
	// fan-in without the closed two-scope claim is rejected 403 with
	// CodeIdentityScopeRequired — matching the events.aggregate handler so
	// the Events page's live and historical feeds can never diverge on
	// authz.
	conv := events.FilterFromWire(req.Filter, verified.TenantID, verified.UserID, verified.SessionID)
	widened := conv.RequiresAdminScope
	if widened {
		if !auth.HasScope(r.Context(), auth.ScopeAdmin) && !auth.HasScope(r.Context(), auth.ScopeConsoleFleet) {
			writeEventsListError(w, protoerrors.CodeIdentityScopeRequired, http.StatusForbidden,
				"cross-tenant events.list requires a verified `admin` or `console:fleet` scope")
			return
		}
	}

	// Fold elided identity axes so the driver's per-event MatchWire enforces
	// the intended scope. The fold is asymmetric by design, mirroring
	// events.aggregate:
	//
	//   - Tenant is ALWAYS folded to the caller's own when elided —
	//     name-to-widen parity with the tasks.list / agents.list fleet
	//     selector (an empty tenant axis never fans across every tenant).
	//   - User and Session fold to the caller's own ONLY on the NON-widened
	//     path. On a widened read (cross-principal / multi-value, scope-gated
	//     above, audited by the substrate) an elided user/session means "all
	//     users / all sessions in the authorized tenant scope" — the fleet
	//     fan-in the widened flag grants. Folding them narrowed a
	//     TenantIDs:[T2] admin read to T2+caller-user+caller-session → EMPTY.
	//
	// A non-admin naming a foreign principal is rejected 403 above BEFORE this
	// fold, so the un-folded user/session path is reachable only by a verified
	// admin / console:fleet caller.
	effFilter := req.Filter
	if len(effFilter.TenantIDs) == 0 {
		effFilter.TenantIDs = []string{verified.TenantID}
	}
	if !widened {
		if len(effFilter.UserIDs) == 0 {
			effFilter.UserIDs = []string{verified.UserID}
		}
		if len(effFilter.SessionIDs) == 0 {
			effFilter.SessionIDs = []string{verified.SessionID}
		}
	}

	// Assert the bus implements the windowed-read capability. A bus without
	// it is surfaced loud (CodeRuntimeError / 500) — never a silent empty
	// page (CLAUDE.md §5 / §13).
	hr, ok := h.bus.(events.HistoryReplayer)
	if !ok {
		writeEventsListError(w, protoerrors.CodeRuntimeError, http.StatusInternalServerError,
			"events.list: the configured event bus does not support windowed history reads")
		return
	}

	page, err := hr.ListWindow(r.Context(), events.EventListQuery{
		Filter: effFilter,
		Before: req.Cursor,
		Limit:  limit,
		Admin:  widened,
	})
	if err != nil {
		code, status, msg := classifyEventsListError(err)
		writeEventsListError(w, code, status, msg)
		return
	}

	resp := h.project(r.Context(), page)
	h.encode(r.Context(), w, verified, &resp)
}

// project turns a windowed page into the wire response: flat StateEvent
// rows (heavy content by routable StateArtifactRef — the shared
// projectEvent helper, byte-identical to state.history) plus the scroll-up
// cursor and the honest retention-gap signal.
func (h *EventsListHandler) project(ctx context.Context, page events.EventListPage) prototypes.EventsListResponse {
	out := prototypes.EventsListResponse{
		Events:     make([]prototypes.StateEvent, 0, len(page.Events)),
		NextCursor: page.NextCursor,
		HasMore:    page.HasMore,
		Truncated:  page.Truncated,
	}
	for _, ev := range page.Events {
		out.Events = append(out.Events, h.projectEvent(ctx, ev))
	}
	return out
}

// projectEvent flattens one event into the StateEvent wire shape and
// enriches any routable artifact refs from the artifact store — the SHARED
// helper defined in state_history_handler.go, so events.list rows are
// byte-identical to state.history rows for the same events.
func (h *EventsListHandler) projectEvent(ctx context.Context, ev events.Event) prototypes.StateEvent {
	// Delegate to the package-shared projection so the row shape,
	// payloadWireValue pass-through, and artifact-ref seeding match
	// state.history exactly (no copy — one projection).
	return projectStateEvent(ctx, h.artifacts, ev)
}

// classifyEventsListError maps an events-layer error onto a canonical
// Protocol Code + HTTP status + safe operator-facing message.
func classifyEventsListError(err error) (protoerrors.Code, int, string) {
	switch {
	case errors.Is(err, events.ErrIdentityScopeRequired):
		return protoerrors.CodeIdentityRequired, http.StatusUnauthorized,
			"events.list: identity scope incomplete"
	case errors.Is(err, events.ErrReplayUnavailable):
		return protoerrors.CodeRuntimeError, http.StatusInternalServerError,
			"events.list: windowed history reads are not available on this driver"
	case errors.Is(err, events.ErrBusClosed):
		return protoerrors.CodeRuntimeError, http.StatusInternalServerError,
			"events.list: event bus is closed"
	default:
		return protoerrors.CodeRuntimeError, http.StatusInternalServerError,
			"events.list: request failed"
	}
}

// decodeEventsListBody reads the bounded request body into dst.
func decodeEventsListBody(w http.ResponseWriter, r *http.Request, dst *prototypes.EventsListRequest) *stateHistoryError {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxEventsListBodyBytes))
	if err != nil {
		return &stateHistoryError{
			code: protoerrors.CodeInvalidRequest, status: http.StatusBadRequest,
			message: "failed to read request body: " + err.Error(),
		}
	}
	if len(body) == 0 {
		// An empty body carries the caller's own-tuple default filter — the
		// resolveIdentity edge already gated identity.
		return nil
	}
	dec := json.NewDecoder(bytesReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return &stateHistoryError{
			code: protoerrors.CodeInvalidRequest, status: http.StatusBadRequest,
			message: "failed to decode request body: " + err.Error(),
		}
	}
	return nil
}

// clampEventsListLimit applies the wire page bounds: zero ⇒ default; above
// the max ⇒ the max.
func clampEventsListLimit(limit int) int {
	if limit <= 0 {
		return prototypes.DefaultEventsListLimit
	}
	if limit > prototypes.MaxEventsListLimit {
		return prototypes.MaxEventsListLimit
	}
	return limit
}

// encode writes a JSON 200 response. An encode failure is logged loudly but
// not re-surfaced — the status line is already committed.
func (h *EventsListHandler) encode(ctx context.Context, w http.ResponseWriter, id identity.Identity, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		h.logger.WarnContext(ctx, "events.list: response encode failed",
			slog.String("error", err.Error()),
			slog.String("tenant_id", id.TenantID),
			slog.String("user_id", id.UserID),
			slog.String("session_id", id.SessionID))
	}
}

// writeEventsListError writes a JSON error body with the canonical Protocol
// Code + the supplied HTTP status.
func writeEventsListError(w http.ResponseWriter, code protoerrors.Code, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(&protoerrors.Error{ //nolint:errcheck // response status already committed — a write error cannot be recovered here.
		Code:    code,
		Message: message,
	})
}
