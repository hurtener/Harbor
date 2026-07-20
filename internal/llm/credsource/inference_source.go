// Package credsource is the inference-plane credential-source seam: it
// answers WHERE a runtime's LLM PROVIDER API KEY comes from — the process
// environment (boot config, the historical default) or a coordinator-served
// broker the runtime PULLS from at connect + refresh.
//
// # A DISTINCT seam, deliberately (not a §13 parallel implementation)
//
// This seam follows the §4.4 seam PATTERN but is NOT the OAuth-plane
// `internal/tools/auth/credsource.Source`. The two resolve different
// credentials for different consumers with different custody models:
//
//   - The OAuth seam resolves an OAuth provider's own client_id /
//     client_secret for the tool-plane token-exchange driver, with a
//     PER-IDENTITY, LAZY pull (a tool bearer is identity-scoped).
//   - This seam resolves an LLM provider API KEY for the bifrost `Account`,
//     with a RUNTIME-SCOPED connect + refresh pull (a provider key is a
//     runtime-level credential, never identity-scoped data — it does not
//     widen the isolation tuple).
//
// Folding both into one driver would leak the per-identity assumptions the
// OAuth seam bakes in onto the runtime-scoped path; the sibling is
// justified, not incidental. The two MAY share the HTTP-pull mechanics
// (this file keeps them credential-agnostic and self-contained), but the
// interface and its custody model are distinct.
//
// # Connect + refresh only — never a per-call KEK decrypt
//
// The pulled key is cached in memory and rides the existing atomic
// key-swap ([llm.LiveKey]): the bifrost `Account.GetKeysForProvider` reads
// the live atomic pointer on the inference HOT PATH with no broker
// round-trip and no per-call decrypt. A broker-side rotation lands on the
// next call via [InferenceKeySource.Refresh], with no `ReloadConfig` race.
//
// # Fail loud, no dual path, no stale cache
//
// A broker unreachable at connect OR a failed refresh raises the typed
// [ErrProviderKeyUnavailable]. The runtime NEVER falls back to a local /
// boot key, and NEVER serves a stale cached key past its refresh contract:
// once the cache TTL is crossed with no successful refresh, the live key is
// zeroed so bound calls fail closed rather than serve a possibly-revoked
// key. A provider is brokered XOR local, config-declared (validated loud at
// boot). Nothing is persisted — the coordinator remains sole custodian.
package credsource

import (
	"context"
	"encoding/json"
	"errors"
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
	"github.com/hurtener/Harbor/internal/llm"
)

// ErrProviderKeyUnavailable — the broker was unreachable at connect OR a
// refresh failed. The runtime NEVER falls back to a local key and NEVER
// serves a stale cached key past its refresh contract. Callers compare via
// errors.Is.
var ErrProviderKeyUnavailable = errors.New("llm/credsource: provider key unavailable from broker (fail-loud; no local fallback, no stale cache)")

// Reserved runtime-scope identity components. A runtime-scoped credential
// pull fires outside any (tenant, user, session) request, but the event bus
// requires a structural triple on every published Event. These reserved
// components satisfy that transport requirement WITHOUT synthesizing a real
// user's session: they are infrastructure identity, never an isolation
// principal, and storage / memory / event-filter scoping never treats them
// as one (they do not widen the isolation tuple, §4). The authoritative
// audit attribution is the payload's RuntimePrincipal, not this triple.
const (
	// RuntimePrincipalTenant is the reserved tenant component of the
	// runtime service-principal scope.
	RuntimePrincipalTenant = "_harbor_runtime"
	// RuntimePrincipalUser is the reserved user component of the runtime
	// service-principal scope.
	RuntimePrincipalUser = "_service_principal"
)

// supportedFormatVersion is the coordinator-response contract version the
// runtime accepts. A response advertising a different version fails loud.
const supportedFormatVersion = 1

const (
	defaultCacheTTL = 5 * time.Minute
	defaultTimeout  = 30 * time.Second
	maxResponseByte = 1 << 16
)

