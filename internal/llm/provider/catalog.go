// Package provider contains the provider-neutral technical descriptor and
// model-catalog contract. It deliberately owns no presentation metadata and
// never carries credentials or provider response bodies.
package provider

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// SupportState is an explicit fact about a provider operation or model
// capability. Unknown is different from Unsupported: the provider did not
// give Harbor enough information to make the stronger claim.
type SupportState string

const (
	SupportSupported   SupportState = "supported"
	SupportUnsupported SupportState = "unsupported"
	SupportManual      SupportState = "manual"
	SupportPartial     SupportState = "partial"
	SupportUnavailable SupportState = "unavailable"
	SupportUnknown     SupportState = "unknown"
	SupportStale       SupportState = "stale"
	SupportUnpriced    SupportState = "unpriced"
	SupportMalformed   SupportState = "malformed"
)

// CredentialMode is a technical credential family. The actual secret is
// resolved by the runtime and is never part of a descriptor or result.
type CredentialMode string

const (
	CredentialAPIKey CredentialMode = "api_key"
	CredentialNone   CredentialMode = "none"
)

// FieldKind identifies an operator-supplied field without prescribing labels,
// help copy, or other presentation metadata.
type FieldKind string

const (
	FieldSecret FieldKind = "secret"
	FieldURL    FieldKind = "url"
	FieldText   FieldKind = "text"
)

// CredentialField is the technical shape of one provider input. Secret marks
// a value that must never be returned or logged; Required is conditional on
// selecting the descriptor's corresponding credential mode.
type CredentialField struct {
	Name     string    `json:"name"`
	Kind     FieldKind `json:"kind"`
	Required bool      `json:"required"`
	Secret   bool      `json:"secret"`
}

// OperationSupport records whether validation or model discovery is exposed
// by the Harbor/Bifrost integration. It describes capability, not readiness:
// a supported operation may still return an unavailable result for a bad
// credential, endpoint, or provider account.
type OperationSupport struct {
	State         SupportState `json:"state"`
	RuntimeOrigin bool         `json:"runtime_origin"`
	Bounded       bool         `json:"bounded"`
}

// ProviderDescriptor is technical provider metadata suitable for a control
// plane to consume. It intentionally excludes logos, labels, prose, secret
// values, endpoint values, and raw provider payloads.
type ProviderDescriptor struct {
	ID               string            `json:"id"`
	Kind             string            `json:"kind"`
	CredentialModes  []CredentialMode  `json:"credential_modes"`
	CredentialFields []CredentialField `json:"credential_fields"`
	CustomEndpoint   SupportState      `json:"custom_endpoint"`
	Validation       OperationSupport  `json:"validation"`
	Discovery        OperationSupport  `json:"discovery"`
}

// RawModel is the small provider-neutral subset that an adapter may assert
// from a provider SDK. Pointer-valued limits preserve the distinction between
// an explicitly reported zero and an omitted field; both are validated
// fail-closed by NormalizeModels.
type RawModel struct {
	ID                  string
	ContextLength       *int
	MaxInputTokens      *int
	MaxOutputTokens     *int
	InputModalities     []string
	OutputModalities    []string
	SupportedParameters []string
	PricingKnown        bool
	Deprecated          bool
}

// ModelPage is one bounded adapter response. KeyFailures counts failed
// provider-key subrequests without carrying their messages or credentials.
// Stale is only set by a source with an explicit cache-freshness fact; the
// live Bifrost source never fabricates stale data.
type ModelPage struct {
	Models        []RawModel
	NextPageToken string
	KeyFailures   int
	Stale         bool
}

// ModelLister is the narrow adapter seam used by Catalog. Implementations
// must honor ctx and must not expose provider response bodies in returned
// errors. PageSize and PageToken are bounded by Catalog before invocation.
type ModelLister interface {
	ListModels(ctx context.Context, providerID string, pageSize int, pageToken string) (ModelPage, error)
}

