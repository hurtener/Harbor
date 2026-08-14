// Package stream — addition: the `observability.query` read handler.
// Like the Sessions-page handler, this is a one-shot request/response
// endpoint — POST JSON in, JSON out — sharing the same `resolveIdentity`
// + defence-in-depth body-identity machinery.
//
// Route shape:
//
//	POST /v1/observability/query — the ONE bounded administrative rollup
//	                             query.
//
// The method is identity-mandatory. The verified caller identity is read
// from the request context; the body NEVER supplies tenant / user /
// session identity for widening. An ordinary caller's query is forced to
// its own verified triple; a widened (admin OR console:fleet) fan-in is
// the SERVICE's audited ctx-scope decision (it emits exactly one
// `audit.admin_scope_used` event BEFORE the read reaches storage) — the
// handler does not re-implement the gate.
//
// ObservabilityHandler is a concurrency-safe compiled artifact — service
// / logger are set once at construction; ServeHTTP holds no per-request
// state.
package stream

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"github.com/hurtener/Harbor/internal/observability/protocol"
	"github.com/hurtener/Harbor/internal/observability/rollups"
	"github.com/hurtener/Harbor/internal/protocol/bodyscope"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
)

// ObservabilityRoutePattern is the http.ServeMux pattern the
// observability handler registers under.
const ObservabilityRoutePattern = "POST /v1/observability/query"

// maxObservabilityBodyBytes bounds an observability request body. The
// wire payload is small (a window + closed sets + filters + a page
// bound); 64 KiB is comfortably over the realistic ceiling and fails
// closed on a client that streams an unbounded body at the edge.
const maxObservabilityBodyBytes = 64 << 10

// ErrObservabilityMisconfigured — NewObservabilityHandler was called
// with a nil observability/protocol.Service.
var ErrObservabilityMisconfigured = errors.New("stream: observability handler missing a mandatory dependency")

// ObservabilityHandler serves `POST /v1/observability/query`. It is the
// wire adapter over a *protocol.Service: resolve identity, decode the
// request, dispatch, encode.
type ObservabilityHandler struct {
	service *protocol.Service
	logger  *slog.Logger
}

// ObservabilityOption configures NewObservabilityHandler at construction.
type ObservabilityOption func(*ObservabilityHandler)

// WithObservabilityLogger sets the slog.Logger the handler logs decode /
// dispatch failures to. A nil logger (the default) routes to
// slog.Default().
func WithObservabilityLogger(l *slog.Logger) ObservabilityOption {
	return func(h *ObservabilityHandler) {
		if l != nil {
			h.logger = l
		}
	}
}

// NewObservabilityHandler builds the observability handler over a
// *protocol.Service. service is mandatory — a nil fails loud with
// ErrObservabilityMisconfigured rather than building a handler that
// would nil-panic on the first request (CLAUDE.md §5).
//
// The returned *ObservabilityHandler is immutable after construction
// and safe for concurrent use by N goroutines.
func NewObservabilityHandler(service *protocol.Service, opts ...ObservabilityOption) (*ObservabilityHandler, error) {
	if service == nil {
		return nil, fmt.Errorf("%w: observability/protocol.Service is nil", ErrObservabilityMisconfigured)
	}
	h := &ObservabilityHandler{service: service, logger: slog.Default()}
	for _, opt := range opts {
		opt(h)
	}
	return h, nil
}

