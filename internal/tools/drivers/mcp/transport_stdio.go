package mcp

import (
	"fmt"
	"os/exec"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// newStdioTransport builds an mcpsdk.CommandTransport from cfg.
// Command MUST be argv form: [0] is the binary, [1:] are arguments.
// Shell-form invocation (`sh -c "..."`) is FORBIDDEN per AGENTS.md
// §7 — argv-form means the kernel sees the program name + each
// argument as separate strings, and no shell ever interprets the
// command line.
//
// Returned Transport is not connected; the SDK's Connect spawns
// the subprocess.
func newStdioTransport(cfg Config) (mcpsdk.Transport, MCPTransportMode, error) {
	if len(cfg.Command) == 0 {
		return nil, "", fmt.Errorf("%w: stdio transport requires Command", ErrInvalidConfig)
	}
	if cfg.Command[0] == "" {
		return nil, "", fmt.Errorf("%w: stdio transport Command[0] (binary) is empty", ErrInvalidConfig)
	}
	// `exec.Command(name, args...)` — argv form ONLY. Never `sh -c`.
	// The kernel sees `name` + each arg as a separate string;
	// nothing is shell-evaluated.
	//
	// This is the only place in the package that calls exec.Command.
	// AGENTS.md §7 + §13 audit grep targets this seam.
	cmd := exec.Command(cfg.Command[0], cfg.Command[1:]...) //nolint:gosec // argv-form only; no shell, per AGENTS.md §7
	t := &mcpsdk.CommandTransport{
		Command: cmd,
	}
	return t, TransportStdio, nil
}
