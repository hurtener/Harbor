package output

import "encoding/json"

// respondWithEnvelope mirrors the JSON shape `renderToolsEnvelopeInstruction`
// asks the model to emit under [llm.OutputModeTools]:
// `{"name":"respond_with","arguments":<args>}`. Unexported — callers use
// [ParseRespondWith], never this type directly.
type respondWithEnvelope struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// respondWithToolName is the fixed `name` field value the
// `OutputModeTools` prompted envelope instructs the model to emit.
const respondWithToolName = "respond_with"

// ParseRespondWith is the READ half of the `OutputModeTools`
// structured-output envelope (the write half is
// `renderToolsEnvelopeInstruction`). It attempts to unwrap content as a
// `{"name":"respond_with","arguments":<args>}` JSON object and, on a
// match, returns the nested `arguments` payload verbatim (its own raw
// bytes — no lossy re-encode) with ok=true.
//
// Returns ok=false — never an error — when content does not parse as
// the envelope shape (malformed JSON, a JSON value that isn't an
// object, a `name` field other than "respond_with", or a missing
// `arguments` field): the caller's contract is "unwrap when possible,
// pass through unchanged otherwise", so a non-envelope response (the
// common case for Native/Prompted profiles, or a model that ignored the
// Tools-mode instruction) is not an error condition here — the caller
// decides what to do with the original content.
//
// This closes the seam `shapeRequestForMode`'s OutputModeTools branch
// opened one-sided: the downgrade layer instructs the model to emit the
// envelope, but nothing parsed it back until this function existed,
// which meant a Tools-mode profile's schema Validator (and the terminal
// Finish payload) saw the ENVELOPE `{"name":...,"arguments":...}`
// instead of the caller's schema-shaped `arguments` — guaranteed
// validation failure even against a perfectly schema-compliant model.
func ParseRespondWith(content string) (json.RawMessage, bool) {
	if content == "" {
		return nil, false
	}
	var env respondWithEnvelope
	if err := json.Unmarshal([]byte(content), &env); err != nil {
		return nil, false
	}
	if env.Name != respondWithToolName {
		return nil, false
	}
	if len(env.Arguments) == 0 {
		return nil, false
	}
	return env.Arguments, true
}
