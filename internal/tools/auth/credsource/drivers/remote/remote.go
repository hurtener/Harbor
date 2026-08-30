// Package remote ships the coordinator-served credential source: the
// runtime PULLS an OAuth provider's client credential
// (`client_id` / `client_secret`) from an operator-configured endpoint
// at first need, authenticated by the runtime's own service token.
//
// # Why it exists
//
// A provider whose credential is env-resolved once at boot can never
// receive a credential a coordinator mints AFTER the runtime booted — the
// process environment is fixed at exec. The remote source closes that
// gap: the credential resolves lazily at first use, so a coordinator that
// provisions the runtime's credential post-boot reaches an
// already-running runtime with zero touch. This is the token-exchange pull posture
// one level up — the broker CLIENT credential is itself an externally
// owned secret, pulled from the authority that owns it and never
// persisted.
//
// # The fetch contract (Harbor-defined, versioned)
//
// A single authenticated GET:
//
//	GET <url>
//	Authorization: Bearer <token from auth_token_env>
//	Accept: application/json
//
// The coordinator responds with a strict JSON object:
//
//	{
//	  "format_version": 1,          // REQUIRED — the contract version
//	  "client_id":      "...",      // REQUIRED
//	  "client_secret":  "...",      // REQUIRED
//	  "expires_in":     3600        // OPTIONAL — seconds; omitted / 0 = no expiry
//	}
//
// Unknown top-level fields are rejected (strict) so contract drift fails
// loud instead of silently ignoring a mis-served field. The
// `format_version` lets the shape evolve; the runtime accepts version 1.
//
// The endpoint MUST be https — the request carries the runtime's service
// bearer token; plaintext http is accepted only for a loopback host
// (127.0.0.0/8 / ::1 / localhost — the fixture / dev case). The fetch is
// terminal: redirects are refused, never followed (a credential endpoint
// that redirects is a fault, and following one could replay the bearer
// to a host the operator never configured).
//
// # Memory-only, TTL-capped, single-flight
//
// The fetched credential is held in memory only — never persisted (a
// sealed on-disk copy would recreate the shadow-store revocation hole a
// persisted externally-owned credential creates). The serve horizon is
// the EARLIER of the response's
// `expires_in` and the operator `cache_ttl` cap; a fetch on expiry
// refetches. A burst of concurrent misses collapses onto ONE fetch
// (single-flight per source instance — the client credential is
// runtime-level, not identity-scoped, so one credential serves every
// identity).
//
// # Fail loud, never fall back
//
// An unreachable endpoint, a non-200 status, or a malformed / strict-
// parse-rejected body fails the tool call with the typed
// [credsource.ErrCredentialSourceUnavailable] (provider + endpoint, ZERO
// secret bytes) and emits a `tool.provider_credential_fetch_failed`
// SafePayload event. There is NEVER a fallback to env, to an
// unauthenticated call, or to the interactive flow (§13). A successful
// fetch emits `tool.provider_credential_fetched`.
package remote

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/hurtener/Harbor/internal/audit"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/tools/auth/credsource"
)

// supportedFormatVersion is the coordinator-response contract version the
// runtime accepts. A response advertising a different version fails loud
// (the field exists so the shape can evolve without a silent mismatch).
const supportedFormatVersion = 1

// defaultCacheTTL bounds the in-memory serve horizon when the operator
// declares no `cache_ttl` and the coordinator advertises a longer (or no)
// expiry. defaultTimeout bounds a single fetch.
const (
	defaultCacheTTL = 5 * time.Minute
	defaultTimeout  = 30 * time.Second
	maxResponseByte = 1 << 16
)

// init self-registers the `remote` credential source.
func init() {
	credsource.MustRegister(credsource.SourceRemote, New)
}

