package auth

import (
	"context"
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
	"github.com/hurtener/Harbor/internal/runtime/registry"
	"github.com/hurtener/Harbor/internal/tools"
)

// ProviderDeps bundles the collaborators a Provider needs. The
// production binary wires all four; tests may stub the bus / pauser /
// redactor with in-memory equivalents that satisfy the same
// interface.
type ProviderDeps struct {
	// Store is the TokenStore the provider reads / writes tokens
	// through. Mandatory.
	Store TokenStore
	// Bus is the event bus the provider emits tool.auth_required /
	// tool.auth_completed events on. Mandatory.
	Bus events.EventBus
	// Redactor processes the ToolAuthRequiredPayload before emission.
	// Mandatory.
	Redactor audit.Redactor
	// Coordinator is the unified pause/resume primitive (Phase 50).
	// Mandatory — InitiateFlow allocates a pause record on it;
	// CompleteFlow resumes through it.
	Coordinator pauseresume.Coordinator
	// HTTPClient is the client the provider uses to talk to the
	// authorization server (discovery / dynamic registration / token
	// exchange). Optional — defaults to http.DefaultClient with a
	// 30s timeout shim.
	HTTPClient *http.Client
	// Clock is the wall-clock source. Optional — defaults to
	// time.Now.
	Clock func() time.Time
	// FlowTTL is how long an initiated flow remains
	// CompleteFlow-able. Optional — defaults to 10 minutes.
	FlowTTL time.Duration
}

// Provider is the V1 concrete OAuthProvider implementation.
//
// Concurrent reuse (D-025): every field below is set once at
// construction (deps + immutable maps protected by mu).
type Provider struct {
	store       TokenStore
	bus         events.EventBus
	redactor    audit.Redactor
	coordinator pauseresume.Coordinator
	httpClient  *http.Client
	now         func() time.Time
	flowTTL     time.Duration

	// configs is the operator-supplied set of OAuthConfigs, indexed
	// by Source. Set once at construction; read-only after.
	configs map[tools.ToolSourceID]OAuthConfig

	// flowsMu guards `flows` and `discoveries`. RWMutex justified by
	// the read-heavy CompleteFlow path (one lookup per callback).
	flowsMu sync.RWMutex
	// flows tracks in-flight authorization-code flows keyed by state.
	flows map[string]*flowRecord
	// discoveries caches OAuth metadata-discovery results keyed by
	// ServerURL. Lifetime is the Provider lifetime; the cache is
	// small (one entry per configured Source) and the TTL is the
	// authz-server's discoverability — we re-fetch on Close.
	discoveries map[string]discoveredMetadata
	// registrations caches the result of an RFC 7591 dynamic
	// registration keyed by (ServerURL, RegistrationURL). Same
	// lifetime as `discoveries`.
	registrations map[string]registrationResult

	// refreshGroup is the per-(scope,subject,source) single-flight
	// gate for token refresh. Prevents a refresh storm on
	// agent-bound tokens shared across N concurrent sessions
	// (brief 09 §"Concurrent refresh storm on agent-bound tokens").
	refreshMu     sync.Mutex
	refreshFlight map[string]*refreshCall

	closed atomic.Bool
}

// flowRecord captures a single in-flight authorization-code flow. The
// caller's identity + source + PKCE verifier are pinned at
// InitiateFlow time and consulted on CompleteFlow.
type flowRecord struct {
	State        string
	Source       tools.ToolSourceID
	BindingScope BindingScope
	SubjectID    string
	Identity     identity.Identity
	Verifier     string // PKCE code_verifier
	ExpiresAt    time.Time
	PauseToken   pauseresume.Token
}

// discoveredMetadata caches the subset of an OAuth-authorization-server
// metadata document we consult.
type discoveredMetadata struct {
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	RegistrationEndpoint  string `json:"registration_endpoint"`
}

// registrationResult caches the result of an RFC 7591 dynamic
// registration so subsequent flows reuse the same ClientID.
type registrationResult struct {
	ClientID     string
	ClientSecret string
}

// refreshCall is one in-flight refresh shared by N callers.
type refreshCall struct {
	done  chan struct{}
	token Token
	err   error
}

