package builtin

import "context"

// TextEchoArgs is the input shape for `text.echo`. Both fields are
// emitted in the schema; `text` is the payload, `tag` is an optional
// caller-supplied label that round-trips alongside the echo so a
// planner that fans out several echo calls in parallel can tell
// them apart on the return.
type TextEchoArgs struct {
	Text string `json:"text"`
	Tag  string `json:"tag,omitempty"`
}

// TextEchoOut is the result shape for `text.echo`.
type TextEchoOut struct {
	Echoed string `json:"echoed"`
	Tag    string `json:"tag,omitempty"`
}

// TextEcho returns the input text verbatim. Useful for smoke-testing
// the planner → executor → trajectory loop without an external
// dependency, and as a deterministic stand-in when authoring an
// agent before its real tools are wired.
func TextEcho(_ context.Context, in TextEchoArgs) (TextEchoOut, error) {
	return TextEchoOut{Echoed: in.Text, Tag: in.Tag}, nil
}
