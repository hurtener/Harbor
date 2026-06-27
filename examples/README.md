# Harbor examples

Runnable, worked examples of the Harbor runtime SDK. Everything here
builds and is exercised by CI (`go test ./examples/...` in the
`examples` job) — a drift in a public surface that breaks an example
fails the build.

For step-by-step how-to guides, see [`docs/recipes/`](../docs/recipes/).

## Layout

```text
examples/
├── README.md              this file
├── harbor.yaml            annotated reference configuration
├── dev.yaml               `harbor dev` loop configuration
├── embed-runonce/         headless assemble → Stack.RunOnce one-call run
├── agents/
│   └── echo/              worked harbortest.Agent + test
└── tools/
    └── weather/           worked in-process tool registration + test
```

## Example configs

| File          | Purpose |
|---------------|---------|
| `harbor.yaml` | Annotated reference config covering every top-level section. Copy and adapt. |
| `dev.yaml`    | The canonical `harbor dev` config. Copy to `harbor.yaml` at your project root. |

Validate either with the in-tree config validator:

```sh
harbor validate ./examples/harbor.yaml
```

## Example agents

[`agents/echo/`](agents/echo/) — `EchoAgent` implements the public
`harbortest.Agent` interface (the same interface `harbor scaffold`
produces) and is driven end-to-end by `harbortest.RunOnce`. Copy the
package, keep the interface satisfaction and the compile-time
assertion, and replace `Run`'s body with your agent's real logic.

## Example programs

[`embed-runonce/`](embed-runonce/) — the embed adopter path end to end:
build a config, `assemble.Assemble` a headless stack, then run a goal
with a single blocking `stack.RunOnce(ctx, goal, identity)` call and
print the answer envelope. Every import is the public `sdk/` facade, so
it builds verbatim from an external module. See
[`docs/recipes/embed-harbor-headless.md`](../docs/recipes/embed-harbor-headless.md)
step 4a for the walkthrough.

## Example tools

[`tools/weather/`](tools/weather/) — registers a Go function as a
Harbor in-process tool via `inproc.RegisterFunc`. The driver derives
JSON Schemas from the typed input/output structs and wraps the
function in the `ToolPolicy` reliability shell. The test shows the
full register → resolve → invoke round-trip.

## Running the examples

```sh
# Build everything under examples/.
go build ./examples/...

# Run the worked-example tests.
go test ./examples/...
```
