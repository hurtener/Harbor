package mcp

// safeDownstreamCause preserves a typed downstream cause for errors.Is/As while
// keeping SDK error text out of tool results and logs. HTTP client errors often
// include the full endpoint URL, including credential-bearing query strings.
func safeDownstreamCause(err error) error {
	if err == nil {
		return nil
	}
	return downstreamCause{cause: err}
}

type downstreamCause struct{ cause error }

func (e downstreamCause) Error() string { return "downstream MCP request failed" }

func (e downstreamCause) Unwrap() error { return e.cause }

// ambiguousToolCall is permanent because the server may have committed the
// tool action before Harbor learned that the session receipt was missing.
type ambiguousToolCall struct{ cause error }

func (e ambiguousToolCall) Error() string { return "downstream MCP tool call outcome is unknown" }

func (e ambiguousToolCall) Unwrap() error { return e.cause }

func (e ambiguousToolCall) Permanent() bool { return true }
