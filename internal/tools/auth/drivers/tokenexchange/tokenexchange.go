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
// (scope, subject, source). The shared TokenStore's Put is NEVER
// called: the broker stays the single source of truth, so revocation
// at the broker is not defeated by a live sealed copy in N runtimes
// (the shadow-source-of-truth smell read southbound). The cache TTL is
// bounded by the broker-advertised expiry, the operator TTL cap, and a
// small floor that guards against a pathological zero-TTL broker
// spamming the audit bus.
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
// # Fail-loud at construction
//
// Operator-facing seams demand explicit configuration. The driver
// fails closed on: empty ClientID / ClientSecret (the env vars named
// by client_id_env / client_secret_env were unset), empty TokenURL
// (the broker endpoint — auth_url / redirect_url are interactive-flow
// fields and are NOT required here), a malformed extra.cache_ttl_cap,
// and missing deps (Store / Bus / Redactor / Coordinator).
package tokenexchange

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hurtener/Harbor/internal/audit"
	"github.com/hurtener/Harbor/internal/events"
	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/runtime/pauseresume"
	"github.com/hurtener/Harbor/internal/tools"
	"github.com/hurtener/Harbor/internal/tools/auth"
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

// TTL knobs. The cache TTL is min(broker expires_in, cacheTTLCap),
// floored at minTTL.
const (
	// defaultCacheTTLCap bounds how long a brokered token is served
	// from cache when the broker advertises a longer (or no) expiry.
	// Overridable per provider via extra.cache_ttl_cap.
	defaultCacheTTLCap = 5 * time.Minute
	// minTTL is the floor on the cache TTL. It guards against a
	// pathological zero-/near-zero-TTL broker spamming the audit bus
	// with one exchange per Token() call: the runtime re-exchanges at
	// most once per minTTL per subject.
	minTTL = 30 * time.Second
)

// Sentinel errors specific to the tokenexchange driver. Broker /
// upstream failures reuse the parent package's sentinels
// (auth.ErrExchangeFailed, auth.ErrAuthRequired, auth.ErrIdentityRequired).
var (
	// ErrMissingClientID — cfg.ClientID was empty at construction. The
	// dev stack resolves os.Getenv(client_id_env) before calling the
	// factory; an empty value means the env var was unset.
	ErrMissingClientID = errors.New("auth/tokenexchange: ClientID is empty (the env var named by client_id_env was unset or empty)")
	// ErrMissingClientSecret — cfg.ClientSecret was empty at
	// construction.
	ErrMissingClientSecret = errors.New("auth/tokenexchange: ClientSecret is empty (the env var named by client_secret_env was unset or empty)")
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
	if cfg.ClientID == "" {
		return nil, fmt.Errorf("%w (provider name=%q)", ErrMissingClientID, cfg.Name)
	}
	if cfg.ClientSecret == "" {
		return nil, fmt.Errorf("%w (provider name=%q)", ErrMissingClientSecret, cfg.Name)
	}
	if cfg.TokenURL == "" {
		return nil, fmt.Errorf("%w (provider name=%q)", ErrMissingTokenURL, cfg.Name)
	}
	if deps.Store == nil || deps.Bus == nil || deps.Redactor == nil || deps.Coordinator == nil {
		return nil, fmt.Errorf("%w (provider name=%q)", ErrMissingDeps, cfg.Name)
	}

	source := tools.ToolSourceID(cfg.Name)

	// audience defaults to the source ID; overridable via extra.
	audience := string(source)
	if v := strings.TrimSpace(cfg.Extra["audience"]); v != "" {
		audience = v
	}

	cacheTTLCap := defaultCacheTTLCap
	if raw := strings.TrimSpace(cfg.Extra["cache_ttl_cap"]); raw != "" {
		parsed, err := time.ParseDuration(raw)
		if err != nil {
			return nil, fmt.Errorf("%w (provider name=%q, got %q): %w", ErrBadCacheTTLCap, cfg.Name, raw, err)
		}
		if parsed < minTTL {
			parsed = minTTL
		}
		cacheTTLCap = parsed
	}

	brokerHost := ""
	if u, err := url.Parse(cfg.TokenURL); err == nil {
		brokerHost = u.Host
	}

	httpClient := deps.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	clock := deps.Clock
	if clock == nil {
		clock = time.Now
	}

	return &provider{
		name:         cfg.Name,
		source:       source,
		tokenURL:     cfg.TokenURL,
		brokerHost:   brokerHost,
		clientID:     cfg.ClientID,
		clientSecret: cfg.ClientSecret,
		scopes:       append([]string(nil), cfg.Scopes...),
		audience:     audience,
		cacheTTLCap:  cacheTTLCap,
		httpClient:   httpClient,
		now:          clock,
		bus:          deps.Bus,
		redactor:     deps.Redactor,
		coordinator:  deps.Coordinator,
		store:        deps.Store, // held to honour the FactoryDeps contract; Put is NEVER called
		cache:        make(map[string]cachedToken),
		flight:       make(map[string]*exchangeCall),
	}, nil
}

