package llm

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"strings"
	"time"
	"unicode"
)

// ErrProviderRouteResolverUnavailable is returned when a caller explicitly
// selects an external provider route but the runtime has no resolver wired.
// Runtime-default calls never consult this seam.
var ErrProviderRouteResolverUnavailable = errors.New("llm: external provider route resolver is unavailable")

// ErrProviderRouteResolutionFailed is the fixed content-free outcome for an
// external resolver failure. Arbitrary resolver error text is never wrapped:
// it may contain credentials or upstream response bodies.
var ErrProviderRouteResolutionFailed = errors.New("llm: external provider route resolution failed")

// ErrProviderRouteProviderFailed is the fixed content-free outcome for a
// downstream provider failure on an external route. Provider error text is
// never returned because it may contain endpoint, credential, or response
// body material.
var ErrProviderRouteProviderFailed = errors.New("llm: external provider route provider call failed")

// ErrProviderRouteInvalid identifies a malformed intent or a resolver result
// that does not exactly match its trusted request context.
var ErrProviderRouteInvalid = errors.New("llm: external provider route is invalid")

const (
	maxProviderRouteFieldBytes = 512
	maxProviderCredentialBytes = 64 << 10
	maxProviderRouteLifetime   = 5 * time.Minute
)

// ProviderRoute identifies one externally managed provider route. It carries
// opaque identifiers and generations only: provider credentials, endpoints,
// and caller-selected provider names are deliberately absent.
type ProviderRoute struct {
	RouteID                      string `json:"route_id"`
	RouteGeneration              uint64 `json:"route_generation"`
	ProviderConnectionID         string `json:"provider_connection_id"`
	ProviderConnectionGeneration uint64 `json:"provider_connection_generation"`
	CredentialAssetGeneration    uint64 `json:"credential_asset_generation"`
	ModelSelector                string `json:"model_selector"`
}

// ProviderRoutePurpose binds a resolver request to its trusted runtime use.
// Run is the admitted Bifrost execution path; Posture is the admin-only
// provider validation/discovery path. Callers cannot select this from wire
// input because the runtime derives it from the trusted route context.
type ProviderRoutePurpose string

const (
	ProviderRoutePurposeRun     ProviderRoutePurpose = "run"
	ProviderRoutePurposePosture ProviderRoutePurpose = "posture"
)

// ProviderRouteRequest is assembled by the runtime after identity and
// effective-Agent admission. None of its authority-bearing fields come from
// an unverified request body.
type ProviderRouteRequest struct {
	TenantID                     string
	UserID                       string
	SessionID                    string
	LogicalRunID                 string
	EffectiveAgentID             string
	RuntimeID                    string
	TaskID                       string
	LogicalCallID                string
	RouteID                      string
	RouteGeneration              uint64
	ProviderConnectionID         string
	ProviderConnectionGeneration uint64
	CredentialAssetGeneration    uint64
	ModelSelector                string
	Purpose                      ProviderRoutePurpose
}

// ProviderEndpointKind identifies one supported typed endpoint projection.
// The empty kind means the provider's standard endpoint. No generic endpoint
// or cloud-credential bundle is accepted.
type ProviderEndpointKind string

const (
	ProviderEndpointAzure            ProviderEndpointKind = "azure"
	ProviderEndpointVLLM             ProviderEndpointKind = "vllm"
	ProviderEndpointOllama           ProviderEndpointKind = "ollama"
	ProviderEndpointSGL              ProviderEndpointKind = "sgl"
	ProviderEndpointOpenAICompatible ProviderEndpointKind = "openai_compatible"
)

// ProviderEndpointBinding is returned only by the trusted resolver. Value is
// the normalized endpoint and Digest is its lowercase SHA-256. Value must
// never be copied into Protocol, task state, events, receipts, or logs.
type ProviderEndpointBinding struct {
	Kind   ProviderEndpointKind
	Value  string `json:"-"`
	Digest string
}

func (e ProviderEndpointBinding) String() string {
	return fmt.Sprintf("provider_endpoint{kind=%q digest=%q}", e.Kind, e.Digest)
}

func (e ProviderEndpointBinding) GoString() string { return e.String() }

func (e ProviderEndpointBinding) LogValue() slog.Value {
	return slog.GroupValue(slog.String("kind", string(e.Kind)), slog.String("digest", e.Digest))
}

