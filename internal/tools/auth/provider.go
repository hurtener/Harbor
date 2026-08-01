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
	// Flows is the durable, sealed pending authorization-flow store.
	// Mandatory: token persistence alone cannot recover PKCE/state/pause
	// correlation after a Runtime restart.
	Flows FlowStore
	// Bus is the event bus the provider emits tool.auth_required /
	// tool.auth_completed events on. Mandatory.
	Bus events.EventBus
	// Redactor processes the ToolAuthRequiredPayload before emission.
	// Mandatory.
	Redactor audit.Redactor
	// Coordinator is the unified pause/resume primitive.
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
// Concurrent reuse: every field below is set once at
// construction (deps + immutable maps protected by mu).
type Provider struct {
	store       TokenStore
	flows       FlowStore
	bus         events.EventBus
	redactor    audit.Redactor
	coordinator pauseresume.Coordinator
	httpClient  *http.Client
	now         func() time.Time
	flowTTL     time.Duration

	// configs is the operator-supplied set of OAuthConfigs, indexed
	// by Source. Set once at construction; read-only after.
	configs map[tools.ToolSourceID]OAuthConfig

	// flowsMu guards endpoint/client caches. Pending authorization flows are
	// owned by the durable FlowStore, not process-local provider state.
	flowsMu sync.RWMutex
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
	// agent-bound tokens shared across N concurrent sessions.
	refreshMu     sync.Mutex
	refreshFlight map[string]*refreshCall

	closed atomic.Bool
}

