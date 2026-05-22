# Recipe: scaffold a new agent project

Produce a fresh, ready-to-build Harbor agent project with `harbor
scaffold`. The generated project passes `harbor validate` and ships a
worked agent plus a `harbortest`-driven test with zero further edits.

## Steps

1. **Build the CLI** (once per clone):

   ```sh
   make build
   ```

2. **Scaffold the project:**

   ```sh
   ./bin/harbor scaffold --name my-agent
   ```

   Flags:

   - `--name` (required) — the project name. Lowercase alphanumeric,
     dash, or underscore.
   - `--template` (optional) — defaults to `minimal-react`.
   - `--output` (optional) — target directory; defaults to `./<name>`.
   - `--json` (optional) — machine-readable output.

3. **Inspect the output.** The `minimal-react` template produces:

   ```text
   my-agent/
   ├── README.md         next-steps guide
   ├── go.mod
   ├── harbor.yaml       production-shaped, pre-validated config
   ├── agent.go          the EchoAgent placeholder
   └── agent_test.go     a harbortest.RunOnce-driven test
   ```

4. **Point `go.mod` at your Harbor checkout.** Until Harbor publishes
   a tagged module release, uncomment the `replace` directive at the
   bottom of the generated `go.mod` and adjust the relative path to
   your local Harbor clone.

5. **Validate and run:**

   ```sh
   harbor validate ./my-agent/harbor.yaml
   harbor dev --config ./my-agent/harbor.yaml
   ```

## Next steps

- Replace `EchoAgent.Run` in `agent.go` with your agent's real logic —
  see [Define an in-process tool](define-a-tool.md) and
  [Select and configure a planner](configure-a-planner.md).
- The worked equivalent of the scaffolded agent lives at
  [`examples/agents/echo/`](../../examples/agents/echo/).
