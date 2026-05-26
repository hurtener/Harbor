---
name: wire-the-llm-provider
description: "Pick a real LLM provider and wire it through Bifrost — OpenRouter / Anthropic / OpenAI / Azure / NVIDIA NIM / OpenAI-compatible endpoints. Use when configuring the `llm:` block beyond the scaffold default, swapping models, setting model_profiles, or using the dev-only mock escape hatch."
license: Apache-2.0
metadata:
  framework: harbor
  surface: llm
  verbs: ""
---

# Wire the LLM provider

Bifrost is Harbor's LLM driver — one wire surface that speaks many providers. You don't change Go code to swap providers; you change the `llm:` block in `harbor.yaml`. This skill walks the four common postures + the dev-mock escape hatch + the `model_profiles` block the planner needs for context budgeting.

## 1. The four canonical postures

### Posture A — OpenRouter (aggregator, easiest start)

```yaml
llm:
  driver: bifrost
  provider: openrouter
  model: anthropic/claude-haiku-4.5
  api_key: env.OPENROUTER_API_KEY
  timeout: 60s
  model_profiles:
    anthropic/claude-haiku-4.5:
      context_window_tokens: 200000
```

OpenRouter aggregates 100+ models behind one API key. Best for prototyping ("does this model work for my agent?") and for production agents that want provider failover without bespoke wiring. Pricing is per-token, slightly above raw provider list price. Get a key at `openrouter.ai`.

### Posture B — Anthropic direct

```yaml
llm:
  driver: bifrost
  provider: anthropic
  model: claude-haiku-4.5
  api_key: env.ANTHROPIC_API_KEY
  timeout: 60s
  model_profiles:
    claude-haiku-4.5:
      context_window_tokens: 200000
```

Direct API access — usually cheapest per-token + lowest latency. Best when you've committed to Anthropic. Get a key at `console.anthropic.com`.

### Posture C — OpenAI direct

```yaml
llm:
  driver: bifrost
  provider: openai
  model: gpt-4.1-mini
  api_key: env.OPENAI_API_KEY
  timeout: 60s
  model_profiles:
    gpt-4.1-mini:
      context_window_tokens: 1000000
```

Same posture for OpenAI's API. Get a key at `platform.openai.com`.

### Posture D — Custom OpenAI-compatible endpoint (NIM, vLLM, ollama, …)

```yaml
llm:
  driver: bifrost
  provider: nim
  model: nvidia/nemotron-3-super
  timeout: 60s
  custom_providers:
    - name: nim
      base_url: https://integrate.api.nvidia.com/v1
      api_key_env_var: NVIDIA_API_KEY
      models: [nvidia/nemotron-3-super]
  model_profiles:
    nvidia/nemotron-3-super:
      context_window_tokens: 128000
```

For any provider that exposes an OpenAI-compatible endpoint — NVIDIA NIM, vLLM serving, ollama for local LLMs, Together AI, Anyscale, Groq, Mistral, Cohere, Bedrock-compatible gateways. The `custom_providers` block tells Bifrost how to reach it; the `provider:` field references the name. Multiple custom providers can coexist — pick one as the active `provider:`, register the others for swap.

## 2. `model_profiles` — the budgeting contract

`model_profiles.<model>.context_window_tokens` is what the planner consults when it decides how much memory to replay, how much tool output to inline, and when to clip. **Without a profile, the planner falls back to a conservative 32k default — which under-uses big models.** Every model you actually use deserves a profile:

```yaml
model_profiles:
  anthropic/claude-haiku-4.5:
    context_window_tokens: 200000
  anthropic/claude-sonnet-4.5:
    context_window_tokens: 200000
  gpt-4.1-mini:
    context_window_tokens: 1000000
```

Look up the official context-window number from the provider's docs; never guess.

Future fields (post-V1.1) will let you set per-model `output_max_tokens`, `pricing_per_input_token`, and `pricing_per_output_token`. For V1.1, only `context_window_tokens` is honoured.

