// Package tokenexchange ships Harbor's pull-based, non-interactive
// external-credential acquisition strategy: a driver on the OAuth
// flow-strategy registry that obtains a downstream tool credential (a
// user's Microsoft 365 token, a Google Workspace token — foreign-IdP
// tokens used to call third-party tools) from an operator-configured
// external credential broker (a fleet orchestrator, an enterprise
// token vault, an STS) via an RFC-8693-shaped token exchange, instead
// of Harbor's interactive authorization-code flow.
//
// # Why a pull, not a push
//
// A fleet orchestrator holding each user's downstream credentials in
// ONE central place wants to provide them to whichever runtime's tool
// call needs them, instead of every runtime independently acquiring
// and sealing its own copy (N consents, N encrypted copies). Harbor
// answers with a PULL: the runtime, at token-miss time, presents its
// own env-indirected broker credential plus the VERIFIED ctx identity
// triple and receives a short-lived, audience-bound downstream token.
// A push (a per-run credential arriving in-band over the Protocol) is
// rejected as credential passthrough — the runtime cannot verify its
// provenance / audience / subject binding, and it rides channels the
// codebase is engineered to keep secrets out of.
//
// # Never persisted
//
// Brokered tokens are TTL-cached in memory only, single-flight per
// (scope, tenant, user, source). The shared TokenStore's Put is NEVER
// called: the broker stays the single source of truth, so revocation
// at the broker is not defeated by a live sealed copy in N runtimes
// (the shadow-source-of-truth smell read southbound).
//
// Cache TTL semantics: a token is served from cache until the EARLIER
// of the broker-advertised expiry and the operator cache-TTL cap
// (extra.cache_ttl_cap, default 5m) — a token is never served past the
// broker's own expiry. Separately, RE-EXCHANGE is rate-floored at 30s
// per key: within 30s of the previous exchange a re-exchange fails
// loudly (a typed exchange error naming the floor) rather than
// round-tripping the broker; once the window elapses the next Token()
// call re-exchanges normally. The floor only ever bites when the
// broker advertises an expiry shorter than 30s (a broker
// misconfiguration) — the net effect there is at most one exchange per
// 30s window with loud failures in between, never one exchange + one
// audit event per tool call.
//
// Cache granularity: the subject_token presented to the broker carries
// the full verified (tenant, user, session) triple, but the cache and
// single-flight key deliberately scope by (binding scope, tenant,
// user, source) — the user-bound granularity. Two sessions of the same
// (tenant, user) share the cached credential by design; a broker that
// scopes grants per-session should advertise a short expiry (the cache
// never outlives it).
//
// # Fail loud, one mode per source
//
// Broker unreachable / 5xx / a non-consent OAuth error → a wrapped
// ErrExchangeFailed to the run. NEVER a silent fallback to the
// interactive flow — that would silently void the central-custody
// policy (N consent prompts reappear). A source is EITHER interactive
// (`oauth2`) or brokered (`tokenexchange`), declared in config — no
// dual path. The interactive-flow methods (InitiateFlow / CompleteFlow
// / DenyFlow) return the typed auth.ErrNonInteractive sentinel.
//
// # One pause path
//
// A broker `consent_required`-class refusal surfaces the SAME typed
// *auth.ErrAuthRequired the interactive flow uses (AuthorizeURL = the
// broker-supplied consent URL when present), so the run parks on the
// unified pause/resume primitive. After the user consents centrally, a
// resume re-drives Token() against the now-granting broker. A bare
// resume against a still-declining broker simply re-parks.
//
// # Trust model, named honestly
//
// V1 is RFC 8693 impersonation semantics: the broker trusts the
// runtime's client credential to assert the subject triple
// (subject_token_type = urn:harbor:oauth:token-type:identity-triple, a
// Harbor-defined URN). The northbound JWT is deliberately NOT
// forwarded as subject_token — durable runs outlive the initiating
// request's JWT, so request-token forwarding breaks exactly the
// durable-run cases Harbor exists for. A signed runtime-side subject
// assertion is the named post-V1 upgrade path. Every actual exchange
// emits the canonical tool.credential_exchanged event (zero token
// bytes).
//
// # Credential source
//
// The runtime's OWN broker client credential (`client_id` /
// `client_secret`) is resolved through the §4.4 credential-source seam
// (`internal/tools/auth/credsource`), NOT captured at construction. With
// the default `env` source the credential is resolved once at boot
// (zero behavior change); with the `remote` source it is pulled from a
// coordinator endpoint LAZILY at the first exchange — TTL-cached in
// memory, single-flight, fail-loud. A resolution failure fails the
// exchange with the typed credsource.ErrCredentialSourceUnavailable; the
// driver NEVER falls back to env, to an unauthenticated call, or to the
// interactive flow (§13). A resolved credential with an empty component
// fails the exchange loud.
//
// # Fail-loud at construction
//
// Operator-facing seams demand explicit configuration. The driver
// fails closed on: a nil CredentialSource (BuildProviders always threads
// one), empty TokenURL (the broker endpoint — auth_url / redirect_url
// are interactive-flow fields and are NOT required here), a malformed
// extra.cache_ttl_cap, and missing deps (Store / Bus / Redactor /
// Coordinator).
package tokenexchange

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/hurtener/Harbor/internal/audit"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/auth"
	"github.com/hurtener/Harbor/internal/tools/auth/credsource"
)

// DriverName is the canonical name the driver registers under. The
// `internal/config` validator's `allowedOAuthDrivers` allowlist mirrors
// this constant.
const DriverName = "tokenexchange"

// RFC-8693 token-exchange wire constants.
const (
	// grantTypeTokenExchange is RFC 8693 §2.1's grant_type.
	grantTypeTokenExchange = "urn:ietf:params:oauth:grant-type:token-exchange" //nolint:gosec // G101 false positive: RFC 8693 grant_type URN, not a credential
	// subjectTokenTypeIdentityTriple is Harbor's own subject-token-type
	// URN. RFC 8693 permits impersonation-style deployments to define
	// their own subject-token types; the broker interprets the
	// base64url-JSON identity triple this URN labels.
	subjectTokenTypeIdentityTriple = "urn:harbor:oauth:token-type:identity-triple" //nolint:gosec // G101 false positive: subject-token-type URN, not a credential
)

