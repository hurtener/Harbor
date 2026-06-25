---
description: The task-first map of Harbor's working docs. Know your goal but not which page holds the answer? Find it here — every job routed to the operator skill, the copy-paste Go recipe, and the concept that explains it.
---

# How do I…?

You know what you want to do. This page tells you which page does it.

Harbor's working documentation lives in three tracks, each answering a different
shape of question. This page maps **jobs to pages** — it distills nothing and
links everything. If you came here to learn rather than to do, jump to
[Concepts](/concepts/) or start fresh at [Get Started](/get-started).

## How the working docs are organized

| Track | Answers | Shape | Index |
| --- | --- | --- | --- |
| **Operator skills** | "How do I run this with the CLI / Console?" | Step-by-step playbooks for a `harbor` operator | [/skills/](/skills/) |
| **Recipes** | "How do I do this in Go?" | Copy-paste how-tos against the runtime API | [/recipes/](/recipes/) |
| **Protocol** | "How do I drive this over the wire?" | Choreographies against the versioned Protocol — `curl` is a complete client | [/protocol/](/protocol/) |

::: tip Why three tracks
The split mirrors Harbor's [four-layer architecture](/concepts/architecture):
the CLI and Console are both Protocol clients of a headless Runtime, and the
Runtime is also an embeddable Go library. So the same capability shows up three
ways — as an operator command, as a Go call, and as a wire exchange. Pick the
track that matches how you're holding Harbor.
:::

## I want to…

Each row points at the operator skill to *run* it, the recipe to *code* it, and
the concept to *understand* it. Cells are blank where a track doesn't cover that
job — that's a signal, not an omission.

| I want to… | Operator skill | Go recipe | Concept |
| --- | --- | --- | --- |
| **Start a project** | [Scaffold an agent](/skills/scaffold-a-harbor-agent/SKILL) | [Scaffold an agent](/recipes/scaffold-an-agent) | [Architecture](/concepts/architecture) |
| **Build the agent** | [Define the agent YAML](/skills/define-the-agent-yaml/SKILL) | [Configure a planner](/recipes/configure-a-planner) | [Runtime & planner](/concepts/runtime-and-planner) |
| **Wire an LLM** | [Wire the LLM provider](/skills/wire-the-llm-provider/SKILL) | — see [config reference](/reference/config) | [Governance & security](/concepts/governance-and-security) |
| **Add a tool** | [Add an in-process tool](/skills/add-an-in-process-tool/SKILL) | [Define a tool](/recipes/define-a-tool) | [Tools](/concepts/tools) |
| **Configure memory & skills** | [Configure memory and skills](/skills/configure-memory-and-skills/SKILL) | [Use memory & skills from Go](/recipes/use-memory-and-skills-from-go) | [Memory & skills](/concepts/memory-and-skills) |
| **Drive it interactively** | [Run the dev loop](/skills/run-the-dev-loop/SKILL) · [Drive the Playground](/skills/drive-the-playground/SKILL) | [Run `harbor dev`](/recipes/run-harbor-dev) | [Sessions, tasks & events](/concepts/sessions-tasks-and-events) |
| **Steer & pause a run** | [Use the Harbor Protocol](/skills/use-the-harbor-protocol/SKILL) | [Steer & resume a run](/recipes/steer-and-resume-a-run) | [Pause, resume & steering](/concepts/pause-resume-and-steering) |
| **Observe & debug** | [Observe with the Console](/skills/observe-with-the-console/SKILL) | [Observe an embedded runtime](/recipes/observe-an-embedded-runtime) | [Observability](/concepts/observability) |
| **Embed Harbor in Go** | — | [Embed Harbor headless](/recipes/embed-harbor-headless) | [Runtime & planner](/concepts/runtime-and-planner) |
| **Build a custom client** | [Use the Harbor Protocol](/skills/use-the-harbor-protocol/SKILL) | — | [Build a client](/protocol/build-a-client) |
| **Validate & ship** | [Validate and package](/skills/validate-and-package/SKILL) | [Test an agent](/recipes/test-an-agent) | [Productionization playbook](/reference/productionization-playbook) |

::: details Handling heavy outputs and attachments
Two adjacent jobs that don't fit the table above: when a tool returns large
payloads, route them through the artifact store and control how they reach the
model. See [Control attachment disposition](/recipes/control-attachment-disposition)
and [Provider-native attachments](/recipes/provider-native-attachments), backed
by the [artifacts & context-safety](/concepts/artifacts-and-context-safety)
concept — a runtime-wide invariant fails loudly rather than letting raw heavy
content reach the LLM. For semantic recall, see
[Embed & retrieve](/recipes/embed-and-retrieve).
:::

## Browse by surface

Every operator skill carries a `metadata.surface` tag — the part of Harbor it
plays against. If you know the surface but not the verb, scan here, then open
the full [skills index](/skills/).

| Surface | Covers | Start with |
| --- | --- | --- |
| `cli` | The `harbor` binary — scaffold, dev, validate, package | [Run the dev loop](/skills/run-the-dev-loop/SKILL) |
| `agent-yaml` | The agent manifest the runtime loads | [Define the agent YAML](/skills/define-the-agent-yaml/SKILL) |
| `tools` | The transport-agnostic tool catalog (in-process, HTTP, MCP, A2A) | [Add an in-process tool](/skills/add-an-in-process-tool/SKILL) |
| `llm` | The LLM provider edge | [Wire the LLM provider](/skills/wire-the-llm-provider/SKILL) |
| `memory` | The declared-policy, identity-scoped memory subsystem | [Configure memory and skills](/skills/configure-memory-and-skills/SKILL) |
| `playground` | The interactive single-agent chat surface | [Drive the Playground](/skills/drive-the-playground/SKILL) |
| `console` | The observability & control plane | [Observe with the Console](/skills/observe-with-the-console/SKILL) |
| `protocol` | The versioned wire contract | [Use the Harbor Protocol](/skills/use-the-harbor-protocol/SKILL) |

## Browse the full tracks

When you'd rather read a track end-to-end than chase a single job:

- **[Operator skills](/skills/)** — the CLI and Console playbooks, ordered so the
  first five minutes (scaffold → dev loop → Playground) actually take five minutes.
- **[Recipes](/recipes/)** — focused, copy-paste Go how-tos against the runtime
  API, from scaffolding to embedding Harbor headless.
- **[The Protocol track](/protocol/)** — the wire contract: a
  [15-minute quickstart](/protocol/quickstart), the generated
  [methods](/protocol/methods) / [events](/protocol/events) /
  [errors](/protocol/errors) / [types](/protocol/types) reference, and a guide to
  [building a client](/protocol/build-a-client).

## New here?

::: tip Two on-ramps

- **Just want it running?** [Get Started](/get-started) walks the shortest path
  from clone to a live agent.
- **Want the *why* first?** [Concepts](/concepts/) explains the design — the
  swappable planner, the mandatory identity triple, the unified pause primitive —
  before you touch a command. For design authority, everything defers to the
  [RFC](/reference/rfc).

:::
