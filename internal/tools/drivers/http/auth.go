// Package http is Harbor's HTTP transport driver for the unified
// tool catalog (Phase 27). It ships two registration paths — inline
// `RegisterHTTPTool(...)` for the dev loop and a UTCP-style YAML
// manifest loader for operator deployments — converging on the same
// `tools.ToolDescriptor` shape (Transport = TransportHTTP). Every
// invocation runs through the Phase 26 `ToolPolicy` reliability shell
// (D-024); the driver itself does NOT loop independently.
//
// Static auth (API key, bearer, cookie) is supported via the
// `AuthSpec` value plus a secret loaded from operator-supplied
// config; secrets MUST NOT live in URL templates or request payloads
// (AGENTS.md §7). OAuth / token-exchange flows are deferred to
// Phase 30 via the unified pause/resume primitive.
//
// Concurrent reuse (D-025): every HTTP `ToolDescriptor` is safe under
// N concurrent invocations against the same underlying `*http.Client`
// because (a) per-invocation state lives on the goroutine stack
// (request value, response value, attempt context, classifier), and
// (b) the descriptor's compiled `text/template`s + cached schema
// validators are read-only after construction.
package http

import (
	"errors"
	"fmt"
	nethttp "net/http"
	"strings"
)

// AuthKind is the static-auth discriminator. Phase 27 ships three
// values; OAuth and token-exchange land in Phase 30.
type AuthKind string

const (
	// AuthKindNone — no auth applied (default). Useful for public
	// endpoints (e.g. weather APIs that take an API key as a query
	// parameter via AuthKindAPIKey + QueryParam, or completely open
	// services).
	AuthKindNone AuthKind = ""
	// AuthKindAPIKey — secret placed in either a header (when
	// HeaderName is set) or a query parameter (when QueryParam is
	// set). Exactly one MUST be set.
	AuthKindAPIKey AuthKind = "api_key"
	// AuthKindBearer — RFC 6750 bearer token. Sets the
	// "Authorization: Bearer <secret>" header.
	AuthKindBearer AuthKind = "bearer"
	// AuthKindCookie — secret placed in the named cookie.
	AuthKindCookie AuthKind = "cookie"
)

// AuthSpec configures static authentication for an HTTP tool. The
// shape is value-typed (no pointers) so a tool descriptor that
// declares auth at construction has it baked-in for every invocation;
// the secret value lives separately in `httpToolConfig.secret` so the
// AuthSpec itself stays loggable / printable.
//
// The Kind drives which of the optional fields is consulted:
//
//   - AuthKindAPIKey: HeaderName XOR QueryParam (exactly one).
//   - AuthKindBearer: no extra fields.
//   - AuthKindCookie: CookieName (mandatory).
type AuthSpec struct {
	Kind       AuthKind
	HeaderName string
	QueryParam string
	CookieName string
}

// Validate reports whether the spec is internally consistent.
// Returns nil for AuthKindNone (no auth applied). Returns
// ErrAuthInvalidSpec wrapped with the offending detail otherwise.
func (s AuthSpec) Validate() error {
	switch s.Kind {
	case AuthKindNone:
		return nil
	case AuthKindAPIKey:
		hdr := strings.TrimSpace(s.HeaderName)
		qry := strings.TrimSpace(s.QueryParam)
		switch {
		case hdr == "" && qry == "":
			return fmt.Errorf("%w: api_key requires HeaderName XOR QueryParam", ErrAuthInvalidSpec)
		case hdr != "" && qry != "":
			return fmt.Errorf("%w: api_key got both HeaderName and QueryParam (set exactly one)", ErrAuthInvalidSpec)
		}
		return nil
	case AuthKindBearer:
		return nil
	case AuthKindCookie:
		if strings.TrimSpace(s.CookieName) == "" {
			return fmt.Errorf("%w: cookie requires CookieName", ErrAuthInvalidSpec)
		}
		return nil
	default:
		return fmt.Errorf("%w: unknown kind %q", ErrAuthInvalidSpec, s.Kind)
	}
}

// Sentinel auth errors.
var (
	// ErrAuthMissing — auth spec declares a secret-bearing kind but
	// the supplied secret is empty. Surfaced loudly at register-time
	// so misconfiguration is caught at boot, not at first invocation.
	ErrAuthMissing = errors.New("http: auth secret missing or empty")
	// ErrAuthInvalidSpec — AuthSpec internally inconsistent (e.g.
	// api_key with both HeaderName and QueryParam set).
	ErrAuthInvalidSpec = errors.New("http: auth spec invalid")
)

// applyAuth stamps the auth spec onto req using secret. Returns
// ErrAuthMissing when the spec requires a secret but secret == "".
// AuthKindNone is a no-op.
//
// Order: query-param mutations happen on the URL BEFORE headers /
// cookies are applied so the URL is finalised by the time the
// transport reads it. The function never mutates secret-bearing
// state beyond what the spec asks for.
func applyAuth(req *nethttp.Request, spec AuthSpec, secret string) error {
	if spec.Kind == AuthKindNone {
		return nil
	}
	if strings.TrimSpace(secret) == "" {
		return fmt.Errorf("%w: kind=%s", ErrAuthMissing, spec.Kind)
	}
	switch spec.Kind {
	case AuthKindAPIKey:
		if spec.HeaderName != "" {
			req.Header.Set(spec.HeaderName, secret)
			return nil
		}
		if spec.QueryParam != "" {
			q := req.URL.Query()
			q.Set(spec.QueryParam, secret)
			req.URL.RawQuery = q.Encode()
			return nil
		}
		return fmt.Errorf("%w: api_key requires HeaderName or QueryParam", ErrAuthInvalidSpec)
	case AuthKindBearer:
		req.Header.Set("Authorization", "Bearer "+secret)
		return nil
	case AuthKindCookie:
		req.AddCookie(&nethttp.Cookie{Name: spec.CookieName, Value: secret}) //nolint:gosec // G124: outbound request cookie attaching upstream auth; Secure/HttpOnly/SameSite are Set-Cookie response directives, N/A on a client request
		return nil
	default:
		return fmt.Errorf("%w: unknown kind %q", ErrAuthInvalidSpec, spec.Kind)
	}
}
