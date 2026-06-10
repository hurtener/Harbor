# Steer and resume a run (HITL approval, tool-side OAuth, durable pauses)

Harbor has exactly ONE pause/resume primitive (RFC §3.3): HITL
approval, tool-side OAuth, A2A `AUTH_REQUIRED`, and operator PAUSE all
park a run through the same `pauseresume.Coordinator` and resume
through the same typed `Decision` markers (`approve` / `reject` /
`resume` / `timeout` — D-096). This recipe shows the one choreography
with its two V1 triggers. It is deliberately a single document — a
per-reason recipe split would re-teach the four-parallel-
implementations mistake the primitive exists to close.

This recipe is acceptance-gated: the OAuth choreography is executed
end-to-end by `test/integration/phase111b_oauth_completion_test.go`
(Phase 111b, D-199) and the approval choreography by
`test/integration/approval_midstep_test.go` (D-192), so every symbol
referenced exists in the tree.

## The shared shape

```text
something needs a human / an external event
  → Coordinator.Request parks a pause record
      (pause.requested on the bus; the Console intervention queue
       lists it via pause.list)
  → the run waits
  → the trigger resolves (an APPROVE control, an OAuth callback, …)
  → Coordinator.Resume terminates the pause
      (pause.resumed on the bus, carrying the typed Decision)
  → the run re-enters and continues
```

What differs per trigger is WHO calls `Resume` and what has to happen
first. Never build a second pause path — CLAUDE.md §13 rejects it on
sight.

## Trigger 1 — HITL approval

Declare an approval gate on a tool in `harbor.yaml`:

```yaml
tools:
  entries:
    - name: delete_doc
      approval:
        policy: deny-all          # or approve-all / tagged
        reason: "deletion requires human review"
```

When the planner dispatches `delete_doc`, the gate parks the call
(`tool.approval_requested` carries the pause token) and the run blocks
inside the dispatch. The approver answers through the steering inbox —
the Protocol `approve` / `reject` methods, the Console intervention
queue, or in-process:

```go
// steering = github.com/hurtener/Harbor/sdk/steering
inbox, _ := steeringRegistry.Lookup(q) // q = the run's identity.Quadruple
_ = inbox.Enqueue(steering.ControlEvent{
    Type:         steering.ControlApprove, // or ControlReject
    Identity:     q,
    CallerScope:  steering.ScopeOwnerUser,
    CallerTenant: q.TenantID,
    Payload:      map[string]any{"token": pauseToken, "reason": "looks safe"},
})
```

The run loop drains the control mid-step (D-192), routes it through
the gate bridge (D-097), and the blocked dispatch unblocks: APPROVE
runs the tool with the original args; REJECT terminates the run with
`Finish{ConstraintsConflict}`.

## Trigger 2 — tool-side OAuth completion

### The choreography

1. The planner dispatches a tool whose catalog entry declares an
   `oauth:` attachment. The wrapper's `Token()` finds no usable token
   and parks a pause: `tool.auth_required` (carrying the
   `authorize_url`, the `state` nonce, and the pause token) and
   `pause.requested` land on the bus.
2. The user (or admin, for agent-bound attachments) visits the
   authorize URL and grants access.
3. The authorization server redirects the browser to your
   `redirect_uri` — Harbor's callback handler.
4. The handler exchanges `(state, code)` via `CompleteFlow`: the token
   persists (AES-GCM at rest), and the pause resumes with
   `pause.resumed{Decision: resume}` + `tool.auth_completed`.
5. The run re-enters; the next dispatch of the tool finds the token
   and succeeds.

### The dev-binary path (zero ceremony)

`harbor dev` mounts the handler automatically at
`GET /v1/tools/oauth/callback`. Point every attachment's redirect URL
at it:

```yaml
tools:
  # Mandatory whenever oauth_providers[] is set: names the env var
  # holding the 32-byte hex key that encrypts tokens at rest (AES-GCM).
  oauth_token_kek_env: HARBOR_OAUTH_TOKEN_KEK
  oauth_providers:
    - name: github
      driver: oauth2
      client_id_env: GITHUB_CLIENT_ID
      client_secret_env: GITHUB_CLIENT_SECRET
      auth_url: https://github.com/login/oauth/authorize
      token_url: https://github.com/login/oauth/access_token
      redirect_url: http://127.0.0.1:18080/v1/tools/oauth/callback
      scopes: [repo]
  entries:
    - name: github_fetch
      oauth:
        provider: github
        binding_scope: user
```

