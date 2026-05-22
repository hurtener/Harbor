package http

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	stdtemplate "text/template"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/hurtener/Harbor/internal/identity"
	"github.com/hurtener/Harbor/internal/tools"
)

// Sentinel errors for the inline / manifest registration paths and
// per-invocation failures. Callers compare via errors.Is.
var (
	// ErrTemplateRender — text/template execution failed (e.g.
	// missing variable referenced via {{ .Args.Foo }}, or a tag
	// the template engine couldn't parse). Wraps the underlying
	// error via %w.
	ErrTemplateRender = errors.New("http: URL/body template render failure")
	// ErrTemplateSecretLeak — a manifest / inline template attempted
	// to reference the .Auth namespace (the credential boundary).
	// Surfaced at registration time so the misconfiguration never
	// reaches a live invocation. AGENTS.md §7 — no credential
	// passthrough by default.
	ErrTemplateSecretLeak = errors.New("http: template attempted to interpolate auth/secret reference")
	// ErrUnsupportedMethod — RegisterHTTPTool got a method outside
	// the HTTP allowlist (POST / GET / PUT / DELETE / PATCH).
	ErrUnsupportedMethod = errors.New("http: HTTP method not supported")
	// ErrIdentityMissing — the per-invocation ctx had no identity
	// quadruple. Identity is mandatory (AGENTS.md §6).
	ErrIdentityMissing = errors.New("http: identity missing from ctx")
	// ErrHTTPStatus — non-2xx response classifier emitted this so
	// upstream callers can errors.Is against it; the wrapped detail
	// carries status + retry hint.
	ErrHTTPStatus = errors.New("http: response status")
)

// allowedMethods is the HTTP-verb allowlist. Phase 27 ships these
// five; OPTIONS / HEAD aren't tool-shaped operations.
var allowedMethods = map[string]struct{}{
	"GET":    {},
	"POST":   {},
	"PUT":    {},
	"DELETE": {},
	"PATCH":  {},
}

// secretLeakRegex matches template tokens that reference the .Auth
// namespace — the credential boundary. Loaders run this against
// every template (URL, body, header) to fail loudly at register
// time. The regex is intentionally permissive ({{ .Auth … }}) so a
// well-meaning misconfiguration ("oh, I'll just stick the API key
// in the URL") is caught before it lands.
var secretLeakRegex = regexp.MustCompile(`{{[\s-]*\.Auth\b`)

// HTTPOption configures an HTTP tool at registration. Mirrors the
// option-functional pattern from the inproc driver.
type HTTPOption func(*httpToolConfig)

// httpToolConfig accumulates option settings applied to the
// to-be-constructed descriptor. Internal — operators consume the
// With* helpers.
type httpToolConfig struct {
	client       *http.Client
	staticHeader map[string]string
	authSpec     AuthSpec
	source       tools.ToolSourceID
	secret       string
	bodyTmpl     string
	description  string
	sideEffect   tools.SideEffect
	loading      tools.LoadingMode
	argsSchema   []byte
	outSchema    []byte
	tags         []string
	authScopes   []string
	policy       tools.ToolPolicy
}

// defaultClient is the http.Client used when WithClient is not
// supplied. Sensible per-attempt timeouts; the policy shell's
// per-attempt context.WithTimeout supplies the actual ceiling.
var defaultClient = &http.Client{
	Timeout: 60 * time.Second,
}

// WithClient overrides the *http.Client used for this tool.
// Operators with bespoke transports (custom TLS roots, proxy,
// retryable client) wire them in here.
func WithClient(c *http.Client) HTTPOption {
	return func(cfg *httpToolConfig) { cfg.client = c }
}

// WithAuth attaches a static auth spec and the corresponding secret
// value. The secret is loaded from operator-supplied config (env
// variable, manifest auth_ref); it MUST NOT come from a request
// payload (D-NNN: no credential passthrough by default).
func WithAuth(spec AuthSpec, secret string) HTTPOption {
	return func(cfg *httpToolConfig) {
		cfg.authSpec = spec
		cfg.secret = secret
	}
}

