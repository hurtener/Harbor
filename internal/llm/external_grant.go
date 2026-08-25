package llm

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	Version  int    `json:"version"`
	KeyID    string `json:"key_id"`
	Audience string `json:"audience"`
	GrantID  string `json:"grant_id"`
	// RouteMode makes the provider-route authority explicit. A blank value is
	// accepted only for legacy v1.30.0 coordinator-bound grants; new signers
	// stamp the explicit coordinator_bound value.
	RouteMode      ExternalGrantRouteMode `json:"route_mode,omitempty"`
	OrganizationID string                 `json:"organization_id"`
	RuntimeID      string                 `json:"runtime_id"`
	// AgentID is required and signed on version 2 grants. It is the exact
	// reach-admitted effective agent configuration, not the boot-derived
	// invoking-agent provenance and not a storage-isolation principal.
	AgentID                      string          `json:"agent_id,omitempty"`
	TenantID                     string          `json:"tenant_id"`
	UserID                       string          `json:"user_id"`
	SessionID                    string          `json:"session_id"`
	LogicalRunID                 string          `json:"logical_run_id"`
	LogicalCallID                string          `json:"logical_call_id"`
	AttemptNonce                 string          `json:"attempt_nonce"`
	Provider                     string          `json:"provider,omitempty"`
	ProviderModelID              string          `json:"provider_model_id,omitempty"`
	ProviderConnectionID         string          `json:"provider_connection_id,omitempty"`
	ProviderConnectionGeneration uint64          `json:"provider_connection_generation,omitempty"`
	RouteID                      string          `json:"route_id,omitempty"`
	CredentialBindingHandle      string          `json:"credential_binding_handle,omitempty"`
	CredentialAssetGeneration    uint64          `json:"credential_asset_generation,omitempty"`
	PolicyGeneration             uint64          `json:"policy_generation"`
	MaxReasoning                 ReasoningEffort `json:"max_reasoning"`
	MaxOutputTokens              int             `json:"max_output_tokens"`
	Lease                        ComputeLease    `json:"lease"`
	IssuedAt                     time.Time       `json:"issued_at"`
	ExpiresAt                    time.Time       `json:"expires_at"`
	Signature                    string          `json:"signature"`
}

const (
	// ExternalGrantVersionLegacy is the deployed v1.30.0/v1.30.1 grant shape.
	// It deliberately carries no signed agent binding.
	ExternalGrantVersionLegacy = 1
	// ExternalGrantVersionAgentBound requires the reach-admitted effective
	// agent configuration to be signed into every provider attempt.
	ExternalGrantVersionAgentBound = 2
)

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

// ExternalGrantRouteMode selects who supplies the provider route. The mode is
// signed as part of the grant and cannot be inferred from optional fields.
type ExternalGrantRouteMode string

const (
	// ExternalGrantRouteRuntimeDefault permits the runtime's configured
	// provider credentials and default model. The grant may carry limits and a
	// lease, but no coordinator-selected provider binding.
	ExternalGrantRouteRuntimeDefault ExternalGrantRouteMode = "runtime_default"
	// ExternalGrantRouteCoordinatorBound requires the signed provider/model,
	// connection, route, and opaque credential binding claims.
	ExternalGrantRouteCoordinatorBound ExternalGrantRouteMode = "coordinator_bound"
)

// EffectiveExternalGrantRouteMode treats an empty mode as the legacy
// coordinator-bound shape. New issuers should always stamp an explicit mode;
// this compatibility rule keeps already-issued v1.30.0 grants verifiable.
func EffectiveExternalGrantRouteMode(mode ExternalGrantRouteMode) ExternalGrantRouteMode {
	if mode == "" {
		return ExternalGrantRouteCoordinatorBound
	}
	return mode
}

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
	// ErrInvalidUsageReceipt identifies a malformed or unbound content-free
	// usage fact before it reaches a delivery transport.
	ErrInvalidUsageReceipt = errors.New("llm: invalid external execution usage receipt")
)

// ExternalGrantVerifier validates a signed grant against the verified request
// context and the actual model request. It must never trust caller-provided
// identity or runtime authority. A verifier accepting version 2 must also bind
// AgentID to an equivalent host-authenticated effective-agent context; Harbor's
// reference verifier uses the private reach-admitted run capability.
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

