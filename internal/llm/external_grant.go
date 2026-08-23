package llm

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/hurtener/Harbor/internal/identity"
)

// ExternalGrant is the signed, content-free authorization envelope a
// coordinator may attach to one provider attempt. The grant contains no
// credential bytes. Its claims bind the attempt to the verified runtime
// identity, the logical run, the selected route, and an immutable credential
// asset generation.
//
// The signature is encoded by the signing implementation as base64url. The
// llm package deliberately does not choose a wire or key-management format;
// internal/llm/grant supplies the Harbor reference signer and verifier.
type ExternalGrant struct {
	Version                      int             `json:"version"`
	KeyID                        string          `json:"key_id"`
	Audience                     string          `json:"audience"`
	GrantID                      string          `json:"grant_id"`
	OrganizationID               string          `json:"organization_id"`
	RuntimeID                    string          `json:"runtime_id"`
	TenantID                     string          `json:"tenant_id"`
	UserID                       string          `json:"user_id"`
	SessionID                    string          `json:"session_id"`
	LogicalRunID                 string          `json:"logical_run_id"`
	LogicalCallID                string          `json:"logical_call_id"`
	AttemptNonce                 string          `json:"attempt_nonce"`
	Provider                     string          `json:"provider"`
	ProviderModelID              string          `json:"provider_model_id"`
	ProviderConnectionID         string          `json:"provider_connection_id"`
	ProviderConnectionGeneration uint64          `json:"provider_connection_generation"`
	RouteID                      string          `json:"route_id"`
	CredentialBindingHandle      string          `json:"credential_binding_handle"`
	CredentialAssetGeneration    uint64          `json:"credential_asset_generation"`
	PolicyGeneration             uint64          `json:"policy_generation"`
	MaxReasoning                 ReasoningEffort `json:"max_reasoning"`
	MaxOutputTokens              int             `json:"max_output_tokens"`
	Lease                        ComputeLease    `json:"lease"`
	IssuedAt                     time.Time       `json:"issued_at"`
	ExpiresAt                    time.Time       `json:"expires_at"`
	Signature                    string          `json:"signature"`
}

// ComputeLease is the bounded local allowance a runtime may consume before
// asking the coordinator for a top-up. Units are provider-neutral token units;
// cost accounting remains in the content-free receipt.
type ComputeLease struct {
	LeaseID string `json:"lease_id"`
	// Epoch changes whenever a coordinator issues a top-up.  A durable
	// reservation store uses it to prevent an old grant from consuming the
	// newer lease generation.
	Epoch         uint64    `json:"epoch"`
	TokenUnits    int64     `json:"token_units"`
	ConsumedUnits int64     `json:"consumed_units"`
	ExpiresAt     time.Time `json:"expires_at"`
}

// RemainingTokens returns the still-available bounded lease units.
func (l ComputeLease) RemainingTokens() int64 {
	remaining := l.TokenUnits - l.ConsumedUnits
	if remaining < 0 {
		return 0
	}
	return remaining
}

// ExternalGrantMode selects how the LLM edge handles grants.
type ExternalGrantMode string

const (
	// ExternalGrantDisabled preserves the pre-HA-70 behavior.
	ExternalGrantDisabled ExternalGrantMode = "disabled"
	// ExternalGrantOptional verifies a supplied grant, but permits legacy calls
	// that carry no grant. It is the migration mode for mixed fleets.
	ExternalGrantOptional ExternalGrantMode = "optional"
	// ExternalGrantRequired refuses every call without a valid grant.
	ExternalGrantRequired ExternalGrantMode = "required"
)

