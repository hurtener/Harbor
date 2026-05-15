# acme-agent

A Harbor agent project scaffolded from the `minimal-react` template.

## Next steps

1. **Set your LLM API key.** The scaffolded `harbor.yaml` reads the
   key from `OPENROUTER_API_KEY` at boot:

   ```sh
   export OPENROUTER_API_KEY=sk-or-...
   ```

   To use a different provider, edit `harbor.yaml` — `llm.provider`,
   `llm.model`, `llm.api_key` (in `env.NAME` form), and add a matching
   `llm.model_profiles.<model>` entry.

2. **Validate the config.** Once `harbor validate` ships, run:

   ```sh
   harbor validate ./harbor.yaml
   ```

   Until then, the scaffolded config has already been validated via
   Harbor's in-tree `internal/config.Load + Validate` — every
   `harbor scaffold`-produced project passes the same checks.

3. **Boot the dev loop.** Once `harbor dev` ships:

   ```sh
   harbor dev --config ./harbor.yaml
   ```

4. **Author your agent.** `agent.go` declares the example
   `EchoAgent` — a minimal `harbortest.Agent` that echoes input. Edit
   the body to call your tools, planner, or LLM.

5. **Test your agent.** `agent_test.go` shows a worked example using
   the public `github.com/hurtener/Harbor/harbortest` package:

   ```sh
   go test ./...
   ```

## Layout

```text
acme-agent/
├── README.md         (this file)
├── go.mod
├── harbor.yaml       Harbor runtime configuration
├── agent.go          your agent code
└── agent_test.go     harbortest-driven test
```

## References

- Harbor README: <https://github.com/hurtener/Harbor>
- `harbortest` godoc: <https://pkg.go.dev/github.com/hurtener/Harbor/harbortest>
