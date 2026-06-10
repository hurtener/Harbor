# Harbor recipes

Practical, copy-paste how-to guides for common Harbor tasks. Each
recipe is grounded in real, current APIs — every symbol referenced
exists in the tree at the recipe's publication phase. The recipes pair
with the runnable code under [`../../examples/`](../../examples/).

These are task-oriented guides, not reference docs. For the
authoritative design, read `RFC-001-Harbor.md`; for the per-package
API, read the godoc.

## Index

| Recipe | What it covers |
|--------|----------------|
| [Scaffold a new agent project](scaffold-an-agent.md) | `harbor scaffold` — produce a fresh, validated agent project. |
| [Define an in-process tool](define-a-tool.md) | Register a Go function as a Harbor tool with `inproc.RegisterFunc`. |
| [Select and configure a planner](configure-a-planner.md) | The `planner` config block and the swappable-driver seam. |
| [Run the local dev loop](run-harbor-dev.md) | `harbor validate` + `harbor dev` — boot a Runtime on the loopback. |
| [Test an agent](test-an-agent.md) | The public `harbortest` kit — `RunOnce`, `AssertNoLeaks`, `SimulateFailure`. |
| [Embed Harbor headless](embed-harbor-headless.md) | `config.Defaults` → `ValidateCore` → `assemble.Assemble` → drive the run loop in your own Go program — no CLI, no Protocol server. |
| [Steer and resume a run](steer-and-resume-a-run.md) | The ONE pause/resume choreography — two triggers (HITL approval, tool-side OAuth completion) + the durable-pause / max-park lifecycle. |
| [Use memory and skills from Go](use-memory-and-skills-from-go.md) | The canonical skills surface headless: `importer.ImportAndStore` to ingest, the Phase-38 handlers to retrieve, `skills.NewDirectory(...).View` to inject. |
| [Observe an embedded runtime](observe-an-embedded-runtime.md) | The bus-first observability chain — redactor → bus → `telemetry.New` Logger → metrics bridge → tracer bridge — and the engine run-error hook. |

## Conventions used in these recipes

- Shell snippets assume `bin/harbor` is on `PATH` (run `make build`
  first) or invoke it as `./bin/harbor`.
- Go snippets import the public `sdk/` facade
  (`github.com/hurtener/Harbor/sdk/...`, RFC §3.6) — they compile from
  an external Go module and in-tree alike. The standing Phase 112b
  preflight gate (`scripts/smoke/phase-112b.sh`) keeps external
  buildability true.
- Anything destructive or production-sensitive is called out inline.
