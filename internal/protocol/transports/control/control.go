// Package control is the Harbor Protocol REST/JSON control transport —
// the client→server half of the wire binding RFC §5.4 resolves to (SSE
// for events + REST/JSON for control). It is a thin adapter over the
// transport-agnostic protocol.ControlSurface that An earlier phase shipped: a
// Handler decodes an HTTP request body into the Protocol wire request
// type the method expects, calls ControlSurface.Dispatch, and encodes
// the wire response — or maps a *protocol/errors.Error onto an HTTP
// status (status.go) and a JSON error body.
//
// # The route shape
//
// One route serves every task-control method:
//
//	POST /v1/control/{method}
//
// where {method} is one of the ten canonical method names
// (internal/protocol/methods). `start` carries a types.StartRequest;
// the nine steering controls carry a types.ControlRequest. The method
// name is read from the path, never hardcoded — the handler validates
// it against methods.IsValidMethod and the single-source lint
// forbids a method string literal anywhere under internal/protocol/
// outside the methods package.
//
// # Identity at the edge (RFC §5.5, CLAUDE.md §6)
//
// The identity triple (+ run for a control) lives in the request body's
// `identity` object — the flat types.IdentityScope. The handler does NOT
// re-validate it: ControlSurface.Dispatch already fails closed on an
// incomplete triple with CodeIdentityRequired, which status.go maps to
// 401. The edge structure — decode, hand the whole request to Dispatch,
// map the error — is the single choke point slots JWT
// validation into without reshaping the handler.
//
// # Concurrent reuse
//
// Handler is a compiled artifact: the ControlSurface and the logger are
// set once at construction and never mutated. ServeHTTP holds no
// per-request state on the Handler — every request's data lives on the
// *http.Request and the response is written to its own
// http.ResponseWriter. One Handler serves N concurrent requests safely;
// internal/protocol/transports/concurrent_test.go pins it under -race.
package control

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/hurtener/Harbor/internal/audit"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	"github.com/hurtener/Harbor/internal/protocol/bodyscope"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/protocol/types"
)

// RoutePattern is the http.ServeMux pattern the control transport
// registers under. The {method} wildcard is read via r.PathValue.
// Exported so internal/protocol/transports can mount the handler under
// the same pattern it documents.
const RoutePattern = "POST /v1/control/{method}"

// maxBodyBytes bounds a control request body. The Protocol control
// payload's own RFC §6.3 bound is 16 KiB; 64 KiB leaves generous room
// for the surrounding JSON envelope while still failing closed on a
// client that streams an unbounded body at the edge.
const maxBodyBytes = 64 << 10

// Handler is the Protocol REST/JSON control transport. It is built once
// per Runtime process via NewHandler and shared across every control
// request; ServeHTTP is safe for concurrent use by N goroutines.
//
// extension: when `bus` AND `redactor` are wired via
// WithEventBus / WithRedactor, the handler accepts admin-impersonation
// requests (IdentityScope.Impersonating set) and emits a redacted
// `audit.admin_scope_used` event on the bus on every accepted
// impersonation. When either dependency is missing, impersonation
// requests are rejected loudly with `CodeRuntimeError` — never silently
// accepted without the audit emit (CLAUDE.md §5, §7 rule 6, §13 "Silent
// degradation").
//
// when constructed with `WithSearchSurface`, the
// handler routes the five `search.*` methods to the search dispatcher
// instead of the task-control ControlSurface. The same handler still
// serves all the task-control methods unchanged.
//
// when constructed with
// `WithPostureSurface`, the handler routes the seven posture methods —
// the five `runtime.*` / `metrics.*` reads plus `governance.posture` /
// `llm.posture` — to the posture dispatcher. Like the search surface,
// this is additive — handlers built without it reject posture calls
// with CodeUnknownMethod (the 404 → SKIP path the smoke script relies
// on).
type Handler struct {
	surface                  *protocol.ControlSurface
	searchSurface            SearchSurface
	postureSurface           PostureSurface
	artifactsSurface         ArtifactsSurface
	mcpSurface               MCPSurface
	appsSurface              AppsSurface
	skillPublicationsSurface SkillPublicationsSurface
	logger                   *slog.Logger
	bus                      events.EventBus // nil ⇒ impersonation accepted-path refused
	redactor                 audit.Redactor  // nil ⇒ impersonation accepted-path refused
	now                      func() time.Time
	// bodyScopeAuditor is the sink the shared body-identity gate records
	// a granted identity crossing on. Built once, after the options have
	// been applied, so it sees the wired bus and redactor.
	bodyScopeAuditor bodyscope.Auditor
}

// MCPSurface is the narrow contract the control transport calls into for
// the twelve `mcp.servers.*` Protocol methods. The
// production implementation is *protocol.MCPSurface; tests inject a
// deterministic surface. A nil surface means the handler rejects MCP
// calls with CodeUnknownMethod — preserving the 404 → SKIP path the
// smoke script relies on while the surface is being wired through.
type MCPSurface interface {
	// Dispatch handles one of the twelve `mcp.servers.*` methods.
	// Returns either a *types.<Method>Response or a *errors.Error.
	Dispatch(ctx context.Context, method methods.Method, req any) (any, error)
}

// AppsSurface is the narrow contract the control transport calls into for
// the two MCP Apps host methods — `mcp.servers.read_resource` and
// `mcp.apps.call_tool`. The production implementation is
// *protocol.AppsSurface; tests inject a deterministic surface. A nil
// surface means the handler rejects MCP Apps calls with
// CodeUnknownMethod — preserving the 404 → SKIP path the smoke script
// relies on while the surface is being wired through.
type AppsSurface interface {
	// Dispatch handles one of the two MCP Apps methods. Returns either a
	// *types.ReadMCPResourceResponse / *types.MCPAppCallToolResponse, or
	// a *errors.Error.
	Dispatch(ctx context.Context, method methods.Method, req any) (any, error)
}

