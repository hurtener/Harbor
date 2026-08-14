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

`model_profiles.<model>.context_window_tokens` is what the planner consults when it decides how much memory to replay, how much tool output to inline, and when to clip. **A profile for your `llm.model` is effectively REQUIRED: a model with no `model_profiles` entry has no context-window number, so the first LLM call hard-fails with `ErrUnsupportedModel` — there is no silent fallback.** Give every model you actually use a profile:

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

The profile is not a one-field shape: alongside `context_window_tokens`, the documented fields `token_estimator`, `json_schema_mode`, `default_max_tokens`, `reasoning_effort`, `cost_overrides`, `corrections`, and `max_retries` (see `docs/CONFIG.md#llmmodel_profiles`) are all honoured. `output_max_tokens` and per-token pricing (`pricing_per_input_token` / `pricing_per_output_token`) are not profile fields today — leave them out of a profile.

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

## 4b. Broker-pull the provider key (a coordinator custodies it)

By default the provider key resolves from `api_key` (env / literal) once at boot — the **local** source. When a coordinator centrally custodies the key (mint / rotate / revoke, pulled per-runtime, never persisted), set the **broker-pull** source instead so a central rotate lands on the next call with no per-runtime env touch:

```yaml
llm:
  provider: openai
  model: gpt-4o
  # api_key omitted — a brokered primary sources NO local key.
  credential_source: remote          # "" / "local" (default) or "remote"
  inference_broker: openai-broker     # names an inference_brokers[] entry below
  inference_brokers:
    - name: openai-broker
      credential_url: https://coordinator.example.com/runtimes/self/provider-key
      auth_token_env: HARBOR_COORDINATOR_TOKEN   # the runtime's OWN broker credential
      # audience: openai            # optional boot-pinned ceiling
      # scope_ceiling: [chat]       # optional boot-pinned ceiling
      # cache_ttl: 5m               # in-memory serve horizon (default 5m)
```

Rules the boot validator enforces (all fail loud):

- **Brokered XOR local.** A `remote` primary MUST name an `inference_brokers[]` entry AND leave `api_key` empty. Both set, or neither, is a config error.
- **No URL or secret on the wire.** Every sink-determining value (`credential_url`, `auth_token_env`, audience, scope ceiling) lives on the boot-declared broker, referenced by non-secret NAME. The `agent_config.set_llm_provider` Protocol write (below) can only reference a broker by name — it can never carry a URL or an env-var name.
- **Fail-loud, no stale key.** A broker unreachable at connect, or a failed refresh once the cache TTL is crossed, raises `ErrProviderKeyUnavailable`; the runtime NEVER falls back to a local key and NEVER serves a stale key. The pull happens at connect + refresh only — the inference hot path reads the cached key with no per-call broker round-trip.
- **Harbor-orchestrated failover, not the SDK's.** When a coordinator supplies an ordered chain of broker-pulled provider keys/providers, Harbor walks that chain itself at the governance layer: on a *retryable* provider error it advances to the next key/provider, emits a `governance.failover` event (identity + cost + from/to provider), **re-runs the budget / rate-limit / MaxTokens check BEFORE re-issuing** (a trip fails loud and stops the walk — a chain can never spend past a run's per-identity ceiling across hops), and re-issues through the same client. Cross-provider chains are expressible; the fallback keys are broker-pulled and never persisted. Harbor deliberately does NOT hand the provider SDK its native `Fallbacks` array — that would hide every hop from audit + bus + cost. Observe the hops on the event stream (`governance.failover`).

**Rotate / install over the Protocol.** An admin can bind (or rotate) a runtime's brokered provider without editing yaml via `agent_config.set_llm_provider` — a ZERO-URL, admin-only write carrying only `{name, provider, credential_source:"remote", inference_broker, model_allow?}`. A write carrying a `credential_url` / `token_url` / `*_env` / secret field is rejected by name at the wire edge; an unknown broker or a `console:fleet`-only (read) token is rejected loud. See the `use-the-harbor-protocol` skill for the request shape.