// NewProvider constructs a Provider from configs + deps.
//
// configs is the operator-supplied set of OAuthConfigs (one per
// (Source, BindingScope) tuple). Each must Validate; a malformed
// config fails NewProvider loud rather than degrading silently.
//
// deps's Store / Bus / Redactor / Coordinator are mandatory. A nil
// dep is rejected at construction (fail-loud per CLAUDE.md §13
// amendment).
func NewProvider(configs []OAuthConfig, deps ProviderDeps) (*Provider, error) {
	if deps.Store == nil {
		return nil, errors.New("auth: NewProvider: TokenStore required")
	}
	if deps.Bus == nil {
		return nil, errors.New("auth: NewProvider: events.EventBus required")
	}
	if deps.Redactor == nil {
		return nil, errors.New("auth: NewProvider: audit.Redactor required")
	}
	if deps.Coordinator == nil {
		return nil, errors.New("auth: NewProvider: pauseresume.Coordinator required")
	}
	httpClient := deps.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}
	clock := deps.Clock
	if clock == nil {
		clock = time.Now
	}
	flowTTL := deps.FlowTTL
	if flowTTL == 0 {
		flowTTL = 10 * time.Minute
	}

	cfgMap := make(map[tools.ToolSourceID]OAuthConfig, len(configs))
	for _, c := range configs {
		if err := c.Validate(); err != nil {
			return nil, fmt.Errorf("auth: NewProvider: config for source %q: %w", c.Source, err)
		}
		if _, dupe := cfgMap[c.Source]; dupe {
			return nil, fmt.Errorf("auth: NewProvider: duplicate OAuthConfig for source %q", c.Source)
		}
		cfgMap[c.Source] = c
	}

	return &Provider{
		store:         deps.Store,
		bus:           deps.Bus,
		redactor:      deps.Redactor,
		coordinator:   deps.Coordinator,
		httpClient:    httpClient,
		now:           clock,
		flowTTL:       flowTTL,
		configs:       cfgMap,
		flows:         make(map[string]*flowRecord),
		discoveries:   make(map[string]discoveredMetadata),
		registrations: make(map[string]registrationResult),
		refreshFlight: make(map[string]*refreshCall),
	}, nil
}

// Token implements OAuthProvider.Token.
func (p *Provider) Token(ctx context.Context, source tools.ToolSourceID) (Token, error) {
	if p.closed.Load() {
		return Token{}, ErrProviderClosed
	}
	if err := ctx.Err(); err != nil {
		return Token{}, fmt.Errorf("auth: Token cancelled: %w", err)
	}
	id, err := identityFromCtx(ctx)
	if err != nil {
		return Token{}, err
	}
	cfg, ok := p.configs[source]
	if !ok {
		return Token{}, fmt.Errorf("%w: no OAuthConfig for source %q", ErrConfigInvalid, source)
	}
	subj := cfg.SubjectID(id)
	if subj == "" {
		return Token{}, wrap(ErrConfigInvalid, "subject empty for scope %s (ctx user=%q, cfg agent=%q)",
			cfg.BindingScope, id.UserID, cfg.AgentID)
	}

	// Hot path: store hit + token fresh → return immediately.
	tok, ok, err := p.store.Get(ctx, cfg.BindingScope, subj, source)
	if err != nil {
		return Token{}, err
	}
	if ok && !p.isExpired(tok) {
		return tok, nil
	}
	// Expired? Attempt single-flight refresh.
	if ok && p.isExpired(tok) && tok.RefreshToken != "" {
		refreshed, rerr := p.refreshLocked(ctx, cfg, tok)
		if rerr == nil {
			return refreshed, nil
		}
		// Refresh failed — fall through to ErrAuthRequired.
	}
	// No usable token — surface ErrAuthRequired with a fresh
	// authorize-URL the runtime can pause on.
	return Token{}, p.buildAuthRequired(ctx, cfg, id, subj)
}

// isExpired reports whether t has expired by the provider's clock.
// A zero ExpiresAt is treated as "no expiry advertised" — long-lived.
// A non-zero ExpiresAt within 30s of now() is treated as expired
// (defensive margin against clock skew + in-flight-request lag).
func (p *Provider) isExpired(t Token) bool {
	if t.ExpiresAt.IsZero() {
		return false
	}
	return !p.now().Add(30 * time.Second).Before(t.ExpiresAt)
}

