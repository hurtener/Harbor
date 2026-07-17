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

- [x] `internal/protocol/client` implements authenticated JSON calls, typed
      Protocol error decoding, SSE framing, `Last-Event-ID`, reconnect inputs,
      context cancellation, and `WithSession` without mutable global identity.
- [x] Request/response methods needed by the first TUI are typed: runtime info
      and health, start, tasks get/list, sessions list/inspect/set-title/delete,
      state history, pause list, control, artifacts put/list, and subscribe.
- [x] `sdk/protocolclient` aliases/forwards only the supported client surface;
      an external-module compile test constructs and calls it.
- [x] Token discovery remains a CLI policy (`HARBOR_TOKEN`, then the existing
      token file); the client accepts an injected `TokenSource` and never reads
      process environment or user files.
- [x] `inspect-events`, `inspect-runs`, and `inspect-topology` use the promoted
      client; duplicate raw REST/SSE parsing is removed from `cmd/harbor`.
- [x] Typed errors preserve HTTP status plus canonical Protocol code; malformed
      JSON/SSE, an incompatible handshake, missing identity, and cancellation
      fail visibly.
- [x] One shared client passes N≥100 concurrent mixed-session calls under
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
    Identity IdentityScope
}

type TokenSource interface {
    Token(context.Context, IdentityScope) (string, error)
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

`TokenSource.Token(ctx, requestedIdentity)` is identity-aware by contract, not
an optional capability. A source must mint, select, or reject a credential for
the requested scope. `StaticToken(token, principal)` is principal-bound and
fails visibly when a regular `WithSession` clone selects another session;
multi-session callers provide a refreshing identity-aware source. An omitted
connection identity remains supported for operator clients whose verified JWT
is the sole identity carrier; a partially populated identity is rejected.
Impersonating clones retarget the execution identity while retaining the
authenticated Actor/Requester principal.

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
- JWT lifetime belongs to the injected token source. The client resolves it for
  every request and SSE connection and never caches, extends, or persists a
  signed credential. Phase 182 owns rotated-file reload and visible in-memory
  replacement for its one-active-session terminal.
- Existing inspect golden output must remain byte-compatible.

## Glossary additions

- Go Protocol client

## Pre-merge checklist

- [x] `make drift-audit` passes
- [ ] `make preflight` passes — orchestrator-owned final broad gate
- [x] `make check-mirror` passes
- [x] All cross-references (`RFC §X.Y`, `brief NN`) resolve
- [x] Coverage on touched packages ≥ stated target
- [x] If multi-isolation paths changed: cross-session isolation test passes
- [x] Concurrent-reuse test passes with N≥100 shared-client invocations under
      `-race`, including cancellation and goroutine-baseline assertions
- [x] Real-driver authenticated integration test covers identity and one
      failure mode under `-race`
- [x] If new vocabulary: glossary updated
- [x] If a brief finding was departed from: N/A; no departure
