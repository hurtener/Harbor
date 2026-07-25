// Package stream — additions: the three
// `memory.*` read handlers. Like `events.aggregate` and `pause.list`,
// these are one-shot request/response — POST JSON in, JSON out — and
// they live in the stream package because their identity + cross-tenant
// scope-claim gating is the same one Subscribe / Aggregate / PauseList
// use, and they share the route prefix family the Console subscription
// surface registers.
//
// Route shapes:
//
//	POST /v1/memory/list
//	POST /v1/memory/get
//	POST /v1/memory/health
//
// The handler reads identity from r.Context() (auth.Middleware) or the
// X-Harbor-* carrier headers (fallback), decodes the JSON body,
// gates cross-tenant filters on auth.HasScope(ScopeAdmin) OR
// auth.HasScope(ScopeConsoleFleet) — the admin closed two-scope set, NO
// new memory scope (audit B1) — projects the answer from the memory
// subsystem, and encodes the response. The response body is the wire
// types.Memory* JSON; on failure, a JSON error body with the canonical
// Protocol Code.
//
// All three methods are READ-ONLY. The memory mutation surface
// (`memory.put` / `memory.delete`) is deferred to a later phase / post-V1
// (page-memory.md §10); this handler ships no mutation path.
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
	"github.com/hurtener/Harbor/internal/memory"
	memprotocol "github.com/hurtener/Harbor/internal/memory/protocol"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	"github.com/hurtener/Harbor/internal/protocol/bodyscope"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
)

// Memory route patterns the handler registers under. Exported so
// internal/protocol/transports can mount each route under the same
// pattern it documents.
const (
	MemoryListRoutePattern          = "POST /v1/memory/list"
	MemoryGetRoutePattern           = "POST /v1/memory/get"
	MemoryHealthRoutePattern        = "POST /v1/memory/health"
	MemoryStrategyTraceRoutePattern = "POST /v1/memory/strategy_trace"
	MemoryPutRoutePattern           = "POST /v1/memory/put"
	MemoryDeleteRoutePattern        = "POST /v1/memory/delete"
)

// maxMemoryBodyBytes bounds a memory request body. The wire payload is
// small (a filter + pagination ints, or a key); 64 KiB is comfortably
// over the realistic ceiling and fails closed on a client that streams
// an unbounded body at the edge.
const maxMemoryBodyBytes = 64 << 10

// ErrMemoryHandlerMisconfigured — NewMemoryHandler was called with a
// nil mandatory dependency (MemoryStore, ArtifactStore, or the
// heavy-content threshold was non-positive).
var ErrMemoryHandlerMisconfigured = errors.New("stream: memory handler missing a mandatory dependency")

// MemoryHandler serves the three `memory.*` read routes. It is the wire
// adapter over the memory subsystem: decode the request, gate on the
// scope claim if the filter is cross-tenant, project the answer
// via internal/memory/protocol, encode the response. The handler is a
// safe for concurrent reuse compiled artifact — every field is set once at
// construction; ServeHTTP holds no per-request state.
type MemoryHandler struct {
	store      memory.MemoryStore
	artifacts  artifacts.ArtifactStore
	aggregator *events.Aggregator // optional — nil ⇒ 24h counters report 0
	bus        events.EventBus    // optional — nil ⇒ mutation audit events not emitted
	logger     *slog.Logger
	threshold  int
	driverName string
}

// MemoryOption configures NewMemoryHandler at construction.
type MemoryOption func(*MemoryHandler)

// WithMemoryLogger sets the slog.Logger the handler logs decode /
// projection failures to. A nil logger (the default) routes to
// slog.Default().
func WithMemoryLogger(l *slog.Logger) MemoryOption {
	return func(h *MemoryHandler) {
		if l != nil {
			h.logger = l
		}
	}
}

// WithMemoryAggregator wires the events Aggregator into the handler so
// the `memory.list` / `memory.health` 24-hour counters
// (identity-rejected / recovery-dropped) can be computed. OPTIONAL —
// when not supplied, those two counters report 0; the page still
// renders and the right-rail cards subscribe to the live event stream
// for the real-time view. A nil aggregator is treated as
// "WithMemoryAggregator not supplied".
func WithMemoryAggregator(a *events.Aggregator) MemoryOption {
	return func(h *MemoryHandler) {
		if a != nil {
			h.aggregator = a
		}
	}
}