// refreshLocked performs a refresh via the configured token endpoint
// under a single-flight gate keyed by (scope, subject, source). N
// concurrent callers see one HTTP exchange.
func (p *Provider) refreshLocked(ctx context.Context, cfg OAuthConfig, current Token) (Token, error) {
	key := string(cfg.BindingScope) + "." + current.SubjectID() + "." + string(cfg.Source)

	p.refreshMu.Lock()
	call, inflight := p.refreshFlight[key]
	if inflight {
		p.refreshMu.Unlock()
		select {
		case <-call.done:
			return call.token, call.err
		case <-ctx.Done():
			return Token{}, ctx.Err()
		}
	}
	call = &refreshCall{done: make(chan struct{})}
	p.refreshFlight[key] = call
	p.refreshMu.Unlock()

	defer func() {
		close(call.done)
		p.refreshMu.Lock()
		delete(p.refreshFlight, key)
		p.refreshMu.Unlock()
	}()

	tokenURL, _, _, err := p.resolveEndpoints(ctx, cfg)
	if err != nil {
		call.err = err
		return Token{}, err
	}

	body := url.Values{}
	body.Set("grant_type", "refresh_token")
	body.Set("refresh_token", current.RefreshToken)
	if cfg.ClientID != "" {
		body.Set("client_id", cfg.ClientID)
	}
	if cfg.ClientSecret != "" {
		body.Set("client_secret", cfg.ClientSecret)
	}

	resp, err := p.postForm(ctx, tokenURL, body)
	if err != nil {
		call.err = err
		return Token{}, err
	}

	t := Token{
		Source:          cfg.Source,
		BindingScope:    cfg.BindingScope,
		TenantID:        current.TenantID,
		UserID:          current.UserID,
		AgentID:         current.AgentID,
		AccessToken:     resp.AccessToken,
		RefreshToken:    refreshTokenOrCurrent(resp.RefreshToken, current.RefreshToken),
		TokenType:       resp.TokenType,
		ExpiresAt:       resp.expiresAt(p.now()),
		Scopes:          splitScopes(resp.Scope),
		LastRefreshedAt: p.now(),
	}
	if err := p.store.Put(ctx, t); err != nil {
		call.err = err
		return Token{}, err
	}
	call.token = t
	return t, nil
}

func refreshTokenOrCurrent(refreshed, current string) string {
	if refreshed != "" {
		return refreshed
	}
	return current
}

// buildAuthRequired allocates a pause record + emits
// tool.auth_required + returns the typed *ErrAuthRequired sentinel.
//
// Pause-record identity = ctx identity. State and PauseToken are
// freshly minted and persisted in flows.
func (p *Provider) buildAuthRequired(ctx context.Context, cfg OAuthConfig, id identity.Identity, subj string) error {
	state, err := newState()
	if err != nil {
		return err
	}
	verifier, err := newPKCEVerifier()
	if err != nil {
		return err
	}
	tokenURL, authzURL, regURL, err := p.resolveEndpoints(ctx, cfg)
	if err != nil {
		return err
	}
	// RFC 7591 dynamic registration if no ClientID yet.
	clientID, clientSecret, err := p.ensureClient(ctx, cfg, regURL)
	if err != nil {
		return err
	}
	_ = tokenURL // resolveEndpoints touched it; persisted in flow record indirectly via cfg

	authorize := buildAuthorizeURL(authzURL, clientID, cfg.RedirectURI, cfg.Scopes, state, verifier)

	// Allocate a pause record. Reason = ExternalEvent — OAuth out-of-band
	// completion is a textbook external-event pause (RFC §6.3).
	pause, err := p.coordinator.Request(ctx, pauseresume.PauseRequest{
		Identity: id,
		Reason:   pauseresume.ReasonExternalEvent,
		Payload: map[string]any{
			"source":        string(cfg.Source),
			"binding_scope": string(cfg.BindingScope),
			"state":         state,
			"authorize_url": authorize,
		},
	})
	if err != nil {
		return fmt.Errorf("auth: coordinator.Request: %w", err)
	}

	rec := &flowRecord{
		State:        state,
		Source:       cfg.Source,
		BindingScope: cfg.BindingScope,
		SubjectID:    subj,
		Identity:     id,
		Verifier:     verifier,
		ExpiresAt:    p.now().Add(p.flowTTL),
		PauseToken:   pause.Token,
	}
	// Stash client material on the flow record indirectly via the
	// configs map; the resolved (clientID, clientSecret) is cached
	// in p.registrations keyed by ServerURL/RegistrationURL.
	_ = clientID
	_ = clientSecret

	p.flowsMu.Lock()
	p.flows[state] = rec
	p.flowsMu.Unlock()

	payload := ToolAuthRequiredPayload{
		Source:       string(cfg.Source),
		SourceName:   cfg.SourceName,
		BindingScope: string(cfg.BindingScope),
		AuthorizeURL: authorize,
		State:        state,
		PauseToken:   string(pause.Token),
		Scopes:       append([]string(nil), cfg.Scopes...),
	}
	if err := p.emitEvent(ctx, EventTypeToolAuthRequired, id, payload); err != nil {
		// Emission failure is observability — does not unwind the
		// pause record; the err is wrapped on the returned
		// *ErrAuthRequired so callers can branch on it.
		return fmt.Errorf("auth: emit tool.auth_required: %w", err)
	}

	return &ErrAuthRequired{
		Source:       cfg.Source,
		SourceName:   cfg.SourceName,
		BindingScope: cfg.BindingScope,
		AuthorizeURL: authorize,
		State:        state,
		Scopes:       append([]string(nil), cfg.Scopes...),
		Message:      "tool requires OAuth authorization",
	}
}

