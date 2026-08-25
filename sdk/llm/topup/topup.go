// Package topup defines Harbor's transport-neutral external-grant renewal
// exchange. Coordinators and runtimes share these exact canonical request and
// response bytes without importing Harbor internals or recreating a private
// HTTP shape.
package topup

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strconv"

	llmsdk "github.com/hurtener/Harbor/sdk/llm"
)

const (
	// ContractVersion is the canonical renewal envelope version.
	ContractVersion = 1
	// MaxCanonicalGrantBytes bounds one complete signed predecessor/successor.
	MaxCanonicalGrantBytes = 64 << 10
	// MaxRequestBytes is the maximum canonical renewal request body.
	MaxRequestBytes = 128 << 10
	// MaxResponseBytes is the maximum canonical renewal response body.
	MaxResponseBytes = 128 << 10
	// MaxRequestedUnits bounds one renewal request independently of platform
	// integer size. Coordinators may enforce a lower policy ceiling.
	MaxRequestedUnits int64 = 1_000_000_000
)

var (
	// ErrInvalidRequest identifies a malformed or non-canonical renewal request.
	ErrInvalidRequest = errors.New("llm/topup: invalid renewal request")
	// ErrInvalidResponse identifies a malformed, non-canonical, or unbound
	// renewal response.
	ErrInvalidResponse = errors.New("llm/topup: invalid renewal response")
	// ErrIdempotencyMismatch identifies a missing or unequal authenticated HTTP
	// Idempotency-Key header independently of which side validates the wire.
	ErrIdempotencyMismatch = errors.New("llm/topup: idempotency header/body mismatch")
)

// Request is the exact content-free renewal request shared by runtimes and
// coordinators. It carries no provider credential or user content.
type Request struct {
	Version        int                               `json:"version"`
	IdempotencyKey string                            `json:"idempotency_key"`
	RequestedUnits int64                             `json:"requested_units"`
	RenewalReason  llmsdk.ExternalGrantRenewalReason `json:"renewal_reason"`
	Predecessor    llmsdk.ExternalGrant              `json:"predecessor"`
}

// Response returns the exact idempotency identity and one signed successor.
// The runtime still authenticates the successor with its configured verifier.
type Response struct {
	Version        int                  `json:"version"`
	IdempotencyKey string               `json:"idempotency_key"`
	Successor      llmsdk.ExternalGrant `json:"successor"`
}

// NewRequest constructs the one canonical request for predecessor and units.
func NewRequest(predecessor llmsdk.ExternalGrant, requestedUnits int64) (Request, error) {
	reason := llmsdk.ExternalGrantRenewalExpired
	if predecessor.Lease.RemainingTokens() < requestedUnits {
		reason = llmsdk.ExternalGrantRenewalLeaseInsufficient
	}
	return NewRequestForReason(predecessor, requestedUnits, reason)
}

// NewRequestForReason constructs the canonical request for an authenticated
// trigger. LeaseInsufficient may come from durable settlement even when the
// predecessor's immutable signed consumption snapshot still looks sufficient.
func NewRequestForReason(predecessor llmsdk.ExternalGrant, requestedUnits int64, reason llmsdk.ExternalGrantRenewalReason) (Request, error) {
	req := Request{
		Version: ContractVersion, RequestedUnits: requestedUnits,
		RenewalReason: reason, Predecessor: predecessor,
	}
	key, err := RenewalIdempotencyKey(predecessor, requestedUnits, reason)
	if err != nil {
		return Request{}, err
	}
	req.IdempotencyKey = key
	if err := ValidateRequest(req); err != nil {
		return Request{}, err
	}
	return req, nil
}

