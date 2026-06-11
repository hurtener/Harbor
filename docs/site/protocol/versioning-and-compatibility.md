# Choreography 5 — Versioning & compatibility

What the Protocol version promises you, what your client should pin, and what
it must tolerate. This is RFC §5.3 made adopter-facing: the rules below are
the same conventions Harbor's own tooling lives by, promoted to the contract
third-party clients can build against.

> Methods demonstrated: `runtime.info`

## The promise

The Harbor Protocol is versioned **independently of the Runtime
implementation**, as a semver triple pinned in one place in the Runtime's
sources (`internal/protocol/types/version.go`, currently `0.1.0`). The
discipline:

- **Bumping the Protocol version is an RFC change** — never a side effect of
  a Runtime refactor. A Runtime release that adds methods, events, fields, or
  capabilities without breaking anything does NOT bump it.
- **Same major ⇒ compatible.** A client built against `0.1.0` can talk to a
  Runtime speaking any `0.x.y`. Compare the major component mechanically;
  don't string-compare the triple.
- **Breaking changes get a deprecation window.** A breaking change ships as a
  structured deprecation notice (the element being retired, the version it is
  deprecated in, the version it will be removed in, and the migration) before
  removal — third-party clients are never whipsawed. At `0.1.0` the
  deprecation registry is empty: nothing has ever been deprecated.

## Where the version surfaces on the wire

Twice, so you can check it cheaply at attach AND continuously:

**1. The attach handshake — `runtime.info`** (`POST /v1/control/runtime.info`).
A real response from a dev Runtime:

```json
{
  "instance_id": "harbor-dev-192.168.1.7",
  "display_name": "harbor dev",
  "build_version": "v0.0.0-dev",
  "build_commit": "dev",
  "build_go_version": "go1.26.3",
  "protocol_version": "0.1.0",
  "capabilities": [
    "events_subscribe",
    "runtime_posture",
    "task_control"
  ],
  "uptime_seconds": 16
}
```

Note the two version fields: `build_version` is the *Runtime's* release
version (its own semver, moving independently); `protocol_version` is the
wire contract. Pin against the latter.

**2. Every control-surface response** carries `protocol_version` — visible in
every example across this track (`{"accepted": true, "method": "cancel",
"protocol_version": "0.1.0"}`). A long-lived client can detect a Runtime
upgrade mid-session without re-handshaking.

## What to pin

Two things, both read at attach:

1. **The Protocol major you were built against.** Refuse (or warn loudly) on
   a major mismatch — that is the one signal that wire shapes you depend on
   may have changed underneath you.
2. **The capability set you require.** `runtime.info.capabilities` is the
   Runtime advertising which Protocol surfaces are live — capability strings
   like `task_control`, `events_subscribe`, `runtime_posture`,
   `topology_snapshot` (the response shape is
   [`RuntimeInfo`](./types.md#runtimeinfo)). Read it and
   shape your client: the dev Runtime above advertises three capabilities and
   not `topology_snapshot` — a dashboard that wants the topology projection
   degrades that panel instead of crashing. This is how Harbor's own Console
   composes against stripped-down runtimes; third-party clients get the same
   gate.

The worked [event-viewer client](./build-a-client.md) does exactly this —
seven lines: parse `protocol_version`, compare majors, scan `capabilities`
for `events_subscribe`.

## What to tolerate

The flip side of the pin. Two tolerance rules keep your client working across
every non-breaking Runtime upgrade:

### Unknown fields: ignore them

New response fields and new event payload keys are **additive**. A `0.x`
Runtime may start sending fields your client has never seen — in
`runtime.info`, in a task snapshot, in an event payload. Never fail on an
unrecognized field; decode what you know and ignore the rest (the default
behavior of virtually every JSON decoder — don't switch yours to a
reject-unknown mode against this wire). The same applies to enum-shaped
string sets your client branches on: a new event type, a new capability
string, or a new task status in a future minor version must fall into your
`default:` branch, not crash it.

(The Runtime is stricter about what it *accepts* than what you must accept —
an unknown method-specific request shape is rejected `invalid_request`. The
tolerance rule is about what arrives at YOUR decoder.)

### Unknown methods: 404/405 means "not served here", so degrade

A method name outside the Runtime's canonical registry returns the canonical
error envelope, captured live:

```text
POST /v1/control/snapshot_universe
→ HTTP 404
{"code":"unknown_method","message":"method \"snapshot_universe\" is not a canonical Protocol method"}
```

A wrong HTTP verb on a known route returns a bare `405 Method Not Allowed`
(no JSON envelope — it never reaches the Protocol layer). Treat both the same
way: **this runtime does not serve that surface — degrade the feature, don't
crash.** A 404 with `code: "unknown_method"` (or `501` from a driver that
cannot satisfy a capability, e.g. `presign_unsupported`) is a composition
signal, not an error condition.

This convention is load-bearing inside Harbor itself: every phase smoke
script in the repo encodes it as the SKIP discipline —
`scripts/smoke/common.sh::skip_if_404` treats 404/405/501 as "surface not
built yet, skip the assertions" so that newer test suites coexist with older
builds. Your client should encode the same posture: probe (or read
`capabilities`), then enable.

### Don't couple to non-contract details

Two things this track deliberately does NOT promise, so don't pin them:
intra-step **event ordering** beyond the per-bus `sequence` numbers (see the
ordering note in [the pause model](./pause-model.md)), and **`message`
strings** on error envelopes — branch on `code`, never on prose
([errors](./errors.md)).

## The attach sequence, assembled

```text
POST /v1/control/runtime.info
  → protocol_version: compare major against what you were built for
  → capabilities: enable/disable your features
open GET /v1/events; drive the run lifecycle per task-control
on 404 unknown_method / 405 / 501 at any later point: degrade that feature
on a protocol_version major change mid-session (runtime upgraded under
    you): re-handshake, re-snapshot, resubscribe
```

## When the contract does change

The first breaking Protocol change will: bump the major, land as an RFC
change, ship structured deprecation notices for a published window before
removal, and version this documentation track alongside it (these pages
describe `0.x`). Additive evolution — new methods, events, fields,
capabilities — keeps arriving in minors, covered by the tolerance rules
above, and regenerates the [reference pages](./methods.md) in the same
commit, mechanically (`make protocol-docs-gen-check` gates a wire change
whose docs didn't move).
