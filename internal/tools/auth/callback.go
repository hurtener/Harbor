package auth

// callback.go ships the production OAuth callback handler (
// ) — the completion leg of the tool-OAuth choreography.
// The pause PRODUCER has been live since then (the catalog OAuth
// wrapper routes a token-missing tool through buildAuthRequired →
// Coordinator.Request, emitting tool.auth_required and parking the
// run); CallbackHandler is the resume half: the provider redirect
// lands here, the handler exchanges (state, code) via
// Provider.CompleteFlow, the token persists, and the parked run's
// pause resumes with the typed DecisionResume marker (RFC §6.4 +
// §3.3). This closes the §13 primitive-without-consumer pair —
// InitiateFlow/CompleteFlow are the SpawnTask/AwaitTask of OAuth.
//
// # Identity: the flow record is the single source of truth
//
// The redirect arrives from the user's browser with NO Harbor JWT —
// the provider's authorization server cannot mint one. The handler
// therefore rebuilds the completing ctx's identity from the
// provider's OWN flow record (PendingFlow), pinned at initiation
// time. The unguessable 256-bit `state` nonce is the bearer
// capability (standard OAuth state semantics); possession proves the
// caller followed the authorize URL the runtime minted for exactly
// that identity. The handler adds zero identity logic of its own —
// it quotes the record back, and CompleteFlow's identity cross-check
// + the Coordinator's resume-scope check verify against the same
// record. For ScopeAgent flows the
// admin gate already fired at InitiateFlow time; the handler restores
// the control scope for the completion leg of that same flow only.
//
// # Secrets discipline (§7)
//
// The authorization `code` and any token bytes NEVER appear in a log
// line or a response body. The success page is static HTML; error
// bodies and log lines carry sentinel-derived static detail only (never
// err.Error(), which may embed untrusted upstream text). `state`
// is a one-time-use nonce, documented as non-secret (auth.go), and is
// safe to log for correlation.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/runtime/registry"
)

// CallbackPath is the documented default mount path for the OAuth
// callback handler — the path `harbor dev` serves and the shape
// operators put in OAuthConfig.RedirectURI
// (`http://<bind>/v1/tools/oauth/callback`). Headless embedders may
// mount the handler at any path that matches their configured
// RedirectURI.
const CallbackPath = "/v1/tools/oauth/callback"

// CallbackRoutePattern is the method-qualified http.ServeMux pattern
// for the default mount (`GET /v1/tools/oauth/callback`).
const CallbackRoutePattern = "GET " + CallbackPath

// defaultSuccessPage is the minimal operator-facing completion page.
// Static — no token material, no flow material, ever.
const defaultSuccessPage = `<!DOCTYPE html>
<html lang="en">
<head><meta charset="utf-8"><title>Harbor — authorization complete</title></head>
<body>
<h1>Authorization complete</h1>
<p>Harbor has received the authorization and resumed your session. You can close this tab and return to your session.</p>
</body>
</html>
`

// callbackConfig is the option-applied construction config.
type callbackConfig struct {
	logger      *slog.Logger
	successPage string
}

// CallbackOption configures CallbackHandler at construction.
type CallbackOption func(*callbackConfig)

// WithCallbackLogger sets the slog.Logger the handler logs through.
// Defaults to slog.Default(). Log lines carry identity attributes +
// the provider/source/binding-scope and the (non-secret) state nonce;
// never the authorization code or token bytes (§7).
func WithCallbackLogger(l *slog.Logger) CallbackOption {
	return func(c *callbackConfig) {
		if l != nil {
			c.logger = l
		}
	}
}

// WithSuccessPage overrides the static HTML success page. The page is
// served verbatim on every successful completion — callers MUST NOT
// template flow or token material into it.
func WithSuccessPage(html string) CallbackOption {
	return func(c *callbackConfig) {
		if html != "" {
			c.successPage = html
		}
	}
}

