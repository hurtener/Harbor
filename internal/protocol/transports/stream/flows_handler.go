// Package stream — Wave 13 additions (Phase 73i / D-117): the Console
// Flows-page HTTP handler. Like `events.aggregate` and `pause.list`,
// these are one-shot request/response endpoints — POST JSON in, JSON
// out. They live in the stream package because their identity +
// cross-tenant scope-claim gating is the same one Subscribe / Aggregate
// / PauseList use, and they share the route prefix family the Console
// surface registers.
//
// Route shapes (all POST):
//
//	POST /v1/flows/list            — paginated flow catalog
//	POST /v1/flows/describe        — a flow's engine-graph description
//	POST /v1/flows/runs/list       — a flow's paginated run history
//	POST /v1/flows/runs/describe   — a single run's per-node timeline
//	POST /v1/flows/run             — invoke a one-shot run (mutating)
//	POST /v1/flows/metrics         — a flow's sparkline metrics
//
// Five routes are read-only; `flows/run` mutates and is gated on the
// verified `auth.ScopeAdmin` claim (D-079 closed two-scope set — NO new
// scope is minted). Every route is identity-mandatory.
//
// The handler emits a per-page audit event on every dispatch:
// `flows.page_viewed` for the five reads, `flows.run_invoked` for the
// mutating run. The events flow through the canonical EventBus so the
// Console activity feed and the audit log see Flows-page traffic.
//
// FlowsHandler is a D-025-safe compiled artifact — surface / bus /
// logger are set once at construction; ServeHTTP holds no per-request
// state.
package stream

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	flowprotocol "github.com/hurtener/Harbor/internal/runtime/flow/protocol"
)

// FlowsRoutePattern is the http.ServeMux prefix pattern the Flows-page
// handler registers under. The handler internally branches on the
// trailing path segment to dispatch the six methods. Exported so
// internal/protocol/transports can mount the handler under the same
// prefix it documents.
const FlowsRoutePattern = "POST /v1/flows/"

// maxFlowsBodyBytes bounds a Flows-page request body. The wire payloads
// are small (filters + pagination ints + a flat input form); 64 KiB is
// comfortably over the realistic ceiling and fails closed on a client
// that streams an unbounded body at the edge.
const maxFlowsBodyBytes = 64 << 10

// Flows-page audit event types. They flow through the canonical
// EventBus so the Console activity feed + audit log see Flows-page
// traffic. Registered from this package's init().
const (
	// EventTypeFlowsPageViewed — emitted on every read dispatch
	// (flows.list / describe / runs.list / runs.describe / metrics).
	EventTypeFlowsPageViewed events.EventType = "flows.page_viewed"
	// EventTypeFlowsRunInvoked — emitted when a `flows.run` dispatch is
	// accepted. The mutating-action audit signal.
	EventTypeFlowsRunInvoked events.EventType = "flows.run_invoked"
)

func init() {
	events.RegisterEventType(EventTypeFlowsPageViewed)
	events.RegisterEventType(EventTypeFlowsRunInvoked)
}

// FlowsPageViewedPayload is the typed payload for
// EventTypeFlowsPageViewed. It records which read method ran and the
// flow it targeted (empty for `flows.list`). It is a SafePayload — it
// carries no raw run output / no tool arguments (CLAUDE.md §7 rule 7).
type FlowsPageViewedPayload struct {
	events.SafeSealed
	// Method is the canonical Protocol method name that ran.
	Method string
	// FlowID is the targeted flow id, when the method targets one.
	FlowID string
	// AdminScoped reports whether the read used the admin scope claim.
	AdminScoped bool
}

// FlowsRunInvokedPayload is the typed payload for
// EventTypeFlowsRunInvoked. It records the invoked flow + the accepted
// run id. SafePayload — no input form, no output.
type FlowsRunInvokedPayload struct {
	events.SafeSealed
	// FlowID is the invoked flow.
	FlowID string
	// RunID is the identifier of the accepted run.
	RunID string
}

