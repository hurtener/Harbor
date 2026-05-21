// Package stream — Wave 13 additions (Phase 73m / D-129): the `auth.*`
// HTTP handler. Like `tools.*` and `tasks.*`, the single `auth.*`
// method (`auth.rotate_token`) is one-shot request/response — POST JSON
// in, JSON out — and the handler lives in the stream package because
// its identity + scope-claim gating reuses the same `resolveIdentity` /
// `auth.HasScope` machinery the subscription surface uses.
//
// Route shape:
//
//	POST /v1/auth/{method}
//
// where {method} is the canonical method's verb suffix:
//
//	rotate_token
//
// `auth.rotate_token` is the ONE net-new Protocol method Phase 73m
// ships — the Console Settings page is otherwise a pure consumer of the
// 72f / 72g posture surfaces. It is an ADMIN method: it rotates the
// operator's current Protocol-auth token and requires the verified
// `auth.ScopeAdmin` claim (D-079 closed two-scope set — there is NO
// `auth.admin` scope). A request without the claim is rejected 403 with
// the canonical CodeIdentityScopeRequired Code. Every successful
// rotation emits a redacted `audit.admin_scope_used` event through the
// shipped audit.Redactor (the emit lives in auth.RotateSurface — the
// handler never bypasses the audit redactor; CLAUDE.md §7 rule 6, §13).

package stream

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/hurtener/Harbor/internal/protocol/auth"
	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	prototypes "github.com/hurtener/Harbor/internal/protocol/types"
)

// AuthRoutePattern is the http.ServeMux pattern the auth handler
// registers under. The {method} wildcard is read via r.PathValue.
const AuthRoutePattern = "POST /v1/auth/{method}"

// maxAuthBodyBytes bounds the auth request body. The wire payload is
// tiny (just the identity scope); 16 KiB is comfortably over the
// realistic ceiling and fails closed on a client that streams an
// unbounded body.
const maxAuthBodyBytes = 16 << 10

// ErrAuthMisconfigured — NewAuthHandler was called with a nil
// auth.RotateSurface.
var ErrAuthMisconfigured = errors.New("stream: auth handler missing a mandatory dependency")

// AuthHandler serves `POST /v1/auth/{method}`. It is the wire adapter
// over an *auth.RotateSurface: resolve identity + scopes, decode the
// request, dispatch, encode. The handler is a D-025-safe compiled
// artifact — every field is set once at construction; ServeHTTP holds
// no per-request state.
type AuthHandler struct {
	rotate *auth.RotateSurface
	logger *slog.Logger
}

// AuthOption configures NewAuthHandler at construction.
type AuthOption func(*AuthHandler)

// WithAuthLogger sets the slog.Logger the handler logs decode /
// dispatch failures to. A nil logger (the default) routes to
// slog.Default().
func WithAuthLogger(l *slog.Logger) AuthOption {
	return func(h *AuthHandler) {
		if l != nil {
			h.logger = l
		}
	}
}

// NewAuthHandler builds the auth handler over an *auth.RotateSurface.
// surface is mandatory — a nil fails loud with ErrAuthMisconfigured
// rather than building a handler that would nil-panic on the first
// request (CLAUDE.md §5). The returned *AuthHandler is immutable after
// construction (D-025) and safe for concurrent use by N goroutines.
func NewAuthHandler(surface *auth.RotateSurface, opts ...AuthOption) (*AuthHandler, error) {
	if surface == nil {
		return nil, fmt.Errorf("%w: auth.RotateSurface is nil", ErrAuthMisconfigured)
	}
	h := &AuthHandler{
		rotate: surface,
		logger: slog.Default(),
	}
	for _, opt := range opts {
		opt(h)
	}
	return h, nil
}