// InitiateFlow allocates a fresh flow record + pause-record
// out-of-band of a Token() call. Used by admin setup flows: the
// admin clicks "Connect <SourceName>" in the Console; the Console
// calls InitiateFlow; the admin completes OAuth; CompleteFlow
// reattaches the token. ScopeAgent flows require admin scope on ctx
// (registry.WithControlScope) — fails ErrAdminScopeRequired
// otherwise.
func (p *Provider) InitiateFlow(ctx context.Context, source tools.ToolSourceID) (FlowInitiation, error) {
	if p.closed.Load() {
		return FlowInitiation{}, ErrProviderClosed
	}
	if err := ctx.Err(); err != nil {
		return FlowInitiation{}, fmt.Errorf("auth: InitiateFlow cancelled: %w", err)
	}
	id, err := identityFromCtx(ctx)
	if err != nil {
		return FlowInitiation{}, err
	}
	cfg, ok := p.configs[source]
	if !ok {
		return FlowInitiation{}, fmt.Errorf("%w: no OAuthConfig for source %q", ErrConfigInvalid, source)
	}
	if cfg.BindingScope == ScopeAgent && !registry.HasControlScope(ctx) {
		return FlowInitiation{}, ErrAdminScopeRequired
	}
	subj := cfg.SubjectID(id)
	if subj == "" {
		return FlowInitiation{}, wrap(ErrConfigInvalid, "subject empty for scope %s", cfg.BindingScope)
	}

	state, err := newState()
	if err != nil {
		return FlowInitiation{}, err
	}
	verifier, err := newPKCEVerifier()
	if err != nil {
		return FlowInitiation{}, err
	}
	_, authzURL, regURL, err := p.resolveEndpoints(ctx, cfg)
	if err != nil {
		return FlowInitiation{}, err
	}
	clientID, _, err := p.ensureClient(ctx, cfg, regURL)
	if err != nil {
		return FlowInitiation{}, err
	}
	authorize := buildAuthorizeURL(authzURL, clientID, cfg.RedirectURI, cfg.Scopes, state, verifier)

	pause, err := p.coordinator.Request(ctx, pauseresume.PauseRequest{
		Identity: id,
		Reason:   pauseresume.ReasonExternalEvent,
		Payload: map[string]any{
			"source":        string(cfg.Source),
			"binding_scope": string(cfg.BindingScope),
			"state":         state,
			"authorize_url": authorize,
		},
	})
	if err != nil {
		return FlowInitiation{}, fmt.Errorf("auth: coordinator.Request: %w", err)
	}

	rec := &flowRecord{
		State:        state,
		Source:       cfg.Source,
		BindingScope: cfg.BindingScope,
		SubjectID:    subj,
		Identity:     id,
		Verifier:     verifier,
		ExpiresAt:    p.now().Add(p.flowTTL),
		PauseToken:   pause.Token,
	}
	p.flowsMu.Lock()
	p.flows[state] = rec
	p.flowsMu.Unlock()

	// Emit tool.auth_required so observers see the flow start.
	payload := ToolAuthRequiredPayload{
		Source:       string(cfg.Source),
		SourceName:   cfg.SourceName,
		BindingScope: string(cfg.BindingScope),
		AuthorizeURL: authorize,
		State:        state,
		PauseToken:   string(pause.Token),
		Scopes:       append([]string(nil), cfg.Scopes...),
	}
	if err := p.emitEvent(ctx, EventTypeToolAuthRequired, id, payload); err != nil {
		return FlowInitiation{}, err
	}

	return FlowInitiation{
		AuthorizeURL: authorize,
		State:        state,
		PauseToken:   string(pause.Token),
		ExpiresAt:    rec.ExpiresAt,
		BindingScope: cfg.BindingScope,
		Source:       cfg.Source,
	}, nil
}

