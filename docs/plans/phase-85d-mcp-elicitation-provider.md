# Phase 85d — mcp-elicitation-provider

## Summary

Support the MCP `elicitation/create` capability so an MCP server can collect structured input from the user mid-tool-call. Two modes per the 2025-11-25 spec: **form mode** (the server supplies a restricted JSON Schema; Harbor renders a form, validates input, returns accept/decline/cancel) and **URL mode** (the server supplies a target URL for sensitive out-of-band flows; Harbor shows the domain, requests consent, opens the URL, and handles the completion notification). The interactive wait runs through the unified pause/resume primitive; the user-facing surface is the Console.

## RFC anchor

- RFC §6.4
- RFC §3.3

## Briefs informing this phase

- brief 14
- brief 03

## Brief findings incorporated

- brief 14 §2 (#28): "Elicitation absent — no `ElicitationHandler`; form & URL modes both unsupported." — both modes are added here.
- brief 14 §4 (biggest gaps #6): "servers cannot collect structured input mid-tool-call." — closed.
- brief 14 §5: servers may task-augment `elicitation/create` (`tasks.requests.elicitation.create`). The non-task path ships here; task-augmented reception is gated on 85h/85i.
- brief 14's matrix note: "form mode collects structured data; URL mode is for sensitive interactions that must not pass through the MCP client … servers must use URL mode for secrets/credentials/payment data." — Harbor enforces this distinction: form mode rejects schemas that look like they collect secrets.

## Findings I'm departing from (if any)

- None.

## Goals

- Harbor advertises the `elicitation` client capability (`form` and `url` sub-capabilities) and registers an `ElicitationHandler`.
- **Form mode:** render a restricted-JSON-Schema form, validate the user's input against the schema, return `accept` (with data) / `decline` / `cancel`.
- **URL mode:** show the user the target domain, request explicit consent, open the URL out-of-band, and handle the `notifications/elicitation/complete` notification.
- The interactive wait is a `RequestPause` through the unified pause/resume primitive — no bespoke coordination.
- Privacy controls per the spec: the requesting server is always identified to the user; decline/cancel are always available; **form mode never collects secrets** — a schema whose fields look credential-shaped is rejected with guidance to use URL mode.

## Non-goals

- Task-augmented elicitation reception (`tasks.requests.elicitation.create`) — gated on Phase 85h/85i.
- Arbitrary HTML forms — form mode is restricted JSON Schema only (the spec's constraint); rich form UIs are not in scope.
- The Console form-rendering component's visual polish — this phase ships a functional form; design refinement rides with the Console wave.

## Acceptance criteria

- [ ] `ClientOptions.Capabilities` advertises `elicitation` with `form` and `url` sub-capabilities.
- [ ] Form mode: a `elicitation/create` request with a restricted JSON Schema renders a form; user input is validated against the schema before return; `accept` returns the data, `decline` / `cancel` return the respective outcome with no data.
- [ ] URL mode: the request's target URL's domain is shown to the user; consent is explicit; on consent the URL opens out-of-band; `notifications/elicitation/complete` resolves the wait.
- [ ] The interactive wait emits `RequestPause` through the unified pause/resume primitive; resume carries the elicitation outcome.
- [ ] The requesting MCP server is named in the user-facing surface for both modes.
- [ ] A form-mode schema with credential-shaped fields (password, api_key, token, card number patterns) is **rejected** with `ErrElicitationSecretInForm` and a message pointing to URL mode.
- [ ] Identity-scoped: elicitation runs under the MCP connection's `(tenant, user, session)`; a test asserts two concurrent connections' elicitations do not cross.
- [ ] Schema validation failures (malformed schema, input not matching schema) fail loudly — no silent accept.

## Files added or changed

- `internal/tools/drivers/mcp/` — new `elicitation.go`: the `ElicitationHandler`, form-mode schema validation, URL-mode consent flow, the secret-field rejector.
- `internal/tools/drivers/mcp/mcp.go` — advertise the `elicitation` capability; register the handler.
- `internal/protocol/` — a Protocol surface so the Console can render the elicitation prompt + return the outcome (an elicitation is a HITL interaction; it needs a Console-visible method/event pair). The exact method shape is finalised against the Console wave.
- `web/console/` — a minimal elicitation form/consent component (functional; design polish deferred).
- Test files — mock MCP server issuing `elicitation/create` in both modes.
- `examples/harbor.yaml` — elicitation operator config (enable/disable).
- `scripts/smoke/phase-85d.sh`.
- `docs/decisions.md` — decision entry (filed at implementation time) on the form-mode secret-rejection rule.
- `docs/plans/README.md` — Status flip on merge.

## Public API surface

```go
// internal/tools/drivers/mcp (delta — illustrative)

var ErrElicitationSecretInForm = errors.New("mcp/elicitation: form mode must not collect secrets — use URL mode")
```

A new Protocol method + event for Console-side elicitation rendering (shape finalised with the Console wave; lands in `internal/protocol/methods` + `internal/protocol/types` per the single-source rule).

## Test plan

- **Unit:** form-schema validation (valid, malformed, input-mismatch); secret-field detection (positive + negative cases); URL-mode domain extraction + consent gating.
- **Integration:** mock MCP server issuing both elicitation modes + real pause/resume coordinator + the Protocol surface; full path pause→render→resume; identity propagation; `-race`.
- **Conformance:** N/A — Phase 85j.
- **Concurrency / leak:** two-identity concurrent elicitations; goroutine-leak baseline.
- **Failure modes:** malformed schema; secret-in-form rejection; user cancel; URL-mode completion never arrives (timeout).

## Smoke script additions

- `scripts/smoke/phase-85d.sh` (classification: `static-only`):
  - Assert `internal/tools/drivers/mcp/elicitation.go` exists.
  - Assert the MCP driver advertises an `elicitation` capability with `form` + `url`.
  - Assert the Protocol method/event for elicitation is registered in `internal/protocol/methods`.

## Coverage target

- `internal/tools/drivers/mcp`: 85%.

## Dependencies

- 28 (MCP driver).
- 50 (Pause/Resume Coordinator — the interactive-wait primitive).

## Risks / open questions

- **Secret-field detection is heuristic.** No detector is perfect; the rejector errs toward false-positives (reject ambiguous fields, point to URL mode) rather than risk a credential in a form. The heuristic's field-name/pattern list is a documented, reviewable constant.
- **URL-mode completion delivery.** `notifications/elicitation/complete` may arrive on any SSE stream (per the transport spec); the handler must accept it on the GET stream too. A timeout bounds an indefinitely-pending URL-mode wait.
- **Console coupling.** This phase needs a Console surface; if the Console wave's elicitation component slips, the Protocol method can ship and be exercised by a Protocol-level test, with the Console component as a fast-follow — but the phase is not "done" until a user can actually answer an elicitation.

## Glossary additions

- **MCP elicitation** — the `elicitation/create` capability: an MCP server collects structured input from the user mid-operation. Form mode uses a restricted JSON Schema (never for secrets); URL mode opens an out-of-band page for sensitive flows.

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references resolve
- [ ] Coverage ≥ target
- [ ] **Cross-isolation test passes** — elicitations are identity-scoped.
- [ ] **Concurrent-reuse test passes** — two-identity concurrent elicitations under `-race`.
- [ ] **Integration test passes** — mock MCP server (both modes) + real pause/resume + Protocol surface.
- [ ] Glossary updated.
- [ ] No brief departures.