// ValidateExternalGrantTopUpSuccessor verifies that successor is a bounded
// lease top-up of current rather than a different execution authority. The
// caller must have authenticated current with its configured verifier before
// calling this relationship-only helper, and must authenticate successor with
// that verifier after this helper succeeds. The only claims allowed to rotate
// are KeyID, IssuedAt, Signature, the lease capacity/accounting epoch, and the
// two validity deadlines.
//
// requestedUnits is both the minimum remaining capacity the successor must
// provide and the maximum increase in total capacity. Validity may move
// forward, but a successor may not lengthen either the grant or lease lifetime
// beyond the respective duration signed into current. These local bounds let
// a coordinator renew a short-lived grant without turning a top-up response
// into an unbounded lease or long-lived authority.
func ValidateExternalGrantTopUpSuccessor(current, successor ExternalGrant, requestedUnits int64) error {
	invalid := func(detail string) error {
		return fmt.Errorf("%w: lease top-up successor %s", ErrExternalGrantInvalid, detail)
	}
	if requestedUnits <= 0 {
		return invalid("has an invalid requested-unit bound")
	}

	// Normalize only the explicitly mutable claims, then compare the complete
	// value. This makes every present and future comparable field immutable by
	// default. Adding a non-comparable field to ExternalGrant deliberately
	// fails compilation here until its successor semantics are reviewed. The
	// raw route mode remains part of the comparison: a legacy blank
	// coordinator-bound predecessor cannot become an explicit mode during a
	// top-up because omission is itself part of the signed authority.
	expected := current
	expected.KeyID = successor.KeyID
	expected.IssuedAt = successor.IssuedAt
	expected.ExpiresAt = successor.ExpiresAt
	expected.Signature = successor.Signature
	expected.Lease.Epoch = successor.Lease.Epoch
	expected.Lease.TokenUnits = successor.Lease.TokenUnits
	expected.Lease.ConsumedUnits = successor.Lease.ConsumedUnits
	expected.Lease.ExpiresAt = successor.Lease.ExpiresAt
	if expected != successor {
		return invalid("changed immutable authority")
	}
	if successor.Version <= 0 || successor.Audience == "" || successor.GrantID == "" ||
		successor.OrganizationID == "" || successor.RuntimeID == "" || successor.TenantID == "" ||
		successor.UserID == "" || successor.SessionID == "" || successor.LogicalRunID == "" ||
		successor.LogicalCallID == "" || successor.AttemptNonce == "" || successor.PolicyGeneration == 0 ||
		successor.MaxReasoning == "" || successor.MaxOutputTokens <= 0 || successor.Lease.LeaseID == "" {
		return invalid("omitted a required immutable claim")
	}
	if successor.Version == ExternalGrantVersionAgentBound && successor.AgentID == "" {
		return invalid("omitted the required agent binding")
	}
	if successor.KeyID == "" || successor.Signature == "" {
		return invalid("omitted rotating signing metadata")
	}

	if current.Lease.Epoch == ^uint64(0) || successor.Lease.Epoch != current.Lease.Epoch+1 {
		return invalid("did not advance the lease epoch exactly once")
	}
	if current.Lease.TokenUnits <= 0 || successor.Lease.TokenUnits <= current.Lease.TokenUnits ||
		current.Lease.ConsumedUnits < 0 || current.Lease.ConsumedUnits > current.Lease.TokenUnits ||
		successor.Lease.ConsumedUnits < current.Lease.ConsumedUnits ||
		successor.Lease.ConsumedUnits > successor.Lease.TokenUnits {
		return invalid("has non-monotonic lease capacity or consumption")
	}
	capacityIncrease := successor.Lease.TokenUnits - current.Lease.TokenUnits
	currentRemaining := current.Lease.TokenUnits - current.Lease.ConsumedUnits
	successorRemaining := successor.Lease.TokenUnits - successor.Lease.ConsumedUnits
	if capacityIncrease > requestedUnits || successorRemaining < requestedUnits ||
		successorRemaining <= currentRemaining {
		return invalid("exceeds the bounded capacity advance")
	}

	if current.IssuedAt.IsZero() || successor.IssuedAt.IsZero() || successor.IssuedAt.Before(current.IssuedAt) {
		return invalid("has a non-monotonic issued-at value")
	}
	exactLifetime := func(issuedAt, expiresAt time.Time) (time.Duration, bool) {
		lifetime := expiresAt.Sub(issuedAt)
		return lifetime, lifetime > 0 && issuedAt.Add(lifetime).Equal(expiresAt)
	}
	grantLifetime, validGrantLifetime := exactLifetime(current.IssuedAt, current.ExpiresAt)
	leaseLifetime, validLeaseLifetime := exactLifetime(current.IssuedAt, current.Lease.ExpiresAt)
	successorGrantLifetime, validSuccessorGrantLifetime := exactLifetime(successor.IssuedAt, successor.ExpiresAt)
	successorLeaseLifetime, validSuccessorLeaseLifetime := exactLifetime(successor.IssuedAt, successor.Lease.ExpiresAt)
	if !validGrantLifetime || !validLeaseLifetime || !validSuccessorGrantLifetime || !validSuccessorLeaseLifetime ||
		successor.ExpiresAt.Before(current.ExpiresAt) ||
		successor.Lease.ExpiresAt.Before(current.Lease.ExpiresAt) ||
		successorGrantLifetime > grantLifetime || successorLeaseLifetime > leaseLifetime {
		return invalid("exceeds the predecessor validity bounds")
	}
	return nil
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
	ReceiptID     string
	GrantID       string
	RouteMode     ExternalGrantRouteMode
	LogicalCallID string
	AttemptNonce  string
	// ParentLogicalCallID and ParentAttemptNonce bind a planner-derived child
	// receipt back to the signed grant identity. They are empty only on legacy
	// v1.30.0 receipts, which retain the old hash representation.
	ParentLogicalCallID          string
	ParentAttemptNonce           string
	PlannerStep                  int
	OrganizationID               string
	RuntimeID                    string
	AgentID                      string
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
	DowngradeNumber              int
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
	var (
		body []byte
		err  error
	)
	if usesLegacyAttemptUsageReceiptWire(receipt) {
		// Preserve the v1.30.0 representation for pending receipts written
		// before this additive identity/mode extension. New receipts use the
		// explicit canonical wire below.
		body, err = json.Marshal(legacyAttemptUsageReceiptFrom(receipt))
	} else {
		body, err = json.Marshal(canonicalAttemptUsageReceiptWire(receipt))
	}
	if err != nil {
		return "", fmt.Errorf("llm: canonical usage receipt: %w", err)
	}
	digest := sha256.Sum256(body)
	return hex.EncodeToString(digest[:]), nil
}