// SelectedProviderRoute is the credential-free provider/model decision used
// by the outer policy chain. The leaf driver independently resolves the
// credential for every actual attempt and exact-confirms this selection.
type SelectedProviderRoute struct {
	Provider                     string
	Model                        string
	KeyName                      string
	RouteID                      string
	RouteGeneration              uint64
	ProviderConnectionID         string
	ProviderConnectionGeneration uint64
	CredentialAssetGeneration    uint64
	ModelSelector                string
	Endpoint                     *ProviderEndpointBinding
	ExpiresAt                    time.Time
}

func (r SelectedProviderRoute) String() string {
	return fmt.Sprintf("selected_provider_route{provider=%q model=%q key_name=%q route_id=%q route_generation=%d provider_connection_id=%q provider_connection_generation=%d credential_asset_generation=%d model_selector=%q endpoint_kind=%q endpoint_digest=%q expires_at=%s}",
		r.Provider, r.Model, r.KeyName, r.RouteID, r.RouteGeneration, r.ProviderConnectionID,
		r.ProviderConnectionGeneration, r.CredentialAssetGeneration, r.ModelSelector,
		endpointKind(r.Endpoint), endpointDigest(r.Endpoint), r.ExpiresAt.UTC().Format(time.RFC3339Nano))
}

func (r SelectedProviderRoute) GoString() string { return r.String() }

func (r SelectedProviderRoute) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("provider", r.Provider), slog.String("model", r.Model), slog.String("key_name", r.KeyName),
		slog.String("route_id", r.RouteID), slog.Uint64("route_generation", r.RouteGeneration),
		slog.String("provider_connection_id", r.ProviderConnectionID),
		slog.Uint64("provider_connection_generation", r.ProviderConnectionGeneration),
		slog.Uint64("credential_asset_generation", r.CredentialAssetGeneration),
		slog.String("model_selector", r.ModelSelector), slog.String("endpoint_kind", endpointKind(r.Endpoint)),
		slog.String("endpoint_digest", endpointDigest(r.Endpoint)), slog.Time("expires_at", r.ExpiresAt),
	)
}

// ResolvedProviderRoute is a short-lived, exact-bound provider selection.
// Credential is intentionally excluded from JSON; it may exist only on the
// in-process Bifrost request path.
type ResolvedProviderRoute struct {
	Provider                     string                   `json:"provider"`
	Model                        string                   `json:"model"`
	KeyName                      string                   `json:"key_name"`
	RouteID                      string                   `json:"route_id"`
	RouteGeneration              uint64                   `json:"route_generation"`
	ProviderConnectionID         string                   `json:"provider_connection_id"`
	ProviderConnectionGeneration uint64                   `json:"provider_connection_generation"`
	CredentialAssetGeneration    uint64                   `json:"credential_asset_generation"`
	ModelSelector                string                   `json:"model_selector"`
	Endpoint                     *ProviderEndpointBinding `json:"-"`
	ExpiresAt                    time.Time                `json:"expires_at"`
	Credential                   string                   `json:"-"`
}

// LogValue deliberately omits credential material when a resolved route is
// passed to slog, including through slog.Any.
func (r ResolvedProviderRoute) LogValue() slog.Value {
	return slog.GroupValue(
		slog.String("provider", r.Provider),
		slog.String("model", r.Model),
		slog.String("key_name", r.KeyName),
		slog.String("route_id", r.RouteID),
		slog.Uint64("route_generation", r.RouteGeneration),
		slog.String("provider_connection_id", r.ProviderConnectionID),
		slog.Uint64("provider_connection_generation", r.ProviderConnectionGeneration),
		slog.Uint64("credential_asset_generation", r.CredentialAssetGeneration),
		slog.String("model_selector", r.ModelSelector),
		slog.String("endpoint_kind", endpointKind(r.Endpoint)),
		slog.String("endpoint_digest", endpointDigest(r.Endpoint)),
		slog.Time("expires_at", r.ExpiresAt),
	)
}

// String returns a content-free summary so ordinary formatted diagnostics do
// not expose the short-lived credential.
func (r ResolvedProviderRoute) String() string {
	return fmt.Sprintf("provider_route{provider=%q model=%q key_name=%q route_id=%q route_generation=%d provider_connection_id=%q provider_connection_generation=%d credential_asset_generation=%d model_selector=%q endpoint_kind=%q endpoint_digest=%q expires_at=%s}",
		r.Provider, r.Model, r.KeyName, r.RouteID, r.RouteGeneration, r.ProviderConnectionID,
		r.ProviderConnectionGeneration, r.CredentialAssetGeneration, r.ModelSelector,
		endpointKind(r.Endpoint), endpointDigest(r.Endpoint),
		r.ExpiresAt.UTC().Format(time.RFC3339Nano))
}