// ProviderError is an adapter-safe error classification. It contains no
// provider message or response body, so it is safe to map to a stable result.
type ProviderError struct {
	Code        string
	StatusCode  int
	Unsupported bool
}

func (e *ProviderError) Error() string {
	if e == nil || e.Code == "" {
		return "provider error"
	}
	return sanitizeProviderCode(e.Code)
}

// NewProviderError creates a redacted adapter error. Code is normalized to a
// stable lower-case token by callers and never contains response text.
func NewProviderError(code string, statusCode int, unsupported bool) error {
	return &ProviderError{Code: sanitizeProviderCode(code), StatusCode: statusCode, Unsupported: unsupported}
}

// Outcome is the bounded, content-free result shared by validation and
// discovery. Message is selected from a fixed vocabulary; it is never an
// adapter error string.
type Outcome struct {
	State         SupportState `json:"state"`
	Code          string       `json:"code"`
	Message       string       `json:"message"`
	ObservedAt    time.Time    `json:"observed_at,omitempty"`
	RuntimeOrigin bool         `json:"runtime_origin"`
	Partial       bool         `json:"partial"`
	Stale         bool         `json:"stale"`
}

// NumericCapability is a normalized numeric model fact.
type NumericCapability struct {
	State SupportState `json:"state"`
	Value int          `json:"value,omitempty"`
}

// SetCapability is a normalized finite set of technical values. Values are
// sorted and contain no provider-specific prose.
type SetCapability struct {
	State  SupportState `json:"state"`
	Values []string     `json:"values,omitempty"`
}

// ReasoningCapability uses Harbor's provider-neutral effort vocabulary only
// when the adapter reports a canonical reasoning parameter.
type ReasoningCapability struct {
	State  SupportState `json:"state"`
	Levels []string     `json:"levels,omitempty"`
}

// PricingCapability reports pricing provenance without exposing a rate table.
// A model with no provider-reported pricing is explicitly unpriced.
type PricingCapability struct {
	State  SupportState `json:"state"`
	Source string       `json:"source,omitempty"`
}

// ModelCapabilities is the normalized, provider-neutral model fact set.
type ModelCapabilities struct {
	Context          NumericCapability   `json:"context"`
	MaxInputTokens   NumericCapability   `json:"max_input_tokens"`
	MaxOutputTokens  NumericCapability   `json:"max_output_tokens"`
	InputModalities  SetCapability       `json:"input_modalities"`
	OutputModalities SetCapability       `json:"output_modalities"`
	Tools            SupportState        `json:"tools"`
	Vision           SupportState        `json:"vision"`
	Reasoning        ReasoningCapability `json:"reasoning"`
	Pricing          PricingCapability   `json:"pricing"`
}

// ModelSource says whether a model was observed from the provider or merely
// configured by the operator. Manual entries are never presented as
// discovered facts.
type ModelSource string

const (
	ModelSourceDiscovered ModelSource = "discovered"
	ModelSourceManual     ModelSource = "manual"
)

// Model is a normalized model catalog entry. ID is an opaque provider model
// identifier, not a secret and not a consumer-facing display label.
type Model struct {
	ID           string            `json:"id"`
	Source       ModelSource       `json:"source"`
	Deprecated   bool              `json:"deprecated"`
	Capabilities ModelCapabilities `json:"capabilities"`
}

// ValidationRequest selects one configured provider for a runtime-origin
// validation probe.
type ValidationRequest struct {
	ProviderID string
}

// ValidationResult confirms only the provider operation, not a global account
// or billing state. It is safe to persist as a content-free observation.
type ValidationResult struct {
	ProviderID string  `json:"provider_id"`
	Outcome    Outcome `json:"outcome"`
}

// DiscoveryRequest bounds one runtime-origin model discovery operation.
type DiscoveryRequest struct {
	ProviderID string
	PageSize   int
	MaxPages   int
}