// KeySource is the §4.4-pattern seam interface an inference-plane
// credential source implements. It is a MANDATORY interface (no
// optional-capability ceremony): the source pulls the provider key at
// connect + refresh, seeds / refreshes the [llm.LiveKey] the bifrost
// `Account` reads, and closes the binding on uninstall.
//
// Concurrent reuse: a KeySource is a compiled artifact built once and
// shared across N goroutines. It holds immutable configuration plus an
// internally-synchronised in-memory cache; the inference hot path reads the
// LiveKey atomic pointer, never the source.
type KeySource interface {
	// Connect performs the first authenticated pull and seeds the LiveKey.
	// Fails loud with ErrProviderKeyUnavailable on broker-unreachable.
	Connect(ctx context.Context) error
	// Refresh re-pulls on the TTL boundary and atomically swaps the LiveKey.
	// A failed refresh raises ErrProviderKeyUnavailable; once the cache TTL
	// is crossed with no success the live key is zeroed rather than served
	// stale.
	Refresh(ctx context.Context) error
	// Close closes the binding: the live key is zeroed so a subsequently
	// bound call fails loud rather than serving the old key.
	Close()
}

// InferenceKeySource pulls a provider key from a boot-declared broker at
// connect + refresh, caches it in memory (TTL-capped, single-flighted), and
// seeds / refreshes the LiveKey the bifrost Account reads. Immutable after
// construction; safe to share across N goroutines. It NEVER decrypts on the
// per-call hot path — the hot path reads the live atomic pointer.
type InferenceKeySource struct {
	provider     string
	brokerName   string
	principal    string
	endpoint     string
	authTokenEnv string
	cacheTTL     time.Duration
	timeout      time.Duration
	httpClient   *http.Client
	now          func() time.Time
	live         *llm.LiveKey
	bus          events.EventBus
	redactor     audit.Redactor
	// getenv reads the runtime service token lazily at fetch time (a rotated
	// token is picked up without restart). Overridable in tests.
	getenv func(string) string

	// refreshLead is how far ahead of the serve horizon the Run scheduler
	// re-pulls, so a fresh key is seeded before the current one expires.
	refreshLead time.Duration

	// mu guards the cache horizon + single-flight table + the backstop timer.
	// The live key holder is itself atomic (internally synchronised) and read
	// on the hot path without this lock.
	mu         sync.Mutex
	serveUntil time.Time
	haveKey    bool
	closed     bool
	flight     *fetchCall
	// generation increments on every horizon change (pull success / drop) so a
	// stale backstop-timer callback can detect it has been superseded.
	generation uint64
	// expiryTimer is the self-arming backstop that zeroes the live key at the
	// serve horizon even if the refresh loop stalls (W2 defense in depth).
	expiryTimer *time.Timer
}

// Config is the constructor input for an InferenceKeySource. The endpoint /
// audience / scope ceiling come from the boot-declared broker (never the
// wire); the Live holder is the SAME atomic key the bifrost Account reads,
// so a pull lands on the next hot-path read with no ReloadConfig.
type Config struct {
	// Provider is the LLM provider the key authenticates (e.g. "openai").
	Provider string
	// BrokerName is the boot-declared inference-broker name (non-secret,
	// audit + error attribution).
	BrokerName string
	// RuntimePrincipal is the per-runtime service-principal id the pull
	// audit event is attributed to. Non-secret. Falls back to AuthTokenEnv
	// (the env var NAME, still non-secret) when empty.
	RuntimePrincipal string
	// CredentialURL is the boot-pinned coordinator pull endpoint.
	CredentialURL string
	// AuthTokenEnv names the env var holding the runtime's own broker
	// credential (the Bearer presented to CredentialURL).
	AuthTokenEnv string
	// CacheTTL caps the in-memory serve horizon (0 = default 5m).
	CacheTTL time.Duration
	// Timeout bounds a single pull (0 = default 30s).
	Timeout time.Duration
	// Live is the atomic key holder the bifrost Account reads. Required.
	Live *llm.LiveKey
	// Bus + Redactor are mandatory — every pull emits a runtime-scoped
	// audit event through them, and a pull that cannot be audited fails loud.
	Bus      events.EventBus
	Redactor audit.Redactor
	// Clock overrides the time source (tests). Nil = time.Now.
	Clock func() time.Time
	// HTTPClient overrides the fetch client (tests). Nil = a fresh client
	// with the resolved timeout. A caller-supplied client is shallow-copied
	// so its CheckRedirect is set without mutating the caller's instance.
	HTTPClient *http.Client
}