## 5. Timeouts + retries

```yaml
llm:
  # ... provider + model ...
  timeout: 60s                  # request-level timeout
  network_defaults:             # applies to every provider (native + custom)
    max_retries: 2              # extra attempts after the first try
    retry_backoff_initial: 1s   # backoff before the first retry
    retry_backoff_max: 8s       # backoff ceiling
  model_profiles:
    anthropic/claude-haiku-4.5:
      context_window_tokens: 200000
      max_retries: 4            # per-model override of network_defaults.max_retries
```

`network_defaults` sets the retry policy for every provider; a `model_profiles.<model>.max_retries` entry overrides it for that one model. The retry policy is per-attempt; the `timeout` applies to each individual attempt. Backoff grows from `retry_backoff_initial` up to the `retry_backoff_max` ceiling. Omit any field and it falls through to Bifrost's package-level default. Bifrost honours `Retry-After` headers from the provider when present.

Long-running models (deep reasoning, large context) sometimes exceed the default 60s; bump to 120s or 240s for those. The Console's Task page surfaces timeout errors with the provider's verbatim response, so you can tune fast.

## 6. Embeddings are a separate block (not an `llm` knob)

Harbor's embedding client is a sibling seam to the chat client — turning text into vectors gets its **own** provider/model/key, configured at the top-level `embeddings:` block, never inherited from `llm.*`:

```yaml
embeddings:
  driver: bifrost                  # the default; omit freely
  provider: openai
  model: text-embedding-3-small
  api_key: env.OPENAI_API_KEY      # same env.NAME convention as llm.api_key
  # dimensions: 256                # optional reduced output dimension
```

You only need it when something consumes embeddings — the opt-in semantic retrieval modes (`memory.retrieval: semantic` / `skills.retrieval: semantic`; see [`configure-memory-and-skills`](../configure-memory-and-skills/SKILL.md)) or your own à-la-carte retrieval (`docs/recipes/embed-and-retrieve.md`). Enabling a semantic mode without the block fails validation loudly naming the missing keys; there is no mock embeddings driver and no fallback to the chat provider.

## Common failure modes

- **`harbor dev` exits immediately with `ErrMissingAPIKey: env.OPENROUTER_API_KEY not set`.** Source your `.env` or export the var in the shell that ran `harbor dev`. Verify with `echo $OPENROUTER_API_KEY`.
- **`harbor dev` exits with `ErrUnknownProvider: "nim"`.** You set `provider: nim` but forgot the matching `custom_providers:` entry. Add it.
- **Every LLM call times out.** Either your `timeout:` is too low for the model, OR the provider is unreachable from the runtime's network. Check with a `curl https://api.openrouter.ai/v1/models` from the runtime host first.
- **`llm.context_leak` events fire mid-run.** A tool returned more than `artifacts.heavy_output_threshold_bytes` (128 KiB by default) inline instead of an `ArtifactStub`. See [`add-an-in-process-tool`](../add-an-in-process-tool/SKILL.md) §4.
- **`harbor dev` fails the first LLM call with `ErrUnsupportedModel: model has no configured ModelProfile`.** Your `llm.model` has no `model_profiles.<model>` entry, so the runtime has no context-window number for it and refuses the call — there is no fallback. Add the `model_profiles.<model>.context_window_tokens` entry.

## See also

- [`define-the-agent-yaml`](../define-the-agent-yaml/SKILL.md) — the `llm:` block in the context of the full yaml.
- [`configure-memory-and-skills`](../configure-memory-and-skills/SKILL.md) — memory budgeting against the context window.
- [`observe-with-the-console`](../observe-with-the-console/SKILL.md) — the LLM tab in the Console's Task page shows every prompt/completion.
- Bifrost's full provider matrix: `github.com/maximhq/bifrost`.
- The CONFIG.md reference: `docs/CONFIG.md#llm` (and `#embeddings` for the embedding client).
