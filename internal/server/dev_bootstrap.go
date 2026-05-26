// Phase 105 (V1.2) — dev-only bootstrap endpoint.
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
// The endpoint is never registered by harbor serve.
package server

import (
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
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
	signer BootstrapSigner
	id     identity.Identity
	scopes []string
	baseURL string
	logger *slog.Logger
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
	if r.Method != http.MethodPost {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusMethodNotAllowed)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"code":    "method_not_allowed",
			"message": "bootstrap endpoint is POST-only",
		})
		return
	}

	if !isLoopback(r.RemoteAddr) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"code":    "forbidden",
			"message": "bootstrap endpoint is loopback-only",
		})
		return
	}

	token, err := h.signer.SignDevToken(time.Now(), h.id.TenantID, h.id.UserID, h.id.SessionID, h.scopes)
	if err != nil {
		h.logger.ErrorContext(r.Context(), "bootstrap: sign token failed",
			slog.String("err", err.Error()),
		)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"code":    "internal",
			"message": "token minting failed",
		})
		return
	}

	resp := BootstrapResponse{
		BaseURL: h.baseURL,
		Token:   token,
		Identity: bootstrapIdentity{
			Tenant:  h.id.TenantID,
			User:    h.id.UserID,
			Session: h.id.SessionID,
		},
		Scopes:          h.scopes,
		ProtocolVersion: "0.1.0",
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}

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