// ServeHTTP implements http.Handler. It resolves identity, decodes the
// body, dispatches to the service, and encodes the response.
func (h *ObservabilityHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeObservabilityError(w, protoerrors.CodeInvalidRequest, http.StatusMethodNotAllowed,
			"observability endpoint accepts POST only")
		return
	}
	id, r, err := resolveIdentity(r)
	if err != nil {
		writeObservabilityError(w, protoerrors.CodeIdentityRequired, http.StatusUnauthorized,
			"identity scope incomplete: "+err.Error())
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxObservabilityBodyBytes))
	if err != nil {
		writeObservabilityError(w, protoerrors.CodeInvalidRequest, http.StatusBadRequest,
			"failed to read request body: "+err.Error())
		return
	}
	var req prototypes.ObservabilityQueryRequest
	if len(body) > 0 {
		if err := decodeGovernanceBody(body, &req); err != nil {
			writeObservabilityError(w, protoerrors.CodeInvalidRequest, http.StatusBadRequest,
				"failed to decode observability.query request: "+err.Error())
			return
		}
	}
	if perr := reconcileBodyScope(r, &req.Identity, bodyscope.SurfaceObservability); perr != nil {
		writeObservabilityError(w, perr.Code, bodyScopeStatus(perr.Code), perr.Message)
		return
	}
	req.Identity = prototypes.IdentityScope{Tenant: id.TenantID, User: id.UserID, Session: id.SessionID}

	resp, err := h.service.Query(r.Context(), protocol.Request{
		From:        req.From,
		To:          req.To,
		Bucket:      rollups.BucketSize(req.Bucket),
		GroupBy:     observabilityDimensions(req.GroupBy),
		Filters:     observabilityFilters(req.Filters),
		Measures:    observabilityMeasures(req.Measures),
		Sort:        rollups.SortKey(req.Sort),
		SortMeasure: rollups.Measure(req.SortMeasure),
		Limit:       req.Limit,
		Cursor:      req.Cursor,
	})
	if err != nil {
		h.writeServiceError(w, r, methods.MethodObservabilityQuery, err)
		return
	}
	writeObservabilityJSON(w, r, projectObservabilityQuery(resp), h.logger)
}

// observabilityDimensions maps wire dimension strings onto the closed
// rollups dimension type.
func observabilityDimensions(in []string) []rollups.Dimension {
	if len(in) == 0 {
		return nil
	}
	out := make([]rollups.Dimension, 0, len(in))
	for _, d := range in {
		out = append(out, rollups.Dimension(d))
	}
	return out
}

// observabilityMeasures maps wire measure strings onto the closed
// rollups measure type.
func observabilityMeasures(in []string) []rollups.Measure {
	if len(in) == 0 {
		return nil
	}
	out := make([]rollups.Measure, 0, len(in))
	for _, m := range in {
		out = append(out, rollups.Measure(m))
	}
	return out
}

// observabilityFilters maps the wire filter onto the rollups filter.
func observabilityFilters(in prototypes.ObservabilityQueryFilter) protocol.Filters {
	return protocol.Filters{
		TenantIDs:  append([]string(nil), in.TenantIDs...),
		UserIDs:    append([]string(nil), in.UserIDs...),
		SessionIDs: append([]string(nil), in.SessionIDs...),
		Models:     append([]string(nil), in.Models...),
	}
}

// writeServiceError maps a service error onto a canonical Protocol Code +
// HTTP status + safe operator-facing message.
func (h *ObservabilityHandler) writeServiceError(w http.ResponseWriter, r *http.Request, method methods.Method, err error) {
	code, status, msg := classifyObservabilityError(err)
	if status >= http.StatusInternalServerError {
		h.logger.ErrorContext(r.Context(), "observability handler: dispatch failed",
			slog.String("method", string(method)), slog.String("error", err.Error()))
	}
	writeObservabilityError(w, code, status, msg)
}

