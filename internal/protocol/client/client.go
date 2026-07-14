// Package client provides the authenticated REST/SSE Harbor Protocol client.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/methods"
	"github.com/hurtener/Harbor/internal/protocol/types"
)

const (
	defaultMaxResponseBytes = 8 << 20
	defaultMaxErrorBytes    = 64 << 10
)

var (
	// ErrInvalidConnection reports an unusable base URL or token source.
	ErrInvalidConnection = errors.New("protocol client: invalid connection")
	// ErrTokenRequired reports that the token source returned an empty token.
	ErrTokenRequired = errors.New("protocol client: bearer token required")
	// ErrTokenIdentityMismatch reports that a token cannot authenticate the requested identity.
	ErrTokenIdentityMismatch = errors.New("protocol client: bearer token identity mismatch")
	// ErrIdentityRequired reports an incomplete client isolation identity.
	ErrIdentityRequired = errors.New("protocol client: complete identity required")
	// ErrResponseTooLarge reports a response beyond the configured safety bound.
	ErrResponseTooLarge = errors.New("protocol client: response body too large")
	// ErrMalformedResponse reports invalid or non-canonical response JSON.
	ErrMalformedResponse = errors.New("protocol client: malformed response")
	// ErrIncompatibleProtocol reports a Runtime with a different Protocol major.
	ErrIncompatibleProtocol = errors.New("protocol client: incompatible Protocol version")
)

// TokenSource resolves a bearer token for each requested identity. The scope is
// an isolated copy and may include the complete impersonation chain. Sources
// must be safe for concurrent use and must reject identities they cannot
// authenticate rather than returning a token for a different session.
type TokenSource interface {
	Token(context.Context, types.IdentityScope) (string, error)
}

// TokenSourceFunc adapts a function to TokenSource.
type TokenSourceFunc func(context.Context, types.IdentityScope) (string, error)

// Token calls f with an isolated identity copy.
func (f TokenSourceFunc) Token(ctx context.Context, scope types.IdentityScope) (string, error) {
	return f(ctx, cloneIdentity(scope))
}

type staticToken struct {
	token     string
	principal types.IdentityScope
}

// StaticToken returns a token source bound to principal. It rejects a regular
// session clone because one JWT cannot authenticate another session. For
// impersonation, principal is matched against Actor while the target may vary.
func StaticToken(token string, principal types.IdentityScope) TokenSource {
	return staticToken{token: token, principal: cloneIdentity(principal)}
}

func (s staticToken) Token(_ context.Context, requested types.IdentityScope) (string, error) {
	principal := requested
	if requested.IsImpersonating() && requested.Actor != nil {
		principal = *requested.Actor
	}
	if !sameTriple(s.principal, principal) {
		return "", fmt.Errorf("%w: token principal %s/%s/%s cannot authenticate %s/%s/%s",
			ErrTokenIdentityMismatch,
			s.principal.Tenant, s.principal.User, s.principal.Session,
			principal.Tenant, principal.User, principal.Session)
	}
	return s.token, nil
}

// Connection describes one authenticated Runtime attachment.
type Connection struct {
	BaseURL  string
	Token    TokenSource
	Identity types.IdentityScope
}

// Option configures a Client at construction.
type Option func(*config)

type config struct {
	httpClient       *http.Client
	maxResponseBytes int64
	maxErrorBytes    int64
}

// WithHTTPClient supplies the HTTP client used for REST and SSE requests.
// A nil value is ignored.
func WithHTTPClient(httpClient *http.Client) Option {
	return func(cfg *config) {
		if httpClient != nil {
			cfg.httpClient = httpClient
		}
	}
}

// WithResponseLimits overrides the success and error body bounds. Non-positive
// values retain their defaults.
func WithResponseLimits(successBytes, errorBytes int64) Option {
	return func(cfg *config) {
		if successBytes > 0 {
			cfg.maxResponseBytes = successBytes
		}
		if errorBytes > 0 {
			cfg.maxErrorBytes = errorBytes
		}
	}
}

