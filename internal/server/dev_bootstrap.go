// dev-only bootstrap endpoint.
//
// BootstrapHandler is mounted at POST /v1/dev/bootstrap.json on
// harbor dev and harbor console only. It mints a fresh dev token
// and returns the full connection envelope so the Console Settings
// page can offer a one-click "Attach to local Runtime" button.
//
// The endpoint is loopback-gated: only localhost peers receive a
// 200. Non-loopback peers get 403 regardless of headers. The gate
// reads r.RemoteAddr directly — no header-based spoofing vector
// exists.
//
// The endpoint additionally validates the request Host, so only a
// request addressed to a genuinely local authority (the literal name
// "localhost" or a loopback IP literal, with an optional port) is
// served, and it strips the CORS allow-headers from its response so
// the envelope stays readable to same-origin callers only — the
// posture the Console's one-click attach flow uses, which fetches it
// from window.location.origin.
//
// The endpoint is never registered by harbor serve.
package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/protocol/transports/cors"
)

// bootstrapIdentity is the wire shape of the identity triple in the
// bootstrap response. Local to the bootstrap endpoint so the canonical
// identity.Identity type stays JSON-tag-free (callers that need to
// marshal identity onto another surface make that choice explicitly).
type bootstrapIdentity struct {
	Tenant  string `json:"tenant"`
	User    string `json:"user"`
	Session string `json:"session"`
}

// BootstrapRequest is the OPTIONAL request body the bootstrap endpoint
// accepts. An empty body (the common `-d '{}'` case) mints the handler's
// default admin dev token, preserving the one-click Console-attach flow.
// A body that names a full identity triple and/or a scope set mints a
// token for THAT principal instead — the dev-only path that makes a
// lesser-privileged (non-admin) token obtainable so the non-admin
// authorization contract is exercisable end-to-end (and the live
// negative-escalation smoke can run).
//
// The override is gated by the same loopback boundary as the default
// mint: only a localhost peer ever reaches the signer, so a caller that
// can request a token for an arbitrary principal already controls the
// loopback dev surface. This is dev-only convenience — production
// (`harbor serve`) never mounts this endpoint.
type BootstrapRequest struct {
	// Tenant / User / Session override the minted token's identity
	// triple. All three must be non-empty to take effect; a partial
	// triple is rejected (identity is mandatory — CLAUDE.md §6). When
	// all three are empty, the handler's default identity is used.
	Tenant  string `json:"tenant"`
	User    string `json:"user"`
	Session string `json:"session"`
	// Scopes overrides the minted token's verified scope set. A nil
	// pointer (the field absent from the JSON) keeps the handler's
	// default scopes; a non-nil pointer — INCLUDING an explicit empty
	// array — replaces them, so `"scopes": []` mints a token with NO
	// scopes (the non-admin case). Each entry SHOULD be a canonical
	// auth.Scope string; unknown scopes are dropped from the verified
	// set at validation time (auth.WithScopes), never elevated.
	Scopes *[]string `json:"scopes"`
}

// BootstrapResponse is the JSON envelope returned by the bootstrap
// endpoint. It carries every field the Console's attachConnection
// helper needs for a one-click attach.
type BootstrapResponse struct {
	BaseURL         string            `json:"base_url"`
	Token           string            `json:"token"`
	Identity        bootstrapIdentity `json:"identity"`
	Scopes          []string          `json:"scopes"`
	ProtocolVersion string            `json:"protocol_version"`
}

// BootstrapSigner is the minimal token-signing surface the bootstrap
// handler needs. The harbor dev cmd's devSigner satisfies it.
type BootstrapSigner interface {
	SignDevToken(now time.Time, tenant, user, session string, scopes []string) (string, error)
}

// BootstrapHandler serves POST /v1/dev/bootstrap.json.
type BootstrapHandler struct {
	signer  BootstrapSigner
	id      identity.Identity
	scopes  []string
	baseURL string
	logger  *slog.Logger
}

// NewBootstrapHandler returns a handler wired for the dev identity
// triple and scope set. baseURL is the absolute URL the bootstrapping
// Console should target (populated from the actual listener address).
func NewBootstrapHandler(
	signer BootstrapSigner,
	id identity.Identity,
	scopes []string,
	baseURL string,
	logger *slog.Logger,
) *BootstrapHandler {
	return &BootstrapHandler{
		signer:  signer,
		id:      id,
		scopes:  scopes,
		baseURL: baseURL,
		logger:  logger,
	}
}