// TTL knobs. A cached token is served until min(broker expiry,
// now+cacheTTLCap) — never past the broker expiry; re-exchange is
// rate-floored at minTTL per key.
const (
	// defaultCacheTTLCap bounds how long a brokered token is served
	// from cache when the broker advertises a longer (or no) expiry.
	// Overridable per provider via extra.cache_ttl_cap (values below
	// minTTL are rejected at construction — fail-loud, not clamped).
	defaultCacheTTLCap = 5 * time.Minute
	// minTTL is the floor on the RE-EXCHANGE rate (not the serve
	// window — a token is never served past the broker's expiry). It
	// guards against a pathological zero-/near-zero-TTL broker
	// spamming the broker and the audit bus with one exchange per
	// Token() call: a re-exchange for a key whose previous exchange is
	// younger than minTTL fails loudly instead.
	minTTL = 30 * time.Second
)

// Sentinel errors specific to the tokenexchange driver. Broker /
// upstream failures reuse the parent package's sentinels
// (auth.ErrExchangeFailed, auth.ErrAuthRequired, auth.ErrIdentityRequired).
var (
	// ErrMissingCredentialSource — cfg.CredentialSource was nil at
	// construction. BuildProviders always threads one (env or remote);
	// a nil source is a direct-construction bug.
	ErrMissingCredentialSource = errors.New("auth/tokenexchange: CredentialSource is nil (BuildProviders threads the env/remote source; direct callers pass credsource.Static)")
	// ErrMissingClientID — the resolved credential carried an empty
	// client_id. Surfaced at exchange time (the credential resolves
	// through the source seam — at boot for env, lazily for remote).
	ErrMissingClientID = errors.New("auth/tokenexchange: resolved client_id is empty")
	// ErrMissingClientSecret — the resolved credential carried an empty
	// client_secret.
	ErrMissingClientSecret = errors.New("auth/tokenexchange: resolved client_secret is empty")
	// ErrMissingTokenURL — cfg.TokenURL was empty. The broker's
	// RFC-8693 token-exchange endpoint is mandatory; auth_url /
	// redirect_url are interactive-flow fields and are NOT used.
	ErrMissingTokenURL = errors.New("auth/tokenexchange: token_url must not be empty (the credential broker's RFC-8693 token-exchange endpoint)")
	// ErrBadCacheTTLCap — extra.cache_ttl_cap did not parse as a Go
	// duration (e.g. "5m", "90s").
	ErrBadCacheTTLCap = errors.New("auth/tokenexchange: extra.cache_ttl_cap must be a Go duration (e.g. \"5m\", \"90s\")")
	// ErrMissingDeps — a mandatory FactoryDep (Store / Bus / Redactor
	// / Coordinator) was nil.
	ErrMissingDeps = errors.New("auth/tokenexchange: Store / Bus / Redactor / Coordinator are mandatory")
	// ErrAudienceMismatch — a JWT-shaped exchanged access token's `aud` claim
	// excluded the boot-declared RFC 8707 resource indicator. The exchange
	// fails loud and the token is NEVER cached (a confused-deputy defence). It
	// is an `aud`-claim comparison only, never signature verification — Harbor
	// has no keying relationship with the broker's AS.
	ErrAudienceMismatch = errors.New("auth/tokenexchange: exchanged token audience excludes the declared resource_indicator")
)

// RFC 8693 actor-token constants.
const (
	// actorTokenTypeInvokingAgent labels the acting-principal (agent_id)
	// actor_token — a Harbor-defined URN mirroring the identity-triple
	// subject-token-type. The broker's AS binds/cross-checks the subject
	// (requesting principal) against this acting principal.
	actorTokenTypeInvokingAgent = "urn:harbor:oauth:token-type:invoking-agent" //nolint:gosec // G101 false positive: actor-token-type URN, not a credential
)

// init self-registers the `tokenexchange` driver under its canonical
// name. `internal/drivers/prod` blank-imports this package so the
// registration fires at process boot (the §4.4 seam pattern).
func init() {
	auth.MustRegister(DriverName, New)
}

