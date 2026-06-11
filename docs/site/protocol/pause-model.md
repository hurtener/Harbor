# Choreography 4 — The pause model

The one that makes Harbor distinctive on the wire. A run can park for many
reasons that look different on the surface — a human must approve a tool call,
a tool needs interactive OAuth, a remote agent demands input, an operator hit
pause — but they are **one Runtime primitive** (RFC §3.3), and therefore one
wire choreography. A client that handles `pause.requested` once handles all of
them. A client that renders pending interventions correctly is a client that
understood Harbor.

> Methods demonstrated: `pause`, `resume`, `approve`, `reject`, `pause.list`

Except for the OAuth intervention section (whose frames are transcribed from
the callback handler and its tests — see the note there), every
request/response and SSE frame on this page is real wire traffic, captured
from a runtime assembled with the production drivers (the same assembly
`harbor dev` boots) running an approval-gated tool — not freehand prose.
Tokens and timestamps vary; shapes do not.

## The choreography at a glance

```text
the run parks (gate / OAuth / A2A / operator PAUSE)
        |
        v
event stream:  pause.requested {Token, Reason}
               (+ tool.approval_requested | tool.auth_required, per cause)
               (+ notification.pause_requested — the inbox-ready projection)
        |
        v
your client:   render the intervention; pause.list to snapshot on attach
        |
        v
intervention:  POST /v1/control/approve | reject | resume     (HITL / operator)
               GET  /v1/tools/oauth/callback?state=…&code=…   (the OAuth leg)
               …or nobody acts and the max-park sweeper reaps it
        |
        v
event stream:  pause.resumed {Token, Reason, Decision}
               Decision ∈ approve | reject | resume | timeout
```

The **pause token** is the thread through everything: minted by the Runtime
when the pause is requested, carried on every related event, listed by
`pause.list`, and quoted back by token-targeted interventions.

## What parks a run

Four causes, one event shape (RFC §3.3):

| Cause | `Reason` on the wire | Companion event |
|---|---|---|
| HITL approval — an approval-gated tool call awaits a human | `approval_required` | `tool.approval_requested` |
| Tool-side OAuth — a tool needs the user to authorize a provider | `external_event` | `tool.auth_required` |
| A2A `AUTH_REQUIRED` / `INPUT_REQUIRED` — a remote agent demands input | `external_event` / `await_input` | — |
| Operator `pause` — a steering PAUSE parked the run at the next step boundary | `await_input` | — |

The closed `Reason` set is `approval_required` / `await_input` /
`external_event` / `constraints_conflict` (RFC §6.3).

## The park, on the stream

A planner picks an approval-gated tool (`deploy_to_production`, gated
`deny-all` — every call needs a human). The run parks and the stream narrates:

```text
event: pause.requested
id: 4
data: {"type":"pause.requested","sequence":4,"occurred_at":"2026-06-11T01:05:04.321615000Z","tenant":"dev","user":"dev","session":"hitl-demo","run":"01KTT3RUNAPPROVE0000000000","payload":{"Token":"01KTT3C5T1S9WBT9ASTJHQ4GJK","Reason":"approval_required"}}

event: tool.approval_requested
id: 5
data: {"type":"tool.approval_requested","sequence":5,"occurred_at":"2026-06-11T01:05:04.321619000Z","tenant":"dev","user":"dev","session":"hitl-demo","payload":{"Tool":"deploy_to_production","PauseToken":"01KTT3C5T1S9WBT9ASTJHQ4GJK","Reason":"production deploys require human sign-off","Tags":null,"ArgsSummary":{"args":{"build":"v1.3.0","environment":"production"},"tool":"deploy_to_production"}}}

event: notification.pause_requested
id: 6
data: {"type":"notification.pause_requested","sequence":6,…,"payload":{"Data":{"class":"notification.pause_requested","deeplink":"/console/interventions/01KTT3C5T1S9WBT9ASTJHQ4GJK","origineventsequence":4,"origineventtype":"pause.requested","sealed":{},"severity":"info","summary":"Run paused awaiting intervention (reason=approval_required)"}}}
```