// SearchSurface is the narrow contract the control transport calls into
// for the five `search.*` Protocol methods. The production
// implementation lives in internal/protocol/control/search.go (the
// SearchHandler shaped over a search.SearcherRegistry); tests inject a
// deterministic surface. A nil surface means the handler will reject
// search calls with CodeUnknownMethod — preserving the 404 path the
// smoke script's `skip_if_404` relies on while the surface is being
// wired through.
type SearchSurface interface {
	// Dispatch handles one of the five canonical search.* methods.
	// Returns either a *types.SearchResponse or a *errors.Error.
	Dispatch(ctx context.Context, method methods.Method, req *types.SearchRequest) (*types.SearchResponse, error)
}

// PostureSurface is the narrow contract the control transport calls
// into for the seven posture Protocol methods — the five `runtime.*` /
// `metrics.*` reads plus `governance.posture` and
// `llm.posture`. The production implementation is
// *protocol.PostureSurface; tests inject a deterministic surface. A nil
// surface means the handler rejects posture calls with
// CodeUnknownMethod — preserving the 404 → SKIP path the smoke script
// relies on while the surface is being wired through.
//
// All seven posture methods share the one read-only request envelope
// `*types.RuntimeInfoRequest` — the governance / llm reads are also
// identity-only, so they reuse the same shape rather than threading two
// near-identical wire types.
type PostureSurface interface {
	// Dispatch handles one of the seven canonical posture methods.
	// Returns either a *types.RuntimeInfo / *types.RuntimeHealth /
	// *types.RuntimeCounters / *types.RuntimeDrivers /
	// *types.MetricsSnapshot / *types.GovernancePostureResponse /
	// *types.LLMPostureResponse, or a *errors.Error.
	Dispatch(ctx context.Context, method methods.Method, req any) (any, error)
}

// ArtifactsSurface is the narrow contract the control transport calls
// into for the three artifacts methods —
// `artifacts.list`, `artifacts.put`, `artifacts.get_ref`. The production
// implementation is *protocol.ArtifactsSurface; tests inject a
// deterministic surface. A nil surface means the handler rejects
// artifacts calls with CodeUnknownMethod — preserving the 404 → SKIP
// path the smoke script relies on while the surface is being wired
// through.
type ArtifactsSurface interface {
	// Dispatch handles one of the three canonical artifacts methods.
	// Returns either a *types.ArtifactsListResponse /
	// *types.ArtifactsPutResponse / *types.ArtifactsGetRefResponse, or
	// a *errors.Error.
	Dispatch(ctx context.Context, method methods.Method, req any) (any, error)
}

// SkillPublicationsSurface is the narrow contract the control transport calls
// into for HA-68's organization publication and verified-caller Agent reach
// methods. The production implementation is
// *protocol.SkillPublicationsSurface; tests may inject a deterministic
// dispatcher. A nil surface leaves the methods unavailable (HTTP 404), which
// is the partial-build posture used by the other optional Protocol surfaces.
type SkillPublicationsSurface interface {
	// Dispatch handles one of the ten canonical skills.publications.* methods.
	// Authority is derived from the verified request context; the request body
	// is data only and must never grant admin or Agent reach.
	Dispatch(ctx context.Context, method methods.Method, req any) (any, error)
}

// Option configures a Handler at construction time.
type Option func(*Handler)

// WithLogger sets the slog.Logger the handler logs decode / dispatch
// failures to. A nil logger (the default) routes to slog.Default().
func WithLogger(l *slog.Logger) Option {
	return func(h *Handler) {
		if l != nil {
			h.logger = l
		}
	}
}

// WithEventBus wires the canonical events.EventBus into the handler so
// the admin-impersonation gate can publish a typed
// `audit.admin_scope_used` event onto the bus when an impersonation
// request is accepted. The bus is OPTIONAL — when not supplied, the
// handler refuses impersonation requests with CodeRuntimeError (the
// audit emit is the load-bearing accountability surface for
// impersonation; rejecting fails-closed rather than silently degrading).
// A nil bus is treated as "WithEventBus not supplied".
func WithEventBus(b events.EventBus) Option {
	return func(h *Handler) {
		if b != nil {
			h.bus = b
		}
	}
}

// WithRedactor wires the audit.Redactor the handler runs the
// impersonation audit payload through before publishing onto the
// event bus (CLAUDE.md §7 rule 6: "every payload goes through
// audit.Redactor"). The redactor is OPTIONAL at the type level but
// MANDATORY in practice for impersonation: a handler without a
// redactor refuses impersonation requests with CodeRuntimeError,
// same as the missing-bus case. A nil redactor is treated as
// "WithRedactor not supplied".
func WithRedactor(r audit.Redactor) Option {
	return func(h *Handler) {
		if r != nil {
			h.redactor = r
		}
	}
}

// WithClock overrides the handler's wall clock — used by tests so the
// `OccurredAt` field on the published audit event is deterministic. A
// nil clock keeps the default (time.Now).
func WithClock(now func() time.Time) Option {
	return func(h *Handler) {
		if now != nil {
			h.now = now
		}
	}
}

// WithSearchSurface wires the search dispatcher into the
// control handler. When supplied, the handler routes the five
// `search.*` methods to s.Dispatch instead of falling through to the
// task-control ControlSurface. Optional — handlers built without it
// reject search calls with CodeUnknownMethod (the 404 → SKIP path the
// smoke script relies on).
func WithSearchSurface(s SearchSurface) Option {
	return func(h *Handler) {
		h.searchSurface = s
	}
}