// NewInferenceKeySource constructs the source. It validates the block SHAPE
// (endpoint well-formed https / loopback, auth-token env named, Live / Bus /
// Redactor present) but does NOT fetch — the pull happens at Connect.
func NewInferenceKeySource(cfg Config) (*InferenceKeySource, error) {
	if cfg.Live == nil {
		return nil, fmt.Errorf("llm/credsource: provider %q: Live key holder is required", cfg.Provider)
	}
	if cfg.Bus == nil || cfg.Redactor == nil {
		return nil, fmt.Errorf("llm/credsource: provider %q: Bus and Redactor are mandatory (pull audit events)", cfg.Provider)
	}
	if strings.TrimSpace(cfg.CredentialURL) == "" {
		return nil, fmt.Errorf("llm/credsource: provider %q: credential_url must not be empty", cfg.Provider)
	}
	u, err := url.Parse(cfg.CredentialURL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, fmt.Errorf("llm/credsource: provider %q: credential_url %q is not a valid http(s) URL with a host", cfg.Provider, cfg.CredentialURL)
	}
	if u.Scheme == "http" && !isLoopbackHostname(u.Hostname()) {
		return nil, fmt.Errorf("llm/credsource: provider %q: credential_url %q must be https for non-loopback hosts (the pull sends the runtime service bearer)", cfg.Provider, cfg.CredentialURL)
	}
	if strings.TrimSpace(cfg.AuthTokenEnv) == "" {
		return nil, fmt.Errorf("llm/credsource: provider %q: auth_token_env must name the env var holding the runtime service token", cfg.Provider)
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	cacheTTL := cfg.CacheTTL
	if cacheTTL <= 0 {
		cacheTTL = defaultCacheTTL
	}
	var httpClient *http.Client
	if cfg.HTTPClient != nil {
		clone := *cfg.HTTPClient
		httpClient = &clone
	} else {
		httpClient = &http.Client{Timeout: timeout}
	}
	// The credential pull is TERMINAL: an endpoint that redirects is a fault,
	// never a hop to follow — following one could replay the runtime's
	// service bearer to a host the operator never configured.
	httpClient.CheckRedirect = func(*http.Request, []*http.Request) error {
		return fmt.Errorf("%w: credential endpoint attempted a redirect (a credential endpoint that redirects is a fault, not a hop to follow)", ErrProviderKeyUnavailable)
	}
	clock := cfg.Clock
	if clock == nil {
		clock = time.Now
	}
	principal := strings.TrimSpace(cfg.RuntimePrincipal)
	if principal == "" {
		principal = cfg.AuthTokenEnv
	}
	// The Run scheduler re-pulls this far ahead of the serve horizon so a
	// fresh key is seeded before the current one expires: a quarter of the
	// cache TTL, capped at 60s (a bounded lead even for long TTLs).
	refreshLead := cacheTTL / 4
	if refreshLead > time.Minute {
		refreshLead = time.Minute
	}
	if refreshLead <= 0 {
		refreshLead = time.Second
	}
	return &InferenceKeySource{
		provider:     cfg.Provider,
		brokerName:   cfg.BrokerName,
		principal:    principal,
		endpoint:     cfg.CredentialURL,
		authTokenEnv: cfg.AuthTokenEnv,
		cacheTTL:     cacheTTL,
		timeout:      timeout,
		refreshLead:  refreshLead,
		httpClient:   httpClient,
		now:          clock,
		live:         cfg.Live,
		bus:          cfg.Bus,
		redactor:     cfg.Redactor,
		getenv:       os.Getenv,
	}, nil
}

// Connect performs the first authenticated pull and seeds the live key.
// Fails loud with ErrProviderKeyUnavailable on broker-unreachable — the
// caller (boot wiring) exits non-zero naming the broker + missing config.
func (s *InferenceKeySource) Connect(ctx context.Context) error {
	return s.pull(ctx, "connect")
}

// Refresh re-pulls on the TTL boundary and atomically swaps the live key.
// A failed refresh raises ErrProviderKeyUnavailable; once the cache TTL is
// crossed with no successful refresh the live key is zeroed rather than
// served stale (a broker-side revocation that fails to propagate surfaces).
func (s *InferenceKeySource) Refresh(ctx context.Context) error {
	return s.pull(ctx, "refresh")
}

// Close closes the binding: the live key is zeroed so a subsequently bound
// call fails loud rather than serving the old key (the provider-SET
// uninstall contract — never a silent no-op serving the old key). The
// backstop timer is stopped. A running Run scheduler is stopped separately by
// cancelling its ctx.
func (s *InferenceKeySource) Close() {
	s.mu.Lock()
	s.closed = true
	s.haveKey = false
	s.serveUntil = time.Time{}
	s.generation++
	s.stopExpiryLocked()
	s.mu.Unlock()
	s.live.Zero()
}

// Run drives the refresh cadence off the broker's expiry: it re-pulls
// s.refreshLead ahead of the serve horizon so a fresh key is seeded before
// the current one expires. It is ctx-cancellable and returns when ctx is
// done (join it on shutdown). A refresh failure is non-fatal to the loop —
// onFetchFailed already drops a stale key past its horizon and the pull
// re-emits its audit event; the loop retries on the next tick. Call Connect
// once before Run so the first key is seeded synchronously (a boot-fail-loud
// point); Run keeps it fresh thereafter.
func (s *InferenceKeySource) Run(ctx context.Context) {
	for {
		s.mu.Lock()
		serveUntil := s.serveUntil
		have := s.haveKey
		s.mu.Unlock()

		wait := s.refreshWait(have, serveUntil)
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			// A refresh failure is already fail-loud + observable inside pull;
			// the loop tolerates it and retries next tick.
			_ = s.Refresh(ctx) //nolint:errcheck // pull failures are audited + drop stale keys internally; the loop retries
		}
	}
}