// DiscoveryResult is bounded by MaxPages and contains normalized model facts
// only. A partial/stale result must not be treated as a complete catalog.
type DiscoveryResult struct {
	ProviderID string  `json:"provider_id"`
	Outcome    Outcome `json:"outcome"`
	Models     []Model `json:"models,omitempty"`
	Pages      int     `json:"pages"`
	ModelCount int     `json:"model_count"`
}

const (
	defaultPageSize        = 100
	maxPageSize            = 1000
	defaultMaxPages        = 20
	maxPageTokenLength     = 4096
	maxModelIDLength       = 512
	maxManualModels        = 1000
	maxCapabilityValueSize = 64
	maxCapabilityValueLen  = 128
	maxParameterCount      = 128
	maxParameterLen        = 128
)

// Catalog provides deterministic descriptor, validation, and discovery
// operations over one adapter. It is immutable and safe for concurrent reuse.
type Catalog struct {
	source      ModelLister
	descriptors map[string]ProviderDescriptor
	active      map[string]struct{}
	manual      map[string][]string
}

// NewCatalog constructs an immutable catalog. descriptors must have unique,
// non-empty IDs. active identifies providers actually configured in the
// runtime; descriptors may include the complete static provider registry.
// manual maps configured provider IDs to operator-declared model IDs.
func NewCatalog(source ModelLister, descriptors []ProviderDescriptor, active []string, manual map[string][]string) (*Catalog, error) {
	byID := make(map[string]ProviderDescriptor, len(descriptors))
	for _, descriptor := range descriptors {
		id := strings.TrimSpace(descriptor.ID)
		if id == "" {
			return nil, errors.New("provider descriptor id is empty")
		}
		if _, exists := byID[id]; exists {
			return nil, fmt.Errorf("provider descriptor %q is duplicated", id)
		}
		descriptor.ID = id
		descriptor.CredentialModes = append([]CredentialMode(nil), descriptor.CredentialModes...)
		descriptor.CredentialFields = append([]CredentialField(nil), descriptor.CredentialFields...)
		byID[id] = descriptor
	}
	activeSet := make(map[string]struct{}, len(active))
	for _, id := range active {
		if id = strings.TrimSpace(id); id != "" {
			activeSet[id] = struct{}{}
		}
	}
	manualCopy := make(map[string][]string, len(manual))
	for id, models := range manual {
		if len(models) > maxManualModels {
			return nil, fmt.Errorf("manual model catalog for %q is too large", id)
		}
		seen := make(map[string]struct{}, len(models))
		for _, modelID := range models {
			modelID = strings.TrimSpace(modelID)
			if modelID == "" {
				return nil, fmt.Errorf("manual model id for %q is empty", id)
			}
			if len(modelID) > maxModelIDLength {
				return nil, fmt.Errorf("manual model id for %q is too long", id)
			}
			if _, exists := seen[modelID]; exists {
				return nil, fmt.Errorf("manual model id %q for %q is duplicated", modelID, id)
			}
			seen[modelID] = struct{}{}
			manualCopy[id] = append(manualCopy[id], modelID)
		}
		sort.Strings(manualCopy[id])
	}
	return &Catalog{source: source, descriptors: byID, active: activeSet, manual: manualCopy}, nil
}