func (h *BootstrapHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// The bootstrap envelope is served to same-origin callers only, so
	// drop any CORS allow-headers an outer middleware set on this
	// response before the body is written. Response headers are still
	// mutable at this point — the CORS middleware sets them before
	// delegating and nothing has been written yet.
	stripCrossOriginExposure(w.Header())

	if r.Method != http.MethodPost {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusMethodNotAllowed)
		_ = json.NewEncoder(w).Encode(map[string]string{ //nolint:errcheck // best-effort response on error path; the operator's already getting a 405 status code, an Encode failure here is unrecoverable and noise on the log
			"code":    "method_not_allowed",
			"message": "bootstrap endpoint is POST-only",
		})
		return
	}

	if !isLoopback(r.RemoteAddr) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]string{ //nolint:errcheck // best-effort response on error path; 403 status code is the contract, body is informational
			"code":    "forbidden",
			"message": "bootstrap endpoint is loopback-only",
		})
		return
	}

	// The socket peer is local; require the request to be ADDRESSED to a
	// local authority too. A request that arrives over loopback carrying
	// an external Host is not a local caller's request and is refused.
	if !isLocalHostHeader(r.Host) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]string{ //nolint:errcheck // best-effort response on error path; 403 status code is the contract, body is informational
			"code":    "forbidden",
			"message": "bootstrap endpoint serves local hosts only",
		})
		return
	}

	// Resolve the identity + scopes to mint for. An empty / absent body
	// uses the handler defaults (the admin dev token); a body that names
	// a full triple and/or a scope set overrides them (the dev-only path
	// to a lesser-privileged token). A malformed body or a partial
	// identity triple fails closed with 400 — identity is mandatory.
	mintID, mintScopes, berr := h.resolveMint(r)
	if berr != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{ //nolint:errcheck // best-effort response on error path; 400 status code is the contract, body is informational
			"code":    "invalid_request",
			"message": berr.Error(),
		})
		return
	}

	token, err := h.signer.SignDevToken(time.Now(), mintID.TenantID, mintID.UserID, mintID.SessionID, mintScopes)
	if err != nil {
		h.logger.ErrorContext(r.Context(), "bootstrap: sign token failed",
			slog.String("err", err.Error()),
		)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{ //nolint:errcheck // best-effort response on error path; the underlying token-signing failure is already logged at Error severity
			"code":    "internal",
			"message": "token minting failed",
		})
		return
	}

	resp := BootstrapResponse{
		BaseURL: h.baseURL,
		Token:   token,
		Identity: bootstrapIdentity{
			Tenant:  mintID.TenantID,
			User:    mintID.UserID,
			Session: mintID.SessionID,
		},
		Scopes:          mintScopes,
		ProtocolVersion: "0.1.0",
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		// Status + headers are already committed; the client gets a
		// truncated body. Log it (the response can't be un-sent).
		h.logger.ErrorContext(r.Context(), "bootstrap: encode response failed",
			slog.String("err", err.Error()))
	}
}

// resolveMint reads the OPTIONAL request body and resolves the identity
// triple + scope set the token should be minted for. An empty body, or a
// body that names none of the override fields, yields the handler
// defaults (the admin dev token). A body that names a full triple
// overrides the identity; a body that carries a `scopes` array (even an
// empty one) overrides the scope set.
//
// It fails closed with an error (the caller maps it to 400) on a
// malformed body or a PARTIAL identity triple — identity is mandatory
// (CLAUDE.md §6 rule 9), there is no "tenant-only" dev token.
func (h *BootstrapHandler) resolveMint(r *http.Request) (identity.Identity, []string, error) {
	mintID := h.id
	mintScopes := h.scopes

	body, err := io.ReadAll(io.LimitReader(r.Body, maxBootstrapBodyBytes))
	if err != nil {
		return identity.Identity{}, nil, fmt.Errorf("bootstrap request body could not be read")
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return mintID, mintScopes, nil
	}

	var req BootstrapRequest
	if err := json.Unmarshal(trimmed, &req); err != nil {
		return identity.Identity{}, nil, fmt.Errorf("bootstrap request body is not valid JSON")
	}

	// Identity override: all-or-nothing. A partial triple fails closed.
	anyID := req.Tenant != "" || req.User != "" || req.Session != ""
	allID := req.Tenant != "" && req.User != "" && req.Session != ""
	switch {
	case allID:
		mintID = identity.Identity{TenantID: req.Tenant, UserID: req.User, SessionID: req.Session}
	case anyID:
		return identity.Identity{}, nil, fmt.Errorf("bootstrap identity override requires a full (tenant, user, session) triple")
	}

	// Scope override: a non-nil pointer (incl. an explicit empty array)
	// replaces the default scope set; nil keeps the default.
	if req.Scopes != nil {
		mintScopes = *req.Scopes
	}

	return mintID, mintScopes, nil
}

// maxBootstrapBodyBytes bounds the optional bootstrap request body. The
// override payload is a tiny identity-triple + scope-list JSON object; a
// generous 4 KiB cap fails closed on a client streaming an unbounded
// body at this dev-only endpoint.
const maxBootstrapBodyBytes = 4 << 10

// isLoopback parses the host from the remoteAddr (stripping the port)
// and returns true when the IP is a loopback address. Accepts
// 127.0.0.0/8 (IPv4) and ::1 (IPv6). No header is consulted — the
// function reads only the TCP peer address.
func isLoopback(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		// No port in the address — treat the whole string as host.
		host = remoteAddr
	}
	if host == "" {
		return false
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback()
}

// isLocalHostHeader reports whether an HTTP Host authority names a local
// address: the literal name "localhost" (case-insensitive) or a loopback
// IP literal (127.0.0.0/8, ::1), each with an optional port, with IPv6
// brackets tolerated, and with a single trailing root dot tolerated
// ("localhost." — the fully-qualified form a browser sends for
// `http://localhost./`). Any other authority — an external DNS name, for
// instance — returns false, so the endpoint answers only requests
// addressed to the local Runtime. An absent Host is refused (fail
// closed).
func isLocalHostHeader(hostHeader string) bool {
	if hostHeader == "" {
		return false
	}
	host := hostHeader
	if stripped, _, err := net.SplitHostPort(host); err == nil {
		host = stripped
	}
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	// A single trailing dot is the DNS root label; "localhost." and
	// "localhost" name the same host. Only one is stripped, so "localhost.."
	// stays invalid.
	host = strings.TrimSuffix(host, ".")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// stripCrossOriginExposure removes the CORS allow-headers that let a
// cross-origin script read a response. The bootstrap envelope is served
// same-origin only (the Console attaches from window.location.origin),
// so the endpoint drops the headers regardless of how permissive the
// operator's dev CORS allowlist is. `Vary: Origin` is deliberately left
// in place — it stays correct for shared caches.
func stripCrossOriginExposure(h http.Header) {
	h.Del(cors.HeaderAccessControlAllowOrigin)
	h.Del(cors.HeaderAccessControlAllowCredentials)
}