// refreshWait computes how long the Run scheduler sleeps before the next
// re-pull: refreshLead ahead of the serve horizon, floored so a transient
// error or a very short horizon still bounds the retry cadence.
func (s *InferenceKeySource) refreshWait(have bool, serveUntil time.Time) time.Duration {
	if !have {
		return s.refreshLead
	}
	d := serveUntil.Sub(s.now()) - s.refreshLead
	if d < time.Second {
		return time.Second
	}
	return d
}

// pull performs one connect / refresh pull, collapsing concurrent pulls onto
// a single fetch. On success it swaps the live key + emits the audit event
// (fail-closed: an un-auditable pull fails the operation). On failure it
// raises ErrProviderKeyUnavailable and, when the TTL is crossed, zeroes the
// live key (no stale serve).
func (s *InferenceKeySource) pull(ctx context.Context, phase string) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("llm/credsource: %s cancelled: %w", phase, err)
	}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return fmt.Errorf("%w: source is closed", ErrProviderKeyUnavailable)
	}
	call := s.flight
	leader := false
	if call == nil {
		call = &fetchCall{done: make(chan struct{})}
		s.flight = call
		leader = true
	}
	s.mu.Unlock()

	if leader {
		s.runPull(ctx, call, phase)
	}

	select {
	case <-call.done:
		return call.err
	case <-ctx.Done():
		return fmt.Errorf("llm/credsource: %s cancelled: %w", phase, ctx.Err())
	}
}

// fetchCall is one in-flight pull shared by N collapsed callers.
type fetchCall struct {
	done chan struct{}
	err  error
}

// runPull is the single-flight leader body: one pull shared by every
// collapsed caller.
func (s *InferenceKeySource) runPull(callerCtx context.Context, call *fetchCall, phase string) {
	defer func() {
		s.mu.Lock()
		s.flight = nil
		s.mu.Unlock()
		close(call.done)
	}()

	fetchCtx, cancel := context.WithTimeout(context.WithoutCancel(callerCtx), s.timeout)
	defer cancel()

	key, fingerprint, expiresAt, err := s.fetch(fetchCtx)
	if err != nil {
		s.onFetchFailed()
		call.err = err
		return
	}
	// Audit BEFORE serving: an un-auditable pull fails loud so the key is
	// never silently served without its audit trail. A stale key past its
	// horizon must still be dropped on this failure branch — a fresh fetch
	// that cannot be audited leaves the OLD key on the hot path otherwise.
	if emitErr := s.emitFetched(phase, fingerprint); emitErr != nil {
		s.onFetchFailed()
		call.err = fmt.Errorf("%w: provider %q: emit %s: %w", ErrProviderKeyUnavailable, s.provider, llm.EventTypeProviderCredentialFetched, emitErr)
		return
	}
	// Swap the live key + extend the serve horizon. The bifrost Account's
	// next GetKeysForProvider reads the new value (the atomic key swap).
	if rotErr := s.live.Rotate(key); rotErr != nil {
		s.onFetchFailed()
		call.err = fmt.Errorf("%w: provider %q: seed live key: %w", ErrProviderKeyUnavailable, s.provider, rotErr)
		return
	}
	// Serve horizon honours the broker's advertised expiry (never past its
	// declared validity), and a self-arming backstop timer zeroes the key at
	// the horizon even if the refresh loop stalls (defense in depth).
	s.mu.Lock()
	s.haveKey = true
	s.serveUntil = s.serveHorizon(expiresAt)
	s.generation++
	s.armExpiryLocked()
	s.mu.Unlock()
}