// MarshalCanonicalAttemptUsageReceipt returns the public, transport-neutral
// JSON representation for a receipt. HTTP, queue, and file deliveries may use
// this same shape without importing Harbor internals or re-deriving fields.
func MarshalCanonicalAttemptUsageReceipt(receipt AttemptUsageReceipt) ([]byte, error) {
	var wire any = canonicalAttemptUsageReceiptWire(receipt)
	if usesLegacyAttemptUsageReceiptWire(receipt) {
		wire = legacyAttemptUsageReceiptFrom(receipt)
	}
	body, err := json.Marshal(wire)
	if err != nil {
		return nil, fmt.Errorf("llm: canonical usage receipt wire: %w", err)
	}
	return body, nil
}

// UnmarshalCanonicalAttemptUsageReceipt reconstructs a validated receipt from
// the exact canonical JSON emitted by MarshalCanonicalAttemptUsageReceipt.
// It rejects unknown, duplicate, missing, reordered, alternatively encoded,
// or trailing content rather than accepting a merely equivalent JSON value.
// This keeps external receipt consumers on Harbor's one private wire shape.
func UnmarshalCanonicalAttemptUsageReceipt(data []byte) (AttemptUsageReceipt, error) {
	var wire canonicalAttemptUsageReceiptWirePayload
	if decodeExactReceiptWire(data, &wire) {
		return validateExactCanonicalReceipt(data, attemptUsageReceiptFromCanonicalWire(wire))
	}

	var legacy legacyAttemptUsageReceipt
	if decodeExactReceiptWire(data, &legacy) {
		return validateExactCanonicalReceipt(data, attemptUsageReceiptFromLegacyWire(legacy))
	}
	return AttemptUsageReceipt{}, fmt.Errorf("%w: malformed canonical wire", ErrInvalidUsageReceipt)
}

func decodeExactReceiptWire(data []byte, wire any) bool {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(wire); err != nil {
		return false
	}
	_, err := decoder.Token()
	return err == io.EOF
}

