package llm

import (
	"encoding/json"
)

// estimateTokens returns a token-count estimate for an assembled
// CompleteRequest. The default algorithm (`chars_div_4` per brief 04
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
// Response-format JSON schemas contribute to the prompt — schemas
// over a few hundred tokens are real. Phase 35's downgrade chain
// will hand-balance schema size vs prompt size; Phase 32 estimates
// the bytes-rendered schema at chars/4.
func estimateTokens(req CompleteRequest, profile ModelProfile) int {
	if profile.TokenEstimator == "" || profile.TokenEstimator == "chars_div_4" {
		return chars4Estimator(req)
	}
	// Unknown estimator: conservative fallback to chars/4. Config
	// validation should have caught this; if it didn't, we'd rather
	// estimate than fail.
	return chars4Estimator(req)
}

const (
	// messageRoleOverhead is the per-message structural cost
	// (role token, delimiters). Calibrated against brief 04 §2's
	// reference; deliberately conservative on the "more tokens"
	// side.
	messageRoleOverhead = 4
	// multimodalPartOverhead — coarse vision/audio tokenization
	// estimate. Under-counts by design; the safety net's job is
	// to catch egregious cases, not to be a tokenizer.
	multimodalPartOverhead = 256
)

func chars4Estimator(req CompleteRequest) int {
	total := 0
	for _, m := range req.Messages {
		total += messageRoleOverhead
		switch {
		case m.Content.Text != nil:
			total += len(*m.Content.Text)/4 + 1
		case m.Content.Parts != nil:
			for _, p := range m.Content.Parts {
				switch p.Type {
				case PartText:
					total += len(p.Text)/4 + 1
				case PartImage, PartAudio, PartFile:
					total += multimodalPartOverhead
				}
			}
		}
		if m.Name != nil {
			total += len(*m.Name)/4 + 1
		}
	}
	// Response-format schema contribution.
	if req.ResponseFormat != nil && len(req.ResponseFormat.JSONSchema) > 0 {
		total += len(req.ResponseFormat.JSONSchema)/4 + 1
	}
	// Stops list — operator-supplied stop sequences contribute.
	for _, s := range req.Stops {
		total += len(s)/4 + 1
	}
	// Extra is opaque-passthrough; estimate by JSON-encoded size.
	if len(req.Extra) > 0 {
		if b, err := json.Marshal(req.Extra); err == nil {
			total += len(b)/4 + 1
		}
	}
	return total
}
