# Phase 85c — mcp-sampling-provider

## Summary

Expose Harbor's LLM client to MCP servers via the `sampling/createMessage` capability. An MCP server that needs an LLM call-back — without holding its own provider API key — asks Harbor; Harbor renders an approval surface, runs the call through `llm.LLMClient`, and returns the completion. Covers `modelPreferences` mapping (hints → a Harbor-configured provider/model), multimodal sampling content (text/image/audio), and tool-enabled sampling (`sampling.tools`). Server-initiated calls are gated by the unified pause/resume primitive so a human can review before the model runs.

## RFC anchor

- RFC §6.4
- RFC §6.5
- RFC §3.3

## Briefs informing this phase

- brief 14
- brief 03

## Brief findings incorporated

- brief 14 §2 (#27): "Sampling absent … a notable miss given Harbor *is* an LLM runtime with an `llm.LLMClient` it could trivially expose." — this phase is that exposure.
- brief 14 §4 (biggest gaps #5): "Servers that need an LLM call-back cannot use Harbor." — closed here.
- brief 14 §5: once sampling exists, servers may task-augment `sampling/createMessage` (Harbor becomes a task *receiver*). This phase ships the non-task path; task-augmented reception is gated on 85h/85i and cross-referenced below.
- brief 03 (LLM client): `llm.LLMClient.Complete` is the single call surface; sampling maps MCP's `CreateMessageRequest` onto it without a parallel LLM path.

## Findings I'm departing from (if any)

- None.

## Goals

- Harbor advertises the `sampling` client capability and registers a `CreateMessageHandler`.
- A server `sampling/createMessage` request is mapped to an `llm.CompleteRequest`, run through `llm.LLMClient`, and the completion returned as a `CreateMessageResult`.
- `modelPreferences` (cost / speed / intelligence priorities + model hints) are interpreted as *advisory* — Harbor makes the final model choice from its configured providers.
- Multimodal sampling content (text / image / audio) is supported where the resolved model supports it; unsupported modalities fail loudly with a clear error.
- Tool-enabled sampling (`sampling.tools`) is capability-gated: when negotiated, tool definitions pass to the model and tool-use / tool-result blocks are handled with balanced pairing.
- Every server-initiated sampling call emits a `RequestPause` for human review before the model runs (configurable per operator policy; default: review on).

## Non-goals

- Task-augmented sampling reception (`tasks.requests.sampling.createMessage`) — gated on Phase 85h/85i.
- Letting servers pick a specific provider API key — Harbor always uses its own configured providers; `modelPreferences` is advisory only.
- Streaming sampling results to the server — the 2025-11-25 sampling result is unary; streaming is out of scope.

## Acceptance criteria

- [ ] `ClientOptions.Capabilities` advertises `sampling` (and `sampling.tools` when the operator enables tool-enabled sampling).
- [ ] A `CreateMessageHandler` maps `CreateMessageRequest` → `llm.CompleteRequest`, invokes `llm.LLMClient.Complete`, maps the response → `CreateMessageResult`.
- [ ] `modelPreferences` hints map to a Harbor-configured model; a test asserts the mapping is advisory (an unmatchable hint falls back to the default model, does not error).
- [ ] Multimodal: image / audio content in a sampling message is passed through to a capable model; an incapable model produces `ErrUnsupportedModality` (fail loud), not a silent drop.
- [ ] Tool-enabled sampling: when `sampling.tools` is negotiated, tool definitions reach the model and `tool_use` / `tool_result` blocks are balanced; an unbalanced sequence fails loudly.
- [ ] A server-initiated sampling call emits `RequestPause` through the unified pause/resume primitive; resume runs the call; reject aborts it. No pause coordination outside the primitive.
- [ ] The sampling call is identity-scoped — it runs under the `(tenant, user, session)` of the MCP connection; cost is attributed to that identity via the Phase 36a accumulator.
- [ ] Cross-isolation: two concurrent MCP connections under different identities cannot see each other's sampling calls or costs.

## Files added or changed

- `internal/tools/drivers/mcp/` — new `sampling.go`: the `CreateMessageHandler`, request/response mapping, `modelPreferences` resolution.
- `internal/tools/drivers/mcp/mcp.go` — advertise the `sampling` capability; register the handler.
- `internal/config/config.go` — operator knob for sampling (enable/disable, review-on-default, tool-enabled toggle).
- Test files — mock MCP server that issues `sampling/createMessage`; stub `llm.LLMClient`.
- `examples/harbor.yaml` — document the sampling config.
- `scripts/smoke/phase-85c.sh`.
- `docs/decisions.md` — decision entry (filed at implementation time) on the advisory-model-preference + mandatory-review stance.
- `docs/plans/README.md` — Status flip on merge.

## Public API surface

```go
// internal/tools/drivers/mcp (delta — illustrative)

// sampling capability handler — mapped onto llm.LLMClient.Complete.
// Not exported beyond the package; the MCP driver owns it.

var ErrUnsupportedModality = errors.New("mcp/sampling: model does not support requested content modality")
```

No change to `llm.LLMClient`. No change to the public MCP driver surface.

## Test plan

- **Unit:** request/response mapping; `modelPreferences` resolution (matchable, unmatchable, empty); modality capability check; tool-use/tool-result balancing.
- **Integration:** mock MCP server issuing `sampling/createMessage` + stub `llm.LLMClient` + real pause/resume coordinator; full path with review-pause-resume; identity propagation; `-race`.
- **Conformance:** N/A — Phase 85j.
- **Concurrency / leak:** two-identity concurrent sampling; cost-attribution isolation; goroutine-leak baseline.
- **Failure modes:** unsupported modality; unbalanced tool blocks; review rejected; LLM call errors.

## Smoke script additions

- `scripts/smoke/phase-85c.sh` (classification: `static-only`):
  - Assert `internal/tools/drivers/mcp/sampling.go` exists.
  - Assert the MCP driver advertises a `sampling` capability (grep the capability construction).

## Coverage target

- `internal/tools/drivers/mcp`: 85%.

## Dependencies

- 28 (MCP driver).
- 32 (LLM client core — `llm.LLMClient.Complete`).
- 50 (Pause/Resume Coordinator — server-initiated-call review gate).

## Risks / open questions

- **Server-initiated LLM spend.** A malicious or buggy MCP server could drive cost via sampling. The mandatory review-pause + per-identity Phase 36a ceilings are the mitigation; the phase documents that sampling spend counts against the connection identity's ceiling.
- **Tool-enabled sampling recursion.** A sampling call whose tool-use triggers another MCP tool call that itself samples could recurse. A depth guard (documented constant) bounds it.
- **Modality mismatch surface.** `modelPreferences` cannot express modality; Harbor must resolve a model that supports the *content actually present*, not just the hints. The resolution order is documented.

## Glossary additions

- **MCP sampling** — the `sampling/createMessage` capability: an MCP server asks the host (Harbor) to run an LLM completion on its behalf, so the server needs no provider API key of its own. In Harbor, sampling runs through `llm.LLMClient` under the MCP connection's identity, gated by a review pause.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references resolve
- [ ] Coverage ≥ target
- [ ] **Cross-isolation test passes** — sampling calls + cost are identity-scoped.
- [ ] **Concurrent-reuse test passes** — two-identity concurrent sampling under `-race`.
- [ ] **Integration test passes** — mock MCP server + stub LLM + real pause/resume.
- [ ] Glossary updated.
- [ ] No brief departures.