// ErrFlowsMisconfigured — NewFlowsHandler was called with a nil
// mandatory dependency.
var ErrFlowsMisconfigured = errors.New("stream: flows handler missing a mandatory dependency")

// FlowsHandler serves the six `POST /v1/flows/*` routes. It is the wire
// adapter over a *flowprotocol.Surface: decode the request, dispatch to
// the surface, emit the per-page audit event, encode the response.
type FlowsHandler struct {
	surface *flowprotocol.Surface
	bus     events.EventBus // optional — nil ⇒ no audit event emitted
	logger  *slog.Logger
}

// FlowsOption configures NewFlowsHandler at construction.
type FlowsOption func(*FlowsHandler)

// WithFlowsLogger sets the slog.Logger the handler logs decode /
// dispatch failures to. A nil logger (the default) routes to
// slog.Default().
func WithFlowsLogger(l *slog.Logger) FlowsOption {
	return func(h *FlowsHandler) {
		if l != nil {
			h.logger = l
		}
	}
}

// WithFlowsBus wires the canonical events.EventBus into the handler so
// every dispatch emits its per-page audit event. The bus is OPTIONAL —
// when not supplied the handler still serves every route, but the audit
// events are not emitted (the handler logs the dispatch at Info instead
// so Flows-page traffic is never fully silent).
func WithFlowsBus(b events.EventBus) FlowsOption {
	return func(h *FlowsHandler) {
		if b != nil {
			h.bus = b
		}
	}
}

// NewFlowsHandler builds the Flows-page handler over a
// *flowprotocol.Surface. surface is mandatory — a nil fails loud with
// ErrFlowsMisconfigured rather than building a handler that would
// nil-panic on the first request (CLAUDE.md §5).
//
// The returned *FlowsHandler is immutable after construction (D-025)
// and safe for concurrent use by N goroutines.
func NewFlowsHandler(surface *flowprotocol.Surface, opts ...FlowsOption) (*FlowsHandler, error) {
	if surface == nil {
		return nil, fmt.Errorf("%w: flowprotocol.Surface is nil", ErrFlowsMisconfigured)
	}
	h := &FlowsHandler{surface: surface, logger: slog.Default()}
	for _, opt := range opts {
		opt(h)
	}
	return h, nil
}

// ServeHTTP implements http.Handler. It resolves identity, branches on
// the trailing path segment, decodes the body, dispatches to the
// surface, emits the audit event, and encodes the response.
func (h *FlowsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeFlowsError(w, protoerrors.CodeInvalidRequest, http.StatusMethodNotAllowed,
			"flows endpoints accept POST only")
		return
	}
	id, err := resolveIdentity(r)
	if err != nil {
		writeFlowsError(w, protoerrors.CodeIdentityRequired, http.StatusUnauthorized,
			"identity scope incomplete: "+err.Error())
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxFlowsBodyBytes))
	if err != nil {
		writeFlowsError(w, protoerrors.CodeInvalidRequest, http.StatusBadRequest,
			"failed to read request body: "+err.Error())
		return
	}
	adminScoped := auth.HasScope(r.Context(), auth.ScopeAdmin)
	wireID := prototypes.IdentityScope{Tenant: id.TenantID, User: id.UserID, Session: id.SessionID}

	switch flowsPathSuffix(r.URL.Path) {
	case "list":
		h.serveList(w, r, body, wireID, adminScoped)
	case "describe":
		h.serveDescribe(w, r, body, wireID, adminScoped)
	case "runs/list":
		h.serveRunsList(w, r, body, wireID, adminScoped)
	case "runs/describe":
		h.serveRunsDescribe(w, r, body, wireID, adminScoped)
	case "run":
		h.serveRun(w, r, body, wireID, adminScoped)
	case "metrics":
		h.serveMetrics(w, r, body, wireID, adminScoped)
	default:
		writeFlowsError(w, protoerrors.CodeUnknownMethod, http.StatusNotFound,
			"unknown flows method route")
	}
}

// flowsPathSuffix extracts the method suffix after the `/v1/flows/`
// prefix — e.g. "list", "runs/describe".
func flowsPathSuffix(path string) string {
	return strings.TrimPrefix(path, "/v1/flows/")
}

