package main

import (
	"fmt"
	"strings"

	protoerrors "github.com/hurtener/Harbor/internal/protocol/errors"
	"github.com/hurtener/Harbor/internal/protocol/transports/control"
)

// errorEntry carries the prose half of the error reference for one
// canonical code: when it fires and whether a client should retry. The
// code set and the HTTP binding are read from the single sources
// (errors.Codes() / control.HTTPStatus); only the guidance strings live
// here, pinned exhaustive by TestGen_ErrorTableInLockstep.
type errorEntry struct {
	// When describes the condition that produces the code.
	When string
	// Retry is the client guidance: retry as-is, fix-then-retry, or
	// don't.
	Retry string
}

// errorTable is the per-code guidance join. Every protoerrors.Codes()
// entry MUST have a row (lockstep-pinned); a new canonical code without
// a row fails the build.
var errorTable = map[protoerrors.Code]errorEntry{
	protoerrors.CodeInvalidRequest: {
		When:  "The request was structurally malformed: undecodable JSON, a wrong wire shape for the method, an out-of-range field.",
		Retry: "No — fix the request shape first.",
	},
	protoerrors.CodeSessionErased: {
		When:  "A `start` named a session id that was permanently deleted by `sessions.delete` (right-to-erasure). The session is terminal and cannot be reopened — its data is gone. A closed-but-not-erased session, by contrast, reopens normally on `start`.",
		Retry: "No — the conversation was erased; start a new one with a fresh session id.",
	},
	protoerrors.CodeRevisionConflict: {
		When: "An `agent_config.*` write declared an `expected_content_hash` and the agent's active revision no longer carries it — another writer moved the base between the caller's read and its write — or the agent has no active revision at all. The request was well-formed and authorised; nothing was persisted (no revision, no active-pointer move, no `agent.config.revised` event). " +
			"The refusal is exact across Runtime processes sharing a shipped StateStore: publication rechecks the active-pointer EventID through `StateStore.SaveIf`; the per-owner lock only reduces local contention. Omitting `expected_content_hash` keeps the unconditional last-writer-wins behaviour.",
		Retry: "Yes, after re-reading — call `agent_config.get` (or `agent_config.user.get` if the door you are retrying is a `user.*` twin; they are separate revision spines and a hash from the wrong one never matches) for the current `revision_id` and `content_hash`, re-apply your edit on top (`agent_config.diff` compares what you read against what it is now), and resubmit with the fresh hash.",
	},
	protoerrors.CodeSessionSkillCutoverPending: {
		When:  "A session-personal skill mutation reached a tenant whose explicit durable cutover is still `dual_read`; Harbor refuses the mutation until the declared migration completes and a fresh verification pass authorizes `state_only`.",
		Retry: "Yes, after the operator completes the tenant's declared cutover; do not retry as an unconditional legacy write.",
	},
	protoerrors.CodeSessionSkillReadUnstable: {
		When:  "All three bounded before/after lifecycle and session-erasure fence reads observed concurrent change. Harbor returned no partial session-skill view.",
		Retry: "Yes, after the concurrent lifecycle or erasure transition settles.",
	},
	protoerrors.CodeIdentityRequired: {
		When:  "The request resolved no complete `(tenant, user, session)` identity scope — a missing bearer, a missing session (no `X-Harbor-Session` header and no default claim), or a body identity that contradicts the verified token. Identity is mandatory and fails closed.",
		Retry: "No — attach a token / session first ([Auth & identity](./auth-and-identity.md)).",
	},
	protoerrors.CodeAuthRejected: {
		When:  "A bearer token was present but failed verification: malformed, an algorithm outside the asymmetric allowlist, bad signature, expired / not-yet-valid, unknown `kid`, audience or issuer mismatch.",
		Retry: "Only after obtaining a fresh valid token.",
	},
	protoerrors.CodeScopeMismatch: {
		When:  "The caller's steering scope claim is below the control method's RFC §6.3 minimum, or a cross-tenant steering / mutation was attempted without `admin`.",
		Retry: "No — the operation needs a higher scope.",
	},
	protoerrors.CodeIdentityScopeRequired: {
		When:  "The request is authenticated and identified, but the requested cross-tenant fan-in (e.g. `events.subscribe?admin=1`) or admin verb needs a verified `admin` / `console:fleet` scope claim the token does not carry.",
		Retry: "No — re-authenticate with a scope-bearing token.",
	},
	protoerrors.CodePayloadInvalid: {
		When:  "A control payload violated an RFC §6.3 bound (depth > 6, > 64 keys, > 50 list items, a string > 4096 chars, > 16 KiB total) or carried an unsupported leaf type.",
		Retry: "No — shrink / restructure the payload.",
	},
	protoerrors.CodeUnknownMethod: {
		When:  "The method name is not in the canonical registry ([methods.md](./methods.md)).",
		Retry: "No — check the method name and this Runtime's advertised capabilities (`runtime.info`).",
	},
	protoerrors.CodeNotFound: {
		When:  "The request's target does not exist in the caller's scope: a steering control for a run with no live inbox (never started or already terminal), an unknown task / flow / artifact id. Cross-tenant existence is never revealed — a foreign id is indistinguishable from a missing one.",
		Retry: "No — the target is gone or never existed for you.",
	},
	protoerrors.CodeRequestTooLarge: {
		When:  "An `artifacts.put` body exceeded the configured `protocol.max_request_bytes` bound. The upload is refused loudly, never truncated.",
		Retry: "No — shrink the payload or raise the operator-side bound.",
	},
	protoerrors.CodePresignUnsupported: {
		When:  "An `artifacts.get_ref` request reached an ArtifactStore driver without presigned-URL support (in-mem / fs / sqlite / postgres blob drivers). The resolver fails loud instead of silently streaming bytes.",
		Retry: "No — the configured driver cannot satisfy it; use a presign-capable store (S3 family) or download via the Console proxy.",
	},
	protoerrors.CodeSessionRunning: {
		When:  "A `sessions.delete` erasure was refused because the target session has a RUNNING task, mirroring the GC never-reap-running invariant. No store is touched on refusal — a session with in-flight work is durable execution state, not a cache entry.",
		Retry: "Yes — re-issue after the session's task finishes (or cancel it first).",
	},
	protoerrors.CodeRuntimeError: {
		When:  "An unclassified runtime-side failure — the catch-all. Also used on the SSE surface for subscriber-limit (429) and bus-closed (503) conditions.",
		Retry: "Yes, with backoff — the request shape is not the problem.",
	},
}