// New constructs the `remote` source for one provider entry. Registered
// as the source Factory; `BuildProviders` dispatches by name.
func New(cfg credsource.Config) (credsource.Source, error) {
	if cfg.Remote == nil {
		return nil, fmt.Errorf("credsource/remote: provider %q: remote block is required", cfg.ProviderName)
	}
	if cfg.Bus == nil || cfg.Redactor == nil {
		return nil, fmt.Errorf("credsource/remote: provider %q: Bus and Redactor are mandatory (fetch events)", cfg.ProviderName)
	}
	timeout := cfg.Remote.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	// The credential fetch is TERMINAL: a credential endpoint that
	// redirects is a fault, not a hop to follow — following one could
	// replay the runtime's service bearer token to a host the operator
	// never configured. CheckRedirect refuses every redirect with the
	// typed sentinel (a caller-supplied client is shallow-copied so the
	// caller's instance is never mutated).
	var httpClient *http.Client
	if cfg.HTTPClient != nil {
		clone := *cfg.HTTPClient
		httpClient = &clone
	} else {
		httpClient = &http.Client{Timeout: timeout}
	}
	httpClient.CheckRedirect = func(*http.Request, []*http.Request) error {
		return fmt.Errorf("%w: credential endpoint attempted a redirect (a credential endpoint that redirects is a fault, not a hop to follow)",
			credsource.ErrCredentialSourceUnavailable)
	}
	clock := cfg.Clock
	if clock == nil {
		clock = time.Now
	}
	cacheTTL := cfg.Remote.CacheTTL
	if cacheTTL <= 0 {
		cacheTTL = defaultCacheTTL
	}
	return &source{
		providerName: cfg.ProviderName,
		endpoint:     cfg.Remote.URL,
		authTokenEnv: cfg.Remote.AuthTokenEnv,
		cacheTTL:     cacheTTL,
		timeout:      timeout,
		httpClient:   httpClient,
		now:          clock,
		bus:          cfg.Bus,
		redactor:     cfg.Redactor,
		getenv:       os.Getenv,
	}, nil
}

// source is the concrete remote credential source.
//
// Concurrent reuse: every field above the mutex is set once at
// construction. The in-memory cache + single-flight table are the only
// mutable state, guarded by one mutex; per-run identity is read from ctx
// on every Resolve, never from the source.
type source struct {
	providerName string
	endpoint     string
	authTokenEnv string
	cacheTTL     time.Duration
	timeout      time.Duration
	httpClient   *http.Client
	now          func() time.Time
	bus          events.EventBus
	redactor     audit.Redactor
	// getenv reads the runtime service token lazily at fetch time (a
	// rotated token is picked up without restart). Overridable in tests.
	getenv func(string) string

	mu         sync.Mutex
	cred       credsource.ClientCredential
	serveUntil time.Time
	haveCred   bool
	flight     *fetchCall
}

// fetchCall is one in-flight remote fetch shared by N collapsed callers.
type fetchCall struct {
	done chan struct{}
	cred credsource.ClientCredential
	err  error
}