func (h *FlowsHandler) serveList(w http.ResponseWriter, r *http.Request, body []byte, wireID prototypes.IdentityScope, adminScoped bool) {
	var req prototypes.FlowListRequest
	if perr := decodeFlowsBody(body, &req); perr != nil {
		writeFlowsError(w, perr.code, perr.status, perr.message)
		return
	}
	req.Identity = mergeIdentity(req.Identity, wireID)
	if perr := assertFlowsIdentity(req.Identity, wireID); perr != nil {
		writeFlowsError(w, perr.code, perr.status, perr.message)
		return
	}
	resp, err := h.surface.List(r.Context(), req, adminScoped)
	if err != nil {
		h.writeSurfaceError(w, r, err)
		return
	}
	h.emitPageViewed(r, methods.MethodFlowsList, "", adminScoped)
	writeFlowsJSON(w, r, resp, h.logger)
}

func (h *FlowsHandler) serveDescribe(w http.ResponseWriter, r *http.Request, body []byte, wireID prototypes.IdentityScope, adminScoped bool) {
	var req prototypes.FlowDescribeRequest
	if perr := decodeFlowsBody(body, &req); perr != nil {
		writeFlowsError(w, perr.code, perr.status, perr.message)
		return
	}
	req.Identity = mergeIdentity(req.Identity, wireID)
	if perr := assertFlowsIdentity(req.Identity, wireID); perr != nil {
		writeFlowsError(w, perr.code, perr.status, perr.message)
		return
	}
	resp, err := h.surface.Describe(r.Context(), req, adminScoped)
	if err != nil {
		h.writeSurfaceError(w, r, err)
		return
	}
	h.emitPageViewed(r, methods.MethodFlowsDescribe, req.ID, adminScoped)
	writeFlowsJSON(w, r, resp, h.logger)
}

func (h *FlowsHandler) serveRunsList(w http.ResponseWriter, r *http.Request, body []byte, wireID prototypes.IdentityScope, adminScoped bool) {
	var req prototypes.FlowRunsListRequest
	if perr := decodeFlowsBody(body, &req); perr != nil {
		writeFlowsError(w, perr.code, perr.status, perr.message)
		return
	}
	req.Identity = mergeIdentity(req.Identity, wireID)
	if perr := assertFlowsIdentity(req.Identity, wireID); perr != nil {
		writeFlowsError(w, perr.code, perr.status, perr.message)
		return
	}
	resp, err := h.surface.RunsList(r.Context(), req, adminScoped)
	if err != nil {
		h.writeSurfaceError(w, r, err)
		return
	}
	h.emitPageViewed(r, methods.MethodFlowsRunsList, req.FlowID, adminScoped)
	writeFlowsJSON(w, r, resp, h.logger)
}

func (h *FlowsHandler) serveRunsDescribe(w http.ResponseWriter, r *http.Request, body []byte, wireID prototypes.IdentityScope, adminScoped bool) {
	var req prototypes.FlowRunDescribeRequest
	if perr := decodeFlowsBody(body, &req); perr != nil {
		writeFlowsError(w, perr.code, perr.status, perr.message)
		return
	}
	req.Identity = mergeIdentity(req.Identity, wireID)
	if perr := assertFlowsIdentity(req.Identity, wireID); perr != nil {
		writeFlowsError(w, perr.code, perr.status, perr.message)
		return
	}
	resp, err := h.surface.RunsDescribe(r.Context(), req, adminScoped)
	if err != nil {
		h.writeSurfaceError(w, r, err)
		return
	}
	h.emitPageViewed(r, methods.MethodFlowsRunsDescribe, resp.Run.FlowID, adminScoped)
	writeFlowsJSON(w, r, resp, h.logger)
}