// WithPostureSurface wires the posture dispatcher into
// the control handler. When supplied, the handler routes the seven
// posture methods — the five `runtime.*` / `metrics.*` reads plus
// `governance.posture` / `llm.posture` — to s.Dispatch instead of
// falling through to the task-control ControlSurface. Optional —
// handlers built without it reject posture calls with
// CodeUnknownMethod (the 404 → SKIP path the smoke script relies on).
func WithPostureSurface(s PostureSurface) Option {
	return func(h *Handler) {
		h.postureSurface = s
	}
}

// WithArtifactsSurface wires the artifacts dispatcher
// into the control handler. When supplied, the handler routes the three
// artifacts methods — `artifacts.list`, `artifacts.put`,
// `artifacts.get_ref` — to s.Dispatch instead of falling through to the
// task-control ControlSurface. Optional — handlers built without it
// reject artifacts calls with CodeUnknownMethod (the 404 → SKIP path the
// smoke script relies on). A nil surface is treated as
// "WithArtifactsSurface not supplied".
func WithArtifactsSurface(s ArtifactsSurface) Option {
	return func(h *Handler) {
		if s != nil {
			h.artifactsSurface = s
		}
	}
}

// WithMCPSurface wires the MCP-Connections dispatcher
// into the control handler. When supplied, the handler routes the twelve
// `mcp.servers.*` methods to s.Dispatch instead of falling through to
// the task-control ControlSurface. Optional — handlers built without it
// reject MCP calls with CodeUnknownMethod (the 404 → SKIP path the smoke
// script relies on).
func WithMCPSurface(s MCPSurface) Option {
	return func(h *Handler) {
		h.mcpSurface = s
	}
}

// WithAppsSurface wires the MCP Apps dispatcher into the control handler.
// When supplied, the handler routes the two MCP Apps methods
// (`mcp.servers.read_resource` / `mcp.apps.call_tool`) to s.Dispatch
// instead of falling through to the task-control ControlSurface.
// Optional — handlers built without it reject MCP Apps calls with
// CodeUnknownMethod (the 404 → SKIP path the smoke script relies on).
func WithAppsSurface(s AppsSurface) Option {
	return func(h *Handler) {
		h.appsSurface = s
	}
}

// WithSkillPublicationsSurface wires the HA-68 publication dispatcher into
// the control handler. When unsupplied, publication methods return the normal
// unknown-method response instead of reaching the task-control surface.
func WithSkillPublicationsSurface(s SkillPublicationsSurface) Option {
	return func(h *Handler) {
		if s != nil {
			h.skillPublicationsSurface = s
		}
	}
}

// NewHandler builds the Protocol REST/JSON control transport over the
// transport-agnostic ControlSurface. The surface is mandatory — a nil
// fails loud with ErrMisconfigured rather than building a handler that
// would nil-panic on the first request (CLAUDE.md §5).
//
// The bus + redactor are OPTIONAL at construction so existing tests
// that don't exercise the impersonation path can call
// `NewHandler(surface)` unchanged. Production callers (transports.NewMux)
// MUST wire both via WithEventBus + WithRedactor; an impersonation
// request without them is rejected with CodeRuntimeError (CLAUDE.md §13
// "Silent degradation" — no quiet accept).
//
// The returned *Handler is immutable after construction and safe
// for concurrent use by N goroutines.
func NewHandler(surface *protocol.ControlSurface, opts ...Option) (*Handler, error) {
	if surface == nil {
		return nil, fmt.Errorf("%w: protocol.ControlSurface is nil", ErrMisconfigured)
	}
	h := &Handler{
		surface: surface,
		logger:  slog.Default(),
		now:     time.Now,
	}
	for _, opt := range opts {
		opt(h)
	}
	h.bodyScopeAuditor = bodyscope.NewBusAuditor(h.bus, h.redactor, h.logger)
	return h, nil
}

// ErrMisconfigured — NewHandler was called with a nil ControlSurface.
var ErrMisconfigured = errors.New("control: REST transport missing a mandatory dependency")