// WithMemoryBus wires the events bus the admin mutation methods
// (`memory.put` / `memory.delete`) publish their audit events on
// (`memory.item_put` / `memory.item_deleted`). OPTIONAL — when not
// supplied, the mutations still apply but emit no audit event; the
// production wiring (`harbor dev`) always supplies it so the mutation is
// audited (page-memory.md §9). A nil bus is treated as "not supplied".
func WithMemoryBus(b events.EventBus) MemoryOption {
	return func(h *MemoryHandler) {
		if b != nil {
			h.bus = b
		}
	}
}

// WithMemoryDriverName sets the configured memory-driver name surfaced
// on each `memory.list` row's Driver field and in the `memory.health`
// per-scope driver mapping. Empty (the default) reports `inmem`.
func WithMemoryDriverName(name string) MemoryOption {
	return func(h *MemoryHandler) {
		if name != "" {
			h.driverName = name
		}
	}
}

// NewMemoryHandler builds the memory handler over a memory.MemoryStore
// + an artifacts.ArtifactStore. store and artStore are mandatory — a
// nil fails loud with ErrMemoryHandlerMisconfigured rather than
// building a handler that would nil-panic on the first request
// (CLAUDE.md §5). threshold is the configured heavy-content byte size
// (cfg.Artifacts.HeavyOutputThresholdBytes); a non-positive value fails
// loud (a zero threshold would route every value).
//
// The returned *MemoryHandler is immutable after construction
// and safe for concurrent use by N goroutines.
func NewMemoryHandler(store memory.MemoryStore, artStore artifacts.ArtifactStore, threshold int, opts ...MemoryOption) (*MemoryHandler, error) {
	if store == nil {
		return nil, fmt.Errorf("%w: memory.MemoryStore is nil", ErrMemoryHandlerMisconfigured)
	}
	if artStore == nil {
		return nil, fmt.Errorf("%w: artifacts.ArtifactStore is nil", ErrMemoryHandlerMisconfigured)
	}
	if threshold <= 0 {
		return nil, fmt.Errorf("%w: heavy-content threshold %d is non-positive", ErrMemoryHandlerMisconfigured, threshold)
	}
	h := &MemoryHandler{
		store:      store,
		artifacts:  artStore,
		logger:     slog.Default(),
		threshold:  threshold,
		driverName: string(prototypes.MemoryDriverInmem),
	}
	for _, opt := range opts {
		opt(h)
	}
	return h, nil
}

// ListHandler returns the http.Handler for `POST /v1/memory/list`.
func (h *MemoryHandler) ListHandler() http.Handler {
	return http.HandlerFunc(h.serveList)
}

// GetHandler returns the http.Handler for `POST /v1/memory/get`.
func (h *MemoryHandler) GetHandler() http.Handler {
	return http.HandlerFunc(h.serveGet)
}

// HealthHandler returns the http.Handler for `POST /v1/memory/health`.
func (h *MemoryHandler) HealthHandler() http.Handler {
	return http.HandlerFunc(h.serveHealth)
}

// StrategyTraceHandler returns the http.Handler for
// `POST /v1/memory/strategy_trace`.
func (h *MemoryHandler) StrategyTraceHandler() http.Handler {
	return http.HandlerFunc(h.serveStrategyTrace)
}

// PutHandler returns the http.Handler for `POST /v1/memory/put`.
func (h *MemoryHandler) PutHandler() http.Handler {
	return http.HandlerFunc(h.servePut)
}

// DeleteHandler returns the http.Handler for `POST /v1/memory/delete`.
func (h *MemoryHandler) DeleteHandler() http.Handler {
	return http.HandlerFunc(h.serveDelete)
}

// serveStrategyTrace answers `POST /v1/memory/strategy_trace` — a read
// method (same read-scope contract as memory.list; no admin claim).
func (h *MemoryHandler) serveStrategyTrace(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeMemoryError(w, protoerrors.CodeInvalidRequest, http.StatusMethodNotAllowed,
			"memory.strategy_trace accepts POST only")
		return
	}
	id, r, perr := h.resolveAndDecode(w, r)
	if perr != nil {
		writeMemoryError(w, perr.code, perr.status, perr.message)
		return
	}
	var req prototypes.MemoryStrategyTraceRequest
	if perr := decodeMemoryBody(w, r, &req); perr != nil {
		writeMemoryError(w, perr.code, perr.status, perr.message)
		return
	}
	if perr := reconcileMemoryIdentity(r, &req.Identity); perr != nil {
		writeMemoryError(w, perr.code, perr.status, perr.message)
		return
	}
	resp, err := memprotocol.StrategyTrace(r.Context(), memprotocol.StrategyTraceDeps{Store: h.store},
		identity.Quadruple{Identity: id})
	if err != nil {
		code, status, msg := classifyMemoryError(err)
		writeMemoryError(w, code, status, msg)
		return
	}
	h.encode(r.Context(), w, id, &resp)
}

