# Harbor-Native TUI Architecture Options

## 1. Recommendation

Build the first generic TUI as a standalone authenticated Harbor Protocol
client:

```text
harbor tui --attach https://runtime.example \
  --token-file ~/.harbor/token \
  --tenant acme \
  --user alice \
  --session s1
```

Use normal REST plus SSE in every mode. Do not introduce an in-memory Runtime
adapter, direct event-bus access, debug endpoint, or Runtime dependency on TUI
code.

Smallest useful release:

- attach and compatibility negotiation;
- one selected session;
- text `start`;
- answer/reasoning streaming;
- terminal reconciliation through `tasks.get`;
- generic task/tool/error blocks;
- cancel;
- `pause.list` plus approve/reject;
- session list/new/switch;
- closed-session resume and erased-session remediation;
- local prompt history and drafts; and
- visible reconnect/replay-gap state.

Defer `harbor serve --tui`, scaffolded `--tui`, public renderer extensions,
full administration screens, full Markdown dependencies, and new Protocol
methods until this slice proves the client and projection boundaries.

## 2. Launch Options

| Mode | Transport | Process model | Order |
|---|---|---|---|
| `harbor tui --attach URL` | network REST/SSE | TUI separate from existing Runtime | first |
| `harbor serve --tui` | same-process loopback REST/SSE | server and TUI are CLI-owned siblings | second |
| scaffolded `--with-tui` agent | same-process loopback REST/SSE | `sdk/server.Handle` plus public TUI client | third |
| in-memory Protocol adapter | direct handler/bus | same process | reject for product |
| child `harbor serve` process | network REST/SSE | TUI supervises server | reserve |

### 2.1 Standalone Attach

Advantages:

- exercises the same surface as Console and third-party clients;
- works locally and remotely;
- does not require `serve.Handle` changes;
- isolates renderer/terminal failure from Runtime operation;
- proves the reusable Protocol client first; and
- avoids competing with production JSON logs in the same terminal.

Tradeoff: the operator must already have a Runtime and valid credentials.

### 2.2 `harbor serve --tui`

This should be sibling composition in `cmd/harbor`:

```text
cmd/harbor
  ├── serve.Boot / Handle.Serve
  └── tui.Run(client connected to bound HTTP address)
```

Sequence:

1. boot the existing production server;
2. start `Handle.Serve` in a joined goroutine;
3. wait for an authoritative ready/bound result;
4. normalize wildcard binds to a local client destination where safe;
5. attach through REST/SSE with ordinary credentials;
6. make TUI-exit/server-exit semantics explicit; and
7. drain both components and restore the terminal.

The current handle does not expose a race-free ephemeral-port readiness
contract. A later co-launch phase may need:

```go
func (h *Handle) WaitReady(ctx context.Context) (addr string, err error)
```

That primitive must ship with `serve --tui` as its first consumer.

Logging issue: production JSON logs and alternate-screen rendering should not
share ordinary stderr. Under co-launch, route logs to a protected file and
surface failures in diagnostics; never silently discard them.

### 2.3 Scaffolded Agent `--tui`

The generated binary should remain headless by default. A future explicit
scaffold option can generate:

```text
harbor scaffold --with-server --with-tui
./my-agent --tui --config harbor.yaml
```

The binary should:

- serve through `sdk/server`;
- attach the public TUI client to the bound Protocol endpoint;
- never expose its tool catalog, planner, event bus, or Runtime handles to the
  renderer; and
- compile TUI dependencies only for `--with-tui` scaffolds.

A child `harbor tui` process would require a separately installed compatible
binary. An in-memory adapter would bypass the exact production boundary being
tested. Same-process loopback HTTP/SSE is the correct default.

## 3. Transport Decision

### Use Same-Process Loopback REST/SSE For Co-Launch

This preserves:

- the canonical mux;
- mandatory JWT middleware;
- identity-scoped SSE;
- `Last-Event-ID` replay;
- the same framing and typed errors as remote attach; and
- realistic detection of configuration, authentication, and projection bugs.

### Reject In-Memory Product Transport

Even an adapter around an internal handler would create another client
transport, remain unavailable through the public server facade, and diverge
from remote attach. Direct bus/surface access is worse: it bypasses JWT,
couples UI to internals, and requires a second replay abstraction.

In-memory handlers remain appropriate in tests only.

### Reserve Child-Process Supervision