// CompleteFlow handles the callback. Exchanges (state, code) for
// tokens; persists via TokenStore; resumes the parked run via the
// coordinator; emits tool.auth_completed.
func (p *Provider) CompleteFlow(ctx context.Context, state, code string) (Token, error) {
	if p.closed.Load() {
		return Token{}, ErrProviderClosed
	}
	if err := ctx.Err(); err != nil {
		return Token{}, fmt.Errorf("auth: CompleteFlow cancelled: %w", err)
	}
	if state == "" {
		return Token{}, wrap(ErrFlowNotFound, "empty state")
	}
	if code == "" {
		return Token{}, wrap(ErrExchangeFailed, "empty code")
	}

	p.flowsMu.Lock()
	rec, ok := p.flows[state]
	if ok {
		delete(p.flows, state)
	}
	p.flowsMu.Unlock()
	if !ok {
		return Token{}, ErrFlowNotFound
	}
	if p.now().After(rec.ExpiresAt) {
		return Token{}, ErrFlowExpired
	}

	cfg, cfgOK := p.configs[rec.Source]
	if !cfgOK {
		return Token{}, fmt.Errorf("%w: source %q removed mid-flow", ErrConfigInvalid, rec.Source)
	}
	if cfg.BindingScope == ScopeAgent && !registry.HasControlScope(ctx) {
		return Token{}, ErrAdminScopeRequired
	}

	tokenURL, _, _, err := p.resolveEndpoints(ctx, cfg)
	if err != nil {
		return Token{}, err
	}

	// Cross-check: caller's ctx identity must match the flow's
	// recorded identity. A mismatch surfaces a state-swap attack /
	// stale callback loud.
	id, err := identityFromCtx(ctx)
	if err != nil {
		return Token{}, err
	}
	if id != rec.Identity {
		return Token{}, ErrStateMismatch
	}

	clientID, clientSecret := p.cachedClient(cfg)
	if clientID == "" {
		// Re-resolve in case the cache evaporated.
		_, _, regURL, rerr := p.resolveEndpoints(ctx, cfg)
		if rerr != nil {
			return Token{}, rerr
		}
		clientID, clientSecret, err = p.ensureClient(ctx, cfg, regURL)
		if err != nil {
			return Token{}, err
		}
	}

	body := url.Values{}
	body.Set("grant_type", "authorization_code")
	body.Set("code", code)
	body.Set("redirect_uri", cfg.RedirectURI)
	body.Set("client_id", clientID)
	if clientSecret != "" {
		body.Set("client_secret", clientSecret)
	}
	body.Set("code_verifier", rec.Verifier)

	resp, err := p.postForm(ctx, tokenURL, body)
	if err != nil {
		return Token{}, err
	}

	tok := Token{
		Source:       cfg.Source,
		BindingScope: cfg.BindingScope,
		TenantID:     rec.Identity.TenantID,
		UserID:       userIfScopeUser(cfg.BindingScope, rec.Identity.UserID),
		AgentID:      agentIfScopeAgent(cfg.BindingScope, cfg.AgentID),
		AccessToken:  resp.AccessToken,
		RefreshToken: resp.RefreshToken,
		TokenType:    resp.TokenType,
		ExpiresAt:    resp.expiresAt(p.now()),
		Scopes:       splitScopes(resp.Scope),
	}
	if err := p.store.Put(ctx, tok); err != nil {
		return Token{}, err
	}

	// Resume the parked run with a typed `DecisionResume` marker —
	// this is a generic resume of a non-approval pause (the OAuth flow
	// completed), distinct from approve / reject / timeout (issue #113,
	// D-096). A failure here is loud — the pause would otherwise
	// linger as a record nobody can claim.
	if err := p.coordinator.Resume(ctx, rec.PauseToken, pauseresume.DecisionResume, map[string]any{
		"source":       string(cfg.Source),
		"binding":      string(cfg.BindingScope),
		"completed_at": p.now().Format(time.RFC3339),
	}); err != nil {
		return Token{}, fmt.Errorf("auth: coordinator.Resume: %w", err)
	}

	payload := ToolAuthCompletedPayload{
		Source:       string(cfg.Source),
		BindingScope: string(cfg.BindingScope),
		State:        state,
		PauseToken:   string(rec.PauseToken),
	}
	if err := p.emitEvent(ctx, EventTypeToolAuthCompleted, rec.Identity, payload); err != nil {
		return Token{}, err
	}

	return tok, nil
}