// ProtocolError is a typed REST or pre-stream Protocol failure.
type ProtocolError struct {
	Status  int
	Code    protoerrors.Code
	Message string
	Cause   error
}

// Error implements error.
func (e *ProtocolError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("protocol client: HTTP %d: %s: %s", e.Status, e.Code, e.Message)
	}
	if e.Cause != nil {
		return fmt.Sprintf("protocol client: HTTP %d: %v", e.Status, e.Cause)
	}
	return fmt.Sprintf("protocol client: HTTP %d: %s", e.Status, e.Message)
}

// Unwrap returns the response decoding cause, when present.
func (e *ProtocolError) Unwrap() error { return e.Cause }

// Client is the supported immutable, concurrent-safe Protocol attachment.
type Client interface {
	RuntimeInfo(context.Context) (types.RuntimeInfo, error)
	RuntimeHealth(context.Context) (types.RuntimeHealth, error)
	Start(context.Context, types.StartRequest) (types.StartResponse, error)
	TasksList(context.Context, types.TaskListRequest) (types.TaskListResponse, error)
	TasksGet(context.Context, types.TaskGetRequest) (types.TaskDetail, error)
	SessionsList(context.Context, types.SessionsListRequest) (types.SessionsListResponse, error)
	SessionsInspect(context.Context, types.SessionsInspectRequest) (types.SessionsInspectResponse, error)
	SessionsSetTitle(context.Context, types.SessionsSetTitleRequest) (types.SessionsSetTitleResponse, error)
	SessionsDelete(context.Context) (types.SessionsDeleteResponse, error)
	StateHistory(context.Context, types.StateHistoryRequest) (types.StateHistoryResponse, error)
	PauseList(context.Context, types.PauseListRequest) (types.PauseListResponse, error)
	Control(context.Context, methods.Method, types.ControlRequest) (types.ControlResponse, error)
	ArtifactsPut(context.Context, types.ArtifactsPutRequest) (types.ArtifactsPutResponse, error)
	ArtifactsList(context.Context, types.ArtifactsListRequest) (types.ArtifactsListResponse, error)
	Subscribe(context.Context, StreamOptions) (*EventStream, error)
	WithSession(string) Client
	Identity() types.IdentityScope
}

type client struct {
	baseURL          *url.URL
	token            TokenSource
	identity         types.IdentityScope
	httpClient       *http.Client
	maxResponseBytes int64
	maxErrorBytes    int64
}

// New constructs a Protocol client. An explicit identity is copied at the
// boundary and must be complete; it may be omitted when the verified JWT is the
// sole identity carrier. TokenSource receives the selected scope for every REST
// and SSE request.
func New(conn Connection, opts ...Option) (Client, error) {
	base, err := url.Parse(strings.TrimSpace(conn.BaseURL))
	if err != nil || base.Scheme == "" || base.Host == "" || (base.Scheme != "http" && base.Scheme != "https") {
		return nil, fmt.Errorf("%w: BaseURL must be an absolute http(s) URL", ErrInvalidConnection)
	}
	if conn.Token == nil {
		return nil, fmt.Errorf("%w: Token is nil", ErrInvalidConnection)
	}
	if !validConnectionIdentity(conn.Identity) {
		return nil, ErrIdentityRequired
	}
	base.Path = strings.TrimRight(base.Path, "/")
	base.RawQuery = ""
	base.Fragment = ""
	cfg := config{
		httpClient:       http.DefaultClient,
		maxResponseBytes: defaultMaxResponseBytes,
		maxErrorBytes:    defaultMaxErrorBytes,
	}
	for _, opt := range opts {
		opt(&cfg)
	}
	return &client{
		baseURL:          base,
		token:            conn.Token,
		identity:         cloneIdentity(conn.Identity),
		httpClient:       cfg.httpClient,
		maxResponseBytes: cfg.maxResponseBytes,
		maxErrorBytes:    cfg.maxErrorBytes,
	}, nil
}

