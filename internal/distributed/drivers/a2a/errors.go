package a2a

import "errors"

// Sentinel errors. Callers compare via errors.Is.
var (
	// ErrInsecureScheme — a peer URL uses http:// but its host is not
	// loopback and `AllowInsecureLoopback` is not set. AGENTS.md §7
	// requires HTTPS for non-localhost peers.
	ErrInsecureScheme = errors.New("a2a: insecure HTTP scheme; require HTTPS or loopback")

	// ErrPeerNotAllowed — a call targeted a peer URL that is not
	// registered with the driver. The driver enforces an explicit
	// allowlist; "discover-on-call" is intentionally not supported.
	ErrPeerNotAllowed = errors.New("a2a: peer URL is not in the registered allowlist")

	// ErrNoJSONRPCInterface — the discovered AgentCard declares no
	// AgentInterface with `ProtocolBinding == "JSONRPC"`. Phase 29 only
	// implements the JSON-RPC binding; HTTP+JSON and gRPC bindings on
	// the same card are read-only metadata.
	ErrNoJSONRPCInterface = errors.New("a2a: AgentCard exposes no JSONRPC interface")

	// ErrAgentCardSchemaInvalid — the fetched AgentCard JSON failed to
	// parse against the Phase 22 Go shapes.
	ErrAgentCardSchemaInvalid = errors.New("a2a: AgentCard schema invalid")

	// ErrJSONRPCError — the peer returned a JSON-RPC error envelope.
	// The wrapping `*jsonRPCError` carries `code` + `message` + `data`.
	ErrJSONRPCError = errors.New("a2a: JSON-RPC error returned by peer")

	// ErrSSEStreamMalformed — an SSE frame did not parse to an
	// `a2a.StreamResponse`. The wrapped detail names the offending
	// frame number.
	ErrSSEStreamMalformed = errors.New("a2a: SSE stream malformed")

	// ErrSSELineTooLong — an SSE line exceeded sseMaxLineBytes. A
	// hostile peer streaming an unterminated line cannot force
	// unbounded buffer growth; the parser bails loudly instead.
	ErrSSELineTooLong = errors.New("a2a: SSE line exceeds maximum length")

	// ErrInvalidPeerURL — a configured peer URL did not parse.
	ErrInvalidPeerURL = errors.New("a2a: invalid peer URL")
)