// onFetchFailed enforces the no-stale-cache contract: a pull failure (fetch,
// audit-emit, or seed) at or after the current serve horizon zeroes the live
// key (stop serving a possibly-revoked key); a transient failure well before
// the horizon leaves the still-valid cached key in place. Called on EVERY
// pull-failure branch so a fresh-but-unauditable pull past the horizon never
// leaves the old key live.
func (s *InferenceKeySource) onFetchFailed() {
	s.mu.Lock()
	horizonCrossed := !s.haveKey || !s.now().Before(s.serveUntil)
	if horizonCrossed {
		s.haveKey = false
		s.serveUntil = time.Time{}
		s.generation++
		s.stopExpiryLocked()
	}
	s.mu.Unlock()
	if horizonCrossed {
		// Do not serve a stale key past its refresh contract.
		s.live.Zero()
	}
}

// armExpiryLocked (re)arms the serve-horizon backstop timer. It fires at
// s.serveUntil (real wall time) and zeroes the live key if no newer pull has
// extended the horizon by then — so a revoked key stops serving at its
// horizon REGARDLESS of the refresh loop's health (W2 defense in depth). The
// captured generation invalidates the callback once a newer pull re-arms.
// Caller holds s.mu.
func (s *InferenceKeySource) armExpiryLocked() {
	s.stopExpiryLocked()
	d := s.serveUntil.Sub(s.now())
	if d <= 0 {
		return
	}
	gen := s.generation
	s.expiryTimer = time.AfterFunc(d, func() { s.expireHorizon(gen) })
}

// stopExpiryLocked stops any armed backstop timer. Caller holds s.mu.
func (s *InferenceKeySource) stopExpiryLocked() {
	if s.expiryTimer != nil {
		s.expiryTimer.Stop()
		s.expiryTimer = nil
	}
}

// expireHorizon is the backstop-timer callback: it zeroes the live key once
// the serve horizon is crossed, unless a newer pull (a different generation)
// has since extended it. Fail-closed defense in depth against a stalled
// refresh loop.
func (s *InferenceKeySource) expireHorizon(gen uint64) {
	s.mu.Lock()
	stale := gen == s.generation && s.haveKey && !s.now().Before(s.serveUntil)
	if stale {
		s.haveKey = false
		s.serveUntil = time.Time{}
	}
	s.mu.Unlock()
	if stale {
		s.live.Zero()
	}
}

// providerKeyResponse is the coordinator's strict JSON shape for an
// inference-plane provider key pull.
type providerKeyResponse struct {
	FormatVersion int    `json:"format_version"`
	APIKey        string `json:"api_key"`
	ExpiresIn     int    `json:"expires_in"`
}