A child process improves failure/logging isolation but adds readiness parsing,
signal forwarding, child cleanup, and error propagation. Keep it as a later
operational option if same-process composition proves inadequate.

## 4. Proposed Package Layout

The exact public surface needs RFC review. A plausible sequence is:

```text
internal/
  protocol/client/
    client.go
    stream.go
    compatibility.go
    projection/
      transcript.go
      reconcile.go
  tui/
    app/
    model/
    screens/
    render/
      registry.go
      builtin/
    composer/
    storage/
    theme/
    testkit/

sdk/
  client/               # only when a curated public client is approved
  tui/                  # only when external scaffold use lands

cmd/harbor/
  cmd_tui.go
  cmd_serve.go           # later co-launch composition
```

Keep Bubble Tea and renderer types private initially. Publishing them in an
SDK facade would make visual implementation details part of Harbor's external
compatibility contract before a second consumer exists.

Wire structs remain single-sourced in `internal/protocol/types`; a public
facade may alias or curate them, never define a third schema.

## 5. Client Boundary

A narrow client needs only the first TUI journeys:

```go
type Connection struct {
    BaseURL   string
    Token     TokenSource
    TenantID  string
    UserID    string
    SessionID string
}

type Client interface {
    RuntimeInfo(context.Context) (RuntimeInfo, error)
    RuntimeHealth(context.Context) (RuntimeHealth, error)
    Start(context.Context, StartRequest) (StartResponse, error)
    TasksGet(context.Context, TaskGetRequest) (TaskDetail, error)
    TasksList(context.Context, TaskListRequest) (TaskListResponse, error)
    SessionsList(context.Context, SessionsListRequest) (SessionsListResponse, error)
    StateHistory(context.Context, StateHistoryRequest) (StateHistoryResponse, error)
    PauseList(context.Context, PauseListRequest) (PauseListResponse, error)
    Control(context.Context, ControlRequest) (ControlResponse, error)
    Subscribe(context.Context, StreamOptions) (EventStream, error)
    WithSession(string) Client
}
```

The first client does not need wrappers for all 116 methods. Requirements:

- tolerate unknown response fields;
- branch on typed Protocol status/code, not prose;
- expose compiled Protocol version and wire digest;
- compare them with `runtime.info`;
- use authorization and identity headers;
- support session-specific clones without global mutable state; and
- fail visibly on incompatible major versions.

## 6. Identity Model

Connection scope:

```text
endpoint + credential source + tenant + user
```

Conversation scope adds session. Active-turn scope adds run/task ID.

Rules:

- tenant and user are fixed for a normal connection profile;
- session changes per selected conversation;
- run is attached to the active task, never global;
- each open session owns its stream cursor, reducer, draft, and pending local
  controls;
- session switching cancels and joins the old stream reader; and
- no package-level current identity or current session exists.

New session IDs are client-generated non-empty IDs. First `start` materializes
the Runtime session.

## 7. Authentication

Recommended credential precedence:

1. `--token-file`;
2. `HARBOR_TOKEN`;
3. a protected existing Harbor token file; and
4. secure interactive prompt.

Avoid literal `--token` by default because process arguments are visible.

Connection sequence:

1. resolve the token without logging it;
2. collect tenant, user, and optional session;
3. call `runtime.info`;
4. compare Protocol major and wire digest;
5. inspect capabilities;
6. open the session stream;
7. hydrate snapshots and transcript; and
8. display endpoint and identity clearly.

The Go client should always use `Authorization: Bearer` for SSE. It does not
need the browser-only query-token fallback.

Security defaults:

- require HTTPS for non-loopback endpoints;
- allow plain HTTP automatically only for loopback;
- make any remote insecure override persistent and visible;
- do not mint tokens from `harbor serve` or `sdk/server`; and
- never put tokens in history, logs, terminal titles, preference files, or
  crash output.

## 8. SSE Reader And Reducer

Use one owned cancellable reader goroutine per selected session:

```text
HTTP/SSE reader
  -> bounded ingress/coalescing queue
  -> Bubble Tea Program.Send
  -> pure Update
  -> transcript reducer
  -> typed blocks
  -> render
```

Do not keep a permanently blocking read inside one `tea.Cmd`; it is harder to
cancel and join reliably.

### 8.1 Initial Hydration

1. load tail-first `state.history`;
2. record its tail sequence;
3. open SSE with `Last-Event-ID`;
4. load `tasks.list`, `sessions.inspect`, and `pause.list`;
5. apply replay/live events by sequence; and
6. reconcile task and pause snapshots as authoritative terminal state.