// Descriptors returns a stable, defensive copy of all technical descriptors.
func (c *Catalog) Descriptors(_ context.Context) []ProviderDescriptor {
	if c == nil {
		return nil
	}
	ids := make([]string, 0, len(c.descriptors))
	for id := range c.descriptors {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := make([]ProviderDescriptor, 0, len(ids))
	for _, id := range ids {
		d := c.descriptors[id]
		d.CredentialModes = append([]CredentialMode(nil), d.CredentialModes...)
		d.CredentialFields = append([]CredentialField(nil), d.CredentialFields...)
		out = append(out, d)
	}
	return out
}

// Validate performs a bounded runtime-origin probe. Provider or endpoint
// failures are represented as stable outcomes, not returned as raw errors.
func (c *Catalog) Validate(ctx context.Context, req ValidationRequest) ValidationResult {
	providerID := strings.TrimSpace(req.ProviderID)
	result := ValidationResult{ProviderID: providerID}
	if c == nil {
		result.Outcome = fixedOutcome(SupportUnavailable, "provider_catalog_unavailable", "provider catalog is unavailable", false)
		return result
	}
	if err := ctx.Err(); err != nil {
		result.Outcome = contextOutcome(err, true)
		return result
	}
	if _, ok := c.descriptors[providerID]; !ok {
		result.Outcome = fixedOutcome(SupportUnsupported, "provider_unknown", "provider is not in the Harbor provider registry", false)
		return result
	}
	if c.source == nil || !c.isActive(providerID) {
		result.Outcome = fixedOutcome(SupportUnavailable, "provider_not_configured", "provider is not configured for this runtime", false)
		return result
	}
	page, err := c.source.ListModels(ctx, providerID, 1, "")
	if err != nil {
		result.Outcome = errorOutcome(ctx, err)
		result.Outcome.RuntimeOrigin = true
		return result
	}
	if err := validatePageShape(page, 1); err != nil {
		result.Outcome = fixedOutcome(SupportMalformed, "provider_reply_malformed", "provider returned malformed model metadata", true)
		result.Outcome.RuntimeOrigin = true
		return result
	}
	result.Outcome = observedOutcome(page, true)
	if result.Outcome.State == SupportSupported && len(page.Models) == 0 {
		result.Outcome = fixedOutcome(SupportPartial, "provider_empty_catalog", "provider validation succeeded but returned no models", true)
	}
	return result
}

// Discover obtains a bounded, normalized model catalog from the configured
// runtime provider. Operator-configured models are returned as manual only
// when discovery is unsupported, unavailable, or empty; they never masquerade
// as discovered provider facts.
func (c *Catalog) Discover(ctx context.Context, req DiscoveryRequest) (DiscoveryResult, error) {
	providerID := strings.TrimSpace(req.ProviderID)
	result := DiscoveryResult{ProviderID: providerID}
	if c == nil {
		result.Outcome = fixedOutcome(SupportUnavailable, "provider_catalog_unavailable", "provider catalog is unavailable", false)
		return result, nil
	}
	if providerID == "" {
		return result, errors.New("provider id is required")
	}
	descriptor, ok := c.descriptors[providerID]
	if !ok {
		result.Outcome = fixedOutcome(SupportUnsupported, "provider_unknown", "provider is not in the Harbor provider registry", false)
		return result, nil
	}
	if descriptor.Discovery.State == SupportManual {
		result.Models = manualModels(c.manual[providerID])
		result.ModelCount = len(result.Models)
		result.Outcome = fixedOutcome(SupportManual, "model_catalog_manual", "model catalog is operator configured; provider discovery is unavailable", false)
		return result, nil
	}
	if descriptor.Discovery.State == SupportUnsupported {
		result.Outcome = fixedOutcome(SupportUnsupported, "model_discovery_unsupported", "provider does not expose model discovery through Harbor", false)
		return result, nil
	}
	if c.source == nil || !c.isActive(providerID) {
		result.Outcome = fixedOutcome(SupportUnavailable, "provider_not_configured", "provider is not configured for this runtime", false)
		return result, nil
	}
	pageSize := req.PageSize
	if pageSize <= 0 {
		pageSize = defaultPageSize
	}
	if pageSize > maxPageSize {
		pageSize = maxPageSize
	}
	maxPages := req.MaxPages
	if maxPages <= 0 {
		maxPages = defaultMaxPages
	}
	if maxPages > defaultMaxPages {
		maxPages = defaultMaxPages
	}
	var all []Model
	pageToken := ""
	partial, stale := false, false
	for page := 0; page < maxPages; page++ {
		if err := ctx.Err(); err != nil {
			result.Pages = page
			result.Models = nil
			result.ModelCount = 0
			result.Outcome = contextOutcome(err, true)
			return result, nil
		}
		response, err := c.source.ListModels(ctx, providerID, pageSize, pageToken)
		result.Pages++
		if err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				result.Outcome = contextOutcome(ctxErr, true)
				return result, nil
			}
			if len(all) > 0 {
				partial = true
				break
			}
			if manual := manualModels(c.manual[providerID]); len(manual) > 0 {
				result.Models = manual
				result.ModelCount = len(manual)
				result.Outcome = fixedOutcome(SupportManual, "model_catalog_manual_fallback", "runtime discovery was unavailable; operator configured models remain available", true)
				result.Outcome.RuntimeOrigin = true
				return result, nil
			}
			result.Outcome = errorOutcome(ctx, err)
			result.Outcome.RuntimeOrigin = true
			return result, nil
		}
		if err := validatePageShape(response, pageSize); err != nil {
			result.Models = nil
			result.ModelCount = 0
			result.Outcome = fixedOutcome(SupportMalformed, "provider_reply_malformed", "provider returned malformed model metadata", true)
			result.Outcome.RuntimeOrigin = true
			return result, nil
		}
		if response.Stale {
			stale = true
		}
		if response.KeyFailures > 0 {
			partial = true
		}
		normalized, err := NormalizeModels(response.Models)
		if err != nil {
			result.Models = nil
			result.ModelCount = 0
			result.Outcome = fixedOutcome(SupportMalformed, "provider_reply_malformed", "provider returned malformed model metadata", true)
			result.Outcome.RuntimeOrigin = true
			return result, nil
		}
		for i := range normalized {
			normalized[i].Source = ModelSourceDiscovered
		}
		all = append(all, normalized...)
		pageToken = response.NextPageToken
		if pageToken == "" {
			break
		}
	}
	if pageToken != "" {
		partial = true
	}
	if len(all) == 0 && !partial && !stale {
		if manual := manualModels(c.manual[providerID]); len(manual) > 0 {
			result.Models = manual
			result.ModelCount = len(manual)
			result.Outcome = fixedOutcome(SupportManual, "model_catalog_manual_fallback", "runtime discovery returned no models; operator configured models remain available", true)
			result.Outcome.RuntimeOrigin = true
			return result, nil
		}
	}
	// A duplicate may arrive across page boundaries. Reject it rather than
	// silently presenting an ambiguous catalog.
	if err := validateUniqueModels(all); err != nil {
		result.Models = nil
		result.ModelCount = 0
		result.Outcome = fixedOutcome(SupportMalformed, "provider_reply_malformed", "provider returned duplicate model metadata", true)
		result.Outcome.RuntimeOrigin = true
		return result, nil
	}
	result.Models = all
	result.ModelCount = len(all)
	result.Outcome = fixedOutcome(SupportSupported, "model_discovery_complete", "model catalog observed from runtime provider", false)
	result.Outcome.RuntimeOrigin = true
	if stale {
		result.Outcome.State = SupportStale
		result.Outcome.Code = "model_catalog_stale"
		result.Outcome.Message = "model catalog came from a stale provider observation"
		result.Outcome.Stale = true
		result.Outcome.Partial = partial
	} else if partial {
		result.Outcome.State = SupportPartial
		result.Outcome.Code = "model_discovery_partial"
		result.Outcome.Message = "model catalog is partial; not all provider pages or keys were available"
		result.Outcome.Partial = true
	}
	return result, nil
}