// fetch performs one authenticated GET and strict-parses the response. The
// returned error is a wrapped ErrProviderKeyUnavailable with ZERO secret
// bytes; the returns are the pulled key, its fingerprint, and the
// broker-advertised expiry (zero when the broker advertised none).
func (s *InferenceKeySource) fetch(ctx context.Context) (key, fingerprint string, expiresAt time.Time, err error) {
	token := s.getenv(s.authTokenEnv)
	if token == "" {
		return "", "", time.Time{}, fmt.Errorf("%w: provider %q broker %q: env var %q (auth_token_env) is unset or empty",
			ErrProviderKeyUnavailable, s.provider, s.brokerName, s.authTokenEnv)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.endpoint, nil)
	if err != nil {
		return "", "", time.Time{}, fmt.Errorf("%w: provider %q broker %q: build request: %w",
			ErrProviderKeyUnavailable, s.provider, s.brokerName, err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return "", "", time.Time{}, fmt.Errorf("%w: provider %q broker %q: GET: %w",
			ErrProviderKeyUnavailable, s.provider, s.brokerName, err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseByte)) //nolint:errcheck // a partial read still yields a usable error; the status code drives the branch
	if resp.StatusCode/100 != 2 {
		return "", "", time.Time{}, fmt.Errorf("%w: provider %q broker %q: status %d",
			ErrProviderKeyUnavailable, s.provider, s.brokerName, resp.StatusCode)
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	var pr providerKeyResponse
	if derr := dec.Decode(&pr); derr != nil {
		return "", "", time.Time{}, fmt.Errorf("%w: provider %q broker %q: malformed response: %w",
			ErrProviderKeyUnavailable, s.provider, s.brokerName, derr)
	}
	if pr.FormatVersion != supportedFormatVersion {
		return "", "", time.Time{}, fmt.Errorf("%w: provider %q broker %q: unsupported format_version %d (runtime accepts %d)",
			ErrProviderKeyUnavailable, s.provider, s.brokerName, pr.FormatVersion, supportedFormatVersion)
	}
	if pr.APIKey == "" {
		return "", "", time.Time{}, fmt.Errorf("%w: provider %q broker %q: response missing api_key",
			ErrProviderKeyUnavailable, s.provider, s.brokerName)
	}
	if pr.ExpiresIn < 0 {
		return "", "", time.Time{}, fmt.Errorf("%w: provider %q broker %q: negative expires_in %d",
			ErrProviderKeyUnavailable, s.provider, s.brokerName, pr.ExpiresIn)
	}
	if pr.ExpiresIn > 0 {
		expiresAt = s.now().Add(time.Duration(pr.ExpiresIn) * time.Second)
	}
	return pr.APIKey, llm.Fingerprint(pr.APIKey), expiresAt, nil
}

// serveHorizon computes the in-memory serve horizon: the EARLIER of the
// broker-advertised expiry and now+cacheTTL. A zero expiry ("none
// advertised") yields the cacheTTL cap alone. The key is NEVER served past
// the broker's own declared validity — a broker key advertised
// `expires_in: 30` stops serving at +30s even when cacheTTL is 5m (the
// no-stale-past-revocation contract; parity with the OAuth `remote` source).
func (s *InferenceKeySource) serveHorizon(expiresAt time.Time) time.Time {
	capped := s.now().Add(s.cacheTTL)
	if expiresAt.IsZero() || capped.Before(expiresAt) {
		return capped
	}
	return expiresAt
}

// emitFetched redacts (defence in depth) and publishes the runtime-scoped
// success event. RETURNS a publish failure so the caller fails the pull
// closed (a pull that cannot be audited is never served).
func (s *InferenceKeySource) emitFetched(phase, fingerprint string) error {
	payload := llm.LLMProviderCredentialFetchedPayload{
		RuntimePrincipal: s.principal,
		Broker:           s.brokerName,
		Provider:         s.provider,
		KeyFingerprint:   fingerprint,
		Phase:            phase,
		Outcome:          "ok",
		OccurredAt:       s.now().UTC(),
	}
	// The pull fires outside any request; build a runtime-scoped ctx carrying
	// the reserved runtime-principal identity so the audit / bus path has the
	// structural triple it requires (the authoritative attribution is the
	// payload's RuntimePrincipal, not this triple).
	ctx := s.runtimeCtx()
	if _, err := s.redactor.Redact(ctx, payload); err != nil {
		return fmt.Errorf("redact: %w", err)
	}
	return s.bus.Publish(ctx, events.Event{
		Type:     llm.EventTypeProviderCredentialFetched,
		Identity: s.runtimeQuad(),
		Payload:  payload,
	})
}

// runtimeQuad is the reserved runtime-principal identity carried on the
// pull's Event.Identity — a transport requirement (the bus needs a triple),
// never an isolation principal. The session slot carries the principal id so
// distinct runtimes are distinguishable on the trail.
func (s *InferenceKeySource) runtimeQuad() identity.Quadruple {
	return identity.Quadruple{Identity: identity.Identity{
		TenantID:  RuntimePrincipalTenant,
		UserID:    RuntimePrincipalUser,
		SessionID: s.principal,
	}}
}

// runtimeCtx builds a ctx carrying the reserved runtime-principal identity
// for the redactor + bus publish.
func (s *InferenceKeySource) runtimeCtx() context.Context {
	ctx, _ := identity.With(context.Background(), s.runtimeQuad().Identity) //nolint:errcheck // the reserved runtime-principal triple is always complete by construction
	return ctx
}

// isLoopbackHostname reports whether a URL hostname is loopback for the TLS
// carve-out: a numeric loopback IP (127.0.0.0/8 or ::1) or the literal
// "localhost".
func isLoopbackHostname(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