// WithHeaders adds static headers applied AFTER auth so an operator
// can't accidentally overwrite the auth header with a static one.
func WithHeaders(h map[string]string) HTTPOption {
	return func(cfg *httpToolConfig) {
		if cfg.staticHeader == nil {
			cfg.staticHeader = make(map[string]string, len(h))
		}
		for k, v := range h {
			cfg.staticHeader[k] = v
		}
	}
}

// WithBodyTemplate sets a text/template for the request body. The
// rendered output is sent with `Content-Type: application/json`
// unless overridden by WithHeaders.
func WithBodyTemplate(tmpl string) HTTPOption {
	return func(cfg *httpToolConfig) { cfg.bodyTmpl = tmpl }
}

// WithHTTPSource overrides the descriptor's ToolSourceID. Defaults
// to "inline:<tool-name>" for the inline registration path or
// "manifest:<filename>" for the manifest path.
func WithHTTPSource(id tools.ToolSourceID) HTTPOption {
	return func(cfg *httpToolConfig) { cfg.source = id }
}

// WithArgsSchema attaches the input JSON schema (raw bytes). When
// absent, the descriptor's Validate is a permissive pass-through —
// the operator opts INTO validation explicitly.
func WithArgsSchema(schema []byte) HTTPOption {
	return func(cfg *httpToolConfig) { cfg.argsSchema = schema }
}

// WithOutSchema attaches the output JSON schema (raw bytes).
func WithOutSchema(schema []byte) HTTPOption {
	return func(cfg *httpToolConfig) { cfg.outSchema = schema }
}

// WithDescription overrides the Tool's planner-facing description.
func WithDescription(s string) HTTPOption {
	return func(cfg *httpToolConfig) { cfg.description = s }
}

// WithPolicy overrides the per-tool ToolPolicy.
func WithPolicy(p tools.ToolPolicy) HTTPOption {
	return func(cfg *httpToolConfig) { cfg.policy = p }
}

// WithTags adds operator-facing tags.
func WithTags(tags ...string) HTTPOption {
	return func(cfg *httpToolConfig) { cfg.tags = append(cfg.tags, tags...) }
}

// WithAuthScopes adds required catalog-visibility scopes.
func WithAuthScopes(scopes ...string) HTTPOption {
	return func(cfg *httpToolConfig) { cfg.authScopes = append(cfg.authScopes, scopes...) }
}

// WithSideEffect declares the tool's side-effect class.
func WithSideEffect(s tools.SideEffect) HTTPOption {
	return func(cfg *httpToolConfig) { cfg.sideEffect = s }
}

// WithLoading overrides LoadingMode (default: LoadingAlways).
func WithLoading(m tools.LoadingMode) HTTPOption {
	return func(cfg *httpToolConfig) { cfg.loading = m }
}