// New constructs the `tokenexchange` driver's OAuthProvider for one
// operator-config entry. Registered as the driver's auth.Factory; the
// dev stack never calls it directly — auth.Resolve dispatches by name.
func New(cfg auth.ProviderConfig, deps auth.FactoryDeps) (auth.OAuthProvider, error) {
	if cfg.CredentialSource == nil {
		return nil, fmt.Errorf("%w (provider name=%q)", ErrMissingCredentialSource, cfg.Name)
	}
	if cfg.TokenURL == "" {
		return nil, fmt.Errorf("%w (provider name=%q)", ErrMissingTokenURL, cfg.Name)
	}
	if deps.Store == nil || deps.Bus == nil || deps.Redactor == nil || deps.Coordinator == nil {
		return nil, fmt.Errorf("%w (provider name=%q)", ErrMissingDeps, cfg.Name)
	}

	source := tools.ToolSourceID(cfg.Name)

	// Audience resolution, ceiling-first (the credential-plane invariant:
	// the audience is boot-declared, never derived from the caller-chosen
	// provider name). When a boot ceiling is declared it is the authority;
	// otherwise the legacy behaviour is preserved (extra.audience override,
	// else the source ID) so existing configs are byte-compatible.
	audience := string(source)
	if v := strings.TrimSpace(cfg.Extra["audience"]); v != "" {
		audience = v
	}
	if v := strings.TrimSpace(cfg.Audience); v != "" {
		audience = v
	}

	// Scope ceiling: when declared, the requested scopes are intersected
	// against it (an out-of-ceiling scope is dropped, never honoured). No
	// ceiling preserves the legacy pass-through of the requested scopes.
	requestedScopes := append([]string(nil), cfg.Scopes...)
	effectiveScopes := requestedScopes
	if len(cfg.ScopeCeiling) > 0 {
		effectiveScopes = intersectScopes(requestedScopes, cfg.ScopeCeiling)
	}

	cacheTTLCap := defaultCacheTTLCap
	if raw := strings.TrimSpace(cfg.Extra["cache_ttl_cap"]); raw != "" {
		parsed, err := time.ParseDuration(raw)
		if err != nil {
			return nil, fmt.Errorf("%w (provider name=%q, got %q): %w", ErrBadCacheTTLCap, cfg.Name, raw, err)
		}
		// Fail loudly on a sub-floor cap instead of silently clamping —
		// the operator asked for something the driver will not honour.
		if parsed < minTTL {
			return nil, fmt.Errorf("%w (provider name=%q): extra.cache_ttl_cap %q is below the %s re-exchange floor",
				ErrBadCacheTTLCap, cfg.Name, raw, minTTL)
		}
		cacheTTLCap = parsed
	}

	brokerHost := ""
	if u, err := url.Parse(cfg.TokenURL); err == nil {
		brokerHost = u.Host
	}

	// The token-exchange POST carries the org's OAuth client_id /
	// client_secret in its form body. Harden the client to the same bar as
	// the discovery client and the credential-source fetch: refuse a
	// private-range / loopback destination at dial time (post-DNS),
	// disable the proxy, and REFUSE every redirect — Go replays the POST
	// body on 307/308, so a redirecting token endpoint would re-POST the
	// client_secret to the redirect target. A caller-supplied client is
	// shallow-copied and re-hardened (redirect refusal), never mutated in
	// place, mirroring the credsource/remote precedent.
	clientTimeout := 30 * time.Second
	httpClient := hardenTokenExchangeClient(deps.HTTPClient, clientTimeout)
	clock := deps.Clock
	if clock == nil {
		clock = time.Now
	}

	return &provider{
		name:                   cfg.Name,
		source:                 source,
		tokenURL:               cfg.TokenURL,
		brokerHost:             brokerHost,
		credSource:             cfg.CredentialSource,
		scopes:                 effectiveScopes,
		audience:               audience,
		resourceIndicator:      strings.TrimSpace(cfg.ResourceIndicator),
		includeActorToken:      cfg.IncludeActorToken,
		allowedDownstreamHosts: append([]string(nil), cfg.AllowedDownstreamHosts...),
		cacheTTLCap:            cacheTTLCap,
		httpClient:             httpClient,
		now:                    clock,
		bus:                    deps.Bus,
		redactor:               deps.Redactor,
		coordinator:            deps.Coordinator,
		store:                  deps.Store, // held to honour the FactoryDeps contract; Put is NEVER called
		cache:                  make(map[string]cachedToken),
		gens:                   make(map[string]uint64),
		flight:                 make(map[string]*exchangeCall),
	}, nil
}

// ErrTokenEndpointRedirect is the typed sentinel the hardened
// token-exchange client refuses a redirect with. The token-exchange POST
// carries the org's client_id / client_secret; Go replays the request body
// on a 307/308, so a redirecting token endpoint is a credential-exfil
// fault, never a hop to follow. Wrapped under auth.ErrExchangeFailed so
// callers observe the exchange as failed.
var ErrTokenEndpointRedirect = errors.New("auth/tokenexchange: token endpoint attempted a redirect (the token POST carries client_id/client_secret; a redirecting endpoint is a fault, not a hop)")

// ErrPrivateDialRefused is the typed sentinel the hardened client refuses a
// private-range / link-local dial destination with (post-DNS) — the
// DNS-rebinding backstop. Wrapped under auth.ErrExchangeFailed at the call
// site. Loopback is deliberately NOT refused (see hardenTokenExchangeClient).
var ErrPrivateDialRefused = errors.New("auth/tokenexchange: dial to a private/link-local address refused (the token POST carries client_id/client_secret)")

// hardenTokenExchangeClient builds (or re-hardens) the HTTP client used for
// the credential-bearing token-exchange POST. A nil supplied client yields
// a fully hardened client: a dial-time control refusing private-range /
// link-local resolved addresses (the DNS-rebinding backstop), no proxy, and
// a redirect refusal. A supplied client (tests, custom embedders) is
// shallow-copied and gets ONLY the redirect refusal — its transport / dial
// path is preserved, exactly the credsource/remote posture (remote.go: a
// caller client is cloned and only CheckRedirect is set).
//
// Loopback carve-out: unlike the discovery client (which fetches
// attacker-influenceable metadata URLs and so refuses loopback too), the
// token_url here is BOOT-DECLARED / config-only — never wire-derived — and a
// broker on a localhost sidecar (a token-vault agent, a fixture) is a
// legitimate deployment. So loopback is allowed, mirroring the
// credsource/remote loopback carve-out for its own bearer-carrying client.
// The DNS-rebinding threat this guard defends is a hostname resolving into
// the RFC1918 / link-local space, which stays refused.
func hardenTokenExchangeClient(supplied *http.Client, timeout time.Duration) *http.Client {
	if supplied != nil {
		clone := *supplied
		clone.CheckRedirect = refuseTokenEndpointRedirect
		return &clone
	}
	dialer := &net.Dialer{Timeout: timeout}
	// Control runs AFTER DNS resolution with the resolved ip:port — the
	// load-bearing backstop (mirrors discovery.go's post-resolution guard).
	// A hostname that resolves into private / link-local space is refused
	// here, fail-closed.
	dialer.Control = func(_, address string, _ syscall.RawConn) error {
		host, _, splitErr := net.SplitHostPort(address)
		if splitErr != nil {
			host = address
		}
		ip := net.ParseIP(host)
		if ip == nil {
			return fmt.Errorf("%w: unresolved dial address %q", ErrPrivateDialRefused, address)
		}
		if isBlockedDialIP(ip) {
			return fmt.Errorf("%w: %s", ErrPrivateDialRefused, host)
		}
		return nil
	}
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			Proxy:       nil, // never route the credential POST through a proxy
			DialContext: dialer.DialContext,
		},
		CheckRedirect: refuseTokenEndpointRedirect,
	}
}

