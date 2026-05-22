// Package stream — Wave 13 additions (Phase 72a): the
// `events.aggregate` HTTP handler. Unlike `events.subscribe` (long-lived
// SSE), this is a one-shot request/response — POST JSON in, JSON out.
// It lives in the stream package because it shares the same events-
// subsystem dependencies and route prefix (`/v1/events*`), and its
// scope-claim gating is the same one Subscribe uses.
//
// Route shape:
//
//	POST /v1/events/aggregate
//
// The handler reads identity from r.Context() (auth.Middleware) and
// gates cross-tenant filters on auth.HasScope(ScopeAdmin) OR
// auth.HasScope(ScopeConsoleFleet). The response body is the wire
// EventAggregateResponse JSON; on failure, a JSON error body with the
// canonical Protocol Code.
package stream

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
)

// AggregateRoutePattern is the http.ServeMux pattern the events-aggregate
// handler registers under. Exported so internal/protocol/transports can
// mount the handler under the same pattern it documents.
const AggregateRoutePattern = "POST /v1/events/aggregate"

// maxAggregateBodyBytes bounds the aggregate request body. The wire
// payload is small (a filter + two durations); 32 KiB is comfortably
// over the realistic ceiling and fails closed on a client that streams
// an unbounded body at the edge.
const maxAggregateBodyBytes = 32 << 10

// AggregateHandler serves `POST /v1/events/aggregate`. It is the wire
// adapter over an *events.Aggregator: decode the request body, gate on
// scope claim if the filter is cross-tenant, dispatch, encode the
// response. The handler is a D-025-safe compiled artifact — bus and
// logger are set once at construction; ServeHTTP holds no per-request
// state.
type AggregateHandler struct {
	aggregator *events.Aggregator
	logger     *slog.Logger
}

// AggregateOption configures NewAggregateHandler at construction.
type AggregateOption func(*AggregateHandler)

// WithAggregateLogger sets the slog.Logger the handler logs decode /
// dispatch failures to. A nil logger (the default) routes to
// slog.Default().
func WithAggregateLogger(l *slog.Logger) AggregateOption {
	return func(h *AggregateHandler) {
		if l != nil {
			h.logger = l
		}
	}
}

// NewAggregateHandler builds the events-aggregate handler over an
// *events.Aggregator. aggregator is mandatory — a nil fails loud with
// ErrAggregateMisconfigured rather than building a handler that would
// nil-panic on the first request (CLAUDE.md §5).
//
// The returned *AggregateHandler is immutable after construction
// (D-025) and safe for concurrent use by N goroutines.
func NewAggregateHandler(aggregator *events.Aggregator, opts ...AggregateOption) (*AggregateHandler, error) {
	if aggregator == nil {
		return nil, fmt.Errorf("%w: events.Aggregator is nil", ErrAggregateMisconfigured)
	}
	h := &AggregateHandler{
		aggregator: aggregator,
		logger:     slog.Default(),
	}
	for _, opt := range opts {
		opt(h)
	}
	return h, nil
}

// ErrAggregateMisconfigured — NewAggregateHandler was called with a nil
// Aggregator.
var ErrAggregateMisconfigured = errors.New("stream: aggregate handler missing a mandatory dependency")