Export the key-encryption key before booting — config validation
fails closed without it (`openssl rand -hex 32` mints one):

```bash
export HARBOR_OAUTH_TOKEN_KEK="$(openssl rand -hex 32)"
```

> **The port-must-match gotcha.** The `redirect_url` you register with
> the OAuth provider (GitHub, Google, …) and the `redirect_url` in
> `harbor.yaml` must BOTH match the address `harbor dev` actually
> binds (`--port` / `HARBOR_BIND`). A mismatch fails at the provider's
> redirect, not in Harbor — check the provider's error page first.

### The headless path

`auth.CallbackHandler` is a plain `http.Handler` over the exported
`OAuthProvider` interface — no Protocol server, no dev server. Mount
it on your own mux at whatever path matches your configured
`RedirectURI` (the providers map is the same
`assemble.Stack.OAuthProviders` the assembly built — D-197):

```go
import (
    toolauth "github.com/hurtener/Harbor/sdk/tools/auth"
)

mux := http.NewServeMux()
mux.Handle(toolauth.CallbackRoutePattern, // "GET /v1/tools/oauth/callback"
    toolauth.CallbackHandler(stack.OAuthProviders,
        toolauth.WithCallbackLogger(logger)))
// or any custom path — just keep RedirectURI in sync:
// mux.Handle("GET /oauth/done", toolauth.CallbackHandler(stack.OAuthProviders))
```

The handler maps the flow sentinels onto typed JSON errors:
`ErrFlowNotFound` → 404, `ErrFlowExpired` → 410, `ErrStateMismatch` →
400, upstream exchange failure → 502; success serves a static
"authorization complete" page (override via
`toolauth.WithSuccessPage`). No token or code material ever appears in
a response or a log line.

> **Process-local constraint (V1).** The pause handle registry is
> process-local (RFC §6.3): the callback must land on the SAME process
> that parked the run. True by construction for `harbor dev` and
> single-process embedders; a multi-process deployment needs the
> post-V1 distributed bus before the callback can be load-balanced.

### Denied authorizations fail loud

If the user clicks "Deny", the provider redirects with
`error=access_denied`. The handler consumes the flow and resumes the
pause with `Decision: reject` (`DenyFlow`) — the run does not hang
until the flow TTL; the rejection is visible on the bus and in the
intervention queue (D-199).

## Durable pauses + the pause lifecycle (Phase 111c)

### Configure a durable Coordinator

Durability rides on the runtime's existing `state.StateStore` (the
§4.4 persistence seam — D-067; no parallel checkpoint driver). The
supported embedding shape is config-driven: point `state` at a durable
DSN, set the pause knobs, and `assemble.Assemble` builds the
Coordinator with the bus, the checkpoint store, and the max-park
ceiling already wired:

```go
cfg := config.Defaults()                 // github.com/hurtener/Harbor/sdk/config
cfg.State.Driver = "sqlite"
cfg.State.DSN = "/var/lib/harbor/state.sqlite"
cfg.PauseResume.MaxParkDuration = 24 * time.Hour // 0 = pauses never expire (the default)
cfg.PauseResume.SweepInterval = time.Minute
// ... llm block etc., then:
stack, err := assemble.Assemble(ctx, cfg, assemble.Options{})
```