// RegisterHTTPTool registers a single HTTP tool inline. Returns the
// catalog's Register error (typically ErrToolDuplicateName) or any
// pre-registration validation error (unsupported method, secret
// leak in template, invalid auth spec).
//
// Identity-mandatory: the registered descriptor reads the (tenant,
// user, session) triple from ctx on every Invoke and fails with
// ErrIdentityMissing when absent. Phase 30 will extend this with
// tool-side OAuth tokens (per-identity); Phase 27 honours only the
// operator-configured static auth secret.
//
// Concurrent reuse (D-025): the produced descriptor is safe for N
// concurrent goroutines — per-invocation state (request, response,
// classifier output) lives on the stack; cached templates and
// compiled schema validators are read-only after construction.
func RegisterHTTPTool(
	cat tools.ToolCatalog,
	name, method, urlTemplate string,
	opts ...HTTPOption,
) error {
	if cat == nil {
		return fmt.Errorf("http.RegisterHTTPTool: catalog is nil")
	}
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("http.RegisterHTTPTool: name is empty")
	}
	if strings.TrimSpace(urlTemplate) == "" {
		return fmt.Errorf("http.RegisterHTTPTool: urlTemplate is empty for %q", name)
	}
	method = strings.ToUpper(strings.TrimSpace(method))
	if _, ok := allowedMethods[method]; !ok {
		return fmt.Errorf("%w: %q for tool %q (allowed: GET, POST, PUT, DELETE, PATCH)",
			ErrUnsupportedMethod, method, name)
	}

	cfg := httpToolConfig{
		client:     defaultClient,
		loading:    tools.LoadingAlways,
		sideEffect: tools.SideEffectExternal,
	}
	for _, opt := range opts {
		opt(&cfg)
	}

	// Validate the auth spec early so the misconfiguration is caught
	// at register time (boot), not at first invocation.
	if err := cfg.authSpec.Validate(); err != nil {
		return fmt.Errorf("http.RegisterHTTPTool[%s]: %w", name, err)
	}
	if cfg.authSpec.Kind != AuthKindNone && strings.TrimSpace(cfg.secret) == "" {
		return fmt.Errorf("http.RegisterHTTPTool[%s]: %w", name, ErrAuthMissing)
	}

	if err := checkNoSecretLeak("url_template", urlTemplate); err != nil {
		return fmt.Errorf("http.RegisterHTTPTool[%s]: %w", name, err)
	}
	if cfg.bodyTmpl != "" {
		if err := checkNoSecretLeak("body_template", cfg.bodyTmpl); err != nil {
			return fmt.Errorf("http.RegisterHTTPTool[%s]: %w", name, err)
		}
	}
	for k, v := range cfg.staticHeader {
		if err := checkNoSecretLeak("header["+k+"]", v); err != nil {
			return fmt.Errorf("http.RegisterHTTPTool[%s]: %w", name, err)
		}
	}

	urlTmpl, err := compileTemplate("url:"+name, urlTemplate)
	if err != nil {
		return fmt.Errorf("http.RegisterHTTPTool[%s]: %w: %w", name, ErrTemplateRender, err)
	}
	var bodyTmpl *stdtemplate.Template
	if cfg.bodyTmpl != "" {
		bodyTmpl, err = compileTemplate("body:"+name, cfg.bodyTmpl)
		if err != nil {
			return fmt.Errorf("http.RegisterHTTPTool[%s]: %w: %w", name, ErrTemplateRender, err)
		}
	}

	// Pre-compile the input + output schema validators once.
	var compiledIn, compiledOut *jsonschema.Schema
	if len(cfg.argsSchema) > 0 {
		compiledIn, err = compileSchema(cfg.argsSchema)
		if err != nil {
			return fmt.Errorf("http.RegisterHTTPTool[%s]: compile args_schema: %w", name, err)
		}
	}
	if len(cfg.outSchema) > 0 {
		compiledOut, err = compileSchema(cfg.outSchema)
		if err != nil {
			return fmt.Errorf("http.RegisterHTTPTool[%s]: compile out_schema: %w", name, err)
		}
	}

	source := cfg.source
	if source == "" {
		source = tools.ToolSourceID("inline:" + name)
	}

	t := tools.Tool{
		Name:        name,
		Description: chooseString(cfg.description, name),
		ArgsSchema:  cfg.argsSchema,
		OutSchema:   cfg.outSchema,
		SideEffects: chooseSideEffect(cfg.sideEffect),
		Tags:        cfg.tags,
		AuthScopes:  cfg.authScopes,
		Loading:     chooseLoading(cfg.loading),
		Source:      source,
		Transport:   tools.TransportHTTP,
		Policy:      cfg.policy,
	}

	desc := buildHTTPDescriptor(t, cfg, method, urlTmpl, bodyTmpl, compiledIn, compiledOut)
	return cat.Register(desc)
}