var (
	// ErrExternalGrantRequired indicates that strict mode received no grant.
	ErrExternalGrantRequired = errors.New("llm: external execution grant required")
	// ErrExternalGrantInvalid indicates a malformed, expired, or mismatched
	// grant. The wrapped detail is intentionally content-free.
	ErrExternalGrantInvalid = errors.New("llm: external execution grant invalid")
	// ErrExternalGrantSignature indicates that the grant was not signed by a
	// configured coordinator key.
	ErrExternalGrantSignature = errors.New("llm: external execution grant signature invalid")
	// ErrExternalGrantRevoked indicates a credential asset generation that is
	// no longer current.
	ErrExternalGrantRevoked = errors.New("llm: external execution grant credential binding revoked or stale")
	// ErrExternalGrantLeaseInsufficient indicates that a bounded lease needs a
	// coordinator top-up before this provider attempt can start.
	ErrExternalGrantLeaseInsufficient = errors.New("llm: external execution grant lease insufficient")
	// ErrExternalGrantAttemptInFlight means a duplicate invocation reached the
	// durable reservation after another process already admitted the exact
	// logical provider attempt. The duplicate must not call the provider.
	ErrExternalGrantAttemptInFlight = errors.New("llm: external execution attempt already in flight")
	// ErrExternalGrantAttemptSettled means a response-loss retry reached an
	// attempt whose provider outcome was already durably settled. Callers may
	// reconcile the persisted receipt; they must not invoke the provider again.
	ErrExternalGrantAttemptSettled = errors.New("llm: external execution attempt already settled")
	// ErrExternalGrantCrossProviderFallback is returned before a provider call
	// when an external grant would cross its signed provider boundary. A
	// coordinator must issue a new grant for a different provider.
	ErrExternalGrantCrossProviderFallback = errors.New("llm: external execution grant forbids cross-provider fallback")
	// ErrUsageReceiptUnavailable indicates that a required content-free receipt
	// could not be durably enqueued. Strict callers fail closed rather than
	// pretending that provider consumption was accounted.
	ErrUsageReceiptUnavailable = errors.New("llm: external execution usage receipt unavailable")
)

// ExternalGrantVerifier validates a signed grant against the verified request
// context and the actual model request. It must never trust caller-provided
// identity or runtime authority.
type ExternalGrantVerifier interface {
	Verify(context.Context, ExternalGrant, CompleteRequest) error
}

// CredentialResolver resolves one opaque credential binding only after the
// grant wrapper has verified the signature and context. Implementations must
// return a secret only to the provider driver; they must not expose it in
// grant claims, logs, receipts, or caller-facing errors.
type CredentialResolver interface {
	Resolve(context.Context, ExternalGrant) (ResolvedCredential, error)
}

// ResolvedCredential is the short-lived provider secret returned to the
// driver boundary. It is never serialized by Harbor's grant or receipt code.
type ResolvedCredential struct {
	Provider                     string
	CredentialBindingHandle      string
	CredentialAssetGeneration    uint64
	ProviderConnectionGeneration uint64
	Secret                       string
}

// LeaseTopUpper can replace a verified grant with a newly signed bounded
// grant when the current lease is insufficient. The implementation owns the
// coordinator exchange; the runtime never invents authority or extends a
// lease locally.
type LeaseTopUpper interface {
	TopUp(context.Context, ExternalGrant, int64) (ExternalGrant, error)
}

// LeaseReservationRequest is the durable per-attempt reservation request.
// Implementations must serialize competing requests for the same LeaseID in
// the StateStore, not in process-local memory.
type LeaseReservationRequest struct {
	AttemptID     string
	LogicalCallID string
	AttemptNonce  string
	GrantID       string
	LeaseID       string
	Epoch         uint64
	Capacity      int64
	Units         int64
	ExpiresAt     time.Time
	Identity      identity.Quadruple
}

// LeaseReservation is the durable reservation identity returned before the
// provider call starts.
type LeaseReservation struct {
	AttemptID     string
	LogicalCallID string
	AttemptNonce  string
	GrantID       string
	LeaseID       string
	Epoch         uint64
	Units         int64
	ExpiresAt     time.Time
	Status        string
	Existing      bool
	Receipt       AttemptUsageReceipt
}

// LeaseSettlement closes a reservation with the content-free attempt receipt.
type LeaseSettlement struct {
	AttemptID     string
	LogicalCallID string
	AttemptNonce  string
	Receipt       AttemptUsageReceipt
	Units         int64
	Now           time.Time
}

// LeaseReservationManager is the optional execution-edge reservation seam.
// Strict deployments should wire it; a nil manager preserves compatibility
// for embedded callers that have not enabled durable global allowances yet.
type LeaseReservationManager interface {
	Reserve(context.Context, LeaseReservationRequest) (LeaseReservation, error)
	Settle(context.Context, LeaseSettlement) error
}