// servePut answers `POST /v1/memory/put` — the admin-gated, audited
// "add a memory turn" mutation.
func (h *MemoryHandler) servePut(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeMemoryError(w, protoerrors.CodeInvalidRequest, http.StatusMethodNotAllowed,
			"memory.put accepts POST only")
		return
	}
	id, r, perr := h.resolveAndDecode(w, r)
	if perr != nil {
		writeMemoryError(w, perr.code, perr.status, perr.message)
		return
	}
	var req prototypes.MemoryPutRequest
	if perr := decodeMemoryBody(w, r, &req); perr != nil {
		writeMemoryError(w, perr.code, perr.status, perr.message)
		return
	}
	if perr := reconcileMemoryIdentity(r, &req.Identity); perr != nil {
		writeMemoryError(w, perr.code, perr.status, perr.message)
		return
	}
	if perr := assertMemoryAdmin(r.Context()); perr != nil {
		writeMemoryError(w, perr.code, perr.status, perr.message)
		return
	}
	resp, err := memprotocol.Put(r.Context(), memprotocol.PutDeps{Store: h.store, Bus: h.bus},
		req, identity.Quadruple{Identity: id})
	if err != nil {
		code, status, msg := classifyMemoryError(err)
		writeMemoryError(w, code, status, msg)
		return
	}
	h.encode(r.Context(), w, id, &resp)
}

// serveDelete answers `POST /v1/memory/delete` — the admin-gated, audited
// "evict a memory turn" mutation.
func (h *MemoryHandler) serveDelete(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeMemoryError(w, protoerrors.CodeInvalidRequest, http.StatusMethodNotAllowed,
			"memory.delete accepts POST only")
		return
	}
	id, r, perr := h.resolveAndDecode(w, r)
	if perr != nil {
		writeMemoryError(w, perr.code, perr.status, perr.message)
		return
	}
	var req prototypes.MemoryDeleteRequest
	if perr := decodeMemoryBody(w, r, &req); perr != nil {
		writeMemoryError(w, perr.code, perr.status, perr.message)
		return
	}
	if perr := reconcileMemoryIdentity(r, &req.Identity); perr != nil {
		writeMemoryError(w, perr.code, perr.status, perr.message)
		return
	}
	if perr := assertMemoryAdmin(r.Context()); perr != nil {
		writeMemoryError(w, perr.code, perr.status, perr.message)
		return
	}
	if req.Key == "" {
		writeMemoryError(w, protoerrors.CodeInvalidRequest, http.StatusBadRequest,
			"memory.delete requires a non-empty key")
		return
	}
	resp, err := memprotocol.Delete(r.Context(), memprotocol.DeleteDeps{Store: h.store, Bus: h.bus},
		req, identity.Quadruple{Identity: id})
	if err != nil {
		code, status, msg := classifyMemoryError(err)
		writeMemoryError(w, code, status, msg)
		return
	}
	h.encode(r.Context(), w, id, &resp)
}

// assertMemoryAdmin gates a mutation method on the verified `admin` scope
// claim. Unlike the cross-tenant READ gate (which admits `admin`
// OR `console:fleet`), a runtime-state MUTATION requires `admin` strictly —
// `console:fleet` is a cross-runtime observation claim, never a write
// entitlement. Fails closed with CodeIdentityScopeRequired (403).
func assertMemoryAdmin(ctx context.Context) *memoryError {
	if auth.HasScope(ctx, auth.ScopeAdmin) {
		return nil
	}
	return &memoryError{
		code:    protoerrors.CodeIdentityScopeRequired,
		status:  http.StatusForbidden,
		message: "this memory mutation requires the verified `admin` scope claim",
	}
}

