---
description: One Runtime primitive backs HITL approval, tool-side OAuth, A2A AUTH_REQUIRED, and operator pause. Learn the nine-instruction steering taxonomy, the four pause reasons, resume-token identity scoping, and the fail-loud tool-context split.
---

# Pause, resume, and steering

Most agent frameworks grow a different mechanism for every reason a run might
stop: one for human approval, one for interactive OAuth, one for a remote agent
that needs input, one for an operator who hits a button. Each grows its own
state, its own resume path, its own bugs. Harbor draws the seam once.

A run that parks — for any reason — backs onto **one Runtime primitive**, and a
client that understands a single pause choreography understands all of them.
This page distills how that primitive works, the control taxonomy layered on top
of it, and the fail-loud rule that keeps a resumed run honest. For the design
authority, see [the RFC](/reference/rfc) (§3.3 and §6.3); to watch it on the
wire, see [the pause model](/protocol/pause-model); to build it, follow the
[steer-and-resume recipe](/recipes/steer-and-resume-a-run).

## One primitive, four reasons

A run can pause for reasons that look nothing alike on the surface:

| Reason | What triggers it | Who resumes it |
| --- | --- | --- |
| **HITL approval** | A planner gates a side-effecting tool call behind a human | An approver via `approve` / `reject` |
| **Tool-side OAuth** | A tool needs an interactive credential it does not yet hold | The OAuth callback leg completes the flow |
| **A2A `AUTH_REQUIRED` / `INPUT_REQUIRED`** | A remote agent demands authentication or more input | The caller supplies it, then `resume` |
| **Operator `PAUSE`** | An operator (or the Console) parks a live run | An operator via `resume` |

At the Runtime level these are **not four implementations** — they are the same
pause coordinator. Planners and tools only *signal* a need to pause: a planner
emits a `RequestPause` decision, a tool surfaces an authn requirement. The
Runtime owns everything after that — minting the pause, emitting the
protocol-level event, issuing a resume token, and driving the run back to life
when the intervention lands.

::: tip Why this matters
This is Harbor's **primitive-without-its-consumer** discipline made concrete:
the pause primitive shipped *with* a `RequestPause`-emitting consumer, so the
design was validated against a real call site rather than left to drift (see
[the RFC](/reference/rfc), §3.3 and §6.3). The payoff is on the wire — a
Protocol client that handles `pause.requested` once handles HITL, OAuth, A2A,
and operator pause for free.
:::

## The steering taxonomy

Steering is the control surface over a live run, and its taxonomy is **settled**
at nine instructions. The Protocol exposes them as task-control operations
(see [task control](/protocol/task-control)); the planner and Runtime honor
them at well-defined points in the run loop.

| Instruction | Effect |
| --- | --- |
| `INJECT_CONTEXT` | Feed new context into the running reasoning loop without restarting it |
| `REDIRECT` | Change the run's goal or direction mid-flight |
| `CANCEL` | Stop the run (cascade or isolate, per task semantics) |
| `PRIORITIZE` | Reorder scheduling for the targeted work |
| `PAUSE` | Park the run on the unified pause primitive |
| `RESUME` | Release a paused run |
| `APPROVE` | Clear a HITL gate so the gated action proceeds |
| `REJECT` | Deny a HITL gate — **terminal** for that gate |
| `USER_MESSAGE` | Deliver an in-band message to the run |

The nine are a closed set, not an open extension point. New control verbs are an
[RFC change](/reference/rfc), not an ad-hoc addition — which is what keeps every
Protocol client's control surface in lockstep with the Runtime.

## The four pause reasons

Where the steering taxonomy describes *actions a client can take*, the pause
taxonomy describes *why a run is currently parked*. It is settled at four
reasons — the four rows of the table above (HITL approval, tool-side OAuth, A2A,
operator). Every `pause.requested` event carries one of them, so a client can
render the right intervention UI without guessing.

::: warning A rejected HITL gate is terminal
`REJECT` does not park-and-wait — it closes the gate for good. There is no
"reject, then reconsider" path on the same gate; the gated action will not run.
A rejected approval resolves the pause and terminates the run as a constraint
the planner cannot satisfy, not a recoverable signal. Model your UI
accordingly: a rejected approval is an end state, not a pause the user can talk
their way out of. (RFC §6.3.)
:::

## Resume tokens are identity-scoped

When the Runtime parks a run it issues a **resume token**. That token is not a
bare nonce — its authentication is checked against the **original pause's
identity scope** (the `(tenant, user, session)` triple that owned the run when
it paused). A resume attempt that presents the wrong identity scope is rejected;
you cannot lift another tenant's or user's pause by replaying a token.

This is the [identity-and-isolation](/concepts/identity-and-isolation) contract
reaching into pause/resume: identity is mandatory and verified at the resume
boundary, never inferred from the request body. See
[auth and identity](/protocol/auth-and-identity) for how the triple travels on
the wire.

## The tool-context split — fail loud, never drop

Pausing a run that is mid-tool-call raises an honest question: what happens to
the tool's in-flight state? Harbor's answer is a deliberate split:

- **Serializable state** — IDs, configs, plain values the tool needs to
  continue — is checkpointed with the pause and restored on resume.
- **Live handles** — open sockets, file descriptors, streaming connections,
  loggers, callbacks, anything that cannot be serialized — are registered with
  the Runtime under a handle key and re-attached on resume. They are **not**
  silently dropped.

If a resume would require a live handle that can no longer be re-attached, the
Runtime fails **loudly** with `ErrToolContextLost` (naming the missing handle)
rather than quietly resuming with missing context and producing a subtly wrong
result.

::: details Why fail loud instead of degrade?
Silent degradation — `try { ... } catch { return nil }` — is exactly the
silent-context-loss failure mode Harbor's [fail-loud design](/reference/rfc)
exists to close. A pause/resume that dropped a live handle and continued anyway
would reintroduce that bug. `ErrToolContextLost` turns an invisible correctness
hazard into a visible, handleable error: the tool author declares what is
serializable, and anything else a resume depends on surfaces as a hard failure
you can see and fix.
:::

The same discipline runs throughout the Runtime: capabilities are mandatory,
identity is mandatory, and serialization failures are raised, not swallowed.

## See it on the wire

The pause primitive and the steering taxonomy are both first-class on the
[Harbor Protocol](/protocol/), so a `curl`-only client can drive them end to
end:

- **[The pause model](/protocol/pause-model)** — the `pause.requested` /
  `pause.resumed` choreography, with the resume token and decision shapes for
  HITL, OAuth, A2A, and operator pause.
- **[Task control](/protocol/task-control)** — the control-plane operations
  behind the nine steering instructions (cancel, pause, resume, redirect,
  inject, prioritize, approve, reject, user_message).

Because the Console is just another Protocol client, what you see in the UI is
exactly what your own client can do — there is no privileged internal pause
path.

## Build it — steer and resume a run

Ready to drive a pause from start to finish?

- **[Steer and resume a run](/recipes/steer-and-resume-a-run)** — the twinned
  recipe: gate a tool behind approval, observe the pause, and resume (or reject)
  it.
- **[Pause model choreography](/protocol/pause-model)** — the wire-level
  reference the recipe is built on.
- **[The RFC](/reference/rfc)** — design authority for the unified primitive
  (§3.3) and the steering taxonomy (§6.3).