// CallbackHandler returns the http.Handler that receives the OAuth
// provider redirect and completes the tool-OAuth flow: it parses
// (state, code, error) from the query, locates the owning provider
// across the supplied map via PendingFlow, rebuilds the completing
// identity from the provider's flow record, and calls CompleteFlow —
// which exchanges the code, persists the token, and resumes the
// parked pause record through the unified pause/resume primitive.
//
// Typed error mapping (sentinel → HTTP status):
//
//	ErrFlowNotFound                                  → 404
//	ErrFlowExpired                                   → 410
//	ErrStateMismatch                                 → 400
//	ErrExchangeFailed / ErrDiscoveryFailed /
//	ErrRegistrationFailed (provider-upstream)        → 502
//	ErrProviderClosed                                → 503
//	missing state / missing code / upstream `error`  → 400
//	anything else                                    → 500
//
// An upstream denial (`error=access_denied`, …) is surfaced loud:
// 400 with the audit-safe reason, AND the flow is consumed via
// DenyFlow so the pause resumes with DecisionReject instead of
// hanging to flow-TTL.
//
// The handler is a plain http.Handler with no dependency on the
// Protocol server, the dev server, or cmd/harbor — `harbor dev`
// mounts it at CallbackRoutePattern; a headless embedder mounts it on
// any mux at the path matching its configured RedirectURI. With the
// production StateStore checkpoint wiring, both the unified pause and
// the sealed OAuth correlation record survive a Runtime restart; the
// callback may land on the rebuilt provider and resume that pause.
//
// Concurrent reuse: the handler is a compiled artifact — the
// provider map is copied at construction and read-only afterwards;
// per-request state lives on the request goroutine's stack.
func CallbackHandler(providers map[string]OAuthProvider, opts ...CallbackOption) http.Handler {
	cfg := callbackConfig{
		logger:      slog.Default(),
		successPage: defaultSuccessPage,
	}
	for _, opt := range opts {
		if opt != nil {
			opt(&cfg)
		}
	}
	m := make(map[string]OAuthProvider, len(providers))
	for name, p := range providers {
		if p != nil {
			m[name] = p
		}
	}
	return &callbackHandler{providers: m, cfg: cfg}
}

// callbackHandler is the concrete http.Handler. Immutable after
// construction.
type callbackHandler struct {
	providers map[string]OAuthProvider
	cfg       callbackConfig
}

// ServeHTTP implements http.Handler.
func (h *callbackHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		h.writeError(w, http.StatusMethodNotAllowed, "method_not_allowed",
			"the OAuth callback accepts GET only")
		return
	}
	q := r.URL.Query()
	state := q.Get("state")
	if state == "" {
		h.writeError(w, http.StatusBadRequest, "invalid_request",
			"missing state parameter")
		return
	}

	name, prov, info, ok, err := h.lookup(r.Context(), state)
	if err != nil {
		h.writeFlowError(w, h.cfg.logger, err)
		return
	}
	if !ok {
		// Covers garbage states, denied flows, and completed-flow states
		// outside their bounded callback retry horizon. Successful
		// completions retain a sealed routing tombstone through that horizon
		// and therefore take the idempotent success path above.
		h.writeError(w, http.StatusNotFound, "flow_not_found",
			"no in-flight OAuth flow matches the supplied state (it may have already been completed)")
		return
	}

	// Rebuild the completing ctx's identity from the provider's own
	// flow record — the single source of truth for the binding (see
	// the file-top doc). For ScopeAgent flows, restore the control
	// scope the InitiateFlow admin gate already verified.
	ctx, err := identity.With(r.Context(), info.Identity)
	if err != nil {
		// Impossible by construction — the record's identity was
		// validated at initiation. Fail loud, not silent (§13).
		h.cfg.logger.ErrorContext(r.Context(), "oauth callback: flow record identity invalid",
			slog.String("provider", name),
			slog.String("error", err.Error()))
		h.writeError(w, http.StatusInternalServerError, "internal_error",
			"the flow record's identity could not be restored")
		return
	}
	if info.BindingScope == ScopeAgent {
		ctx = registry.WithControlScope(ctx)
	}
	logger := h.cfg.logger.With(
		slog.String("tenant_id", info.Identity.TenantID),
		slog.String("user_id", info.Identity.UserID),
		slog.String("session_id", info.Identity.SessionID),
		slog.String("provider", name),
		slog.String("source", string(info.Source)),
		slog.String("binding_scope", string(info.BindingScope)),
		slog.String("state", state),
	)

	// Upstream denial (RFC 6749 §4.1.2.1): surface loud + consume the
	// flow so the pause resumes with DecisionReject instead
	// of hanging to flow-TTL.
	if upstreamErr := q.Get("error"); upstreamErr != "" {
		denialClass := classifyOAuthDenial(upstreamErr)
		if derr := prov.DenyFlow(ctx, state, string(denialClass)); derr != nil {
			h.writeFlowError(w, logger, derr)
			return
		}
		logger.WarnContext(ctx, "oauth callback: authorization denied upstream; flow rejected",
			slog.String("denial_class", string(denialClass)))
		h.writeError(w, http.StatusBadRequest, "authorization_denied", string(denialClass))
		return
	}

	code := q.Get("code")
	if code == "" {
		h.writeError(w, http.StatusBadRequest, "invalid_request",
			"missing code parameter")
		return
	}

	if _, cerr := prov.CompleteFlow(ctx, state, code); cerr != nil {
		h.writeFlowError(w, logger, cerr)
		return
	}

	logger.InfoContext(ctx, "oauth callback: flow completed; token persisted; parked run resumed")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	//nolint:errcheck // operator-facing success page; a failed write is non-actionable (the flow already completed)
	_, _ = w.Write([]byte(h.cfg.successPage))
}