// GoString applies the same secret-free posture to %#v diagnostics.
func (r ResolvedProviderRoute) GoString() string { return r.String() }

// ProviderRouteResolver resolves one explicitly selected external route. It
// is optional; a nil resolver preserves the runtime-default path with zero
// work. Implementations must be safe for concurrent reuse and honor ctx.
type ProviderRouteResolver interface {
	// SelectProviderRoute returns only the provider/model decision. It must
	// never return or retain credential material.
	SelectProviderRoute(context.Context, ProviderRouteRequest) (SelectedProviderRoute, error)
	// ResolveProviderRoute resolves the short-lived credential at the leaf for
	// one actual provider attempt.
	ResolveProviderRoute(context.Context, ProviderRouteRequest) (ResolvedProviderRoute, error)
}

// ProviderRouteSelectionValidator is implemented by a route-capable driver so
// the credential-free selection can be rejected before governance and every
// model-sensitive wrapper. It is not a public driver registry.
type ProviderRouteSelectionValidator interface {
	ValidateProviderRouteSelection(SelectedProviderRoute) error
}

// ProviderRouteConfig carries the optional resolver seam and the runtime ID
// stamped into trusted requests. A zero value is fully latent.
type ProviderRouteConfig struct {
	Resolver  ProviderRouteResolver
	RuntimeID string
}

// ValidateProviderRouteConfig validates an injected resolver configuration.
// The zero value is the disabled, runtime-default-only posture.
func ValidateProviderRouteConfig(cfg ProviderRouteConfig) error {
	configured := cfg.Resolver != nil || strings.TrimSpace(cfg.RuntimeID) != ""
	if !configured {
		return nil
	}
	if cfg.Resolver == nil || strings.TrimSpace(cfg.RuntimeID) == "" {
		return fmt.Errorf("%w: configured resolver and runtime id are both required", ErrProviderRouteInvalid)
	}
	return nil
}

type providerRouteContextKey struct{}

// TrustedProviderRouteContext is installed by the runtime only after normal
// identity and effective-Agent admission. Protocol callers cannot construct
// this context through wire fields alone.
type TrustedProviderRouteContext struct {
	Route            ProviderRoute
	EffectiveAgentID string
	RuntimeID        string
	TaskID           string
	Purpose          ProviderRoutePurpose
}

// WithTrustedProviderRoute installs a server-derived route context.
func WithTrustedProviderRoute(ctx context.Context, trusted TrustedProviderRouteContext) context.Context {
	return context.WithValue(ctx, providerRouteContextKey{}, trusted)
}

// TrustedProviderRouteFrom returns the server-derived route context.
func TrustedProviderRouteFrom(ctx context.Context) (TrustedProviderRouteContext, bool) {
	trusted, ok := ctx.Value(providerRouteContextKey{}).(TrustedProviderRouteContext)
	return trusted, ok
}

// ValidateProviderRoute validates the opaque selector without interpreting
// any identifier. Empty means runtime-default and is valid.
func ValidateProviderRoute(route ProviderRoute) error {
	empty := route.RouteID == "" && route.RouteGeneration == 0 && route.ProviderConnectionID == "" &&
		route.ProviderConnectionGeneration == 0 && route.CredentialAssetGeneration == 0 && route.ModelSelector == ""
	if empty {
		return nil
	}
	if strings.TrimSpace(route.RouteID) == "" || route.RouteGeneration == 0 ||
		strings.TrimSpace(route.ProviderConnectionID) == "" || route.ProviderConnectionGeneration == 0 ||
		route.CredentialAssetGeneration == 0 || strings.TrimSpace(route.ModelSelector) == "" {
		return fmt.Errorf("%w: explicit route requires opaque IDs, generations, and model selector", ErrProviderRouteInvalid)
	}
	if len(route.RouteID) > maxProviderRouteFieldBytes || len(route.ProviderConnectionID) > maxProviderRouteFieldBytes ||
		len(route.ModelSelector) > maxProviderRouteFieldBytes {
		return fmt.Errorf("%w: opaque route field exceeds byte bound", ErrProviderRouteInvalid)
	}
	return nil
}