// buildHTTPDescriptor packages the compiled artifacts into a
// `tools.ToolDescriptor` with a single `Invoke` closure that runs
// through the policy shell exactly ONCE (D-024 no double-wrap).
//
// The descriptor is the unit of concurrent reuse: the closure
// captures only read-only state (cfg, method, compiled templates,
// compiled validators); per-invocation values (ctx, args, *http.Request,
// *http.Response, []byte body, classifier output) live on the
// goroutine stack.
func buildHTTPDescriptor(
	t tools.Tool,
	cfg httpToolConfig,
	method string,
	urlTmpl *stdtemplate.Template,
	bodyTmpl *stdtemplate.Template,
	compiledIn *jsonschema.Schema,
	compiledOut *jsonschema.Schema,
) tools.ToolDescriptor {
	validateIn := func(args json.RawMessage) error {
		if compiledIn == nil {
			return nil
		}
		return validateAgainst(compiledIn, args)
	}
	validateOut := func(res tools.ToolResult) error {
		if compiledOut == nil {
			return nil
		}
		return validateAgainstResult(compiledOut, res)
	}

	invoke := func(ctx context.Context, args json.RawMessage) (tools.ToolResult, error) {
		// Identity is mandatory. Drivers reject empty triples (AGENTS.md §6).
		if _, ok := identity.From(ctx); !ok {
			return tools.ToolResult{}, ErrIdentityMissing
		}
		return doHTTPInvoke(ctx, args, cfg, method, urlTmpl, bodyTmpl)
	}

	return tools.ToolDescriptor{
		Tool:     t,
		Validate: validateIn,
		Invoke: func(ctx context.Context, args json.RawMessage) (tools.ToolResult, error) {
			// RunWithPolicy is called exactly ONCE per Invoke. The
			// driver's invoke closure runs the unary HTTP request; the
			// shell handles retry / backoff / Retry-After honour.
			return tools.RunWithPolicy(ctx, args, invoke, validateIn, validateOut, t.Policy)
		},
	}
}

// doHTTPInvoke runs ONE HTTP attempt. Render templates, apply auth,
// fire the request, read the body, classify the response. Retry +
// Retry-After honouring happens at the policy shell layer.
func doHTTPInvoke(
	ctx context.Context,
	args json.RawMessage,
	cfg httpToolConfig,
	method string,
	urlTmpl *stdtemplate.Template,
	bodyTmpl *stdtemplate.Template,
) (tools.ToolResult, error) {
	// Decode args into a generic shape for templates ({{ .Args.City }}).
	// Templates see ONLY the args namespace (.Args.*); the .Auth
	// namespace is rejected at registration time, so there's no
	// secret to leak here.
	var argsAny any
	if len(args) > 0 && !bytes.Equal(bytes.TrimSpace(args), []byte("null")) {
		if err := json.Unmarshal(args, &argsAny); err != nil {
			return tools.ToolResult{}, fmt.Errorf("%w: decode args: %w", tools.ErrToolInvalidArgs, err)
		}
	}
	tmplData := map[string]any{"Args": argsAny}

	renderedURL, err := renderTemplate(urlTmpl, tmplData)
	if err != nil {
		return tools.ToolResult{}, fmt.Errorf("%w: %w", ErrTemplateRender, err)
	}

	// Sanity: URL must parse.
	parsedURL, err := url.Parse(renderedURL)
	if err != nil {
		return tools.ToolResult{}, fmt.Errorf("%w: url parse %q: %w", ErrTemplateRender, renderedURL, err)
	}

	var body io.Reader
	var bodyBytes []byte
	if bodyTmpl != nil {
		rendered, err := renderTemplate(bodyTmpl, tmplData) //nolint:govet // block-local; err shadow is benign
		if err != nil {
			return tools.ToolResult{}, fmt.Errorf("%w: body: %w", ErrTemplateRender, err)
		}
		bodyBytes = []byte(rendered)
		body = bytes.NewReader(bodyBytes)
	} else if method != "GET" && method != "DELETE" && len(args) > 0 {
		// No explicit body template — for POST/PUT/PATCH, forward the
		// raw args as the JSON body. This is the common case for
		// "send arguments as JSON to an endpoint."
		bodyBytes = []byte(args)
		body = bytes.NewReader(bodyBytes)
	}

	req, err := http.NewRequestWithContext(ctx, method, parsedURL.String(), body)
	if err != nil {
		return tools.ToolResult{}, fmt.Errorf("http: build request: %w", err)
	}

	// Apply auth FIRST so static headers can't accidentally overwrite it.
	if err = applyAuth(req, cfg.authSpec, cfg.secret); err != nil {
		return tools.ToolResult{}, err
	}

	// Default Content-Type for non-empty body (operator can override
	// via WithHeaders).
	if len(bodyBytes) > 0 && req.Header.Get("Content-Type") == "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range cfg.staticHeader {
		req.Header.Set(k, v)
	}

	client := cfg.client
	if client == nil {
		client = defaultClient
	}

	resp, err := client.Do(req)
	if err != nil {
		// Transport-level error — connection refused, TLS handshake,
		// etc. Classifier maps these to ErrClassTransient (the
		// ClassifyError default).
		return tools.ToolResult{}, fmt.Errorf("http: do request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return tools.ToolResult{}, fmt.Errorf("http: read body: %w", err)
	}

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		// 2xx — return the body bytes as the result. Higher layers
		// (ObservationRenderer, ArtifactStore enforcement) handle
		// heavy outputs.
		return buildSuccessResult(resp, respBytes)
	}

	// Non-2xx — classify and return a typed error so the policy
	// shell knows whether to retry.
	classified := classifyHTTPResponse(resp, respBytes)

	// Rate-limit `Retry-After` honour: the policy shell handles
	// exponential backoff between attempts, but the server may
	// require a specific minimum delay. We block here (ctx-aware)
	// until that delay has elapsed before returning, so the shell's
	// own backoff stacks on top. This keeps the "shell wraps the
	// invoke once" contract intact (no double-retry inside the
	// driver) while still honouring Retry-After.
	if delay, ok := IsRateLimited(classified); ok && delay > 0 {
		if err := sleepCtx(ctx, delay); err != nil {
			return tools.ToolResult{}, err
		}
	}
	return tools.ToolResult{}, classified
}

