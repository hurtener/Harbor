---
description: The newcomer on-ramp to Harbor — scaffold an agent, wire a provider, run the dev loop, and chat with a running, steerable agent in under five minutes.
---

# Get Started

Harbor is a Go-native runtime for durable, steerable, event-driven AI agents. It ships as one static binary, `harbor`, and a four-layer architecture — a headless [Runtime](/concepts/architecture), a versioned [Protocol](/protocol/), a [Console](/skills/observe-with-the-console/SKILL), and the CLI — so everything that observes or controls an agent does so over the same wire. This page is the shortest path from a blank directory to a working agent you can talk to.

## What you'll have in five minutes

A running agent you can chat with, watch, and steer mid-run:

- A `harbor.yaml` describing one agent, one LLM provider, and a planner.
- A Runtime serving the [Harbor Protocol](/protocol/) on `127.0.0.1:18080`.
- The Console attached over that Protocol — the same path a remote browser or a third-party client would use. There is no privileged internal view; the CLI and the Console are both Protocol clients.
- A Playground where you can send messages, attach files, and issue steering instructions (pause, redirect, approve) against a live run.

::: tip First-success target
The `scaffold` → `dev-loop` → `playground` chain is designed to land in **under five minutes** from a fresh clone. Each step below links to its canonical skill — the playbooks are the source of truth; this page only sequences them.
:::

## Prerequisites

| You need | Why | Reference |
| --- | --- | --- |
| Go 1.26+ | Harbor builds CGo-free as a single static binary on Go 1.26 or newer. | [RFC-001](/reference/rfc) |
| The `harbor` binary | The one CLI that drives scaffold, dev, validate, and console. | [scaffold-a-harbor-agent](/skills/scaffold-a-harbor-agent/SKILL) |
| One LLM provider key | Harbor fails loudly at boot if no real provider is configured — there is no silent stub fallback. | [Configuration reference](/reference/config) |

::: warning A real provider is mandatory
Harbor refuses to boot against a stub on the golden path. With no provider configured, the binary exits non-zero and names the missing config key. A dev-only mock exists behind an explicit flag and prints a `DEV-ONLY MOCK LLM` banner on every boot — it is never the default. See [wire-the-llm-provider](/skills/wire-the-llm-provider/SKILL).
:::

## The five-minute path

Each step is a numbered playbook. Follow them in order.

### 1. Scaffold the agent

`harbor init` drops a tiered, commented `harbor.yaml` plus companion docs; `harbor scaffold` materialises the Go project around it.

```bash
harbor init --target ~/my-first-agent
harbor scaffold --name my-first-agent
```

→ [scaffold-a-harbor-agent](/skills/scaffold-a-harbor-agent/SKILL) · recipe: [scaffold-an-agent](/recipes/scaffold-an-agent)

### 2. Define `agent.yaml`

The scaffold writes a tiered config — `REQUIRED` → `COMMON` → `ADVANCED`. The skill explains every field: identity, planner, tools, memory, and the LLM block.

→ [define-the-agent-yaml](/skills/define-the-agent-yaml/SKILL) · full schema: [Configuration reference](/reference/config)

### 3. Wire the LLM provider

Uncomment exactly one provider block and set `api_key: env.YOUR_KEY_NAME`. Bifrost — Harbor's LLM driver — speaks many providers under one wire surface, so you swap providers by editing yaml, not Go.

```yaml
llm:
  driver: bifrost
  provider: openrouter
  model: anthropic/claude-haiku-4.5
  api_key: env.OPENROUTER_API_KEY
  timeout: 60s
```

→ [wire-the-llm-provider](/skills/wire-the-llm-provider/SKILL)

### 4. Run the dev loop

`harbor dev` boots the Runtime; `harbor console` serves the Console and attaches over the Protocol. The single-process posture needs no auth ceremony.

```bash
harbor console   # boots a co-resident Runtime, prints the URL, one-click attach
```

→ [run-the-dev-loop](/skills/run-the-dev-loop/SKILL) · recipe: [run-harbor-dev](/recipes/run-harbor-dev)

### 5. Drive the playground

Open the printed Console URL, attach, and chat. Send messages, upload files, and steer the run mid-flight.

→ [drive-the-playground](/skills/drive-the-playground/SKILL)

::: details Validate before you commit
Once the chain works, `harbor validate harbor.yaml` is a fail-loud config check and `make preflight` is the build-boot-smoke gate. See [validate-and-package](/skills/validate-and-package/SKILL) and the recipe [test-an-agent](/recipes/test-an-agent).
:::

## Then what?

Pick the fork that matches your intent.

| I want to… | Go to |
| --- | --- |
| **Learn the concepts** — how the Runtime, Planner, identity model, and pause/resume primitive fit together | [Concepts](/concepts/) → start with [Architecture](/concepts/architecture) |
| **Find a how-to** — task-shaped recipes for tools, planners, memory, steering, attachments | [Recipes](/recipes/) |
| **Build a Protocol client** — talk to the Runtime directly from any language; `curl` is a complete client | [Protocol quickstart](/protocol/quickstart) and [use-the-harbor-protocol](/skills/use-the-harbor-protocol/SKILL) |
| **Go to production** — the persistence triad, governance, JWT auth, and the deployment checklist | [Productionization playbook](/reference/productionization-playbook) |

## Choose your shape

Harbor runs in three shapes off the same Runtime. None is privileged over another — the Console and the CLI both attach over the [Protocol](/protocol/).

| Shape | What it is | When to reach for it |
| --- | --- | --- |
| **Single static binary** | `harbor dev` / `harbor console` drive the Runtime + Console out of the box. CGo-free, SQLite and Postgres both compiled in. | First runs, demos, self-contained deployments. |
| **Embedded Go SDK** | Import the Runtime into your own Go service; drive runs in-process, observe the typed event bus directly. | You already have a Go service and want agents inside it. See [embed-harbor-headless](/recipes/embed-harbor-headless). |
| **Headless Runtime + Console** | Run the Runtime on a server, attach a Console (or a custom client) over the Protocol from anywhere. | Multi-node, remote attach, third-party or white-label clients. |

For the seam that makes all three possible — why the Runtime is headless and everything observes it over a versioned wire — read [Architecture](/concepts/architecture). For the embedded path end to end, follow [embed-harbor-headless](/recipes/embed-harbor-headless).

::: tip The design authority is the RFC
Every page on this site distills the canonical skills, recipes, and reference docs. When a detail matters, the design source of truth is [RFC-001](/reference/rfc).
:::