// AttemptUsageReceipt is the content-free provider-attempt fact emitted by
// the grant wrapper. It intentionally contains no messages, prompts,
// responses, tool arguments, or reasoning traces.
type AttemptUsageReceipt struct {
	ReceiptID                    string
	GrantID                      string
	LogicalCallID                string
	AttemptNonce                 string
	OrganizationID               string
	RuntimeID                    string
	TenantID                     string
	UserID                       string
	SessionID                    string
	LogicalRunID                 string
	Provider                     string
	ProviderModelID              string
	ProviderConnectionID         string
	ProviderConnectionGeneration uint64
	RouteID                      string
	CredentialAssetGeneration    uint64
	PolicyGeneration             uint64
	AttemptNumber                int
	RetryNumber                  int
	FallbackHop                  int
	RequestedReasoning           ReasoningEffort
	EffectiveReasoning           ReasoningEffort
	PromptTokens                 int
	CompletionTokens             int
	ReasoningTokens              int
	TotalTokens                  int
	CacheReadTokens              int
	CacheWriteTokens             int
	InputCostMicros              int64
	OutputCostMicros             int64
	ReasoningCostMicros          int64
	TotalCostMicros              int64
	Currency                     string
	LatencyMS                    int64
	Status                       string
	StartedAt                    time.Time
	CompletedAt                  time.Time
	IdempotencyKey               string
	CanonicalBodyHash            string
}

// UsageReceiptSink durably accepts a receipt before a strict provider call is
// considered fully observed. Implementations are responsible for idempotent
// receipt identity and replay semantics.
type UsageReceiptSink interface {
	Enqueue(context.Context, AttemptUsageReceipt) error
}

// CanonicalAttemptUsageReceiptBodyHash returns the SHA-256 digest of the
// receipt body with its digest field blank. encoding/json preserves the
// declared field order, giving the outbox and downstream consumers one
// deterministic content-free idempotency representation without adding a
// second serialization format.
func CanonicalAttemptUsageReceiptBodyHash(receipt AttemptUsageReceipt) (string, error) {
	receipt.CanonicalBodyHash = ""
	body, err := json.Marshal(receipt)
	if err != nil {
		return "", fmt.Errorf("llm: canonical usage receipt: %w", err)
	}
	digest := sha256.Sum256(body)
	return hex.EncodeToString(digest[:]), nil
}

// ExternalGrantConfig wires the opt-in grant and receipt seams into llm.Open.
// A zero value disables the layer and preserves legacy behavior.
type ExternalGrantConfig struct {
	Mode            ExternalGrantMode
	Verifier        ExternalGrantVerifier
	Credentials     CredentialResolver
	TopUpper        LeaseTopUpper
	ReceiptSink     UsageReceiptSink
	ReceiptRequired bool
	RuntimeID       string
	Reservations    LeaseReservationManager
}

// VerifiedGrantContext is installed only after ExternalGrantVerifier succeeds.
// Provider drivers use it to resolve the opaque credential binding without
// accepting a caller-selected key or a process-global mutable secret.
type VerifiedGrantContext struct {
	Grant       ExternalGrant
	Credentials CredentialResolver
}

type verifiedOrganizationKey struct{}

// WithVerifiedOrganization attaches the request-edge organization authority
// established by the coordinator. Like identity.WithVerified, this helper is
// intended for the authentication/dispatch boundary; provider callers must
// not derive or overwrite it from a request body.
func WithVerifiedOrganization(ctx context.Context, organizationID string) context.Context {
	return context.WithValue(ctx, verifiedOrganizationKey{}, organizationID)
}

// VerifiedOrganizationFrom returns the coordinator-verified organization
// scope, if the request edge supplied one.
func VerifiedOrganizationFrom(ctx context.Context) (string, bool) {
	organizationID, ok := ctx.Value(verifiedOrganizationKey{}).(string)
	return organizationID, ok && organizationID != ""
}

type verifiedGrantContextKey struct{}

// WithVerifiedGrantContext attaches a verified grant for the provider-driver
// boundary. Callers should use the grant wrapper rather than this helper.
func WithVerifiedGrantContext(ctx context.Context, grant ExternalGrant, resolver CredentialResolver) context.Context {
	return context.WithValue(ctx, verifiedGrantContextKey{}, VerifiedGrantContext{Grant: grant, Credentials: resolver})
}

// VerifiedGrantContextFrom returns the verified grant context, if the grant
// wrapper has installed one for this call.
func VerifiedGrantContextFrom(ctx context.Context) (VerifiedGrantContext, bool) {
	v, ok := ctx.Value(verifiedGrantContextKey{}).(VerifiedGrantContext)
	return v, ok
}