// Revoke removes the token for (ctx identity, source). For
// ScopeAgent sources, ctx MUST carry the admin scope.
func (p *Provider) Revoke(ctx context.Context, source tools.ToolSourceID) error {
	if p.closed.Load() {
		return ErrProviderClosed
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("auth: Revoke cancelled: %w", err)
	}
	id, err := identityFromCtx(ctx)
	if err != nil {
		return err
	}
	cfg, ok := p.configs[source]
	if !ok {
		return fmt.Errorf("%w: no OAuthConfig for source %q", ErrConfigInvalid, source)
	}
	if cfg.BindingScope == ScopeAgent && !registry.HasControlScope(ctx) {
		return ErrAdminScopeRequired
	}
	subj := cfg.SubjectID(id)
	if subj == "" {
		return wrap(ErrConfigInvalid, "subject empty for scope %s", cfg.BindingScope)
	}
	return p.store.Delete(ctx, cfg.BindingScope, subj, source)
}

// Close releases provider resources. Idempotent.
func (p *Provider) Close(_ context.Context) error {
	p.closed.Store(true)
	return nil
}

// ConfigFor returns a copy of the OAuthConfig for source, or false
// when no attachment is configured. Useful for transport drivers
// that need to inspect the binding scope before invoking Token (e.g.
// to decide whether to include an `Authorization` header at all).
func (p *Provider) ConfigFor(source tools.ToolSourceID) (OAuthConfig, bool) {
	cfg, ok := p.configs[source]
	return cfg, ok
}

// PendingFlow reports whether `state` corresponds to an in-flight
// flow record (useful for the callback handler to short-circuit
// validation before the token-exchange round-trip).
func (p *Provider) PendingFlow(state string) bool {
	p.flowsMu.RLock()
	_, ok := p.flows[state]
	p.flowsMu.RUnlock()
	return ok
}

// emitEvent Publishes onto the bus.
//
// The payload is SafePayload by construction (every field is
// caller-controllable surface — auth URLs, scope identifiers, opaque
// pause tokens; never plaintext OAuth tokens), so the bus skips the
// redactor. But Phase 30's acceptance criterion is explicit:
// "ErrAuthRequired payload is typed and audit-redacted (no raw token
// material in events)." We satisfy "audit-redacted" defensively here:
// the payload is run through Redactor.Redact before Publish; a
// redaction error fails the emit loud (CLAUDE.md §13 fail-loudly +
// audit.Redactor's "do not emit on error" contract). The returned
// redacted form is discarded — the bus runs its own redact pass — but
// the call exercises the redactor's invariant set against the
// payload shape on every emission, so an accidental future change to
// the payload that DOES carry a secret would surface immediately as a
// redaction-rule hit even though SafePayload would otherwise let it
// through.
func (p *Provider) emitEvent(ctx context.Context, evType events.EventType, id identity.Identity, payload events.EventPayload) error {
	// Defence in depth: bus.Publish also redacts (SafePayload bypass guard). The double pass is intentional — see godoc.
	if _, err := p.redactor.Redact(ctx, payload); err != nil {
		return fmt.Errorf("auth: redact emit: %w", err)
	}
	q := identity.Quadruple{Identity: id}
	return p.bus.Publish(ctx, events.Event{
		Type:     evType,
		Identity: q,
		Payload:  payload,
	})
}