// WithSession returns an immutable clone selecting session. For impersonation,
// it retargets the execution identity and Impersonating while retaining the
// verified Actor and Requester authentication principal.
func (c *client) WithSession(session string) Client {
	clone := *c
	clone.identity = cloneIdentity(c.identity)
	clone.identity.Session = strings.TrimSpace(session)
	clone.identity.Run = ""
	if clone.identity.Impersonating != nil {
		clone.identity.Impersonating.Session = clone.identity.Session
		clone.identity.Impersonating.Run = ""
	}
	return &clone
}

// Identity returns the client's identity value.
func (c *client) Identity() types.IdentityScope { return cloneIdentity(c.identity) }

// Call executes one authenticated JSON POST against path. It is the generic
// core for later typed namespaces; callers remain responsible for using a
// canonical route and Protocol-owned request/response values.
func (c *client) Call(ctx context.Context, path string, request, response any) error {
	body, err := json.Marshal(request)
	if err != nil {
		return fmt.Errorf("protocol client: encode request: %w", err)
	}
	req, err := c.newRequest(ctx, http.MethodPost, path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("protocol client: POST %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return c.decodeError(resp)
	}
	if response == nil {
		_, err = readBounded(resp.Body, c.maxResponseBytes)
		return err
	}
	data, err := readBounded(resp.Body, c.maxResponseBytes)
	if err != nil {
		return err
	}
	if err := decodeStrict(data, response); err != nil {
		return fmt.Errorf("%w: %w", ErrMalformedResponse, err)
	}
	return nil
}

func (c *client) newRequest(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	identity := c.scope()
	token, err := c.token.Token(ctx, identity)
	if err != nil {
		return nil, fmt.Errorf("protocol client: resolve bearer token: %w", err)
	}
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, ErrTokenRequired
	}
	u := *c.baseURL
	u.Path = strings.TrimRight(u.Path, "/") + "/" + strings.TrimLeft(path, "/")
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return nil, fmt.Errorf("protocol client: build request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-Harbor-Tenant", identity.Tenant)
	req.Header.Set("X-Harbor-User", identity.User)
	req.Header.Set("X-Harbor-Session", identity.Session)
	return req, nil
}

func (c *client) decodeError(resp *http.Response) error {
	data, err := readBounded(resp.Body, c.maxErrorBytes)
	if err != nil {
		return &ProtocolError{Status: resp.StatusCode, Cause: err}
	}
	var wire protoerrors.Error
	if err := decodeStrict(data, &wire); err != nil {
		return &ProtocolError{
			Status:  resp.StatusCode,
			Message: strings.TrimSpace(string(data)),
			Cause:   fmt.Errorf("%w: Protocol error envelope: %w", ErrMalformedResponse, err),
		}
	}
	return &ProtocolError{Status: resp.StatusCode, Code: wire.Code, Message: wire.Message}
}

func readBounded(r io.Reader, limit int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, fmt.Errorf("protocol client: read response: %w", err)
	}
	if int64(len(data)) > limit {
		return nil, ErrResponseTooLarge
	}
	return data, nil
}

func decodeStrict(data []byte, out any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return err
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}

func (c *client) callMethod(ctx context.Context, method methods.Method, request, response any) error {
	return c.Call(ctx, routeFor(method), request, response)
}

func routeFor(method methods.Method) string {
	name := string(method)
	suffix := name
	if dot := strings.LastIndexByte(name, '.'); dot >= 0 {
		suffix = name[dot+1:]
	}
	switch {
	case methods.IsTasksMethod(method):
		return "/v1/tasks/" + suffix
	case methods.IsSessionsMethod(method):
		return "/v1/sessions/" + suffix
	case methods.IsStateMethod(method):
		return "/v1/state/" + suffix
	case methods.IsPauseMethod(method):
		return "/v1/pause/" + suffix
	default:
		return "/v1/control/" + name
	}
}

func (c *client) scope() types.IdentityScope { return cloneIdentity(c.identity) }