// lookup locates the provider owning `state` across the map. States
// are unguessable 256-bit nonces minted per-provider, so at most one
// provider claims a given state — flow A's completion can never reach
// flow B's record (asserted by the concurrent-reuse test).
func (h *callbackHandler) lookup(ctx context.Context, state string) (string, OAuthProvider, PendingFlowInfo, bool, error) {
	for name, prov := range h.providers {
		info, ok, err := prov.PendingFlow(ctx, state)
		if err != nil {
			return "", nil, PendingFlowInfo{}, false, fmt.Errorf("oauth callback: lookup provider %q: %w", name, err)
		}
		if ok {
			return name, prov, info, true, nil
		}
	}
	return "", nil, PendingFlowInfo{}, false, nil
}

// writeFlowError maps the package's typed sentinels onto HTTP
// statuses and writes the JSON error body. Logs and response bodies carry only
// the sentinel-derived status and code: upstream servers may reflect bearer,
// authorization-code, or client-secret material in their error text.
func (h *callbackHandler) writeFlowError(w http.ResponseWriter, logger *slog.Logger, err error) {
	var (
		status int
		code   string
		detail string
	)
	switch {
	case errors.Is(err, ErrFlowNotFound):
		status, code = http.StatusNotFound, "flow_not_found"
		detail = "no in-flight OAuth flow matches the supplied state (it may have already been completed)"
	case errors.Is(err, ErrFlowInProgress):
		status, code = http.StatusConflict, "flow_in_progress"
		detail = "the OAuth flow callback is already being completed; retry shortly"
	case errors.Is(err, ErrFlowExpired):
		status, code = http.StatusGone, "flow_expired"
		detail = "the OAuth flow expired before the callback arrived; re-initiate the authorization"
	case errors.Is(err, ErrStateMismatch):
		status, code = http.StatusBadRequest, "state_mismatch"
		detail = "the supplied state does not match the initiating flow's identity"
	case errors.Is(err, ErrExchangeFailed), errors.Is(err, ErrDiscoveryFailed), errors.Is(err, ErrRegistrationFailed):
		status, code = http.StatusBadGateway, "exchange_failed"
		detail = "the authorization server rejected the token exchange"
	case errors.Is(err, ErrProviderClosed):
		status, code = http.StatusServiceUnavailable, "provider_closed"
		detail = "the OAuth provider is shut down"
	default:
		status, code = http.StatusInternalServerError, "internal_error"
		detail = "the OAuth flow could not be completed"
	}
	logger.Error("oauth callback: flow completion failed",
		slog.Int("status", status),
		slog.String("code", code))
	h.writeError(w, status, code, detail)
}

// writeError writes the typed JSON error body. The body is composed
// from static, audit-safe strings only — JSON-escaped by hand-shape
// (no fields are caller-controlled except the OAuth `error` code,
// which is escaped via the encoder below).
func (h *callbackHandler) writeError(w http.ResponseWriter, status int, code, detail string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	//nolint:errcheck // error-response write; a failure is non-actionable and the status is already committed
	_, _ = w.Write([]byte(`{"error":` + jsonString(code) + `,"detail":` + jsonString(detail) + `}` + "\n"))
}

// jsonString minimally JSON-encodes s as a string literal. The inputs
// are static sentinel-derived strings plus the upstream OAuth `error`
// code; encoding defensively keeps a hostile redirect from breaking
// the body shape.
func jsonString(s string) string {
	buf := make([]byte, 0, len(s)+2)
	buf = append(buf, '"')
	for _, r := range s {
		switch r {
		case '"':
			buf = append(buf, '\\', '"')
		case '\\':
			buf = append(buf, '\\', '\\')
		case '\n':
			buf = append(buf, '\\', 'n')
		case '\r':
			buf = append(buf, '\\', 'r')
		case '\t':
			buf = append(buf, '\\', 't')
		default:
			if r < 0x20 {
				const hex = "0123456789abcdef"
				buf = append(buf, '\\', 'u', '0', '0', hex[r>>4], hex[r&0xf])
			} else {
				buf = append(buf, []byte(string(r))...)
			}
		}
	}
	buf = append(buf, '"')
	return string(buf)
}