// refuseTokenEndpointRedirect is the CheckRedirect hook: it refuses every
// redirect so the credential-bearing POST body is never replayed to a
// redirect target.
func refuseTokenEndpointRedirect(_ *http.Request, _ []*http.Request) error {
	return ErrTokenEndpointRedirect
}

// isBlockedDialIP reports whether ip is in a range the credential POST must
// never reach: link-local, unique-local, RFC 1918 private, or the
// unspecified address. Loopback is deliberately EXCLUDED (the boot-declared
// broker on a localhost sidecar is a legitimate deployment — see
// hardenTokenExchangeClient). The DNS-rebinding backstop this powers refuses
// a hostname that resolves into the blocked space.
func isBlockedDialIP(ip net.IP) bool {
	return ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() ||
		ip.IsPrivate() ||
		ip.IsUnspecified()
}

// intersectScopes returns the requested scopes that also appear in the
// ceiling, preserving the requested order and dropping duplicates. An
// out-of-ceiling requested scope is dropped, never honoured.
func intersectScopes(requested, ceiling []string) []string {
	allow := make(map[string]struct{}, len(ceiling))
	for _, s := range ceiling {
		allow[s] = struct{}{}
	}
	var out []string
	seen := make(map[string]struct{}, len(requested))
	for _, s := range requested {
		if _, ok := allow[s]; !ok {
			continue
		}
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

// provider is the concrete tokenexchange OAuthProvider.
//
// Concurrent reuse: every field below the mutexes is set once at
// construction and read-only after. The in-memory TTL cache and the
// single-flight table are the only mutable state, each guarded by its
// own mutex. Per-run identity is read from ctx on every call, never
// from the provider.
type provider struct {
	name       string
	source     tools.ToolSourceID
	tokenURL   string
	brokerHost string
	// credSource resolves the runtime's OWN broker client credential
	// (`client_id` / `client_secret`) — at boot for the env source,
	// lazily at exchange time for the remote source. Never
	// logged; the resolved values ride the broker request body only.
	credSource credsource.Source
	scopes     []string
	audience   string
	// resourceIndicator is the boot-declared RFC 8707 `resource` value carried
	// on every exchange request and verified against the returned token's
	// `aud` claim (when JWT-shaped). Empty disables both. Set once at
	// construction; read-only.
	resourceIndicator string
	// includeActorToken opts this provider into carrying the run's verified
	// acting principal (agent_id) as an RFC 8693 actor_token. Set once at
	// construction; read-only.
	includeActorToken bool
	// allowedDownstreamHosts is the boot-declared sink allow-list — the
	// downstream connection hosts the exchanged bearer may be injected
	// into. Set once at construction; the MCP southbound binding refuses a
	// connection host absent from it (fail-closed).
	allowedDownstreamHosts []string
	cacheTTLCap            time.Duration
	httpClient             *http.Client
	now                    func() time.Time
	bus                    events.EventBus
	redactor               audit.Redactor
	coordinator            pauseresume.Coordinator

	// store is the shared TokenStore. Held only to honour the
	// FactoryDeps mandatory-dep contract; brokered tokens live in the
	// in-memory cache and store.Put is NEVER called (the broker stays
	// the single source of truth).
	store auth.TokenStore

	// cacheMu guards cache + gens. Short critical sections only — never
	// held across the broker HTTP round-trip.
	cacheMu sync.Mutex
	cache   map[string]cachedToken
	// gens is the per-key revocation generation counter. Revoke bumps
	// the key's generation; an exchange that was in flight when the
	// Revoke landed observes the bump and declines to (re)populate the
	// cache — closing the "revoke racing an in-flight exchange is
	// silently lost" window.
	gens map[string]uint64

	// flightMu guards flight — the per-(scope,tenant,user,source)
	// single-flight table collapsing a burst of concurrent misses onto
	// one broker request. The flight's broker round-trip runs on a
	// context DETACHED from the initiating caller (values preserved,
	// cancellation dropped, bounded by the HTTP client timeout) so
	// cancelling one collapsed caller never poisons the others.
	flightMu sync.Mutex
	flight   map[string]*exchangeCall

	closed atomic.Bool
}

// cachedToken is one in-memory brokered token plus its serve horizon
// and exchange timestamp (the re-exchange rate-floor input). A stale
// entry (past serveUntil) is deliberately retained until the next
// successful exchange overwrites it — the exchangedAt timestamp is
// what rate-floors a re-exchange against a short-expiry broker.
type cachedToken struct {
	token       auth.Token
	serveUntil  time.Time
	exchangedAt time.Time
}

// exchangeCall is one in-flight broker exchange shared by N callers.
type exchangeCall struct {
	done  chan struct{}
	token auth.Token
	err   error
}

// consentError is the internal signal that the broker refused with a
// `consent_required`-class error. It is NOT returned to callers: the
// per-caller Token frame converts it into a *auth.ErrAuthRequired
// parked under that caller's own ctx identity (so N collapsed callers
// each park correctly, rather than sharing one session's pause).
type consentError struct {
	consentURL string
}

func (e *consentError) Error() string { return "auth/tokenexchange: broker requires consent" }

// Token implements auth.OAuthProvider.Token. The `requested` source is
// retargeted onto the operator-configured source (the V1 one-provider-
// one-attachment model, mirroring the oauth2 driver).
func (p *provider) Token(ctx context.Context, _ tools.ToolSourceID) (auth.Token, error) {
	if p.closed.Load() {
		return auth.Token{}, auth.ErrProviderClosed
	}
	if err := ctx.Err(); err != nil {
		return auth.Token{}, fmt.Errorf("auth/tokenexchange: Token cancelled: %w", err)
	}
	id, err := p.identityFromCtx(ctx)
	if err != nil {
		return auth.Token{}, err
	}
	key := p.cacheKey(id)

	// Hot path: fresh cache hit → return immediately, emit nothing.
	p.cacheMu.Lock()
	ct, ok := p.cache[key]
	if ok && p.now().Before(ct.serveUntil) {
		tok := ct.token
		p.cacheMu.Unlock()
		return tok, nil
	}
	p.cacheMu.Unlock()

	tok, err := p.exchangeSingleFlight(ctx, id, key)
	if err != nil {
		var ce *consentError
		if errors.As(err, &ce) {
			// Park THIS caller under its own ctx identity on the unified
			// pause primitive; a resume re-drives Token().
			return auth.Token{}, p.buildConsentRequired(ctx, id, ce)
		}
		return auth.Token{}, err
	}
	return tok, nil
}

// exchangeSingleFlight collapses a burst of concurrent misses for one
// (scope, tenant, user, source) key onto a single broker request,
// mirroring the interactive Provider's refresh single-flight. The
// broker round-trip runs in its own goroutine on a context DETACHED
// from the initiating caller's cancellation (values — identity, trace
// — are preserved; the deadline is the HTTP client timeout), so a
// cancelled caller never poisons the collapsed waiters: every caller,
// the initiator included, waits on the shared flight result or its
// OWN ctx, whichever resolves first.
func (p *provider) exchangeSingleFlight(ctx context.Context, id identity.Identity, key string) (auth.Token, error) {
	p.flightMu.Lock()
	call, inflight := p.flight[key]
	if !inflight {
		call = &exchangeCall{done: make(chan struct{})}
		p.flight[key] = call
		go p.runExchange(ctx, id, key, call)
	}
	p.flightMu.Unlock()

	select {
	case <-call.done:
		return call.token, call.err
	case <-ctx.Done():
		return auth.Token{}, fmt.Errorf("auth/tokenexchange: Token cancelled: %w", ctx.Err())
	}
}

// runExchange is the single-flight worker body: one broker exchange
// shared by every caller collapsed onto the flight. It performs the
// exchange, emits tool.credential_exchanged exactly once, and caches
// the result. The goroutine's lifetime is bounded by the HTTP client
// timeout (it deliberately outlives a cancelled initiator so the
// remaining waiters still get their result — the flight, not the
// caller, owns the round-trip).
func (p *provider) runExchange(callerCtx context.Context, id identity.Identity, key string, call *exchangeCall) {
	defer func() {
		// Ordering is load-bearing: the flight MUST leave the table
		// BEFORE its result is published. Once done closes, waiters are
		// released and a released caller may immediately issue a fresh
		// Token(); were the completed flight still findable in the
		// table, that caller would join it and receive THIS attempt's
		// stale result (e.g. a rate-floor error frozen at an earlier
		// clock instant) instead of starting a new exchange.
		p.flightMu.Lock()
		delete(p.flight, key)
		p.flightMu.Unlock()
		close(call.done)
	}()

	// Detach from the initiating caller's cancellation, keeping its
	// values (identity, trace context); bound by the HTTP client
	// timeout so a wedged broker cannot leak the goroutine.
	timeout := p.httpClient.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(callerCtx), timeout)
	defer cancel()

	now := p.now()
	p.cacheMu.Lock()
	if ct, ok := p.cache[key]; ok {
		// Re-check under the flight: a flight that completed between
		// the caller's miss and this flight's start may have populated
		// the cache.
		if now.Before(ct.serveUntil) {
			call.token = ct.token
			p.cacheMu.Unlock()
			return
		}
		// Re-exchange rate floor. This only bites when the broker
		// advertises an expiry shorter than the floor (a broker
		// misconfiguration) — fail loud naming the constraint instead
		// of one broker exchange + one audit event per tool call.
		if now.Before(ct.exchangedAt.Add(minTTL)) {
			p.cacheMu.Unlock()
			call.err = fmt.Errorf("%w: broker %s advertised a token expiry shorter than the %s re-exchange floor (previous exchange %s ago); raise the broker's expires_in",
				auth.ErrExchangeFailed, p.brokerHost, minTTL, now.Sub(ct.exchangedAt))
			return
		}
	}
	gen := p.gens[key]
	p.cacheMu.Unlock()

	tok, meta, err := p.exchange(ctx, id)
	if err != nil {
		call.err = err
		return
	}

	// Audit the ACTUAL exchange before caching. A redaction / emit
	// failure fails loud (§7 external-credential posture): the token is
	// NOT cached and the error propagates, so the exchange is retried
	// rather than silently served without its audit trail.
	if err := p.emitCredentialExchanged(ctx, id, tok, meta); err != nil {
		call.err = err
		return
	}

	p.cacheMu.Lock()
	// A Revoke that raced this exchange bumped the key's generation —
	// honour it by NOT caching. The collapsed callers still receive the
	// freshly minted token (the broker really issued it, and the audit
	// event fired); the next Token() call re-exchanges.
	if p.gens[key] == gen {
		p.cache[key] = cachedToken{
			token:       tok,
			serveUntil:  p.serveUntil(tok.ExpiresAt),
			exchangedAt: p.now(),
		}
	}
	p.cacheMu.Unlock()
	call.token = tok
}

// serveUntil computes the cache-serve horizon for a freshly exchanged
// token: the EARLIER of the broker-advertised expiry and
// now+cacheTTLCap. A token is never served from cache past the
// broker's own expiry (the minTTL floor rate-limits re-exchange, not
// the serve window). A zero brokerExpiry ("no expiry advertised")
// yields the cap alone.
func (p *provider) serveUntil(brokerExpiry time.Time) time.Time {
	capped := p.now().Add(p.cacheTTLCap)
	if brokerExpiry.IsZero() || capped.Before(brokerExpiry) {
		return capped
	}
	return brokerExpiry
}

// brokerResponse is the RFC-8693 token-exchange response (a superset of
// the RFC 6749 §5.1 shape). Only the fields Harbor consults are typed.
type brokerResponse struct {
	AccessToken     string `json:"access_token"`
	IssuedTokenType string `json:"issued_token_type"`
	TokenType       string `json:"token_type"`
	ExpiresIn       int    `json:"expires_in"`
	Scope           string `json:"scope"`
}

// brokerError is the RFC 6749 §5.2 error body shape.
type brokerError struct {
	Err              string `json:"error"`
	ErrorDescription string `json:"error_description"`
	// ConsentURL / VerificationURI carry the broker-supplied central
	// consent URL when the refusal is consent_required-class. Both
	// spellings are accepted (brokers vary).
	ConsentURL      string `json:"consent_url"`
	VerificationURI string `json:"verification_uri"`
}

// exchangeMeta records the post-exchange verification outcomes threaded onto
// the tool.credential_exchanged audit event: whether the returned token's
// audience was verified against the declared resource indicator, and whether
// an actor_token (the verified acting principal) rode the request.
type exchangeMeta struct {
	audienceVerified bool
	actorAsserted    bool
}

// exchange performs one RFC-8693 token-exchange POST against the broker
// and maps the outcome onto (token / *consentError / ErrExchangeFailed).
func (p *provider) exchange(ctx context.Context, id identity.Identity) (auth.Token, exchangeMeta, error) {
	// Resolve the runtime's OWN broker client credential through the
	// §4.4 credential-source seam. For the env source this returns the
	// boot-resolved value (zero behavior change); for the remote source
	// this is the lazy, TTL-cached, single-flight coordinator pull. A
	// resolution failure fails the exchange loud (credsource carries the
	// typed ErrCredentialSourceUnavailable; no fallback — §13).
	cred, err := p.credSource.Resolve(ctx)
	if err != nil {
		return auth.Token{}, exchangeMeta{}, fmt.Errorf("%w: resolve broker client credential: %w", auth.ErrExchangeFailed, err)
	}
	if cred.ClientID == "" {
		return auth.Token{}, exchangeMeta{}, fmt.Errorf("%w: %w (provider name=%q)", auth.ErrExchangeFailed, ErrMissingClientID, p.name)
	}
	if cred.ClientSecret == "" {
		return auth.Token{}, exchangeMeta{}, fmt.Errorf("%w: %w (provider name=%q)", auth.ErrExchangeFailed, ErrMissingClientSecret, p.name)
	}

	subjectToken, err := encodeSubjectToken(id)
	if err != nil {
		return auth.Token{}, exchangeMeta{}, fmt.Errorf("%w: encode subject token: %w", auth.ErrExchangeFailed, err)
	}

	form := url.Values{}
	form.Set("grant_type", grantTypeTokenExchange)
	form.Set("subject_token_type", subjectTokenTypeIdentityTriple)
	form.Set("subject_token", subjectToken)
	form.Set("audience", p.audience)
	if len(p.scopes) > 0 {
		form.Set("scope", strings.Join(p.scopes, " "))
	}
	// RFC 8707 resource indicator: when declared, ride the exchange as the
	// `resource` form parameter (the audience the exchanged token binds to).
	// Empty preserves the byte-identical pre-resource request shape.
	if p.resourceIndicator != "" {
		form.Set("resource", p.resourceIndicator)
	}
	// RFC 8693 actor_token: when opted in AND the run carries a VERIFIED
	// acting principal (agent_id, read from ctx — never a client-named field),
	// carry it as the actor_token so the broker's AS can bind/cross-check the
	// acting principal against the subject. Absent either → byte-identical to
	// today (no actor_token sent).
	var actorAsserted bool
	if p.includeActorToken {
		if agentID, ok := tools.InvokingAgentFrom(ctx); ok && agentID != "" {
			form.Set("actor_token", agentID)
			form.Set("actor_token_type", actorTokenTypeInvokingAgent)
			actorAsserted = true
		}
	}
	// Runtime→broker client authentication (§7 rule 2; the credential was
	// resolved through the credential-source seam above).
	form.Set("client_id", cred.ClientID)
	form.Set("client_secret", cred.ClientSecret)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return auth.Token{}, exchangeMeta{}, fmt.Errorf("%w: build request: %w", auth.ErrExchangeFailed, err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return auth.Token{}, exchangeMeta{}, fmt.Errorf("%w: POST broker %s: %w", auth.ErrExchangeFailed, p.brokerHost, err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16)) //nolint:errcheck // a partial read still yields a usable error message; the status code drives the branch
	if resp.StatusCode/100 != 2 {
		// Decode the OAuth error body to distinguish a consent refusal
		// (park path) from a hard failure (fail-loud path).
		var be brokerError
		_ = json.Unmarshal(raw, &be) //nolint:errcheck // a non-JSON body simply leaves be zero → treated as a hard failure
		if isConsentRequired(be.Err) {
			return auth.Token{}, exchangeMeta{}, &consentError{consentURL: firstNonEmpty(be.ConsentURL, be.VerificationURI)}
		}
		return auth.Token{}, exchangeMeta{}, fmt.Errorf("%w: broker %s status %d (error=%q)",
			auth.ErrExchangeFailed, p.brokerHost, resp.StatusCode, be.Err)
	}

	var br brokerResponse
	if err := json.Unmarshal(raw, &br); err != nil {
		return auth.Token{}, exchangeMeta{}, fmt.Errorf("%w: decode broker response: %w", auth.ErrExchangeFailed, err)
	}
	if br.AccessToken == "" {
		return auth.Token{}, exchangeMeta{}, fmt.Errorf("%w: empty access_token in broker response", auth.ErrExchangeFailed)
	}

	// Best-effort audience verification (confused-deputy defence). When a
	// resource indicator is declared and the returned token is JWT-shaped, its
	// `aud` claim MUST include the resource — a mismatch fails the exchange
	// loud and nothing is cached. An opaque token (RFC 8693 permits one)
	// leaves audienceVerified false — an honest no-op, never a fabricated
	// pass. This is an `aud`-claim comparison only, NOT signature verification.
	audienceVerified, audErr := verifyAudience(br.AccessToken, p.resourceIndicator)
	if audErr != nil {
		return auth.Token{}, exchangeMeta{}, fmt.Errorf("%w: %w (broker %s, resource=%q)",
			auth.ErrExchangeFailed, audErr, p.brokerHost, p.resourceIndicator)
	}

	// ExpiresAt is the broker-advertised token validity (zero when the
	// broker advertises none) — the cache-serve horizon is computed
	// separately and never exceeds it.
	var expiresAt time.Time
	if br.ExpiresIn > 0 {
		expiresAt = p.now().Add(time.Duration(br.ExpiresIn) * time.Second)
	}
	tok := auth.Token{
		Source:       p.source,
		BindingScope: auth.ScopeUser,
		TenantID:     id.TenantID,
		UserID:       id.UserID,
		AccessToken:  br.AccessToken,
		TokenType:    firstNonEmpty(br.TokenType, "Bearer"),
		ExpiresAt:    expiresAt,
		Scopes:       splitScopes(br.Scope),
		// RefreshToken deliberately empty: refresh is re-exchange, and a
		// brokered token is never persisted.
	}
	return tok, exchangeMeta{audienceVerified: audienceVerified, actorAsserted: actorAsserted}, nil
}

// verifyAudience checks a JWT-shaped access token's `aud` claim against the
// declared resource indicator. It returns:
//   - (false, nil) when no resource is declared (no check requested), or the
//     token is opaque (not a decodable three-segment JWT) — an honest no-op.
//   - (true, nil) when the token is JWT-shaped and its `aud` includes the
//     resource.
//   - (false, ErrAudienceMismatch) when the token is JWT-shaped and its `aud`
//     EXCLUDES the resource (a confused-deputy signal).
//
// It decodes the JWT payload's `aud` claim ONLY — never a signature check
// (Harbor has no keying relationship with the broker's AS).
func verifyAudience(accessToken, resource string) (bool, error) {
	if resource == "" {
		return false, nil
	}
	parts := strings.Split(accessToken, ".")
	if len(parts) != 3 {
		// Opaque bearer — RFC 8693 does not require a JWT. Honest no-op.
		return false, nil
	}
	payloadBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		// Not a decodable JWT payload — treat as opaque, not a mismatch.
		//nolint:nilerr // an undecodable JWT payload is a deliberate opaque no-op, not an exchange error
		return false, nil
	}
	var claims struct {
		Aud json.RawMessage `json:"aud"`
	}
	if err := json.Unmarshal(payloadBytes, &claims); err != nil {
		// Not a JSON JWT payload — treat as opaque, not a mismatch.
		//nolint:nilerr // a non-JSON JWT payload is a deliberate opaque no-op, not an exchange error
		return false, nil
	}
	auds := parseAudClaim(claims.Aud)
	if len(auds) == 0 {
		// A JWT that binds no audience cannot be confirmed to target the
		// declared resource — fail loud rather than serve a possibly-misbound
		// token to a downstream sink.
		return false, ErrAudienceMismatch
	}
	for _, a := range auds {
		if a == resource {
			return true, nil
		}
	}
	return false, ErrAudienceMismatch
}

