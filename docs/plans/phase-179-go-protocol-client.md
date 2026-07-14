# Phase 179 — Go Protocol client foundation

## Summary

Promote Harbor's duplicated command-local REST/SSE logic into one typed,
authenticated Go Protocol client at `internal/protocol/client`, with a curated
`sdk/protocolclient` facade. Convert the shipped `inspect-*` commands in the
same phase so the client has production consumers before the TUI depends on it.

## RFC anchor

- RFC §3.6
- RFC §5.1
- RFC §5.3
- RFC §5.4
- RFC §5.5
- RFC §8

## Briefs informing this phase

- brief 06
- brief 07
- brief 12

## Brief findings incorporated

- brief 06 §1 and §8: CLI, Console, TUI, and third-party clients consume the
  same canonical Protocol; no client gets an internal Runtime view.
- brief 06 §5: a client must reconcile one canonical event stream rather than
  introduce a second streaming channel or command-private endpoint.
- brief 07 §10: identity and correlation identifiers are explicit and stable;
  client state must not rely on process-global current-session state.
- brief 12 §2: co-located and remote interfaces use the same typed Protocol
  path; deployment convenience does not create an in-memory adapter.

## Findings I'm departing from (if any)

None. The TUI dossier recommends a narrow first client rather than generating
wrappers for all canonical methods; this phase follows that recommendation and
keeps an extensible typed `Call` core for later namespaces.

## Goals

- Provide one concurrent-safe client for REST calls and SSE subscriptions.
- Promote token resolution needed by CLI consumers without making environment
  lookup part of the reusable client.
- Expose a curated external facade usable by generated agent binaries.
- Delete the equivalent HTTP/SSE implementation from `inspect_common.go`.

## Non-goals

- No TUI rendering or Bubble Tea dependency.
- No Protocol method, event, error, capability, or version change.
- No automatic anonymous/dev authentication fallback.
- No complete 116-method convenience-wrapper inventory.

## Acceptance criteria

- [ ] `internal/protocol/client` implements authenticated JSON calls, typed
      Protocol error decoding, SSE framing, `Last-Event-ID`, reconnect inputs,
      context cancellation, and `WithSession` without mutable global identity.
- [ ] Request/response methods needed by the first TUI are typed: runtime info
      and health, start, tasks get/list, sessions list/inspect/set-title/delete,
      state history, pause list, control, artifacts put/list, and subscribe.
- [ ] `sdk/protocolclient` aliases/forwards only the supported client surface;
      an external-module compile test constructs and calls it.
- [ ] Token discovery remains a CLI policy (`HARBOR_TOKEN`, then the existing
      token file); the client accepts an injected `TokenSource` and never reads
      process environment or user files.
- [ ] `inspect-events`, `inspect-runs`, and `inspect-topology` use the promoted
      client; duplicate raw REST/SSE parsing is removed from `cmd/harbor`.
- [ ] Typed errors preserve HTTP status plus canonical Protocol code; malformed
      JSON/SSE, an incompatible handshake, missing identity, and cancellation
      fail visibly.
- [ ] One shared client passes N≥100 concurrent mixed-session calls under
      `-race` with no context bleed, cancellation cross-talk, or goroutine leak.

## Files added or changed

- `internal/protocol/client/`
- `sdk/protocolclient/`
- `cmd/harbor/inspect_common.go`
- `cmd/harbor/cmd_inspect_*.go`
- `test/integration/protocol_client_test.go`
- `docs/skills/use-the-harbor-protocol/SKILL.md`
- `docs/site/skills/use-the-harbor-protocol/SKILL.md`
- `scripts/smoke/phase-179.sh`

## Public API surface

```go
type Connection struct {
    BaseURL string
    Token   TokenSource
}

type Client interface {
    RuntimeInfo(context.Context) (RuntimeInfo, error)
    RuntimeHealth(context.Context) (RuntimeHealth, error)
    Start(context.Context, StartRequest) (StartResponse, error)
    Subscribe(context.Context, StreamOptions) (EventStream, error)
    WithSession(string) Client
}
```

The concrete interface also carries the typed session/task/history/pause/
control/artifact methods named in the acceptance criteria.

## Test plan

- **Unit:** JSON calls, canonical error mapping, URL joining, SSE multiline/id/
  retry/comment/EOF/malformed fixtures, token-source failures, unknown fields.
- **Integration:** real authenticated Protocol mux with in-memory production
  drivers; runtime handshake, start, stream, task terminal read, missing-token
  failure, and identity propagation.
- **Conformance:** external-module `sdk/protocolclient` compile gate and existing
  inspect-command golden outputs.
- **Concurrency / leak:** N≥100 calls through one shared client with distinct
  sessions and one cancelled subset; reader goroutines return to baseline.

## Smoke script additions

- Run the focused client and inspect-command tests under `-race`.
- Assert no command-local SSE parser remains outside the client package.
- Compile the external facade consumer.

## Coverage target

- `internal/protocol/client`: 90%
- `sdk/protocolclient`: 80%
- touched `cmd/harbor` client paths: 80%

## Dependencies

- 60
- 61
- 118
- 159
- 160

## Risks / open questions

- The public facade must stay curated; exposing every internal wire type would
  turn implementation packages into an accidental SDK contract.
- SSE reconnect policy belongs to callers; the client exposes cursors and
  framing, while the TUI phase owns backoff and snapshot reconciliation.
- Existing inspect golden output must remain byte-compatible.

## Glossary additions

- Go Protocol client

## Pre-merge checklist

- [ ] `make drift-audit` passes
- [ ] `make preflight` passes
- [ ] `make check-mirror` passes
- [ ] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [ ] Coverage on touched packages ≥ stated target
- [ ] If multi-isolation paths changed: cross-session isolation test passes
- [ ] Concurrent-reuse test passes with N≥100 shared-client invocations under
      `-race`, including cancellation and goroutine-baseline assertions
- [ ] Real-driver authenticated integration test covers identity and one
      failure mode under `-race`
- [ ] If new vocabulary: glossary updated
- [ ] If a brief finding was departed from: justified above + decisions.md entry filed