// sleepCtx blocks for d or until ctx is cancelled, whichever
// fires first. Never uses time.Sleep — the policy shell's
// timeout/cancellation contract requires ctx-aware blocking
// (AGENTS.md §11).
func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// buildSuccessResult packages a 2xx response into a ToolResult.
// The Value is the parsed JSON when Content-Type indicates JSON;
// otherwise the raw bytes (as a string) so the output validator
// has something to walk.
func buildSuccessResult(resp *http.Response, body []byte) (tools.ToolResult, error) {
	meta := map[string]any{
		"http.status":       resp.StatusCode,
		"http.content_type": resp.Header.Get("Content-Type"),
	}
	contentType := resp.Header.Get("Content-Type")
	if strings.HasPrefix(contentType, "application/json") || strings.HasPrefix(contentType, "application/vnd.api+json") {
		var v any
		if len(body) == 0 {
			return tools.ToolResult{Value: nil, Meta: meta}, nil
		}
		if err := json.Unmarshal(body, &v); err != nil {
			return tools.ToolResult{}, fmt.Errorf("http: decode response: %w", err)
		}
		return tools.ToolResult{Value: v, Meta: meta}, nil
	}
	return tools.ToolResult{Value: string(body), Meta: meta}, nil
}

// classifyHTTPResponse maps a non-2xx response to a Go error whose
// string carries the status code in a shape ClassifyError can map:
//
//   - 4xx (except 429): "status 4xx" → ErrClassPermanent (Phase 26
//     classifier maps unknown strings to Transient by default, so
//     4xx returns explicitly bubble ErrToolInvalidArgs / sentinel
//     to terminate retry).
//   - 429: returns errHTTPRateLimit (transient class) with the
//     parsed Retry-After value.
//   - 5xx (incl. 503): "status 5xx" string → ErrClass5xx; 503 also
//     parses Retry-After for the next sleep floor.
func classifyHTTPResponse(resp *http.Response, body []byte) error {
	status := resp.StatusCode
	snippet := bodySnippet(body)
	switch {
	case status == http.StatusTooManyRequests:
		delay := parseRetryAfter(resp.Header.Get("Retry-After"))
		return &rateLimitError{status: status, delay: delay, snippet: snippet}
	case status == http.StatusServiceUnavailable:
		delay := parseRetryAfter(resp.Header.Get("Retry-After"))
		return &rateLimitError{status: status, delay: delay, snippet: snippet}
	case status >= 400 && status < 500:
		// 4xx → permanent. All 4xx wrap ErrToolInvalidArgs so the
		// Phase 26 classifier maps them to ErrClassPermanent (no
		// retry) and the planner-facing event channel surfaces them
		// as `tool.invalid_args`. The wrapped detail names the
		// status so operators can distinguish 400 (planner can
		// reformulate) from 401 / 403 / 404 (operator must fix
		// config) via log/event inspection.
		return fmt.Errorf("%w: status %d: %s", tools.ErrToolInvalidArgs, status, snippet)
	case status >= 500:
		// 5xx → transient. The error string carries "status 5xx" so
		// ClassifyError maps it to ErrClass5xx.
		return fmt.Errorf("%w: status %d (5xx): %s", ErrHTTPStatus, status, snippet)
	default:
		// 3xx or other unhandled — treat as transient by default.
		return fmt.Errorf("%w: status %d: %s", ErrHTTPStatus, status, snippet)
	}
}