func validateExactCanonicalReceipt(data []byte, receipt AttemptUsageReceipt) (AttemptUsageReceipt, error) {
	if err := ValidateAttemptUsageReceipt(receipt); err != nil {
		// v1.30.0 receipts predate RouteMode. Retain the blank public value only
		// for that exact historical shape when it validates with the preserved
		// legacy body hash. The byte-identity gate below still requires the
		// matching historical wire and does not admit an alternative encoding.
		legacy, ok := legacyAttemptUsageReceiptFromCanonicalWire(receipt)
		if !ok {
			return AttemptUsageReceipt{}, err
		}
		if legacyErr := ValidateAttemptUsageReceipt(legacy); legacyErr != nil {
			return AttemptUsageReceipt{}, err
		}
		receipt = legacy
	}
	if receipt.ReceiptID != CanonicalAttemptID(receipt.GrantID, receipt.LogicalCallID, receipt.AttemptNonce, receipt.AttemptNumber, receipt.RetryNumber, receipt.DowngradeNumber, receipt.FallbackHop) || receipt.IdempotencyKey != receipt.ReceiptID {
		return AttemptUsageReceipt{}, fmt.Errorf("%w: attempt identity is not canonical", ErrInvalidUsageReceipt)
	}
	canonical, err := MarshalCanonicalAttemptUsageReceipt(receipt)
	if err != nil {
		return AttemptUsageReceipt{}, fmt.Errorf("%w: canonical re-encode failed", ErrInvalidUsageReceipt)
	}
	if !bytes.Equal(data, canonical) {
		return AttemptUsageReceipt{}, fmt.Errorf("%w: input is not the exact canonical wire", ErrInvalidUsageReceipt)
	}
	return receipt, nil
}

func usesLegacyAttemptUsageReceiptWire(receipt AttemptUsageReceipt) bool {
	return receipt.ParentLogicalCallID == "" && receipt.ParentAttemptNonce == "" &&
		receipt.PlannerStep == 0 && receipt.DowngradeNumber == 0 && receipt.RouteMode == "" && receipt.AgentID == ""
}

// ValidateAttemptUsageReceipt validates content-free shape and the explicit
// route boundary. It intentionally does not authenticate a grant; use
// ValidateAttemptUsageReceiptAgainstGrant after the signed grant has been
// verified.
func ValidateAttemptUsageReceipt(receipt AttemptUsageReceipt) error {
	if receipt.ReceiptID == "" || receipt.GrantID == "" || receipt.LogicalCallID == "" || receipt.AttemptNonce == "" ||
		receipt.OrganizationID == "" || receipt.RuntimeID == "" || receipt.TenantID == "" || receipt.UserID == "" || receipt.SessionID == "" || receipt.LogicalRunID == "" ||
		receipt.Provider == "" || receipt.ProviderModelID == "" || receipt.PolicyGeneration == 0 || receipt.AttemptNumber <= 0 || receipt.RetryNumber < 0 || receipt.DowngradeNumber < 0 || receipt.FallbackHop < 0 {
		return fmt.Errorf("%w: missing identity, route, policy, or attempt field", ErrInvalidUsageReceipt)
	}
	if receipt.Status != "success" && receipt.Status != "error" && receipt.Status != "canceled" {
		return fmt.Errorf("%w: unsupported status", ErrInvalidUsageReceipt)
	}
	if receipt.StartedAt.IsZero() || receipt.CompletedAt.IsZero() || receipt.CompletedAt.Before(receipt.StartedAt) {
		return fmt.Errorf("%w: invalid interval", ErrInvalidUsageReceipt)
	}
	if receipt.PromptTokens < 0 || receipt.CompletionTokens < 0 || receipt.ReasoningTokens < 0 || receipt.TotalTokens < 0 || receipt.CacheReadTokens < 0 || receipt.CacheWriteTokens < 0 ||
		receipt.InputCostMicros < 0 || receipt.OutputCostMicros < 0 || receipt.ReasoningCostMicros < 0 || receipt.TotalCostMicros < 0 || receipt.LatencyMS < 0 {
		return fmt.Errorf("%w: negative usage", ErrInvalidUsageReceipt)
	}
	mode := receipt.RouteMode
	if mode == "" {
		mode = ExternalGrantRouteCoordinatorBound // legacy v1.30.0 receipt
	}
	switch mode {
	case ExternalGrantRouteCoordinatorBound:
		if receipt.ProviderConnectionID == "" || receipt.ProviderConnectionGeneration == 0 || receipt.RouteID == "" || receipt.CredentialAssetGeneration == 0 {
			return fmt.Errorf("%w: coordinator-bound route is incomplete", ErrInvalidUsageReceipt)
		}
	case ExternalGrantRouteRuntimeDefault:
		if receipt.ProviderConnectionID != "" || receipt.ProviderConnectionGeneration != 0 || receipt.RouteID != "" || receipt.CredentialAssetGeneration != 0 {
			return fmt.Errorf("%w: runtime-default receipt carries coordinator route claims", ErrInvalidUsageReceipt)
		}
	default:
		return fmt.Errorf("%w: unsupported route mode", ErrInvalidUsageReceipt)
	}
	if receipt.CanonicalBodyHash == "" {
		return fmt.Errorf("%w: missing canonical body hash", ErrInvalidUsageReceipt)
	}
	wantHash, err := CanonicalAttemptUsageReceiptBodyHash(receipt)
	if err != nil {
		return fmt.Errorf("%w: canonical body: %w", ErrInvalidUsageReceipt, err)
	}
	if wantHash != receipt.CanonicalBodyHash {
		return fmt.Errorf("%w: canonical body hash mismatch", ErrInvalidUsageReceipt)
	}
	if receipt.PlannerStep < 0 || receipt.PlannerStep > 0 && (receipt.ParentLogicalCallID == "" || receipt.ParentAttemptNonce == "") {
		return fmt.Errorf("%w: incomplete parent attempt binding", ErrInvalidUsageReceipt)
	}
	if receipt.ParentLogicalCallID != "" || receipt.ParentAttemptNonce != "" {
		if receipt.ParentLogicalCallID == "" || receipt.ParentAttemptNonce == "" {
			return fmt.Errorf("%w: incomplete parent attempt binding", ErrInvalidUsageReceipt)
		}
	}
	return nil
}