Also reconcile `runtime.health.retention`, `state.history.truncated`, session
`counters_partial`, and aggregate `truncated` signals. A connection can be live
while retained historical context remains explicitly partial.

### 8.2 Live State

Track:

- largest fully processed sequence;
- dedupe key by session and sequence;
- per-run block indexes;
- open tool lifecycle rows;
- active answer/reasoning buffers;
- terminal task state;
- stream state: connecting, live, retrying, gap, failed; and
- snapshot generation/reconciliation marker.

Batch answer and reasoning deltas around 16-40 ms. Deliver intervention,
control, and lifecycle events immediately.

### 8.3 Reconnect

On disconnect:

1. retry with bounded exponential backoff and jitter;
2. send the largest processed sequence as `Last-Event-ID`;
3. dedupe replayed events; and
4. reset backoff after a stable connection.

On `bus.dropped` or replay-unavailable:

1. show a persistent incomplete-history state;
2. reload history, task snapshots, and pause snapshots;
3. rebuild/reconcile the affected session; and
4. resume from the new durable tail.

## 9. Transcript Projection

### 9.1 MVP

Build a pure Go projection independent of Bubble Tea. Inputs:

```text
flat StateEvent stream
+ tasks list/detail snapshots
+ pause snapshots
+ optimistic local user submissions
```

Output:

```text
ordered typed TranscriptBlock values
```

Block kinds:

- user turn;
- answer;
- reasoning;
- planner decision;
- task lifecycle;
- tool lifecycle;
- artifact reference;
- pause/intervention;
- control state;
- cost/result;
- error; and
- unknown event.

Unknown events must remain visible in diagnostics or an unknown block.

### 9.2 Console/TUI Drift

A Go reducer alone does not align the existing TypeScript Console reducer.
Add language-neutral captured fixtures:

```text
testdata/protocol/transcript/
  basic.json
  tools.json
  reasoning.json
  reconnect-gap.json
  unknown-event.json
  expected.json
```

Both reducers should consume the same event frames and expected normalized
projection.

### 9.3 Future Canonical Projection

A wire-level transcript projection could solve durable user turns, stable part
ordering, tool-call identity, and cross-client parity. It requires an RFC for
retention, redaction, erasure, pagination, part IDs, and compatibility and
must ship with an immediate Console or TUI consumer.

Do not block the MVP on it and do not create a TUI-private endpoint.

The projection-completeness gate on the v1.15 base improves flows, memory,
sessions, tasks, and tools, but does not cover transcript-part semantics.
Shared Console/TUI fixtures remain necessary.

## 10. Renderer Registry And Slots

Initial registry should be immutable after construction:

```go
type Renderer interface {
    Kind() BlockKind
    Render(RenderContext, Block) RenderedBlock
}

type Registry struct {
    byKind   map[BlockKind]Renderer
    fallback Renderer
}
```

Dedicated tool-name renderers are optional. The generic fallback is the
primary path because Harbor cannot assume tool names.

Keep any initial slots private and immediately consumed:

- `session_sidebar`;
- `composer_status`;
- `transcript_after`;
- `runtime_status`; and
- `intervention_footer`.

Do not ship an empty public plugin framework or Go runtime plugins. When a
scaffolded customization use case exists, expose statically linked constructor
options and its first consumer in the same wave.

## 11. Configuration And Persistence

TUI preferences do not belong in Runtime `harbor.yaml`. Use separate local
configuration, for example:

```yaml
version: 1

connection:
  profile: local

profiles:
  local:
    url: http://127.0.0.1:8686
    tenant: acme
    user: alice
    token_source:
      kind: file
      path: ~/.harbor/token

appearance:
  theme: harbor-dark
  color_mode: auto
  reduced_motion: false
  mouse: true
  reasoning: collapsed

stream:
  reconnect_max: 30s
  visual_batch_interval: 24ms
  ingress_capacity: 1024
```

Persist only:

- connection metadata without bearer tokens;
- prompt history;
- drafts/stashes;
- theme and keybinding preferences;
- recent session IDs as references; and
- client-local queued follow-ups, explicitly labelled local.

Runtime sessions, tasks, tools, events, artifacts, pauses, agents, and memory
are always reloaded from Protocol surfaces.

Use standard config/state directories, mode 0700 directories and 0600 files
where supported, bounded NDJSON/JSON, and atomic replacement.

## 12. Security