Three layers, by audience:

- **`pause.requested`** is the primitive: the token and the reason. Always
  emitted, whatever the cause.
- **`tool.approval_requested`** is the cause-specific companion: which tool,
  why it is gated, and a redacted `ArgsSummary` for the reviewer. The OAuth
  cause emits `tool.auth_required` instead (below). Note `PauseToken` here ==
  `Token` on `pause.requested` — that is the join key.
- **`notification.pause_requested`** (and its `notification.*` siblings) is
  the derived, inbox-ready projection: severity, human summary, deep link.
  Render it directly or build your own from the primitives.

A parked run's **task stays `running`** on the task surface — parking is a
pause-record state, not a task-FSM transition. The authoritative "what awaits
a human" read is `pause.list`, not a task-status filter.

## The snapshot: pause.list

Events narrate; snapshots catch you up. A client that attaches after the park
(or re-attaches after a drop) calls `pause.list` (`POST /v1/pause/list`):

```bash
curl -sS -X POST "$HARBOR_BASE_URL/v1/pause/list" \
  -H "Authorization: Bearer $TOKEN" \
  -H "X-Harbor-Session: $SESSION" \
  -H "Content-Type: application/json" \
  -d '{"identity": {}}'
```

```json
{
  "snapshots": [
    {
      "token": "01KTT3C5T1S9WBT9ASTJHQ4GJK",
      "reason": "approval_required",
      "state": "paused",
      "identity": { "tenant": "dev", "user": "dev", "session": "hitl-demo" },
      "paused_at": "2026-06-10T22:05:04.321294-03:00",
      "resumed_at": "0001-01-01T00:00:00Z",
      "payload": {
        "reason": "production deploys require human sign-off",
        "tool": "deploy_to_production"
      }
    }
  ],
  "page": 1,
  "page_size": 50,
  "page_count": 1,
  "total_rows": 1
}
```