// ServeHTTP implements http.Handler. It resolves identity + scopes,
// decodes the method-specific wire request, dispatches into the
// auth.RotateSurface, and encodes the response.
func (h *AuthHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeAuthError(w, protoerrors.CodeInvalidRequest, http.StatusMethodNotAllowed,
			"auth surface accepts POST only")
		return
	}

	// Resolve the canonical method from the {method} path verb.
	verb := r.PathValue("method")
	method := methods.Method("auth." + verb)
	if !methods.IsAuthMethod(method) {
		writeAuthError(w, protoerrors.CodeUnknownMethod, http.StatusNotFound,
			fmt.Sprintf("unknown auth method %q", verb))
		return
	}

	// Identity at the edge — RFC §5.5, CLAUDE.md §6 rule 9. A missing /
	// incomplete triple fails closed with CodeIdentityRequired (401).
	id, err := resolveIdentity(r)
	if err != nil {
		writeAuthError(w, protoerrors.CodeIdentityRequired, http.StatusUnauthorized,
			"identity scope incomplete: "+err.Error())
		return
	}

	// Decode the body — bounded.
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxAuthBodyBytes))
	if err != nil {
		writeAuthError(w, protoerrors.CodeInvalidRequest, http.StatusBadRequest,
			"failed to read request body: "+err.Error())
		return
	}

	// Reconstruct the verified-auth result from ctx. auth.Middleware
	// stashes the verified identity (identity.With) and the verified
	// scope set (auth.WithScopes); the RotateSurface consumes both.
	scopes, _ := auth.ScopesFrom(r.Context())
	verified := auth.Verified{Identity: id, Scopes: scopes}

	switch method {
	case methods.MethodAuthRotateToken:
		h.serveRotateToken(w, r, body, verified)
	default:
		// Unreachable — IsAuthMethod gated the switch above.
		writeAuthError(w, protoerrors.CodeUnknownMethod, http.StatusNotFound,
			"unknown auth method")
	}
}

func (h *AuthHandler) serveRotateToken(w http.ResponseWriter, r *http.Request, body []byte, verified auth.Verified) {
	var req prototypes.AuthRotateTokenRequest
	if len(body) > 0 {
		dec := json.NewDecoder(strings.NewReader(string(body)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			writeAuthError(w, protoerrors.CodeInvalidRequest, http.StatusBadRequest,
				"failed to decode auth.rotate_token request: "+err.Error())
			return
		}
	}
	resp, err := h.rotate.Rotate(r.Context(), verified, req)
	if err != nil {
		code, status, msg := classifyAuthError(err)
		if status >= http.StatusInternalServerError {
			h.logger.ErrorContext(r.Context(), "auth handler: rotate_token failed",
				slog.String("error", err.Error()))
		}
		writeAuthError(w, code, status, msg)
		return
	}
	writeAuthJSON(w, h.logger, r, resp)
}

// classifyAuthError maps an auth.RotateSurface sentinel error onto a
// canonical Protocol Code + HTTP status + safe operator-facing message.
func classifyAuthError(err error) (protoerrors.Code, int, string) {
	switch {
	case errors.Is(err, auth.ErrRotateIdentityRequired),
		errors.Is(err, auth.ErrRotateIdentityMismatch):
		return protoerrors.CodeIdentityRequired, http.StatusUnauthorized,
			"auth.rotate_token: identity scope incomplete or mismatched"
	case errors.Is(err, auth.ErrRotateScopeRequired):
		return protoerrors.CodeIdentityScopeRequired, http.StatusForbidden,
			"auth.rotate_token: requires the verified `admin` scope claim"
	case errors.Is(err, auth.ErrRotateMisconfigured):
		return protoerrors.CodeRuntimeError, http.StatusInternalServerError,
			"auth.rotate_token: token-rotation surface is misconfigured"
	default:
		// ErrRotateIssueFailed + any other runtime-side failure.
		return protoerrors.CodeRuntimeError, http.StatusInternalServerError,
			"auth.rotate_token: token rotation failed"
	}
}

// writeAuthJSON encodes resp as a 200 JSON body.
func writeAuthJSON(w http.ResponseWriter, logger *slog.Logger, r *http.Request, resp any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		logger.WarnContext(r.Context(), "auth handler: response encode failed",
			slog.String("error", err.Error()))
	}
}

// writeAuthError writes a JSON error body with the canonical Protocol
// Code + the supplied HTTP status. The body shape matches the REST
// control transport's error body so a client decodes both with the
// same Error wire type.
func writeAuthError(w http.ResponseWriter, code protoerrors.Code, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(&protoerrors.Error{
		Code:    code,
		Message: message,
	})
}
