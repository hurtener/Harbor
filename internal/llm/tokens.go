package llm

import (
	"encoding/json"
	"fmt"
	"math"
)

// EstimateRequestTokens returns Harbor's canonical token-count estimate for an assembled
// CompleteRequest. The default algorithm (`chars_div_4`
// §2) is `len(text) / 4 + 1` per text fragment plus a 4-token
// per-message overhead for role + structural framing.
//
// ModelProfile.TokenEstimator selects the algorithm; an unknown
// estimator name falls back to chars_div_4 (this is a defensive
// fallback — config validation should already reject unknown
// estimators, but at the runtime edge we prefer "estimate
// conservatively" over "fail unnecessarily").
//
// Multimodal parts:
//   - PartText: estimate the text content directly.
//   - PartImage / PartAudio / PartFile: 256 tokens (a rough provider-
//     side overhead for vision tokenization). This is deliberately
//     coarse — the safety net's job is to fail when CLEARLY over
//     budget, not to perfectly predict the provider's cost. An
//     under-estimate gives the planner a chance to react; a
//     materialize-rewritten ArtifactStub is small (the JSON shape is
//     well under 200 tokens) so the safety pass rarely fires on
//     legitimately-bounded multimodal requests.
//
// Native tool declarations, historical call arguments and correlation IDs
// contribute to input too. Output reservations are intentionally separate:
// callers must subtract output headroom from the model window, not bill it
// as already consumed input.
//
// Response-format JSON schemas contribute to the prompt — schemas
// over a few hundred tokens are real. the downgrade chain
// will hand-balance schema size vs prompt size; estimates
// the bytes-rendered schema at chars/4.
//
// Callers that reserve a provider-attempt allowance must use this function
// rather than maintaining a second prompt estimator. The result remains an
// estimate: provider tokenizers are authoritative, so settlement must record
// the provider's actual usage even when it exceeds the pre-call estimate.
func EstimateRequestTokens(req CompleteRequest, profile ModelProfile) int {
	return EstimateRequestTokenSections(req, profile).Total()
}

// RequestTokenSections partitions the canonical input estimate by structural
// category. It contains counts only, not content. Output reservations are not
// input tokens. These categories do not infer semantic prompt-section boundaries.
type RequestTokenSections struct {
	Text    int
	Tools   int
	Calls   int
	Schema  int
	Media   int
	Framing int
	Other   int
}

// Total returns the exact sum used by request admission.
func (s RequestTokenSections) Total() int {
	return s.Text + s.Tools + s.Calls + s.Schema + s.Media + s.Framing + s.Other
}

// EstimateRequestTokenSections uses the same algorithm as EstimateRequestTokens.
// Unknown estimator names retain the existing chars_div_4 fallback; no separate
// diagnostic estimator or provider tokenizer is introduced.
func EstimateRequestTokenSections(req CompleteRequest, profile ModelProfile) RequestTokenSections {
	return chars4Sections(req)
}

const (
	// messageRoleOverhead is the per-message structural cost
	// (role token, delimiters). Calibrated against the memory design's
	// reference; deliberately conservative on the "more tokens"
	// side.
	messageRoleOverhead = 4
	// multimodalPartOverhead — coarse vision/audio tokenization
	// estimate. Under-counts by design; the safety net's job is
	// to catch egregious cases, not to be a tokenizer.
	multimodalPartOverhead = 256
)

func chars4Sections(req CompleteRequest) RequestTokenSections {
	var sections RequestTokenSections
	for _, m := range req.Messages {
		sections.Framing += messageRoleOverhead
		switch {
		case m.Content.Text != nil:
			sections.Text += len(*m.Content.Text)/4 + 1
		case m.Content.Parts != nil:
			for _, p := range m.Content.Parts {
				switch p.Type {
				case PartText:
					sections.Text += len(p.Text)/4 + 1
				case PartImage, PartAudio, PartFile:
					sections.Media += multimodalPartOverhead
				}
			}
		}
		if m.Name != nil {
			sections.Framing += len(*m.Name)/4 + 1
		}
		if m.ToolCallID != nil {
			sections.Calls += len(*m.ToolCallID)/4 + 1
		}
		for _, call := range m.ToolCalls {
			sections.Framing += messageRoleOverhead
			sections.Calls += len(call.ID)/4 + 1
			sections.Calls += len(call.Name)/4 + 1
			sections.Calls += len(call.Args)/4 + 1
		}
	}
	for _, tool := range req.Tools {
		sections.Framing += messageRoleOverhead
		sections.Tools += len(tool.Name)/4 + 1
		sections.Tools += len(tool.Description)/4 + 1
		sections.Tools += len(tool.Schema)/4 + 1
	}
	if req.ToolChoice != "" {
		sections.Framing += len(req.ToolChoice)/4 + 1
	}
	if req.ResponseFormat != nil && len(req.ResponseFormat.JSONSchema) > 0 {
		sections.Schema += len(req.ResponseFormat.JSONSchema)/4 + 1
	}
	for _, s := range req.Stops {
		sections.Other += len(s)/4 + 1
	}
	if len(req.Extra) > 0 {
		if b, err := json.Marshal(req.Extra); err == nil {
			sections.Other += len(b)/4 + 1
		}
	}
	return sections
}

// requestInputLimit is the one capacity calculation used by request admission.
// The limit is exclusive, preserving the existing reserve-boundary check. An
// unspecified output bound stays unknown (zero here), not an invented provider
// default. Explicit or profile-default output bounds must be positive. Reasoning
// that shares the output allowance is not reserved a second time.
func requestInputLimit(req CompleteRequest, profile ModelProfile, reserve float64) (int, int, error) {
	if profile.ContextWindowTokens <= 0 || math.IsNaN(reserve) || math.IsInf(reserve, 0) || reserve < 0 || reserve >= 1 {
		return 0, 0, fmt.Errorf("%w: invalid context capacity or reserve", ErrInvalidConfig)
	}
	output := req.MaxTokens
	if output == nil {
		output = profile.DefaultMaxTokens
	}
	reserved := 0
	if output != nil {
		if *output <= 0 {
			return 0, 0, fmt.Errorf("%w: output-token allowance must be positive", ErrInvalidConfig)
		}
		reserved = *output
	}
	// Convert only the margin, which is strictly below the integer window.
	// Converting the entire float64 window can overflow at the int boundary.
	roundedMargin := math.Ceil(float64(profile.ContextWindowTokens) * reserve)
	if roundedMargin >= float64(profile.ContextWindowTokens) {
		return 0, reserved, nil
	}
	margin := int(roundedMargin)
	capacity := profile.ContextWindowTokens - margin
	if reserved >= capacity {
		return 0, reserved, nil
	}
	return capacity - reserved, reserved, nil
}