// ServeHTTP implements http.Handler. It decodes the request into the
// wire type the path's method expects, dispatches it through the
// ControlSurface, and writes the JSON wire response — or a JSON error
// body with the mapped HTTP status.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// The ServeMux pattern already pins POST; defend anyway so a Handler
	// mounted bare (not via NewMux) still rejects a non-POST closed.
	if r.Method != http.MethodPost {
		h.writeError(w, r, protoerrors.Newf(protoerrors.CodeInvalidRequest,
			"control transport accepts POST only, got %s", r.Method))
		return
	}

	method := methods.Method(r.PathValue("method"))
	if !methods.IsValidMethod(method) {
		h.writeError(w, r, protoerrors.Newf(protoerrors.CodeUnknownMethod,
			"method %q is not a canonical Protocol method", string(method)))
		return
	}

	// the five `search.*` methods route through a
	// separate SearchSurface — they're not steering controls, they
	// don't reach the task registry, and their wire shape is
	// `*types.SearchRequest`. If no SearchSurface is wired, fall
	// through to the unknown-method path so the smoke `skip_if_404`
	// branch fires.
	if methods.IsSearchMethod(method) {
		if h.searchSurface == nil {
			h.writeError(w, r, protoerrors.Newf(protoerrors.CodeUnknownMethod,
				"method %q: search surface is not configured on this Runtime", string(method)))
			return
		}
		h.serveSearch(w, r, method)
		return
	}

	// the seven posture methods — the
	// five `runtime.*` / `metrics.*` reads plus `governance.posture` /
	// `llm.posture` — route through a separate PostureSurface. They are
	// read-only runtime-config / observability projections, not
	// steering controls, they don't reach the task registry, and they
	// share the one `*types.RuntimeInfoRequest` envelope. If no
	// PostureSurface is wired, fall through to the unknown-method path
	// so the smoke
	// `skip_if_404` branch fires.
	if methods.IsPostureMethod(method) {
		if h.postureSurface == nil {
			h.writeError(w, r, protoerrors.Newf(protoerrors.CodeUnknownMethod,
				"method %q: posture surface is not configured on this Runtime", string(method)))
			return
		}
		h.servePosture(w, r, method)
		return
	}

	// the three `artifacts.*` methods route through a
	// separate ArtifactsSurface — they're not steering controls, they
	// don't reach the task registry, and they carry their own wire
	// shapes (ArtifactsListRequest / ArtifactsPutRequest /
	// ArtifactsGetRefRequest). artifacts.put also carries an upload
	// payload larger than the 64 KiB control body cap, so the artifacts
	// adapter applies its own 8 MiB transport-edge cap. If no
	// ArtifactsSurface is wired, fall through to the unknown-method path
	// so the smoke `skip_if_404` branch fires.
	if methods.IsArtifactsMethod(method) {
		if h.artifactsSurface == nil {
			h.writeError(w, r, protoerrors.Newf(protoerrors.CodeUnknownMethod,
				"method %q: artifacts surface is not configured on this Runtime", string(method)))
			return
		}
		h.serveArtifacts(w, r, method)
		return
	}

	// the twelve `mcp.servers.*` methods route
	// through a separate MCPSurface — they reach the runtime's MCP
	// driver registry + OAuth provider, not the steering inbox. If no
	// MCPSurface is wired, fall through to the unknown-method path so
	// the smoke `skip_if_404` branch fires.
	if methods.IsMCPServersMethod(method) {
		if h.mcpSurface == nil {
			h.writeError(w, r, protoerrors.Newf(protoerrors.CodeUnknownMethod,
				"method %q: MCP surface is not configured on this Runtime", string(method)))
			return
		}
		h.serveMCP(w, r, method)
		return
	}

	// the two MCP Apps host methods
	// (`mcp.servers.read_resource` / `mcp.apps.call_tool`) route through a
	// separate AppsSurface — they reach the runtime's MCP registry + tool
	// catalog, not the steering inbox. If no AppsSurface is wired, fall
	// through to the unknown-method path so the smoke `skip_if_404` branch
	// fires.
	if methods.IsMCPAppsMethod(method) {
		if h.appsSurface == nil {
			h.writeError(w, r, protoerrors.Newf(protoerrors.CodeUnknownMethod,
				"method %q: MCP Apps surface is not configured on this Runtime", string(method)))
			return
		}
		h.serveApps(w, r, method)
		return
	}

	// HA-68 publication methods have their own wire envelopes and
	// fail-closed authority checks. In particular, do not run the generic
	// body-identity reconciliation here: a publication body must never grant
	// authority, and the surface compares it to the verified ctx identity.
	if methods.IsSkillPublicationMethod(method) {
		if h.skillPublicationsSurface == nil {
			h.writeError(w, r, protoerrors.Newf(protoerrors.CodeUnknownMethod,
				"method %q: skill-publications surface is not configured on this Runtime", string(method)))
			return
		}
		h.serveSkillPublications(w, r, method)
		return
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		// A MaxBytesReader overflow or a transport read error — the
		// request body could not be read. Fail closed with
		// invalid_request rather than guessing at a partial body.
		h.writeError(w, r, protoerrors.Newf(protoerrors.CodeInvalidRequest,
			"method %q: request body could not be read", string(method)))
		return
	}

	req, perr := decodeRequest(method, body)
	if perr != nil {
		h.writeError(w, r, perr)
		return
	}

	// The body's identity scope is caller-supplied input; the verified
	// identity on ctx is the authority. The shared gate reconciles the
	// two under the surface's registered policy before Dispatch is
	// called, and returns the ctx the dispatch runs under.
	ctx, perr := h.reconcileBodyIdentity(r, req)
	if perr != nil {
		h.writeError(w, r, perr)
		return
	}

	// admin-impersonation gate. When the body's
	// IdentityScope carries `Impersonating`, the handler validates the
	// full triplet (Actor / Requester / Impersonating) and gates on
	// auth.ScopeAdmin on the verified JWT before allowing Dispatch.
	// The accepted-path emit (audit.admin_scope_used with a typed
	// AdminScopeUsedPayload) happens only AFTER Dispatch succeeds, so
	// a failed Dispatch never reaches the bus.
	impersonating, iperr := h.assertImpersonationShape(r, method, req)
	if iperr != nil {
		h.writeError(w, r, iperr)
		return
	}
	if impersonating {
		// The accepted impersonation runs under the impersonated
		// identity. Seating it as an audited crossing means any
		// re-scoping the dispatch performs downstream sees the identity
		// the run actually executes under, with the reason on record.
		elevated, eperr := h.elevateForImpersonation(ctx, method, req)
		if eperr != nil {
			h.writeError(w, r, eperr)
			return
		}
		ctx = elevated
	}

	// Dispatch is the transport-agnostic surface; the wire transport is
	// a thin adapter over it. Identity-scope enforcement, scope checks,
	// payload validation all live inside Dispatch — the handler does not
	// re-implement any of them (CLAUDE.md §13 forbids a second
	// validator).
	resp, derr := h.surface.Dispatch(ctx, method, req)
	if derr != nil {
		h.writeDispatchError(w, r, method, derr)
		return
	}

	// emit the audit.admin_scope_used event AFTER a
	// successful Dispatch. Identity is mandatory, capability (admin
	// scope) is mandatory, and the audit emit is mandatory whenever
	// impersonation was accepted (CLAUDE.md §5 + §7 rule 6). The
	// handler refuses impersonation paths up-front when bus/redactor
	// are not wired (assertImpersonationShape), so by the time we get
	// here the dependencies are present.
	if impersonating {
		if eerr := h.emitAdminScopeUsed(r.Context(), method, req); eerr != nil {
			// The emit failed *after* Dispatch succeeded — the run is
			// already in flight. We log loudly, but the response stays
			// 200 because the caller-visible action did succeed. The
			// operator MUST see the bus-side audit drift through the
			// log channel. CLAUDE.md §5 "fail loudly" — never silent.
			h.logger.ErrorContext(r.Context(), "control: impersonation accepted but audit emit failed",
				slog.String("method", string(method)),
				slog.String("error", eerr.Error()))
		}
	}

	h.writeJSON(w, r, http.StatusOK, resp)
}

