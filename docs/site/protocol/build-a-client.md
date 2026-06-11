# Build a client

The shortest credible Protocol client, worked end to end: an **event viewer**
— it authenticates, handshakes, and renders one session's live event stream.
Roughly a hundred lines of Go, **stdlib only**: no Harbor import, no SDK, no
generated code. That is the point of this page — the Protocol is the
integration surface, and `net/http` + `encoding/json` is a complete toolkit
for it.

> Methods demonstrated: `runtime.info`, `events.subscribe`

The full source lives in the repository at
[`examples/protocol-clients/event-viewer/`](https://github.com/hurtener/Harbor/tree/main/examples/protocol-clients/event-viewer)
and is **compile-gated by the project's preflight smoke** — a wire-type or
route change that breaks this client fails the build, so the listing below
cannot rot.

## Run it first

```bash
harbor dev &                                  # any Harbor Runtime works
go run ./examples/protocol-clients/event-viewer
```

Real output against a dev Runtime running one short task:

```text
runtime harbor-dev-192.168.1.7 (build v0.0.0-dev) speaks Protocol 0.1.0
tailing session "event-viewer" — Ctrl-C to stop
     5  session.opened               run=                           {"SessionID":"event-viewer","OpenedAt":1781139747555658000}
     6  task.spawned                 run=                           {"TaskID":"01KTT37CQ3317D7DH8QRZC4YRZ","Kind":"foreground","ParentTaskID":"","Priority":0,"IdempotencyKey":""}
     7  task.started                 run=                           {"TaskID":"01KTT37CQ3317D7DH8QRZC4YRZ","PriorState":"pending"}
    10  llm.completion.chunk         run=01KTT37CQ3317D7DH8QRZC4YRZ {…,"Delta":"mock:User ","Done":false,"Kind":"content",…}
    15  planner.decision             run=01KTT37CQ3317D7DH8QRZC4YRZ {…,"DecisionKind":"Finish","Tool":"",…}
    16  task.completed               run=                           {"TaskID":"01KTT37CQ3317D7DH8QRZC4YRZ"}
```

## The three moves

Every Protocol client makes the same three moves the event viewer makes;
everything richer (a dashboard, a TUI, an intervention inbox) is these three
plus rendering.

### 1. Get a token

A token authenticates **who** — `(tenant, user)` plus scopes; the
conversation is chosen per-request via the `X-Harbor-Session` header
([auth & identity](./auth-and-identity.md)). The event viewer takes
`HARBOR_TOKEN` from the environment, falling back to the loopback-only dev
bootstrap for the `harbor dev` case:

```go
token := os.Getenv("HARBOR_TOKEN")
if token == "" {
    var boot struct {
        Token string `json:"token"`
    }
    mustPost(baseURL+"/v1/dev/bootstrap.json", "", "", []byte(`{}`), &boot)
    token = boot.Token
}
```

In production there is no bootstrap endpoint — your identity provider mints
the JWT.

### 2. Handshake: runtime.info

Pin what you were built against; tolerate what you don't know
([versioning & compatibility](./versioning-and-compatibility.md)):

```go
var info struct {
    InstanceID      string   `json:"instance_id"`
    BuildVersion    string   `json:"build_version"`
    ProtocolVersion string   `json:"protocol_version"`
    Capabilities    []string `json:"capabilities"`
}
mustPost(baseURL+"/v1/control/runtime.info", token, session, []byte(`{"identity":{}}`), &info)

const builtAgainstMajor = "0" // Protocol 0.x — same major ⇒ compatible
if strings.Split(info.ProtocolVersion, ".")[0] != builtAgainstMajor {
    fail("protocol major version skew: runtime speaks %s, this client was built against %s.x",
        info.ProtocolVersion, builtAgainstMajor)
}
capable := false
for _, c := range info.Capabilities {
    capable = capable || c == "events_subscribe"
}
if !capable {
    fail("this runtime does not advertise the events_subscribe capability")
}
```

Note what the struct does NOT declare: `display_name`, `uptime_seconds`, and
any field a future minor adds. A partial decode that ignores unknown fields
is the unknown-field tolerance rule, implemented for free by `encoding/json`.

### 3. Tail the stream

SSE is line-oriented; a `bufio.Scanner` is a complete SSE parser for this
wire. `Last-Event-ID: 0` replays the session's retained history before
live-tailing ([streaming semantics](./streaming-semantics.md)):

```go
req, err := http.NewRequest(http.MethodGet, baseURL+"/v1/events", nil)
if err != nil {
    fail("build events request: %v", err)
}
req.Header.Set("Authorization", "Bearer "+token)
req.Header.Set("X-Harbor-Session", session)
req.Header.Set("Last-Event-ID", "0")
resp, err := http.DefaultClient.Do(req)
// … status check …

scanner := bufio.NewScanner(resp.Body)
scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
var eventType, id string
for scanner.Scan() {
    line := scanner.Text()
    switch {
    case strings.HasPrefix(line, "event: "):
        eventType = strings.TrimPrefix(line, "event: ")
    case strings.HasPrefix(line, "id: "):
        id = strings.TrimPrefix(line, "id: ")
    case strings.HasPrefix(line, "data: "):
        var frame struct {
            Run     string          `json:"run"`
            Payload json.RawMessage `json:"payload"`
        }
        data := strings.TrimPrefix(line, "data: ")
        if err := json.Unmarshal([]byte(data), &frame); err != nil {
            fail("malformed data frame: %v", err) // a broken frame is a wire bug — fail loud
        }
        fmt.Printf("%6s  %-28s run=%-26s %s\n", id, eventType, frame.Run, frame.Payload)
    }
}
```

The viewer keeps `payload` as `json.RawMessage` and prints it — it consumes
the stream *generically*, which is why it survives event-catalog growth
untouched. A client that branches on types decodes per-type shapes from the
[events reference](./events.md) inside that `data:` case.

What the teaching artifact deliberately leaves out, and what a production
client adds: **reconnect** (remember the largest `id:`, reopen with
`Last-Event-ID` on drop), **re-snapshot** on `bus.dropped` /
`stream.replay_unavailable`, and a **steering surface** (`start`, `cancel`,
the intervention verbs — all single POSTs; see
[task control](./task-control.md) and [the pause model](./pause-model.md)).
The closing pseudocode in [streaming semantics](./streaming-semantics.md) is
the upgrade path.

## The two doors up

When the event viewer stops being enough, two reference implementations show
the full pattern — read them, vendor from them:

1. **The Console's TypeScript client module** —
   `web/console/src/lib/protocol.ts` in the Harbor repo: every wire type plus
   a typed `HarborClient`, the module the bundled Console runs on. It is
   **hand-maintained in lockstep with the Go wire types and kept honest by
   the Console's own CI** — there is no TS generator today (that remains an
   open deferral, D-132), so treat it as a high-quality reference to vendor,
   not as generated ground truth. The generated ground truth is the
   [types reference](./types.md), regenerated from the Go sources on every
   wire change.
2. **The Console itself** — the canonical full client: chat, fleet
   observation, the intervention queue, artifacts, topology. Everything it
   renders arrives through the same surface documented in this track; it
   holds no privileged access (RFC §5.1). When you wonder "how should a
   client consume X?", the Console's consumption of X is the worked answer.

## The closing door: prove compatibility

When your client (or your own Runtime-side implementation of this contract)
needs a compatibility claim stronger than "it works on my machine", the
[conformance certification](./conformance-certification.md) page documents
the suite Harbor itself is gated on, and exactly what a pass claims.
