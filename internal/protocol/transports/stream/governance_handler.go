// Package stream — addition: the admin-scoped `governance.*`
// tenant-default override HTTP handler. Like the Agents / Runs page
// handlers, this is a one-shot request/response endpoint — POST JSON in,
// JSON out — sharing the same `resolveIdentity` + defence-in-depth
// body-identity machinery.
//
// Route shape (POST):
//
//	POST /v1/governance/set_tenant_overrides — set/clear a tenant default
//	POST /v1/governance/get_tenant_overrides — read a tenant default
//
// Both routes are identity-mandatory AND admin-scoped: they MUTATE / read
// the tenant control plane, so the verified `auth.ScopeAdmin` claim is
// required. A non-admin caller is rejected with CodeScopeMismatch (403)
// before the request reaches the service. The tenant whose defaults are
// set/read is the caller's verified tenant (the handler overlays the
// verified identity onto the request body).
//
// GovernanceHandler is a concurrency-safe compiled artifact — service /
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

	"github.com/hurtener/Harbor/internal/governance"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/llm"
	"github.com/hurtener/Harbor/internal/protocol/auth"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
	governanceprotocol "github.com/hurtener/Harbor/internal/runtime/governance/protocol"
)

// GovernanceRoutePattern is the http.ServeMux prefix pattern the
// governance admin handler registers under. The handler internally
// branches on the trailing path segment to dispatch.
const GovernanceRoutePattern = "POST /v1/governance/"

// maxGovernanceBodyBytes bounds a governance admin request body. The wire
// payload is small (an identity scope + five optional override fields, one
// of them an extra-instructions string); 256 KiB is comfortably over the
// realistic ceiling — extra-instructions are bounded text, never heavy
// content — and fails closed on a client that streams an unbounded body.
const maxGovernanceBodyBytes = 256 << 10

// ErrGovernanceMisconfigured — NewGovernanceHandler was called with a nil
// service.
var ErrGovernanceMisconfigured = errors.New("stream: governance handler missing a mandatory dependency")

// GovernanceHandler serves the `POST /v1/governance/*` admin routes. It is
// the wire adapter over a *governanceprotocol.Service: resolve identity,
// enforce the admin scope, branch on the trailing path segment, decode,
// dispatch, encode.
type GovernanceHandler struct {
	service      *governanceprotocol.Service
	keyRotate    *governanceprotocol.KeyRotateService    // optional — nil ⇒ rotate_key route 501s
	postureWrite *governanceprotocol.PostureWriteService // optional — nil ⇒ set_posture route 501s
	logger       *slog.Logger
}

// GovernanceOption configures NewGovernanceHandler.
type GovernanceOption func(*GovernanceHandler)

// WithGovernanceLogger sets the slog.Logger. A nil logger (the default)
// routes to slog.Default().
func WithGovernanceLogger(l *slog.Logger) GovernanceOption {
	return func(h *GovernanceHandler) {
		if l != nil {
			h.logger = l
		}
	}
}

// WithGovernanceKeyRotate wires the rotate-key service so the
// `POST /v1/governance/rotate_key` route is live. A nil service (the
// default) leaves the route returning CodeUnknownMethod (501) — the
// partial-build convention.
func WithGovernanceKeyRotate(s *governanceprotocol.KeyRotateService) GovernanceOption {
	return func(h *GovernanceHandler) {
		if s != nil {
			h.keyRotate = s
		}
	}
}

// WithGovernancePostureWrite wires the set-posture service so the
// `POST /v1/governance/set_posture` route is live (the admin identity-tier
// policy WRITE surface). A nil service (the default) leaves the route
// returning 501 — the partial-build convention. When supplied AND
// WithValidator is set, the route inherits the same admin gate as the rest
// of `/v1/governance/*` (auth.ScopeAdmin ONLY).
func WithGovernancePostureWrite(s *governanceprotocol.PostureWriteService) GovernanceOption {
	return func(h *GovernanceHandler) {
		if s != nil {
			h.postureWrite = s
		}
	}
}