// SelectProviderRoute obtains the credential-free route decision used before
// governance, retry, correction, and context-window enforcement.
func SelectProviderRoute(ctx context.Context, cfg ProviderRouteConfig, req ProviderRouteRequest, now time.Time) (SelectedProviderRoute, error) {
	if cfg.Resolver == nil {
		return SelectedProviderRoute{}, ErrProviderRouteResolverUnavailable
	}
	if err := ctx.Err(); err != nil {
		return SelectedProviderRoute{}, err
	}
	selected, err := cfg.Resolver.SelectProviderRoute(ctx, req)
	if err != nil {
		return SelectedProviderRoute{}, providerRouteResolverError(ctx, err)
	}
	if !validProviderRouteSelection(selected.Provider, selected.Model, selected.KeyName, selected.RouteID, selected.RouteGeneration,
		selected.ProviderConnectionID, selected.ProviderConnectionGeneration, selected.CredentialAssetGeneration,
		selected.ModelSelector, selected.Endpoint, selected.ExpiresAt, req, now) {
		return SelectedProviderRoute{}, ErrProviderRouteInvalid
	}
	return selected, nil
}

// ResolveProviderRoute resolves and exact-validates one trusted explicit
// route. It performs no caching or background work, so rotation, revocation,
// and expiry are observed on the next provider attempt.
func ResolveProviderRoute(ctx context.Context, cfg ProviderRouteConfig, req ProviderRouteRequest, now time.Time) (ResolvedProviderRoute, error) {
	if cfg.Resolver == nil {
		return ResolvedProviderRoute{}, ErrProviderRouteResolverUnavailable
	}
	if err := ctx.Err(); err != nil {
		return ResolvedProviderRoute{}, err
	}
	resolved, err := cfg.Resolver.ResolveProviderRoute(ctx, req)
	if err != nil {
		return ResolvedProviderRoute{}, providerRouteResolverError(ctx, err)
	}
	if resolved.Provider == "" || len(resolved.Provider) > maxProviderRouteFieldBytes ||
		resolved.Model == "" || len(resolved.Model) > maxProviderRouteFieldBytes ||
		strings.TrimSpace(resolved.KeyName) == "" || len(resolved.KeyName) > maxProviderRouteFieldBytes ||
		resolved.Credential == "" || len(resolved.Credential) > maxProviderCredentialBytes ||
		!resolved.ExpiresAt.After(now) || resolved.ExpiresAt.After(now.Add(maxProviderRouteLifetime)) ||
		resolved.RouteID != req.RouteID || resolved.RouteGeneration != req.RouteGeneration ||
		resolved.ProviderConnectionID != req.ProviderConnectionID ||
		resolved.ProviderConnectionGeneration != req.ProviderConnectionGeneration ||
		resolved.CredentialAssetGeneration != req.CredentialAssetGeneration ||
		resolved.ModelSelector != req.ModelSelector || validateProviderEndpoint(resolved.Provider, resolved.Endpoint) != nil {
		return ResolvedProviderRoute{}, ErrProviderRouteInvalid
	}
	return resolved, nil
}

func validProviderRouteSelection(provider, model, keyName, routeID string, routeGeneration uint64,
	providerConnectionID string, providerConnectionGeneration, credentialAssetGeneration uint64,
	modelSelector string, endpoint *ProviderEndpointBinding, expiresAt time.Time, req ProviderRouteRequest, now time.Time,
) bool {
	return provider != "" && len(provider) <= maxProviderRouteFieldBytes &&
		model != "" && len(model) <= maxProviderRouteFieldBytes &&
		strings.TrimSpace(keyName) != "" && len(keyName) <= maxProviderRouteFieldBytes &&
		expiresAt.After(now) && !expiresAt.After(now.Add(maxProviderRouteLifetime)) &&
		routeID == req.RouteID && routeGeneration == req.RouteGeneration &&
		providerConnectionID == req.ProviderConnectionID &&
		providerConnectionGeneration == req.ProviderConnectionGeneration &&
		credentialAssetGeneration == req.CredentialAssetGeneration && modelSelector == req.ModelSelector &&
		validateProviderEndpoint(provider, endpoint) == nil
}

func providerRouteResolverError(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return ErrProviderRouteResolutionFailed
}

type selectedProviderRouteContextKey struct{}

// WithSelectedProviderRoute confines a credential-free, exact-bound route
// selection to one logical LLM call.
func WithSelectedProviderRoute(ctx context.Context, route SelectedProviderRoute) context.Context {
	return context.WithValue(ctx, selectedProviderRouteContextKey{}, route)
}