// parseAudClaim normalises the JWT `aud` claim (RFC 7519 §4.1.3: either a
// single string or an array of strings) into a slice.
func parseAudClaim(raw json.RawMessage) []string {
	if len(raw) == 0 {
		return nil
	}
	var single string
	if err := json.Unmarshal(raw, &single); err == nil {
		if single == "" {
			return nil
		}
		return []string{single}
	}
	var many []string
	if err := json.Unmarshal(raw, &many); err == nil {
		return many
	}
	return nil
}

// buildConsentRequired parks the calling run on the unified pause
// primitive and returns the typed *auth.ErrAuthRequired the runtime
// catches. Mirrors the interactive Provider's buildAuthRequired: it
// allocates a pause record under the ctx identity, emits
// tool.auth_required, and returns the sentinel. A resume after central
// consent re-drives Token(); a bare resume against a still-declining
// broker re-parks here.
func (p *provider) buildConsentRequired(ctx context.Context, id identity.Identity, ce *consentError) error {
	pause, err := p.coordinator.Request(ctx, pauseresume.PauseRequest{
		Identity: id,
		Reason:   pauseresume.ReasonExternalEvent,
		Payload: map[string]any{
			"source":        string(p.source),
			"binding_scope": string(auth.ScopeUser),
			"authorize_url": ce.consentURL,
			"broker_host":   p.brokerHost,
		},
	})
	if err != nil {
		return fmt.Errorf("auth/tokenexchange: coordinator.Request: %w", err)
	}

	payload := auth.ToolAuthRequiredPayload{
		Source:       string(p.source),
		SourceName:   p.name,
		BindingScope: string(auth.ScopeUser),
		AuthorizeURL: ce.consentURL,
		State:        string(pause.Token),
		PauseToken:   string(pause.Token),
		Scopes:       append([]string(nil), p.scopes...),
	}
	// Emit ordering: the pause is requested BEFORE the event so the
	// payload can carry the pause token. An emit failure after a
	// successful Request leaves the pause record allocated without its
	// announcing event — deliberate and mirrors the interactive
	// provider's posture: the record is discoverable via the
	// coordinator's own pause.requested event and its List/Status
	// surface, and a retried Token() call simply parks anew; unwinding
	// the pause here would race a resume that already claimed it.
	if err := p.emit(ctx, auth.EventTypeToolAuthRequired, id, payload); err != nil {
		return fmt.Errorf("auth/tokenexchange: emit tool.auth_required: %w", err)
	}

	return &auth.ErrAuthRequired{
		Source:       p.source,
		SourceName:   p.name,
		BindingScope: auth.ScopeUser,
		AuthorizeURL: ce.consentURL,
		State:        string(pause.Token),
		Scopes:       append([]string(nil), p.scopes...),
		Message:      fmt.Sprintf("credential broker %s requires consent", p.brokerHost),
	}
}