// classifyObservabilityError maps an observability service error onto a
// canonical Protocol Code + HTTP status — the single place the
// observability wire surface translates a Go error into a Protocol
// error. The closed-set refusals stay 400; the budget and cursor
// refusals keep their distinct machine-branchable codes.
func classifyObservabilityError(err error) (protoerrors.Code, int, string) {
	switch {
	case errors.Is(err, protocol.ErrIdentityRequired):
		return protoerrors.CodeIdentityRequired, http.StatusUnauthorized,
			"identity scope incomplete"
	case errors.Is(err, protocol.ErrCrossTenantScope),
		errors.Is(err, protocol.ErrCrossUserScope),
		errors.Is(err, protocol.ErrCrossSessionScope):
		return protoerrors.CodeIdentityScopeRequired, http.StatusForbidden,
			"observability query widening requires the verified admin or console:fleet scope"
	case errors.Is(err, protocol.ErrInvalidRequest):
		return protoerrors.CodeInvalidRequest, http.StatusBadRequest,
			"observability query invalid: " + err.Error()
	case errors.Is(err, protocol.ErrBudgetExceeded):
		return protoerrors.CodeQueryBudgetExceeded, http.StatusBadRequest,
			"observability query exceeds a result budget: " + err.Error()
	case errors.Is(err, protocol.ErrBadCursor):
		return protoerrors.CodeInvalidCursor, http.StatusBadRequest,
			"observability query cursor is invalid or foreign: " + err.Error()
	case errors.Is(err, protocol.ErrAuditFailed):
		return protoerrors.CodeRuntimeError, http.StatusInternalServerError,
			"observability widened-read audit emit failed"
	case errors.Is(err, protocol.ErrQueryFailed),
		errors.Is(err, protocol.ErrQualityFailed),
		errors.Is(err, protocol.ErrMisconfigured):
		return protoerrors.CodeRuntimeError, http.StatusInternalServerError,
			"observability query failed: " + err.Error()
	default:
		return protoerrors.CodeRuntimeError, http.StatusInternalServerError,
			"observability query failed: " + err.Error()
	}
}

// projectObservabilityQuery maps the service's response onto the wire:
// the deterministic page of exact-value rows, the full-query-bound
// cursor, and the MANDATORY freshness block.
func projectObservabilityQuery(resp protocol.Response) prototypes.ObservabilityQueryResponse {
	rows := make([]prototypes.ObservabilityQueryRow, 0, len(resp.Rows))
	for _, r := range resp.Rows {
		dimensions := make(map[string]string, len(r.Dimensions))
		for d, v := range r.Dimensions {
			dimensions[string(d)] = v
		}
		measures := make(map[string]prototypes.ObservabilityMeasureValue, len(r.Measures))
		for m, v := range r.Measures {
			measures[string(m)] = prototypes.ObservabilityMeasureValue{N: v.N, Scale: v.Scale}
		}
		rows = append(rows, prototypes.ObservabilityQueryRow{
			BucketStart: r.BucketStart,
			Dimensions:  dimensions,
			Measures:    measures,
		})
	}
	return prototypes.ObservabilityQueryResponse{
		Rows:            rows,
		NextCursor:      resp.NextCursor,
		Quality:         projectObservabilityQuality(resp.Quality),
		ProtocolVersion: prototypes.ProtocolVersion,
	}
}

// projectObservabilityQuality maps the mandatory freshness block. The
// coverage / state strings ride verbatim; the quality error is never
// serialized (the wire block is the honest closed status, never the
// internal failure detail).
func projectObservabilityQuality(q protocol.QualityBlock) prototypes.ObservabilityQualityBlock {
	return prototypes.ObservabilityQualityBlock{
		State:          string(q.State),
		Watermark:      q.Watermark,
		WatermarkAt:    q.WatermarkAt,
		RetentionStart: q.RetentionStart,
		RetentionEnd:   q.RetentionEnd,
		Coverage:       string(q.Coverage),
	}
}

// writeObservabilityJSON encodes a successful response.
func writeObservabilityJSON(w http.ResponseWriter, r *http.Request, v any, logger *slog.Logger) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		logger.WarnContext(r.Context(), "observability handler: response encode failed",
			slog.String("error", err.Error()))
	}
}

// writeObservabilityError writes a JSON error body with the canonical
// Protocol Code + the supplied HTTP status.
func writeObservabilityError(w http.ResponseWriter, code protoerrors.Code, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(&protoerrors.Error{Code: code, Message: message}) //nolint:errcheck // response status already committed — a write error cannot be recovered here.
}