// provider is the concrete tokenexchange OAuthProvider.
//
// Concurrent reuse: every field below the mutexes is set once at
// construction and read-only after. The in-memory TTL cache and the
// single-flight table are the only mutable state, each guarded by its
// own mutex. Per-run identity is read from ctx on every call, never
// from the provider.
type provider struct {
	name         string
	source       tools.ToolSourceID
	tokenURL     string
	brokerHost   string
	clientID     string
	clientSecret string
	scopes       []string
	audience     string
	cacheTTLCap  time.Duration
	httpClient   *http.Client
	now          func() time.Time
	bus          events.EventBus
	redactor     audit.Redactor
	coordinator  pauseresume.Coordinator

	// store is the shared TokenStore. Held only to honour the
	// FactoryDeps mandatory-dep contract; brokered tokens live in the
	// in-memory cache and store.Put is NEVER called (the broker stays
	// the single source of truth).
	store auth.TokenStore

	// cacheMu guards cache. Short critical sections only — never held
	// across the broker HTTP round-trip.
	cacheMu sync.Mutex
	cache   map[string]cachedToken

	// flightMu guards flight — the per-(scope,subject,source)
	// single-flight table collapsing a burst of concurrent misses onto
	// one broker request.
	flightMu sync.Mutex
	flight   map[string]*exchangeCall

	closed atomic.Bool
}