// NormalizeModels validates and maps adapter metadata into neutral
// capabilities. It rejects ambiguous malformed rows instead of returning
// plausible-looking partial facts.
func NormalizeModels(raw []RawModel) ([]Model, error) {
	if len(raw) > maxPageSize {
		return nil, errors.New("model page exceeds bounded response shape")
	}
	out := make([]Model, 0, len(raw))
	for _, model := range raw {
		id := strings.TrimSpace(model.ID)
		if id == "" {
			return nil, errors.New("model id is empty")
		}
		if len(id) > maxModelIDLength {
			return nil, errors.New("model id is too long")
		}
		if err := validateModelShape(model); err != nil {
			return nil, err
		}
		for _, value := range []*int{model.ContextLength, model.MaxInputTokens, model.MaxOutputTokens} {
			if value != nil && *value <= 0 {
				return nil, errors.New("model limit is not positive")
			}
		}
		out = append(out, Model{ID: id, Deprecated: model.Deprecated, Capabilities: normalizeCapabilities(model)})
	}
	if err := validateUniqueModels(out); err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func normalizeCapabilities(model RawModel) ModelCapabilities {
	capabilities := ModelCapabilities{
		Context:          numeric(model.ContextLength),
		MaxInputTokens:   numeric(model.MaxInputTokens),
		MaxOutputTokens:  numeric(model.MaxOutputTokens),
		InputModalities:  modalities(model.InputModalities),
		OutputModalities: modalities(model.OutputModalities),
		Tools:            SupportUnknown,
		Vision:           SupportUnknown,
		Reasoning:        ReasoningCapability{State: SupportUnknown},
		Pricing:          PricingCapability{State: SupportUnpriced},
	}
	params := make(map[string]struct{}, len(model.SupportedParameters))
	for _, param := range model.SupportedParameters {
		param = strings.ToLower(strings.TrimSpace(param))
		if param != "" {
			params[param] = struct{}{}
		}
	}
	if hasAny(params, "tools", "tool_choice", "parallel_tool_calls", "function_call") {
		capabilities.Tools = SupportSupported
	}
	if contains(capabilities.InputModalities.Values, "image") {
		capabilities.Vision = SupportSupported
	} else if capabilities.InputModalities.State == SupportSupported {
		capabilities.Vision = SupportUnsupported
	}
	if hasAny(params, "reasoning_effort", "reasoning") {
		capabilities.Reasoning = ReasoningCapability{State: SupportSupported, Levels: []string{"off", "low", "medium", "high"}}
	}
	if model.PricingKnown {
		capabilities.Pricing = PricingCapability{State: SupportSupported, Source: "provider_reported"}
	}
	return capabilities
}

func numeric(value *int) NumericCapability {
	if value == nil {
		return NumericCapability{State: SupportUnknown}
	}
	return NumericCapability{State: SupportSupported, Value: *value}
}

func modalities(values []string) SetCapability {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.ToLower(strings.TrimSpace(value))
		if value == "" {
			continue
		}
		if _, ok := seen[value]; !ok {
			seen[value] = struct{}{}
			out = append(out, value)
		}
	}
	if len(out) == 0 {
		return SetCapability{State: SupportUnknown}
	}
	sort.Strings(out)
	return SetCapability{State: SupportSupported, Values: out}
}