// SelectedProviderRouteFrom returns the credential-free pre-policy selection.
func SelectedProviderRouteFrom(ctx context.Context) (SelectedProviderRoute, bool) {
	route, ok := ctx.Value(selectedProviderRouteContextKey{}).(SelectedProviderRoute)
	return route, ok
}

// ProviderRouteResolutionMatchesSelection reports whether one attempt-time
// credential resolution exactly matches the credential-free pre-policy
// decision. Credentials and expiries are deliberately excluded.
func ProviderRouteResolutionMatchesSelection(resolved ResolvedProviderRoute, selected SelectedProviderRoute) bool {
	return resolved.Provider == selected.Provider && resolved.Model == selected.Model &&
		resolved.KeyName == selected.KeyName && providerEndpointsEqual(resolved.Endpoint, selected.Endpoint) &&
		resolved.RouteID == selected.RouteID && resolved.RouteGeneration == selected.RouteGeneration &&
		resolved.ProviderConnectionID == selected.ProviderConnectionID &&
		resolved.ProviderConnectionGeneration == selected.ProviderConnectionGeneration &&
		resolved.CredentialAssetGeneration == selected.CredentialAssetGeneration &&
		resolved.ModelSelector == selected.ModelSelector
}

// NormalizeProviderEndpoint validates and canonicalizes one resolver-selected
// endpoint. Remote endpoints require HTTPS; loopback HTTP is retained for
// deliberate local development and tests. Userinfo, query, and fragment are
// always refused.
func NormalizeProviderEndpoint(raw string) (string, string, error) {
	if strings.IndexFunc(raw, unicode.IsControl) >= 0 {
		return "", "", ErrProviderRouteInvalid
	}
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", "", ErrProviderRouteInvalid
	}
	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = strings.ToLower(u.Host)
	if u.Scheme != "https" && (u.Scheme != "http" || !providerRouteLoopback(u.Hostname())) {
		return "", "", ErrProviderRouteInvalid
	}
	u.Path = strings.TrimRight(u.EscapedPath(), "/")
	if u.Path == "" {
		u.Path = ""
	}
	u.RawPath = ""
	normalized := strings.TrimRight(u.String(), "/")
	digest := fmt.Sprintf("%x", sha256.Sum256([]byte(normalized)))
	return normalized, digest, nil
}

func validateProviderEndpoint(provider string, endpoint *ProviderEndpointBinding) error {
	if endpoint == nil {
		return nil
	}
	normalized, digest, err := NormalizeProviderEndpoint(endpoint.Value)
	if err != nil || normalized != endpoint.Value || digest != endpoint.Digest {
		return ErrProviderRouteInvalid
	}
	switch endpoint.Kind {
	case ProviderEndpointAzure:
		if provider != "azure" {
			return ErrProviderRouteInvalid
		}
	case ProviderEndpointVLLM:
		if provider != "vllm" {
			return ErrProviderRouteInvalid
		}
	case ProviderEndpointOllama:
		if provider != "ollama" {
			return ErrProviderRouteInvalid
		}
	case ProviderEndpointSGL:
		if provider != "sgl" {
			return ErrProviderRouteInvalid
		}
	case ProviderEndpointOpenAICompatible:
		if provider != "openai" {
			return ErrProviderRouteInvalid
		}
	default:
		return ErrProviderRouteInvalid
	}
	return nil
}

func providerEndpointsEqual(a, b *ProviderEndpointBinding) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func endpointKind(endpoint *ProviderEndpointBinding) string {
	if endpoint == nil {
		return ""
	}
	return string(endpoint.Kind)
}

func endpointDigest(endpoint *ProviderEndpointBinding) string {
	if endpoint == nil {
		return ""
	}
	return endpoint.Digest
}

func providerRouteLoopback(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

type resolvedProviderRouteContextKey struct{}

// WithResolvedProviderRoute confines a validated secret to one provider
// attempt context. It must never be copied into events or Protocol results.
func WithResolvedProviderRoute(ctx context.Context, route ResolvedProviderRoute) context.Context {
	return context.WithValue(ctx, resolvedProviderRouteContextKey{}, route)
}

// ResolvedProviderRouteFrom reads one per-attempt resolved route.
func ResolvedProviderRouteFrom(ctx context.Context) (ResolvedProviderRoute, bool) {
	route, ok := ctx.Value(resolvedProviderRouteContextKey{}).(ResolvedProviderRoute)
	return route, ok
}