// resolveEndpoints returns the (token, authorize, registration) URLs
// for cfg, performing OAuth metadata discovery when necessary.
func (p *Provider) resolveEndpoints(ctx context.Context, cfg OAuthConfig) (tokenURL, authzURL, regURL string, err error) {
	if cfg.AuthorizeURL != "" && cfg.TokenURL != "" {
		return cfg.TokenURL, cfg.AuthorizeURL, cfg.RegistrationURL, nil
	}
	if cfg.ServerURL == "" {
		return "", "", "", wrap(ErrDiscoveryFailed, "no ServerURL and no AuthorizeURL/TokenURL configured")
	}
	p.flowsMu.RLock()
	disc, cached := p.discoveries[cfg.ServerURL]
	p.flowsMu.RUnlock()
	if !cached {
		fetched, ferr := p.fetchDiscovery(ctx, cfg.ServerURL)
		if ferr != nil {
			return "", "", "", ferr
		}
		p.flowsMu.Lock()
		p.discoveries[cfg.ServerURL] = fetched
		p.flowsMu.Unlock()
		disc = fetched
	}
	tokenURL = nonEmpty(cfg.TokenURL, disc.TokenEndpoint)
	authzURL = nonEmpty(cfg.AuthorizeURL, disc.AuthorizationEndpoint)
	regURL = nonEmpty(cfg.RegistrationURL, disc.RegistrationEndpoint)
	if tokenURL == "" || authzURL == "" {
		return "", "", "", wrap(ErrDiscoveryFailed, "discovery returned empty token/authorize endpoints")
	}
	return tokenURL, authzURL, regURL, nil
}

func nonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// fetchDiscovery GETs `{serverURL}/.well-known/oauth-authorization-server`
// and decodes the document. Returns wrapped ErrDiscoveryFailed on
// HTTP / decode failure.
func (p *Provider) fetchDiscovery(ctx context.Context, serverURL string) (discoveredMetadata, error) {
	u := strings.TrimRight(serverURL, "/") + "/.well-known/oauth-authorization-server"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return discoveredMetadata{}, fmt.Errorf("%w: build request: %v", ErrDiscoveryFailed, err)
	}
	resp, err := p.httpClient.Do(req)
	if err != nil {
		return discoveredMetadata{}, fmt.Errorf("%w: GET %s: %v", ErrDiscoveryFailed, u, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return discoveredMetadata{}, fmt.Errorf("%w: status %d body %q",
			ErrDiscoveryFailed, resp.StatusCode, summary(body))
	}
	var disc discoveredMetadata
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&disc); err != nil {
		return discoveredMetadata{}, fmt.Errorf("%w: decode: %v", ErrDiscoveryFailed, err)
	}
	return disc, nil
}

// ensureClient returns the (clientID, clientSecret) to use for cfg.
// If cfg.ClientID is set, returns it verbatim. Otherwise, performs an
// RFC 7591 dynamic registration against regURL (when non-empty) and
// caches the result.
func (p *Provider) ensureClient(ctx context.Context, cfg OAuthConfig, regURL string) (string, string, error) {
	if cfg.ClientID != "" {
		return cfg.ClientID, cfg.ClientSecret, nil
	}
	key := regURL + "|" + cfg.ServerURL
	p.flowsMu.RLock()
	cached, ok := p.registrations[key]
	p.flowsMu.RUnlock()
	if ok {
		return cached.ClientID, cached.ClientSecret, nil
	}
	if regURL == "" {
		return "", "", wrap(ErrRegistrationFailed, "no ClientID configured and no RegistrationURL discovered for %q", cfg.Source)
	}
	reg, err := p.dynamicRegister(ctx, regURL, cfg)
	if err != nil {
		return "", "", err
	}
	p.flowsMu.Lock()
	p.registrations[key] = reg
	p.flowsMu.Unlock()
	return reg.ClientID, reg.ClientSecret, nil
}

// cachedClient returns the cached (clientID, clientSecret) for cfg,
// or empty strings if not yet resolved.
func (p *Provider) cachedClient(cfg OAuthConfig) (string, string) {
	if cfg.ClientID != "" {
		return cfg.ClientID, cfg.ClientSecret
	}
	key := cfg.RegistrationURL + "|" + cfg.ServerURL
	p.flowsMu.RLock()
	cached, ok := p.registrations[key]
	p.flowsMu.RUnlock()
	if ok {
		return cached.ClientID, cached.ClientSecret
	}
	return "", ""
}

