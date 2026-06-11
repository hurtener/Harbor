# Conformance certification

This track tells you what the Protocol promises; the conformance suite is how
that promise is **enforced**. This page documents what the suite covers, how
to run it, and — precisely — what a pass does and does not claim.

## What the suite is

`internal/protocol/conformance`, in the Harbor repository, is the single
binding pass/fail definition of "the Protocol surface works at version
0.1.0". It runs in Harbor's own CI on every commit; every Runtime release you
can download has passed it.

What it exercises:

- **Every canonical method** — each entry in the methods registry carries a
  happy-path scenario AND a malformed-request scenario. The suite asserts
  this exhaustively at startup: a method added to the registry without its
  scenarios fails the run before any scenario executes.
- **Every canonical error code** — each code in the
  [errors reference](./errors.md) is surfaced by at least one failure
  scenario, with its HTTP binding asserted.
- **Every advertised capability** — the version + capability handshake
  ([versioning & compatibility](./versioning-and-compatibility.md)) is
  observed for every registered capability constant.
- **The auth pipeline** — including the fail-closed legs: HS256
  algorithm-confusion tokens, `alg: none` tokens, and expired tokens are all
  asserted rejected (the asymmetric-only allowlist,
  [auth & identity](./auth-and-identity.md)).
- **The documented event-filter shapes** on the streaming surface.
- **Concurrent reuse** — a mixed-method N=100 concurrent run against one
  shared stack under the race detector.

Every scenario body runs against **two consumer profiles** — the same two a
real client population reaches a Runtime through:

1. **In-process** — direct dispatch against the Protocol control surface, no
   HTTP. The profile an embedder consuming Harbor as a Go library exercises.
2. **Over the wire** — the real HTTP mux (the same one `harbor dev` mounts)
   under an HTTP test server, with the real auth middleware: JWT bearers,
   JSON bodies, status codes.

A pass means the Protocol surface is **consistent across both profiles** —
what an in-process consumer observes and what a remote client observes is the
same contract.

## How to run it

The suite is a standard Go test in the repository:

```bash
git clone https://github.com/hurtener/Harbor
cd Harbor
go test -race ./internal/protocol/conformance/
```

That runs the suite against the canonical stack: real task registry, real
event bus, real state store, real control surface, real wire mux, real JWT
validation over a real ES256 keypair — production drivers on every seam, no
mocks at the boundary.

**Pointing it at a runtime build under test** is what the suite's `Factory`
seam is for. The suite's entry point is `RunSuite(t, factory)`, where the
factory builds a fresh stack per scenario group; Harbor's own gate is exactly
one line over the canonical factory:

```go
func TestProtocol_Conformance(t *testing.T) {
    conformance.RunSuite(t, conformance.NewDefaultFactory(""))
}
```

To certify a modified runtime assembly — a new transport, a different driver
composition, your fork — you wire a `Factory` that assembles YOUR stack
(your control surface, your mux, your token minter) and hand it to the same
`RunSuite`. A future Protocol transport (WebSocket, stdio) is certified
through the same seam: no second conformance implementation, ever.

## What a pass claims

Stated carefully, because a certification that oversells is worse than none:

A conformance pass claims **wire-level compatibility with Harbor Protocol
`0.1.0`**: every canonical method behaves per the
[generated reference](./methods.md) on both consumer profiles — routes,
request/response shapes, error codes with their HTTP bindings, the version
and capability handshake, and the fail-closed auth posture.

What it does **not** claim:

- Nothing about *behavioral quality* behind the surface — planner reasoning,
  LLM output, memory strategy. The suite certifies the contract, not the
  agent.
- Nothing about surfaces outside the canonical registry — a vendor extension
  your runtime adds is invisible to it.
- Nothing about operational properties — throughput, latency, retention
  windows. Those are deployment characteristics, not contract.

## Scope, honestly: this is an in-repo suite today

The suite lives under `internal/` in the Harbor repository and runs with
`go test` from a clone, as shown above. It is **not** published as an
importable package or a standalone certification runner, deliberately: the
right shape for an external self-certification kit (what it exports, how
results are attested) should be designed against real third-party demand,
not speculated. If you are building a client or an alternate runtime surface
and want to certify against the suite from outside the repo —
[open an issue](https://github.com/hurtener/Harbor/issues); that demand is
exactly the signal the export decision is waiting on.

Until then the working posture is: **clients** validate themselves against a
stock Runtime (which the suite has certified) using this track's documented
contract — the [quickstart](./quickstart.md) plus the
[choreographies](./pause-model.md) are designed to be assertable; **runtime
forks and embedders** certify in-tree through the `Factory` seam.