// RuntimeInfo reads and validates the Runtime handshake.
func (c *client) RuntimeInfo(ctx context.Context) (types.RuntimeInfo, error) {
	var out types.RuntimeInfo
	err := c.callMethod(ctx, methods.MethodRuntimeInfo, types.RuntimeInfoRequest{Identity: c.scope()}, &out)
	if err != nil {
		return types.RuntimeInfo{}, err
	}
	version, err := types.ParseVersion(out.ProtocolVersion)
	if err != nil || !version.Compatible(types.CurrentVersion) {
		return types.RuntimeInfo{}, fmt.Errorf("%w: client=%s runtime=%q", ErrIncompatibleProtocol, types.ProtocolVersion, out.ProtocolVersion)
	}
	return out, nil
}

// RuntimeHealth reads the Runtime readiness rollup.
func (c *client) RuntimeHealth(ctx context.Context) (types.RuntimeHealth, error) {
	var out types.RuntimeHealth
	err := c.callMethod(ctx, methods.MethodRuntimeHealth, types.RuntimeInfoRequest{Identity: c.scope()}, &out)
	return out, err
}

// Start starts a foreground task in this client's session.
func (c *client) Start(ctx context.Context, request types.StartRequest) (types.StartResponse, error) {
	request.Identity = c.scope()
	var out types.StartResponse
	err := c.callMethod(ctx, methods.MethodStart, request, &out)
	return out, err
}

// TasksList lists tasks visible to this client's identity.
func (c *client) TasksList(ctx context.Context, request types.TaskListRequest) (types.TaskListResponse, error) {
	request.Identity = c.scope()
	var out types.TaskListResponse
	err := c.callMethod(ctx, methods.MethodTasksList, request, &out)
	return out, err
}

// TasksGet gets one task visible to this client's identity.
func (c *client) TasksGet(ctx context.Context, request types.TaskGetRequest) (types.TaskDetail, error) {
	request.Identity = c.scope()
	var out types.TaskDetail
	err := c.callMethod(ctx, methods.MethodTasksGet, request, &out)
	return out, err
}

// SessionsList lists sessions visible to this client's identity.
func (c *client) SessionsList(ctx context.Context, request types.SessionsListRequest) (types.SessionsListResponse, error) {
	request.Identity = c.scope()
	var out types.SessionsListResponse
	err := c.callMethod(ctx, methods.MethodSessionsList, request, &out)
	return out, err
}

// SessionsInspect inspects one session visible to this client's identity.
func (c *client) SessionsInspect(ctx context.Context, request types.SessionsInspectRequest) (types.SessionsInspectResponse, error) {
	request.Identity = c.scope()
	var out types.SessionsInspectResponse
	err := c.callMethod(ctx, methods.MethodSessionsInspect, request, &out)
	return out, err
}

// SessionsSetTitle sets or clears a session title.
func (c *client) SessionsSetTitle(ctx context.Context, request types.SessionsSetTitleRequest) (types.SessionsSetTitleResponse, error) {
	request.Identity = c.scope()
	var out types.SessionsSetTitleResponse
	err := c.callMethod(ctx, methods.MethodSessionsSetTitle, request, &out)
	return out, err
}

// SessionsDelete erases this client's session and its scoped data.
func (c *client) SessionsDelete(ctx context.Context) (types.SessionsDeleteResponse, error) {
	var out types.SessionsDeleteResponse
	err := c.callMethod(ctx, methods.MethodSessionsDelete, types.SessionsDeleteRequest{Identity: c.scope()}, &out)
	return out, err
}

// StateHistory reads a bounded durable-event window.
func (c *client) StateHistory(ctx context.Context, request types.StateHistoryRequest) (types.StateHistoryResponse, error) {
	request.Identity = c.scope()
	if request.SessionID == "" {
		request.SessionID = c.identity.Session
	}
	var out types.StateHistoryResponse
	err := c.callMethod(ctx, methods.MethodStateHistory, request, &out)
	return out, err
}

// PauseList reads the unified pause snapshot.
func (c *client) PauseList(ctx context.Context, request types.PauseListRequest) (types.PauseListResponse, error) {
	request.Identity = c.scope()
	var out types.PauseListResponse
	err := c.callMethod(ctx, methods.MethodPauseList, request, &out)
	return out, err
}

