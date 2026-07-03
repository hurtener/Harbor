// Package auth is the public SDK facade over Harbor's
// internal/tools/auth package — the tool-side OAuth completion leg: a
// plain http.Handler that exchanges the provider redirect and resumes
// the parked pause (RFC §3.6, §6.3). Alias-based re-exports only: no
// behavior lives here. The steer-and-resume-a-run recipe's headless
// path mounts the callback handler over
// `assemble.Stack.OAuthProviders`. Provider construction (BuildProviders),
// the token store, and the sealer are deliberately private — the
// assembly builds providers from harbor.yaml.
package auth

import (
	internal "github.com/hurtener/Harbor/internal/tools/auth"
)

// OAuth vocabulary — aliases of the internal types.
type (
	// OAuthProvider is the per-provider flow surface the callback
	// handler drives (the same values assemble.Stack.OAuthProviders
	// carries).
	OAuthProvider = internal.OAuthProvider
	// CallbackOption customises CallbackHandler.
	CallbackOption = internal.CallbackOption
	// PendingFlowInfo describes one in-flight authorization flow.
	PendingFlowInfo = internal.PendingFlowInfo
)

// CallbackPath is the canonical callback route path.
const CallbackPath = internal.CallbackPath

// CallbackRoutePattern is the canonical mux pattern
// ("GET /v1/tools/oauth/callback") for mounting CallbackHandler.
const CallbackRoutePattern = internal.CallbackRoutePattern

// Re-exported sentinel errors callers compare via errors.Is.
var (
	// ErrFlowNotFound — no pending flow for the redirect's state (404).
	ErrFlowNotFound = internal.ErrFlowNotFound
	// ErrFlowExpired — the pending flow's TTL elapsed (410).
	ErrFlowExpired = internal.ErrFlowExpired
	// ErrStateMismatch — the state nonce did not match (400).
	ErrStateMismatch = internal.ErrStateMismatch
)

// CallbackHandler builds the plain http.Handler that completes (or
// denies) an OAuth flow and resumes the parked pause.
var CallbackHandler = internal.CallbackHandler

// WithCallbackLogger threads a logger into the handler.
var WithCallbackLogger = internal.WithCallbackLogger

// WithSuccessPage overrides the static "authorization complete" page.
var WithSuccessPage = internal.WithSuccessPage
