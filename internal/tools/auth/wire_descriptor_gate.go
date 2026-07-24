// wire_descriptor_gate.go — the process-global boot capture of the dev-only,
// fail-closed opt-in that permits a FULL OAuth-provider binding to be carried
// over the wire (`agent_config.set_oauth_provider` / `add_mcp_connection`).
//
// The effective posture is (the `tools.allow_wire_oauth_descriptor` config flag)
// OR (the `HARBOR_ALLOW_WIRE_OAUTH_DESCRIPTOR` boot env captured here). Both
// default false. This file owns ONLY the boot-env half: cmd/harbor captures the
// env ONCE at process start (write-once atomic, reciprocal with a
// [DEV-ONLY WIRE OAUTH DESCRIPTOR — DO NOT USE IN PRODUCTION] stderr banner), and
// the serve wiring reads it, ORs it with the config flag, and threads the
// effective posture into the agent-config service's wire-descriptor gate.
//
// It is dev-only, fail-closed, and boot-only; it is never Protocol-writable and
// never derived from a request. The default (never captured, or captured false)
// leaves the gate CLOSED — a wire descriptor carrying any credential-sink field
// is rejected, the zero-URL name-only posture unchanged.

package auth

import "sync/atomic"

// allowWireOAuthDescriptor holds the boot-captured env half of the
// wire-descriptor opt-in. Default false (unset ⇒ gate closed).
var allowWireOAuthDescriptor atomic.Bool

// RegisterAllowWireOAuthDescriptorCaptured records, ONCE at boot, whether the
// `HARBOR_ALLOW_WIRE_OAUTH_DESCRIPTOR` dev escape hatch fired. cmd/harbor calls
// it (unconditionally, with the honest boot value) at the same call site that
// emits the dev banner, keeping the capture and the banner structurally
// reciprocal. Calling it with false (or never calling it — the zero value)
// leaves the gate closed. It is dev-only, fail-closed, and boot-only; never
// Protocol-writable.
func RegisterAllowWireOAuthDescriptorCaptured(v bool) {
	allowWireOAuthDescriptor.Store(v)
}

// AllowWireOAuthDescriptorCaptured reports whether the boot-captured
// wire-descriptor escape hatch is active. Read at serve wiring time and ORed
// with the `tools.allow_wire_oauth_descriptor` config flag to compute the
// effective posture threaded into the agent-config service.
func AllowWireOAuthDescriptorCaptured() bool {
	return allowWireOAuthDescriptor.Load()
}
