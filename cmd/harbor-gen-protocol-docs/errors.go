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
	protoerrors.CodeAgentPackCopyConflict: {
		When:  "A same-runtime `agent_config.agent_packs.copy` operation was well-formed and its composition preconditions were current, but an independently authored target pack would be overwritten. The atomic copy was refused and no selected pack was partially applied.",
		Retry: "No — inspect the target, resolve the authored collision explicitly, then choose a new reviewed copy request.",
	},
	protoerrors.CodeAgentPackCopyIdempotencyConflict: {
		When:  "A same-runtime Agent pack copy idempotency key was reused with a different source, target, selected-pack set, or composition precondition fingerprint. The replay is refused without mutation.",
		Retry: "No — replay the original request with that key or choose a new idempotency key for a distinct copy.",
	},
	protoerrors.CodeSessionSkillCutoverPending: {
		When:  "A session-personal skill mutation reached a tenant whose explicit durable cutover is still `dual_read`; Harbor refuses the mutation until the declared migration completes and a fresh verification pass authorizes `state_only`.",
		Retry: "Yes, after the operator completes the tenant's declared cutover; do not retry as an unconditional legacy write.",
	},
	protoerrors.CodeSessionSkillReadUnstable: {
		When:  "All three bounded before/after lifecycle and session-erasure fence reads observed concurrent change. Harbor returned no partial session-skill view.",
		Retry: "Yes, after the concurrent lifecycle or erasure transition settles.",
	},
	protoerrors.CodeAgentRetired: {
		When:  "An authorized agent-addressed operation selected a terminally retired agent configuration.",
		Retry: "No — choose a different agent; retirement is terminal.",
	},
	protoerrors.CodeAgentRetirementConflict: {
		When:  "A retirement request used a stale active-content hash or a different operation id from the durable replay identity.",
		Retry: "Only by replaying `agent_config.retire` with the exact original operation id and expected content hash.",
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
	protoerrors.CodeRestartUnavailable: {
		When:  "A persisted tranche pause has no live in-process run loop capable of exact restart redrive. Harbor fails closed rather than pretending to continue the run as a new task.",
		Retry: "No — exact restart redrive is unavailable; start a new run instead.",
	},
	protoerrors.CodeSkillPublicationConflict: {
		When:  "An HA-68 publication or Agent reference mutation used a stale exact generation/content-hash precondition.",
		Retry: "Yes, after re-reading the current publication or reference and resubmitting the reviewed change with fresh exact preconditions.",
	},
	protoerrors.CodeSkillPublicationNotFound: {
		When:  "An HA-68 publication or exact Agent reference was not found in the caller's same-runtime scope.",
		Retry: "No — re-read the same-runtime metadata and use an existing exact identifier.",
	},
	protoerrors.CodeSkillPublicationRetired: {
		When:  "An HA-68 request attempted to use a terminally retired publication revision.",
		Retry: "No — choose an active publication; retirement is terminal.",
	},
	protoerrors.CodeSkillPublicationRuntimeMismatch: {
		When:  "An HA-68 publication or reference belongs to another Harbor runtime/deployment binding.",
		Retry: "No — same-runtime publication references are not portable; discover a reference on this Runtime.",
	},
	protoerrors.CodeSkillPublicationIdempotencyConflict: {
		When:  "An HA-68 idempotency key was reused with a different mutation request.",
		Retry: "No — use the original request for replay or choose a new idempotency key for a distinct mutation.",
	},
	protoerrors.CodeSessionRunning: {
		When:  "A `sessions.delete` erasure was refused because the target session has a RUNNING task, mirroring the GC never-reap-running invariant. No store is touched on refusal — a session with in-flight work is durable execution state, not a cache entry.",
		Retry: "Yes — re-issue after the session's task finishes (or cancel it first).",
	},
	protoerrors.CodeRuntimeError: {
		When:  "An unclassified runtime-side failure — the catch-all. Also used on the SSE surface for subscriber-limit (429) and bus-closed (503) conditions.",
		Retry: "Yes, with backoff — the request shape is not the problem.",
	},
	protoerrors.CodeRenderAdmissionMissing: {
		When:  "An `mcp.apps.call_tool` request carried a render-admission authority but the value was empty, or the request referenced one that was not supplied. The MCP Apps render admission is the HA-56 sealed authority minted only by a successful opt-in `ui://` read.",
		Retry: "No — re-read the resource with `request_render_admission: true` and echo the returned token.",
	},
	protoerrors.CodeRenderAdmissionUnavailable: {
		When:  "The supplied render-admission token could not be opened: invalid base64url, an oversized token, envelope tamper, or a different sealing key/replica. The token's content is unrecoverable by design.",
		Retry: "Yes — re-read the resource for a fresh admission; the App itself is still current.",
	},
	protoerrors.CodeRenderAdmissionInvalid: {
		When:  "The render-admission token opened but its sealed claims are structurally invalid: unknown schema/version, bound violations, a malformed nonce, an absurd lifetime, or future issuance.",
		Retry: "No — re-read the resource for a fresh admission.",
	},
	protoerrors.CodeRenderAdmissionExpired: {
		When:  "The render-admission token is well-formed but its expiry is past. The App must re-read the resource to mint a fresh admission — an otherwise-current App with an expired admission never collapses to an ambiguous not-found.",
		Retry: "Yes — re-read the resource for a fresh admission.",
	},
	protoerrors.CodeRenderAdmissionMismatch: {
		When:  "The render-admission token is well-formed and time-valid but does not match the requested render tuple (identity / agent / server / resource URI / current descriptor generation). The error names no dimension.",
		Retry: "No — re-read the exact resource under the same identity/agent for a fresh admission.",
	},
	protoerrors.CodeRenderAuthorityAmbiguous: {
		When:  "An `mcp.apps.call_tool` request supplied BOTH the legacy `binding` authority and the fresh `render_admission` authority. The two are distinct; Harbor never guesses which the App meant.",
		Retry: "No — supply exactly one authority.",
	},
	protoerrors.CodeSkillImportProposalInvalid: {
		When:  "An `agent_config.user.skills.import_commit` echoed a proposal token that is unknown, consumed, foreign, or stale (oversize, malformed base64, failed authentication, unknown schema, malformed claims, or bound to different server-side inputs).",
		Retry: "Yes — re-run `agent_config.user.skills.import_validate` for a fresh proposal and echo it exactly.",
	},
	protoerrors.CodeSkillImportProposalExpired: {
		When:  "The echoed import proposal token's review window elapsed before the explicit commit.",
		Retry: "Yes — re-run `agent_config.user.skills.import_validate` for a fresh proposal.",
	},
	protoerrors.CodeSkillImportPackageInvalid: {
		When:  "The referenced artifact is not a valid complete skill package (archive / path / MIME / SKILL.md / support-ref / frontmatter violations, including authority-bearing frontmatter). The request envelope was well formed; the package was not.",
		Retry: "No — fix the package and re-upload, then re-validate.",
	},
	protoerrors.CodeSkillImportReplaceRequired: {
		When:  "A different package already wins the target canonical key and the commit did not carry explicit replacement consent.",
		Retry: "Yes — re-review the existing winner and commit again with `replace: true`.",
	},
	protoerrors.CodeQueryBudgetExceeded: {
		When:  "An `observability.query` request exceeds a result budget: the window spans more buckets than the closed maximum, or the page limit exceeds the maximum. Fails loudly; never a truncated response.",
		Retry: "Yes — narrow the window, coarsen the bucket, or lower the page limit.",
	},
	protoerrors.CodeInvalidCursor: {
		When:  "The `observability.query` page cursor is malformed or was produced by a differently-shaped query (including a different identity scope). The query never silently restarts at an arbitrary position.",
		Retry: "Yes — restart from the first page (no cursor).",
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