// serveList answers `POST /v1/memory/list`.
func (h *MemoryHandler) serveList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeMemoryError(w, protoerrors.CodeInvalidRequest, http.StatusMethodNotAllowed,
			"memory.list accepts POST only")
		return
	}
	id, r, perr := h.resolveAndDecode(w, r)
	if perr != nil {
		writeMemoryError(w, perr.code, perr.status, perr.message)
		return
	}
	var req prototypes.MemoryListRequest
	if perr := decodeMemoryBody(w, r, &req); perr != nil {
		writeMemoryError(w, perr.code, perr.status, perr.message)
		return
	}
	if perr := reconcileMemoryIdentity(r, &req.Identity); perr != nil {
		writeMemoryError(w, perr.code, perr.status, perr.message)
		return
	}
	// Cross-tenant gate — a filter naming a tenant other than the
	// caller's own, or more than one tenant, requires the verified
	// `admin` (or `console:fleet`) scope claim from the closed
	// two-scope set. NO new memory scope (audit B1).
	if memoryCrossTenantRequested(req.Filter.TenantIDs, id.TenantID) && !memoryHasAdminScope(r.Context()) {
		writeMemoryError(w, protoerrors.CodeIdentityScopeRequired, http.StatusForbidden,
			"cross-tenant memory.list requires a verified `admin` or `console:fleet` scope claim")
		return
	}

	resp, err := memprotocol.List(r.Context(), memprotocol.ListDeps{
		Store:          h.store,
		Aggregator:     h.aggregator,
		DriverName:     h.driverName,
		HeavyThreshold: h.threshold,
	}, req, identity.Quadruple{Identity: id})
	if err != nil {
		code, status, msg := classifyMemoryError(err)
		writeMemoryError(w, code, status, msg)
		return
	}
	h.encode(r.Context(), w, id, &resp)
}

// serveGet answers `POST /v1/memory/get`.
func (h *MemoryHandler) serveGet(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeMemoryError(w, protoerrors.CodeInvalidRequest, http.StatusMethodNotAllowed,
			"memory.get accepts POST only")
		return
	}
	id, r, perr := h.resolveAndDecode(w, r)
	if perr != nil {
		writeMemoryError(w, perr.code, perr.status, perr.message)
		return
	}
	var req prototypes.MemoryGetRequest
	if perr := decodeMemoryBody(w, r, &req); perr != nil {
		writeMemoryError(w, perr.code, perr.status, perr.message)
		return
	}
	if perr := reconcileMemoryIdentity(r, &req.Identity); perr != nil {
		writeMemoryError(w, perr.code, perr.status, perr.message)
		return
	}

	resp, err := memprotocol.Get(r.Context(), memprotocol.GetDeps{
		Store:          h.store,
		Artifacts:      h.artifacts,
		DriverName:     h.driverName,
		HeavyThreshold: h.threshold,
	}, req, identity.Quadruple{Identity: id})
	if err != nil {
		code, status, msg := classifyMemoryError(err)
		writeMemoryError(w, code, status, msg)
		return
	}
	h.encode(r.Context(), w, id, &resp)
}

// serveHealth answers `POST /v1/memory/health`.
func (h *MemoryHandler) serveHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeMemoryError(w, protoerrors.CodeInvalidRequest, http.StatusMethodNotAllowed,
			"memory.health accepts POST only")
		return
	}
	id, r, perr := h.resolveAndDecode(w, r)
	if perr != nil {
		writeMemoryError(w, perr.code, perr.status, perr.message)
		return
	}
	var req prototypes.MemoryHealthRequest
	if perr := decodeMemoryBody(w, r, &req); perr != nil {
		writeMemoryError(w, perr.code, perr.status, perr.message)
		return
	}
	if perr := reconcileMemoryIdentity(r, &req.Identity); perr != nil {
		writeMemoryError(w, perr.code, perr.status, perr.message)
		return
	}

	resp, err := memprotocol.Health(r.Context(), memprotocol.HealthDeps{
		Store:      h.store,
		Aggregator: h.aggregator,
		DriverName: h.driverName,
	}, identity.Quadruple{Identity: id})
	if err != nil {
		code, status, msg := classifyMemoryError(err)
		writeMemoryError(w, code, status, msg)
		return
	}
	h.encode(r.Context(), w, id, &resp)
}

// resolveAndDecode establishes the request identity and returns the
// request whose context carries it. A missing / incomplete triple fails
// closed with CodeIdentityRequired (401). Callers MUST use the returned
// request: its context is what the body-scope gate reconciles against.
func (h *MemoryHandler) resolveAndDecode(_ http.ResponseWriter, r *http.Request) (identity.Identity, *http.Request, *memoryError) {
	id, r, err := resolveIdentity(r)
	if err != nil {
		return identity.Identity{}, r, &memoryError{
			code: protoerrors.CodeIdentityRequired, status: http.StatusUnauthorized,
			message: "identity scope incomplete: " + err.Error(),
		}
	}
	return id, r, nil
}

