// Package cors is the Harbor Protocol CORS middleware — the Phase 83v
// security primitive that unlocks the D-091 multi-process Console+Runtime
// posture (Console on one origin attaches to a Runtime on a different
// origin) without weakening the browser-side enforcement contract.
//
// # Default deny
//
// An empty allowlist means NO cross-origin requests are permitted: the
// middleware is a no-op pass-through and the browser's same-origin
// policy denies every cross-origin preflight by default. The operator
// MUST opt in by listing exact origins in `server.allowed_origins`. The
// pre-83v behavior — no CORS headers at all — is the same shape, but
// 83v makes the posture explicit instead of accidental.
//
// # Per-origin echo, NEVER wildcard in production
//
// On an allowlist match, the middleware echoes the request's `Origin`
// header verbatim into `Access-Control-Allow-Origin` and sets
// `Access-Control-Allow-Credentials: true`. The browser locks the
// response to that specific origin — no `*` ever appears in production
// paths because `*` is incompatible with credentialed requests and the
// browser refuses the combination. The non-allowed path emits NO CORS
// headers; the browser then blocks per the standard contract.
//
// # The dev-only wildcard escape hatch (CLAUDE.md §13)
//
// `Config.DevAllowAny=true` accepts ANY origin and is allowed in the
// validator only when the operator explicitly sets the
// `server.cors_dev_allow_any: true` flag. When the flag is set, the
// `harbor dev` boot path prints a stderr banner so the posture is
// visibly dev-only. Production deployments declare exact origins; the
// validator rejects `*` (or any wildcard shape) when the flag is
// unset.
//
// # Concurrent reuse (D-025)
//
// Wrap returns an immutable middleware: the allowlist is set once at
// construction and read by every concurrent request without
// synchronisation. The middleware adds no mutable state.
package cors

import (
	"net/http"
	"strings"
)

// Header names. Kept as named constants so the unit tests can grep them
// and so any future header additions land in one place.
const (
	HeaderOrigin                        = "Origin"
	HeaderVary                          = "Vary"
	HeaderAccessControlAllowOrigin      = "Access-Control-Allow-Origin"
	HeaderAccessControlAllowCredentials = "Access-Control-Allow-Credentials"
	HeaderAccessControlAllowMethods     = "Access-Control-Allow-Methods"
	HeaderAccessControlAllowHeaders     = "Access-Control-Allow-Headers"
	HeaderAccessControlMaxAge           = "Access-Control-Max-Age"
	HeaderAccessControlRequestMethod    = "Access-Control-Request-Method"
)

// The fixed allow-method / allow-header / max-age values the middleware
// emits on a match. Per the phase plan: explicit lists, not `*`. The set
// covers every method the Protocol surface needs (REST + SSE GET) and
// every header a cross-origin Console sends:
//
//   - Authorization — Bearer auth
//   - Content-Type — POST bodies
//   - Last-Event-ID — SSE resume
//   - X-Harbor-Tenant / X-Harbor-User / X-Harbor-Session — the Console's
//     identity envelope (`web/console/src/lib/protocol/client.ts`). Round-3
//     walkthrough caught the omission: the browser's preflight allow-
//     headers check failed because these custom headers were not listed,
//     blocking every cross-origin request even though the server-side
//     auth had nothing to reject. (Round 3 / post-83v structural fix.)
const (
	allowMethods = "GET, POST, PUT, DELETE, PATCH, OPTIONS"
	allowHeaders = "Authorization, Content-Type, Last-Event-ID, X-Harbor-Tenant, X-Harbor-User, X-Harbor-Session"
	// 24h preflight cache — the standard browser default. Future phases
	// can make this operator-configurable; V1 picks one value.
	maxAgeSeconds = "86400"
)

// Config drives Wrap. Set once at construction; the middleware never
// mutates it after.
type Config struct {
	// AllowedOrigins is the exact-match allowlist. Each entry is a full
	// origin (`scheme://host[:port]`). Empty list = no CORS = same-origin
	// only (the default-deny posture).
	AllowedOrigins []string
	// DevAllowAny opens the door to ANY origin. CLAUDE.md §13 dev-only
	// escape hatch: the validator rejects this flag unless the operator
	// explicitly sets `server.cors_dev_allow_any: true`, and the dev
	// boot path prints a stderr banner. NEVER set in production.
	DevAllowAny bool
}

