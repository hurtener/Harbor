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

2. **Fetch the dependencies.** `go.mod` already requires
   `github.com/hurtener/Harbor v1.22.0` — the version
   of the `harbor` binary that scaffolded this project — so it resolves
   straight from the module proxy:

   ```sh
   go mod tidy && go build ./...
   ```

   Building against a **local** Harbor checkout instead? Uncomment the
   `replace` directive at the bottom of `go.mod` and point it at your
   clone.

3. **Validate the config.**

   ```sh
   harbor validate ./harbor.yaml
   ```

   The scaffolded config has already been validated via Harbor's
   in-tree `internal/config.Load + Validate` — every
   `harbor scaffold`-produced project passes the same checks.

4. **Boot the dev loop.**

   ```sh
   harbor dev --config ./harbor.yaml
   ```

5. **Author your agent.** `agent.go` declares the example
   `EchoAgent` — a minimal `harbortest.Agent` that echoes input. Edit
   the body to call your tools, planner, or LLM.

6. **Test your agent.** `agent_test.go` shows a worked example using
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