// AttemptScope carries a stable call identity plus the retry/downgrade/failover
// coordinates used to produce deterministic receipt IDs. It is per invocation,
// never stored on a reusable client.
type AttemptScope struct {
	CallID        string
	LogicalCallID string
	AttemptNonce  string
	Attempt       int
	Retry         int
	Downgrade     int
	FallbackHop   int
}

type attemptScopeKey struct{}

type attemptStepKey struct{}

// WithAttemptStep installs the server-owned planner step coordinate used to
// derive a child logical-call identity from a signed grant. The run loop is
// the only production caller; the provider grant verifier still authenticates
// the parent grant and the child is deterministic from that verified parent.
func WithAttemptStep(ctx context.Context, step int) context.Context {
	if step <= 0 {
		return ctx
	}
	return context.WithValue(ctx, attemptStepKey{}, step)
}

func attemptStepFrom(ctx context.Context) (int, bool) {
	step, ok := ctx.Value(attemptStepKey{}).(int)
	return step, ok && step > 0
}

// WithAttemptScope installs a scope on ctx. The caller owns the returned scope
// and may derive per-attempt contexts with WithAttemptCoordinates.
func WithAttemptScope(ctx context.Context, scope *AttemptScope) context.Context {
	return context.WithValue(ctx, attemptScopeKey{}, scope)
}

// AttemptScopeFrom reads the current per-call scope.
func AttemptScopeFrom(ctx context.Context) (*AttemptScope, bool) {
	s, ok := ctx.Value(attemptScopeKey{}).(*AttemptScope)
	return s, ok
}

// EnsureAttemptScope returns a context with one stable invocation identity.
// The scope is allocated per call, never stored on a reusable client; retry,
// downgrade, and failover wrappers derive their attempt coordinates from it.
func EnsureAttemptScope(ctx context.Context) (context.Context, *AttemptScope, error) {
	if scope, ok := AttemptScopeFrom(ctx); ok && scope != nil && scope.CallID != "" {
		return ctx, scope, nil
	}
	var callID [16]byte
	if _, err := rand.Read(callID[:]); err != nil {
		return ctx, nil, fmt.Errorf("llm: allocate attempt scope: %w", err)
	}
	callIDText := hex.EncodeToString(callID[:])
	scope := &AttemptScope{CallID: callIDText, LogicalCallID: callIDText, AttemptNonce: callIDText}
	return WithAttemptScope(ctx, scope), scope, nil
}

// EnsureGrantAttemptScope binds retry coordinates to the coordinator-signed
// grant. A true response-loss replay carries the same signed grant and thus
// receives the same identity. ReAct/planner steps receive deterministic child
// identities derived from the server-owned step coordinate, so two steps
// cannot share one durable reservation.
func EnsureGrantAttemptScope(ctx context.Context, grant ExternalGrant) (context.Context, *AttemptScope, error) {
	if grant.LogicalCallID == "" || grant.AttemptNonce == "" {
		return ctx, nil, ErrExternalGrantInvalid
	}
	ctx, scope, err := EnsureAttemptScope(ctx)
	if err != nil {
		return ctx, nil, err
	}
	copyScope := *scope
	copyScope.LogicalCallID = grant.LogicalCallID
	copyScope.AttemptNonce = grant.AttemptNonce
	copyScope.CallID = grant.LogicalCallID
	if step, ok := attemptStepFrom(ctx); ok {
		copyScope.LogicalCallID = fmt.Sprintf("%s/step/%d", grant.LogicalCallID, step)
		copyScope.CallID = copyScope.LogicalCallID
		digest := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%d", grant.AttemptNonce, step)))
		copyScope.AttemptNonce = hex.EncodeToString(digest[:])
	}
	return WithAttemptScope(ctx, &copyScope), &copyScope, nil
}

// WithAttemptCoordinates returns a derived context with provider-attempt
// coordinates updated without mutating the shared scope.
func WithAttemptCoordinates(ctx context.Context, attempt, retry, downgrade, fallbackHop int) context.Context {
	scope, ok := AttemptScopeFrom(ctx)
	if !ok || scope == nil {
		return ctx
	}
	copyScope := *scope
	copyScope.Attempt = attempt
	copyScope.Retry = retry
	copyScope.Downgrade = downgrade
	copyScope.FallbackHop = fallbackHop
	return WithAttemptScope(ctx, &copyScope)
}
