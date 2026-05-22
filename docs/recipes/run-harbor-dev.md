# Recipe: run the local dev loop

`harbor dev` boots a local Runtime + Protocol server on the loopback,
mints an ephemeral dev token, and serves until you stop it. Pair it
with `harbor validate` as a pre-boot check.

## Steps

1. **Build the CLI:**

   ```sh
   make build
   ```

2. **Write a config.** Start from the annotated reference at
   [`examples/dev.yaml`](../../examples/dev.yaml) — copy it to
   `harbor.yaml` at your project root. The two fields you almost
   always edit:

   - `identity.issuer` / `identity.audience` / `identity.jwks_url` —
     point at a real OIDC provider for non-local deployments. The dev
     loop mints its own ephemeral ES256 token for local convenience.
   - `llm.api_key` — uses an `env.NAME` reference (e.g.
     `env.OPENROUTER_API_KEY`). The bifrost driver resolves it via
     `os.Getenv` at boot; a missing env var fails closed with
     `ErrMissingAPIKey` (CLAUDE.md §13 — fail loudly).

3. **Validate before booting:**

   ```sh
   harbor validate ./harbor.yaml
   ```

   `harbor validate` runs the in-process config loader without
   booting any subsystem and emits stable, file:line-precise errors —
   suitable as a CI pre-flight gate. Exit `0` = valid, `1` =
   validation errors, `2` = unexpected error.

4. **Boot the dev loop:**

   ```sh
   export OPENROUTER_API_KEY=sk-or-...
   harbor dev
   ```

   Flags:

   - `--config <path>` — config file; defaults to `harbor.yaml`.
   - `--port <int>` — loopback port; defaults to `18080` (also
     overridable via the `HARBOR_BIND` env var).
   - `--no-hot-reload` — disable the fsnotify-driven hot-reload
     watcher (overrides `cli.dev_hot_reload.enabled`).

   `harbor dev` serves until `SIGINT` / `SIGTERM`, then shuts down
   gracefully.

## The dev-only mock LLM escape hatch

For first-clone convenience and CI smoke with no real provider:

```sh
HARBOR_DEV_ALLOW_MOCK=1 harbor dev
```

This prints a `[DEV-ONLY MOCK LLM — DO NOT USE IN PRODUCTION]` stderr
banner on every boot. The default path with no flag set demands a
real provider (CLAUDE.md §13).

## Notes

- `harbor dev` boots the Runtime only — it does NOT serve the Console.
  The Console runs via the separate `harbor console` subcommand
  (D-091).
- Hot reload watches the config file's directory and
  `.harbor/agents` by default; an edit to `harbor.yaml` triggers a
  drained reload.
- To inspect a running dev server, use `harbor inspect-events` and
  `harbor inspect-runs` against the same loopback.
