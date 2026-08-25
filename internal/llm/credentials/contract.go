// Package credentials defines the canonical, provider-neutral coordinator
// exchange for resolving one already-verified external-grant credential.
package credentials

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/hurtener/Harbor/internal/llm"
)

const (
	// Version is the only credential-resolution contract version supported.
	Version = 1
	// MaxRequestBytes bounds the full signed-grant request envelope.
	MaxRequestBytes = 32 << 10
	// MaxResponseBytes bounds the credential response envelope.
	MaxResponseBytes = 64 << 10
)

var (
	// ErrInvalidRequest identifies a malformed or noncanonical request.
	ErrInvalidRequest = errors.New("llm/credentials: invalid request")
	// ErrInvalidResponse identifies a malformed, mismatched, or noncanonical response.
	ErrInvalidResponse = errors.New("llm/credentials: invalid response")
)

// Request carries only the complete signed grant that the runtime already
// verified. It deliberately has no independently selectable authority fields.
type Request struct {
	Version int
	Grant   llm.ExternalGrant
}

// Response is the bounded provider secret returned to the runtime driver
// edge. Every non-secret binding field must exactly match the signed request.
type Response struct {
	Version                      int
	Provider                     string
	CredentialBindingHandle      string
	CredentialAssetGeneration    uint64
	ProviderConnectionGeneration uint64
	Secret                       string
	ExpiresAt                    time.Time
}

type requestWire struct {
	Version int             `json:"version"`
	Grant   json.RawMessage `json:"grant"`
}

type responseWire struct {
	Version                      int    `json:"version"`
	Provider                     string `json:"provider"`
	CredentialBindingHandle      string `json:"credential_binding_handle"`
	CredentialAssetGeneration    uint64 `json:"credential_asset_generation"`
	ProviderConnectionGeneration uint64 `json:"provider_connection_generation"`
	//nolint:gosec // G117: this canonical credential response requires the fixed public "secret" wire key; it is never logged or persisted.
	Secret    string    `json:"secret"`
	ExpiresAt time.Time `json:"expires_at"`
}

// NewRequest validates the coordinator-bound shape before transport.
func NewRequest(grant llm.ExternalGrant) (Request, error) {
	if llm.EffectiveExternalGrantRouteMode(grant.RouteMode) != llm.ExternalGrantRouteCoordinatorBound ||
		grant.OrganizationID == "" || grant.RuntimeID == "" || grant.AgentID == "" ||
		grant.TenantID == "" || grant.UserID == "" || grant.SessionID == "" || grant.LogicalRunID == "" ||
		grant.Provider == "" || grant.ProviderModelID == "" || grant.ProviderConnectionID == "" ||
		grant.ProviderConnectionGeneration == 0 || grant.RouteID == "" ||
		grant.CredentialBindingHandle == "" || grant.CredentialAssetGeneration == 0 || grant.Signature == "" {
		return Request{}, fmt.Errorf("%w: incomplete coordinator-bound grant", ErrInvalidRequest)
	}
	return Request{Version: Version, Grant: grant}, nil
}

// MarshalCanonicalRequest returns the exact bounded request wire.
func MarshalCanonicalRequest(request Request) ([]byte, error) {
	validated, err := NewRequest(request.Grant)
	if err != nil || request.Version != Version {
		return nil, fmt.Errorf("%w: unsupported request", ErrInvalidRequest)
	}
	grant, err := llm.MarshalCanonicalExternalGrant(validated.Grant)
	if err != nil {
		return nil, fmt.Errorf("%w: canonical grant", ErrInvalidRequest)
	}
	body, err := json.Marshal(requestWire{Version: Version, Grant: grant})
	if err != nil || len(body) > MaxRequestBytes {
		return nil, fmt.Errorf("%w: request exceeds canonical bound", ErrInvalidRequest)
	}
	return body, nil
}

// UnmarshalCanonicalRequest accepts only byte-exact canonical request JSON.
// It parses authority but does not authenticate the grant signature.
func UnmarshalCanonicalRequest(data []byte) (Request, error) {
	if len(data) == 0 || len(data) > MaxRequestBytes {
		return Request{}, fmt.Errorf("%w: request byte bound", ErrInvalidRequest)
	}
	var wire requestWire
	if err := decodeStrict(data, &wire); err != nil || wire.Version != Version {
		return Request{}, fmt.Errorf("%w: malformed request", ErrInvalidRequest)
	}
	grant, err := llm.UnmarshalCanonicalExternalGrant(wire.Grant)
	if err != nil {
		return Request{}, fmt.Errorf("%w: malformed grant", ErrInvalidRequest)
	}
	request, err := NewRequest(grant)
	if err != nil {
		return Request{}, err
	}
	canonical, err := MarshalCanonicalRequest(request)
	if err != nil || !bytes.Equal(canonical, data) {
		return Request{}, fmt.Errorf("%w: request is not canonical", ErrInvalidRequest)
	}
	return request, nil
}

// MarshalCanonicalResponse returns the exact bounded response wire.
func MarshalCanonicalResponse(request Request, response Response) ([]byte, error) {
	if err := validateResponse(request, response); err != nil {
		return nil, err
	}
	body, err := json.Marshal(responseWire{
		Version: Version, Provider: response.Provider,
		CredentialBindingHandle:      response.CredentialBindingHandle,
		CredentialAssetGeneration:    response.CredentialAssetGeneration,
		ProviderConnectionGeneration: response.ProviderConnectionGeneration,
		Secret:                       response.Secret, ExpiresAt: response.ExpiresAt.UTC(),
	})
	if err != nil || len(body) > MaxResponseBytes {
		return nil, fmt.Errorf("%w: response exceeds canonical bound", ErrInvalidResponse)
	}
	return body, nil
}

// ParseCanonicalResponse parses and binds a response to the exact request.
func ParseCanonicalResponse(request Request, data []byte) (Response, error) {
	if len(data) == 0 || len(data) > MaxResponseBytes {
		return Response{}, fmt.Errorf("%w: response byte bound", ErrInvalidResponse)
	}
	var wire responseWire
	if err := decodeStrict(data, &wire); err != nil {
		return Response{}, fmt.Errorf("%w: malformed response", ErrInvalidResponse)
	}
	response := Response{
		Version: wire.Version, Provider: wire.Provider,
		CredentialBindingHandle:      wire.CredentialBindingHandle,
		CredentialAssetGeneration:    wire.CredentialAssetGeneration,
		ProviderConnectionGeneration: wire.ProviderConnectionGeneration,
		Secret:                       wire.Secret, ExpiresAt: wire.ExpiresAt.UTC(),
	}
	if err := validateResponse(request, response); err != nil {
		return Response{}, err
	}
	canonical, err := MarshalCanonicalResponse(request, response)
	if err != nil || !bytes.Equal(canonical, data) {
		return Response{}, fmt.Errorf("%w: response is not canonical", ErrInvalidResponse)
	}
	return response, nil
}

func validateResponse(request Request, response Response) error {
	if request.Version != Version || response.Version != Version || response.Secret == "" || response.ExpiresAt.IsZero() ||
		response.Provider != request.Grant.Provider ||
		response.CredentialBindingHandle != request.Grant.CredentialBindingHandle ||
		response.CredentialAssetGeneration != request.Grant.CredentialAssetGeneration ||
		response.ProviderConnectionGeneration != request.Grant.ProviderConnectionGeneration {
		return fmt.Errorf("%w: response binding mismatch", ErrInvalidResponse)
	}
	return nil
}

func decodeStrict(data []byte, out any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		return err
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return errors.New("trailing JSON value")
	}
	return nil
}