// emitCredentialExchanged emits the canonical tool.credential_exchanged
// audit event for one actual exchange (zero token bytes).
func (p *provider) emitCredentialExchanged(ctx context.Context, id identity.Identity, tok auth.Token, meta exchangeMeta) error {
	payload := auth.ToolCredentialExchangedPayload{
		Source:           string(p.source),
		BindingScope:     string(auth.ScopeUser),
		SubjectKind:      string(auth.ScopeUser),
		BrokerHost:       p.brokerHost,
		GrantedScopes:    append([]string(nil), tok.Scopes...),
		ExpiresAt:        tok.ExpiresAt,
		AudienceVerified: meta.audienceVerified,
		ActorAsserted:    meta.actorAsserted,
	}
	if err := p.emit(ctx, auth.EventTypeToolCredentialExchanged, id, payload); err != nil {
		return fmt.Errorf("auth/tokenexchange: emit tool.credential_exchanged: %w", err)
	}
	return nil
}

// emit runs the payload through the redactor (defence in depth; the
// payload is SafePayload so the bus skips its own pass) and publishes.
func (p *provider) emit(ctx context.Context, evType events.EventType, id identity.Identity, payload events.EventPayload) error {
	if _, err := p.redactor.Redact(ctx, payload); err != nil {
		return fmt.Errorf("auth/tokenexchange: redact emit: %w", err)
	}
	return p.bus.Publish(ctx, events.Event{
		Type:     evType,
		Identity: identity.Quadruple{Identity: id},
		Payload:  payload,
	})
}

