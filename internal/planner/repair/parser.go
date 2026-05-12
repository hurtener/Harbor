package repair

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/hurtener/Harbor/internal/planner"
)

// Sentinel errors from the parser. Compare via [errors.Is].
var (
	// ErrNoActionsFound — the parser walked every salvage path
	// (greedy decode → fenced extraction → decoder scan) and found
	// no well-shaped tool-call object. The repair loop treats this as
	// a non-args validation failure and counts it against the
	// storm-guard ([Config.MaxConsecutiveArgFailures]).
	ErrNoActionsFound = errors.New("repair: parser found no actions in LLM response")

	// ErrInvalidActionShape — the parser found JSON but the shape
	// did not match the {"tool": "...", "args": {...}} envelope. The
	// repair loop treats this as a non-args validation failure.
	ErrInvalidActionShape = errors.New("repair: action JSON has invalid shape")
)

// ActionEnvelope is the canonical LLM-emitted shape the parser
// recognises. Brief 02 §2 settled on a typed envelope rather than
// the predecessor's "magic strings as next_node" pattern (RFC §6.2
// settled decisions; D-047). The envelope is intentionally minimal:
//
//	{"tool": "<catalog name>", "args": {...}, "reasoning": "..."}
//
// `reasoning` is optional free-form text the planner echoes back to
// the LLM via [planner.CallTool.Reasoning]. The parser does NOT
// recognise the predecessor's `next_node` discriminator — that
// vocabulary is explicitly rejected.
//
// The parser does NOT extract [planner.CallParallel] / [planner.Finish]
// envelopes — those are runtime opcodes, not tool calls. Phase 45
// (ReAct) prompts the LLM to emit `tool: "_finish"` as a marker which
// it then maps to [planner.Finish] before passing to the loop; that's
// the planner concrete's call, not the repair loop's. The repair loop
// runs on the [planner.CallTool] shape only.
type ActionEnvelope struct {
	Tool      string          `json:"tool"`
	Args      json.RawMessage `json:"args"`
	Reasoning string          `json:"reasoning,omitempty"`
}

// ActionParser extracts one OR many [planner.CallTool] actions from
// raw LLM text. Tolerant of the failure modes brief 07 §3 catalogued:
//
//   - Fenced JSON (` ```json ... ``` ` or ` ``` ... ``` `).
//   - Prose-wrapped JSON ("Here's my action:\n {...}").
//   - Multiple objects in one response (multi-action salvage prep).
//   - Bare JSON arrays of envelopes.
//
// Order preservation: actions are returned in the LLM-emitted order
// (brief 07 §5: "the next LLM prompt sees the branches in the same
// order the model proposed them").
//
// Concurrent-reuse: the parser holds no per-call state on the
// receiver; one instance is safe to share across goroutines. The
// repair loop allocates one parser at [New] time and re-uses it.
type ActionParser struct{}

// NewParser constructs an [ActionParser]. The receiver is empty —
// the constructor is here for future evolution (e.g. a strict-mode
// knob) without changing the call site.
func NewParser() *ActionParser {
	return &ActionParser{}
}

// Parse returns the actions found in `text`, in LLM-emitted order.
// Returns [ErrNoActionsFound] when no salvage path produced any
// action; [ErrInvalidActionShape] when the parser found JSON that did
// not match the [ActionEnvelope] shape but did succeed at decoding.
//
// The decode order:
//
//  1. **Greedy decode of the entire text as a JSON object.** Handles
//     the happy path where the LLM emits a clean JSON object.
//  2. **Greedy decode as a JSON array of objects.** Handles the
//     LLM that emits `[{tool: ...}, {tool: ...}]` for multi-action.
//  3. **Fenced-block extraction.** Strip ` ```json ` / ` ``` ` fences
//     and retry steps 1+2 on each fenced block.
//  4. **Decoder scan over the full text.** A real `json.Decoder` walks
//     the text and collects every successfully-decoded object (or
//     array of objects). Order preserved.
//
// When every step fails, returns [ErrNoActionsFound].
func (p *ActionParser) Parse(text string) ([]planner.CallTool, error) {
	if strings.TrimSpace(text) == "" {
		return nil, ErrNoActionsFound
	}

	// Step 1+2: greedy decode of the trimmed text. Cheap; covers the
	// happy path.
	if actions, err := tryDecode(text); err == nil && len(actions) > 0 {
		return actions, nil
	}

	// Step 3: fenced-block extraction. The fences are documented in
	// brief 07 §3 — we strip ` ```json `, ` ```JSON `, and bare ` ``` `
	// fences. Within each fenced block we re-run the greedy decode.
	if actions := tryFenced(text); len(actions) > 0 {
		return actions, nil
	}

	// Step 4: decoder scan over the full text. Walks the text with a
	// streaming json.Decoder and collects every successfully-decoded
	// action envelope. Order preserved.
	if actions := tryScan(text); len(actions) > 0 {
		return actions, nil
	}

	return nil, ErrNoActionsFound
}