// NewGovernanceHandler builds the governance admin handler over a
// *governanceprotocol.Service. service is mandatory — a nil fails loud
// with ErrGovernanceMisconfigured (CLAUDE.md §5).
//
// The returned *GovernanceHandler is immutable after construction and safe
// for concurrent use by N goroutines.
func NewGovernanceHandler(service *governanceprotocol.Service, opts ...GovernanceOption) (*GovernanceHandler, error) {
	if service == nil {
		return nil, fmt.Errorf("%w: governance/protocol.Service is nil", ErrGovernanceMisconfigured)
	}
	h := &GovernanceHandler{service: service, logger: slog.Default()}
	for _, opt := range opts {
		opt(h)
	}
	return h, nil
}

// ServeHTTP implements http.Handler.
func (h *GovernanceHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeGovernanceError(w, protoerrors.CodeInvalidRequest, http.StatusMethodNotAllowed,
			"governance endpoints accept POST only")
		return
	}
	id, err := resolveIdentity(r)
	if err != nil {
		writeGovernanceError(w, protoerrors.CodeIdentityRequired, http.StatusUnauthorized,
			"identity scope incomplete: "+err.Error())
		return
	}
	// Admin gate: these verbs read/mutate the tenant control plane and
	// require the verified `auth.ScopeAdmin` claim — authority derives
	// from the verified ctx, never the request body. A non-admin caller
	// is rejected before any dispatch.
	if !auth.HasScope(r.Context(), auth.ScopeAdmin) {
		writeGovernanceError(w, protoerrors.CodeScopeMismatch, http.StatusForbidden,
			"governance tenant-override methods require the admin scope claim")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxGovernanceBodyBytes))
	if err != nil {
		writeGovernanceError(w, protoerrors.CodeInvalidRequest, http.StatusBadRequest,
			"failed to read request body: "+err.Error())
		return
	}
	wireID := prototypes.IdentityScope{Tenant: id.TenantID, User: id.UserID, Session: id.SessionID}

	switch strings.TrimPrefix(r.URL.Path, "/v1/governance/") {
	case "set_tenant_overrides":
		h.serveSet(w, r, body, wireID)
	case "get_tenant_overrides":
		h.serveGet(w, r, body, wireID)
	case "rotate_key":
		h.serveRotateKey(w, r, body, wireID)
	case "set_posture":
		h.serveSetPosture(w, r, body, wireID)
	default:
		writeGovernanceError(w, protoerrors.CodeUnknownMethod, http.StatusNotFound,
			"unknown governance method route")
	}
}

// serveRotateKey handles `governance.rotate_key`. The request carries the
// new key as a SECRET on the body — it is decoded, passed straight to the
// service (which hands it to the atomic key holder), and never logged or
// echoed. The route is admin-gated above.
func (h *GovernanceHandler) serveRotateKey(w http.ResponseWriter, r *http.Request, body []byte, wireID prototypes.IdentityScope) {
	if h.keyRotate == nil {
		writeGovernanceError(w, protoerrors.CodeUnknownMethod, http.StatusNotImplemented,
			"governance.rotate_key is not wired on this runtime")
		return
	}
	var req prototypes.GovernanceRotateKeyRequest
	if err := decodeGovernanceBody(body, &req); err != nil {
		writeGovernanceError(w, protoerrors.CodeInvalidRequest, http.StatusBadRequest,
			"failed to decode governance.rotate_key request: "+err.Error())
		return
	}
	if perr := assertGovernanceIdentity(req.Identity, wireID); perr != "" {
		writeGovernanceError(w, protoerrors.CodeIdentityRequired, http.StatusUnauthorized, perr)
		return
	}
	req.Identity = wireID
	resp, err := h.keyRotate.RotateKey(r.Context(), req)
	if err != nil {
		h.writeServiceError(w, r, methods.MethodGovernanceRotateKey, err)
		return
	}
	writeGovernanceJSON(w, r, resp, h.logger)
}