// InitiateFlow implements auth.OAuthProvider.InitiateFlow — a
// non-interactive driver has no authorization-code flow. Returns the
// typed sentinel rather than a silent no-op (§13).
func (p *provider) InitiateFlow(_ context.Context, _ tools.ToolSourceID) (auth.FlowInitiation, error) {
	return auth.FlowInitiation{}, fmt.Errorf("auth/tokenexchange: InitiateFlow (provider name=%q): %w", p.name, auth.ErrNonInteractive)
}

// CompleteFlow implements auth.OAuthProvider.CompleteFlow — no
// authorization-code callback exists for a brokered credential.
func (p *provider) CompleteFlow(_ context.Context, _, _ string) (auth.Token, error) {
	return auth.Token{}, fmt.Errorf("auth/tokenexchange: CompleteFlow (provider name=%q): %w", p.name, auth.ErrNonInteractive)
}

// DenyFlow implements auth.OAuthProvider.DenyFlow — no interactive flow
// to deny.
func (p *provider) DenyFlow(_ context.Context, _, _ string) error {
	return fmt.Errorf("auth/tokenexchange: DenyFlow (provider name=%q): %w", p.name, auth.ErrNonInteractive)
}

// PendingFlow implements auth.OAuthProvider.PendingFlow. A
// non-interactive driver holds no flow records — always reports none.
func (p *provider) PendingFlow(_ string) (auth.PendingFlowInfo, bool) {
	return auth.PendingFlowInfo{}, false
}