// decodeRequest decodes a request body into the wire request type the
// method expects: types.StartRequest for `start`, types.ControlRequest
// for the nine steering controls. A decode failure surfaces as
// CodeInvalidRequest — never a silent zero-value request.
func decodeRequest(method methods.Method, body []byte) (any, *protoerrors.Error) {
	if method == methods.MethodStart {
		var sr types.StartRequest
		if err := decodeStrict(body, &sr); err != nil {
			return nil, protoerrors.Newf(protoerrors.CodeInvalidRequest,
				"method %q: request body is not a valid StartRequest: %s", string(method), decodeDetail(err))
		}
		return &sr, nil
	}
	// `topology.snapshot` carries a flat
	// TopologySnapshotRequest (just the identity scope) — neither a
	// StartRequest nor a ControlRequest.
	if methods.IsTopologyMethod(method) {
		var tr types.TopologySnapshotRequest
		if err := decodeStrict(body, &tr); err != nil {
			return nil, protoerrors.Newf(protoerrors.CodeInvalidRequest,
				"method %q: request body is not a valid TopologySnapshotRequest: %s", string(method), decodeDetail(err))
		}
		return &tr, nil
	}
	var cr types.ControlRequest
	if err := decodeStrict(body, &cr); err != nil {
		return nil, protoerrors.Newf(protoerrors.CodeInvalidRequest,
			"method %q: request body is not a valid ControlRequest: %s", string(method), decodeDetail(err))
	}
	return &cr, nil
}

// decodeStrict decodes a control-transport request body into target and
// REFUSES any member the wire type does not define. It is the ONE decode
// on this transport — every handler in the package routes through it, so
// there is no lax sibling to drift against (CLAUDE.md §13 forbids two
// implementations of one concept).
//
// # Why unknown members are refused rather than dropped
//
// `encoding/json`'s default is to discard a member no field matches. On a
// Protocol request that turns a caller's explicit instruction into a
// SUCCESS that did not happen: a client speaking a newer Protocol sends an
// additive optional field, an older Runtime drops it, and the run proceeds
// without the content the caller believes it supplied. That is the silent
// degradation CLAUDE.md §13 forbids, at a version boundary, and no amount
// of downstream validation can recover it — the bytes are gone before any
// validator sees them.
//
// The refusal is not a new Protocol posture. Every OTHER Harbor Protocol
// request handler already decodes strictly (the whole
// `internal/protocol/transports/stream` family, the Go Protocol client,
// the agent-config credential-descriptor decodes whose "rejected BY NAME"
// guarantee is stated in their godoc). This transport was the outlier; the
// asymmetry was an omission, not a policy.
//
// It also rejects trailing data after the JSON document, which
// `json.Unmarshal` did and a bare `Decoder.Decode` does not — the swap
// must not loosen anything.
func decodeStrict(body []byte, target any) error {
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(target); err != nil {
		return err
	}
	if dec.More() {
		return errTrailingData
	}
	return nil
}

// errTrailingData is returned by decodeStrict when a second JSON value
// follows the request document. `json.Unmarshal` refused this shape, so
// keeping it refused is what makes the strict decode a strict SUPERSET of
// the decode it replaced.
var errTrailingData = errors.New("unexpected trailing data after the JSON document")

// maxDecodeDetailBytes bounds how much of a decoder error is echoed back
// to the caller. The detail is what makes a strict refusal actionable —
// `json: unknown field "caller_memory"` names the field the caller must
// stop sending, which is the whole point of refusing instead of dropping.
// The bound exists because the quoted member name is caller-controlled and
// a request body may carry 64 KiB of it.
const maxDecodeDetailBytes = 160

// decodeDetail renders a decoder error for the wire: the reason, bounded.
// It never carries a decoded VALUE — `encoding/json` reports the offending
// member's name and type, never its contents — so this cannot echo caller
// payload back out.
func decodeDetail(err error) string {
	msg := err.Error()
	if len(msg) > maxDecodeDetailBytes {
		return msg[:maxDecodeDetailBytes] + "…"
	}
	return msg
}

// writeDispatchError maps a Dispatch error onto the wire. Dispatch's
// contract is that every error case is a *protoerrors.Error; if a
// non-Protocol error ever surfaces, it is wrapped as CodeRuntimeError
// rather than leaked verbatim (CLAUDE.md §5 + §7 — no raw runtime detail
// on the wire).
func (h *Handler) writeDispatchError(w http.ResponseWriter, r *http.Request, method methods.Method, err error) {
	var perr *protoerrors.Error
	if errors.As(err, &perr) {
		h.writeError(w, r, perr)
		return
	}
	h.logger.ErrorContext(r.Context(), "control transport: Dispatch returned a non-Protocol error",
		slog.String("method", string(method)))
	h.writeError(w, r, protoerrors.Newf(protoerrors.CodeRuntimeError,
		"method %q: dispatch failed", string(method)))
}