func manualModels(ids []string) []Model {
	out := make([]Model, 0, len(ids))
	for _, id := range ids {
		manual := func() SetCapability { return SetCapability{State: SupportManual} }
		out = append(out, Model{ID: id, Source: ModelSourceManual, Capabilities: ModelCapabilities{
			Context: NumericCapability{State: SupportManual}, MaxInputTokens: NumericCapability{State: SupportManual}, MaxOutputTokens: NumericCapability{State: SupportManual},
			InputModalities: manual(), OutputModalities: manual(), Tools: SupportManual, Vision: SupportManual,
			Reasoning: ReasoningCapability{State: SupportManual}, Pricing: PricingCapability{State: SupportUnpriced},
		}})
	}
	return out
}

func validateUniqueModels(models []Model) error {
	seen := make(map[string]struct{}, len(models))
	for _, model := range models {
		if _, exists := seen[model.ID]; exists {
			return fmt.Errorf("model %q is duplicated", model.ID)
		}
		seen[model.ID] = struct{}{}
	}
	return nil
}

func (c *Catalog) isActive(providerID string) bool {
	_, ok := c.active[providerID]
	return ok
}

func fixedOutcome(state SupportState, code, message string, partial bool) Outcome {
	return Outcome{State: state, Code: code, Message: message, ObservedAt: time.Now().UTC(), Partial: partial}
}