// Control submits one of the nine canonical steering controls.
func (c *client) Control(ctx context.Context, method methods.Method, request types.ControlRequest) (types.ControlResponse, error) {
	if !methods.IsControlMethod(method) {
		return types.ControlResponse{}, fmt.Errorf("protocol client: %q is not a control method", method)
	}
	identity := c.scope()
	identity.Run = request.Identity.Run
	identity.Scope = request.Identity.Scope
	request.Identity = identity
	var out types.ControlResponse
	err := c.callMethod(ctx, method, request, &out)
	return out, err
}

// ArtifactsPut uploads bytes and returns their reference.
func (c *client) ArtifactsPut(ctx context.Context, request types.ArtifactsPutRequest) (types.ArtifactsPutResponse, error) {
	identity := c.scope()
	request.Scope.Tenant = identity.Tenant
	request.Scope.User = identity.User
	request.Scope.Session = identity.Session
	var out types.ArtifactsPutResponse
	err := c.callMethod(ctx, methods.MethodArtifactsPut, request, &out)
	return out, err
}

// ArtifactsList lists metadata for artifacts visible to this identity.
func (c *client) ArtifactsList(ctx context.Context, request types.ArtifactsListRequest) (types.ArtifactsListResponse, error) {
	identity := c.scope()
	request.Scope.Tenant = identity.Tenant
	request.Scope.User = identity.User
	request.Scope.Session = identity.Session
	var out types.ArtifactsListResponse
	err := c.callMethod(ctx, methods.MethodArtifactsList, request, &out)
	return out, err
}

func completeTriple(scope types.IdentityScope) bool {
	return strings.TrimSpace(scope.Tenant) != "" && strings.TrimSpace(scope.User) != "" && strings.TrimSpace(scope.Session) != ""
}

func validIdentity(scope types.IdentityScope) bool {
	if !completeTriple(scope) {
		return false
	}
	impersonating := scope.Actor != nil || scope.Requester != nil || scope.Impersonating != nil
	if !impersonating {
		return true
	}
	return scope.Actor != nil && scope.Requester != nil && scope.Impersonating != nil &&
		completeTriple(*scope.Actor) && completeTriple(*scope.Requester) && completeTriple(*scope.Impersonating) &&
		sameTriple(*scope.Actor, *scope.Requester) && sameTriple(scope, *scope.Impersonating)
}

func validConnectionIdentity(scope types.IdentityScope) bool {
	if scope.Tenant == "" && scope.User == "" && scope.Session == "" && scope.Actor == nil && scope.Requester == nil && scope.Impersonating == nil {
		return true
	}
	return validIdentity(scope)
}

func sameTriple(left, right types.IdentityScope) bool {
	return left.Tenant == right.Tenant && left.User == right.User && left.Session == right.Session
}

func cloneIdentity(scope types.IdentityScope) types.IdentityScope {
	return cloneIdentitySeen(&scope, make(map[*types.IdentityScope]*types.IdentityScope))
}

func cloneIdentitySeen(scope *types.IdentityScope, seen map[*types.IdentityScope]*types.IdentityScope) types.IdentityScope {
	if scope == nil {
		return types.IdentityScope{}
	}
	if clone, ok := seen[scope]; ok {
		return *clone
	}
	clone := &types.IdentityScope{
		Tenant: scope.Tenant, User: scope.User, Session: scope.Session, Run: scope.Run, Scope: scope.Scope,
	}
	seen[scope] = clone
	clone.Actor = cloneIdentityPointer(scope.Actor, seen)
	clone.Requester = cloneIdentityPointer(scope.Requester, seen)
	clone.Impersonating = cloneIdentityPointer(scope.Impersonating, seen)
	return *clone
}

func cloneIdentityPointer(scope *types.IdentityScope, seen map[*types.IdentityScope]*types.IdentityScope) *types.IdentityScope {
	if scope == nil {
		return nil
	}
	if clone, ok := seen[scope]; ok {
		return clone
	}
	clone := cloneIdentitySeen(scope, seen)
	return &clone
}