// NewResponse constructs a response bound to request and validates that the
// successor advances only what request's renewal reason permits.
func NewResponse(request Request, successor llmsdk.ExternalGrant) (Response, error) {
	if err := ValidateRequest(request); err != nil {
		return Response{}, err
	}
	response := Response{Version: ContractVersion, IdempotencyKey: request.IdempotencyKey, Successor: successor}
	if err := ValidateResponse(request, response); err != nil {
		return Response{}, err
	}
	return response, nil
}

// TopUpIdempotencyKey derives the stable response-loss identity from the exact
// canonical signed predecessor bytes, requested units, and the renewal reason
// implied by the predecessor's signed remaining capacity. Callers acting on
// newer durable insufficiency use RenewalIdempotencyKey explicitly.
func TopUpIdempotencyKey(predecessor llmsdk.ExternalGrant, requestedUnits int64) (string, error) {
	reason := llmsdk.ExternalGrantRenewalExpired
	if predecessor.Lease.RemainingTokens() < requestedUnits {
		reason = llmsdk.ExternalGrantRenewalLeaseInsufficient
	}
	return RenewalIdempotencyKey(predecessor, requestedUnits, reason)
}

// RenewalIdempotencyKey covers every semantic request field: predecessor,
// units, and reason. The reason matters when durable settlement proves
// insufficiency while the immutable signed snapshot still appears ample.
func RenewalIdempotencyKey(predecessor llmsdk.ExternalGrant, requestedUnits int64, reason llmsdk.ExternalGrantRenewalReason) (string, error) {
	if requestedUnits <= 0 || requestedUnits > MaxRequestedUnits {
		return "", fmt.Errorf("%w: requested units outside bound", ErrInvalidRequest)
	}
	predecessorBytes, err := llmsdk.MarshalCanonicalExternalGrant(predecessor)
	if err != nil {
		return "", fmt.Errorf("%w: canonical predecessor", ErrInvalidRequest)
	}
	if len(predecessorBytes) > MaxCanonicalGrantBytes {
		return "", fmt.Errorf("%w: canonical predecessor exceeds bound", ErrInvalidRequest)
	}
	if reason != llmsdk.ExternalGrantRenewalExpired && reason != llmsdk.ExternalGrantRenewalLeaseInsufficient {
		return "", fmt.Errorf("%w: unsupported renewal reason", ErrInvalidRequest)
	}
	digest := sha256.New()
	_, _ = digest.Write([]byte("harbor.external-grant.top-up.v1\x00"))
	_, _ = digest.Write(predecessorBytes)
	_, _ = digest.Write([]byte("\x00"))
	_, _ = digest.Write([]byte(strconv.FormatInt(requestedUnits, 10)))
	_, _ = digest.Write([]byte("\x00"))
	_, _ = digest.Write([]byte(reason))
	return "sha256:" + hex.EncodeToString(digest.Sum(nil)), nil
}

// ValidateRequest checks version, bounds, derived reason, and exact
// idempotency identity. Signature authority belongs to the runtime verifier.
func ValidateRequest(request Request) error {
	if request.Version != ContractVersion || request.RequestedUnits <= 0 || request.RequestedUnits > MaxRequestedUnits {
		return fmt.Errorf("%w: version or requested-unit bound", ErrInvalidRequest)
	}
	if request.RenewalReason != llmsdk.ExternalGrantRenewalExpired && request.RenewalReason != llmsdk.ExternalGrantRenewalLeaseInsufficient {
		return fmt.Errorf("%w: renewal reason mismatch", ErrInvalidRequest)
	}
	if request.RenewalReason == llmsdk.ExternalGrantRenewalExpired && request.Predecessor.Lease.RemainingTokens() < request.RequestedUnits {
		return fmt.Errorf("%w: expiry renewal lacks signed capacity", ErrInvalidRequest)
	}
	wantKey, err := RenewalIdempotencyKey(request.Predecessor, request.RequestedUnits, request.RenewalReason)
	if err != nil || request.IdempotencyKey != wantKey {
		return fmt.Errorf("%w: idempotency identity mismatch", ErrInvalidRequest)
	}
	return nil
}