// Revoke implements auth.OAuthProvider.Revoke by clearing the local
// cache entry for (ctx identity, source) and bumping the key's
// revocation generation, so an exchange that was already in flight
// when the Revoke landed declines to repopulate the cache. Idempotent
// — no error when nothing is cached. Broker-side custody is untouched
// (revocation there is the broker's concern; the serve horizon bounds
// our staleness).
func (p *provider) Revoke(ctx context.Context, _ tools.ToolSourceID) error {
	if p.closed.Load() {
		return auth.ErrProviderClosed
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("auth/tokenexchange: Revoke cancelled: %w", err)
	}
	id, err := p.identityFromCtx(ctx)
	if err != nil {
		return err
	}
	key := p.cacheKey(id)
	p.cacheMu.Lock()
	p.gens[key]++
	delete(p.cache, key)
	p.cacheMu.Unlock()
	return nil
}

// Close implements auth.OAuthProvider.Close. Idempotent.
func (p *provider) Close(_ context.Context) error {
	p.closed.Store(true)
	return nil
}

// AllowedDownstreamHosts implements auth.OAuthProvider — the boot-declared
// sink allow-list for the exchanged bearer. A copy is returned so callers
// cannot alias provider state.
func (p *provider) AllowedDownstreamHosts() []string {
	return append([]string(nil), p.allowedDownstreamHosts...)
}

// identityFromCtx pulls the identity triple from ctx and fails closed
// when any component is missing (CLAUDE.md §6 rule 9).
func (p *provider) identityFromCtx(ctx context.Context) (identity.Identity, error) {
	id, ok := identity.From(ctx)
	if !ok {
		return identity.Identity{}, fmt.Errorf("auth/tokenexchange: Token (provider name=%q): %w", p.name, auth.ErrIdentityRequired)
	}
	if err := identity.Validate(id); err != nil {
		return identity.Identity{}, fmt.Errorf("auth/tokenexchange: %w: %w", auth.ErrIdentityRequired, err)
	}
	return id, nil
}

// encodeSubjectToken serialises the verified identity triple as
// base64url(JSON) — the subject_token the broker interprets under the
// Harbor-defined subject_token_type URN.
func encodeSubjectToken(id identity.Identity) (string, error) {
	b, err := json.Marshal(struct {
		TenantID  string `json:"tenant_id"`
		UserID    string `json:"user_id"`
		SessionID string `json:"session_id"`
	}{TenantID: id.TenantID, UserID: id.UserID, SessionID: id.SessionID})
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// cacheKey composes the cache / single-flight / revocation key for the
// verified ctx identity: (binding scope, tenant, user, source) — the
// same composite the sealed token store keys by. Components are
// length-prefixed so external-input IDs containing separator bytes
// cannot collide two keys (tenant "a;1" + user "b" vs tenant "a" +
// user "1;b"). The session is deliberately NOT part of the key —
// ScopeUser granularity; see the package godoc.
func (p *provider) cacheKey(id identity.Identity) string {
	scope := string(auth.ScopeUser)
	src := string(p.source)
	return fmt.Sprintf("%d:%s;%d:%s;%d:%s;%d:%s",
		len(scope), scope,
		len(id.TenantID), id.TenantID,
		len(id.UserID), id.UserID,
		len(src), src)
}

// isConsentRequired reports whether an OAuth error code is a
// consent-required-class refusal (the park path) rather than a hard
// failure (the fail-loud path).
func isConsentRequired(code string) bool {
	switch code {
	case "consent_required", "interaction_required", "authorization_pending", "login_required":
		return true
	default:
		return false
	}
}

// splitScopes splits a space-separated scope string into a slice; an
// empty string yields a nil slice.
func splitScopes(s string) []string {
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return nil
	}
	return fields
}

// firstNonEmpty returns the first non-empty argument.
func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