func observedOutcome(page ModelPage, runtimeOrigin bool) Outcome {
	outcome := fixedOutcome(SupportSupported, "provider_reachable", "provider endpoint accepted a bounded validation request", false)
	outcome.RuntimeOrigin = runtimeOrigin
	if page.Stale {
		outcome.State = SupportStale
		outcome.Code = "provider_observation_stale"
		outcome.Message = "provider validation used a stale observation"
		outcome.Stale = true
	}
	if page.KeyFailures > 0 {
		outcome.State = SupportPartial
		outcome.Code = "provider_validation_partial"
		outcome.Message = "provider validation reached only part of the configured account"
		outcome.Partial = true
		if page.Stale {
			outcome.State = SupportStale
			outcome.Code = "provider_observation_stale"
			outcome.Message = "provider validation used a stale observation"
		}
	}
	return outcome
}

func contextOutcome(err error, runtimeOrigin bool) Outcome {
	state, code, message := SupportUnavailable, "provider_unavailable", "provider validation was cancelled"
	if errors.Is(err, context.DeadlineExceeded) {
		code, message = "provider_timeout", "provider validation timed out"
	}
	outcome := fixedOutcome(state, code, message, false)
	outcome.RuntimeOrigin = runtimeOrigin
	return outcome
}

func errorOutcome(ctx context.Context, err error) Outcome {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return contextOutcome(ctxErr, true)
	}
	var providerErr *ProviderError
	if errors.As(err, &providerErr) {
		if providerErr.Unsupported {
			return fixedOutcome(SupportUnsupported, "model_discovery_unsupported", "provider does not support the requested Harbor operation", false)
		}
		switch providerErr.StatusCode {
		case 401, 403:
			return fixedOutcome(SupportUnavailable, "provider_credential_rejected", "provider rejected the runtime credential", false)
		case 404:
			return fixedOutcome(SupportUnavailable, "provider_endpoint_unavailable", "provider endpoint was not found", false)
		case 429:
			return fixedOutcome(SupportUnavailable, "provider_rate_limited", "provider rate-limited the runtime validation request", false)
		}
	}
	return fixedOutcome(SupportUnavailable, "provider_unavailable", "provider validation failed without a safe provider-specific diagnosis", false)
}

func validatePageShape(page ModelPage, pageSize int) error {
	if page.KeyFailures < 0 || page.KeyFailures > pageSize || len(page.Models) > pageSize || len(page.NextPageToken) > maxPageTokenLength {
		return errors.New("provider page exceeds bounded response shape")
	}
	return nil
}

func validateModelShape(model RawModel) error {
	if len(model.InputModalities) > maxCapabilityValueSize || len(model.OutputModalities) > maxCapabilityValueSize {
		return errors.New("model modality list is too large")
	}
	for _, value := range append(append([]string(nil), model.InputModalities...), model.OutputModalities...) {
		if len(strings.TrimSpace(value)) > maxCapabilityValueLen {
			return errors.New("model modality value is too long")
		}
	}
	if len(model.SupportedParameters) > maxParameterCount {
		return errors.New("model parameter list is too large")
	}
	for _, value := range model.SupportedParameters {
		if len(strings.TrimSpace(value)) > maxParameterLen {
			return errors.New("model parameter value is too long")
		}
	}
	return nil
}

func sanitizeProviderCode(code string) string {
	code = strings.TrimSpace(strings.ToLower(code))
	if code == "" || len(code) > 64 {
		return "provider_unavailable"
	}
	for i := 0; i < len(code); i++ {
		if (code[i] < 'a' || code[i] > 'z') && (code[i] < '0' || code[i] > '9') && code[i] != '_' && code[i] != '-' {
			return "provider_unavailable"
		}
	}
	return code
}

func hasAny(set map[string]struct{}, values ...string) bool {
	for _, value := range values {
		if _, ok := set[value]; ok {
			return true
		}
	}
	return false
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