// Wrap returns an http.Handler that applies CORS to next per cfg. A nil
// next is forbidden; an empty allowlist + DevAllowAny=false leaves the
// wrapped handler effectively unchanged (no CORS headers ever emitted)
// — equivalent to the pre-83v same-origin-only posture.
//
// The middleware short-circuits a preflight OPTIONS request with 204:
//   - on a matching allowlist (or DevAllowAny), it returns 204 with the
//     full allow-* header set so the browser proceeds to the real
//     request.
//   - on a non-match, it returns 204 with NO CORS headers so the browser
//     blocks the subsequent request per the standard contract.
//
// On a non-preflight cross-origin request, the middleware adds the
// allow-origin + allow-credentials headers (on match) BEFORE delegating
// to next so the response carries them. Same-origin requests (empty
// Origin header) pass through untouched.
//
// Wrap returns an immutable handler safe for concurrent use by N
// goroutines (D-025) — the allowlist is read-only after construction.
func Wrap(next http.Handler, cfg Config) http.Handler {
	if next == nil {
		// Fail loud rather than mounting a half-wired middleware that
		// returns 404 / 500 silently. The mux constructor calls Wrap
		// once at boot — a nil next is a programming bug, not a
		// runtime condition.
		panic("cors.Wrap: next http.Handler is nil")
	}
	// Materialise the allowlist into a set for O(1) lookup. The set is
	// case-sensitive — Origin is host-portion-case-insensitive per
	// RFC 6454 but every real browser sends a lowercased host, and the
	// validator already normalises scheme + host before storing.
	allowed := make(map[string]struct{}, len(cfg.AllowedOrigins))
	for _, o := range cfg.AllowedOrigins {
		trimmed := strings.TrimSpace(o)
		if trimmed != "" {
			allowed[trimmed] = struct{}{}
		}
	}
	devAllowAny := cfg.DevAllowAny

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get(HeaderOrigin)
		// Same-origin requests carry no Origin header. Pass through
		// untouched; we are NOT in the cross-origin contract.
		if origin == "" {
			next.ServeHTTP(w, r)
			return
		}

		matched := originAllowed(origin, allowed, devAllowAny)

		// Always set Vary: Origin once we observe an Origin header so
		// shared caches do not poison cross-origin responses. (Even on
		// non-match — the cache must vary on Origin so a later allowed
		// origin sees a fresh response.)
		w.Header().Add(HeaderVary, HeaderOrigin)

		if r.Method == http.MethodOptions && r.Header.Get(HeaderAccessControlRequestMethod) != "" {
			// CORS preflight. Either emit allow-* headers (match) or
			// emit none (no-match); in both cases the response is 204
			// No Content with no body.
			if matched {
				writeAllowHeaders(w, origin)
			}
			w.WriteHeader(http.StatusNoContent)
			return
		}

		// Non-preflight cross-origin request. On match, set the
		// allow-origin + allow-credentials headers so the browser
		// surfaces the response to the cross-origin script. On
		// non-match, omit them — the browser then blocks per the
		// standard contract. Either way, next gets to handle the
		// request (the browser-side block is the enforcement, not a
		// server-side 4xx).
		if matched {
			writeAllowHeaders(w, origin)
		}
		next.ServeHTTP(w, r)
	})
}

// originAllowed reports whether origin matches the allowlist or whether
// the dev-only any-origin flag is active.
func originAllowed(origin string, allowed map[string]struct{}, devAllowAny bool) bool {
	if devAllowAny {
		return true
	}
	_, ok := allowed[origin]
	return ok
}

// writeAllowHeaders emits the four allow-* headers + max-age. The
// allow-origin is the request's exact origin (never `*`) so the response
// is compatible with `Access-Control-Allow-Credentials: true`.
func writeAllowHeaders(w http.ResponseWriter, origin string) {
	h := w.Header()
	h.Set(HeaderAccessControlAllowOrigin, origin)
	h.Set(HeaderAccessControlAllowCredentials, "true")
	h.Set(HeaderAccessControlAllowMethods, allowMethods)
	h.Set(HeaderAccessControlAllowHeaders, allowHeaders)
	h.Set(HeaderAccessControlMaxAge, maxAgeSeconds)
}