// ValidateAttemptUsageReceiptAgainstGrant verifies that the receipt is the
// server-derived attempt represented by the signed grant. It catches forged
// planner children and receipt route/identity drift without needing the
// provider credential or prompt content.
func ValidateAttemptUsageReceiptAgainstGrant(receipt AttemptUsageReceipt, grant ExternalGrant) error {
	if err := ValidateAttemptUsageReceipt(receipt); err != nil {
		return err
	}
	if receipt.GrantID != grant.GrantID || receipt.OrganizationID != grant.OrganizationID || receipt.RuntimeID != grant.RuntimeID ||
		receipt.TenantID != grant.TenantID || receipt.UserID != grant.UserID || receipt.SessionID != grant.SessionID || receipt.LogicalRunID != grant.LogicalRunID ||
		receipt.PolicyGeneration != grant.PolicyGeneration {
		return fmt.Errorf("%w: grant identity mismatch", ErrInvalidUsageReceipt)
	}
	if grant.Version == ExternalGrantVersionAgentBound {
		if grant.AgentID == "" || receipt.AgentID == "" || receipt.AgentID != grant.AgentID {
			return fmt.Errorf("%w: grant agent binding mismatch", ErrInvalidUsageReceipt)
		}
	} else if receipt.AgentID != "" {
		return fmt.Errorf("%w: legacy grant receipt carries an unsigned agent binding", ErrInvalidUsageReceipt)
	}
	mode := grant.RouteMode
	if mode == "" {
		mode = ExternalGrantRouteCoordinatorBound
	}
	receiptMode := receipt.RouteMode
	if receiptMode == "" {
		receiptMode = ExternalGrantRouteCoordinatorBound
	}
	if receiptMode != mode {
		return fmt.Errorf("%w: grant and receipt route modes differ", ErrInvalidUsageReceipt)
	}
	if receipt.ParentLogicalCallID != "" || receipt.ParentAttemptNonce != "" {
		if receipt.ParentLogicalCallID != grant.LogicalCallID || receipt.ParentAttemptNonce != grant.AttemptNonce {
			return fmt.Errorf("%w: parent attempt mismatch", ErrInvalidUsageReceipt)
		}
		if receipt.PlannerStep > 0 {
			digest := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%d", grant.AttemptNonce, receipt.PlannerStep)))
			wantNonce := hex.EncodeToString(digest[:])
			if receipt.LogicalCallID != fmt.Sprintf("%s/step/%d", grant.LogicalCallID, receipt.PlannerStep) || receipt.AttemptNonce != wantNonce {
				return fmt.Errorf("%w: planner step derivation mismatch", ErrInvalidUsageReceipt)
			}
		} else if receipt.LogicalCallID != grant.LogicalCallID || receipt.AttemptNonce != grant.AttemptNonce {
			return fmt.Errorf("%w: root attempt derivation mismatch", ErrInvalidUsageReceipt)
		}
	} else if receipt.LogicalCallID != grant.LogicalCallID || receipt.AttemptNonce != grant.AttemptNonce {
		return fmt.Errorf("%w: attempt identity mismatch", ErrInvalidUsageReceipt)
	}
	if receipt.ReceiptID != CanonicalAttemptID(grant.GrantID, receipt.LogicalCallID, receipt.AttemptNonce, receipt.AttemptNumber, receipt.RetryNumber, receipt.DowngradeNumber, receipt.FallbackHop) || receipt.IdempotencyKey != receipt.ReceiptID {
		return fmt.Errorf("%w: attempt id mismatch", ErrInvalidUsageReceipt)
	}
	if mode == ExternalGrantRouteCoordinatorBound {
		if receipt.Provider != grant.Provider || receipt.ProviderModelID != grant.ProviderModelID || receipt.ProviderConnectionID != grant.ProviderConnectionID || receipt.ProviderConnectionGeneration != grant.ProviderConnectionGeneration || receipt.RouteID != grant.RouteID || receipt.CredentialAssetGeneration != grant.CredentialAssetGeneration {
			return fmt.Errorf("%w: coordinator route mismatch", ErrInvalidUsageReceipt)
		}
	} else if receipt.ProviderConnectionID != "" || receipt.RouteID != "" {
		return fmt.Errorf("%w: runtime-default route mismatch", ErrInvalidUsageReceipt)
	}
	return nil
}