// writeError encodes a *protoerrors.Error as a JSON body with the
// mapped HTTP status.
func (h *Handler) writeError(w http.ResponseWriter, r *http.Request, perr *protoerrors.Error) {
	h.writeJSON(w, r, HTTPStatus(perr.Code), perr)
}

// elevateForImpersonation seats the impersonated identity on ctx as an
// audited crossing, so a dispatch that re-scopes downstream sees the
// identity the run actually executes under rather than the admin's own.
//
// It runs only after assertImpersonationShape has passed: the admin
// claim is verified, the actor matches the verified identity, and the
// bus + redactor are wired for the post-dispatch audit emit. The reason
// names the surface and the actor so the crossing reads back as
// "this admin, acting as this identity, through this method".
func (h *Handler) elevateForImpersonation(ctx context.Context, method methods.Method, req any) (context.Context, *protoerrors.Error) {
	var scope types.IdentityScope
	switch v := req.(type) {
	case *types.StartRequest:
		scope = v.Identity
	case *types.ControlRequest:
		scope = v.Identity
	default:
		return ctx, nil
	}
	imp := scope.Impersonating
	if imp == nil {
		return ctx, nil
	}
	target := identity.Identity{TenantID: imp.Tenant, UserID: imp.User, SessionID: imp.Session}
	elevated, err := identity.WithElevated(ctx, target,
		fmt.Sprintf("method %q: admin impersonation accepted under the admin scope claim", string(method)))
	if err != nil {
		// Unreachable: assertImpersonationShape already validated the
		// impersonated triple. Kept as a defensive fail-closed.
		return ctx, protoerrors.Newf(protoerrors.CodeIdentityRequired,
			"method %q: impersonation target identity incomplete: %v", string(method), err)
	}
	return elevated, nil
}

// reconcileBodyIdentity routes a decoded control-transport request body
// through the shared body-identity gate under the surface's registered
// policy.
//
// Two surfaces arrive here. `task.start` and the steering controls hold
// all three components pinned to the verified identity: acting as
// another identity travels as the impersonation shape, never as a plain
// body triple. `topology.snapshot` declares the tenant admin-scoped, so
// a fleet operator reads another tenant's runtime graph under the admin
// claim and the gate records the crossing before granting it.
//
// An impersonation-shaped body is passed through untouched: its
// top-level triple is DELIBERATELY the impersonated identity, and
// assertImpersonationShape owns that shape's verification — it
// reconciles the body's actor triplet against the verified identity,
// requires the admin claim, and anchors the audit trail.
//
// It returns the ctx to dispatch under.
func (h *Handler) reconcileBodyIdentity(r *http.Request, req any) (context.Context, *protoerrors.Error) {
	ctx := r.Context()
	var scope *types.IdentityScope
	surface := bodyscope.SurfaceControlTask
	var aud bodyscope.Auditor
	switch v := req.(type) {
	case *types.StartRequest:
		scope = &v.Identity
	case *types.ControlRequest:
		scope = &v.Identity
	case *types.TopologySnapshotRequest:
		scope = &v.Identity
		surface = bodyscope.SurfaceTopology
		aud = h.bodyScopeAuditor
	default:
		return ctx, nil
	}
	if scope.IsImpersonating() {
		return ctx, nil
	}
	return bodyscope.Reconcile(ctx, bodyscope.ForIdentityScope(scope), surface, aud)
}