// discoveredMetadata caches the subset of an OAuth-authorization-server
// metadata document we consult. It is the SINGLE parse shape for an
// RFC 8414 / OIDC authorization-server-metadata document across the
// package: the interactive-flow endpoint resolver (resolveEndpoints /
// fetchDiscovery) reads the first three fields; the report-only OAuth
// requirement-discovery walker (discovery.go) reads all of them. The
// extra fields are additive JSON — the flow path ignores them.
type discoveredMetadata struct {
	Issuer                        string   `json:"issuer"`
	AuthorizationEndpoint         string   `json:"authorization_endpoint"`
	TokenEndpoint                 string   `json:"token_endpoint"`
	RegistrationEndpoint          string   `json:"registration_endpoint"`
	ScopesSupported               []string `json:"scopes_supported"`
	CodeChallengeMethodsSupported []string `json:"code_challenge_methods_supported"`
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
	// waiters is protected by Provider.refreshMu. It records how many Token
	// calls joined this still-live flight, making the single-flight boundary
	// observable without timing guesses in the concurrent-reuse test.
	waiters int
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
	if deps.Flows == nil {
		return nil, errors.New("auth: NewProvider: FlowStore required")
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
		flows:         deps.Flows,
		bus:           deps.Bus,
		redactor:      deps.Redactor,
		coordinator:   deps.Coordinator,
		httpClient:    httpClient,
		now:           clock,
		flowTTL:       flowTTL,
		configs:       cfgMap,
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
// under a single-flight gate keyed by (tenant, scope, subject, source)
// — the token store's composite key. N concurrent callers see one
// HTTP exchange.
//
// The refresh round-trip runs in its own goroutine on a context
// DETACHED from the initiating caller's cancellation (values are
// preserved; the deadline is the HTTP client timeout), so a cancelled
// caller never poisons the collapsed waiters: every caller — the
// initiator included — waits on the shared flight result or its OWN
// ctx, whichever resolves first.
func (p *Provider) refreshLocked(ctx context.Context, cfg OAuthConfig, current Token) (Token, error) {
	key := refreshKey(current)

	p.refreshMu.Lock()
	call, inflight := p.refreshFlight[key]
	if !inflight {
		call = &refreshCall{done: make(chan struct{})}
		p.refreshFlight[key] = call
		go p.runRefresh(ctx, cfg, current, key, call)
	}
	call.waiters++
	p.refreshMu.Unlock()

	select {
	case <-call.done:
		return call.token, call.err
	case <-ctx.Done():
		return Token{}, ctx.Err()
	}
}

// refreshKey composes the refresh single-flight key: the token store's
// composite (tenant, scope, subject, source), length-prefixed so
// external-input IDs containing separator bytes cannot collide two
// keys. Tenant is part of the key — two tenants sharing a user/agent
// ID must never collapse onto one refresh flight (the follower would
// receive the leader tenant's token).
func refreshKey(t Token) string {
	scope := string(t.BindingScope)
	subj := t.SubjectID()
	src := string(t.Source)
	return fmt.Sprintf("%d:%s;%d:%s;%d:%s;%d:%s",
		len(t.TenantID), t.TenantID,
		len(scope), scope,
		len(subj), subj,
		len(src), src)
}

// runRefresh is the refresh single-flight worker body: one token
// refresh shared by every caller collapsed onto the flight. The
// goroutine's lifetime is bounded by the HTTP client timeout; it
// deliberately outlives a cancelled initiator so the remaining waiters
// still get their result (the flight, not the caller, owns the
// round-trip).
func (p *Provider) runRefresh(callerCtx context.Context, cfg OAuthConfig, current Token, key string, call *refreshCall) {
	defer func() {
		// Ordering is load-bearing: the flight MUST leave the table
		// BEFORE its result is published (done closed). Otherwise a
		// caller released by the close can issue a fresh Token(), find
		// the completed flight still in the table, join it, and receive
		// this attempt's stale result instead of starting a new refresh.
		p.refreshMu.Lock()
		delete(p.refreshFlight, key)
		p.refreshMu.Unlock()
		close(call.done)
	}()

	// Detach from the initiating caller's cancellation, keeping its
	// values (identity, trace context); bound by the HTTP client
	// timeout so a wedged authorization server cannot leak the
	// goroutine.
	timeout := p.httpClient.Timeout
	if timeout <= 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(callerCtx), timeout)
	defer cancel()

	tokenURL, _, _, err := p.resolveEndpoints(ctx, cfg)
	if err != nil {
		call.err = err
		return
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
		return
	}

	t := Token{
		Source:             cfg.Source,
		BindingScope:       cfg.BindingScope,
		TenantID:           current.TenantID,
		UserID:             current.UserID,
		AgentID:            current.AgentID,
		AccessToken:        resp.AccessToken,
		RefreshToken:       refreshTokenOrCurrent(resp.RefreshToken, current.RefreshToken),
		TokenType:          resp.TokenType,
		ExpiresAt:          resp.expiresAt(p.now()),
		Scopes:             splitScopes(resp.Scope),
		LastRefreshedAt:    p.now(),
		completedFlowState: current.completedFlowState,
	}
	if err := p.store.Put(ctx, t); err != nil {
		call.err = err
		return
	}
	call.token = t
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

	rec := PendingFlowRecord{
		State:        state,
		Source:       cfg.Source,
		BindingScope: cfg.BindingScope,
		SubjectID:    subj,
		Identity:     id,
		Verifier:     verifier,
		CreatedAt:    p.now(),
		ExpiresAt:    p.now().Add(p.flowTTL),
		PauseToken:   pause.Token,
		ClientID:     clientID,
		ClientSecret: clientSecret,
		TokenURL:     tokenURL,
		RedirectURI:  cfg.RedirectURI,
	}
	if err := p.flows.Put(ctx, rec); err != nil {
		cleanupCtx, cancelCleanup := oauthCleanupContext(ctx)
		defer cancelCleanup()
		resumeErr := p.coordinator.Resume(cleanupCtx, pause.Token, pauseresume.DecisionReject, map[string]any{
			"source": string(cfg.Source), "reason": "oauth_flow_persistence_failed",
		})
		return errors.Join(fmt.Errorf("auth: persist pending flow: %w", err), resumeErr)
	}

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
		PauseToken:   string(pause.Token),
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
	tokenURL, authzURL, regURL, err := p.resolveEndpoints(ctx, cfg)
	if err != nil {
		return FlowInitiation{}, err
	}
	clientID, clientSecret, err := p.ensureClient(ctx, cfg, regURL)
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

	rec := PendingFlowRecord{
		State:        state,
		Source:       cfg.Source,
		BindingScope: cfg.BindingScope,
		SubjectID:    subj,
		Identity:     id,
		Verifier:     verifier,
		CreatedAt:    p.now(),
		ExpiresAt:    p.now().Add(p.flowTTL),
		PauseToken:   pause.Token,
		ClientID:     clientID,
		ClientSecret: clientSecret,
		TokenURL:     tokenURL,
		RedirectURI:  cfg.RedirectURI,
	}
	if err := p.flows.Put(ctx, rec); err != nil {
		cleanupCtx, cancelCleanup := oauthCleanupContext(ctx)
		defer cancelCleanup()
		resumeErr := p.coordinator.Resume(cleanupCtx, pause.Token, pauseresume.DecisionReject, map[string]any{
			"source": string(cfg.Source), "reason": "oauth_flow_persistence_failed",
		})
		return FlowInitiation{}, errors.Join(fmt.Errorf("auth: persist pending flow: %w", err), resumeErr)
	}

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
//
// CompleteFlow is the resume half of the tool-OAuth pause. Its
// production caller is `auth.CallbackHandler`,
// which `harbor dev` mounts at `GET /v1/tools/oauth/callback`;
// headless embedders mount the same handler (or call CompleteFlow
// directly) at whatever path matches the configured RedirectURI. A
// bare Coordinator.Resume without CompleteFlow re-parks the run
// immediately (the token is still missing), so this method is the
// ONLY correct completion path.
func (p *Provider) CompleteFlow(ctx context.Context, state, code string) (_ Token, retErr error) {
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

	// A prior attempt may have persisted the token and exact-state completion
	// tombstone, then failed before or during destructive pending-flow cleanup.
	// Consult that immutable per-flow oracle before the mutable singleton token
	// slot: a later flow for the same user/source may legitimately replace the
	// current credential and its completedFlowState marker.
	completed, completedOK, err := p.flows.GetCompleted(ctx, state)
	if err != nil {
		return Token{}, err
	}
	if completedOK {
		return p.convergeCompletedFlow(ctx, completed)
	}

	rec, ok, err := p.flows.Get(ctx, state)
	if err != nil {
		return Token{}, err
	}
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
	if cfg.BindingScope != rec.BindingScope || cfg.SubjectID(rec.Identity) != rec.SubjectID {
		return Token{}, fmt.Errorf("%w: OAuth binding changed while flow was pending", ErrConfigInvalid)
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
	rec, claim, ok, err := p.flows.Claim(ctx, state)
	if err != nil {
		return Token{}, err
	}
	if !ok {
		return Token{}, ErrFlowInProgress
	}
	claimed := true
	defer func() {
		if claimed {
			cleanupCtx, cancelCleanup := oauthCleanupContext(ctx)
			defer cancelCleanup()
			retErr = errors.Join(retErr, p.flows.Release(cleanupCtx, claim))
		}
	}()
	if rec.TerminalFailure != "" {
		cleanupCtx, cancelCleanup := oauthCleanupContext(ctx)
		defer cancelCleanup()
		resumeErr := p.coordinator.Resume(cleanupCtx, rec.PauseToken, pauseresume.DecisionReject, map[string]any{
			"source": string(cfg.Source), "binding": string(cfg.BindingScope), "reason": rec.TerminalFailure,
		})
		if resumeErr != nil {
			if !errors.Is(resumeErr, pauseresume.ErrAlreadyResumed) {
				return Token{}, fmt.Errorf("auth: retry terminal OAuth rejection: %w", resumeErr)
			}
			status, statusErr := p.coordinator.Status(cleanupCtx, rec.PauseToken)
			if statusErr != nil || status.State != pauseresume.StatusResumed || status.Decision != pauseresume.DecisionReject {
				return Token{}, errors.Join(
					fmt.Errorf("auth: terminal OAuth rejection decision mismatch: state=%q decision=%q", status.State, status.Decision),
					statusErr,
				)
			}
		}
		if err := p.flows.Finish(cleanupCtx, claim); err != nil {
			return Token{}, err
		}
		claimed = false
		return Token{}, fmt.Errorf("auth: OAuth flow terminated after token persistence failure: %w", ErrExchangeFailed)
	}

	tok, persisted, err := p.store.Get(ctx, rec.BindingScope, rec.SubjectID, rec.Source)
	if err != nil {
		return Token{}, err
	}
	if !persisted || tok.completedFlowState != state {
		body := url.Values{}
		body.Set("grant_type", "authorization_code")
		body.Set("code", code)
		body.Set("redirect_uri", rec.RedirectURI)
		body.Set("client_id", rec.ClientID)
		if rec.ClientSecret != "" {
			body.Set("client_secret", rec.ClientSecret)
		}
		body.Set("code_verifier", rec.Verifier)

		resp, postErr := p.postForm(ctx, rec.TokenURL, body)
		if postErr != nil {
			return Token{}, postErr
		}
		now := p.now()
		tok = Token{
			Source:             cfg.Source,
			BindingScope:       cfg.BindingScope,
			TenantID:           rec.Identity.TenantID,
			UserID:             userIfScopeUser(cfg.BindingScope, rec.Identity.UserID),
			AgentID:            agentIfScopeAgent(cfg.BindingScope, cfg.AgentID),
			AccessToken:        resp.AccessToken,
			RefreshToken:       resp.RefreshToken,
			TokenType:          resp.TokenType,
			ExpiresAt:          resp.expiresAt(now),
			Scopes:             splitScopes(resp.Scope),
			LastRefreshedAt:    now,
			completedFlowState: state,
		}
		if err := p.store.Put(ctx, tok); err != nil {
			// The authorization code has been spent but the token is not
			// durable, so retrying this callback cannot recover. Terminate the
			// unified pause explicitly and consume the flow instead of leaving a
			// permanently paused orphan behind an unusable one-time code.
			cleanupCtx, cancelCleanup := oauthCleanupContext(ctx)
			defer cancelCleanup()
			markErr := p.flows.MarkTerminal(cleanupCtx, claim, flowTerminalWriteFailed)
			resumeErr := p.coordinator.Resume(cleanupCtx, rec.PauseToken, pauseresume.DecisionReject, map[string]any{
				"source": string(cfg.Source), "binding": string(cfg.BindingScope), "reason": "oauth_token_persistence_failed",
			})
			resumeOutcomeErr := resumeErr
			if errors.Is(resumeErr, pauseresume.ErrAlreadyResumed) {
				status, statusErr := p.coordinator.Status(cleanupCtx, rec.PauseToken)
				if statusErr == nil && status.State == pauseresume.StatusResumed && status.Decision == pauseresume.DecisionReject {
					resumeOutcomeErr = nil
				} else {
					resumeOutcomeErr = errors.Join(
						fmt.Errorf("auth: token-persistence rejection decision mismatch: state=%q decision=%q", status.State, status.Decision),
						statusErr,
					)
				}
			}
			var finishErr error
			if resumeOutcomeErr == nil {
				finishErr = p.flows.Finish(cleanupCtx, claim)
				if finishErr == nil {
					claimed = false
				}
			} else if markErr != nil {
				// The code is spent and neither durable terminal state nor pause
				// rejection succeeded. Retain the claim so no retry can re-exchange
				// the one-time code against ambiguous state.
				claimed = false
			}
			return Token{}, errors.Join(fmt.Errorf("auth: persist exchanged token: %w", err), markErr, resumeOutcomeErr, finishErr)
		}
	}

	completed = CompletedFlowRecord{
		State:            state,
		TokenMarker:      state,
		Source:           rec.Source,
		BindingScope:     rec.BindingScope,
		SubjectID:        rec.SubjectID,
		Identity:         rec.Identity,
		PauseToken:       rec.PauseToken,
		ExpectedDecision: pauseresume.DecisionResume,
		ExpiresAt:        rec.ExpiresAt,
	}
	// Persist the exact-state proof before resuming the pause or deleting any
	// routing material. If this write cannot be reconciled, retain the pending
	// record and claim; retry can still use the token's encrypted exact-state
	// marker to reach this point without another exchange.
	cleanupCtx, cancelCompletion := oauthCleanupContext(ctx)
	if err := p.flows.MarkCompleted(cleanupCtx, claim, completed); err != nil {
		cancelCompletion()
		return Token{}, err
	}
	cancelCompletion()

	// Resume the parked run with a typed `DecisionResume` marker —
	// this is a generic resume of a non-approval pause (the OAuth flow
	// completed), distinct from approve / reject / timeout (issue #113).
	// A failure here is loud — the pause would otherwise
	// linger as a record nobody can claim.
	resumeErr := p.coordinator.Resume(ctx, rec.PauseToken, pauseresume.DecisionResume, map[string]any{
		"source":       string(cfg.Source),
		"binding":      string(cfg.BindingScope),
		"completed_at": p.now().Format(time.RFC3339),
	})
	// A prior attempt may have durably stored this flow's token and resumed the
	// pause, then failed while deleting the flow record. The encrypted exact-state
	// marker stored atomically with the token proves it belongs to this flow;
	// AlreadyResumed is therefore a successful cleanup retry, never permission to
	// skip exchange.
	if resumeErr != nil {
		if !errors.Is(resumeErr, pauseresume.ErrAlreadyResumed) {
			return Token{}, fmt.Errorf("auth: coordinator.Resume: %w", resumeErr)
		}
		status, statusErr := p.coordinator.Status(ctx, rec.PauseToken)
		if statusErr != nil || status.State != pauseresume.StatusResumed || status.Decision != pauseresume.DecisionResume {
			return Token{}, errors.Join(
				fmt.Errorf("auth: OAuth pause terminal decision is not resume: state=%q decision=%q", status.State, status.Decision),
				statusErr,
			)
		}
	}
	cleanupCtx, cancelCleanup := oauthCleanupContext(ctx)
	defer cancelCleanup()
	if err := p.flows.Finish(cleanupCtx, claim); err != nil {
		return Token{}, err
	}
	claimed = false

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

// convergeCompletedFlow resolves an idempotent callback retry from the sealed
// exact-state tombstone. It deliberately does not require the mutable current
// token's completedFlowState to equal this flow: a later same-user/source flow
// may already have replaced that credential. The tombstone's token marker and
// the exact unified-pause decision are the durable proof for this flow.
func (p *Provider) convergeCompletedFlow(ctx context.Context, completed CompletedFlowRecord) (Token, error) {
	cfg, ok := p.configs[completed.Source]
	if !ok {
		return Token{}, fmt.Errorf("%w: source %q removed after OAuth completion", ErrConfigInvalid, completed.Source)
	}
	if cfg.BindingScope == ScopeAgent && !registry.HasControlScope(ctx) {
		return Token{}, ErrAdminScopeRequired
	}
	if cfg.BindingScope != completed.BindingScope || cfg.SubjectID(completed.Identity) != completed.SubjectID {
		return Token{}, fmt.Errorf("%w: OAuth binding changed after flow completion", ErrConfigInvalid)
	}
	id, err := identityFromCtx(ctx)
	if err != nil {
		return Token{}, err
	}
	if id != completed.Identity || completed.TokenMarker != completed.State ||
		completed.ExpectedDecision != pauseresume.DecisionResume {
		return Token{}, ErrStateMismatch
	}
	if p.now().After(completed.ExpiresAt) {
		cleanupCtx, cancelCleanup := oauthCleanupContext(ctx)
		defer cancelCleanup()
		return Token{}, errors.Join(ErrFlowExpired, p.flows.ForgetCompleted(cleanupCtx, completed))
	}

	// The completion marker proves an exact token Put succeeded. Return the
	// current credential for the same tuple; it may be a newer completion and is
	// therefore intentionally not required to carry this older state's marker.
	tok, persisted, err := p.store.Get(ctx, completed.BindingScope, completed.SubjectID, completed.Source)
	if err != nil {
		return Token{}, err
	}
	if !persisted {
		return Token{}, ErrTokenNotFound
	}

	resumeErr := p.coordinator.Resume(ctx, completed.PauseToken, completed.ExpectedDecision, map[string]any{
		"source":       string(completed.Source),
		"binding":      string(completed.BindingScope),
		"completed_at": p.now().Format(time.RFC3339),
	})
	if resumeErr != nil && !errors.Is(resumeErr, pauseresume.ErrAlreadyResumed) {
		return Token{}, fmt.Errorf("auth: converge completed OAuth pause: %w", resumeErr)
	}
	status, statusErr := p.coordinator.Status(ctx, completed.PauseToken)
	if statusErr != nil || status.State != pauseresume.StatusResumed || status.Decision != completed.ExpectedDecision {
		return Token{}, errors.Join(
			fmt.Errorf("auth: completed OAuth pause decision mismatch: state=%q decision=%q", status.State, status.Decision),
			statusErr,
		)
	}

	// A Finish failure that happened before deleting the pending flow leaves
	// secret-bearing PKCE/client material behind. Converge that cleanup under a
	// fresh exact claim. If the pending record is already absent, the earlier
	// Finish landed (possibly with a lost acknowledgement) and no cleanup is
	// needed. The completed tombstone itself is intentionally retained.
	if _, pending, err := p.flows.Get(ctx, completed.State); err != nil {
		return Token{}, err
	} else if pending {
		_, claim, claimed, claimErr := p.flows.Claim(ctx, completed.State)
		if claimErr != nil {
			return Token{}, claimErr
		}
		if !claimed {
			return Token{}, ErrFlowInProgress
		}
		cleanupCtx, cancelCleanup := oauthCleanupContext(ctx)
		defer cancelCleanup()
		if err := p.flows.Finish(cleanupCtx, claim); err != nil {
			releaseErr := p.flows.Release(cleanupCtx, claim)
			return Token{}, errors.Join(err, releaseErr)
		}
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

// AllowedDownstreamHosts implements OAuthProvider. The bare interactive
// Provider carries no southbound-binding sink allow-list of its own — the
// allow-list is a driver-boundary concern declared per operator-config
// entry and carried by the wrapping driver (the `oauth2` / `tokenexchange`
// drivers store it and answer here). A direct `*Provider` used as an
// OAuthProvider therefore declares no allowed sink, so any southbound
// binding against it is refused fail-closed until a driver supplies the
// allow-list.
func (p *Provider) AllowedDownstreamHosts() []string { return nil }

// ConfigFor returns a copy of the OAuthConfig for source, or false
// when no attachment is configured. Useful for transport drivers
// that need to inspect the binding scope before invoking Token (e.g.
// to decide whether to include an `Authorization` header at all).
func (p *Provider) ConfigFor(source tools.ToolSourceID) (OAuthConfig, bool) {
	cfg, ok := p.configs[source]
	return cfg, ok
}

// PendingFlow reports whether `state` corresponds to an in-flight
// flow record, returning the record's read-only PendingFlowInfo
// projection. The callback handler (`auth.CallbackHandler`) uses it
// to locate the owning provider and to rebuild the completing ctx's
// identity from the record. The lookup does NOT consume the record.
func (p *Provider) PendingFlow(ctx context.Context, state string) (PendingFlowInfo, bool, error) {
	rec, ok, err := p.flows.Get(ctx, state)
	if err != nil {
		return PendingFlowInfo{}, false, fmt.Errorf("auth: PendingFlow: %w", err)
	}
	if !ok {
		completed, completedOK, completedErr := p.flows.GetCompleted(ctx, state)
		if completedErr != nil {
			return PendingFlowInfo{}, false, fmt.Errorf("auth: PendingFlow completed lookup: %w", completedErr)
		}
		if !completedOK {
			return PendingFlowInfo{}, false, nil
		}
		if _, owned := p.configs[completed.Source]; !owned {
			return PendingFlowInfo{}, false, nil
		}
		return PendingFlowInfo{
			Source:       completed.Source,
			BindingScope: completed.BindingScope,
			Identity:     completed.Identity,
			ExpiresAt:    completed.ExpiresAt,
		}, true, nil
	}
	if _, owned := p.configs[rec.Source]; !owned {
		return PendingFlowInfo{}, false, nil
	}
	return PendingFlowInfo{
		Source:       rec.Source,
		BindingScope: rec.BindingScope,
		Identity:     rec.Identity,
		ExpiresAt:    rec.ExpiresAt,
	}, true, nil
}

// DenyFlow consumes the flow record for `state` and resumes the
// associated pause with the typed DecisionReject marker — the
// fail-loud terminal for an upstream authorization denial. The token
// store is never touched (there is no token); the pause record does
// not linger to flow-TTL on a denial the authorization server already
// made final — a recorded design decision.
//
// Identity + admin-scope checks mirror CompleteFlow: the calling ctx
// must carry the flow's parking identity, and a ScopeAgent flow
// requires the control scope.
func (p *Provider) DenyFlow(ctx context.Context, state, reason string) (retErr error) {
	if p.closed.Load() {
		return ErrProviderClosed
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("auth: DenyFlow cancelled: %w", err)
	}
	if state == "" {
		return wrap(ErrFlowNotFound, "empty state")
	}

	rec, ok, err := p.flows.Get(ctx, state)
	if err != nil {
		return err
	}
	if !ok {
		return ErrFlowNotFound
	}
	if p.now().After(rec.ExpiresAt) {
		return ErrFlowExpired
	}

	cfg, cfgOK := p.configs[rec.Source]
	if !cfgOK {
		return fmt.Errorf("%w: source %q removed mid-flow", ErrConfigInvalid, rec.Source)
	}
	if cfg.BindingScope == ScopeAgent && !registry.HasControlScope(ctx) {
		return ErrAdminScopeRequired
	}
	if cfg.BindingScope != rec.BindingScope || cfg.SubjectID(rec.Identity) != rec.SubjectID {
		return fmt.Errorf("%w: OAuth binding changed while flow was pending", ErrConfigInvalid)
	}

	id, err := identityFromCtx(ctx)
	if err != nil {
		return err
	}
	if id != rec.Identity {
		return ErrStateMismatch
	}
	rec, claim, ok, err := p.flows.Claim(ctx, state)
	if err != nil {
		return err
	}
	if !ok {
		return ErrFlowInProgress
	}
	claimed := true
	defer func() {
		if claimed {
			cleanupCtx, cancelCleanup := oauthCleanupContext(ctx)
			defer cancelCleanup()
			retErr = errors.Join(retErr, p.flows.Release(cleanupCtx, claim))
		}
	}()

	// Resume the pause with DecisionReject so the denial is loud on
	// the canonical event stream (pause.resumed{Decision: reject})
	// instead of the pause hanging to TTL. Classify `reason` again at this
	// trust boundary even when CallbackHandler already did so: direct callers
	// must not persist arbitrary redirect text into the canonical pause record.
	denialClass := classifyOAuthDenial(reason)
	resumeErr := p.coordinator.Resume(ctx, rec.PauseToken, pauseresume.DecisionReject, map[string]any{
		"source":        string(cfg.Source),
		"binding":       string(cfg.BindingScope),
		"denied_reason": string(denialClass),
		"denied_at":     p.now().Format(time.RFC3339),
	})
	if resumeErr != nil {
		if !errors.Is(resumeErr, pauseresume.ErrAlreadyResumed) {
			return fmt.Errorf("auth: coordinator.Resume (deny): %w", resumeErr)
		}
		status, statusErr := p.coordinator.Status(ctx, rec.PauseToken)
		if statusErr != nil || status.State != pauseresume.StatusResumed || status.Decision != pauseresume.DecisionReject {
			return errors.Join(
				fmt.Errorf("auth: denied OAuth pause terminal decision mismatch: state=%q decision=%q", status.State, status.Decision),
				statusErr,
			)
		}
	}
	cleanupCtx, cancelCleanup := oauthCleanupContext(ctx)
	defer cancelCleanup()
	if err := p.flows.Finish(cleanupCtx, claim); err != nil {
		return err
	}
	claimed = false
	return nil
}

// oauthDenialClass is the closed, local vocabulary permitted to cross the
// untrusted OAuth redirect boundary. The seven protocol error codes are from
// RFC 6749 sections 4.1.2.1 and 4.2.2.1; anything else is deliberately reduced
// to Harbor's static authorization-denied classification.
type oauthDenialClass string

const (
	denialInvalidRequest           oauthDenialClass = "invalid_request"
	denialUnauthorizedClient       oauthDenialClass = "unauthorized_client"
	denialAccessDenied             oauthDenialClass = "access_denied"
	denialUnsupportedResponseType  oauthDenialClass = "unsupported_response_type"
	denialInvalidScope             oauthDenialClass = "invalid_scope"
	denialServerError              oauthDenialClass = "server_error"
	denialTemporarilyUnavailable   oauthDenialClass = "temporarily_unavailable"
	denialAuthorizationDeniedLocal oauthDenialClass = "authorization_denied"
)

func classifyOAuthDenial(value string) oauthDenialClass {
	switch oauthDenialClass(value) {
	case denialInvalidRequest,
		denialUnauthorizedClient,
		denialAccessDenied,
		denialUnsupportedResponseType,
		denialInvalidScope,
		denialServerError,
		denialTemporarilyUnavailable:
		return oauthDenialClass(value)
	default:
		return denialAuthorizationDeniedLocal
	}
}

const oauthCleanupTimeout = 5 * time.Second

// oauthCleanupContext preserves the initiating identity and control-scope
// values while detaching cleanup from a caller cancellation that may arrive
// after Harbor has allocated a pause or spent a one-time authorization code.
// The timeout keeps cleanup bounded and joins/shuts down no background work.
func oauthCleanupContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), oauthCleanupTimeout)
}

// emitEvent Publishes onto the bus.
//
// The payload is SafePayload by construction (every field is
// caller-controllable surface — auth URLs, scope identifiers, opaque
// pause tokens; never plaintext OAuth tokens), so the bus skips the
// redactor. But the acceptance criterion is explicit:
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
		return discoveredMetadata{}, fmt.Errorf("%w: build request: %w", ErrDiscoveryFailed, err)
	}
	resp, err := p.httpClient.Do(req)
	if err != nil {
		return discoveredMetadata{}, fmt.Errorf("%w: GET %s: %w", ErrDiscoveryFailed, u, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return discoveredMetadata{}, fmt.Errorf("%w: status %d", ErrDiscoveryFailed, resp.StatusCode)
	}
	var disc discoveredMetadata
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&disc); err != nil {
		return discoveredMetadata{}, fmt.Errorf("%w: decode: %w", ErrDiscoveryFailed, err)
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
		return registrationResult{}, fmt.Errorf("%w: marshal: %w", ErrRegistrationFailed, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, regURL, strings.NewReader(string(body)))
	if err != nil {
		return registrationResult{}, fmt.Errorf("%w: build request: %w", ErrRegistrationFailed, err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := p.httpClient.Do(req)
	if err != nil {
		return registrationResult{}, fmt.Errorf("%w: POST %s: %w", ErrRegistrationFailed, regURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return registrationResult{}, fmt.Errorf("%w: status %d", ErrRegistrationFailed, resp.StatusCode)
	}
	var out struct {
		ClientID     string `json:"client_id"`
		ClientSecret string `json:"client_secret"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&out); err != nil {
		return registrationResult{}, fmt.Errorf("%w: decode: %w", ErrRegistrationFailed, err)
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
		return tokenExchangeResponse{}, fmt.Errorf("%w: build request: %w", ErrExchangeFailed, err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := p.httpClient.Do(req)
	if err != nil {
		return tokenExchangeResponse{}, fmt.Errorf("%w: POST %s: %w", ErrExchangeFailed, tokenURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return tokenExchangeResponse{}, fmt.Errorf("%w: status %d", ErrExchangeFailed, resp.StatusCode)
	}
	var out tokenExchangeResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&out); err != nil {
		return tokenExchangeResponse{}, fmt.Errorf("%w: decode: %w", ErrExchangeFailed, err)
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
