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

## Conventions used in these recipes

- Shell snippets assume `bin/harbor` is on `PATH` (run `make build`
  first) or invoke it as `./bin/harbor`.
- Go snippets assume the module `github.com/hurtener/Harbor`.
- Anything destructive or production-sensitive is called out inline.