// rateLimitError carries the parsed Retry-After delay so the policy
// shell's classifier can honour it. Implements RateLimited so
// callers can errors.As against it.
type rateLimitError struct {
	snippet string
	status  int
	delay   time.Duration
}

// Error formats the status + parsed delay. The string deliberately
// does NOT match Phase 26 ClassifyError's "status 5xx" / " 500 "
// patterns; instead it falls through to the catch-all
// ErrClassTransient, which is retryable by default. The Retry-After
// delay is honoured by the driver itself before the error is
// returned (see doHTTPInvoke); the policy shell's exponential
// backoff stacks on top.
func (e *rateLimitError) Error() string {
	if e.delay > 0 {
		return fmt.Sprintf("http: rate-limit (status %d, retry-after %s): %s", e.status, e.delay, e.snippet)
	}
	return fmt.Sprintf("http: rate-limit (status %d): %s", e.status, e.snippet)
}

// RateLimitDelay returns the parsed Retry-After delay, or 0 when
// the server didn't supply a parsable value. Exported so the policy
// shell's caller can honour it as a sleep floor.
func (e *rateLimitError) RateLimitDelay() time.Duration { return e.delay }

// Status returns the HTTP status code that produced the rate-limit.
func (e *rateLimitError) Status() int { return e.status }

// RateLimitedError is implemented by error types that carry a parsed
// Retry-After delay. Callers can errors.As against this interface to
// honour the delay (e.g. as a sleep floor before retry).
type RateLimitedError interface {
	error
	RateLimitDelay() time.Duration
	Status() int
}

// IsRateLimited reports whether err is (or wraps) a RateLimitedError.
// Returns the delay alongside the boolean for ergonomic callers.
func IsRateLimited(err error) (time.Duration, bool) {
	var rle RateLimitedError
	if errors.As(err, &rle) {
		return rle.RateLimitDelay(), true
	}
	return 0, false
}