func (h *FlowsHandler) serveRun(w http.ResponseWriter, r *http.Request, body []byte, wireID prototypes.IdentityScope, adminScoped bool) {
	var req prototypes.FlowRunRequest
	if perr := decodeFlowsBody(body, &req); perr != nil {
		writeFlowsError(w, perr.code, perr.status, perr.message)
		return
	}
	req.Identity = mergeIdentity(req.Identity, wireID)
	if perr := assertFlowsIdentity(req.Identity, wireID); perr != nil {
		writeFlowsError(w, perr.code, perr.status, perr.message)
		return
	}
	resp, err := h.surface.Run(r.Context(), req, adminScoped)
	if err != nil {
		h.writeSurfaceError(w, r, err)
		return
	}
	h.emitRunInvoked(r, req.FlowID, resp.RunID)
	writeFlowsJSON(w, r, resp, h.logger)
}

func (h *FlowsHandler) serveMetrics(w http.ResponseWriter, r *http.Request, body []byte, wireID prototypes.IdentityScope, adminScoped bool) {
	var req prototypes.FlowMetricsRequest
	if perr := decodeFlowsBody(body, &req); perr != nil {
		writeFlowsError(w, perr.code, perr.status, perr.message)
		return
	}
	req.Identity = mergeIdentity(req.Identity, wireID)
	if perr := assertFlowsIdentity(req.Identity, wireID); perr != nil {
		writeFlowsError(w, perr.code, perr.status, perr.message)
		return
	}
	resp, err := h.surface.Metrics(r.Context(), req, adminScoped)
	if err != nil {
		h.writeSurfaceError(w, r, err)
		return
	}
	h.emitPageViewed(r, methods.MethodFlowsMetrics, req.FlowID, adminScoped)
	writeFlowsJSON(w, r, resp, h.logger)
}

// emitPageViewed publishes the `flows.page_viewed` audit observation
// for a read dispatch. When no bus is wired it logs at Info so Flows-
// page traffic is never fully silent.
func (h *FlowsHandler) emitPageViewed(r *http.Request, method methods.Method, flowID string, adminScoped bool) {
	id, _ := resolveIdentity(r) //nolint:errcheck // best-effort audit-observation emit, post-authorization — a re-derivation miss degrades to a zero-value identity tag, never blocks the request.
	h.logger.InfoContext(r.Context(), "flows: page viewed",
		slog.String("method", string(method)),
		slog.String("flow_id", flowID),
		slog.Bool("admin_scoped", adminScoped),
		slog.String("tenant_id", id.TenantID))
	if h.bus == nil {
		return
	}
	ev := events.Event{
		Type:     EventTypeFlowsPageViewed,
		Identity: identity.Quadruple{Identity: id},
		Payload: FlowsPageViewedPayload{
			Method:      string(method),
			FlowID:      flowID,
			AdminScoped: adminScoped,
		},
	}
	if err := h.bus.Publish(r.Context(), ev); err != nil {
		h.logger.WarnContext(r.Context(), "flows: page_viewed emit failed",
			slog.String("error", err.Error()))
	}
}

// emitRunInvoked publishes the `flows.run_invoked` audit observation
// for an accepted `flows.run` dispatch.
func (h *FlowsHandler) emitRunInvoked(r *http.Request, flowID, runID string) {
	id, _ := resolveIdentity(r) //nolint:errcheck // best-effort audit-observation emit, post-authorization — a re-derivation miss degrades to a zero-value identity tag, never blocks the request.
	h.logger.InfoContext(r.Context(), "flows: run invoked",
		slog.String("flow_id", flowID),
		slog.String("run_id", runID),
		slog.String("tenant_id", id.TenantID))
	if h.bus == nil {
		return
	}
	ev := events.Event{
		Type:     EventTypeFlowsRunInvoked,
		Identity: identity.Quadruple{Identity: id},
		Payload:  FlowsRunInvokedPayload{FlowID: flowID, RunID: runID},
	}
	if err := h.bus.Publish(r.Context(), ev); err != nil {
		h.logger.WarnContext(r.Context(), "flows: run_invoked emit failed",
			slog.String("error", err.Error()))
	}
}

