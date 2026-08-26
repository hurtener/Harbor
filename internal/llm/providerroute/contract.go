// Package providerroute defines the bounded, provider-neutral route-resolution
// exchange used by an optional external resolver.
package providerroute

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
	Version          = 1
	MaxRequestBytes  = 32 << 10
	MaxResponseBytes = 64 << 10
	OperationSelect  = "select"
	OperationResolve = "resolve"
)

type requestWire struct {
	Version                      int    `json:"version"`
	Operation                    string `json:"operation"`
	TenantID                     string `json:"tenant_id"`
	UserID                       string `json:"user_id"`
	SessionID                    string `json:"session_id"`
	LogicalRunID                 string `json:"logical_run_id"`
	EffectiveAgentID             string `json:"effective_agent_id"`
	RuntimeID                    string `json:"runtime_id"`
	TaskID                       string `json:"task_id"`
	LogicalCallID                string `json:"logical_call_id"`
	RouteID                      string `json:"route_id"`
	RouteGeneration              uint64 `json:"route_generation"`
	ProviderConnectionID         string `json:"provider_connection_id"`
	ProviderConnectionGeneration uint64 `json:"provider_connection_generation"`
	CredentialAssetGeneration    uint64 `json:"credential_asset_generation"`
	ModelSelector                string `json:"model_selector"`
}

type responseWire struct {
	Version                      int           `json:"version"`
	Provider                     string        `json:"provider"`
	Model                        string        `json:"model"`
	KeyName                      string        `json:"key_name"`
	RouteID                      string        `json:"route_id"`
	RouteGeneration              uint64        `json:"route_generation"`
	ProviderConnectionID         string        `json:"provider_connection_id"`
	ProviderConnectionGeneration uint64        `json:"provider_connection_generation"`
	CredentialAssetGeneration    uint64        `json:"credential_asset_generation"`
	ModelSelector                string        `json:"model_selector"`
	Endpoint                     *endpointWire `json:"endpoint,omitempty"`
	Credential                   string        `json:"credential,omitempty"`
	ExpiresAt                    time.Time     `json:"expires_at"`
}

type endpointWire struct {
	Kind   string `json:"kind"`
	Value  string `json:"value"`
	Digest string `json:"digest"`
}

// MarshalRequest returns the canonical bounded trusted request.
func MarshalRequest(req llm.ProviderRouteRequest) ([]byte, error) {
	return marshalRequest(req, OperationResolve)
}

// MarshalSelectionRequest returns the credential-free pre-policy selection
// request. The resolver must omit credential material from its response.
func MarshalSelectionRequest(req llm.ProviderRouteRequest) ([]byte, error) {
	return marshalRequest(req, OperationSelect)
}

func marshalRequest(req llm.ProviderRouteRequest, operation string) ([]byte, error) {
	w := requestWire{
		Version: Version, Operation: operation, TenantID: req.TenantID, UserID: req.UserID, SessionID: req.SessionID,
		LogicalRunID: req.LogicalRunID, EffectiveAgentID: req.EffectiveAgentID, RuntimeID: req.RuntimeID,
		TaskID: req.TaskID, LogicalCallID: req.LogicalCallID, RouteID: req.RouteID,
		RouteGeneration: req.RouteGeneration, ProviderConnectionID: req.ProviderConnectionID,
		ProviderConnectionGeneration: req.ProviderConnectionGeneration,
		CredentialAssetGeneration:    req.CredentialAssetGeneration, ModelSelector: req.ModelSelector,
	}
	if (w.Operation != OperationSelect && w.Operation != OperationResolve) ||
		w.TenantID == "" || w.UserID == "" || w.SessionID == "" || w.LogicalRunID == "" ||
		w.EffectiveAgentID == "" || w.RuntimeID == "" || w.TaskID == "" || w.LogicalCallID == "" ||
		w.RouteID == "" || w.RouteGeneration == 0 || w.ProviderConnectionID == "" ||
		w.ProviderConnectionGeneration == 0 || w.CredentialAssetGeneration == 0 || w.ModelSelector == "" {
		return nil, llm.ErrProviderRouteInvalid
	}
	body, err := json.Marshal(w)
	if err != nil || len(body) > MaxRequestBytes {
		return nil, fmt.Errorf("provider route: request exceeds bound")
	}
	return body, nil
}

// UnmarshalRequest strictly parses the bounded trusted-context request.
func UnmarshalRequest(body []byte) (llm.ProviderRouteRequest, error) {
	_, req, err := UnmarshalOperationRequest(body)
	return req, err
}

