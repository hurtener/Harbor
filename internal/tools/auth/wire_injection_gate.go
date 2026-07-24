// wire_injection_gate.go — the process-global boot capture of the dev-only,
// fail-closed opt-in that permits a per-user credential-INJECTION mapping for a
// receiver-style MCP server to be carried over the wire
// (`agent_config.add_mcp_connection`'s `injection` object).
//
// The effective posture is (the `tools.allow_wire_injection` config flag) OR
// (the `HARBOR_ALLOW_WIRE_INJECTION` boot env captured here). Both default
// false. This file owns ONLY the boot-env half: cmd/harbor captures the env ONCE
// at process start (write-once atomic, reciprocal with a
// [DEV-ONLY WIRE INJECTION — DO NOT USE IN PRODUCTION] stderr banner), and the
// serve wiring reads it, ORs it with the config flag, and threads the effective
// posture into the agent-config service's wire-injection gate.
//
// It is a PARALLEL, INDEPENDENT opt-in from the wire-OAuth-descriptor gate
// (wire_descriptor_gate.go): an operator may enable one without the other. It is
// dev-only, fail-closed, and boot-only; never Protocol-writable and never
// derived from a request. The default (never captured, or captured false) leaves
// the gate CLOSED — a connection carrying any injection field is rejected.

package auth

import "sync/atomic"

// allowWireInjection holds the boot-captured env half of the wire-injection
// opt-in. Default false (unset ⇒ gate closed).
var allowWireInjection atomic.Bool

// RegisterAllowWireInjectionCaptured records, ONCE at boot, whether the
// `HARBOR_ALLOW_WIRE_INJECTION` dev escape hatch fired. cmd/harbor calls it
// (unconditionally, with the honest boot value) at the same call site that emits
// the dev banner, keeping the capture and the banner structurally reciprocal.
// Calling it with false (or never calling it — the zero value) leaves the gate
// closed. It is dev-only, fail-closed, and boot-only; never Protocol-writable.
func RegisterAllowWireInjectionCaptured(v bool) {
	allowWireInjection.Store(v)
}

// AllowWireInjectionCaptured reports whether the boot-captured wire-injection
// escape hatch is active. Read at serve wiring time and ORed with the
// `tools.allow_wire_injection` config flag to compute the effective posture
// threaded into the agent-config service.
func AllowWireInjectionCaptured() bool {
	return allowWireInjection.Load()
}
