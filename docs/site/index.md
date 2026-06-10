---
layout: home

hero:
  name: "Harbor"
  text: "Durable, steerable, event-driven AI agents in Go."
  tagline: "A runtime SDK and one static binary. Agents that pause for approval, survive restarts, and stay observable — without the agent's author writing orchestration code."
  actions:
    - theme: brand
      text: Operator skills
      link: /skills/
    - theme: alt
      text: Recipes
      link: /recipes/
    - theme: alt
      text: View on GitHub
      link: https://github.com/hurtener/Harbor

features:
  - title: Durable
    details: "Run state is persisted, not held in a goroutine. Pauses survive a process restart via trajectory checkpoints; serialization failures are loud (ErrUnserializable), never silent."
  - title: Steerable
    details: "Cancel, redirect, inject, pause, resume — one control primitive. Human-in-the-loop approval, tool-side OAuth, and operator pause are the same mechanism, not three."
  - title: Embeddable
    details: "The sdk/ facade makes the runtime importable from any external Go module. A standing CI gate compiles a scaffolded external agent on every commit, so it stays true."
  - title: Multi-isolation from day one
    details: "Every layer carries (tenant, user, session). One user can hold many concurrent sessions and they never see each other's state. The runtime fails closed on missing identity."
  - title: Headless-first
    details: "The Runtime is a Protocol server; the Console is just a client. The same canonical event surface powers a remote attach, a TUI, or a third-party dashboard."
  - title: Swappable reasoning
    details: "The runtime owns mechanism — tasks, tools, memory, events, artifacts, pause/resume. Planners own reasoning policy behind one interface; ReAct and Deterministic ship in the box."
---

## Five minutes to a working agent

```bash
go install github.com/hurtener/Harbor/cmd/harbor@latest

mkdir my-agent && cd my-agent
harbor init                       # tiered harbor.yaml + companion docs
# edit harbor.yaml — uncomment one LLM provider block + set its API key env var
harbor validate ./harbor.yaml     # fail-loud config check
harbor scaffold --name my-agent   # materialise the Go project + worked agent + test
harbor dev                        # local runtime + protocol server on :18080
```

The [first-five-minutes chain](/skills/) walks this path step by step:
[scaffold-a-harbor-agent](/skills/scaffold-a-harbor-agent/SKILL) →
[run-the-dev-loop](/skills/run-the-dev-loop/SKILL) →
[drive-the-playground](/skills/drive-the-playground/SKILL).

## Or embed the runtime in your own program

```go
import (
    _ "github.com/hurtener/Harbor/sdk/drivers/prod"

    "github.com/hurtener/Harbor/sdk/assemble"
    "github.com/hurtener/Harbor/sdk/config"
)

cfg := config.Defaults()
// populate cfg.LLM, validate with cfg.ValidateCore() ...
stack, err := assemble.Assemble(ctx, cfg, assemble.Options{})
```

No CLI, no HTTP listener, no Console — one call turns a validated
config into a running stack. The full path is the
[Embed Harbor headless](/recipes/embed-harbor-headless) recipe.

## Where everything lives

| Surface | Page |
|---------|------|
| Operator skills — numbered playbooks for building agents | [/skills/](/skills/) |
| Recipes — copy-paste how-to guides grounded in current APIs | [/recipes/](/recipes/) |
| Every `harbor.yaml` knob | [Configuration reference](/reference/config) |
| The design RFC | [RFC-001](/reference/rfc) |
| Vocabulary | [Glossary](/reference/glossary) |
| Settled architectural decisions (D-001…) | [Decisions log](/reference/decisions) |
| How Harbor was built, phase by phase | [Master phase plan](/reference/master-plan) |
| Hardening an agent for production | [Productionization playbook](/reference/productionization-playbook) |
| Release history | [Changelog](/reference/changelog) |

This site renders from the canonical files in the
[repository](https://github.com/hurtener/Harbor) — the repo stays the
source of truth; drift between source and site is impossible by
construction. For the per-package API, read the
[godoc](https://pkg.go.dev/github.com/hurtener/Harbor).