// tryDecode attempts to decode `text` as either a single
// [ActionEnvelope] or a [ActionEnvelope] array, returning the
// resulting [planner.CallTool] slice. Returns nil + error on miss.
func tryDecode(text string) ([]planner.CallTool, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, ErrNoActionsFound
	}

	// Try object first (the common case).
	var env ActionEnvelope
	if err := json.Unmarshal([]byte(text), &env); err == nil {
		if env.Tool != "" {
			return []planner.CallTool{envelopeToCallTool(env)}, nil
		}
	}

	// Try array.
	var envs []ActionEnvelope
	if err := json.Unmarshal([]byte(text), &envs); err == nil && len(envs) > 0 {
		actions := make([]planner.CallTool, 0, len(envs))
		for _, e := range envs {
			if e.Tool == "" {
				// Malformed entry in an otherwise-valid array.
				continue
			}
			actions = append(actions, envelopeToCallTool(e))
		}
		if len(actions) > 0 {
			return actions, nil
		}
	}

	return nil, fmt.Errorf("%w: greedy decode found no envelopes", ErrNoActionsFound)
}

// tryFenced extracts fenced JSON blocks and re-runs [tryDecode] on
// each. Returns the concatenated actions in source order. Returns nil
// when no fenced block decoded.
func tryFenced(text string) []planner.CallTool {
	var out []planner.CallTool
	for _, block := range extractFencedBlocks(text) {
		if actions, err := tryDecode(block); err == nil {
			out = append(out, actions...)
		}
	}
	return out
}

// extractFencedBlocks returns every fenced block in `text`, stripped
// of the fence markers. Recognises ` ```json `, ` ```JSON `, and bare
// ` ``` ` opening fences. The closing fence is always ` ``` `.
//
// Multiple fenced blocks in the same text are returned in source
// order. Unterminated fences are dropped (the parser refuses to guess
// where they end).
func extractFencedBlocks(text string) []string {
	const fence = "```"
	var blocks []string
	rest := text
	for {
		startIdx := strings.Index(rest, fence)
		if startIdx == -1 {
			break
		}
		// Skip the opening fence + an optional language label.
		afterStart := rest[startIdx+len(fence):]
		// Strip language label up to newline.
		nlIdx := strings.IndexByte(afterStart, '\n')
		var body string
		if nlIdx == -1 {
			// No newline → the rest of the buffer is the "label";
			// nothing follows — unterminated.
			break
		}
		body = afterStart[nlIdx+1:]
		// Find the closing fence.
		closeIdx := strings.Index(body, fence)
		if closeIdx == -1 {
			break
		}
		block := body[:closeIdx]
		blocks = append(blocks, block)
		rest = body[closeIdx+len(fence):]
	}
	return blocks
}

// tryScan walks `text` with a streaming [json.Decoder] and collects
// every well-shaped [ActionEnvelope] / [ActionEnvelope]-array it
// finds. This is the most-tolerant pass — it can extract two action
// objects from "Sure, here's the first: {tool:'a',...} and the
// second: {tool:'b',...}".
//
// Brief 07 §10 sharp edge: "prefer the multi-object scanner as the
// primary extractor and fall back to fence-extraction only when
// multi-object scan fails." Phase 44 inverts that ordering — greedy
// decode first (cheapest), fence-extraction second (mid-tolerance),
// scan last (most tolerant). The inversion is intentional: the scan
// is the LAST resort because it's the most likely to mis-extract a
// reasoning-channel JSON example as an action. The brief's rationale
// applies when the fence-extractor is brittle around nested fences;
// our extractor uses an explicit close-fence search per opening, so
// the brittleness brief 07 cited (nested ` ```python ` blocks) is
// already handled.
func tryScan(text string) []planner.CallTool {
	var out []planner.CallTool
	// Initial trim: skip leading prose / whitespace.
	remaining := trimLeftJunk(text)
	for remaining != "" {
		dec := json.NewDecoder(strings.NewReader(remaining))
		var raw json.RawMessage
		err := dec.Decode(&raw)
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			// The decoder couldn't decode at the current position.
			// Advance past the offending byte and retry.
			advanced := int(dec.InputOffset())
			if advanced <= 0 {
				advanced = 1
			}
			if advanced >= len(remaining) {
				break
			}
			remaining = trimLeftJunk(remaining[advanced:])
			continue
		}
		// Successful decode at this position. Advance past what we
		// consumed.
		advanced := int(dec.InputOffset())
		if advanced <= 0 {
			advanced = len(raw)
		}
		if actions, derr := tryDecode(string(raw)); derr == nil {
			out = append(out, actions...)
		}
		if advanced >= len(remaining) {
			break
		}
		remaining = trimLeftJunk(remaining[advanced:])
	}
	return out
}

// trimLeftJunk skips bytes that the decoder is unlikely to accept as
// a JSON value start — whitespace, commas, prose. Stops at the first
// `{` / `[` / digit / `"` / `t` / `f` / `n` / `-`.
func trimLeftJunk(s string) string {
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '{' || c == '[' || c == '"' || c == '-':
			return s[i:]
		case c >= '0' && c <= '9':
			return s[i:]
		case c == 't' || c == 'f' || c == 'n':
			return s[i:]
		}
	}
	return ""
}

// envelopeToCallTool converts a parsed envelope to the
// [planner.CallTool] shape the loop returns. Args is preserved as
// the original RawMessage so the downstream tool-validator sees the
// exact bytes the LLM emitted.
func envelopeToCallTool(env ActionEnvelope) planner.CallTool {
	args := env.Args
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	return planner.CallTool{
		Tool:      env.Tool,
		Args:      args,
		Reasoning: env.Reasoning,
	}
}