// serveSetPosture handles `governance.set_posture`. The request carries the
// WHOLE identity-tier policy table (a full replace) and NO identity field —
// authority is server-side: the verified identity resolved above is
// the admin actor. The route is admin-gated (auth.ScopeAdmin ONLY) at the
// top of ServeHTTP. A widening / zeroing / empty write is rejected
// fail-closed (CodeInvalidRequest) by the policy before any state mutation.
func (h *GovernanceHandler) serveSetPosture(w http.ResponseWriter, r *http.Request, body []byte, wireID prototypes.IdentityScope) {
	if h.postureWrite == nil {
		writeGovernanceError(w, protoerrors.CodeUnknownMethod, http.StatusNotImplemented,
			"governance.set_posture is not wired on this runtime")
		return
	}
	var req prototypes.GovernanceSetPostureRequest
	if err := decodeGovernanceBody(body, &req); err != nil {
		writeGovernanceError(w, protoerrors.CodeInvalidRequest, http.StatusBadRequest,
			"failed to decode governance.set_posture request: "+err.Error())
		return
	}
	actor := identity.Quadruple{Identity: identity.Identity{
		TenantID:  wireID.Tenant,
		UserID:    wireID.User,
		SessionID: wireID.Session,
	}}
	resp, err := h.postureWrite.SetPosture(r.Context(), actor, req)
	if err != nil {
		h.writeServiceError(w, r, methods.MethodGovernanceSetPosture, err)
		return
	}
	writeGovernanceJSON(w, r, resp, h.logger)
}

func (h *GovernanceHandler) serveSet(w http.ResponseWriter, r *http.Request, body []byte, wireID prototypes.IdentityScope) {
	var req prototypes.GovernanceSetTenantOverridesRequest
	if err := decodeGovernanceBody(body, &req); err != nil {
		writeGovernanceError(w, protoerrors.CodeInvalidRequest, http.StatusBadRequest,
			"failed to decode governance.set_tenant_overrides request: "+err.Error())
		return
	}
	if perr := assertGovernanceIdentity(req.Identity, wireID); perr != "" {
		writeGovernanceError(w, protoerrors.CodeIdentityRequired, http.StatusUnauthorized, perr)
		return
	}
	req.Identity = wireID
	resp, err := h.service.SetTenantOverrides(r.Context(), req)
	if err != nil {
		h.writeServiceError(w, r, methods.MethodGovernanceSetTenantOverrides, err)
		return
	}
	writeGovernanceJSON(w, r, resp, h.logger)
}

func (h *GovernanceHandler) serveGet(w http.ResponseWriter, r *http.Request, body []byte, wireID prototypes.IdentityScope) {
	var req prototypes.GovernanceGetTenantOverridesRequest
	if err := decodeGovernanceBody(body, &req); err != nil {
		writeGovernanceError(w, protoerrors.CodeInvalidRequest, http.StatusBadRequest,
			"failed to decode governance.get_tenant_overrides request: "+err.Error())
		return
	}
	if perr := assertGovernanceIdentity(req.Identity, wireID); perr != "" {
		writeGovernanceError(w, protoerrors.CodeIdentityRequired, http.StatusUnauthorized, perr)
		return
	}
	req.Identity = wireID
	resp, err := h.service.GetTenantOverrides(r.Context(), req)
	if err != nil {
		h.writeServiceError(w, r, methods.MethodGovernanceGetTenantOverrides, err)
		return
	}
	writeGovernanceJSON(w, r, resp, h.logger)
}

// decodeGovernanceBody decodes the JSON body into req, rejecting unknown
// fields. An empty body decodes to the zero value.
func decodeGovernanceBody(body []byte, req any) error {
	if len(body) == 0 {
		return nil
	}
	dec := json.NewDecoder(bytesReader(body))
	dec.DisallowUnknownFields()
	return dec.Decode(req)
}

