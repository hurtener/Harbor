// Package providerroute exposes Harbor's canonical, provider-neutral external
// route-resolution exchange to coordinators and runtime hosts.
package providerroute

import (
	"github.com/hurtener/Harbor/internal/llm"
	internal "github.com/hurtener/Harbor/internal/llm/providerroute"
)

type Request = llm.ProviderRouteRequest

// Purpose binds a resolver request to its trusted runtime use.
type Purpose = llm.ProviderRoutePurpose
type Response = llm.ResolvedProviderRoute
type SelectedResponse = llm.SelectedProviderRoute
type SelectedProviderRoute = llm.SelectedProviderRoute
type EndpointBinding = llm.ProviderEndpointBinding
type EndpointKind = llm.ProviderEndpointKind

const (
	// PurposeRun is the admitted Bifrost execution purpose.
	PurposeRun = llm.ProviderRoutePurposeRun
	// PurposePosture is the admin provider validation purpose.
	PurposePosture = llm.ProviderRoutePurposePosture

	EndpointAzure            = llm.ProviderEndpointAzure
	EndpointVLLM             = llm.ProviderEndpointVLLM
	EndpointOllama           = llm.ProviderEndpointOllama
	EndpointSGL              = llm.ProviderEndpointSGL
	EndpointOpenAICompatible = llm.ProviderEndpointOpenAICompatible
)

const (
	Version          = internal.Version
	MaxRequestBytes  = internal.MaxRequestBytes
	MaxResponseBytes = internal.MaxResponseBytes
	OperationSelect  = internal.OperationSelect
	OperationResolve = internal.OperationResolve
)

var (
	MarshalRequest            = internal.MarshalRequest
	MarshalSelectionRequest   = internal.MarshalSelectionRequest
	UnmarshalRequest          = internal.UnmarshalRequest
	UnmarshalOperationRequest = internal.UnmarshalOperationRequest
	MarshalResponse           = internal.MarshalResponse
	MarshalSelectionResponse  = internal.MarshalSelectionResponse
	ParseResponse             = internal.ParseResponse
	ParseSelectionResponse    = internal.ParseSelectionResponse
	NormalizeEndpoint         = llm.NormalizeProviderEndpoint
)