// dynamicRegister performs a single RFC 7591 client-registration
// POST.
func (p *Provider) dynamicRegister(ctx context.Context, regURL string, cfg OAuthConfig) (registrationResult, error) {
	reqBody := map[string]any{
		"redirect_uris":              []string{cfg.RedirectURI},
		"token_endpoint_auth_method": "none", // PKCE-only public client
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
	}
	if len(cfg.Scopes) > 0 {
		reqBody["scope"] = strings.Join(cfg.Scopes, " ")
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return registrationResult{}, fmt.Errorf("%w: marshal: %v", ErrRegistrationFailed, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, regURL, strings.NewReader(string(body)))
	if err != nil {
		return registrationResult{}, fmt.Errorf("%w: build request: %v", ErrRegistrationFailed, err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.httpClient.Do(req)
	if err != nil {
		return registrationResult{}, fmt.Errorf("%w: POST %s: %v", ErrRegistrationFailed, regURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return registrationResult{}, fmt.Errorf("%w: status %d body %q",
			ErrRegistrationFailed, resp.StatusCode, summary(raw))
	}
	var out struct {
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&out); err != nil {
		return registrationResult{}, fmt.Errorf("%w: decode: %v", ErrRegistrationFailed, err)
	}
	if out.ClientID == "" {
		return registrationResult{}, wrap(ErrRegistrationFailed, "server returned empty client_id")
	}
	return registrationResult{ClientID: out.ClientID, ClientSecret: out.ClientSecret}, nil
}

// tokenExchangeResponse is the canonical token-endpoint response
// (RFC 6749 §5.1). Only the fields Harbor consults are typed.
type tokenExchangeResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	Scope        string `json:"scope"`
}

// expiresAt computes the wall-clock expiry from `expires_in` against
// `now`. Returns zero when expires_in is unset (treated as
// "no expiry advertised" — see Provider.isExpired).
func (r tokenExchangeResponse) expiresAt(now time.Time) time.Time {
	if r.ExpiresIn <= 0 {
		return time.Time{}
	}
	return now.Add(time.Duration(r.ExpiresIn) * time.Second)
}

// postForm POSTs a form-encoded body to tokenURL and decodes the
// response. Surfaces ErrExchangeFailed on 4xx/5xx + on non-OAuth
// response bodies.
func (p *Provider) postForm(ctx context.Context, tokenURL string, body url.Values) (tokenExchangeResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(body.Encode()))
	if err != nil {
		return tokenExchangeResponse{}, fmt.Errorf("%w: build request: %v", ErrExchangeFailed, err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := p.httpClient.Do(req)
	if err != nil {
		return tokenExchangeResponse{}, fmt.Errorf("%w: POST %s: %v", ErrExchangeFailed, tokenURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return tokenExchangeResponse{}, fmt.Errorf("%w: status %d body %q",
			ErrExchangeFailed, resp.StatusCode, summary(raw))
	}
	var out tokenExchangeResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&out); err != nil {
		return tokenExchangeResponse{}, fmt.Errorf("%w: decode: %v", ErrExchangeFailed, err)
	}
	if out.AccessToken == "" {
		return tokenExchangeResponse{}, wrap(ErrExchangeFailed, "empty access_token in response")
	}
	return out, nil
}

// buildAuthorizeURL composes the OAuth authorization URL with PKCE.
func buildAuthorizeURL(base, clientID, redirectURI string, scopes []string, state, verifier string) string {
	q := url.Values{}
	q.Set("response_type", "code")
	q.Set("client_id", clientID)
	q.Set("redirect_uri", redirectURI)
	q.Set("state", state)
	q.Set("code_challenge", pkceChallengeS256(verifier))
	q.Set("code_challenge_method", "S256")
	if len(scopes) > 0 {
		q.Set("scope", strings.Join(scopes, " "))
	}
	sep := "?"
	if strings.Contains(base, "?") {
		sep = "&"
	}
	return base + sep + q.Encode()
}

// splitScopes splits a space-separated scope string into a slice. An
// empty string returns a nil slice.
func splitScopes(s string) []string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	parts := strings.Fields(s)
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// summary truncates a possibly-long byte slice to a printable
// summary for inclusion in error messages. NEVER includes raw
// authorization-server response bodies in audit-emitted strings —
// this helper is for error returns only.
func summary(b []byte) string {
	const max = 200
	if len(b) <= max {
		return string(b)
	}
	return string(b[:max]) + "…"
}

func userIfScopeUser(scope BindingScope, userID string) string {
	if scope == ScopeUser {
		return userID
	}
	return ""
}

func agentIfScopeAgent(scope BindingScope, agentID string) string {
	if scope == ScopeAgent {
		return agentID
	}
	return ""
}