## 3. The dev-mock escape hatch

CLAUDE.md §13 forbids stub LLMs as production defaults. The boot path is fail-loud: no key set, no provider configured, the binary exits with `ErrMissingAPIKey`. There IS a documented escape hatch for first-clone convenience and CI smoke:

```bash
HARBOR_DEV_ALLOW_MOCK=1 harbor dev
```

When `HARBOR_DEV_ALLOW_MOCK=1` is set, the binary boots with a deterministic stub LLM and prints a stderr banner on every boot:

```text
[DEV-ONLY MOCK LLM — DO NOT USE IN PRODUCTION]
```

The banner is unmissable — it's bright, it's printed on every request to the LLM endpoint, and it's gated by a single env var. Production deployments NEVER set this var; CI smoke runs do, so we don't burn provider tokens validating the boot path. The flag does NOT degrade silently — a `production.yaml` with a misconfigured key + `HARBOR_DEV_ALLOW_MOCK=0` (the default) is still a fail-loud exit.

## 4. Swap models without redeploying

Models are hot-reloadable. With `harbor dev` running:

1. Edit `harbor.yaml`, change `llm.model:` (and add the matching `model_profiles` entry).
2. Save.
3. `harbor dev`'s fsnotify watcher drains in-flight runs, re-reads the config, re-binds Bifrost to the new model, and accepts new runs.

You see this in the runtime stderr:

```text
time=... msg="config reload: llm.model changed" old=claude-haiku-4.5 new=claude-sonnet-4.5
```

The Console's connection footer reflects the new model on the next Task run.

Provider swap (e.g. OpenRouter → Anthropic direct) is the same flow — edit, save, watcher reloads. Bifrost handles the provider handshake internally.

## 5. Timeouts + retries

```yaml
llm:
  # ... provider + model ...
  timeout: 60s                  # request-level timeout
  retry:
    max_attempts: 3             # default 3
    initial_backoff: 1s
    max_backoff: 8s
    jitter: 0.2
```

The retry policy is per-attempt; the `timeout` applies to each individual attempt. Total worst-case wall-clock = `timeout * max_attempts + sum(backoffs)`. Bifrost honours `Retry-After` headers from the provider when present.

Long-running models (deep reasoning, large context) sometimes exceed the default 60s; bump to 120s or 240s for those. The Console's Task page surfaces timeout errors with the provider's verbatim response, so you can tune fast.

## Common failure modes

- **`harbor dev` exits immediately with `ErrMissingAPIKey: env.OPENROUTER_API_KEY not set`.** Source your `.env` or export the var in the shell that ran `harbor dev`. Verify with `echo $OPENROUTER_API_KEY`.
- **`harbor dev` exits with `ErrUnknownProvider: "nim"`.** You set `provider: nim` but forgot the matching `custom_providers:` entry. Add it.
- **Every LLM call times out.** Either your `timeout:` is too low for the model, OR the provider is unreachable from the runtime's network. Check with a `curl https://api.openrouter.ai/v1/models` from the runtime host first.
- **`llm.context_leak` events fire mid-run.** A tool returned >32KB inline instead of an `ArtifactStub`. See [`add-an-in-process-tool`](../add-an-in-process-tool/SKILL.md) §4.
- **The planner clips memory aggressively even on a big-context model.** You forgot the `model_profiles.<model>.context_window_tokens` entry. The planner is using the 32k fallback. Add the entry.

## See also

- [`define-the-agent-yaml`](../define-the-agent-yaml/SKILL.md) — the `llm:` block in the context of the full yaml.
- [`configure-memory-and-skills`](../configure-memory-and-skills/SKILL.md) — memory budgeting against the context window.
- [`observe-with-the-console`](../observe-with-the-console/SKILL.md) — the LLM tab in the Console's Task page shows every prompt/completion.
- Bifrost's full provider matrix: `github.com/maximhq/bifrost`.
- The CONFIG.md reference: `docs/CONFIG.md#llm`.
