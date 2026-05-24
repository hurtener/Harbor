package auth

import (
	"net/http"
)

// SSEAccessTokenShim returns an http.Handler decorator that promotes an
// `?access_token=<jwt>` URL query parameter to a synthesized
// `Authorization: Bearer <jwt>` header BEFORE delegating to the standard
// auth.Middleware. It is the wire-side counterpart of the Console's
// EventSource SSE-subscribe path (`web/console/src/lib/protocol/client.ts`
// — "the bearer token rides as `access_token` — `EventSource` cannot
// carry an `Authorization` header").
//
// # Why an SSE-only shim
//
// The standard browser `EventSource` API cannot set request headers
// (https://html.spec.whatwg.org/multipage/server-sent-events.html#the-eventsource-interface)
// — there is no `Authorization` header path for an EventSource subscriber.
// The conventional workaround is the `access_token` URL query parameter
// (OAuth 2.0 bearer-token URI usage, RFC 6750 §2.3) for SSE endpoints only.
//
// The shim is SSE-ONLY by design. Accepting the query parameter on every
// endpoint would leak the bearer token into:
//
//   - browser history,
//   - intermediary access logs (proxies, CDNs, runtime stderr access-log
//     middleware),
//   - the Referer header on any embedded link,
//   - server-side request-dump panics.
//
// Limiting the shim to the SSE endpoint scopes the leakage to the
// surface where EventSource has no alternative.
//
// # Behavior
//
// On a request with NO Authorization header AND a non-empty
// `access_token` query parameter, the shim clones the request, sets
// `Authorization: Bearer <access_token>` on the clone, and calls next
// with the clone. The original request — and the original query
// parameter — is not mutated; downstream code sees a synthesized
// Authorization header as if the client had supplied one. The query
// param is intentionally LEFT in the URL on the cloned request so the
// SSE handler's URL parsing (event-type filters, admin flag) sees the
// same shape it always did.
//
// On a request that ALREADY has an Authorization header, the shim is a
// pass-through: an explicit Authorization header is always preferred,
// and a same-origin Console (or a non-browser SSE client) that already
// sets the header gets the standard contract.
//
// On a request with neither Authorization nor access_token, the shim is
// a pass-through; the standard middleware rejects the request with
// CodeIdentityRequired as it always did.
//
// # Concurrent reuse (D-025)
//
// The shim wraps next once at construction and holds no mutable state;
// it is safe to share across N concurrent requests.
//
// Round-3 walkthrough fix: pre-shim, the Console's cross-origin SSE
// subscribe got 401 on every request because the standard
// auth.Middleware only read Authorization. The CORS preflight pass
// (Phase 83v / D-162) unblocked the REST surface; this shim unblocks
// the SSE surface for the same multi-process Console+Runtime posture.
func SSEAccessTokenShim(next http.Handler) http.Handler {
	if next == nil {
		// Fail loud rather than mounting a half-wired decorator.
		// The mux constructor calls this once at boot — a nil next is
		// a programming bug, not a runtime condition.
		panic("auth.SSEAccessTokenShim: next http.Handler is nil")
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Authorization-already-set: pass through. The explicit header
		// is always preferred.
		if r.Header.Get("Authorization") != "" {
			next.ServeHTTP(w, r)
			return
		}
		// EventSource path: read access_token from the URL query. A
		// missing/empty value also passes through — the standard
		// middleware rejects it with CodeIdentityRequired.
		token := r.URL.Query().Get("access_token")
		if token == "" {
			next.ServeHTTP(w, r)
			return
		}
		// Clone the request and synthesize the Authorization header on
		// the clone. The original request is untouched; downstream
		// code (event-type filtering, identity propagation) reads from
		// the cloned request only.
		clone := r.Clone(r.Context())
		clone.Header.Set("Authorization", bearerPrefix+token)
		next.ServeHTTP(w, clone)
	})
}