// writeSurfaceError maps a flowprotocol.Surface error onto a canonical
// Protocol Code + HTTP status + safe operator-facing message.
func (h *FlowsHandler) writeSurfaceError(w http.ResponseWriter, r *http.Request, err error) {
	code, status, msg := classifyFlowsError(err)
	if status >= http.StatusInternalServerError {
		h.logger.ErrorContext(r.Context(), "flows: dispatch failed",
			slog.String("error", err.Error()))
	}
	writeFlowsError(w, code, status, msg)
}

// flowsError is the internal carrier for a classified Flows-page
// decode failure.
type flowsError struct {
	code    protoerrors.Code
	status  int
	message string
}

// decodeFlowsBody decodes a JSON body into the supplied request struct.
// An empty body is permitted (the struct keeps its zero value); a
// malformed body is a CodeInvalidRequest.
func decodeFlowsBody(body []byte, v any) *flowsError {
	if len(body) == 0 {
		return nil
	}
	dec := json.NewDecoder(bytesReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return &flowsError{
			code: protoerrors.CodeInvalidRequest, status: http.StatusBadRequest,
			message: "failed to decode request body: " + err.Error(),
		}
	}
	return nil
}

// mergeIdentity backfills an empty body identity scope from the
// verified identity. A populated body scope is left intact (the
// assertFlowsIdentity check then enforces it matches).
func mergeIdentity(body, verified prototypes.IdentityScope) prototypes.IdentityScope {
	if body.Tenant == "" && body.User == "" && body.Session == "" {
		return verified
	}
	return body
}

// assertFlowsIdentity is the defence-in-depth check: when the request
// body carries an identity, every non-empty component MUST match the
// verified identity — a caller cannot present a valid JWT for tenant T1
// while submitting a body claiming tenant T2.
func assertFlowsIdentity(body, verified prototypes.IdentityScope) *flowsError {
	if (body.Tenant != "" && body.Tenant != verified.Tenant) ||
		(body.User != "" && body.User != verified.User) ||
		(body.Session != "" && body.Session != verified.Session) {
		return &flowsError{
			code: protoerrors.CodeIdentityRequired, status: http.StatusUnauthorized,
			message: "body identity scope does not match the verified identity",
		}
	}
	return nil
}

// classifyFlowsError maps a Surface error onto a canonical Protocol
// Code + HTTP status + safe operator-facing message.
func classifyFlowsError(err error) (protoerrors.Code, int, string) {
	switch {
	case errors.Is(err, flowprotocol.ErrIdentityRequired):
		return protoerrors.CodeIdentityRequired, http.StatusUnauthorized,
			"flows: identity scope incomplete"
	case errors.Is(err, flowprotocol.ErrInvalidRequest):
		return protoerrors.CodeInvalidRequest, http.StatusBadRequest,
			"flows: " + err.Error()
	case errors.Is(err, flowprotocol.ErrNotFound):
		return protoerrors.CodeNotFound, http.StatusNotFound,
			"flows: " + err.Error()
	case errors.Is(err, flowprotocol.ErrCrossTenantScope):
		return protoerrors.CodeIdentityScopeRequired, http.StatusForbidden,
			"flows: cross-tenant filter requires the `admin` scope claim"
	case errors.Is(err, flowprotocol.ErrRunScopeRequired):
		return protoerrors.CodeScopeMismatch, http.StatusForbidden,
			"flows.run: requires the `admin` scope claim"
	default:
		return protoerrors.CodeRuntimeError, http.StatusInternalServerError,
			"flows: dispatch failed"
	}
}

// writeFlowsJSON encodes a successful Flows-page response.
func writeFlowsJSON(w http.ResponseWriter, r *http.Request, v any, logger *slog.Logger) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		logger.WarnContext(r.Context(), "flows: response encode failed",
			slog.String("error", err.Error()))
	}
}

// writeFlowsError writes a JSON error body with the canonical Protocol
// Code + the supplied HTTP status.
func writeFlowsError(w http.ResponseWriter, code protoerrors.Code, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(&protoerrors.Error{Code: code, Message: message}) //nolint:errcheck // response status already committed — a write error cannot be recovered here.
}