// assertImpersonationShape is the admin-impersonation gate.
// It returns (impersonating, error):
//
//   - impersonating == false, err == nil → no impersonation on the
//     body; existing behaviour is preserved.
//   - impersonating == true,  err == nil → the body's impersonation
//     triplet (Actor / Requester / Impersonating) passed every
//     structural + scope check; Dispatch is safe to call.
//   - impersonating == ?,     err != nil → the body is malformed
//     (incomplete triple, mismatched Actor, missing scope claim,
//     etc.) — the handler writes the protoerror and never calls
//     Dispatch.
//
// The checks are layered in fail-loud order:
//
//  1. Body carries Impersonating? → if not, return (false, nil).
//  2. Bus + Redactor wired? → if not, refuse with CodeRuntimeError
//     (no silent accept without the audit emit — CLAUDE.md §13).
//  3. JWT carries auth.ScopeAdmin? → if not, refuse with
//     CodeScopeMismatch (impersonation is an admin-only feature per
//     the Console design).
//  4. Impersonating triple complete? → if not, refuse with
//     CodeIdentityRequired (identity is mandatory; the impersonated
//     triple is identity too — CLAUDE.md §6 rule 9).
//  5. Actor + Requester present + complete? → if not, refuse with
//     CodeIdentityRequired.
//  6. Actor matches the verified JWT triple? → if not, refuse with
//     CodeScopeMismatch (the actor is the audit anchor; faking it is
//     a privilege-escalation attempt).
//  7. Requester == Actor (V1 invariant)? → if not, refuse with
//     CodeScopeMismatch (delegated impersonation is post-V1).
//  8. Top-level Tenant/User/Session == Impersonating triple? → if
//     not, refuse with CodeIdentityRequired (the run must execute
//     under the impersonated identity).
func (h *Handler) assertImpersonationShape(r *http.Request, method methods.Method, req any) (bool, *protoerrors.Error) {
	var bodyScope types.IdentityScope
	switch v := req.(type) {
	case *types.StartRequest:
		bodyScope = v.Identity
	case *types.ControlRequest:
		bodyScope = v.Identity
	default:
		return false, nil
	}
	if !bodyScope.IsImpersonating() {
		return false, nil
	}

	// (2) Audit emit is a hard precondition for accepting an
	// impersonation request — otherwise we'd silently accept the
	// admin-on-behalf-of action without bus-visible accountability.
	// CLAUDE.md §13 forbids silent degradation.
	if h.bus == nil || h.redactor == nil {
		h.logger.ErrorContext(r.Context(), "control: impersonation request received but transport not wired for audit emit",
			slog.String("method", string(method)))
		return false, protoerrors.Newf(protoerrors.CodeRuntimeError,
			"method %q: impersonation accepted-path requires audit emit (bus + redactor) on the transport; refusing fail-closed", string(method))
	}

	// (3) admin scope is mandatory — read from the verified scope set
	// on ctx (auth.Middleware injected it). HasScope returns false
	// when no scopes are attached (trust-based posture or
	// non-admin token), which is the safe default for a privilege
	// check.
	if !auth.HasScope(r.Context(), auth.ScopeAdmin) {
		return false, protoerrors.Newf(protoerrors.CodeScopeMismatch,
			"method %q: impersonation requires the admin scope claim", string(method))
	}

	// (4) The impersonated triple must be complete — identity is
	// mandatory; the impersonated triple IS identity (CLAUDE.md §6
	// rule 9). No "anonymous impersonation" mode.
	imp := bodyScope.Impersonating
	if err := identity.Validate(identity.Identity{
		TenantID:  imp.Tenant,
		UserID:    imp.User,
		SessionID: imp.Session,
	}); err != nil {
		return false, protoerrors.Newf(protoerrors.CodeIdentityRequired,
			"method %q: impersonation target identity incomplete: %v", string(method), err)
	}

	// (5) Actor + Requester must both be present and complete. They
	// are the audit anchor — a request without them cannot be
	// audited, so refuse.
	if bodyScope.Actor == nil {
		return false, protoerrors.Newf(protoerrors.CodeIdentityRequired,
			"method %q: impersonation requires the actor field", string(method))
	}
	if bodyScope.Requester == nil {
		return false, protoerrors.Newf(protoerrors.CodeIdentityRequired,
			"method %q: impersonation requires the requester field", string(method))
	}
	actor := bodyScope.Actor
	requester := bodyScope.Requester
	if err := identity.Validate(identity.Identity{
		TenantID:  actor.Tenant,
		UserID:    actor.User,
		SessionID: actor.Session,
	}); err != nil {
		return false, protoerrors.Newf(protoerrors.CodeIdentityRequired,
			"method %q: impersonation actor identity incomplete: %v", string(method), err)
	}
	if err := identity.Validate(identity.Identity{
		TenantID:  requester.Tenant,
		UserID:    requester.User,
		SessionID: requester.Session,
	}); err != nil {
		return false, protoerrors.Newf(protoerrors.CodeIdentityRequired,
			"method %q: impersonation requester identity incomplete: %v", string(method), err)
	}

	// (6) Actor MUST equal the verified JWT identity — the audit
	// trail's accountability stands or falls here. When the middleware
	// has run (production posture), we have the verified identity on
	// ctx and assert against it. When no middleware ran (test-only
	// posture WITHOUT WithoutValidator's escape hatch wired through),
	// we cannot verify the actor, so the impersonation gate refuses
	// fail-closed.
	authed, ok := identity.From(r.Context())
	if !ok {
		return false, protoerrors.Newf(protoerrors.CodeScopeMismatch,
			"method %q: impersonation requires a verified identity in context (auth middleware must run)", string(method))
	}
	if actor.Tenant != authed.TenantID || actor.User != authed.UserID || actor.Session != authed.SessionID {
		return false, protoerrors.Newf(protoerrors.CodeScopeMismatch,
			"method %q: impersonation actor does not match the verified JWT identity", string(method))
	}

	// (7) Requester == Actor at V1 (delegated impersonation is
	// post-V1 — the field is reserved for future use, not for V1
	// divergence).
	if requester.Tenant != actor.Tenant || requester.User != actor.User || requester.Session != actor.Session {
		return false, protoerrors.Newf(protoerrors.CodeScopeMismatch,
			"method %q: V1 invariant requires impersonation requester == actor (delegated impersonation is post-V1)", string(method))
	}

	// (8) The top-level Tenant/User/Session MUST equal the
	// Impersonating triple — the run executes as the impersonated
	// identity. A mismatch is a malformed shape (the caller asked to
	// impersonate user A but the body wants the run to execute as
	// user B); refuse closed.
	if bodyScope.Tenant != imp.Tenant || bodyScope.User != imp.User || bodyScope.Session != imp.Session {
		return false, protoerrors.Newf(protoerrors.CodeIdentityRequired,
			"method %q: impersonation requires the top-level identity to equal the impersonated triple", string(method))
	}

	return true, nil
}