// ServeHTTP implements http.Handler. It resolves identity from r.Context()
// (auth.Middleware) or the X-Harbor-* carrier headers (Phase 60
// fallback), decodes the JSON body into an EventAggregateRequest, gates
// on cross-tenant scope, dispatches to the Aggregator, and encodes the
// response.
func (h *AggregateHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAggregateError(w, protoerrors.CodeInvalidRequest, http.StatusMethodNotAllowed,
			"events.aggregate accepts POST only")
		return
	}

	// Identity at the edge — RFC §5.5, CLAUDE.md §6 rule 9.
	id, err := resolveIdentity(r)
	if err != nil {
		writeAggregateError(w, protoerrors.CodeIdentityRequired, http.StatusUnauthorized,
			"identity scope incomplete: "+err.Error())
		return
	}

	// Decode the body — bounded.
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxAggregateBodyBytes))
	if err != nil {
		writeAggregateError(w, protoerrors.CodeInvalidRequest, http.StatusBadRequest,
			"failed to read request body: "+err.Error())
		return
	}
	var req prototypes.EventAggregateRequest
	if len(body) > 0 {
		dec := json.NewDecoder(bytesReader(body))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			writeAggregateError(w, protoerrors.CodeInvalidRequest, http.StatusBadRequest,
				"failed to decode request body: "+err.Error())
			return
		}
	}

	// Cross-tenant gate — the wire filter may name a tenant other than
	// the caller's, or multiple tenants. FilterFromWire flags this via
	// RequiresAdminScope; we consult auth.HasScope before dispatching.
	// A request that asks for cross-tenant fan-in without the scope
	// claim is rejected 403 with CodeIdentityScopeRequired — distinct
	// from CodeIdentityRequired (no identity at all → 401) and
	// CodeAuthRejected (token invalid → 401).
	conv := events.FilterFromWire(req.Filter, id.TenantID, id.UserID, id.SessionID)
	if conv.RequiresAdminScope {
		if !(auth.HasScope(r.Context(), auth.ScopeAdmin) || auth.HasScope(r.Context(), auth.ScopeConsoleFleet)) {
			writeAggregateError(w, protoerrors.CodeIdentityScopeRequired, http.StatusForbidden,
				"cross-tenant aggregate requires a verified `admin` or `console:fleet` scope")
			return
		}
	}

	// If the filter elided identity components AND the wire request did
	// not name any explicit identity, fold the caller's triple into the
	// filter so the aggregator's per-event MatchWire enforces it.
	effReq := req
	if len(effReq.Filter.TenantIDs) == 0 {
		effReq.Filter.TenantIDs = []string{id.TenantID}
	}
	if len(effReq.Filter.UserIDs) == 0 {
		effReq.Filter.UserIDs = []string{id.UserID}
	}
	if len(effReq.Filter.SessionIDs) == 0 {
		effReq.Filter.SessionIDs = []string{id.SessionID}
	}

	resp, err := h.aggregator.Aggregate(r.Context(), effReq)
	if err != nil {
		code, status, msg := classifyAggregateError(err)
		writeAggregateError(w, code, status, msg)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		h.logger.WarnContext(r.Context(), "events.aggregate: response encode failed",
			slog.String("error", err.Error()),
			slog.String("tenant_id", id.TenantID),
			slog.String("user_id", id.UserID),
			slog.String("session_id", id.SessionID))
	}
}

// classifyAggregateError maps an Aggregator error onto a canonical
// Protocol Code + HTTP status + safe operator-facing message. A
// bad-window error is CodeInvalidRequest (400); a replay-unavailable is
// CodeRuntimeError (500 — the runtime has no historical events to
// aggregate over); a ctx error is the catch-all CodeRuntimeError.
func classifyAggregateError(err error) (protoerrors.Code, int, string) {
	switch {
	case errors.Is(err, events.ErrAggregateBadWindow):
		return protoerrors.CodeInvalidRequest, http.StatusBadRequest,
			"aggregate window/bucket invalid: " + err.Error()
	case errors.Is(err, events.ErrReplayUnavailable):
		return protoerrors.CodeRuntimeError, http.StatusInternalServerError,
			"event store does not support historical aggregation"
	default:
		return protoerrors.CodeRuntimeError, http.StatusInternalServerError,
			"aggregate failed: " + err.Error()
	}
}

// writeAggregateError writes a JSON error body with the canonical
// Protocol Code + the supplied HTTP status. The body shape matches the
// REST control transport's error body so a client can decode both with
// the same Error wire type.
func writeAggregateError(w http.ResponseWriter, code protoerrors.Code, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(&protoerrors.Error{ //nolint:errcheck // response status already committed — a write error cannot be recovered here.
		Code:    code,
		Message: message,
	})
}

// bytesReader is a tiny adapter so json.NewDecoder can consume a []byte
// without pulling in bytes.NewReader at every call site. Keeps the
// import surface narrow.
func bytesReader(b []byte) *bytesIOReader { return &bytesIOReader{b: b} }

type bytesIOReader struct {
	b   []byte
	off int
}

func (r *bytesIOReader) Read(p []byte) (int, error) {
	if r.off >= len(r.b) {
		return 0, io.EOF
	}
	n := copy(p, r.b[r.off:])
	r.off += n
	return n, nil
}