// ValidateResponse binds the response to request and applies Harbor's
// reason-aware immutable-successor relationship check. Signature trust is a
// separate runtime verifier step after this transport-neutral validation.
func ValidateResponse(request Request, response Response) error {
	if err := ValidateRequest(request); err != nil {
		return err
	}
	if response.Version != ContractVersion || response.IdempotencyKey != request.IdempotencyKey {
		return fmt.Errorf("%w: envelope identity mismatch", ErrInvalidResponse)
	}
	successorBytes, err := llmsdk.MarshalCanonicalExternalGrant(response.Successor)
	if err != nil || len(successorBytes) > MaxCanonicalGrantBytes {
		return fmt.Errorf("%w: canonical successor exceeds bound", ErrInvalidResponse)
	}
	if err := llmsdk.ValidateExternalGrantRenewalSuccessor(request.Predecessor, response.Successor, request.RequestedUnits, request.RenewalReason); err != nil {
		return fmt.Errorf("%w: successor relationship", ErrInvalidResponse)
	}
	return nil
}

// ValidateIdempotencyHeader requires the authenticated HTTP header and
// canonical body to name the same request identity.
func ValidateIdempotencyHeader(headerValue, bodyValue string) error {
	if headerValue == "" || headerValue != bodyValue {
		return ErrIdempotencyMismatch
	}
	return nil
}

// MarshalCanonicalRequest returns the exact version-1 request bytes.
func MarshalCanonicalRequest(request Request) ([]byte, error) {
	if err := ValidateRequest(request); err != nil {
		return nil, err
	}
	body, err := json.Marshal(request)
	if err != nil || len(body) > MaxRequestBytes {
		return nil, fmt.Errorf("%w: canonical body exceeds bound", ErrInvalidRequest)
	}
	return body, nil
}

// ParseCanonicalRequest accepts only MarshalCanonicalRequest's exact bytes.
func ParseCanonicalRequest(data []byte) (Request, error) {
	if len(data) == 0 || len(data) > MaxRequestBytes {
		return Request{}, fmt.Errorf("%w: body size", ErrInvalidRequest)
	}
	var request Request
	if err := decodeExact(data, &request); err != nil {
		return Request{}, fmt.Errorf("%w: malformed body", ErrInvalidRequest)
	}
	canonical, err := MarshalCanonicalRequest(request)
	if err != nil || !bytes.Equal(data, canonical) {
		return Request{}, fmt.Errorf("%w: body is not canonical", ErrInvalidRequest)
	}
	return request, nil
}

// MarshalCanonicalResponse returns the exact version-1 response bytes.
func MarshalCanonicalResponse(request Request, response Response) ([]byte, error) {
	if err := ValidateResponse(request, response); err != nil {
		return nil, err
	}
	body, err := json.Marshal(response)
	if err != nil || len(body) > MaxResponseBytes {
		return nil, fmt.Errorf("%w: canonical body exceeds bound", ErrInvalidResponse)
	}
	return body, nil
}

// ParseCanonicalResponse accepts only MarshalCanonicalResponse's exact bytes
// and validates it against the exact request.
func ParseCanonicalResponse(request Request, data []byte) (Response, error) {
	if len(data) == 0 || len(data) > MaxResponseBytes {
		return Response{}, fmt.Errorf("%w: body size", ErrInvalidResponse)
	}
	var response Response
	if err := decodeExact(data, &response); err != nil {
		return Response{}, fmt.Errorf("%w: malformed body", ErrInvalidResponse)
	}
	canonical, err := MarshalCanonicalResponse(request, response)
	if err != nil || !bytes.Equal(data, canonical) {
		return Response{}, fmt.Errorf("%w: body is not canonical", ErrInvalidResponse)
	}
	return response, nil
}

func decodeExact(data []byte, out any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(out); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("trailing JSON value")
	}
	return nil
}