// UnmarshalOperationRequest strictly parses the operation and trusted-context
// request so a resolver can keep selection credential-free and resolution
// attempt-scoped on one authenticated endpoint.
func UnmarshalOperationRequest(body []byte) (string, llm.ProviderRouteRequest, error) {
	if len(body) == 0 || len(body) > MaxRequestBytes {
		return "", llm.ProviderRouteRequest{}, llm.ErrProviderRouteInvalid
	}
	var w requestWire
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&w); err != nil {
		return "", llm.ProviderRouteRequest{}, llm.ErrProviderRouteInvalid
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) || w.Version != Version ||
		(w.Operation != OperationSelect && w.Operation != OperationResolve) {
		return "", llm.ProviderRouteRequest{}, llm.ErrProviderRouteInvalid
	}
	req := llm.ProviderRouteRequest{
		TenantID: w.TenantID, UserID: w.UserID, SessionID: w.SessionID, LogicalRunID: w.LogicalRunID,
		EffectiveAgentID: w.EffectiveAgentID, RuntimeID: w.RuntimeID, TaskID: w.TaskID, LogicalCallID: w.LogicalCallID,
		RouteID: w.RouteID, RouteGeneration: w.RouteGeneration, ProviderConnectionID: w.ProviderConnectionID,
		ProviderConnectionGeneration: w.ProviderConnectionGeneration,
		CredentialAssetGeneration:    w.CredentialAssetGeneration, ModelSelector: w.ModelSelector,
	}
	if _, err := MarshalRequest(req); err != nil {
		return "", llm.ProviderRouteRequest{}, err
	}
	return w.Operation, req, nil
}

// ParseSelectionResponse strictly parses a credential-free exact-bound route
// decision. A resolver that includes a credential in this stage is rejected.
func ParseSelectionResponse(req llm.ProviderRouteRequest, body []byte) (llm.SelectedProviderRoute, error) {
	if len(body) == 0 || len(body) > MaxResponseBytes {
		return llm.SelectedProviderRoute{}, llm.ErrProviderRouteInvalid
	}
	var w responseWire
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&w); err != nil {
		return llm.SelectedProviderRoute{}, llm.ErrProviderRouteInvalid
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) || w.Version != Version ||
		w.Provider == "" || w.Model == "" || w.KeyName == "" || w.Credential != "" ||
		w.RouteID != req.RouteID || w.RouteGeneration != req.RouteGeneration ||
		w.ProviderConnectionID != req.ProviderConnectionID ||
		w.ProviderConnectionGeneration != req.ProviderConnectionGeneration ||
		w.CredentialAssetGeneration != req.CredentialAssetGeneration ||
		w.ModelSelector != req.ModelSelector || w.ExpiresAt.IsZero() {
		return llm.SelectedProviderRoute{}, llm.ErrProviderRouteInvalid
	}
	endpoint, err := parseEndpoint(w.Endpoint)
	if err != nil {
		return llm.SelectedProviderRoute{}, err
	}
	return llm.SelectedProviderRoute{
		Provider: w.Provider, Model: w.Model, KeyName: w.KeyName, RouteID: w.RouteID, RouteGeneration: w.RouteGeneration,
		ProviderConnectionID:         w.ProviderConnectionID,
		ProviderConnectionGeneration: w.ProviderConnectionGeneration,
		CredentialAssetGeneration:    w.CredentialAssetGeneration,
		ModelSelector:                w.ModelSelector,
		Endpoint:                     endpoint,
		ExpiresAt:                    w.ExpiresAt.UTC(),
	}, nil
}

// MarshalResponse emits one exact-bound bounded resolver response.
func MarshalResponse(req llm.ProviderRouteRequest, response llm.ResolvedProviderRoute) ([]byte, error) {
	if response.Provider == "" || response.Model == "" || response.KeyName == "" || response.Credential == "" || response.ExpiresAt.IsZero() ||
		response.RouteID != req.RouteID || response.RouteGeneration != req.RouteGeneration ||
		response.ProviderConnectionID != req.ProviderConnectionID ||
		response.ProviderConnectionGeneration != req.ProviderConnectionGeneration ||
		response.CredentialAssetGeneration != req.CredentialAssetGeneration ||
		response.ModelSelector != req.ModelSelector {
		return nil, llm.ErrProviderRouteInvalid
	}
	body, err := json.Marshal(responseWire{
		Version: Version, Provider: response.Provider, Model: response.Model, KeyName: response.KeyName,
		RouteID: response.RouteID, RouteGeneration: response.RouteGeneration,
		ProviderConnectionID:         response.ProviderConnectionID,
		ProviderConnectionGeneration: response.ProviderConnectionGeneration,
		CredentialAssetGeneration:    response.CredentialAssetGeneration,
		ModelSelector:                response.ModelSelector,
		Endpoint:                     marshalEndpoint(response.Endpoint),
		Credential:                   response.Credential, ExpiresAt: response.ExpiresAt.UTC(),
	})
	if err != nil || len(body) > MaxResponseBytes {
		return nil, llm.ErrProviderRouteInvalid
	}
	return body, nil
}