// assertGovernanceIdentity is the defence-in-depth check: when the request
// body carries an identity, every non-empty component MUST match the
// verified identity. Returns "" when consistent (or empty).
func assertGovernanceIdentity(body, verified prototypes.IdentityScope) string {
	if (body.Tenant != "" && body.Tenant != verified.Tenant) ||
		(body.User != "" && body.User != verified.User) ||
		(body.Session != "" && body.Session != verified.Session) {
		return "body identity scope does not match the verified identity"
	}
	return ""
}

// writeServiceError maps a service error onto a canonical Protocol Code +
// HTTP status + safe operator-facing message.
func (h *GovernanceHandler) writeServiceError(w http.ResponseWriter, r *http.Request, method methods.Method, err error) {
	code, status, msg := classifyGovernanceError(method, err)
	if status >= http.StatusInternalServerError {
		h.logger.ErrorContext(r.Context(), "governance handler: dispatch failed",
			slog.String("method", string(method)), slog.String("error", err.Error()))
	}
	writeGovernanceError(w, code, status, msg)
}

// classifyGovernanceError maps a service/policy error onto the canonical
// Protocol Code + HTTP status. The single place the governance wire
// surface translates a Go error into a Protocol error.
func classifyGovernanceError(method methods.Method, err error) (protoerrors.Code, int, string) {
	m := string(method)
	switch {
	case errors.Is(err, governanceprotocol.ErrIdentityRequired), errors.Is(err, governance.ErrIdentityRequired):
		return protoerrors.CodeIdentityRequired, http.StatusUnauthorized,
			m + ": identity scope incomplete"
	case errors.Is(err, governance.ErrUnknownModel):
		return protoerrors.CodeInvalidRequest, http.StatusBadRequest,
			m + ": " + err.Error()
	case errors.Is(err, governance.ErrInvalidOverride):
		return protoerrors.CodeInvalidRequest, http.StatusBadRequest,
			m + ": " + err.Error()
	case errors.Is(err, governance.ErrPolicyWidening):
		// Fail-closed reject of a ceiling-omitting / zeroing write — the
		// phase's real fail-loud path. 400: the submitted table is invalid
		// (never budget-widening), not a runtime fault.
		return protoerrors.CodeInvalidRequest, http.StatusBadRequest,
			m + ": " + err.Error()
	case errors.Is(err, governance.ErrInvalidPosture):
		return protoerrors.CodeInvalidRequest, http.StatusBadRequest,
			m + ": " + err.Error()
	case errors.Is(err, llm.ErrEmptyKey):
		return protoerrors.CodeInvalidRequest, http.StatusBadRequest,
			m + ": rotate key value is empty"
	case errors.Is(err, llm.ErrUnknownProvider):
		return protoerrors.CodeInvalidRequest, http.StatusBadRequest,
			m + ": provider is not the configured provider"
	case errors.Is(err, governance.ErrClosed):
		return protoerrors.CodeRuntimeError, http.StatusServiceUnavailable,
			m + ": governance subsystem closed"
	case errors.Is(err, governance.ErrStateUnavailable):
		return protoerrors.CodeRuntimeError, http.StatusInternalServerError,
			m + ": state store unavailable"
	default:
		return protoerrors.CodeRuntimeError, http.StatusInternalServerError,
			m + ": request failed"
	}
}

// writeGovernanceJSON encodes a successful response.
func writeGovernanceJSON(w http.ResponseWriter, r *http.Request, v any, logger *slog.Logger) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		logger.WarnContext(r.Context(), "governance handler: response encode failed",
			slog.String("error", err.Error()))
	}
}

// writeGovernanceError writes a JSON error body with the canonical
// Protocol Code + the supplied HTTP status.
func writeGovernanceError(w http.ResponseWriter, code protoerrors.Code, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(&protoerrors.Error{Code: code, Message: message}) //nolint:errcheck // response status already committed — a write error cannot be recovered here.
}