// emitAdminScopeUsed publishes the typed
// `audit.admin_scope_used` event with an AdminScopeUsedPayload (
// ) onto the wired event bus. The payload runs through the wired
// audit.Redactor BEFORE the publish so any redaction rule that would
// rewrite a string field (e.g. a configurable PII rule) takes effect at
// the audit boundary, per CLAUDE.md §7 rule 6 ("every payload goes
// through audit.Redactor"; Audit owns redaction).
//
// The event's Identity is the IMPERSONATED triple so a Console
// subscribing to events for the impersonated session sees the audit
// emit alongside the run's own events. The Actor / Requester /
// Impersonating fields on the payload carry the full triplet for
// audit-side correlation. This shape is by-design redundant with the
// Identity quadruple — the payload is the source of truth for
// "who impersonated whom"; the event Identity is the bus-side scope.
//
// A publish failure is returned to the caller, which logs loudly but
// returns 200 to the client (the user-visible action already succeeded
// by the time emit runs). CLAUDE.md §5 "fail loudly" — never silent.
func (h *Handler) emitAdminScopeUsed(ctx context.Context, method methods.Method, req any) error {
	var scope types.IdentityScope
	switch v := req.(type) {
	case *types.StartRequest:
		scope = v.Identity
	case *types.ControlRequest:
		scope = v.Identity
	default:
		return fmt.Errorf("control: emitAdminScopeUsed called with unsupported request type %T", req)
	}
	if !scope.IsImpersonating() || scope.Actor == nil || scope.Requester == nil {
		return fmt.Errorf("control: emitAdminScopeUsed called with non-impersonation scope (gate ordering bug)")
	}

	// CLAUDE.md §7 rule 6 — run the impersonation fields
	// through the audit redactor BEFORE building the typed payload.
	// The redactor walks a flat `map[string]any` of the fields the
	// payload carries; we extract redacted strings back (with the
	// pre-redaction fallback) and assemble the typed
	// `AdminScopeUsedPayload`. This mirrors the audit
	// pattern in `internal/protocol/auth/auth.go::audit`: the
	// redactor IS run, but the published payload is the typed
	// `SafePayload` so subscribers see the canonical shape (no
	// `RedactedMap` ambiguity for an audit type the operator's
	// payload-shape contract explicitly carved out as flat).
	auditView := map[string]any{
		"actor_tenant":          scope.Actor.Tenant,
		"actor_user":            scope.Actor.User,
		"actor_session":         scope.Actor.Session,
		"requester_tenant":      scope.Requester.Tenant,
		"requester_user":        scope.Requester.User,
		"requester_session":     scope.Requester.Session,
		"impersonating_tenant":  scope.Impersonating.Tenant,
		"impersonating_user":    scope.Impersonating.User,
		"impersonating_session": scope.Impersonating.Session,
		"reason":                auth.AdminImpersonationReason,
		"method":                string(method),
	}
	redacted, err := h.redactor.Redact(ctx, auditView)
	if err != nil {
		// Fail loud — never emit unredacted. CLAUDE.md §13 forbids
		// silent fall-through to the unredacted payload.
		return fmt.Errorf("control: redactor refused admin_scope_used payload: %w", err)
	}
	redactedMap, ok := redacted.(map[string]any)
	if !ok {
		// Defensive: production redactors return a map (the
		// patterns driver's reflective walk shape). A redactor
		// returning a non-map shape on a map input is a contract
		// violation; refuse rather than emit a half-redacted view.
		return fmt.Errorf("control: redactor returned non-map shape %T on map input (cannot extract redacted fields)", redacted)
	}

	emitPayload := auth.AdminScopeUsedPayload{
		Actor: auth.IdentityTriple{
			Tenant:  redactedString(redactedMap, "actor_tenant", scope.Actor.Tenant),
			User:    redactedString(redactedMap, "actor_user", scope.Actor.User),
			Session: redactedString(redactedMap, "actor_session", scope.Actor.Session),
		},
		Requester: auth.IdentityTriple{
			Tenant:  redactedString(redactedMap, "requester_tenant", scope.Requester.Tenant),
			User:    redactedString(redactedMap, "requester_user", scope.Requester.User),
			Session: redactedString(redactedMap, "requester_session", scope.Requester.Session),
		},
		Impersonating: auth.IdentityTriple{
			Tenant:  redactedString(redactedMap, "impersonating_tenant", scope.Impersonating.Tenant),
			User:    redactedString(redactedMap, "impersonating_user", scope.Impersonating.User),
			Session: redactedString(redactedMap, "impersonating_session", scope.Impersonating.Session),
		},
		Reason: redactedString(redactedMap, "reason", auth.AdminImpersonationReason),
		Method: redactedString(redactedMap, "method", string(method)),
	}

	// Identity for the event: use the IMPERSONATED triple so a
	// Console subscribing to events for the impersonated session
	// sees the audit emit alongside the run's own events. The Actor
	// is on the payload for audit correlation.
	ev := events.Event{
		Type: events.EventTypeAdminScopeUsed,
		Identity: identity.Quadruple{
			Identity: identity.Identity{
				TenantID:  scope.Impersonating.Tenant,
				UserID:    scope.Impersonating.User,
				SessionID: scope.Impersonating.Session,
			},
		},
		OccurredAt: h.now(),
		Payload:    emitPayload,
	}
	if err := h.bus.Publish(ctx, ev); err != nil {
		return fmt.Errorf("control: publish admin_scope_used: %w", err)
	}
	return nil
}

// redactedString returns key's string value from the redactor's
// output when present (and a string); falls back to the original
// fallback value otherwise. Mirrors the pattern in
// `internal/protocol/auth/auth.go::redactedString`: production
// redactors usually pass through string values unchanged, but a
// custom redactor that rewrites a field returns the rewritten value
// here; an unexpected shape (non-string at the key) is treated as a
// pass-through to the pre-redaction value rather than crashing the
// emit.
func redactedString(red map[string]any, key, fallback string) string {
	if v, ok := red[key].(string); ok {
		return v
	}
	return fallback
}

// writeJSON encodes v as a JSON body with the given status. A marshal
// failure is logged and degraded to a bare 500 — never a partial body.
func (h *Handler) writeJSON(w http.ResponseWriter, r *http.Request, status int, v any) {
	buf, err := json.Marshal(v)
	if err != nil {
		h.logger.ErrorContext(r.Context(), "control transport: response marshal failed",
			slog.Int("status", status))
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if _, err := w.Write(buf); err != nil {
		// The client hung up mid-write — nothing recoverable, but log it
		// rather than swallowing silently.
		h.logger.DebugContext(r.Context(), "control transport: response write failed",
			slog.Int("status", status))
	}
}