// parseRetryAfter parses the Retry-After header value. Accepts:
//
//   - seconds-integer ("60" → 60s)
//   - HTTP-date ("Sun, 06 Nov 1994 08:49:37 GMT")
//
// Returns 0 when neither form parses.
func parseRetryAfter(header string) time.Duration {
	header = strings.TrimSpace(header)
	if header == "" {
		return 0
	}
	if secs, err := strconv.Atoi(header); err == nil && secs >= 0 {
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(header); err == nil {
		d := time.Until(t)
		if d > 0 {
			return d
		}
	}
	return 0
}

// bodySnippet returns a bounded preview of the response body for
// inclusion in error messages. Bounded so audit redaction has
// less surface to cover.
func bodySnippet(body []byte) string {
	const maxLen = 256
	if len(body) == 0 {
		return ""
	}
	if len(body) <= maxLen {
		return strings.TrimSpace(string(body))
	}
	return strings.TrimSpace(string(body[:maxLen])) + "…"
}

// checkNoSecretLeak inspects a template source for {{ .Auth }}
// references — the credential boundary. Returns ErrTemplateSecretLeak
// when found so the misconfiguration is caught at load time.
func checkNoSecretLeak(label, tmplSrc string) error {
	if secretLeakRegex.MatchString(tmplSrc) {
		return fmt.Errorf("%w: in %s", ErrTemplateSecretLeak, label)
	}
	return nil
}

// compileTemplate compiles a text/template with strict missing-key
// handling so {{ .Args.NotPresent }} fails loudly rather than
// rendering as the empty string. Includes a `urlquery` function
// alias — operators reach for `{{ .Args.Foo | urlquery }}` to be
// explicit; the renderer also auto-escapes substituted values.
func compileTemplate(name, src string) (*stdtemplate.Template, error) {
	funcMap := stdtemplate.FuncMap{
		"urlquery": url.QueryEscape,
	}
	t, err := stdtemplate.New(name).Option("missingkey=error").Funcs(funcMap).Parse(src)
	if err != nil {
		return nil, err
	}
	return t, nil
}

// renderTemplate executes t against data. The caller passes the
// {Args: argsAny} map; the Auth namespace is intentionally absent
// (it's rejected at register time anyway).
func renderTemplate(t *stdtemplate.Template, data any) (string, error) {
	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// compileSchema compiles a JSON-Schema document into a reusable
// validator. Wraps the schema in a synthetic URL so the compiler
// resolves it stand-alone. Mirrors the inproc driver's helper.
func compileSchema(schemaBytes []byte) (*jsonschema.Schema, error) {
	c := jsonschema.NewCompiler()
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(schemaBytes))
	if err != nil {
		return nil, fmt.Errorf("unmarshal schema: %w", err)
	}
	const syntheticURL = "mem://tool/schema.json"
	if err := c.AddResource(syntheticURL, doc); err != nil {
		return nil, fmt.Errorf("add resource: %w", err)
	}
	return c.Compile(syntheticURL)
}

// validateAgainst decodes args into a JSON value and validates it
// against schema.
func validateAgainst(schema *jsonschema.Schema, args json.RawMessage) error {
	if schema == nil {
		return nil
	}
	if len(args) == 0 {
		args = json.RawMessage("null")
	}
	v, err := jsonschema.UnmarshalJSON(bytes.NewReader(args))
	if err != nil {
		return fmt.Errorf("decode args: %w", err)
	}
	return schema.Validate(v)
}

// validateAgainstResult marshals the result's Value into JSON and
// validates it against schema.
func validateAgainstResult(schema *jsonschema.Schema, result tools.ToolResult) error {
	if schema == nil {
		return nil
	}
	if result.Value == nil {
		return validateAgainst(schema, json.RawMessage("null"))
	}
	buf, err := json.Marshal(result.Value)
	if err != nil {
		return fmt.Errorf("marshal result: %w", err)
	}
	return validateAgainst(schema, buf)
}

// chooseString returns first when non-empty, else second.
func chooseString(first, second string) string {
	if first != "" {
		return first
	}
	return second
}

// chooseSideEffect normalises a zero-value SideEffect to the
// "external" default for HTTP tools.
func chooseSideEffect(s tools.SideEffect) tools.SideEffect {
	if s == "" {
		return tools.SideEffectExternal
	}
	return s
}

// chooseLoading normalises a zero-value LoadingMode to Always.
func chooseLoading(m tools.LoadingMode) tools.LoadingMode {
	if m == "" {
		return tools.LoadingAlways
	}
	return m
}