// encode writes a JSON 200 response. An encode failure is logged
// loudly but not re-surfaced — the status line is already committed.
func (h *MemoryHandler) encode(ctx context.Context, w http.ResponseWriter, id identity.Identity, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		h.logger.WarnContext(ctx, "memory: response encode failed",
			slog.String("error", err.Error()),
			slog.String("tenant_id", id.TenantID),
			slog.String("user_id", id.UserID),
			slog.String("session_id", id.SessionID))
	}
}

// memoryError is the internal carrier for a classified memory.* failure.
type memoryError struct {
	code    protoerrors.Code
	status  int
	message string
}

// decodeMemoryBody reads the bounded request body into dst.
func decodeMemoryBody(w http.ResponseWriter, r *http.Request, dst any) *memoryError {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxMemoryBodyBytes))
	if err != nil {
		return &memoryError{
			code: protoerrors.CodeInvalidRequest, status: http.StatusBadRequest,
			message: "failed to read request body: " + err.Error(),
		}
	}
	if len(body) == 0 {
		return nil
	}
	dec := json.NewDecoder(bytesReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return &memoryError{
			code: protoerrors.CodeInvalidRequest, status: http.StatusBadRequest,
			message: "failed to decode request body: " + err.Error(),
		}
	}
	return nil
}

// reconcileMemoryIdentity routes the request body's identity scope
// through the shared body-identity gate under the memory surface's
// registered policy: an empty triple is backfilled from the verified
// identity, and a populated component must match it. A cross-tenant
// memory read travels in the list filter's tenant set, gated on the
// verified claim — never in the body triple.
func reconcileMemoryIdentity(r *http.Request, scope *prototypes.IdentityScope) *memoryError {
	perr := reconcileBodyScope(r, scope, bodyscope.SurfaceMemory)
	if perr == nil {
		return nil
	}
	return &memoryError{
		code: perr.Code, status: bodyScopeStatus(perr.Code), message: perr.Message,
	}
}

// memoryCrossTenantRequested reports whether the filter's tenant set
// reaches outside the caller's own tenant — a foreign tenant, or more
// than one tenant.
func memoryCrossTenantRequested(tenantIDs []string, callerTenant string) bool {
	if len(tenantIDs) == 0 {
		return false
	}
	if len(tenantIDs) > 1 {
		return true
	}
	return tenantIDs[0] != callerTenant
}

// memoryHasAdminScope reports whether ctx carries the closed
// two-scope set's cross-tenant entitlement — `admin` OR `console:fleet`.
// NO new memory scope is consulted (audit B1).
func memoryHasAdminScope(ctx context.Context) bool {
	return auth.HasScope(ctx, auth.ScopeAdmin) || auth.HasScope(ctx, auth.ScopeConsoleFleet)
}

// classifyMemoryError maps an internal/memory/protocol error onto a
// canonical Protocol Code + HTTP status + safe operator-facing message.
func classifyMemoryError(err error) (protoerrors.Code, int, string) {
	switch {
	case errors.Is(err, memory.ErrIdentityRequired):
		return protoerrors.CodeIdentityRequired, http.StatusUnauthorized,
			"memory: identity scope incomplete"
	case errors.Is(err, memprotocol.ErrInvalidFilter):
		return protoerrors.CodeInvalidRequest, http.StatusBadRequest,
			"memory: invalid filter — " + err.Error()
	case errors.Is(err, memprotocol.ErrPageOutOfRange):
		return protoerrors.CodeInvalidRequest, http.StatusBadRequest,
			"memory: invalid pagination — " + err.Error()
	case errors.Is(err, memory.ErrNotFound):
		return protoerrors.CodeNotFound, http.StatusNotFound,
			"memory: record not found"
	case errors.Is(err, memprotocol.ErrContextLeak):
		return protoerrors.CodeRuntimeError, http.StatusInternalServerError,
			"memory: heavy value reached the response path inline — failed loudly"
	case errors.Is(err, memory.ErrStoreClosed):
		return protoerrors.CodeRuntimeError, http.StatusInternalServerError,
			"memory: store is closed"
	default:
		return protoerrors.CodeRuntimeError, http.StatusInternalServerError,
			"memory: request failed"
	}
}

// writeMemoryError writes a JSON error body with the canonical Protocol
// Code + the supplied HTTP status. The body shape matches the REST
// control transport's error body so a client can decode both with the
// same Error wire type.
func writeMemoryError(w http.ResponseWriter, code protoerrors.Code, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(&protoerrors.Error{ //nolint:errcheck // response status already committed — a write error cannot be recovered here.
		Code:    code,
		Message: message,
	})
}