Every pause record — including the run's serialized trajectory — is
checkpointed through that store. The pause Coordinator itself is
deliberately NOT part of the `sdk/` facade (D-205's curation call):
the assembled stack is the supported construction path, in-module and
external alike. The binary's `harbor.yaml` carries the same two knobs
(see below).

### What survives a restart

A fresh Coordinator over the **same** store rehydrates a parked pause
on demand: `Status(token)` reports it paused, `Resume(token, ...)`
re-attaches tool-context handles, restores the serializable half, and
clears the checkpoint. Resume is **destructive**: a resumed pause's
checkpoint is deleted, and a later Coordinator sees
`ErrPauseNotFound` — a resumed pause is terminal, not history (use
`events.subscribe` on `pause.resumed` for history).

What fails loud (never silently degrades):

- **`planner.ErrUnserializable`** (`sdk/planner`) at `Request` time —
  a pause whose trajectory or payload carries a non-JSON-encodable
  leaf is rejected before anything is recorded. No half-persisted
  checkpoint.
- **`planner.ErrToolContextLost`** at `Resume` time — the handle
  registry is process-local at V1 (RFC §6.3); a resume that needs a
  handle the restarted process never re-registered fails loud rather
  than resuming with a nil tool context.

### Who reaps abandoned pauses

Without a ceiling, a pause nobody answers (or a run cancelled while
paused) parks forever. `max_park_duration` + the pause sweeper give
the lifecycle an end. With `pauseresume.max_park_duration > 0` and a
non-zero `sweep_interval`, the assembly starts the sweeper goroutine
for you and joins it on `stack.Close(ctx)` — there is nothing to wire
by hand.

Every sweep pass resumes each pause past `PausedAt + max_park_duration`
with the typed **`timeout`** Decision (D-096): the `pause.resumed`
event carries `decision: timeout`, the checkpoint is deleted, and the
waiting run terminates as a **constraints-conflict** — a deadline the
human missed is a constraint the planner cannot resolve. Never a
silent unpark-and-continue.

One V1 boundary (recorded in D-200): the sweeper walks the LIVE
process's pause registry. A checkpoint orphaned by a process crash is
not proactively scanned — it is reaped only after something rehydrates
it (a `Status` or `Resume` against the restarted process).

In the binary, the same two knobs come from `harbor.yaml`:

```yaml
pauseresume:
  max_park_duration: 24h # 0 = never expire (default; no sweeper)
  sweep_interval: 1m     # must be <= max_park_duration when set
```

The sweeper fails loud (`ErrSweeperMisconfigured`) against a
Coordinator with no max-park duration — a sweeper that silently reaps
nothing forever is the failure mode the error closes. Config
validation enforces the `sweep_interval <= max_park_duration`
relationship before the stack ever boots.

## What is and is NOT automatic (honesty notes)

- **Automatic:** the OAuth pause record's resolution. The callback →
  `CompleteFlow` → `Coordinator.Resume` leg needs no operator action;
  `pause.resumed{Decision: resume}` is emitted the moment the
  redirect lands.
- **Operator-steered at V1:** the RUN-level re-entry. A planner that
  parked the run with `RequestPause` re-enters when a steering
  `RESUME` control arrives on its inbox — the Protocol `resume`
  method, the Console intervention queue, or an in-process `Enqueue`
  (watch for `tool.auth_completed` / `pause.resumed` on the bus and
  resume then, as the Phase 111b E2E does). An automatic
  completion→run-resume bridge is a candidate follow-up; today the
  resume control is the operator's (or your watcher's) call — the
  same surface HITL approval already uses.
- **Don't bare-Resume.** Resuming the run WITHOUT the callback having
  completed the flow re-parks it immediately — the token is still
  missing, so the next dispatch raises `tool.auth_required` again.
  The callback is the completion path; the steering RESUME is only
  the re-entry trigger.
- **Token refresh is automatic.** Once a token exists, expiry triggers
  a single-flight refresh inside `Token()`; no new pause unless the
  refresh irrecoverably fails.

## Observability checklist

| Event | Emitted by | When |
|-------|-----------|------|
| `tool.auth_required` | the OAuth provider | flow initiated (pause parked) |
| `pause.requested` | the Coordinator | same moment |
| `tool.auth_completed` | `CompleteFlow` | callback exchanged + token persisted |
| `pause.resumed` (`Decision: resume`) | the Coordinator | same moment |
| `pause.resumed` (`Decision: reject`) | the Coordinator | upstream denial (`DenyFlow`) |
| `tool.approval_requested` / `tool.approved` / `tool.rejected` | the approval gate | the HITL trigger |