[`PauseListRequest`](./types.md#pauselistrequest) supports a
[`PauseFilter`](./types.md#pausefilter) and cursorless page/page_size
pagination; cross-tenant fan-in requires `admin` or `console:fleet`. A heavy
pause payload is routed through the artifact store and arrives as a
`payload_ref` ([`PauseArtifactRef`](./types.md#pauseartifactref)) instead of
inline `payload` — bytes never ride the snapshot.

## Intervention surface 1 — approve / reject (HITL)

The human verdict travels as a steering control —
`POST /v1/control/approve` or `/v1/control/reject` — with the gate's pause
token in the payload's `token` key and the verdict's rationale in `reason`:

```bash
curl -sS -X POST "$HARBOR_BASE_URL/v1/control/approve" \
  -H "Authorization: Bearer $TOKEN" \
  -H "X-Harbor-Session: $SESSION" \
  -H "Content-Type: application/json" \
  -d '{
    "identity": {"run": "01KTT3RUNAPPROVE0000000000", "scope": "owner_user"},
    "payload": {"token": "01KTT3C5T1S9WBT9ASTJHQ4GJK", "reason": "reviewed the deploy plan — go"}
  }'
```

```json
{ "accepted": true, "method": "approve", "protocol_version": "0.1.0" }
```

As everywhere on the control surface, the `200` means *validated and
enqueued*; the effect is narrated on the stream
([task control](./task-control.md)). Here is the approve's full wake, as
captured:

```text
event: pause.resumed
id: 8
data: {"type":"pause.resumed","sequence":8,…,"run":"01KTT3RUNAPPROVE0000000000","payload":{"Token":"01KTT3C5T1S9WBT9ASTJHQ4GJK","Reason":"approval_required","Decision":"approve"}}

event: control.received
id: 9
data: {"type":"control.received","sequence":9,…,"payload":{"Type":"APPROVE","Outcome":"received","Err":""}}

event: control.applied
id: 10
data: {"type":"control.applied","sequence":10,…,"payload":{"Type":"APPROVE","Outcome":"applied","Err":""}}

event: tool.approved
id: 11
data: {"type":"tool.approved","sequence":11,…,"payload":{"Tool":"deploy_to_production","PauseToken":"01KTT3C5T1S9WBT9ASTJHQ4GJK","ApproverReason":"reviewed the deploy plan — go"}}
```

The gated tool call proceeds with its original arguments, the run re-enters
the planner, and a subsequent `pause.list` is empty again. (Don't rely on
intra-step event ordering beyond the sequence numbers — the resume can land
before the control-lifecycle records, as it did here.)

**Reject** is the same request against `/v1/control/reject`. The pause
resolves with `Decision: "reject"`, a `tool.rejected` event carries the
rejection reason, and the gated tool call **fails with that reason instead of
executing** — the tool body never runs. A rejected HITL gate is terminal for
the gated step (D-071): when the rejection resolves a run parked at a step
boundary, the run finishes as a `constraints_conflict` failure (on the task
surface: `task.failed`, error code `constraints_conflict`); when it resolves
an in-flight gated tool call, the planner observes the failure as the step's
outcome.

The wire payload's `token` key is what routes the verdict to the gate. An
`approve`/`reject` **without** a `token` targets the run's own outstanding
pause (the operator-pause shape below) — that is the canonical non-gate
resume path.

## Intervention surface 2 — the OAuth callback

When a planner calls a tool whose OAuth binding has no stored token, the run
parks (`Reason: "external_event"`) and the companion event carries everything
a client needs to send the user into the provider flow:

```text
event: tool.auth_required
data: {…,"payload":{"Source":"mcp","SourceName":"github","BindingScope":"user","AuthorizeURL":"https://github.com/login/oauth/authorize?…&state=<nonce>","State":"<nonce>","PauseToken":"01KTT…","Scopes":["repo","read:user"]}}
```

Your client's only job is to surface `AuthorizeURL` to the user (open a tab,
render a button). The rest is between the user's browser and the Runtime:
the provider redirects to the Runtime's callback —

```text
GET /v1/tools/oauth/callback?state=<nonce>&code=<authorization-code>
```

— which validates the `state` nonce against the pending flow it minted,
exchanges the code, persists the token (encrypted at rest), resumes the
parked pause with `Decision: "resume"`, and serves a static
"Authorization complete" HTML page to the user's tab. No Harbor JWT is
involved on this leg: the unguessable one-time `state` nonce is the bearer
capability, bound at initiation to the pausing identity. On the stream you
observe `tool.auth_completed` followed by `pause.resumed`
(`Decision: "resume"`), and the original tool call proceeds.

Failure shapes on the callback (JSON bodies, except the success page):

| HTTP | `error` | Meaning |
|---|---|---|
| 400 | `invalid_request` / `state_mismatch` / `authorization_denied` | Missing `state`/`code`, identity mismatch, or the user clicked "deny" upstream — a denial consumes the flow and resumes the pause with `Decision: "reject"`. |
| 404 | `flow_not_found` | No in-flight flow matches `state` — including a replayed callback (completion consumes the flow; idempotency by consumption). |
| 410 | `flow_expired` | The flow outlived its TTL before the callback arrived; re-initiate. |
| 502 | `exchange_failed` | The authorization server rejected the token exchange. |
| 503 | `provider_closed` | The provider is shut down. |

This whole OAuth leg — the `tool.auth_required` frame above (its placeholder
`state` / `PauseToken` values included) and the failure-shape table — is
transcribed from the callback handler, the payload type, and their tests
(`internal/tools/auth`), not from a live capture: exercising it end-to-end
needs a real authorization server. The Runtime's OAuth-completion E2Es gate
the shapes on every commit.

The callback path is fixed for `harbor dev` (`/v1/tools/oauth/callback`);
headless embedders may mount it elsewhere, matching their configured
`redirect_url`. Note this route is a provider-redirect mount, not a canonical
Protocol method — it deliberately does not appear in
[methods.md](./methods.md).

## Intervention surface 3 — plain pause / resume (operator)

An operator (or your product) can park a run with no gate involved:

```bash
curl -sS -X POST "$HARBOR_BASE_URL/v1/control/pause" \
  -H "Authorization: Bearer $TOKEN" \
  -H "X-Harbor-Session: $SESSION" \
  -H "Content-Type: application/json" \
  -d '{"identity": {"run": "'$TASK_ID'", "scope": "owner_user"}}'
```

The run parks at the next planner-step boundary (`pause.requested`,
`Reason: "await_input"`) and waits. `POST /v1/control/resume` — same shape,
no `token` needed (it targets the run's own outstanding pause) — resolves it
with `Decision: "resume"` and the planner re-enters, seeing any context you
injected while it was parked (`inject_context` / `user_message` apply to a
parked run and surface on the next step). A `cancel` while parked terminates
the run instead — there is no point waiting for a resume that will never
come.

## Durable pauses: reconnect, then trust pause.list

Pause records are checkpointed through the StateStore (with the run's
serialized trajectory) when the Runtime is configured with a persistent state
driver, so **a pause survives a Runtime restart**: the run can park, the
process can die, and the intervention can land in the new process — the
checkpointed trajectory is restored and the run continues.

The client-side consequence is a rule worth hardcoding: **after any
reconnect, `pause.list` is authoritative — your in-memory event replay is
not.** A restart resets the event bus's sequence numbers and its replay ring;
the pauses themselves persisted. Reconcile your intervention inbox against
`pause.list` on every (re)attach, exactly as you re-snapshot tasks via
`tasks.list` after a `bus.dropped` ([streaming semantics](./streaming-semantics.md)).

Two V1 boundaries, stated so you don't design against more than is promised:
resume must land on a Runtime that can re-attach the pause's non-serializable
tool handles — re-attachment failure fails loudly (`ErrToolContextLost`-shaped,
never a silent resume); and the in-memory state driver checkpoints nothing
durable — durability follows the operator's `state.driver` choice.

## Timeout reaps: DecisionTimeout

Nobody is obliged to answer. When the operator configures a max-park window
(`pauseresume.max_park_duration` in `harbor.yaml`; default `0s` = pauses
never expire), the Runtime's pause sweeper resumes any pause past its
deadline with the typed `timeout` marker — captured here with a 1-second
window:

```text
event: pause.resumed
id: 8
data: {"type":"pause.resumed","sequence":8,…,"run":"01KTT3RUNTIMEOUT0000000000","payload":{"Token":"01KTT3CKF639EWXGRKN3GQQ23F","Reason":"approval_required","Decision":"timeout"}}
```

A timeout is **terminal**: the run finishes as a `constraints_conflict`
failure — a deadline the human missed is a constraint the planner cannot
resolve, never a silent unpark-and-continue. This is why a client should
render deadlines next to pending interventions: an unanswered approval is not
"pending forever", it is "failing at `paused_at + max_park_duration`".

## What pause.resumed tells you

Branch on the typed `Decision` — never parse `Reason` strings (the `Reason`
field echoes why the run *paused*, not how it resolved):

| `Decision` | Produced by | What follows |
|---|---|---|
| `approve` | `POST /v1/control/approve` | `tool.approved`; the gated call runs; the run continues. |
| `reject` | `POST /v1/control/reject`, or an upstream OAuth denial | `tool.rejected`; the gated call fails with the reason; a boundary-pause reject terminates the run (`constraints_conflict`). |
| `resume` | `POST /v1/control/resume`, or the OAuth callback completing | The run re-enters the planner. |
| `timeout` | The max-park sweeper | Terminal: the run fails as `constraints_conflict`. |

## The minimum correct intervention client

```text
subscribe to pause.requested / pause.resumed (plus the tool.* companions
    and notification.* if you want pre-built summaries)
on (re)attach: reconcile against POST /v1/pause/list — it is authoritative
render each open pause: reason, the companion's tool/auth context, and the
    deadline when max-park is configured
route the verdict: approve/reject with the pause token; resume for operator
    pauses; surface AuthorizeURL for OAuth — never invent a second pause path
on pause.resumed: clear the entry, branching on Decision for the epilogue
```

That loop — the same one the Console's intervention queue runs — is the whole
contract. Everything else on this page is detail.