- Keep JWT mandatory on loopback.
- Reject remote plaintext HTTP by default.
- Strip or escape server-provided ANSI/control sequences.
- Permit only approved hyperlink schemes.
- Require explicit OSC 52 clipboard actions.
- Confirm OAuth/external-open actions.
- Use argv execution, never a shell.
- Do not execute MCP App HTML in the terminal.
- Keep heavy content artifact-backed.
- Show unknown and unsupported data rather than dropping it.
- Confirm destructive actions with visible identity/session target.
- Gate admin/fleet controls on effective verified scopes.

## 13. Testing

### Client

- authenticated REST and identity headers;
- typed error mapping;
- Protocol major and wire-digest compatibility;
- unknown-field tolerance;
- SSE multiline/comment/retry/id/EOF/malformed fixtures;
- header auth, never query-token use;
- controllable reconnect clock and cancellation.

### Reducer

- shared Console/TUI fixtures;
- duplicate/out-of-order sequences;
- tool lifecycle matching;
- answer/reasoning coalescing;
- terminal task reconciliation;
- `session.reopened` invalidating stale closed-session state;
- terminal `session_erased` remediation;
- lower-bound session counters and scoped retention horizons;
- capability-gated tool annotations and unavailable/best-effort analytics;
- missing user-turn join;
- unknown-event fallback;
- replay-gap rebuild; and
- cross-session isolation with N>=100 concurrent reducers.

### Lifecycle

- reader cancellation and join;
- server-first and TUI-first failure;
- terminal restoration;
- ephemeral-port readiness;
- shared shutdown;
- Runtime crash reporting;
- race detector and goroutine leak gates.

### End To End

Use a real authenticated server and Protocol paths to:

1. connect;
2. start a run;
3. stream events;
4. reconcile terminal task state;
5. exercise pause/approval;
6. force disconnect and replay;
7. force replay gap and resnapshot;
8. run multiple identities/sessions concurrently; and
9. assert terminal cleanup and no goroutine leaks.

## 14. Rollout Sequence

### A. RFC And Dependency Decision

Decide CLI surface, Bubble Tea stack, client-local config, first screen set,
and explicit coding-agent exclusions.

### B. Client Plus First TUI Consumer

Ship together:

- curated Go Protocol client;
- SSE reconnect;
- pure transcript reducer;
- shared projection fixtures;
- `harbor tui --attach`;
- connect screen;
- one-session conversation;
- start/cancel;
- terminal reconciliation; and
- generic rendering.

### C. Sessions And Interventions

Add session navigation/history, closed-session resume, erased-session
remediation, `pause.list`, approve/reject, initial-turn artifact upload, local
drafts/history, and replay-gap resnapshot.

### D. `harbor serve --tui`

Ship readiness contract, ephemeral-port lifecycle, same-process attach,
logging coexistence, shared shutdown, and live smoke together.

### E. Scaffolded TUI

Ship public client/TUI facade, `--with-tui` scaffold option, generated flag,
external-module compile and authenticated integration test together.

### F. Fidelity

Add command palette, sidebar, reasoning collapse, task/result panels, observed
live cost without claiming authoritative task-cost completeness, renderer
specializations, themes, terminal degradation, and transcript export.

### G. Optional Protocol Projection

Only after measured reducer drift: design a canonical turn/part projection and
migrate a Console or TUI consumer in the same wave.

## 15. RFC Decisions Required

- new `harbor tui` subcommand;
- `harbor serve --tui` semantics;
- scaffold `--with-tui` and generated `--tui`;
- TUI dependency stack;
- any new public SDK client/TUI facade;
- canonical transcript/turn Protocol additions;
- credential persistence beyond token-source references;
- public plugin ABI; and
- any expansion into coding-agent functionality.

## 16. Primary Sources

- `RFC-001-Harbor.md:44,210-242,1298-1300,1354-1363`
- `internal/runtime/serve/serve.go`
- `internal/runtime/serve/mux.go`
- `internal/runtime/serve/external/external.go`
- `internal/protocol/transports/transports.go`
- `internal/protocol/transports/stream/stream.go`
- `sdk/server/server.go`
- `cmd/harbor/cmd_serve.go`
- `cmd/harbor/scaffold/templates/minimal-react/cmd_main.go.tmpl`
- `web/console/src/lib/sessions/history.ts`
- `docs/site/protocol/streaming-semantics.md`
- `docs/site/protocol/versioning-and-compatibility.md`