// MarshalSelectionResponse emits one exact-bound credential-free selection.
func MarshalSelectionResponse(req llm.ProviderRouteRequest, selected llm.SelectedProviderRoute) ([]byte, error) {
	if selected.Provider == "" || selected.Model == "" || selected.KeyName == "" || selected.ExpiresAt.IsZero() ||
		selected.RouteID != req.RouteID || selected.RouteGeneration != req.RouteGeneration ||
		selected.ProviderConnectionID != req.ProviderConnectionID ||
		selected.ProviderConnectionGeneration != req.ProviderConnectionGeneration ||
		selected.CredentialAssetGeneration != req.CredentialAssetGeneration || selected.ModelSelector != req.ModelSelector {
		return nil, llm.ErrProviderRouteInvalid
	}
	body, err := json.Marshal(responseWire{
		Version: Version, Provider: selected.Provider, Model: selected.Model, KeyName: selected.KeyName,
		RouteID: selected.RouteID, RouteGeneration: selected.RouteGeneration,
		ProviderConnectionID:         selected.ProviderConnectionID,
		ProviderConnectionGeneration: selected.ProviderConnectionGeneration,
		CredentialAssetGeneration:    selected.CredentialAssetGeneration,
		ModelSelector:                selected.ModelSelector,
		Endpoint:                     marshalEndpoint(selected.Endpoint),
		ExpiresAt:                    selected.ExpiresAt.UTC(),
	})
	if err != nil || len(body) > MaxResponseBytes {
		return nil, llm.ErrProviderRouteInvalid
	}
	return body, nil
}

// ParseResponse strictly parses and exact-binds a resolver response.
func ParseResponse(req llm.ProviderRouteRequest, body []byte) (llm.ResolvedProviderRoute, error) {
	if len(body) == 0 || len(body) > MaxResponseBytes {
		return llm.ResolvedProviderRoute{}, llm.ErrProviderRouteInvalid
	}
	var w responseWire
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&w); err != nil {
		return llm.ResolvedProviderRoute{}, llm.ErrProviderRouteInvalid
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return llm.ResolvedProviderRoute{}, llm.ErrProviderRouteInvalid
	}
	if w.Version != Version || w.Provider == "" || w.Model == "" || w.KeyName == "" || w.Credential == "" ||
		w.RouteID != req.RouteID || w.RouteGeneration != req.RouteGeneration ||
		w.ProviderConnectionID != req.ProviderConnectionID ||
		w.ProviderConnectionGeneration != req.ProviderConnectionGeneration ||
		w.CredentialAssetGeneration != req.CredentialAssetGeneration ||
		w.ModelSelector != req.ModelSelector || w.ExpiresAt.IsZero() {
		return llm.ResolvedProviderRoute{}, llm.ErrProviderRouteInvalid
	}
	endpoint, err := parseEndpoint(w.Endpoint)
	if err != nil {
		return llm.ResolvedProviderRoute{}, err
	}
	return llm.ResolvedProviderRoute{
		Provider: w.Provider, Model: w.Model, KeyName: w.KeyName, RouteID: w.RouteID, RouteGeneration: w.RouteGeneration,
		ProviderConnectionID:         w.ProviderConnectionID,
		ProviderConnectionGeneration: w.ProviderConnectionGeneration,
		CredentialAssetGeneration:    w.CredentialAssetGeneration,
		ModelSelector:                w.ModelSelector,
		Endpoint:                     endpoint,
		ExpiresAt:                    w.ExpiresAt.UTC(), Credential: w.Credential,
	}, nil
}

func marshalEndpoint(endpoint *llm.ProviderEndpointBinding) *endpointWire {
	if endpoint == nil {
		return nil
	}
	return &endpointWire{Kind: string(endpoint.Kind), Value: endpoint.Value, Digest: endpoint.Digest}
}

func parseEndpoint(endpoint *endpointWire) (*llm.ProviderEndpointBinding, error) {
	if endpoint == nil {
		return nil, nil
	}
	normalized, digest, err := llm.NormalizeProviderEndpoint(endpoint.Value)
	if err != nil || normalized != endpoint.Value || digest != endpoint.Digest || endpoint.Kind == "" {
		return nil, llm.ErrProviderRouteInvalid
	}
	return &llm.ProviderEndpointBinding{Kind: llm.ProviderEndpointKind(endpoint.Kind), Value: endpoint.Value, Digest: endpoint.Digest}, nil
}