// ValidateAtBoot validates the block SHAPE only — the endpoint URL is
// well-formed http(s) with a host, and the auth-token env var is named.
// It does NOT fetch: the credential resolves lazily at first use so a
// coordinator-minted credential reaches an already-running runtime.
func (s *source) ValidateAtBoot(context.Context) error {
	if strings.TrimSpace(s.endpoint) == "" {
		return fmt.Errorf("credsource/remote: provider %q: remote.url must not be empty", s.providerName)
	}
	u, err := url.Parse(s.endpoint)
	if err != nil {
		return fmt.Errorf("credsource/remote: provider %q: remote.url %q is not a valid URL: %w", s.providerName, s.endpoint, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("credsource/remote: provider %q: remote.url %q must be http(s), got scheme %q", s.providerName, s.endpoint, u.Scheme)
	}
	if u.Host == "" {
		return fmt.Errorf("credsource/remote: provider %q: remote.url %q has no host", s.providerName, s.endpoint)
	}
	// TLS is mandatory for non-loopback hosts: the fetch sends the
	// runtime's service bearer token, and a plaintext bearer must never
	// leave the box. Loopback (127.0.0.0/8 / ::1 / localhost) is the one
	// carve-out — the local fixture / dev case. Mirrors the config
	// validator's gate (§4.4 duplication: config must not import driver
	// packages), so a direct-construction caller that skipped
	// `harbor validate` still fails loud here.
	if u.Scheme == "http" && !isLoopbackHostname(u.Hostname()) {
		return fmt.Errorf("credsource/remote: provider %q: remote.url %q must be https for non-loopback hosts (the fetch sends the runtime's service bearer token; plaintext http is allowed only for 127.0.0.1 / ::1 / localhost)",
			s.providerName, s.endpoint)
	}
	if strings.TrimSpace(s.authTokenEnv) == "" {
		return fmt.Errorf("credsource/remote: provider %q: remote.auth_token_env must name the env var holding the runtime service token", s.providerName)
	}
	return nil
}

// isLoopbackHostname reports whether a URL hostname is loopback for the
// TLS carve-out: a numeric loopback IP (127.0.0.0/8 or ::1) or the
// literal "localhost". Mirrors internal/config's validator-side check
// (duplicated by design — §4.4: config never imports driver packages).
func isLoopbackHostname(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Resolve returns the current client credential, fetching lazily and
// refetching on expiry. Concurrent misses collapse onto one fetch.
func (s *source) Resolve(ctx context.Context) (credsource.ClientCredential, error) {
	if err := ctx.Err(); err != nil {
		return credsource.ClientCredential{}, fmt.Errorf("credsource/remote: Resolve cancelled: %w", err)
	}
	// Hot path: fresh cache hit.
	s.mu.Lock()
	if s.haveCred && s.now().Before(s.serveUntil) {
		cred := s.cred
		s.mu.Unlock()
		return cred, nil
	}
	s.mu.Unlock()
	return s.fetchSingleFlight(ctx)
}

// fetchSingleFlight collapses concurrent misses onto one fetch. The fetch
// runs on a context DETACHED from the initiating caller's cancellation
// (values — identity, trace — preserved; deadline is the fetch timeout)
// so a cancelled caller never poisons the collapsed waiters.
func (s *source) fetchSingleFlight(ctx context.Context) (credsource.ClientCredential, error) {
	s.mu.Lock()
	call := s.flight
	if call == nil {
		call = &fetchCall{done: make(chan struct{})}
		s.flight = call
		go s.runFetch(ctx, call)
	}
	s.mu.Unlock()

	select {
	case <-call.done:
		return call.cred, call.err
	case <-ctx.Done():
		return credsource.ClientCredential{}, fmt.Errorf("credsource/remote: Resolve cancelled: %w", ctx.Err())
	}
}

// runFetch is the single-flight worker body: one remote fetch shared by
// every collapsed caller. It emits the fetch outcome event and caches on
// success. Its lifetime is bounded by the fetch timeout so a wedged
// coordinator cannot leak the goroutine.
func (s *source) runFetch(callerCtx context.Context, call *fetchCall) {
	defer func() {
		// Leave the flight table BEFORE releasing waiters, so a released
		// caller that immediately re-misses starts a fresh fetch rather
		// than joining this completed one.
		s.mu.Lock()
		s.flight = nil
		s.mu.Unlock()
		close(call.done)
	}()

	fetchCtx, cancel := context.WithTimeout(context.WithoutCancel(callerCtx), s.timeout)
	defer cancel()

	// Re-check the cache under the flight: a flight that completed
	// between the caller's miss and this start may have populated it.
	s.mu.Lock()
	if s.haveCred && s.now().Before(s.serveUntil) {
		call.cred = s.cred
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()

	cred, formatVersion, err := s.fetch(fetchCtx)
	if err != nil {
		// Fail loud + observable. The failure event is best-effort-after:
		// the sentinel already propagates to the run regardless.
		s.emitFailed(fetchCtx, callerCtx, err)
		call.err = err
		return
	}

	// Audit the actual fetch before caching (mirror the tokenexchange
	// posture: an emit failure fails the operation so the fetch is not
	// silently served without its audit trail).
	if emitErr := s.emitFetched(fetchCtx, cred, formatVersion); emitErr != nil {
		call.err = fmt.Errorf("credsource/remote: provider %q: emit tool.provider_credential_fetched: %w", s.providerName, emitErr)
		return
	}

	s.mu.Lock()
	s.cred = cred
	s.serveUntil = s.serveHorizon(cred.ExpiresAt)
	s.haveCred = true
	s.mu.Unlock()
	call.cred = cred
}

// serveHorizon computes the in-memory serve horizon: the EARLIER of the
// coordinator-advertised expiry and now+cacheTTL. A zero expiry ("none
// advertised") yields the cap alone. A token is never served past the
// coordinator's own expiry.
func (s *source) serveHorizon(expiresAt time.Time) time.Time {
	capped := s.now().Add(s.cacheTTL)
	if expiresAt.IsZero() || capped.Before(expiresAt) {
		return capped
	}
	return expiresAt
}

// credentialResponse is the coordinator's strict JSON shape.
type credentialResponse struct {
	FormatVersion int    `json:"format_version"`
	ClientID      string `json:"client_id"`
	ClientSecret  string `json:"client_secret"`
	ExpiresIn     int    `json:"expires_in"`
}

// fetch performs one authenticated GET and strict-parses the response.
// The returned error is a wrapped ErrCredentialSourceUnavailable with
// ZERO secret bytes.
func (s *source) fetch(ctx context.Context) (credsource.ClientCredential, int, error) {
	token := s.getenv(s.authTokenEnv)
	if token == "" {
		return credsource.ClientCredential{}, 0, fmt.Errorf("%w: provider %q endpoint %q: env var %q (named by auth_token_env) is unset or empty",
			credsource.ErrCredentialSourceUnavailable, s.providerName, s.endpoint, s.authTokenEnv)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.endpoint, nil)
	if err != nil {
		return credsource.ClientCredential{}, 0, fmt.Errorf("%w: provider %q endpoint %q: build request: %w",
			credsource.ErrCredentialSourceUnavailable, s.providerName, s.endpoint, err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return credsource.ClientCredential{}, 0, fmt.Errorf("%w: provider %q endpoint %q: GET: %w",
			credsource.ErrCredentialSourceUnavailable, s.providerName, s.endpoint, err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseByte)) //nolint:errcheck // a partial read still yields a usable error; the status code drives the branch
	if resp.StatusCode/100 != 2 {
		return credsource.ClientCredential{}, 0, fmt.Errorf("%w: provider %q endpoint %q: status %d",
			credsource.ErrCredentialSourceUnavailable, s.providerName, s.endpoint, resp.StatusCode)
	}

	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	var cr credentialResponse
	if err := dec.Decode(&cr); err != nil {
		return credsource.ClientCredential{}, 0, fmt.Errorf("%w: provider %q endpoint %q: malformed response: %w",
			credsource.ErrCredentialSourceUnavailable, s.providerName, s.endpoint, err)
	}
	if cr.FormatVersion != supportedFormatVersion {
		return credsource.ClientCredential{}, 0, fmt.Errorf("%w: provider %q endpoint %q: unsupported format_version %d (runtime accepts %d)",
			credsource.ErrCredentialSourceUnavailable, s.providerName, s.endpoint, cr.FormatVersion, supportedFormatVersion)
	}
	if cr.ClientID == "" || cr.ClientSecret == "" {
		return credsource.ClientCredential{}, 0, fmt.Errorf("%w: provider %q endpoint %q: response missing client_id / client_secret",
			credsource.ErrCredentialSourceUnavailable, s.providerName, s.endpoint)
	}
	if cr.ExpiresIn < 0 {
		return credsource.ClientCredential{}, 0, fmt.Errorf("%w: provider %q endpoint %q: negative expires_in %d",
			credsource.ErrCredentialSourceUnavailable, s.providerName, s.endpoint, cr.ExpiresIn)
	}

	var expiresAt time.Time
	if cr.ExpiresIn > 0 {
		expiresAt = s.now().Add(time.Duration(cr.ExpiresIn) * time.Second)
	}
	return credsource.ClientCredential{
		ClientID:     cr.ClientID,
		ClientSecret: cr.ClientSecret,
		ExpiresAt:    expiresAt,
	}, cr.FormatVersion, nil
}

// emitFetched publishes the success event (zero credential bytes). The
// event is attributed to the identity of whichever collapsed caller's
// ctx initiated the single-flight fetch — not to every waiter; the
// credential is runtime-level, so one fetch (and one audit record)
// serves the whole burst.
func (s *source) emitFetched(ctx context.Context, cred credsource.ClientCredential, formatVersion int) error {
	payload := credsource.ProviderCredentialFetchedPayload{
		Provider:      s.providerName,
		Endpoint:      hostOf(s.endpoint),
		FormatVersion: formatVersion,
		ExpiresAt:     cred.ExpiresAt,
	}
	return s.emit(ctx, credsource.EventTypeProviderCredentialFetched, payload)
}

// emitFailed publishes the failure event. It is best-effort — the run
// already fails loud via the returned sentinel — but an emit error is
// logged nowhere here; the fetch error is what propagates. Like
// emitFetched, the event is attributed to the identity of the caller
// whose ctx initiated the single-flight fetch, not to every collapsed
// waiter. The ctx used is that caller's (identity-bearing) even when
// the fetch ctx timed out.
func (s *source) emitFailed(fetchCtx, callerCtx context.Context, cause error) {
	payload := credsource.ProviderCredentialFetchFailedPayload{
		Provider: s.providerName,
		Endpoint: hostOf(s.endpoint),
		Reason:   redactReason(cause),
	}
	// Prefer the caller ctx for identity; fall back to the fetch ctx.
	emitCtx := callerCtx
	if _, ok := identity.From(emitCtx); !ok {
		emitCtx = fetchCtx
	}
	_ = s.emit(emitCtx, credsource.EventTypeProviderCredentialFetchFailed, payload) //nolint:errcheck // failure event is observability-only; the sentinel is the authoritative failure signal
}

// emit redacts (defence in depth; SafePayload so the bus skips its own
// pass) and publishes with the ctx identity.
func (s *source) emit(ctx context.Context, evType events.EventType, payload events.EventPayload) error {
	if _, err := s.redactor.Redact(ctx, payload); err != nil {
		return fmt.Errorf("redact: %w", err)
	}
	id, _ := identity.From(ctx)
	return s.bus.Publish(ctx, events.Event{
		Type:     evType,
		Identity: identity.Quadruple{Identity: id},
		Payload:  payload,
	})
}

// hostOf returns the host of a URL (empty on parse failure), so events
// never carry query material.
func hostOf(raw string) string {
	if u, err := url.Parse(raw); err == nil {
		return u.Host
	}
	return ""
}

// redactReason maps a fetch error onto a short, secret-free
// classification for the failure event. The underlying error already
// carries no secret bytes; this keeps the event field terse.
func redactReason(err error) string {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "status "):
		if i := strings.LastIndex(msg, "status "); i >= 0 {
			return strings.TrimSpace(msg[i:])
		}
		return "non-2xx status"
	case strings.Contains(msg, "malformed response"):
		return "malformed response"
	case strings.Contains(msg, "unsupported format_version"):
		return "unsupported format_version"
	case strings.Contains(msg, "missing client_id"):
		return "incomplete response"
	case strings.Contains(msg, "auth_token_env"):
		return "service token unset"
	default:
		return "unreachable"
	}
}