// CanonicalAttemptID returns the stable durable identity for one provider
// attempt. Logical call and nonce are server-derived; retry, downgrade and
// fallback coordinates are explicit to keep distinct calls distinct.
func CanonicalAttemptID(grantID, logicalCallID, attemptNonce string, attempt, retry, downgrade, fallbackHop int) string {
	if attempt <= 0 {
		attempt = 1
	}
	return fmt.Sprintf("%s/%s/%s/%d/%d/%d/%d", grantID, logicalCallID, attemptNonce, retry, downgrade, fallbackHop, attempt)
}

// legacyAttemptUsageReceipt preserves the v1.30.0 hash shape for receipts
// that predate RouteMode and planner-parent fields.
type legacyAttemptUsageReceipt struct {
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

func legacyAttemptUsageReceiptFrom(receipt AttemptUsageReceipt) legacyAttemptUsageReceipt {
	return legacyAttemptUsageReceipt{ReceiptID: receipt.ReceiptID, GrantID: receipt.GrantID, LogicalCallID: receipt.LogicalCallID, AttemptNonce: receipt.AttemptNonce, OrganizationID: receipt.OrganizationID, RuntimeID: receipt.RuntimeID, TenantID: receipt.TenantID, UserID: receipt.UserID, SessionID: receipt.SessionID, LogicalRunID: receipt.LogicalRunID, Provider: receipt.Provider, ProviderModelID: receipt.ProviderModelID, ProviderConnectionID: receipt.ProviderConnectionID, ProviderConnectionGeneration: receipt.ProviderConnectionGeneration, RouteID: receipt.RouteID, CredentialAssetGeneration: receipt.CredentialAssetGeneration, PolicyGeneration: receipt.PolicyGeneration, AttemptNumber: receipt.AttemptNumber, RetryNumber: receipt.RetryNumber, FallbackHop: receipt.FallbackHop, RequestedReasoning: receipt.RequestedReasoning, EffectiveReasoning: receipt.EffectiveReasoning, PromptTokens: receipt.PromptTokens, CompletionTokens: receipt.CompletionTokens, ReasoningTokens: receipt.ReasoningTokens, TotalTokens: receipt.TotalTokens, CacheReadTokens: receipt.CacheReadTokens, CacheWriteTokens: receipt.CacheWriteTokens, InputCostMicros: receipt.InputCostMicros, OutputCostMicros: receipt.OutputCostMicros, ReasoningCostMicros: receipt.ReasoningCostMicros, TotalCostMicros: receipt.TotalCostMicros, Currency: receipt.Currency, LatencyMS: receipt.LatencyMS, Status: receipt.Status, StartedAt: receipt.StartedAt, CompletedAt: receipt.CompletedAt, IdempotencyKey: receipt.IdempotencyKey, CanonicalBodyHash: receipt.CanonicalBodyHash}
}

func attemptUsageReceiptFromLegacyWire(wire legacyAttemptUsageReceipt) AttemptUsageReceipt {
	return AttemptUsageReceipt{
		ReceiptID: wire.ReceiptID, GrantID: wire.GrantID, LogicalCallID: wire.LogicalCallID, AttemptNonce: wire.AttemptNonce,
		OrganizationID: wire.OrganizationID, RuntimeID: wire.RuntimeID, TenantID: wire.TenantID, UserID: wire.UserID,
		SessionID: wire.SessionID, LogicalRunID: wire.LogicalRunID, Provider: wire.Provider, ProviderModelID: wire.ProviderModelID,
		ProviderConnectionID: wire.ProviderConnectionID, ProviderConnectionGeneration: wire.ProviderConnectionGeneration,
		RouteID: wire.RouteID, CredentialAssetGeneration: wire.CredentialAssetGeneration, PolicyGeneration: wire.PolicyGeneration,
		AttemptNumber: wire.AttemptNumber, RetryNumber: wire.RetryNumber, FallbackHop: wire.FallbackHop,
		RequestedReasoning: wire.RequestedReasoning, EffectiveReasoning: wire.EffectiveReasoning,
		PromptTokens: wire.PromptTokens, CompletionTokens: wire.CompletionTokens, ReasoningTokens: wire.ReasoningTokens,
		TotalTokens: wire.TotalTokens, CacheReadTokens: wire.CacheReadTokens, CacheWriteTokens: wire.CacheWriteTokens,
		InputCostMicros: wire.InputCostMicros, OutputCostMicros: wire.OutputCostMicros,
		ReasoningCostMicros: wire.ReasoningCostMicros, TotalCostMicros: wire.TotalCostMicros,
		Currency: wire.Currency, LatencyMS: wire.LatencyMS, Status: wire.Status, StartedAt: wire.StartedAt,
		CompletedAt: wire.CompletedAt, IdempotencyKey: wire.IdempotencyKey, CanonicalBodyHash: wire.CanonicalBodyHash,
	}
}

// canonicalAttemptUsageReceiptWire is the stable public JSON contract. The
// internal struct remains untagged for backwards-compatible persistence.
type canonicalAttemptUsageReceiptWirePayload struct {
	ReceiptID                    string                 `json:"receipt_id"`
	GrantID                      string                 `json:"grant_id"`
	RouteMode                    ExternalGrantRouteMode `json:"route_mode"`
	LogicalCallID                string                 `json:"logical_call_id"`
	AttemptNonce                 string                 `json:"attempt_nonce"`
	ParentLogicalCallID          string                 `json:"parent_logical_call_id,omitempty"`
	ParentAttemptNonce           string                 `json:"parent_attempt_nonce,omitempty"`
	PlannerStep                  int                    `json:"planner_step,omitempty"`
	OrganizationID               string                 `json:"organization_id"`
	RuntimeID                    string                 `json:"runtime_id"`
	AgentID                      string                 `json:"agent_id,omitempty"`
	TenantID                     string                 `json:"tenant_id"`
	UserID                       string                 `json:"user_id"`
	SessionID                    string                 `json:"session_id"`
	LogicalRunID                 string                 `json:"logical_run_id"`
	Provider                     string                 `json:"provider"`
	ProviderModelID              string                 `json:"provider_model_id"`
	ProviderConnectionID         string                 `json:"provider_connection_id,omitempty"`
	ProviderConnectionGeneration uint64                 `json:"provider_connection_generation,omitempty"`
	RouteID                      string                 `json:"route_id,omitempty"`
	CredentialAssetGeneration    uint64                 `json:"credential_asset_generation,omitempty"`
	PolicyGeneration             uint64                 `json:"policy_generation"`
	AttemptNumber                int                    `json:"attempt_number"`
	RetryNumber                  int                    `json:"retry_number"`
	DowngradeNumber              int                    `json:"downgrade_number"`
	FallbackHop                  int                    `json:"fallback_hop"`
	RequestedReasoning           ReasoningEffort        `json:"requested_reasoning"`
	EffectiveReasoning           ReasoningEffort        `json:"effective_reasoning"`
	PromptTokens                 int                    `json:"prompt_tokens"`
	CompletionTokens             int                    `json:"completion_tokens"`
	ReasoningTokens              int                    `json:"reasoning_tokens"`
	TotalTokens                  int                    `json:"total_tokens"`
	CacheReadTokens              int                    `json:"cache_read_tokens"`
	CacheWriteTokens             int                    `json:"cache_write_tokens"`
	InputCostMicros              int64                  `json:"input_cost_micros"`
	OutputCostMicros             int64                  `json:"output_cost_micros"`
	ReasoningCostMicros          int64                  `json:"reasoning_cost_micros"`
	TotalCostMicros              int64                  `json:"total_cost_micros"`
	Currency                     string                 `json:"currency"`
	LatencyMS                    int64                  `json:"latency_ms"`
	Status                       string                 `json:"status"`
	StartedAt                    time.Time              `json:"started_at"`
	CompletedAt                  time.Time              `json:"completed_at"`
	IdempotencyKey               string                 `json:"idempotency_key"`
	CanonicalBodyHash            string                 `json:"canonical_body_hash,omitempty"`
}

func canonicalAttemptUsageReceiptWire(receipt AttemptUsageReceipt) canonicalAttemptUsageReceiptWirePayload {
	mode := receipt.RouteMode
	if mode == "" {
		mode = ExternalGrantRouteCoordinatorBound
	}
	return canonicalAttemptUsageReceiptWirePayload{ReceiptID: receipt.ReceiptID, GrantID: receipt.GrantID, RouteMode: mode, LogicalCallID: receipt.LogicalCallID, AttemptNonce: receipt.AttemptNonce, ParentLogicalCallID: receipt.ParentLogicalCallID, ParentAttemptNonce: receipt.ParentAttemptNonce, PlannerStep: receipt.PlannerStep, OrganizationID: receipt.OrganizationID, RuntimeID: receipt.RuntimeID, AgentID: receipt.AgentID, TenantID: receipt.TenantID, UserID: receipt.UserID, SessionID: receipt.SessionID, LogicalRunID: receipt.LogicalRunID, Provider: receipt.Provider, ProviderModelID: receipt.ProviderModelID, ProviderConnectionID: receipt.ProviderConnectionID, ProviderConnectionGeneration: receipt.ProviderConnectionGeneration, RouteID: receipt.RouteID, CredentialAssetGeneration: receipt.CredentialAssetGeneration, PolicyGeneration: receipt.PolicyGeneration, AttemptNumber: receipt.AttemptNumber, RetryNumber: receipt.RetryNumber, DowngradeNumber: receipt.DowngradeNumber, FallbackHop: receipt.FallbackHop, RequestedReasoning: receipt.RequestedReasoning, EffectiveReasoning: receipt.EffectiveReasoning, PromptTokens: receipt.PromptTokens, CompletionTokens: receipt.CompletionTokens, ReasoningTokens: receipt.ReasoningTokens, TotalTokens: receipt.TotalTokens, CacheReadTokens: receipt.CacheReadTokens, CacheWriteTokens: receipt.CacheWriteTokens, InputCostMicros: receipt.InputCostMicros, OutputCostMicros: receipt.OutputCostMicros, ReasoningCostMicros: receipt.ReasoningCostMicros, TotalCostMicros: receipt.TotalCostMicros, Currency: receipt.Currency, LatencyMS: receipt.LatencyMS, Status: receipt.Status, StartedAt: receipt.StartedAt, CompletedAt: receipt.CompletedAt, IdempotencyKey: receipt.IdempotencyKey, CanonicalBodyHash: receipt.CanonicalBodyHash}
}

func attemptUsageReceiptFromCanonicalWire(wire canonicalAttemptUsageReceiptWirePayload) AttemptUsageReceipt {
	// Direct conversion is intentional: the private wire and public receipt must
	// stay field-for-field identical (JSON tags do not affect conversion). A
	// field/type/order drift now fails compilation instead of silently dropping a
	// canonical receipt fact in a handwritten reverse projection.
	return AttemptUsageReceipt(wire)
}

// legacyAttemptUsageReceiptFromCanonicalWire restores the blank public route
// mode only for a receipt whose canonical wire is the historical projection of
// the v1.30.0 receipt shape. That shape has no parent/planner identity and no
// downgrade coordinate; its body hash intentionally predates those additive
// fields. The public marshal helper still canonicalizes the restored value to
// coordinator_bound, preserving byte-identical delivery replay.
func legacyAttemptUsageReceiptFromCanonicalWire(receipt AttemptUsageReceipt) (AttemptUsageReceipt, bool) {
	if receipt.RouteMode != ExternalGrantRouteCoordinatorBound ||
		receipt.ParentLogicalCallID != "" || receipt.ParentAttemptNonce != "" ||
		receipt.PlannerStep != 0 || receipt.DowngradeNumber != 0 || receipt.AgentID != "" {
		return AttemptUsageReceipt{}, false
	}
	receipt.RouteMode = ""
	return receipt, true
}

// ExternalGrantConfig wires the opt-in grant and receipt seams into llm.Open.
// A zero value disables the layer and preserves legacy behavior.
type ExternalGrantConfig struct {
	Mode ExternalGrantMode
	// RouteMode optionally restricts accepted signed grant shapes. Empty
	// accepts both explicit route modes; the signed grant remains authoritative.
	RouteMode       ExternalGrantRouteMode
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
	CallID              string
	LogicalCallID       string
	AttemptNonce        string
	ParentLogicalCallID string
	ParentAttemptNonce  string
	PlannerStep         int
	Attempt             int
	Retry               int
	Downgrade           int
	FallbackHop         int
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
	copyScope.ParentLogicalCallID = grant.LogicalCallID
	copyScope.ParentAttemptNonce = grant.AttemptNonce
	copyScope.PlannerStep = 0
	copyScope.CallID = grant.LogicalCallID
	if step, ok := attemptStepFrom(ctx); ok {
		copyScope.PlannerStep = step
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