// renderErrorsPage emits errors.md: every canonical code with its HTTP
// binding (read from the SAME control.HTTPStatus the wire transport
// serves) and retry guidance.
func renderErrorsPage() (string, error) {
	var b strings.Builder
	b.WriteString(generatedHeader + "\n\n")
	b.WriteString("# Protocol errors\n\n")
	fmt.Fprintf(&b, "The %d canonical Harbor Protocol error codes, generated from the single-source\n", len(protoerrors.Codes()))
	b.WriteString("registry (`internal/protocol/errors`). The HTTP column is read from the same\n")
	b.WriteString("code-to-status binding the wire transport serves — the two cannot drift.\n\n")
	b.WriteString("Every error response body is the one [`Error`](./types.md#error) envelope:\n\n")
	b.WriteString("```json\n{ \"code\": \"<stable code>\", \"message\": \"<human-readable, advisory>\" }\n```\n\n")
	b.WriteString("Clients branch on `code` (stable across Runtime refactors — RFC §5.3), never on\n")
	b.WriteString("`message`.\n\n")
	b.WriteString("| Code | HTTP | When it fires | Should you retry? |\n")
	b.WriteString("|---|---|---|---|\n")
	for _, code := range protoerrors.Codes() {
		entry, ok := errorTable[code]
		if !ok {
			return "", fmt.Errorf("error code %q has no errorTable entry — extend the table (the lockstep test pins this)", code)
		}
		fmt.Fprintf(&b, "| `%s` | %d | %s | %s |\n", code, control.HTTPStatus(code), entry.When, entry.Retry)
	}
	return b.String(), nil
}