// cachedToken is one in-memory brokered token plus its cache expiry.
type cachedToken struct {
	token     auth.Token
	expiresAt time.Time
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
	subj := id.UserID // ScopeUser: the subject is the ctx user
	if subj == "" {
		return auth.Token{}, fmt.Errorf("auth/tokenexchange: %w: subject (user) empty", auth.ErrIdentityRequired)
	}
	key := cacheKey(subj, p.source)

	// Hot path: fresh cache hit → return immediately, emit nothing.
	p.cacheMu.Lock()
	ct, ok := p.cache[key]
	if ok && p.now().Before(ct.expiresAt) {
		tok := ct.token
		p.cacheMu.Unlock()
		return tok, nil
	}
	p.cacheMu.Unlock()

	tok, err := p.exchangeSingleFlight(ctx, id, subj, key)
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
// (scope, subject, source) onto a single broker request, mirroring the
// interactive Provider's refresh single-flight. The leader performs the
// exchange, caches the result, and emits tool.credential_exchanged
// exactly once; waiters receive the shared token/err and emit nothing.
func (p *provider) exchangeSingleFlight(ctx context.Context, id identity.Identity, subj, key string) (auth.Token, error) {
	p.flightMu.Lock()
	call, inflight := p.flight[key]
	if inflight {
		p.flightMu.Unlock()
		select {
		case <-call.done:
			return call.token, call.err
		case <-ctx.Done():
			return auth.Token{}, fmt.Errorf("auth/tokenexchange: Token cancelled: %w", ctx.Err())
		}
	}
	call = &exchangeCall{done: make(chan struct{})}
	p.flight[key] = call
	p.flightMu.Unlock()

	defer func() {
		close(call.done)
		p.flightMu.Lock()
		delete(p.flight, key)
		p.flightMu.Unlock()
	}()

	// Re-check the cache under the flight: a flight that completed
	// between our miss and acquiring leadership may have populated it.
	p.cacheMu.Lock()
	if ct, ok := p.cache[key]; ok && p.now().Before(ct.expiresAt) {
		tok := ct.token
		p.cacheMu.Unlock()
		call.token = tok
		return tok, nil
	}
	p.cacheMu.Unlock()

	tok, err := p.exchange(ctx, id, subj)
	if err != nil {
		call.err = err
		return auth.Token{}, err
	}

	// Audit the ACTUAL exchange before caching. A redaction / emit
	// failure fails loud (§7 external-credential posture): the token is
	// NOT cached and the error propagates, so the exchange is retried
	// rather than silently served without its audit trail.
	if err := p.emitCredentialExchanged(ctx, id, tok); err != nil {
		call.err = err
		return auth.Token{}, err
	}

	p.cacheMu.Lock()
	p.cache[key] = cachedToken{token: tok, expiresAt: tok.ExpiresAt}
	p.cacheMu.Unlock()
	call.token = tok
	return tok, nil
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

// exchange performs one RFC-8693 token-exchange POST against the broker
// and maps the outcome onto (token / *consentError / ErrExchangeFailed).
func (p *provider) exchange(ctx context.Context, id identity.Identity, subj string) (auth.Token, error) {
	subjectToken, err := encodeSubjectToken(id)
	if err != nil {
		return auth.Token{}, fmt.Errorf("%w: encode subject token: %w", auth.ErrExchangeFailed, err)
	}

	form := url.Values{}
	form.Set("grant_type", grantTypeTokenExchange)
	form.Set("subject_token_type", subjectTokenTypeIdentityTriple)
	form.Set("subject_token", subjectToken)
	form.Set("audience", p.audience)
	if len(p.scopes) > 0 {
		form.Set("scope", strings.Join(p.scopes, " "))
	}
	// Runtime→broker client authentication (§7 rule 2 env-indirection;
	// the resolved values arrived via FactoryDeps).
	form.Set("client_id", p.clientID)
	form.Set("client_secret", p.clientSecret)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return auth.Token{}, fmt.Errorf("%w: build request: %w", auth.ErrExchangeFailed, err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := p.httpClient.Do(req)
	if err != nil {
		return auth.Token{}, fmt.Errorf("%w: POST broker %s: %w", auth.ErrExchangeFailed, p.brokerHost, err)
	}
	defer resp.Body.Close()

	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16)) //nolint:errcheck // a partial read still yields a usable error message; the status code drives the branch
	if resp.StatusCode/100 != 2 {
		// Decode the OAuth error body to distinguish a consent refusal
		// (park path) from a hard failure (fail-loud path).
		var be brokerError
		_ = json.Unmarshal(raw, &be) //nolint:errcheck // a non-JSON body simply leaves be zero → treated as a hard failure
		if isConsentRequired(be.Err) {
			return auth.Token{}, &consentError{consentURL: firstNonEmpty(be.ConsentURL, be.VerificationURI)}
		}
		return auth.Token{}, fmt.Errorf("%w: broker %s status %d (error=%q)",
			auth.ErrExchangeFailed, p.brokerHost, resp.StatusCode, be.Err)
	}

	var br brokerResponse
	if err := json.Unmarshal(raw, &br); err != nil {
		return auth.Token{}, fmt.Errorf("%w: decode broker response: %w", auth.ErrExchangeFailed, err)
	}
	if br.AccessToken == "" {
		return auth.Token{}, fmt.Errorf("%w: empty access_token in broker response", auth.ErrExchangeFailed)
	}

	tok := auth.Token{
		Source:       p.source,
		BindingScope: auth.ScopeUser,
		TenantID:     id.TenantID,
		UserID:       subj,
		AccessToken:  br.AccessToken,
		TokenType:    firstNonEmpty(br.TokenType, "Bearer"),
		ExpiresAt:    p.now().Add(p.cacheTTL(br.ExpiresIn)),
		Scopes:       splitScopes(br.Scope),
		// RefreshToken deliberately empty: refresh is re-exchange, and a
		// brokered token is never persisted.
	}
	return tok, nil
}

// cacheTTL computes the effective in-memory cache TTL from the broker's
// advertised expires_in, the operator TTL cap, and the floor.
func (p *provider) cacheTTL(expiresIn int) time.Duration {
	ttl := p.cacheTTLCap
	if expiresIn > 0 {
		if adv := time.Duration(expiresIn) * time.Second; adv < ttl {
			ttl = adv
		}
	}
	if ttl < minTTL {
		ttl = minTTL
	}
	return ttl
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
func (p *provider) emitCredentialExchanged(ctx context.Context, id identity.Identity, tok auth.Token) error {
	payload := auth.ToolCredentialExchangedPayload{
		Source:        string(p.source),
		BindingScope:  string(auth.ScopeUser),
		SubjectKind:   string(auth.ScopeUser),
		BrokerHost:    p.brokerHost,
		GrantedScopes: append([]string(nil), tok.Scopes...),
		ExpiresAt:     tok.ExpiresAt,
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
// cache entry for (ctx identity, source). Idempotent — no error when
// nothing is cached. Broker-side custody is untouched (revocation there
// is the broker's concern; the TTL cap bounds our staleness).
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
	subj := id.UserID
	if subj == "" {
		return fmt.Errorf("auth/tokenexchange: %w: subject (user) empty", auth.ErrIdentityRequired)
	}
	key := cacheKey(subj, p.source)
	p.cacheMu.Lock()
	delete(p.cache, key)
	p.cacheMu.Unlock()
	return nil
}

// Close implements auth.OAuthProvider.Close. Idempotent.
func (p *provider) Close(_ context.Context) error {
	p.closed.Store(true)
	return nil
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

// cacheKey composes the (scope, subject, source) cache / single-flight
// key. The V1 driver serves ScopeUser only.
func cacheKey(subject string, source tools.ToolSourceID) string {
	return string(auth.ScopeUser) + "." + subject + "." + string(source)
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
